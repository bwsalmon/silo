package gateway

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStartSeedsRootAndUserAgent(t *testing.T) {
	e := newEnv(t)
	root, main := e.g.st.Agents[RootAgentID], e.g.st.Agents[UserAgentID]
	if root == nil || !root.Root || !root.Standing || root.User != RootUser || root.Parent != "" ||
		root.State != StateRegistered || root.Permissions != PermAsk {
		t.Fatalf("root %+v", root)
	}
	if main == nil || main.Root || !main.Standing || main.User != UserAgentUser || main.Parent != RootAgentID ||
		!main.MayRegister || main.State != StateRegistered || main.Limits.MemoryMax != "4G" || main.Permissions != PermAsk {
		t.Fatalf("main %+v", main)
	}
	for _, id := range []string{RootAgentID, UserAgentID} {
		fi, err := os.Stat(filepath.Join(e.cfg.TokenDir(), id))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("token file %s: %v %v", id, fi, err)
		}
	}
	if ev := e.lastEvent(EvRegister); ev["agent"] != UserAgentID || ev["_actor"] != "gateway" || ev["parent"] != RootAgentID || ev["permissions"] != PermAsk {
		t.Fatalf("register audit %v", ev)
	}

	// The token file connects the agent once; the file goes away.
	tok := e.tokenFile(RootAgentID)
	var cr ConnectResult
	e.mustCall(strangerCred, "agent.connect", map[string]any{"token": tok}, &cr)
	if cr.Agent != RootAgentID || !cr.Root || cr.SessionToken == "" || cr.HeartbeatSec != 30 || cr.Permissions != PermAsk {
		t.Fatalf("connect %+v", cr)
	}
	if _, err := os.Stat(filepath.Join(e.cfg.TokenDir(), RootAgentID)); !os.IsNotExist(err) {
		t.Fatalf("token file left behind: %v", err)
	}
	if err := e.call(strangerCred, "agent.connect", map[string]any{"token": tok}, nil); codeOf(err) != CodeForbidden {
		t.Fatalf("token reused: %v", err)
	}

	// A restart neither duplicates nor disturbs them; the unconnected one
	// gets a fresh token.
	before := e.tokenFile(UserAgentID)
	e.restart()
	if len(e.g.st.Agents) != 2 || e.g.st.Agents[RootAgentID].State != StateConnected {
		t.Fatalf("after restart %+v", e.g.st.Agents)
	}
	if after := e.tokenFile(UserAgentID); after == before {
		t.Fatal("user agent token not re-armed")
	}
	if err := e.call(strangerCred, "agent.connect", map[string]any{"token": before}, nil); codeOf(err) != CodeForbidden {
		t.Fatalf("stale token accepted: %v", err)
	}
}

func TestStandingAgentsDisabled(t *testing.T) {
	off := false
	e := newEnv(t, func(c *Config) { c.RootAgent.Enabled = &off })
	if e.g.st.Agents[RootAgentID] != nil || e.g.st.Agents[UserAgentID].Parent != "" {
		t.Fatalf("agents %+v", e.g.st.Agents)
	}
	e = newEnv(t, func(c *Config) { c.RootAgent.Enabled = &off; c.UserAgent.Enabled = &off })
	if len(e.g.st.Agents) != 0 {
		t.Fatalf("agents %+v", e.g.st.Agents)
	}
}

