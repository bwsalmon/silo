package silo_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	silo "github.com/bwsalmon/silo/v2"
	"github.com/bwsalmon/silo/v2/control"
	"github.com/bwsalmon/silo/v2/events"
	"github.com/bwsalmon/silo/v2/internal/fakecli"
	"github.com/bwsalmon/silo/v2/session"
)

var creds = map[string]string{"token": "tok-1234"}

// TestInjectLive injects a message into a claude-like CLI that reads
// stream-json on stdin: it arrives mid-run, before the turn's result.
func TestInjectLive(t *testing.T) {
	h := newHost(t)
	h.fake(t, fakecli.Config{Echo: true, Answers: 2})
	s, err := silo.Start(context.Background(), file(time.Minute), silo.Options{Environ: h.environ, Credentials: creds})
	if err != nil {
		t.Fatal(err)
	}
	var got silo.Delivery
	for e := range s.Events() {
		if e.Transcript != nil && e.Transcript.Text == "got: do the thing" {
			if got, err = s.Inject("more"); err != nil {
				t.Errorf("Inject: %v", err)
			}
		}
	}
	res, _ := s.Wait()
	if got != silo.Delivered {
		t.Errorf("Inject said %q, want delivered", got)
	}
	if res.Exit.Reason != events.ReasonCompleted || res.FinalText != "got: more" {
		t.Errorf("result %+v, final %q", res.Exit, res.FinalText)
	}
	if _, err := s.Inject("late"); err == nil {
		t.Error("a session that has ended took a message")
	}
}

// TestInjectQueued injects into codex, which takes no input mid-run:
// the message is queued and is the next turn, through codex exec
// resume, once the first has ended.
func TestInjectQueued(t *testing.T) {
	h := newHost(t)
	self, _ := os.Executable()
	if err := os.Symlink(self, filepath.Join(h.root, "bin", "codex")); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(h.home, ".codex"), 0o700)
	os.WriteFile(filepath.Join(h.home, ".codex", "auth.json"), []byte("{}"), 0o600)
	f := file(time.Minute)
	f.Agent.CLI = "codex"
	s, err := silo.Start(context.Background(), f, silo.Options{Environ: h.environ, Credentials: creds})
	if err != nil {
		t.Fatal(err)
	}
	var got silo.Delivery
	for e := range s.Events() {
		if e.Transcript != nil && e.Transcript.Text == "working" {
			if got, err = s.Inject("more"); err != nil {
				t.Errorf("Inject: %v", err)
			}
			os.WriteFile(filepath.Join(h.home, fakecli.GoFile), nil, 0o600)
		}
	}
	res, _ := s.Wait()
	if got != silo.Queued {
		t.Errorf("Inject said %q, want queued", got)
	}
	if res.Exit.Reason != events.ReasonCompleted || res.FinalText != "resumed: more" {
		t.Errorf("result %+v, final %q, errors %v", res.Exit, res.FinalText, res.Errors)
	}
	b, _ := os.ReadFile(filepath.Join(h.home, "codex-runs"))
	runs := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(runs) != 2 || !strings.HasPrefix(runs[1], "exec resume --json") || !strings.HasSuffix(runs[1], " th-1 -") {
		t.Errorf("codex runs:\n%s", b)
	}
}

// rawCall writes one line to the socket and reads one back.
func rawCall(t *testing.T, path, line string) map[string]any {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	c.Write([]byte(line + "\n"))
	sc := bufio.NewScanner(c)
	if !sc.Scan() {
		t.Fatalf("%s: no answer", line)
	}
	var m map[string]any
	json.Unmarshal(sc.Bytes(), &m)
	return m
}

func errCode(m map[string]any) float64 {
	e, _ := m["error"].(map[string]any)
	c, _ := e["code"].(float64)
	return c
}

// TestControlSocket drives a running session over control.sock: a
// session with a one-second deadline is extended past it, takes an
// injected message, refuses malformed requests, and is cancelled.
func TestControlSocket(t *testing.T) {
	h := newHost(t)
	h.fake(t, fakecli.Config{Wait: true})
	start := time.Now()
	s, err := silo.Start(context.Background(), file(time.Second), silo.Options{Environ: h.environ, Credentials: creds, Grace: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ev := s.Events()
	var id string
	for e := range ev {
		id = e.Session
		if e.Status != nil && e.Status.Step == "agent" && e.Status.State == events.StateStarted {
			break
		}
	}
	sock := session.ControlSocket(func(k string) string {
		for _, kv := range h.environ {
			if strings.HasPrefix(kv, k+"=") {
				return kv[len(k)+1:]
			}
		}
		return ""
	}, id)
	if fi, err := os.Stat(sock); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("control.sock: %v %v", fi.Mode(), err)
	}
	ctx := context.Background()
	var ext session.ExtendResult
	if err := control.Call(ctx, sock, "extend_deadline", session.ExtendParams{Deadline: time.Now().Add(3 * time.Second)}, &ext); err != nil {
		t.Fatalf("extend_deadline: %v", err)
	}
	var inj session.InjectResult
	if err := control.Call(ctx, sock, "inject", session.InjectParams{Message: "more"}, &inj); err != nil || inj.Delivery != session.Delivered {
		t.Errorf("inject: %+v %v", inj, err)
	}
	for _, c := range []struct {
		line string
		code float64
	}{
		{`not json`, control.CodeParseError},
		{`{"jsonrpc":"1.0","id":1,"method":"cancel"}`, control.CodeInvalidRequest},
		{`{"jsonrpc":"2.0","id":1,"method":"detach"}`, control.CodeMethodNotFound},
		{`{"jsonrpc":"2.0","id":1,"method":"inject","params":{"msg":"x"}}`, control.CodeInvalidParams},
		{`{"jsonrpc":"2.0","id":1,"method":"inject","params":{"message":""}}`, control.CodeInvalidParams},
		{`{"jsonrpc":"2.0","id":1,"method":"extend_deadline","params":{}}`, control.CodeInvalidParams},
		{`{"jsonrpc":"2.0","id":1,"method":"extend_deadline","params":{"deadline":"tomorrow"}}`, control.CodeInvalidParams},
		{`{"jsonrpc":"2.0","id":1,"method":"cancel","params":[1]}`, control.CodeInvalidParams},
	} {
		if got := errCode(rawCall(t, sock, c.line)); got != c.code {
			t.Errorf("%s: error code %v, want %v", c.line, got, c.code)
		}
	}
	if err := s.Approve("", true); err == nil || !strings.Contains(err.Error(), "no local script") {
		t.Errorf("Approve with nothing to approve: %v", err)
	}
	// Past the original deadline, the session still runs.
	time.Sleep(time.Until(start.Add(1500 * time.Millisecond)))
	if err := control.Call(ctx, sock, "cancel", session.CancelParams{Reason: "superseded"}, nil); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	for range ev {
	}
	res, _ := s.Wait()
	if res.Exit.Reason != events.ReasonCancelled || !strings.Contains(res.Exit.Detail, "superseded") {
		t.Errorf("result %+v", res.Exit)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("ended after %s: the cancel did not end it", d)
	}
}
