package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var ctx = context.Background()

func codeOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

func TestStartCreatesRootAgent(t *testing.T) {
	e := newEnv(t)
	r := e.g.st.Agents[RootAgentID]
	if r == nil || r.State != StateRunning || !r.Root {
		t.Fatalf("root agent = %+v", r)
	}
	if r.User != RootUser || r.UID == 0 || r.UID != 61000 {
		t.Fatalf("root user %s uid %d", r.User, r.UID)
	}
	if r.Unit != "silo-agent-root.service" || r.Slice != "silo.slice" {
		t.Fatalf("root unit %s slice %s", r.Unit, r.Slice)
	}
	u := e.sd.unit(r.Unit)
	if u == nil || !reflect.DeepEqual(u.spec.Argv, []string{"/bin/sleep", "infinity"}) {
		t.Fatalf("root unit %+v", u)
	}
	for k, want := range map[string]string{
		"User": RootUser, "NoNewPrivileges": "yes", "PrivateTmp": "yes", "ProtectSystem": "full",
		"AmbientCapabilities": "CAP_DAC_READ_SEARCH", "CapabilityBoundingSet": "CAP_DAC_READ_SEARCH",
		"MemoryMax": "4G", "CPUQuota": "200%",
	} {
		if got, _ := e.sd.prop(r.Unit, k); got != want {
			t.Errorf("root %s = %q, want %q", k, got, want)
		}
	}
	inacc, _ := e.sd.prop(r.Unit, "InaccessiblePaths")
	for _, p := range []string{"/etc/silo/secrets", e.cfg.StateDir, filepath.Dir(e.cfg.AuditLog), e.cfg.SocketPath} {
		if !strings.Contains(inacc, "-"+p) {
			t.Errorf("InaccessiblePaths %q lacks %s", inacc, p)
		}
	}
	fi, err := os.Stat(r.Home)
	if err != nil || r.Home != filepath.Join(e.cfg.HomeRoot, RootUser) {
		t.Fatalf("root home %s: %v", r.Home, err)
	}
	_ = fi
	if ev := e.lastEvent(EvSpawn); ev["agent"] != "root" || ev["_actor"] != "gateway" {
		t.Fatalf("spawn audit %v", ev)
	}
	e.lastEvent(EvGatewayStart)
	e.lastEvent(EvUserCreate)

	// A second start leaves the running root alone.
	e.restart()
	if n := strings.Count(strings.Join(e.sd.calls, "\n"), "start silo-agent-root.service"); n != 1 {
		t.Fatalf("root started %d times", n)
	}
	e.lastEvent(EvAdopt)
}

func TestRootDisabled(t *testing.T) {
	off := false
	e := newEnv(t, func(c *Config) { c.RootAgent.Enabled = &off })
	if len(e.g.st.Agents) != 0 {
		t.Fatalf("agents %v", e.g.st.Agents)
	}
}

