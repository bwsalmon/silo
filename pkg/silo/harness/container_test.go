package harness_test

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bwsalmon/silo/pkg/silo/harness"
)

var layout = harness.Layout{SocketDir: "/run/silo/sessions", StateDir: "/var/lib/silo/sessions"}

func spec(t *testing.T, slot int) harness.Spec {
	t.Helper()
	a := harness.ClaudeCode{}
	return harness.NewSpec(layout, "m3x8re", slot, a, a.Command(harness.Launch{Model: "claude-opus-5-5", Prompt: "hi", SessionID: a.NewSessionID()}), harness.Limits{})
}

// TestSpecIsolation asserts every isolation property of a session's
// container (agents.md, "Isolation on the control VM", SV-118) in the
// spec silod builds, and in what sessiond hands the runtime for it.
func TestSpecIsolation(t *testing.T) {
	s := spec(t, 7)
	if err := s.Validate(layout); err != nil {
		t.Fatal(err)
	}
	if s.UID != harness.UIDBase+7 || s.UID < 30000 {
		t.Errorf("uid %d: want its own, unprivileged, by slot", s.UID)
	}
	if spec(t, 8).UID == s.UID {
		t.Error("two slots share a uid")
	}
	if !slices.Equal(s.CapDrop, []string{"ALL"}) || len(s.CapAdd) != 0 || !s.NoNewPrivileges || s.Seccomp != "" || !s.ReadOnlyRoot {
		t.Errorf("privileges: %+v", s)
	}
	if s.Network != "none" || !s.DropMetadata {
		t.Errorf("network %q, drop metadata %v", s.Network, s.DropMetadata)
	}
	wantMounts := []harness.Mount{
		{Source: "/run/silo/sessions/m3x8re/silo.sock", Target: harness.SocketPath},
		{Source: "/var/lib/silo/sessions/m3x8re", Target: harness.Home},
	}
	if !slices.Equal(s.Mounts, wantMounts) {
		t.Errorf("mounts %+v, want only the socket and the private directory", s.Mounts)
	}
	if s.MemoryBytes != 1<<30 || s.CPUShares != harness.DefaultCPUShares || s.Pids != harness.DefaultPids {
		t.Errorf("limits: memory %d, cpu %d, pids %d", s.MemoryBytes, s.CPUShares, s.Pids)
	}
	if s.Argv[0] != harness.Init || s.Argv[1] != "harness-init" || !slices.Contains(s.Argv, "claude") {
		t.Errorf("argv %q", s.Argv)
	}

	// What the runtime is given.
	c := harness.HarnessContainer(&s, "img@sha256:1")
	h := c.HostConfig
	if c.User != "30007:30007" {
		t.Errorf("user %q", c.User)
	}
	if !h.ReadonlyRootfs || h.Privileged || !slices.Equal(h.CapDrop, []string{"ALL"}) || len(h.CapAdd) != 0 {
		t.Errorf("host config privileges: %+v", h)
	}
	if !slices.Equal(h.SecurityOpt, []string{"no-new-privileges:true"}) {
		t.Errorf("security options %q: want no-new-privileges and the default seccomp profile", h.SecurityOpt)
	}
	if h.NetworkMode != "container:"+harness.NetName("m3x8re") || h.PidMode != "" || h.IpcMode != "private" {
		t.Errorf("namespaces: network %q, pid %q, ipc %q", h.NetworkMode, h.PidMode, h.IpcMode)
	}
	if h.Memory != 1<<30 || h.MemorySwap != h.Memory || h.CPUShares != harness.DefaultCPUShares || h.PidsLimit != harness.DefaultPids {
		t.Errorf("limits %+v", h)
	}
	if len(h.Mounts) != 2 || h.Mounts[0].Source != wantMounts[0].Source || h.Mounts[1].Source != wantMounts[1].Source || len(h.Tmpfs) != 1 || !strings.Contains(h.Tmpfs["/tmp"], "noexec") {
		t.Errorf("mounts %+v, tmpfs %v", h.Mounts, h.Tmpfs)
	}
	b, _ := json.Marshal(c)
	for _, bad := range []string{"docker.sock", "seccomp", "Privileged\":true", "host"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("the harness container's create request has %q: %s", bad, b)
		}
	}

	n := harness.NetContainer(&s, "img@sha256:1")
	if n.HostConfig.NetworkMode != "none" || !n.NetworkDisabled || n.User != c.User || len(n.HostConfig.CapAdd) != 0 ||
		!n.HostConfig.ReadonlyRootfs || len(n.HostConfig.Mounts) != 0 || !slices.Equal(n.Cmd, []string{"pause"}) {
		t.Errorf("namespace container %+v", n)
	}
	fw := harness.FirewallContainer(&s, "img@sha256:1")
	if fw.HostConfig.NetworkMode != h.NetworkMode || !slices.Equal(fw.HostConfig.CapAdd, []string{"NET_ADMIN", "NET_RAW"}) ||
		len(fw.HostConfig.Mounts) != 0 || !fw.HostConfig.ReadonlyRootfs {
		t.Errorf("firewall container %+v", fw)
	}
	script := strings.Join(fw.Cmd, " ")
	if !strings.Contains(script, "-A OUTPUT -d 169.254.169.254 -j DROP") || !strings.Contains(script, "-C OUTPUT -d 169.254.169.254 -j DROP") {
		t.Errorf("the firewall doesn't drop the metadata server: %s", script)
	}
}

