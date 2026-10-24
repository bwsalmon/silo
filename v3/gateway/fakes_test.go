package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

type fakeUnit struct {
	spec  UnitSpec
	props []Property // set-property calls
	state string     // active, failed, inactive
	pid   int
}

// fakeSystemd keeps units in memory. Stopping a slice stops every unit in it
// and in the slices below it.
type fakeSystemd struct {
	mu         sync.Mutex
	units      map[string]*fakeUnit
	calls      []string
	nextPID    int
	failNext   map[string]error // method -> error, consumed once
	ignoreStop bool             // units survive Stop
}

func newFakeSystemd() *fakeSystemd {
	return &fakeSystemd{units: map[string]*fakeUnit{}, nextPID: 1000, failNext: map[string]error{}}
}

func (f *fakeSystemd) fail(method string) error {
	if err, ok := f.failNext[method]; ok {
		delete(f.failNext, method)
		return err
	}
	return nil
}

func (f *fakeSystemd) StartService(_ context.Context, spec UnitSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "start "+spec.Name)
	if err := f.fail("start"); err != nil {
		return err
	}
	if u := f.units[spec.Name]; u != nil && u.state == "active" {
		return fmt.Errorf("unit %s already exists", spec.Name)
	}
	f.nextPID++
	f.units[spec.Name] = &fakeUnit{spec: spec, state: "active", pid: f.nextPID}
	return nil
}

func (f *fakeSystemd) SetProperties(_ context.Context, unit string, props []Property) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "set-property "+unit)
	if err := f.fail("set-property"); err != nil {
		return err
	}
	u := f.units[unit]
	if u == nil {
		u = &fakeUnit{state: "active"}
		f.units[unit] = u
	}
	u.props = append(u.props, props...)
	return nil
}

// inSlice reports whether a unit placed in unitSlice lies under slice.
func inSlice(unitSlice, slice string) bool {
	return unitSlice == slice || strings.HasPrefix(unitSlice, strings.TrimSuffix(slice, ".slice")+"-")
}

func (f *fakeSystemd) Stop(_ context.Context, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "stop "+unit)
	if f.ignoreStop {
		return nil
	}
	if strings.HasSuffix(unit, ".slice") {
		for name, u := range f.units {
			if strings.HasSuffix(name, ".service") && inSlice(u.spec.Slice, unit) {
				delete(f.units, name)
			}
		}
		delete(f.units, unit)
		return nil
	}
	if f.units[unit] == nil {
		return fmt.Errorf("unit %s not loaded", unit)
	}
	delete(f.units, unit)
	return nil
}

func (f *fakeSystemd) Kill(_ context.Context, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "kill "+unit)
	return nil
}

func (f *fakeSystemd) ResetFailed(_ context.Context, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "reset-failed "+unit)
	if u := f.units[unit]; u != nil && u.state == "failed" {
		delete(f.units, unit)
	}
	return nil
}

func (f *fakeSystemd) Status(_ context.Context, unit string) (UnitStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("status"); err != nil {
		return UnitStatus{}, err
	}
	u := f.units[unit]
	if u == nil {
		return UnitStatus{}, nil
	}
	st := UnitStatus{Exists: true, ActiveState: u.state, SubState: "running", MainPID: u.pid}
	if u.state == "failed" {
		st.SubState, st.Result, st.ExecMainStatus, st.MainPID = "failed", "exit-code", 3, 0
	}
	return st, nil
}

// exit makes a unit's process end with state ("failed" keeps the unit
// loaded, "gone" removes it like a transient unit that exited cleanly).
func (f *fakeSystemd) exit(unit, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if state == "gone" {
		delete(f.units, unit)
		return
	}
	f.units[unit].state = state
}

func (f *fakeSystemd) unit(name string) *fakeUnit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.units[name]
}

func (f *fakeSystemd) prop(unit, key string) (string, bool) {
	u := f.unit(unit)
	if u == nil {
		return "", false
	}
	for _, p := range append(append([]Property{}, u.spec.Properties...), u.props...) {
		if p.Key == key {
			return p.Value, true
		}
	}
	return "", false
}

type fakeUsers struct {
	mu     sync.Mutex
	users  map[string]*UserInfo
	groups map[string]int
}

func newFakeUsers() *fakeUsers {
	return &fakeUsers{
		users: map[string]*UserInfo{
			"root": {Name: "root", UID: 0, GID: 0, Home: "/root"},
			"silo": {Name: "silo", UID: 998, GID: 998, Home: "/var/lib/silo"},
		},
		groups: map[string]int{},
	}
}

