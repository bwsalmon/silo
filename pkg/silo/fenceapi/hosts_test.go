package fenceapi_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/agents"
	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/store"
)

func TestSessionHosts(t *testing.T) {
	e := newAgentEnv(t)
	old := e.create(e.root, "team", "old", nil)
	time.Sleep(2 * time.Millisecond)
	young := e.create(e.root, "team", "young", nil)
	for _, a := range []string{old, young} {
		if err := e.runner.Connect(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []store.Replica{
		{ID: "ctl-a-1", Host: "ctl-a", AgentSlots: 2, AgentSessions: 1, Started: time.Now()},
		{ID: "ctl-b-1", Host: "ctl-b", AgentSlots: 1, Started: time.Now()},
		{ID: "other", Started: time.Now()}, // no sessions: not a host
	} {
		if err := store.Heartbeat(ctx, e.db, r); err != nil {
			t.Fatal(err)
		}
	}
	loads, err := e.api.HostLoads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []fenceapi.HostLoad{{Host: "ctl-a", Slots: 2, Sessions: 1}, {Host: "ctl-b", Slots: 1}}; !slices.Equal(loads, want) {
		t.Errorf("loads %+v, want %+v", loads, want)
	}
	if h := fenceapi.PickHost(loads, "ctl-a"); h != "ctl-a" {
		t.Errorf("a tie goes to %s, not this VM", h)
	}
	// Both on a control VM that is gone: the leader moves them, oldest
	// first, to the most free slots; the one that fits nowhere waits.
	for _, a := range []string{old, young} {
		if err := e.api.PlaceAgent(ctx, a, "ctl-dead"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Heartbeat(ctx, e.db, store.Replica{ID: "ctl-a-1", Host: "ctl-a", AgentSlots: 2, AgentSessions: 2, Started: time.Now()}); err != nil {
		t.Fatal(err)
	}
	n, err := e.api.MoveSessions(ctx, nil)
	if err != nil || n != 1 {
		t.Fatalf("moved %d, %v", n, err)
	}
	hosted, err := e.api.HostedAgents(ctx, "ctl-b")
	if err != nil || len(hosted) != 1 || hosted[0].Agent != old || hosted[0].Adapter != "claude-code" || hosted[0].Prompt != "Fix the off-by-one." {
		t.Fatalf("ctl-b hosts %+v, %v", hosted, err)
	}
	got := e.ok(e.root, "GET", "/silo/v1/silos/team/agents/young", nil)
	if got["status_message"] != fenceapi.WaitingForSlot || got["state"] != agents.StateRunning {
		t.Errorf("young: %v", got)
	}
	if hosted, _ := e.api.HostedAgents(ctx, "ctl-dead"); len(hosted) != 1 || hosted[0].Agent != young {
		t.Errorf("ctl-dead still hosts %+v", hosted)
	}
	if err := e.api.SessionStarted(ctx, young); err != nil {
		t.Fatal(err)
	}
	if got := e.ok(e.root, "GET", "/silo/v1/silos/team/agents/young", nil); got["status_message"] != "" && got["status_message"] != nil {
		t.Errorf("young still waits once started: %v", got["status_message"])
	}

	// The harness's state, saved after a turn.
	st := fenceapi.SessionState{Adapter: "claude-code", SessionID: "sid", State: []byte{0x1f, 0x8b, 0, 1}}
	if err := e.api.SaveSessionState(ctx, old, st); err != nil {
		t.Fatal(err)
	}
	if back, err := e.api.LoadSessionState(ctx, old); err != nil || back.SessionID != "sid" || string(back.State) != string(st.State) {
		t.Errorf("loaded %+v, %v", back, err)
	}
	if none, err := e.api.LoadSessionState(ctx, young); err != nil || none.SessionID != "" {
		t.Errorf("young has state %+v, %v", none, err)
	}
}

func TestSessionRestarted(t *testing.T) {
	e := newAgentEnv(t)
	a := e.create(e.root, "team", "fixer", nil)
	if err := e.runner.Connect(ctx, a); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []struct {
		kind string
		data map[string]any
	}{
		{fenceapi.EventToolCall, map[string]any{"id": 1, "tool": "read_file"}},
		{fenceapi.EventToolResult, map[string]any{"id": 1, "tool": "read_file"}},
		{fenceapi.EventToolCall, map[string]any{"id": 2, "tool": "exec"}},
	} {
		if err := e.api.RecordEvent(ctx, a, ev.kind, ev.data); err != nil {
			t.Fatal(err)
		}
	}
	open, err := e.api.SessionRestarted(ctx, a, "ctl-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].ID != "2" || open[0].Tool != "exec" {
		t.Errorf("interrupted %+v", open)
	}
	got := e.ok(e.root, "GET", "/silo/v1/silos/team/agents/fixer", nil)
	if got["session"].(obj)["restarts"] != float64(1) {
		t.Errorf("session %v", got["session"])
	}
	events := e.ok(e.root, "GET", "/silo/v1/silos/team/agents/fixer/session", nil)["events"].([]any)
	last := events[len(events)-1].(obj)
	if last["kind"] != fenceapi.EventToolResult || !strings.Contains(last["data"].(obj)["content"].(string), "interrupted") {
		t.Errorf("last event %v", last)
	}
	recs := e.records(e.team, fenceapi.KindAgentSessionRestart)
	if len(recs) != 1 {
		t.Fatalf("%d %s records", len(recs), fenceapi.KindAgentSessionRestart)
	}
	if again, _ := e.api.SessionRestarted(ctx, a, "ctl-b"); len(again) != 0 {
		t.Errorf("reported interrupted twice: %+v", again)
	}
}

func TestThreeFailedStarts(t *testing.T) {
	e := newAgentEnv(t)
	a := e.create(e.root, "team", "fixer", nil)
	if err := e.runner.Connect(ctx, a); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= fenceapi.MaxStartFailures; i++ {
		failed, err := e.api.SessionStartFailed(ctx, a, "exit 1")
		if err != nil || failed != (i == fenceapi.MaxStartFailures) {
			t.Fatalf("failure %d: failed %v, %v", i, failed, err)
		}
		if i == 1 {
			// A start in between starts the count again.
			e.api.SessionStarted(ctx, a)
			if failed, _ := e.api.SessionStartFailed(ctx, a, "exit 1"); failed {
				t.Fatal("failed after a start in between")
			}
		}
	}
	got := e.ok(e.root, "GET", "/silo/v1/silos/team/agents/fixer", nil)
	if got["state"] != agents.StateFailed || !strings.Contains(got["status_message"].(string), "3 times in a row") {
		t.Errorf("agent %v", got)
	}
	if got := e.used(e.team, fenceapi.AgentSessions); got != "0" {
		t.Errorf("a FAILED agent still holds %s sessions", got)
	}
}

// TestSessionGateway is where a tool channel attach that reached
// ctl-a's silod goes: to the gateway of a live host, nowhere for a
// session ctl-a hosts or that isn't placed, and nowhere for one whose
// host is gone, which the leader's MoveSessions re-places.
func TestSessionGateway(t *testing.T) {
	e := newAgentEnv(t)
	a := e.create(e.root, "team", "fixer", nil)
	for _, r := range []store.Replica{
		{ID: "ctl-a-1", Host: "ctl-a", AgentSlots: 2, GatewayAddress: "10.0.0.2", Started: time.Now()},
		{ID: "ctl-b-1", Host: "ctl-b", AgentSlots: 2, GatewayAddress: "10.0.0.3", Started: time.Now()},
	} {
		if err := store.Heartbeat(ctx, e.db, r); err != nil {
			t.Fatal(err)
		}
	}
	gw := func(agent string) string {
		t.Helper()
		addr, err := e.api.SessionGateway(ctx, agent, "ctl-a")
		if err != nil {
			t.Fatal(err)
		}
		return addr
	}
	if got := gw(a); got != "" {
		t.Errorf("an unplaced session redirects to %q", got)
	}
	if got := gw("agent-gone"); got != "" {
		t.Errorf("an unknown agent redirects to %q", got)
	}
	for host, want := range map[string]string{"ctl-a": "", "ctl-b": "10.0.0.3", "ctl-dead": ""} {
		if err := e.api.PlaceAgent(ctx, a, host); err != nil {
			t.Fatal(err)
		}
		if got := gw(a); got != want {
			t.Errorf("hosted on %s: redirected to %q, want %q", host, got, want)
		}
	}
	// ctl-b's heartbeat goes stale: it is gone, and its session is the
	// leader's to move, not ctl-b's to take.
	if err := e.api.PlaceAgent(ctx, a, "ctl-b"); err != nil {
		t.Fatal(err)
	}
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE replicas SET heartbeat_at = ? WHERE id = ?`,
			store.Millis(time.Now().Add(-2*store.ReplicaTimeout)), "ctl-b-1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := gw(a); got != "" {
		t.Errorf("hosted on a gone control VM: redirected to %q", got)
	}
	if n, err := e.api.MoveSessions(ctx, nil); err != nil || n != 1 {
		t.Fatalf("moved %d, %v", n, err)
	}
	if got := gw(a); got != "" {
		t.Errorf("moved to this VM: redirected to %q", got)
	}
}