func TestValidateRefusesLooserSpecs(t *testing.T) {
	for name, edit := range map[string]func(*harness.Spec){
		"root":           func(s *harness.Spec) { s.UID = 0 },
		"silod's uid":    func(s *harness.Spec) { s.UID = 2001 },
		"past the range": func(s *harness.Spec) { s.UID = harness.UIDBase + harness.MaxSessions },
		"capabilities":   func(s *harness.Spec) { s.CapAdd = []string{"NET_ADMIN"} },
		"keeps some":     func(s *harness.Spec) { s.CapDrop = []string{"SYS_ADMIN"} },
		"new privileges": func(s *harness.Spec) { s.NoNewPrivileges = false },
		"no seccomp":     func(s *harness.Spec) { s.Seccomp = "unconfined" },
		"writable root":  func(s *harness.Spec) { s.ReadOnlyRoot = false },
		"network":        func(s *harness.Spec) { s.Network = "host" },
		"bridge":         func(s *harness.Spec) { s.Network = "bridge" },
		"metadata":       func(s *harness.Spec) { s.DropMetadata = false },
		"extra mount": func(s *harness.Spec) {
			s.Mounts = append(s.Mounts, harness.Mount{Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"})
		},
		"another's socket": func(s *harness.Spec) { s.Mounts[0].Source = "/run/silo/sessions/other/silo.sock" },
		"silod's dir":      func(s *harness.Spec) { s.Mounts[1].Source = "/run/silo" },
		"no memory limit":  func(s *harness.Spec) { s.MemoryBytes = 0 },
		"no pids limit":    func(s *harness.Spec) { s.Pids = 0 },
		"no cpu share":     func(s *harness.Spec) { s.CPUShares = 0 },
		"entrypoint":       func(s *harness.Spec) { s.Argv = []string{"/bin/sh", "-c", "id"} },
		"state escape":     func(s *harness.Spec) { s.StatePaths = []string{"../other"} },
		"absolute state":   func(s *harness.Spec) { s.StatePaths = []string{"/etc"} },
		"agent path":       func(s *harness.Spec) { s.Agent = "../x" },
	} {
		s := spec(t, 0)
		edit(&s)
		if err := s.Validate(layout); err == nil {
			t.Errorf("%s: a spec with it passes", name)
		}
	}
}

// engine is a fake container runtime that logs its calls.
type engine struct {
	mu      sync.Mutex
	calls   []string
	running map[string]bool
	created map[string]harness.Container
	out     []string
	fwExit  int
}

func (e *engine) log(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, s)
}

func (e *engine) Create(_ context.Context, c harness.Container) error {
	e.log("create " + c.Name)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.created == nil {
		e.created = map[string]harness.Container{}
	}
	e.created[c.Name] = c
	return nil
}
func (e *engine) Start(_ context.Context, n string) error {
	e.log("start " + n)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running == nil {
		e.running = map[string]bool{}
	}
	e.running[n] = true
	return nil
}
func (e *engine) Wait(_ context.Context, n string) (int, error) {
	e.log("wait " + n)
	if strings.HasSuffix(n, "-fw") {
		return e.fwExit, nil
	}
	return 0, nil
}
func (e *engine) Remove(_ context.Context, n string) error {
	e.log("remove " + n)
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.running, n)
	return nil
}
func (e *engine) Running(_ context.Context, n string) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running[n], nil
}
func (e *engine) Labelled(context.Context, string) (map[string]string, error) {
	return map[string]string{"silo-session-a1": "a1", "silo-session-a1-net": "a1", "silo-session-b2": "b2"}, nil
}
func (e *engine) Logs(_ context.Context, n string, line func([]byte)) error {
	e.log("logs " + n)
	for _, l := range e.out {
		line([]byte(l))
	}
	return nil
}

