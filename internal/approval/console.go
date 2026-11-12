package approval

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// Console owns the person's terminal for a session. It runs the agent
// CLI on a pseudo-terminal, copying the person's keystrokes to it and its
// output back, and implements Approver: while a prompt is up it stops
// both copies, shows the request, and reads the answer itself.
type Console struct {
	in  *os.File
	out *os.File

	approveMu sync.Mutex // one prompt at a time
	outMu     sync.Mutex // held while a prompt is on screen
	mu        sync.Mutex
	ptmx      *os.File
	capture   chan []byte
}

// NewConsole returns a console on the process's stdin and stdout.
func NewConsole() *Console { return &Console{in: os.Stdin, out: os.Stdout} }

// IsTerminal reports whether stdin is a terminal; without one there is
// nobody to approve anything.
func (c *Console) IsTerminal() bool { return term.IsTerminal(int(c.in.Fd())) }

// Run starts cmd on a pseudo-terminal and relays the person's terminal to
// it until it exits.
func (c *Console) Run(cmd *exec.Cmd) error {
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return err
	}
	defer ptmx.Close()
	c.mu.Lock()
	c.ptmx = ptmx
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.ptmx = nil
		c.mu.Unlock()
	}()

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			pty.InheritSize(c.in, ptmx)
		}
	}()
	pty.InheritSize(c.in, ptmx)

	state, err := term.MakeRaw(int(c.in.Fd()))
	if err != nil {
		return err
	}
	defer term.Restore(int(c.in.Fd()), state)

	go c.pumpIn(ptmx)
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				c.outMu.Lock()
				c.out.Write(buf[:n])
				c.outMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	err = cmd.Wait()
	<-outDone
	return err
}