func TestRegisterAndConnect(t *testing.T) {
	e := newEnv(t)
	var r RegisterResult
	e.mustCall(adminCred, "agent.register", map[string]any{
		"name": "web", "location": map[string]any{"kind": "container", "detail": "podman:web"},
		"runtime": "gemini", "brief": "serve it", "limits": map[string]any{"memory_max": "1G"},
		"budget_usd": 5, "stop_cmd": []string{"podman", "rm", "-f", "web"},
	}, &r)
	if r.Agent != "web" || !strings.HasPrefix(r.Token, regTokenPrefix) || r.HubURL != "unix://"+e.cfg.SocketPath || r.State != StateRegistered {
		t.Fatalf("register %+v", r)
	}
	a := e.g.st.Agents["web"]
	if a.Parent != "" || a.Location.Kind != "container" || a.Limits.MemoryMax != "1G" || a.Limits.CPUQuota != "100%" ||
		a.RegisteredBy != "admin:uid=0" || a.State != StateRegistered || a.Permissions != PermAsk {
		t.Fatalf("agent %+v", a)
	}
	// The token itself is in neither the state file nor the audit log.
	state, _ := os.ReadFile(filepath.Join(e.cfg.StateDir, "state.json"))
	log, _ := os.ReadFile(e.cfg.AuditLog)
	if strings.Contains(string(state), r.Token) || strings.Contains(string(log), r.Token) {
		t.Fatal("registration token persisted")
	}

	// A stranger may only connect.
	if err := e.call(strangerCred, "agent.list", nil, nil); codeOf(err) != CodeForbidden {
		t.Fatalf("stranger list: %v", err)
	}
	sess := e.connect(r.Token)
	if a.State != StateConnected || a.Peer.PID != 42 || a.Peer.UID != strangerCred.UID {
		t.Fatalf("connected %+v", a)
	}
	if ev := e.lastEvent(EvConnect); ev["agent"] != "web" || ev["_actor"] != "agent:web" {
		t.Fatalf("connect audit %v", ev)
	}
	// The session token identifies the agent, whoever dials.
	var list []AgentInfo
	e.mustCall(strangerCred, "agent.list", with(sess, nil), &list)
	if len(list) != 1 || list[0].ID != "web" {
		t.Fatalf("agent sees %+v", list)
	}
	if err := e.call(strangerCred, "agent.list", with("silo_ses_bogus", nil), nil); codeOf(err) != CodeForbidden {
		t.Fatalf("bogus token: %v", err)
	}
	// Re-registering an active name conflicts; after a stop it is reused,
	// and the old incarnation goes to history.
	if err := e.call(adminCred, "agent.register", map[string]any{"name": "web"}, nil); codeOf(err) != CodeConflict {
		t.Fatalf("duplicate: %v", err)
	}
	var sr StopResult
	e.mustCall(adminCred, "agent.stop", map[string]any{"agent": "web"}, &sr)
	if !reflect.DeepEqual(sr.StopCmds["web"], []string{"podman", "rm", "-f", "web"}) {
		t.Fatalf("stop cmds %+v", sr)
	}
	e.register(adminCred, "", map[string]any{"name": "web"})
	e.mustCall(adminCred, "agent.list", map[string]any{"history": true}, &list)
	if len(list) != 4 {
		t.Fatalf("history %+v", list)
	}
}

