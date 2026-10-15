package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bwsalmon/silo/old/agentcli"
	"github.com/bwsalmon/silo/old/control"
	"github.com/bwsalmon/silo/old/events"
	"github.com/bwsalmon/silo/old/internal/fakecli"
	"github.com/bwsalmon/silo/old/sandbox"
	"github.com/bwsalmon/silo/old/session"
)

// asSiloVar makes this test binary silo itself, for a caller process
// that runs silo run --detach and exits.
const asSiloVar = "SILO_TEST_AS_SILO"

func init() {
	if len(os.Args) > 1 && os.Args[1] == superviseCmd || os.Getenv(asSiloVar) == "1" {
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
	}
}

// detachEnv is a home, a fake claude on PATH and a silo folder whose
// headless CLI echoes the messages it is sent and never ends by itself.
type detachEnv struct {
	root, dir string
	environ   []string
}

func newDetachEnv(t *testing.T) *detachEnv {
	t.Helper()
	// A short root: the control socket's path is limited to about 100
	// bytes.
	root, err := os.MkdirTemp("", "silo-d")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	home, bin := filepath.Join(root, "home"), filepath.Join(root, "bin")
	for _, d := range []string{home, bin, filepath.Join(root, "run"), filepath.Join(root, "state"), filepath.Join(root, "silo")} {
		os.MkdirAll(d, 0o700)
	}
	self, _ := os.Executable()
	if err := os.Symlink(self, filepath.Join(bin, "claude")); err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(fakecli.Config{Echo: true, Answers: 99})
	os.WriteFile(filepath.Join(home, "fake.json"), cfg, 0o600)
	os.WriteFile(filepath.Join(root, "silo", "Silofile"), []byte(`silo: v2
name: detached
agent: {cli: claude, mode: headless}
`), 0o644)
	return &detachEnv{root: root, dir: filepath.Join(root, "silo"), environ: []string{
		"PATH=" + bin, "HOME=" + home,
		"XDG_RUNTIME_DIR=" + filepath.Join(root, "run"), "XDG_STATE_HOME=" + filepath.Join(root, "state"),
	}}
}

func (e *detachEnv) getenv(k string) string { return agentcli.Getenv(e.environ, k) }

// detach runs silo run --detach as a caller process of its own, which
// exits once the session has started, and returns the session's id.
func (e *detachEnv) detach(t *testing.T, args ...string) string {
	t.Helper()
	self, _ := os.Executable()
	cmd := exec.Command(self, append(append([]string{"run", "--detach", "--prompt", "first"}, args...), e.dir)...)
	cmd.Env = append([]string{asSiloVar + "=1"}, e.environ...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("silo run --detach: %v\n%s", err, stderr.String())
	}
	id := strings.TrimSpace(stdout.String())
	if !session.ValidID(id) {
		t.Fatalf("silo run --detach printed %q", id)
	}
	t.Cleanup(func() {
		if rec, err := session.ReadRecord(filepath.Join(e.root, "run", "silo", id)); err == nil && rec.PID > 0 {
			syscall.Kill(rec.PID, syscall.SIGKILL)
		}
		session.GC(context.Background(), session.GCOptions{Getenv: e.getenv, Backend: noBackend{}, MinAge: time.Nanosecond})
	})
	return id
}

var errStop = errors.New("stop")

// follow attaches from from and collects events until until says stop,
// or the result.
func (e *detachEnv) follow(t *testing.T, id string, from int64, until func(events.Event) bool) []events.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var got []events.Event
	err := session.Follow(ctx, e.getenv, id, from, func(ev events.Event) error {
		got = append(got, ev)
		if until != nil && until(ev) {
			return errStop
		}
		return nil
	})
	if err != nil && err != errStop {
		t.Fatalf("following %s from %d: %v (after %d events)", id, from, err, len(got))
	}
	return got
}

func said(text string) func(events.Event) bool {
	return func(e events.Event) bool { return e.Transcript != nil && e.Transcript.Text == text }
}