func TestSpawnChildOfRoot(t *testing.T) {
	e := newEnv(t)
	root := e.asAgent(RootAgentID)
	r := e.spawn(root, SpawnParams{Name: "web-dev", Brief: "build it", Limits: Limits{MemoryMax: "1G"}, Lifetime: Lifetime{Kind: LifetimeTTL, Seconds: 600}})
	want := SpawnResult{Agent: "web-dev", User: "silo-a-web-dev", Unit: "silo-agent-web-dev.service", Slice: `silo-agent-web\x2ddev.slice`}
	if *r != want {
		t.Fatalf("result %+v, want %+v", *r, want)
	}
	a := e.g.st.Agents["web-dev"]
	if a.Parent != RootAgentID || a.UID != 61001 || a.SpawnedBy != "agent:root" {
		t.Fatalf("agent %+v", a)
	}
	u := e.sd.unit(a.Unit)
	if u.spec.Slice != want.Slice {
		t.Fatalf("slice %s", u.spec.Slice)
	}
	if !reflect.DeepEqual(u.spec.Argv, []string{"/opt/silo/cli/claude", "--print", "build it"}) {
		t.Fatalf("argv %v", u.spec.Argv)
	}
	if u.spec.Env["SILO_AGENT"] != "web-dev" || u.spec.Env["HOME"] != a.Home || u.spec.Env["X"] != "1" {
		t.Fatalf("env %v", u.spec.Env)
	}
	for k, want := range map[string]string{
		"User": "silo-a-web-dev", "ProtectHome": "tmpfs", "BindPaths": a.Home,
		"TemporaryFileSystem": e.cfg.HomeRoot + ":ro", "CapabilityBoundingSet": "",
		"MemoryMax": "1G", "CPUQuota": "100%", "TasksMax": "512", "RuntimeMaxSec": "600",
	} {
		if got, ok := e.sd.prop(a.Unit, k); !ok || got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if _, ok := e.sd.prop(a.Unit, "AmbientCapabilities"); ok {
		t.Error("child has ambient capabilities")
	}
	// The subtree's slice carries the same limits.
	if got, _ := e.sd.prop(want.Slice, "MemoryMax"); got != "1G" {
		t.Errorf("slice MemoryMax %q", got)
	}
	if ev := e.lastEvent(EvSpawn); ev["parent"] != "root" || ev["brief"] != "build it" || ev["_actor"] != "agent:root" {
		t.Fatalf("audit %v", ev)
	}
}

func TestSpawnNesting(t *testing.T) {
	e := newEnv(t)
	e.spawn(admin, SpawnParams{Name: "a"})
	r := e.spawn(admin, SpawnParams{Name: "b", Parent: "a"})
	if r.Slice != "silo-agent-a-b.slice" {
		t.Fatalf("slice %s", r.Slice)
	}
	r = e.spawn(admin, SpawnParams{Name: "c-1", Parent: "b"})
	if r.Slice != `silo-agent-a-b-c\x2d1.slice` {
		t.Fatalf("slice %s", r.Slice)
	}
	_, err := e.g.Spawn(ctx, admin, SpawnParams{Name: "d", Parent: "c-1", CLI: "claude"})
	if codeOf(err) != CodeInvalidParams || !strings.Contains(err.Error(), "levels below root") {
		t.Fatalf("depth 4: %v", err)
	}
	if e.g.st.Agents["a"].Parent != "" || e.g.st.Agents["b"].Parent != "a" {
		t.Fatal("parents not recorded")
	}
}

func TestSpawnAuthorization(t *testing.T) {
	e := newEnv(t)
	e.spawn(admin, SpawnParams{Name: "kid"})
	kid := e.asAgent("kid")
	_, err := e.g.Spawn(ctx, kid, SpawnParams{Name: "grandkid", CLI: "claude"})
	if codeOf(err) != CodeForbidden {
		t.Fatalf("child spawn: %v", err)
	}
	if ev := e.lastEvent(EvAuthDeny); ev["method"] != "spawn" || ev["_actor"] != "agent:kid" {
		t.Fatalf("deny audit %v", ev)
	}
	// Root cannot place a child under someone else.
	_, err = e.g.Spawn(ctx, e.asAgent(RootAgentID), SpawnParams{Name: "x", Parent: "kid", CLI: "claude"})
	if codeOf(err) != CodeForbidden {
		t.Fatalf("root spawning under kid: %v", err)
	}
	// The bridge (the UI) spawns for the human.
	e.spawn(Caller{UID: 998, Role: RoleBridge}, SpawnParams{Name: "ui"})
	if e.g.st.Agents["ui"].SpawnedBy != "bridge" {
		t.Fatal("bridge actor")
	}
}

func TestSpawnValidation(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct {
		p    SpawnParams
		code int
		msg  string
	}{
		{SpawnParams{Name: "Bad", CLI: "claude"}, CodeInvalidParams, "name"},
		{SpawnParams{Name: "root", CLI: "claude"}, CodeInvalidParams, "reserved"},
		{SpawnParams{Name: "a-", CLI: "claude"}, CodeInvalidParams, "name"},
		{SpawnParams{Name: "a", User: "alice", CLI: "claude"}, CodeInvalidParams, "user"},
		{SpawnParams{Name: "a", User: RootUser, CLI: "claude"}, CodeInvalidParams, "user"},
		{SpawnParams{Name: "a", CLI: "nope"}, CodeInvalidParams, "unknown cli"},
		{SpawnParams{Name: "a", CLI: "claude", Limits: Limits{MemoryMax: "2G\nUser=root"}}, CodeInvalidParams, "memory_max"},
		{SpawnParams{Name: "a", CLI: "claude", Limits: Limits{CPUQuota: "2"}}, CodeInvalidParams, "cpu_quota"},
		{SpawnParams{Name: "a", CLI: "claude", Limits: Limits{DiskQuota: "1G"}}, CodeInvalidParams, "disk_quota"},
		{SpawnParams{Name: "a", CLI: "claude", Lifetime: Lifetime{Kind: "ttl"}}, CodeInvalidParams, "ttl"},
		{SpawnParams{Name: "a", CLI: "claude", Lifetime: Lifetime{Kind: "forever"}}, CodeInvalidParams, "lifetime"},
		{SpawnParams{Name: "a", CLI: "claude", Parent: "ghost"}, CodeInvalidParams, "parent"},
	} {
		_, err := e.g.Spawn(ctx, admin, tc.p)
		if codeOf(err) != tc.code || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%+v: got %v, want code %d containing %q", tc.p, err, tc.code, tc.msg)
		}
	}
	if _, ok := e.g.st.Agents["a"]; ok {
		t.Fatal("an invalid spawn was recorded")
	}
}