func TestPermissionsDefaultAskAndApproval(t *testing.T) {
	e := newEnv(t)
	mainTok := e.standing(UserAgentID)

	// By default main has permissions="ask", so registering a child puts it
	// in state "pending" and its token cannot connect until human approval.
	var reg RegisterResult
	e.mustCall(strangerCred, "agent.register", with(mainTok, map[string]any{"name": "kid"}), &reg)
	if reg.State != StatePending || e.g.st.Agents["kid"].Permissions != PermAsk {
		t.Fatalf("expected pending registration, got %+v", reg)
	}
	if err := e.call(strangerCred, "agent.connect", map[string]any{"token": reg.Token}, nil); codeOf(err) != CodeForbidden {
		t.Fatalf("unapproved token connected: %v", err)
	}
	// An agent cannot approve its own child or change permissions.
	if err := e.call(strangerCred, "agent.approve", with(mainTok, map[string]any{"agent": "kid"}), nil); codeOf(err) != CodeForbidden {
		t.Fatalf("agent approved child: %v", err)
	}
	if err := e.call(strangerCred, "agent.permissions", with(mainTok, map[string]any{"agent": UserAgentID, "permissions": PermAuto}), nil); codeOf(err) != CodeForbidden {
		t.Fatalf("agent changed permissions: %v", err)
	}
	if err := e.call(strangerCred, "agent.register", with(mainTok, map[string]any{"name": "kid2", "permissions": PermAuto}), nil); codeOf(err) != CodeForbidden {
		t.Fatalf("agent registered child with auto permissions: %v", err)
	}

	// Human approves kid; now its token connects.
	var info AgentInfo
	e.mustCall(bridgeCred, "agent.approve", map[string]any{"agent": "kid"}, &info)
	if info.State != StateRegistered {
		t.Fatalf("after approve: %+v", info)
	}
	e.connect(reg.Token)

	// Human rejects a pending child registration.
	e.mustCall(strangerCred, "agent.register", with(mainTok, map[string]any{"name": "rejected-kid"}), &reg)
	no := false
	e.mustCall(bridgeCred, "agent.approve", map[string]any{"agent": "rejected-kid", "approve": no, "comment": "not needed"}, &info)
	if info.State != StateStopped || !strings.Contains(info.Tombstone.Cause, "not needed") {
		t.Fatalf("after reject: %+v", info)
	}
	if err := e.call(strangerCred, "agent.connect", map[string]any{"token": reg.Token}, nil); codeOf(err) != CodeForbidden {
		t.Fatalf("rejected token connected: %v", err)
	}

	// When the human explicitly switches main to permissions="auto",
	// main's new child registrations are immediately active ( still defaulting
	// the child itself to permissions="ask").
	e.mustCall(bridgeCred, "agent.permissions", map[string]any{"agent": UserAgentID, "permissions": PermAuto}, &info)
	if info.Permissions != PermAuto {
		t.Fatalf("permissions %+v", info)
	}
	e.mustCall(strangerCred, "agent.register", with(mainTok, map[string]any{"name": "auto-kid"}), &reg)
	if reg.State != StateRegistered || e.g.st.Agents["auto-kid"].Permissions != PermAsk {
		t.Fatalf("auto-kid %+v %+v", reg, e.g.st.Agents["auto-kid"])
	}
	e.connect(reg.Token)
}

func TestRegisterAuthorization(t *testing.T) {
	e := newEnv(t)
	mainTok := e.standing(UserAgentID)
	kid := e.spawn(strangerCred, mainTok, map[string]any{"name": "kid"})
	if a := e.g.st.Agents["kid"]; a.Parent != UserAgentID || a.MayRegister || a.RegisteredBy != "agent:main" {
		t.Fatalf("kid %+v", a)
	}
	for name, params := range map[string]map[string]any{
		"kid without may_register": with(kid, map[string]any{"name": "gk"}),
		"someone else's child":     with(mainTok, map[string]any{"name": "x", "parent": RootAgentID}),
	} {
		if err := e.call(strangerCred, "agent.register", params, nil); codeOf(err) != CodeForbidden {
			t.Errorf("%s: %v", name, err)
		}
	}
	if ev := e.lastEvent(EvAuthDeny); ev["method"] != "agent.register" {
		t.Fatalf("deny audit %v", ev)
	}
	// Depth: root > main > a > b, and no deeper.
	a := e.spawn(strangerCred, mainTok, map[string]any{"name": "a", "may_register": true})
	b := e.spawn(strangerCred, a, map[string]any{"name": "b", "may_register": true})
	if err := e.call(strangerCred, "agent.register", with(b, map[string]any{"name": "c"}), nil); codeOf(err) != CodeInvalidParams {
		t.Fatalf("too deep: %v", err)
	}
	for _, bad := range []map[string]any{
		{"name": "Bad"},
		{"name": "root"},
		{"name": "ok", "user": "root"},
		{"name": "ok", "user": RootUser},
		{"name": "ok", "location": map[string]any{"kind": "mainframe"}},
		{"name": "ok", "runtime": "Has Spaces"},
		{"name": "ok", "permissions": " reckless"},
		{"name": "ok", "limits": map[string]any{"memory_max": "lots"}},
		{"name": "ok", "budget_usd": -1},
		{"name": "ok", "parent": "nobody"},
		{"name": "ok", "stop_cmd": []string{""}},
		{"name": "ok", "value": "x"},
	} {
		if err := e.call(adminCred, "agent.register", bad, nil); codeOf(err) != CodeInvalidParams {
			t.Errorf("%v: %v", bad, err)
		}
	}
	if err := e.call(adminCred, "agent.register", map[string]any{"name": "ok", "user": UserAgentUser}, nil); codeOf(err) != CodeConflict {
		t.Errorf("standing user reused: %v", err)
	}
	// Special-case inter-agent messaging is removed.
	if err := e.call(adminCred, "agent.message", map[string]any{"agent": "kid", "text": "hi"}, nil); codeOf(err) != CodeMethodNotFound {
		t.Errorf("agent.message still exists: %v", err)
	}
	if err := e.call(strangerCred, "agent.inbox", with(kid, nil), nil); codeOf(err) != CodeMethodNotFound {
		t.Errorf("agent.inbox still exists: %v", err)
	}
}