// TestDetachAndAttach starts a detached session from a caller process
// that then exits, finds it with silo ls by its label, attaches from the
// start and from a cursor mid-stream, injects a message and cancels the
// session through the control socket, and checks that the replays have
// no gap and no repeat.
func TestDetachAndAttach(t *testing.T) {
	e := newDetachEnv(t)
	id := e.detach(t, "--label", "grain.run=r-1", "--label", "team=eu")

	// The caller has exited; the session goes on.
	first := e.follow(t, id, 0, said("got: first"))
	if first[0].Seq != 1 {
		t.Fatalf("attach from 0 starts at seq %d", first[0].Seq)
	}

	var out, errb bytes.Buffer
	if code := ls([]string{"--label", "grain.run=r-1"}, e.getenv, &out, &errb); code != 0 || !strings.Contains(out.String(), id) || !strings.Contains(out.String(), "grain.run=r-1,team=eu") {
		t.Fatalf("silo ls --label grain.run=r-1: exit %d\n%s%s", code, out.String(), errb.String())
	}
	out.Reset()
	if code := ls([]string{"--label", "grain.run=r-2", "--json"}, e.getenv, &out, &errb); code != 0 || out.Len() != 0 {
		t.Fatalf("silo ls --label grain.run=r-2: exit %d\n%s", code, out.String())
	}
	out.Reset()
	ls([]string{"--json"}, e.getenv, &out, &errb)
	var rec session.Record
	if err := json.Unmarshal(out.Bytes(), &rec); err != nil || rec.ID != id || !rec.Detached || rec.Labels["team"] != "eu" {
		t.Fatalf("silo ls --json: %q (%v)", out.String(), err)
	}

	// Re-attached, the control socket takes inject.
	cursor := first[len(first)-1].Seq
	sock := session.ControlSocket(e.getenv, id)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var inj session.InjectResult
	if err := control.Call(ctx, sock, "inject", session.InjectParams{Message: "second"}, &inj); err != nil || inj.Delivery != session.Delivered {
		t.Fatalf("inject: %+v, %v", inj, err)
	}
	mid := e.follow(t, id, cursor, said("got: second"))
	if mid[0].Seq != cursor+1 {
		t.Fatalf("attach from %d starts at seq %d", cursor, mid[0].Seq)
	}

	if err := control.Call(ctx, sock, "cancel", session.CancelParams{Reason: "done here"}, nil); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	all := e.follow(t, id, 0, nil)
	for i, ev := range all {
		if ev.Seq != int64(i+1) || ev.Session != id {
			t.Fatalf("event %d has seq %d, session %q: a gap or a repeat", i, ev.Seq, ev.Session)
		}
	}
	for i, ev := range append(first, mid...) {
		a, _ := events.Marshal(ev)
		b, _ := events.Marshal(all[i])
		if !bytes.Equal(a, b) {
			t.Fatalf("event %d differs between attaches:\n%s%s", i+1, a, b)
		}
	}
	res := all[len(all)-1].Result
	if res == nil || res.Exit.Reason != events.ReasonCancelled || !strings.Contains(res.Exit.Detail, "done here") {
		t.Fatalf("the last event is not the cancelled result: %+v", all[len(all)-1])
	}

	// An ended session is replayed by silo attach, which exits with its
	// code; silo ls no longer lists it.
	out.Reset()
	if code := attach([]string{id, "--from", "3", "--json"}, e.getenv, &out, &errb); code != session.ExitTerminated {
		t.Fatalf("silo attach: exit %d\n%s", code, errb.String())
	}
	sc := bufio.NewScanner(&out)
	n := int64(3)
	for sc.Scan() {
		var ev events.Event
		if err := events.Unmarshal(sc.Bytes(), &ev); err != nil || ev.Seq != n+1 {
			t.Fatalf("silo attach --from 3 line %q: %v, want seq %d", sc.Text(), err, n+1)
		}
		n++
	}
	if n != int64(len(all)) {
		t.Errorf("silo attach --from 3 wrote up to seq %d of %d", n, len(all))
	}
	out.Reset()
	ls(nil, e.getenv, &out, &errb)
	if strings.Contains(out.String(), id) {
		t.Errorf("silo ls lists an ended session:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(e.root, "state", "silo", "sessions", id, supervisorLog)); err != nil {
		t.Errorf("no supervisor log: %v", err)
	}
}

// TestKilledSupervisorIsCollected kills a detached session's supervisor
// with SIGKILL: attach then ends without a result, and silo gc removes
// the session directory and the CLI it left.
func TestKilledSupervisorIsCollected(t *testing.T) {
	e := newDetachEnv(t)
	id := e.detach(t)
	e.follow(t, id, 0, said("got: first"))
	dir := filepath.Join(e.root, "run", "silo", id)
	rec, err := session.ReadRecord(dir)
	if err != nil {
		t.Fatal(err)
	}
	var pgid int
	for _, r := range rec.Resources {
		if r.Kind == session.KindProcessGroup {
			pgid = r.PGID
		}
	}
	if err := syscall.Kill(rec.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); syscall.Kill(rec.PID, 0) == nil; {
		if time.Now().After(deadline) {
			t.Fatal("the supervisor outlived SIGKILL")
		}
		time.Sleep(20 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := session.Follow(ctx, e.getenv, id, 0, func(events.Event) error { return nil }); !errors.Is(err, session.ErrNoResult) {
		t.Fatalf("following a killed session: %v, want ErrNoResult", err)
	}
	rep, err := session.GC(ctx, session.GCOptions{Getenv: e.getenv, Backend: noBackend{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) || len(rep.Removed) != 1 {
		t.Fatalf("gc removed %v; the session directory: %v", rep.Removed, err)
	}
	if pgid > 0 {
		for deadline := time.Now().Add(10 * time.Second); syscall.Kill(-pgid, 0) == nil; {
			if time.Now().After(deadline) {
				t.Fatal("the CLI's process group outlived gc")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// TestDetachRefusesInteractive keeps --detach to headless sessions.
func TestDetachRefusesInteractive(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Silofile"), []byte("silo: v2\nname: i\nagent: {cli: claude}\n"), 0o644)
	var stdout, stderr bytes.Buffer
	if code := runSession([]string{"--detach", dir}, strings.NewReader(""), &stdout, &stderr, session.Options{}); code != exitUsage {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
}

// noBackend runs no sandboxes, so gc here touches no real containers.
type noBackend struct{}

func (noBackend) Create(context.Context, sandbox.SandboxSpec) (sandbox.Sandbox, error) {
	return nil, errors.New("no sandboxes here")
}

func (noBackend) Find(context.Context, string) ([]sandbox.Sandbox, error) { return nil, nil }