func newSessiond(t *testing.T) (*harness.Sessiond, *engine, harness.Layout) {
	t.Helper()
	dir := t.TempDir()
	lay := harness.Layout{SocketDir: filepath.Join(dir, "sock"), StateDir: filepath.Join(dir, "state"), LinkDir: filepath.Join(dir, "links")}
	e := &engine{}
	var mu sync.Mutex
	owners := map[string]int{}
	d := &harness.Sessiond{Engine: e, Image: "harness@sha256:1", Layout: lay, Owner: os.Getuid(), Chown: func(p string, uid, _ int) error {
		mu.Lock()
		defer mu.Unlock()
		owners[p] = uid
		return nil
	}}
	return d, e, lay
}

func TestSessiondRunsATurn(t *testing.T) {
	d, e, lay := newSessiond(t)
	a := harness.ClaudeCode{}
	s := harness.NewSpec(lay, "a1", 3, a, a.Command(harness.Launch{Model: "m", Prompt: "p", SessionID: "sid"}), harness.Limits{})
	listen(t, lay.Socket("a1"))
	listen(t, lay.Socket("b2"))
	e.out = []string{`{"type":"system","subtype":"init","session_id":"sid"}`}
	var lines []string
	res, err := d.Turn(context.Background(), s, func(b []byte) { lines = append(lines, string(b)) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Exit != 0 || len(lines) != 1 {
		t.Errorf("result %+v, lines %q", res, lines)
	}
	// The namespace and its rule are in place before the harness is
	// created; the harness runs in sessiond's image.
	want := []string{
		"remove silo-session-a1", "remove silo-session-a1-fw", "remove silo-session-a1-net",
		"create silo-session-a1-net", "start silo-session-a1-net",
		"create silo-session-a1-fw", "start silo-session-a1-fw", "wait silo-session-a1-fw", "remove silo-session-a1-fw",
		"remove silo-session-a1", "create silo-session-a1", "start silo-session-a1", "logs silo-session-a1", "wait silo-session-a1",
		"remove silo-session-a1",
	}
	if !slices.Equal(e.calls, want) {
		t.Errorf("calls:\n got %q\nwant %q", e.calls, want)
	}
	c := e.created["silo-session-a1"]
	if c.Image != "harness@sha256:1" || c.User != "30003:30003" {
		t.Errorf("harness container %+v", c)
	}
	// It mounts sessiond's link to the socket, not silod's path.
	if m := c.HostConfig.Mounts[0]; m.Source != lay.Link("a1") || m.Target != harness.SocketPath {
		t.Errorf("socket mount %+v, want %s", m, lay.Link("a1"))
	}
	if fi, err := os.Lstat(lay.Link("a1")); err != nil || fi.Mode().Type() != os.ModeSocket || fi.Mode().Perm() != 0o600 {
		t.Errorf("link %v %v", fi, err)
	}
	if fi, err := os.Stat(lay.Private("a1")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("private directory: %v %v", fi, err)
	}

	// A second turn reuses the namespace.
	e.calls = nil
	if _, err := d.Turn(context.Background(), s, func([]byte) {}); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(e.calls, "create silo-session-a1-fw") {
		t.Errorf("the namespace was made again: %q", e.calls)
	}

	// Another session may not take a1's uid.
	other := harness.NewSpec(lay, "b2", 3, a, a.Command(harness.Launch{Model: "m", Prompt: "p", SessionID: "x"}), harness.Limits{})
	if _, err := d.Turn(context.Background(), other, func([]byte) {}); err == nil || !strings.Contains(err.Error(), "uid") {
		t.Errorf("a second session as uid 30003: %v", err)
	}
	if err := d.Remove(context.Background(), "a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lay.Private("a1")); !os.IsNotExist(err) {
		t.Errorf("the private directory outlives the session: %v", err)
	}
	if _, err := d.Turn(context.Background(), other, func([]byte) {}); err != nil {
		t.Errorf("uid 30003 once a1 is gone: %v", err)
	}
	if list, err := d.List(context.Background()); err != nil || !slices.Equal(list, []string{"a1", "b2"}) {
		t.Errorf("list %v %v", list, err)
	}
}

// listen makes a Unix socket at path, as silod's Sockets.Open does.
func listen(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
}

// TestSessiondTakesOnlySilodsSocket: the socket is in silod's
// directory, so sessiond takes nothing there on trust. A symlink in
// its place (to the runtime's socket, or a file to chmod) or a
// socket that isn't silod's is refused, and nothing runs.
func TestSessiondTakesOnlySilodsSocket(t *testing.T) {
	a := harness.Codex{}
	for name, plant := range map[string]func(t *testing.T, sock string){
		"symlink to a socket": func(t *testing.T, sock string) {
			other := filepath.Join(t.TempDir(), "docker.sock")
			listen(t, other)
			os.MkdirAll(filepath.Dir(sock), 0o700)
			os.Symlink(other, sock)
		},
		"symlink to a file": func(t *testing.T, sock string) {
			f := filepath.Join(t.TempDir(), "shadow")
			os.WriteFile(f, []byte("x"), 0o640)
			os.MkdirAll(filepath.Dir(sock), 0o700)
			os.Symlink(f, sock)
			t.Cleanup(func() {
				if fi, _ := os.Stat(f); fi.Mode().Perm() != 0o640 {
					t.Errorf("the symlink's target was chmodded: %v", fi.Mode())
				}
			})
		},
		"a file": func(t *testing.T, sock string) {
			os.MkdirAll(filepath.Dir(sock), 0o700)
			os.WriteFile(sock, nil, 0o600)
		},
		"nothing": func(*testing.T, string) {},
	} {
		t.Run(name, func(t *testing.T) {
			d, e, lay := newSessiond(t)
			plant(t, lay.Socket("a1"))
			s := harness.NewSpec(lay, "a1", 0, a, a.Command(harness.Launch{Model: "m", Prompt: "p"}), harness.Limits{})
			if _, err := d.Turn(context.Background(), s, func([]byte) {}); err == nil {
				t.Error("the turn ran")
			}
			if len(e.calls) != 0 {
				t.Errorf("the runtime was called: %q", e.calls)
			}
			if _, err := os.Lstat(lay.Link("a1")); !os.IsNotExist(err) {
				t.Errorf("a link was left: %v", err)
			}
		})
	}
	// A socket that isn't silod's.
	d, e, lay := newSessiond(t)
	listen(t, lay.Socket("a1"))
	d.Owner = os.Getuid() + 1
	s := harness.NewSpec(lay, "a1", 0, a, a.Command(harness.Launch{Model: "m", Prompt: "p"}), harness.Limits{})
	if _, err := d.Turn(context.Background(), s, func([]byte) {}); err == nil || len(e.calls) != 0 {
		t.Errorf("another user's socket: %v, calls %q", err, e.calls)
	}
	// A link directory that isn't sessiond's own.
	d, e, lay = newSessiond(t)
	listen(t, lay.Socket("a1"))
	os.Symlink(t.TempDir(), lay.LinkDir)
	if _, err := d.Turn(context.Background(), s, func([]byte) {}); err == nil || len(e.calls) != 0 {
		t.Errorf("a symlinked link directory: %v, calls %q", err, e.calls)
	}
}

func TestSessiondRefuses(t *testing.T) {
	d, e, lay := newSessiond(t)
	listen(t, lay.Socket("a1"))
	s := harness.NewSpec(lay, "a1", 0, harness.Codex{}, harness.Codex{}.Command(harness.Launch{Model: "m", Prompt: "p"}), harness.Limits{})
	bad := s
	bad.Network = "host"
	if _, err := d.Turn(context.Background(), bad, func([]byte) {}); err == nil {
		t.Error("a spec with the host's network ran")
	}
	img := s
	img.Image = "attacker/shell"
	if _, err := d.Turn(context.Background(), img, func([]byte) {}); err == nil {
		t.Error("another image ran")
	}
	if len(e.calls) != 0 {
		t.Errorf("the runtime was called for refused specs: %q", e.calls)
	}
	// No namespace without its rule: the harness doesn't run.
	e.fwExit = 1
	if _, err := d.Turn(context.Background(), s, func([]byte) {}); err == nil {
		t.Error("a harness ran without the metadata rule")
	}
	if slices.Contains(e.calls, "create silo-session-a1") {
		t.Errorf("the harness was created: %q", e.calls)
	}
}

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, ".claude", "projects", "-state-work")
	os.MkdirAll(proj, 0o700)
	os.WriteFile(filepath.Join(proj, "sid.jsonl"), []byte(`{"type":"user"}`+"\n"), 0o600)
	secret := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secret, []byte("key"), 0o600)
	os.Symlink(secret, filepath.Join(proj, "leak.jsonl"))
	os.WriteFile(filepath.Join(dir, "other"), []byte("not state"), 0o600)
	st, err := harness.SaveState(dir, []string{".claude/projects"})
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := harness.RestoreState(out, []string{".claude/projects"}, st); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(out, ".claude", "projects", "-state-work", "sid.jsonl")); err != nil || string(b) != `{"type":"user"}`+"\n" {
		t.Errorf("session file: %q %v", b, err)
	}
	for _, p := range []string{".claude/projects/-state-work/leak.jsonl", "other"} {
		if _, err := os.Lstat(filepath.Join(out, p)); !os.IsNotExist(err) {
			t.Errorf("%s was saved: %v", p, err)
		}
	}
	// A state path reached through a symlink saves nothing of where it
	// points.
	evil := t.TempDir()
	os.Symlink(filepath.Dir(secret), filepath.Join(evil, ".claude"))
	st, err = harness.SaveState(evil, []string{".claude"})
	if err != nil {
		t.Fatal(err)
	}
	out2 := t.TempDir()
	if err := harness.RestoreState(out2, []string{".claude"}, st); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(out2, ".claude", "secret")); !os.IsNotExist(err) {
		t.Errorf("a file outside the private directory was saved through a symlink: %v", err)
	}
	// A restore can't write outside the state paths.
	if err := harness.RestoreState(t.TempDir(), []string{".codex/sessions"}, st0(t)); err == nil {
		t.Error("restored a file outside the state paths")
	}
}

func st0(t *testing.T) []byte {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".claude", "projects"), 0o700)
	os.WriteFile(filepath.Join(dir, ".claude", "projects", "x"), []byte("x"), 0o600)
	st, err := harness.SaveState(dir, []string{".claude/projects"})
	if err != nil {
		t.Fatal(err)
	}
	return st
}