func TestReportAndRequests(t *testing.T) {
	e := newEnv(t)
	main := e.standing(UserAgentID)
	kid := e.spawn(strangerCred, main, map[string]any{"name": "kid"})

	var info AgentInfo
	e.mustCall(strangerCred, "agent.report", with(kid, map[string]any{"status": "ok", "summary": "all tests green"}), &info)
	if info.Report == nil || info.Report.Status != "ok" || info.Report.Summary != "all tests green" {
		t.Fatalf("report %+v", info.Report)
	}
	if err := e.call(strangerCred, "agent.report", with(kid, map[string]any{"status": "bogus", "summary": "x"}), nil); codeOf(err) != CodeInvalidParams {
		t.Errorf("bad report status: %v", err)
	}

	var req AgentRequest
	e.mustCall(strangerCred, "request.file", with(kid, map[string]any{"kind": "privileged", "summary": "install postgresql", "detail": "need psql for tests"}), &req)
	if req.ID != "req-1" || req.Agent != "kid" || req.State != RequestPending {
		t.Fatalf("request %+v", req)
	}
	var reqs []AgentRequest
	e.mustCall(bridgeCred, "request.list", map[string]any{"pending": true}, &reqs)
	if len(reqs) != 1 || reqs[0].ID != "req-1" {
		t.Fatalf("request list %+v", reqs)
	}
	e.mustCall(bridgeCred, "request.resolve", map[string]any{"id": "req-1", "resolution": "resolved", "note": "installed"}, &req)
	if req.State != RequestResolved || req.Note != "installed" {
		t.Fatalf("resolved %+v", req)
	}
}

