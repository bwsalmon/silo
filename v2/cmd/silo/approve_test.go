package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/v2/control"
	"github.com/bwsalmon/silo/v2/localexec"
	"github.com/bwsalmon/silo/v2/session"
)

func TestApprove(t *testing.T) {
	run, err := os.MkdirTemp("", "rt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(run)
	getenv := func(k string) string {
		if k == "XDG_RUNTIME_DIR" {
			return run
		}
		return ""
	}
	// A session with calls waiting, answered by the same queue the
	// session's approve method uses.
	const id = "s-abcdefghijkl"
	os.MkdirAll(filepath.Join(run, "silo", id), 0o700)
	os.MkdirAll(filepath.Join(run, "silo", "s-abcdefghzzzz"), 0o700)
	srv, err := control.Listen(session.ControlSocket(getenv, id))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	q := &localexec.Queue{}
	srv.Handle("approve", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p session.ApproveParams
		json.Unmarshal(raw, &p)
		r, err := q.Answer(p.ID, !p.Deny)
		if err != nil {
			return nil, control.Errorf("%v", err)
		}
		return session.ApproveResult{Request: r, Approved: !p.Deny}, nil
	})
	results := make(chan bool, 2)
	for _, rid := range []string{"1", "2"} {
		go func() {
			ok, _ := q.Approve(context.Background(), localexec.Request{ID: rid, Tool: "local_deploy", Script: "deploy", Args: []string{"x y"}})
			results <- ok
		}()
	}
	for len(q.Pending()) < 2 {
		time.Sleep(10 * time.Millisecond)
	}

	cmd := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := approve(args, getenv, &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}
	if code, _, stderr := cmd(id); code != 1 || !strings.Contains(stderr, "2 calls are waiting") {
		t.Errorf("ambiguous: %d %s", code, stderr)
	}
	if code, _, stderr := cmd("s-abcdefgh", "1"); code != 1 || !strings.Contains(stderr, "names 2 sessions") {
		t.Errorf("ambiguous prefix: %d %s", code, stderr)
	}
	if code, _, stderr := cmd("s-nope"); code != 1 || !strings.Contains(stderr, "no running session") {
		t.Errorf("unknown session: %d %s", code, stderr)
	}
	if code, stdout, stderr := cmd("s-abcdefghi", "--deny", "2"); code != 0 || stdout != "denied call 2: local_deploy deploy \"x y\"\n" {
		t.Errorf("deny: %d %q %s", code, stdout, stderr)
	}
	if code, stdout, stderr := cmd(id); code != 0 || !strings.HasPrefix(stdout, "approved call 1") {
		t.Errorf("approve: %d %q %s", code, stdout, stderr)
	}
	a, b := <-results, <-results
	if a == b {
		t.Errorf("results %v %v, want one approved and one denied", a, b)
	}
	if code, _, _ := cmd(); code != exitUsage {
		t.Errorf("no session: exit %d", code)
	}
	if code, _, _ := cmd(id, "--force"); code != exitUsage {
		t.Errorf("unknown flag: exit %d", code)
	}
}

func TestValidateWarnsOnScripts(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "scripts"), 0o755)
	os.WriteFile(filepath.Join(dir, "Silofile"), []byte("silo: v2\nname: t\nagent: {cli: claude}\nlocal: {}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "scripts", "run-anything"), []byte("#!/bin/sh\neval \"$@\"\n"), 0o755)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"validate", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "warning:") || !strings.Contains(stderr.String(), "script run-anything: line 2: eval") {
		t.Errorf("no warning:\n%s", stderr.String())
	}
	os.Chmod(filepath.Join(dir, "scripts"), 0o777)
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"validate", dir}, &stdout, &stderr); code != exitInvalid || !strings.Contains(stderr.String(), "writable by group or others") {
		t.Errorf("exit %d: %s", code, stderr.String())
	}
}