func TestSpawnUsers(t *testing.T) {
	e := newEnv(t)
	// A user silo did not create is never used.
	e.users.users["silo-a-mallory"] = &UserInfo{Name: "silo-a-mallory", UID: 1001, Home: "/home/m"}
	_, err := e.g.Spawn(ctx, admin, SpawnParams{Name: "mallory", CLI: "claude"})
	if codeOf(err) != CodeForbidden {
		t.Fatalf("foreign user: %v", err)
	}
	if ev := e.lastEvent(EvSpawnFailed); ev["agent"] != "mallory" {
		t.Fatalf("spawn.failed audit %v", ev)
	}
	// uid allocation skips taken uids.
	e.users.users["other"] = &UserInfo{Name: "other", UID: 61001}
	e.spawn(admin, SpawnParams{Name: "a"})
	if uid := e.g.st.Agents["a"].UID; uid != 61002 {
		t.Fatalf("uid %d", uid)
	}
	// An existing silo user can be reused once its agent has stopped...
	_, err = e.g.Spawn(ctx, admin, SpawnParams{Name: "b", User: "silo-a-a", CLI: "claude"})
	if codeOf(err) != CodeConflict {
		t.Fatalf("user in use: %v", err)
	}
	if _, err := e.g.Stop(ctx, admin, StopParams{Agent: "a"}); err != nil {
		t.Fatal(err)
	}
	e.spawn(admin, SpawnParams{Name: "b", User: "silo-a-a"})
	if e.g.st.Agents["b"].UID != 61002 {
		t.Fatal("reused user has a new uid")
	}
	// ...and an id can be reused, keeping the old tombstone in history.
	if _, err := e.g.Stop(ctx, admin, StopParams{Agent: "b"}); err != nil {
		t.Fatal(err)
	}
	e.spawn(admin, SpawnParams{Name: "a"})
	if len(e.g.st.History) != 1 || e.g.st.History[0].ID != "a" || e.g.st.History[0].Tombstone == nil {
		t.Fatalf("history %+v", e.g.st.History)
	}
	_, err = e.g.Spawn(ctx, admin, SpawnParams{Name: "a", CLI: "claude"})
	if codeOf(err) != CodeConflict {
		t.Fatalf("running id: %v", err)
	}
}