func TestStopCascades(t *testing.T) {
	e := newEnv(t)
	main := e.standing(UserAgentID)
	kid := e.spawn(strangerCred, main, map[string]any{"name": "kid", "may_register": true, "stop_cmd": []string{"/bin/true"}})
	gk := e.spawn(strangerCred, kid, map[string]any{"name": "gk"})
	e.mustCall(strangerCred, "page.register", with(gk, map[string]any{"slug": "gk", "title": "GK", "port": 8080}), nil)
	var sec SecretRequest
	e.mustCall(strangerCred, "secret.request", with(gk, map[string]any{"name": "api-key", "reason": "r"}), &sec)

	// A child cannot stop its parent, nor a sibling's subtree.
	if err := e.call(strangerCred, "agent.stop", with(gk, map[string]any{"agent": "kid"}), nil); codeOf(err) != CodeForbidden {
		t.Fatalf("child stopped parent: %v", err)
	}
	var sr StopResult
	e.mustCall(strangerCred, "agent.stop", with(main, map[string]any{"agent": "kid", "mode": "kill"}), &sr)
	if !reflect.DeepEqual(sr.Stopped, []string{"kid", "gk"}) || sr.Tombstone.Cause != "stopped by agent:main" ||
		!reflect.DeepEqual(sr.Tombstone.Children, []string{"gk"}) || len(sr.StopCmds) != 1 {
		t.Fatalf("stop %+v", sr)
	}
	if g := e.g.st.Agents["gk"]; g.State != StateStopped || g.Tombstone.Cause != "parent kid stopped" {
		t.Fatalf("gk %+v", g)
	}
	if e.g.st.Pages["gk"].Active || e.g.st.Secrets[0].State != SecretCancelled {
		t.Fatal("page or secret request outlived its agent")
	}
	// The wrapper learns to stop from its heartbeat; nothing else works.
	var hb HeartbeatResult
	e.mustCall(strangerCred, "agent.heartbeat", with(gk, nil), &hb)
	if hb.State != StateStopped || hb.Stop != "kill" {
		t.Fatalf("heartbeat %+v", hb)
	}
	if err := e.call(strangerCred, "agent.list", with(gk, nil), nil); codeOf(err) != CodeForbidden {
		t.Fatalf("stopped agent listed: %v", err)
	}
	e.mustCall(strangerCred, "agent.disconnect", with(gk, nil), nil)
	if err := e.call(strangerCred, "agent.heartbeat", with(gk, nil), nil); codeOf(err) != CodeForbidden {
		t.Fatalf("token survived disconnect: %v", err)
	}
	if err := e.call(adminCred, "agent.stop", map[string]any{"agent": "kid"}, nil); codeOf(err) != CodeConflict {
		t.Fatalf("double stop: %v", err)
	}
	if err := e.call(adminCred, "agent.stop", map[string]any{"agent": "kid", "mode": "gently"}, nil); codeOf(err) != CodeInvalidParams {
		t.Fatalf("bad mode: %v", err)
	}

	// Stopping root stops the user agent too; both come back, re-armed,
	// on the next gateway start.
	e.mustCall(adminCred, "agent.stop", map[string]any{"agent": RootAgentID}, &sr)
	if !reflect.DeepEqual(sr.Stopped, []string{RootAgentID, UserAgentID}) {
		t.Fatalf("stop root %+v", sr)
	}
	e.restart()
	for _, id := range []string{RootAgentID, UserAgentID} {
		if a := e.g.st.Agents[id]; a.State != StateRegistered || a.Tombstone != nil {
			t.Fatalf("%s after restart %+v", id, a)
		}
		e.standing(id)
	}
}

func TestHeartbeatTimeoutAndDisconnect(t *testing.T) {
	e := newEnv(t)
	main := e.standing(UserAgentID)
	kid := e.spawn(strangerCred, main, map[string]any{"name": "kid"})
	e.clock.Advance(60 * time.Second)
	e.mustCall(strangerCred, "agent.heartbeat", with(kid, nil), nil)
	e.clock.Advance(60 * time.Second)
	if err := e.g.Reconcile(); err != nil {
		t.Fatal(err)
	}
	// main went 120s without a heartbeat; kid only 60s.
	if a := e.g.st.Agents[UserAgentID]; a.State != StateRegistered {
		t.Fatalf("main %+v", a)
	}
	if e.g.st.Agents["kid"].State != StateConnected {
		t.Fatal("kid timed out early")
	}
	if ev := e.lastEvent(EvDisconnect); ev["agent"] != UserAgentID || !strings.HasPrefix(ev["reason"].(string), "no heartbeat") {
		t.Fatalf("disconnect audit %v", ev)
	}
	if err := e.call(strangerCred, "agent.list", with(main, nil), nil); codeOf(err) != CodeForbidden {
		t.Fatalf("timed-out session still valid: %v", err)
	}
	// The standing agent reconnects with its re-armed token file.
	e.standing(UserAgentID)

	// A non-standing agent that disconnects stays disconnected, and its
	// registration token is long gone.
	var info AgentInfo
	e.mustCall(strangerCred, "agent.disconnect", with(kid, map[string]any{"reason": "cli exited 0", "exit_code": 0}), &info)
	if info.State != StateDisconnected || e.g.st.Agents["kid"].TokenHash != "" || info.ExitCode == nil || *info.ExitCode != 0 {
		t.Fatalf("kid %+v", info)
	}
	// Methods that act on the caller's own session need an agent.
	if err := e.call(adminCred, "agent.heartbeat", nil, nil); codeOf(err) != CodeInvalidParams {
		t.Fatalf("admin heartbeat: %v", err)
	}
}

