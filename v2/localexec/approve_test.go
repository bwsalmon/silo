package localexec

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTerminal is silo's terminal: the test reads what silo writes and
// types answers.
type fakeTerminal struct {
	inW  *io.PipeWriter
	outR *bufio.Reader
	t    *Terminal
}

func newFakeTerminal() *fakeTerminal {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	return &fakeTerminal{inW: inW, outR: bufio.NewReader(outR), t: &Terminal{In: inR, Out: outW}}
}

// waitPrompt reads silo's output up to the [y/N] prompt.
func (f *fakeTerminal) waitPrompt(t *testing.T) string {
	t.Helper()
	return f.readUntil(t, "[y/N] ")
}

func (f *fakeTerminal) readUntil(t *testing.T, suffix string) string {
	t.Helper()
	var b strings.Builder
	for !strings.HasSuffix(b.String(), suffix) {
		c, err := f.outR.ReadByte()
		if err != nil {
			t.Fatal(err)
		}
		b.WriteByte(c)
	}
	return b.String()
}

func (f *fakeTerminal) drain() {
	go io.Copy(io.Discard, f.outR)
}

func TestTerminalApprover(t *testing.T) {
	ft := newFakeTerminal()
	req := Request{ID: "7", Tool: "local_deploy", Script: "deploy", Args: []string{"feature x", "--prod"}}
	for _, tc := range []struct {
		typed string
		want  bool
	}{{"y\n", true}, {"n\n", false}, {"maybe\nYES\n", true}, {"\n", false}} {
		got := make(chan bool, 1)
		go func() {
			ok, err := ft.t.Approve(context.Background(), req)
			if err != nil {
				t.Error(err)
			}
			got <- ok
		}()
		p := ft.waitPrompt(t)
		if !strings.Contains(p, `deploy "feature x" "--prod"`) || !strings.Contains(p, "local_deploy (call 7") {
			t.Errorf("prompt %q", p)
		}
		go io.WriteString(ft.inW, tc.typed)
		if tc.typed == "maybe\nYES\n" {
			// Asked again after the wrong answer.
			ft.readUntil(t, "answer y or n: ")
		}
		if ok := <-got; ok != tc.want {
			t.Errorf("typed %q: approved %v", tc.typed, ok)
		}
	}

	// Unanswered, the call gives up when its context ends.
	ft.drain()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := ft.t.Approve(ctx, req); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timeout: %v", err)
	}
}

func TestQueue(t *testing.T) {
	var mu sync.Mutex
	var notified []string
	q := &Queue{Notify: func(r Request) { mu.Lock(); notified = append(notified, r.ID); mu.Unlock() }}
	if _, err := q.Answer("", true); err == nil || !strings.Contains(err.Error(), "no call") {
		t.Errorf("empty queue: %v", err)
	}
	type answer struct {
		ok  bool
		err error
	}
	wait := func(id string, ctx context.Context) chan answer {
		ch := make(chan answer, 1)
		go func() {
			ok, err := q.Approve(ctx, Request{ID: id, Tool: "local_x", Script: "x"})
			ch <- answer{ok, err}
		}()
		for {
			for _, r := range q.Pending() {
				if r.ID == id {
					return ch
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	a := wait("1", context.Background())
	if r, err := q.Answer("", true); err != nil || r.ID != "1" {
		t.Fatalf("the only call: %v %v", r, err)
	}
	if got := <-a; !got.ok || got.err != nil {
		t.Errorf("approved: %+v", got)
	}
	b := wait("2", context.Background())
	c := wait("3", context.Background())
	if _, err := q.Answer("", true); err == nil || !strings.Contains(err.Error(), "2 calls are waiting") {
		t.Errorf("ambiguous: %v", err)
	}
	if _, err := q.Answer("9", true); err == nil {
		t.Error("answered a call that is not waiting")
	}
	q.Answer("3", false)
	q.Answer("2", true)
	if got := <-c; got.ok || got.err != nil {
		t.Errorf("denied: %+v", got)
	}
	if got := <-b; !got.ok {
		t.Errorf("approved: %+v", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	d := wait("4", ctx)
	if got := <-d; !errors.Is(got.err, context.DeadlineExceeded) {
		t.Errorf("timeout: %+v", got)
	}
	if len(q.Pending()) != 0 {
		t.Errorf("left pending: %v", q.Pending())
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(notified, ",") != "1,2,3,4" {
		t.Errorf("notified %v", notified)
	}
}

func TestApprovedScripts(t *testing.T) {
	f := folder(t, header+`local:
  scripts:
    list:
      - path: scripts/deploy
        approve: true
        timeout: 1s
`, map[string]string{"scripts/deploy": "#!/bin/sh\necho deployed \"$1\"\n", "scripts/free": "#!/bin/sh\necho free\n"})
	q := &Queue{}
	ex := executor(t, f, Config{Approver: q})
	answer := func(approve bool) {
		go func() {
			for len(q.Pending()) == 0 {
				time.Sleep(10 * time.Millisecond)
			}
			q.Answer("", approve)
		}()
	}

	answer(true)
	r, _, err := call(t, ex, "local_deploy", map[string]any{"args": []any{"main"}})
	if err != nil || r.Stdout != "deployed main\n" {
		t.Fatalf("approved: %+v %v", r, err)
	}
	answer(false)
	if _, _, err := call(t, ex, "local_deploy", map[string]any{"args": []any{"main"}}); !errors.Is(err, ErrDenied) {
		t.Errorf("denied: %v", err)
	}
	start := time.Now()
	_, _, err = call(t, ex, "local_deploy", map[string]any{"args": []any{"main"}})
	if err == nil || !strings.Contains(err.Error(), "not approved within 1s") || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timeout: %v", err)
	}
	if d := time.Since(start); d < time.Second || d > 5*time.Second {
		t.Errorf("waited %s for a 1s timeout", d)
	}
	// A script without approve: true never asks.
	if r, _, err := call(t, ex, "local_free", map[string]any{"args": []any{}}); err != nil || r.Stdout != "free\n" {
		t.Errorf("free: %+v %v", r, err)
	}

	// Without an approver, approve: true calls cannot run.
	ex2 := executor(t, f, Config{})
	if _, _, err := call(t, ex2, "local_deploy", map[string]any{"args": []any{}}); err == nil || !strings.Contains(err.Error(), "no way to ask") {
		t.Errorf("no approver: %v", err)
	}
}
