package localexec

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Request is one call waiting for approval (design §12.3, approve:
// true).
type Request struct {
	// ID names the call to silo approve: a number, unique in the session.
	ID     string   `json:"id"`
	Tool   string   `json:"tool"`
	Script string   `json:"script"`
	Args   []string `json:"args"`
	// Stdin is how many bytes the call gives the script on stdin.
	Stdin int `json:"stdin_bytes"`
	// Deadline is when the call gives up waiting.
	Deadline time.Time `json:"deadline"`
}

// Summary is the request on one line, for a prompt or a notification.
func (r Request) Summary() string {
	var b strings.Builder
	b.WriteString(r.Script)
	for _, a := range r.Args {
		b.WriteByte(' ')
		b.WriteString(strconv.Quote(a))
	}
	if r.Stdin > 0 {
		fmt.Fprintf(&b, " (and %d bytes on stdin)", r.Stdin)
	}
	return b.String()
}

// Approver asks the user whether a call may run. Approve returns
// whether it may; it returns ctx's error when ctx ends first, which is
// the call's approval timeout.
type Approver interface {
	Approve(ctx context.Context, r Request) (bool, error)
}

// ErrDenied is a call the user denied.
var ErrDenied = errors.New("the user denied this call")

// Terminal asks on silo's own terminal: headless mode, where the CLI
// does not own it (design §12.3). Prompts are asked one at a time.
type Terminal struct {
	In  io.Reader
	Out io.Writer

	mu    sync.Mutex // one prompt at a time
	once  sync.Once
	lines chan string
}

func (t *Terminal) start() {
	t.lines = make(chan string)
	go func() {
		defer close(t.lines)
		sc := bufio.NewScanner(t.In)
		for sc.Scan() {
			t.lines <- sc.Text()
		}
	}()
}

// Approve prompts and waits for y or n.
func (t *Terminal) Approve(ctx context.Context, r Request) (bool, error) {
	t.once.Do(t.start)
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// A line typed while nothing was asked answers nothing: a stray y
	// must not approve the next call.
drain:
	for {
		select {
		case _, ok := <-t.lines:
			if !ok {
				break drain
			}
		default:
			break drain
		}
	}
	wait := ""
	if !r.Deadline.IsZero() {
		wait = fmt.Sprintf(", waiting %s", time.Until(r.Deadline).Round(time.Second))
	}
	fmt.Fprintf(t.Out, "\nsilo: the agent asks to run %s (call %s%s)\nsilo:   %s\nsilo: allow? [y/N] ", r.Tool, r.ID, wait, r.Summary())
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintf(t.Out, "\nsilo: call %s was not answered in time; it is refused\n", r.ID)
			return false, ctx.Err()
		case line, ok := <-t.lines:
			if !ok {
				return false, errors.New("silo's terminal closed before the call was approved")
			}
			switch strings.ToLower(strings.TrimSpace(line)) {
			case "y", "yes":
				return true, nil
			case "n", "no", "":
				return false, nil
			}
			fmt.Fprint(t.Out, "silo: answer y or n: ")
		}
	}
}

// Queue holds calls until silo approve, or a caller on the control
// socket, answers them: interactive mode, where the CLI owns the
// terminal, and --json (design §12.3). Notify, when set, is told of each
// call as it starts waiting.
type Queue struct {
	Notify func(Request)

	mu      sync.Mutex
	pending map[string]*pending
}

type pending struct {
	req    Request
	answer chan bool
}

// Approve queues r and waits for its answer.
func (q *Queue) Approve(ctx context.Context, r Request) (bool, error) {
	p := &pending{req: r, answer: make(chan bool, 1)}
	q.mu.Lock()
	if q.pending == nil {
		q.pending = map[string]*pending{}
	}
	q.pending[r.ID] = p
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		delete(q.pending, r.ID)
		q.mu.Unlock()
	}()
	if q.Notify != nil {
		q.Notify(r)
	}
	select {
	case ok := <-p.answer:
		return ok, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// Pending lists the calls waiting, oldest first.
func (q *Queue) Pending() []Request {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Request, 0, len(q.pending))
	for _, p := range q.pending {
		out = append(out, p.req)
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := strconv.Atoi(out[i].ID)
		b, _ := strconv.Atoi(out[j].ID)
		return a < b
	})
	return out
}

// Answer approves or denies the call id. With no id it answers the one
// call waiting, and is an error when there are none or several.
func (q *Queue) Answer(id string, approve bool) (Request, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if id == "" {
		switch len(q.pending) {
		case 0:
			return Request{}, errors.New("no call is waiting for approval")
		case 1:
			for k := range q.pending {
				id = k
			}
		default:
			var ids []string
			for k, p := range q.pending {
				ids = append(ids, fmt.Sprintf("%s (%s)", k, p.req.Summary()))
			}
			sort.Strings(ids)
			return Request{}, fmt.Errorf("%d calls are waiting for approval; name one: %s", len(ids), strings.Join(ids, ", "))
		}
	}
	p, ok := q.pending[id]
	if !ok {
		return Request{}, fmt.Errorf("no call %q is waiting for approval", id)
	}
	delete(q.pending, id)
	p.answer <- approve
	return p.req, nil
}

// Notify sends a desktop notification, best-effort: notify-send where
// it is installed, osascript on macOS, and nothing otherwise. path is
// the PATH to look in.
func Notify(ctx context.Context, path, title, body string) {
	look := func(name string) string {
		for _, dir := range strings.Split(path, ":") {
			if dir == "" {
				continue
			}
			p := dir + "/" + name
			if _, err := exec.LookPath(p); err == nil {
				return p
			}
		}
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if p := look("notify-send"); p != "" {
		_ = exec.CommandContext(ctx, p, "--app-name=silo", title, body).Run()
		return
	}
	if p := look("osascript"); p != "" {
		script := fmt.Sprintf("display notification %s with title %s", appleString(body), appleString(title))
		_ = exec.CommandContext(ctx, p, "-e", script).Run()
	}
}

// appleString quotes s as an AppleScript string literal.
func appleString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