func TestBridgeRelay(t *testing.T) {
	e := newEnv(t)
	e.standing(UserAgentID)
	var list []AgentInfo
	relay := map[string]any{"on_behalf_of": map[string]any{"agent": UserAgentID}}
	e.mustCall(bridgeCred, "agent.list", relay, &list)
	if len(list) != 1 || list[0].ID != UserAgentID {
		t.Fatalf("relayed list %+v", list)
	}
	if err := e.call(bridgeCred, "audit.read", relay, nil); codeOf(err) != CodeForbidden {
		t.Fatalf("relayed audit.read: %v", err)
	}
	if err := e.call(adminCred, "agent.list", relay, nil); codeOf(err) != CodeForbidden {
		t.Fatalf("non-bridge relay: %v", err)
	}
	if err := e.call(bridgeCred, "agent.list", map[string]any{"on_behalf_of": map[string]any{"agent": RootAgentID}}, nil); codeOf(err) != CodeForbidden {
		t.Fatalf("relay for unconnected agent: %v", err)
	}
	// Unrelayed, the bridge is the human.
	e.mustCall(bridgeCred, "agent.list", nil, &list)
	if len(list) != 2 {
		t.Fatalf("bridge list %+v", list)
	}
}

func TestListAndStatus(t *testing.T) {
	e := newEnv(t)
	main := e.standing(UserAgentID)
	e.spawn(strangerCred, main, map[string]any{"name": "kid", "brief": "secret plan"})
	var info AgentInfo
	e.mustCall(adminCred, "agent.status", map[string]any{"agent": "kid"}, &info)
	if info.Brief != "secret plan" || info.TokenHash != "" || info.SessionHash != "" || info.Permissions != PermAsk {
		t.Fatalf("status %+v", info)
	}
	e.mustCall(adminCred, "agent.status", map[string]any{"agent": RootAgentID}, &info)
	if !reflect.DeepEqual(info.Children, []string{UserAgentID}) {
		t.Fatalf("root children %v", info.Children)
	}
	// main sees itself and kid, not root.
	if err := e.call(strangerCred, "agent.status", with(main, map[string]any{"agent": RootAgentID}), nil); codeOf(err) != CodeNotFound {
		t.Fatalf("main sees root: %v", err)
	}
	var list []AgentInfo
	e.mustCall(strangerCred, "agent.list", with(main, map[string]any{"state": []string{StateConnected}}), &list)
	if len(list) != 2 || list[0].Brief != "" {
		t.Fatalf("list %+v", list)
	}
	parent := UserAgentID
	e.mustCall(adminCred, "agent.list", map[string]any{"parent": parent}, &list)
	if len(list) != 1 || list[0].ID != "kid" {
		t.Fatalf("by parent %+v", list)
	}
}

func TestStatePersistence(t *testing.T) {
	e := newEnv(t)
	main := e.standing(UserAgentID)
	kid := e.spawn(strangerCred, main, map[string]any{"name": "kid"})
	e.mustCall(strangerCred, "agent.report", with(kid, map[string]any{"status": "ok", "summary": "done"}), nil)
	e.restart()
	// Sessions survive a gateway restart: the wrappers keep going.
	var list []AgentInfo
	e.mustCall(strangerCred, "agent.list", with(main, nil), &list)
	if len(list) != 2 || list[0].ID != "kid" || list[0].Report == nil || list[0].Report.Status != "ok" {
		t.Fatalf("after restart %+v", list)
	}
	os.WriteFile(filepath.Join(e.cfg.StateDir, "state.json"), []byte(`{"version":1}`), 0o600)
	if _, err := (Store{Dir: e.cfg.StateDir}).Load(); err == nil {
		t.Fatal("loaded version 1 state")
	}
}