// pumpIn copies keystrokes to the CLI, or to a waiting prompt.
func (c *Console) pumpIn(ptmx *os.File) {
	buf := make([]byte, 4096)
	for {
		n, err := c.in.Read(buf)
		if n > 0 {
			b := append([]byte(nil), buf[:n]...)
			c.mu.Lock()
			capture, live := c.capture, c.ptmx == ptmx
			c.mu.Unlock()
			if capture != nil {
				capture <- b
			} else if live {
				ptmx.Write(b)
			} else {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// Approve shows req and asks the person. With no CLI running on the
// console (no terminal), it rejects.
func (c *Console) Approve(ctx context.Context, req Request) Decision {
	c.approveMu.Lock()
	defer c.approveMu.Unlock()
	c.mu.Lock()
	ptmx := c.ptmx
	if ptmx == nil {
		c.mu.Unlock()
		return Decision{Reason: "no terminal to ask the person on"}
	}
	keys := make(chan []byte, 64)
	c.capture = keys
	c.mu.Unlock()
	c.outMu.Lock()
	defer func() {
		c.mu.Lock()
		c.capture = nil
		c.mu.Unlock()
		c.outMu.Unlock()
		redraw(ptmx)
	}()
	p := &prompter{out: c.out, keys: keys, settle: Settle}
	return p.ask(ctx, req)
}

// redraw nudges the CLI to repaint over the prompt by bouncing its
// window size.
func redraw(ptmx *os.File) {
	sz, err := pty.GetsizeFull(ptmx)
	if err != nil || sz.Cols < 2 {
		return
	}
	small := *sz
	small.Cols--
	pty.Setsize(ptmx, &small)
	pty.Setsize(ptmx, sz)
}

// Settle is how long a prompt ignores keystrokes after it is shown, so
// that what the person was already typing to the CLI when it appeared
// (a "y" and Enter meant for something else) cannot answer it.
var Settle = 500 * time.Millisecond

// prompter renders a request and reads the answer from raw keystrokes.
type prompter struct {
	out  io.Writer
	keys <-chan []byte
	rest []byte
	// settle discards keystrokes for this long once the request is shown.
	settle time.Duration
}

func (p *prompter) ask(ctx context.Context, req Request) Decision {
	// The leading reset clears any colour or concealment the CLI's output
	// left set, so the request is shown as written.
	p.print("\x1b[0m\r\n\x1b[1;33m─── silo: approval needed ───\x1b[0m\r\n")
	p.print("\x1b[1m" + Sanitize(req.Title) + "\x1b[0m\r\n\r\n")
	p.print(Render(req.Body))
	p.print("\r\n")
	if err := p.discardTypeahead(ctx); err != nil {
		p.print("\r\nrejected\r\n")
		return Decision{Reason: "the person cancelled the prompt"}
	}
	for {
		ans, err := p.line(ctx, "\x1b[1;33mApprove? [y/n]: \x1b[0m")
		if err != nil {
			p.print("\r\nrejected\r\n")
			return Decision{Reason: "the person cancelled the prompt"}
		}
		switch strings.ToLower(strings.TrimSpace(ans)) {
		case "y", "yes":
			p.print("approved\r\n")
			return Decision{Approved: true}
		case "n", "no":
			reason, err := p.line(ctx, "Reason, sent to the agent (optional): ")
			if err != nil || strings.TrimSpace(reason) == "" {
				reason = "the person rejected it without giving a reason"
			}
			p.print("rejected\r\n")
			return Decision{Reason: strings.TrimSpace(reason)}
		}
	}
}

var errCancelled = errors.New("cancelled")

// discardTypeahead drops every keystroke that arrives before settle has
// passed, and any already queued.
func (p *prompter) discardTypeahead(ctx context.Context) error {
	p.rest = nil
	if p.settle <= 0 {
		return nil
	}
	t := time.NewTimer(p.settle)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.keys:
		case <-t.C:
			return nil
		}
	}
}

// line reads one line, echoing it, with backspace; Ctrl-C or Ctrl-D
// cancels.
func (p *prompter) line(ctx context.Context, prompt string) (string, error) {
	p.print(prompt)
	var line []byte
	for {
		if len(p.rest) == 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case b := <-p.keys:
				p.rest = b
			}
		}
		ch := p.rest[0]
		p.rest = p.rest[1:]
		switch ch {
		case '\r', '\n':
			p.print("\r\n")
			return string(line), nil
		case 3, 4: // Ctrl-C, Ctrl-D
			return "", errCancelled
		case 127, 8:
			if len(line) > 0 {
				line = line[:len(line)-1]
				p.print("\b \b")
			}
		case 27: // drop escape sequences (arrow keys and the like)
			p.rest = nil
		default:
			if ch >= 32 {
				line = append(line, ch)
				p.print(string(ch))
			}
		}
	}
}

func (p *prompter) print(s string) { io.WriteString(p.out, s) }

// Sanitize makes agent-written text safe to print on the person's
// terminal: every control character but newline and tab (escape
// sequences, carriage returns, backspaces), every C1 control and every
// bidirectional override is shown as a visible \x.. or \u.... escape
// instead of being acted on, and invalid UTF-8 as U+FFFD. Without this a
// commit message or file could move the cursor or clear the screen and
// show the person a different change from the one they approve.
func Sanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(s, "�") {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\x%02x", r)
		case r >= 0x80 && r <= 0x9f,
			r >= 0x202a && r <= 0x202e,
			r >= 0x2066 && r <= 0x2069,
			r == 0x200e || r == 0x200f || r == 0x061c:
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Render formats a request body for a raw-mode terminal, colouring diff
// lines. The body is sanitized first (Sanitize).
func Render(body string) string {
	var b bytes.Buffer
	for _, l := range strings.Split(strings.TrimRight(Sanitize(body), "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
			fmt.Fprintf(&b, "\x1b[1m%s\x1b[0m\r\n", l)
		case strings.HasPrefix(l, "+"):
			fmt.Fprintf(&b, "\x1b[32m%s\x1b[0m\r\n", l)
		case strings.HasPrefix(l, "-"):
			fmt.Fprintf(&b, "\x1b[31m%s\x1b[0m\r\n", l)
		case strings.HasPrefix(l, "@@"):
			fmt.Fprintf(&b, "\x1b[36m%s\x1b[0m\r\n", l)
		default:
			b.WriteString(l + "\r\n")
		}
	}
	return b.String()
}
