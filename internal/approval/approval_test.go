package approval

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

func ask(t *testing.T, typed string) (Decision, string) {
	t.Helper()
	keys := make(chan []byte, len(typed))
	for _, b := range []byte(typed) {
		keys <- []byte{b}
	}
	var out bytes.Buffer
	p := &prompter{out: &out, keys: keys}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.ask(ctx, Request{Kind: "commit", Title: "Commit X", Body: "+added\n-removed\n"}), out.String()
}

func TestPrompterApprove(t *testing.T) {
	d, out := ask(t, "maybe\ry\r")
	if !d.Approved {
		t.Errorf("decision = %+v", d)
	}
	if !strings.Contains(out, "Commit X") || !strings.Contains(out, "\x1b[32m+added") || strings.Count(out, "Approve?") != 2 {
		t.Errorf("output = %q", out)
	}
}

func TestPrompterShowsControlCharactersInsteadOfActingOnThem(t *testing.T) {
	keys := make(chan []byte, 2)
	keys <- []byte("y")
	keys <- []byte("\r")
	var out bytes.Buffer
	p := &prompter{out: &out, keys: keys}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p.ask(ctx, Request{
		Kind:  "commit",
		Title: "Fix typo\x1b[2J\rhidden",
		Body:  "+ok\n+evil\x1b[1A\x1b[2K\b\u202e\u009b\n",
	})
	got := out.String()
	for _, bad := range []string{"\x1b[2J", "\x1b[1A", "\x1b[2K", "\b", "\u202e", "\u009b", "\rhidden"} {
		if strings.Contains(got, bad) {
			t.Errorf("output contains %q unescaped: %q", bad, got)
		}
	}
	for _, want := range []string{`Fix typo\x1b[2J\x0dhidden`, `+evil\x1b[1A\x1b[2K\x08\u202e\u009b`} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q: %q", want, got)
		}
	}
}

func TestPrompterRejectWithReason(t *testing.T) {
	d, _ := ask(t, "n\rwrong fill\x7f\x7fle\r")
	if d.Approved || d.Reason != "wrong file" {
		t.Errorf("decision = %+v", d)
	}
	d, _ = ask(t, "no\r\r")
	if d.Approved || !strings.Contains(d.Reason, "without giving a reason") {
		t.Errorf("decision = %+v", d)
	}
}

func TestPrompterCtrlCRejects(t *testing.T) {
	d, _ := ask(t, "y\x03")
	if d.Approved {
		t.Errorf("Ctrl-C approved: %+v", d)
	}
}

func TestConsoleWithoutCLIRejects(t *testing.T) {
	c := NewConsole()
	if d := c.Approve(context.Background(), Request{}); d.Approved {
		t.Error("approved with no terminal session")
	}
}

// TestConsoleAnswerComesFromThePerson runs a "CLI" on the console that
// prints "y" lines and records its input. The approval must wait for the
// person's keystrokes, which must not reach the CLI.
func TestConsoleAnswerComesFromThePerson(t *testing.T) {
	ptmx, tty, err := pty.Open() // the person's terminal: they type into ptmx
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()
	var screen syncBuffer
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			screen.Write(buf[:n])
			if err != nil {
				return
			}
		}
	}()

	got := t.TempDir() + "/cli-input"
	cli := exec.Command("sh", "-c", `stty raw -echo; (while :; do echo y; sleep 0.05; done) & head -c 3 > `+got+`; kill $!`)
	c := &Console{in: tty, out: tty}
	done := make(chan error, 1)
	go func() { done <- c.Run(cli) }()
	waitFor(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.ptmx != nil })

	decided := make(chan Decision, 1)
	go func() { decided <- c.Approve(context.Background(), Request{Title: "Commit Y", Body: "+x"}) }()
	waitFor(t, func() bool { return strings.Contains(screen.String(), "Approve?") })
	time.Sleep(200 * time.Millisecond)
	select {
	case d := <-decided:
		t.Fatalf("decided before the person answered: %+v", d)
	default:
	}
	ptmx.Write([]byte("y\r"))
	select {
	case d := <-decided:
		if !d.Approved {
			t.Fatalf("decision = %+v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no decision")
	}
	ptmx.Write([]byte("abc"))
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("CLI did not exit")
	}
	if data, _ := os.ReadFile(got); string(data) != "abc" {
		t.Errorf("CLI read %q; the approval keystrokes leaked to it", data)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("timed out")
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