func (u *fakeUsers) Lookup(name string) (*UserInfo, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if v := u.users[name]; v != nil {
		c := *v
		return &c, nil
	}
	return nil, nil
}

func (u *fakeUsers) UIDTaken(uid int) (bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, v := range u.users {
		if v.UID == uid {
			return true, nil
		}
	}
	return false, nil
}

func (u *fakeUsers) Create(name string, uid int, home string) (*UserInfo, error) {
	u.mu.Lock()
	if u.users[name] != nil {
		u.mu.Unlock()
		return nil, fmt.Errorf("useradd: user %s exists", name)
	}
	u.users[name] = &UserInfo{Name: name, UID: uid, GID: uid, Home: home}
	u.mu.Unlock()
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	return u.Lookup(name)
}

func (u *fakeUsers) EnsureGroup(name string) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if gid, ok := u.groups[name]; ok {
		return gid, nil
	}
	u.groups[name] = 900 + len(u.groups)
	return u.groups[name], nil
}

type fakeProcs map[int]string

func (p fakeProcs) Cgroup(pid int) (string, error) {
	if cg, ok := p[pid]; ok {
		return cg, nil
	}
	return "", fmt.Errorf("no such process %d", pid)
}

// env is a gateway wired to fakes in a temp dir.
type env struct {
	t     *testing.T
	dir   string
	cfg   *Config
	sd    *fakeSystemd
	users *fakeUsers
	procs fakeProcs
	clock *fakeClock
	audit *AuditLog
	g     *Gateway
}

func newEnv(t *testing.T, mutate ...func(*Config)) *env {
	t.Helper()
	dir := t.TempDir()
	cfg := &Config{
		StateDir:   filepath.Join(dir, "state"),
		HomeRoot:   filepath.Join(dir, "home"),
		AuditLog:   filepath.Join(dir, "log", "audit.jsonl"),
		SocketPath: filepath.Join(dir, "gateway.sock"),
		CLIsDir:    filepath.Join(dir, "clis.d"),
		AdminUIDs:  []int{1000},
	}
	for _, m := range mutate {
		m(cfg)
	}
	cfg.setDefaults()
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, dir: dir, cfg: cfg, sd: newFakeSystemd(), users: newFakeUsers(), procs: fakeProcs{},
		clock: &fakeClock{t: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}}
	e.restart()
	return e
}

// restart builds a fresh Gateway on the same state, audit log and host, as
// a gateway restart would, and starts it.
func (e *env) restart() {
	e.t.Helper()
	if e.audit != nil {
		e.audit.Close()
	}
	a, err := OpenAuditLog(e.cfg.AuditLog, e.clock, false)
	if err != nil {
		e.t.Fatal(err)
	}
	e.audit = a
	e.t.Cleanup(func() { a.Close() })
	clis := BuiltinCLIs()
	clis["claude"] = CLIDef{Name: "claude", Command: []string{"/opt/silo/cli/claude", "--print", "{brief}"}, Env: map[string]string{"X": "1"}}
	g, err := New(e.cfg, Deps{
		Systemd: e.sd, Users: e.users, Procs: e.procs, Clock: e.clock,
		Launcher: &CLILauncher{CLIs: clis}, Audit: a, Store: Store{Dir: e.cfg.StateDir},
		Logf: e.t.Logf,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	if err := g.Start(context.Background()); err != nil {
		e.t.Fatal(err)
	}
	e.g = g
}

var admin = Caller{UID: 0, PID: 1, Role: RoleAdmin}

func (e *env) asAgent(id string) Caller {
	return Caller{UID: e.g.st.Agents[id].UID, Role: RoleAgent, Agent: id}
}

func (e *env) spawn(c Caller, p SpawnParams) *SpawnResult {
	e.t.Helper()
	if p.CLI == "" {
		p.CLI = "claude"
	}
	r, err := e.g.Spawn(context.Background(), c, p)
	if err != nil {
		e.t.Fatalf("spawn %s: %v", p.Name, err)
	}
	return r
}

func (e *env) auditEvents() []AuditEntry {
	e.t.Helper()
	entries, err := e.audit.Read(0, 10000)
	if err != nil {
		e.t.Fatal(err)
	}
	return entries
}

func (e *env) lastEvent(event string) map[string]any {
	e.t.Helper()
	entries := e.auditEvents()
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Event == event {
			var m map[string]any
			if err := json.Unmarshal(entries[i].Payload, &m); err != nil {
				e.t.Fatal(err)
			}
			m["_actor"] = entries[i].Actor
			return m
		}
	}
	e.t.Fatalf("no %s event in the audit log", event)
	return nil
}