func TestSpawnStartFailure(t *testing.T) {
	e := newEnv(t)
	e.sd.failNext["start"] = errors.New("boom")
	if _, err := e.g.Spawn(ctx, admin, SpawnParams{Name: "a", CLI: "claude"}); err == nil {
		t.Fatal("no error")
	}
	if _, ok := e.g.st.Agents["a"]; ok {
		t.Fatal("failed spawn recorded as an agent")
	}
	e.sd.failNext["set-property"] = errors.New("boom")
	if _, err := e.g.Spawn(ctx, admin, SpawnParams{Name: "a", CLI: "claude"}); err == nil {
		t.Fatal("no error")
	}
	if e.sd.unit("silo-agent-a.service") != nil {
		t.Fatal("unit left running after the slice could not be limited")
	}
}

func TestStopSubtree(t *testing.T) {
	e := newEnv(t)
	e.spawn(admin, SpawnParams{Name: "p"})
	e.spawn(admin, SpawnParams{Name: "c1", Parent: "p"})
	e.spawn(admin, SpawnParams{Name: "c2", Parent: "p"})
	e.spawn(admin, SpawnParams{Name: "g", Parent: "c1"})
	e.spawn(admin, SpawnParams{Name: "other"})
	e.clock.Advance(time.Minute)

	res, err := e.g.Stop(ctx, admin, StopParams{Agent: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Stopped, []string{"p", "c1", "c2", "g"}) {
		t.Fatalf("stopped %v", res.Stopped)
	}
	ts := res.Tombstone
	if ts.Cause != "stopped by admin:uid=0" || ts.LastState != "active/running" || !reflect.DeepEqual(ts.Children, []string{"c1", "c2"}) {
		t.Fatalf("tombstone %+v", ts)
	}
	if !ts.Time.Equal(e.clock.Now()) {
		t.Fatalf("tombstone time %v", ts.Time)
	}
	for id, cause := range map[string]string{"c1": "parent p stopped", "c2": "parent p stopped", "g": "parent c1 stopped"} {
		a := e.g.st.Agents[id]
		if a.State != StateStopped || a.Tombstone.Cause != cause {
			t.Errorf("%s: %s %+v", id, a.State, a.Tombstone)
		}
		if e.sd.unit(a.Unit) != nil {
			t.Errorf("%s still has a unit", id)
		}
	}
	if !reflect.DeepEqual(e.g.st.Agents["c1"].Tombstone.Children, []string{"g"}) {
		t.Error("c1 tombstone children")
	}
	if e.g.st.Agents["other"].State != StateRunning || e.g.st.Agents[RootAgentID].State != StateRunning {
		t.Fatal("stopped an unrelated agent")
	}
	if ev := e.lastEvent(EvStop); ev["agent"] != "p" || len(ev["stopped"].([]any)) != 4 {
		t.Fatalf("stop audit %v", ev)
	}
	if _, err := e.g.Stop(ctx, admin, StopParams{Agent: "p"}); codeOf(err) != CodeConflict {
		t.Fatalf("second stop: %v", err)
	}
	if _, err := e.g.Stop(ctx, admin, StopParams{Agent: "nobody"}); codeOf(err) != CodeNotFound {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := e.g.Stop(ctx, admin, StopParams{Agent: "other", Mode: "nicely"}); codeOf(err) != CodeInvalidParams {
		t.Fatalf("bad mode: %v", err)
	}
}

func TestStopKillAndRoot(t *testing.T) {
	e := newEnv(t)
	root := e.asAgent(RootAgentID)
	e.spawn(root, SpawnParams{Name: "k"})
	res, err := e.g.Stop(ctx, admin, StopParams{Agent: RootAgentID, Mode: "kill"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Stopped, []string{"root", "k"}) {
		t.Fatalf("stopped %v", res.Stopped)
	}
	calls := strings.Join(e.sd.calls, "\n")
	for _, c := range []string{"kill silo-agent-root.service", "kill silo-agent-k.service", "stop silo-agent-k.slice", "stop silo-agent-root.service"} {
		if !strings.Contains(calls, c) {
			t.Errorf("no %q in calls:\n%s", c, calls)
		}
	}
	if strings.Contains(calls, "stop silo.slice") {
		t.Error("stopped silo.slice")
	}
}

func TestStopUnitThatWillNotStop(t *testing.T) {
	e := newEnv(t)
	e.spawn(admin, SpawnParams{Name: "stuck"})
	e.sd.ignoreStop = true
	_, err := e.g.Stop(ctx, admin, StopParams{Agent: "stuck"})
	if err == nil || !strings.Contains(err.Error(), "stuck") {
		t.Fatalf("err %v", err)
	}
	if e.g.st.Agents["stuck"].State != StateRunning {
		t.Fatal("tombstoned a running unit")
	}
}

func TestStopAuthorization(t *testing.T) {
	e := newEnv(t)
	root := e.asAgent(RootAgentID)
	e.spawn(root, SpawnParams{Name: "a"})
	e.spawn(root, SpawnParams{Name: "b"})
	e.spawn(admin, SpawnParams{Name: "a1", Parent: "a"})
	a := e.asAgent("a")
	for _, target := range []string{"b", "a", RootAgentID} {
		if _, err := e.g.Stop(ctx, a, StopParams{Agent: target}); codeOf(err) != CodeForbidden {
			t.Errorf("a stopping %s: %v", target, err)
		}
	}
	if ev := e.lastEvent(EvAuthDeny); ev["method"] != "stop" {
		t.Fatalf("deny audit %v", ev)
	}
	if _, err := e.g.Stop(ctx, a, StopParams{Agent: "a1"}); err != nil {
		t.Fatalf("a stopping its child: %v", err)
	}
	if _, err := e.g.Stop(ctx, root, StopParams{Agent: "b"}); err != nil {
		t.Fatalf("root stopping its child: %v", err)
	}
	if got := e.g.st.Agents["b"].Tombstone.Cause; got != "stopped by agent:root" {
		t.Fatalf("cause %q", got)
	}
}

func TestListAndStatus(t *testing.T) {
	e := newEnv(t)
	root := e.asAgent(RootAgentID)
	e.spawn(root, SpawnParams{Name: "a", Brief: "secret-ish brief"})
	e.spawn(root, SpawnParams{Name: "b"})
	e.spawn(admin, SpawnParams{Name: "a1", Parent: "a"})
	if _, err := e.g.Stop(ctx, admin, StopParams{Agent: "b"}); err != nil {
		t.Fatal(err)
	}
	ids := func(infos []AgentInfo) []string {
		var out []string
		for _, i := range infos {
			out = append(out, i.ID)
		}
		return out
	}
	all, _ := e.g.List(ctx, admin, ListParams{})
	if got := ids(all); !reflect.DeepEqual(got, []string{"a", "a1", "b", "root"}) {
		t.Fatalf("admin list %v", got)
	}
	for _, i := range all {
		if i.Brief != "" {
			t.Fatal("list includes briefs")
		}
		if i.ID == "a" && !reflect.DeepEqual(i.Children, []string{"a1"}) {
			t.Fatalf("children %v", i.Children)
		}
	}
	running, _ := e.g.List(ctx, admin, ListParams{State: []string{StateRunning}})
	if got := ids(running); !reflect.DeepEqual(got, []string{"a", "a1", "root"}) {
		t.Fatalf("running %v", got)
	}
	parent := "root"
	kids, _ := e.g.List(ctx, admin, ListParams{Parent: &parent})
	if got := ids(kids); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("root's children %v", got)
	}
	mine, _ := e.g.List(ctx, e.asAgent("a"), ListParams{})
	if got := ids(mine); !reflect.DeepEqual(got, []string{"a", "a1"}) {
		t.Fatalf("a's view %v", got)
	}

	st, err := e.g.Status(ctx, admin, StatusParams{Agent: "a"})
	if err != nil || st.Brief != "secret-ish brief" || st.UnitNow == nil || st.UnitNow.ActiveState != "active" {
		t.Fatalf("status %+v %v", st, err)
	}
	if _, err := e.g.Status(ctx, e.asAgent("a"), StatusParams{Agent: "b"}); codeOf(err) != CodeNotFound {
		t.Fatalf("a seeing b: %v", err)
	}
	st, _ = e.g.Status(ctx, admin, StatusParams{Agent: "b"})
	if st.UnitNow != nil || st.Tombstone == nil {
		t.Fatalf("stopped status %+v", st)
	}
}

func TestRestartAdoptsAndTombstones(t *testing.T) {
	e := newEnv(t)
	e.spawn(admin, SpawnParams{Name: "alive"})
	e.spawn(admin, SpawnParams{Name: "dead"})
	e.spawn(admin, SpawnParams{Name: "dead-kid", Parent: "dead"})
	// While the gateway is down, "dead" exits; its child keeps running.
	e.sd.exit("silo-agent-dead.service", "failed")
	e.restart()

	if a := e.g.st.Agents["alive"]; a.State != StateRunning {
		t.Fatalf("alive: %s", a.State)
	}
	d := e.g.st.Agents["dead"]
	if d.State != StateExited || d.Tombstone.Cause != "exited while the gateway was down" ||
		d.Tombstone.LastState != "failed/failed" || d.Tombstone.Result != "exit-code" || d.Tombstone.ExitStatus != 3 {
		t.Fatalf("dead: %s %+v", d.State, d.Tombstone)
	}
	k := e.g.st.Agents["dead-kid"]
	if k.State != StateStopped || k.Tombstone.Cause != "parent dead exited" {
		t.Fatalf("dead-kid: %s %+v", k.State, k.Tombstone)
	}
	if e.sd.unit("silo-agent-dead.service") != nil {
		t.Fatal("failed unit not reset")
	}
	if ev := e.lastEvent(EvAdopt); ev["agent"] == nil {
		t.Fatal("no adopt event")
	}
	// The state survives on disk: a third start sees the same tombstones.
	e.restart()
	if e.g.st.Agents["dead"].State != StateExited {
		t.Fatal("tombstone lost across restart")
	}
}

func TestReconcileExitAndRootRestart(t *testing.T) {
	e := newEnv(t)
	e.spawn(admin, SpawnParams{Name: "w"})
	e.sd.exit("silo-agent-w.service", "gone")
	if err := e.g.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	w := e.g.st.Agents["w"]
	if w.State != StateExited || w.Tombstone.Cause != "exited" || w.Tombstone.LastState != "gone" {
		t.Fatalf("w %s %+v", w.State, w.Tombstone)
	}
	if ev := e.lastEvent(EvExit); ev["agent"] != "w" {
		t.Fatalf("exit audit %v", ev)
	}

	// Root that dies at once is not restarted until the next gateway start.
	e.sd.exit("silo-agent-root.service", "failed")
	if err := e.g.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if e.g.st.Agents[RootAgentID].State != StateExited {
		t.Fatal("root restarted in a loop")
	}
	e.restart()
	if e.g.st.Agents[RootAgentID].State != StateRunning || len(e.g.st.History) != 1 {
		t.Fatal("root not restarted at gateway start")
	}
	// Root that ran for a while is restarted by the reconciler.
	e.clock.Advance(2 * time.Minute)
	e.sd.exit("silo-agent-root.service", "gone")
	if err := e.g.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if r := e.g.st.Agents[RootAgentID]; r.State != StateRunning || r.UID != 61000 {
		t.Fatalf("root after crash: %+v", r)
	}
}

func TestReconcileStatusError(t *testing.T) {
	e := newEnv(t)
	e.sd.failNext["status"] = errors.New("dbus down")
	if err := e.g.Reconcile(ctx); err == nil {
		t.Fatal("no error")
	}
	if e.g.st.Agents[RootAgentID].State != StateRunning {
		t.Fatal("a status error tombstoned an agent")
	}
}

func TestIdentify(t *testing.T) {
	e := newEnv(t)
	e.spawn(admin, SpawnParams{Name: "a"})
	a := e.g.st.Agents["a"]
	e.procs[4242] = "/silo.slice/silo-agent.slice/silo-agent-a.slice/" + a.Unit
	e.procs[4343] = "/user.slice/user-1000.slice/session-1.scope"
	e.g.bridgeUID = 998

	for _, tc := range []struct {
		name  string
		cred  PeerCred
		relay *OnBehalf
		want  Caller
		err   string
	}{
		{"root", PeerCred{UID: 0, PID: 1}, nil, Caller{UID: 0, PID: 1, Role: RoleAdmin}, ""},
		{"admin uid", PeerCred{UID: 1000, PID: 2}, nil, Caller{UID: 1000, PID: 2, Role: RoleAdmin}, ""},
		{"bridge", PeerCred{UID: 998, PID: 3}, nil, Caller{UID: 998, PID: 3, Role: RoleBridge}, ""},
		{"agent", PeerCred{UID: a.UID, PID: 4242}, nil, Caller{UID: a.UID, PID: 4242, Role: RoleAgent, Agent: "a"}, ""},
		{"agent uid outside its unit", PeerCred{UID: a.UID, PID: 4343}, nil, Caller{}, "not in"},
		{"stranger", PeerCred{UID: 1234, PID: 5}, nil, Caller{}, "not an admin"},
		{"relay", PeerCred{UID: 998, PID: 3}, &OnBehalf{Agent: "a", PID: 4242}, Caller{UID: 998, PID: 4242, Role: RoleAgent, Agent: "a"}, ""},
		{"relay wrong pid", PeerCred{UID: 998, PID: 3}, &OnBehalf{Agent: "a", PID: 4343}, Caller{}, "not in"},
		{"relay dead pid", PeerCred{UID: 998, PID: 3}, &OnBehalf{Agent: "a", PID: 1}, Caller{}, "cannot read"},
		{"relay unknown agent", PeerCred{UID: 998, PID: 3}, &OnBehalf{Agent: "zz", PID: 4242}, Caller{}, "not running"},
		{"relay by non-bridge", PeerCred{UID: 1000, PID: 2}, &OnBehalf{Agent: "a", PID: 4242}, Caller{}, "only the bridge"},
	} {
		got, err := e.g.identify(tc.cred, tc.relay)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: err %v, want %q", tc.name, err, tc.err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: got %+v, %v; want %+v", tc.name, got, err, tc.want)
		}
	}
}

func TestStatePersistence(t *testing.T) {
	dir := t.TempDir()
	s := Store{Dir: filepath.Join(dir, "x")}
	st, err := s.Load()
	if err != nil || len(st.Agents) != 0 {
		t.Fatalf("empty load: %+v %v", st, err)
	}
	st.Agents["a"] = &Agent{ID: "a", State: StateRunning, Limits: Limits{MemoryMax: "1G"}}
	st.Users["silo-a-a"] = 61000
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(s.Dir)
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("state dir mode %v", fi.Mode())
	}
	got, err := s.Load()
	if err != nil || !reflect.DeepEqual(got, st) {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	os.WriteFile(s.path(), []byte(`{"version": 99}`), 0o600)
	if _, err := s.Load(); err == nil {
		t.Fatal("loaded a future version")
	}
}
