package registry

import (
	"errors"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newRegistry() (*Registry, *clock) {
	c := &clock{t: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)}
	r := New()
	r.Now = c.now
	r.StaleAfter = 30 * time.Second
	return r, c
}

func TestValidate(t *testing.T) {
	n, err := Validate(Node{ID: "ws-1", Repos: []string{"Bwsalmon/Silo", "bwsalmon/grain", "bwsalmon/silo"}, Slots: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := n.Repos; len(got) != 2 || got[0] != "bwsalmon/grain" || got[1] != "bwsalmon/silo" {
		t.Errorf("repos %v, want lower-cased, sorted and deduplicated", got)
	}
	n, err = Validate(Node{ID: "ws-1", Features: []string{"interactive", "attest", "interactive"}})
	if err != nil || len(n.Features) != 2 || n.Features[0] != "attest" || !n.Supports(FeatureInteractive) {
		t.Errorf("features %v %v, want sorted and deduplicated", n.Features, err)
	}
	for _, bad := range []Node{
		{ID: "ok", Features: []string{"Not A Feature"}},
		{ID: ""},
		{ID: "has space"},
		{ID: "ok", Slots: -1},
		{ID: "ok", Repos: []string{"noslash"}},
		{ID: "ok", Repos: []string{"a/b/c"}},
	} {
		if _, err := Validate(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("Validate(%+v) = %v, want ErrInvalid", bad, err)
		}
	}
}

func TestLifecycle(t *testing.T) {
	r, c := newRegistry()
	l, err := r.Register(Node{ID: "ws-1", Repos: []string{"bwsalmon/silo"}, Slots: 3, Version: "v1"}, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	e, ok := r.Get("ws-1")
	if !ok || e.Status != Online || e.Free != 3 || e.Owner != "alice@example.com" {
		t.Fatalf("after Register: %+v", e)
	}

	c.advance(31 * time.Second)
	if e, _ := r.Get("ws-1"); e.Status != Stale || e.Available() != 0 {
		t.Errorf("after silence: %s with %d available, want stale with none", e.Status, e.Available())
	}
	if !l.Heartbeat(1) {
		t.Fatal("Heartbeat on a live lease reported false")
	}
	if e, _ := r.Get("ws-1"); e.Status != Online || e.Free != 1 {
		t.Errorf("after heartbeat: %+v", e)
	}
	l.Heartbeat(99)
	if e, _ := r.Get("ws-1"); e.Free != 3 {
		t.Errorf("free %d, want it capped at the node's 3 slots", e.Free)
	}

	l.Close()
	select {
	case <-l.Done():
	default:
		t.Error("Done not closed by Close")
	}
	if e, _ := r.Get("ws-1"); e.Status != Offline || e.Free != 0 {
		t.Errorf("after Close: %+v", e)
	}
	if l.Heartbeat(1) {
		t.Error("Heartbeat on an ended lease reported true")
	}
	if !r.Forget("ws-1") {
		t.Error("Forget of an offline node reported false")
	}
	if len(r.List()) != 0 {
		t.Errorf("directory %v, want empty", r.List())
	}
}

func TestOwner(t *testing.T) {
	r, _ := newRegistry()
	l, err := r.Register(Node{ID: "ws-1", Slots: 1}, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Register(Node{ID: "ws-1", Slots: 1}, "mallory@example.com"); !errors.Is(err, ErrOwner) {
		t.Errorf("another owner's Register = %v, want ErrOwner", err)
	}
	l.Close()
	if _, err := r.Register(Node{ID: "ws-1", Slots: 1}, "mallory@example.com"); !errors.Is(err, ErrOwner) {
		t.Errorf("another owner's Register of an offline node = %v, want ErrOwner", err)
	}
	if _, err := r.Register(Node{ID: "ws-2"}, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("Register with no owner = %v, want ErrInvalid", err)
	}
}

func TestSupersede(t *testing.T) {
	r, _ := newRegistry()
	old, _ := r.Register(Node{ID: "ws-1", Slots: 1}, "alice@example.com")
	cur, err := r.Register(Node{ID: "ws-1", Slots: 4}, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-old.Done():
	default:
		t.Fatal("the superseded lease's Done is open")
	}
	// The old connection noticing late must not take the new one offline.
	old.Close()
	if old.Heartbeat(0) {
		t.Error("the superseded lease still heartbeats")
	}
	if e, _ := r.Get("ws-1"); e.Status != Online || e.Slots != 4 {
		t.Errorf("after the old lease closed: %+v", e)
	}
	if r.Forget("ws-1") {
		t.Error("Forget removed an online node")
	}
	cur.Close()
}

func TestClaim(t *testing.T) {
	r, c := newRegistry()
	a, _ := r.Register(Node{ID: "a", Repos: []string{"o/r"}, Slots: 1}, "alice@example.com")
	b, _ := r.Register(Node{ID: "b", Repos: []string{"o/r", "o/other"}, Slots: 2}, "bob@example.com")
	defer a.Close()

	if got := r.ForRepo("O/R"); len(got) != 2 || got[0].ID != "b" {
		t.Fatalf("ForRepo = %v, want b (2 free) then a", got)
	}
	var claims []*Claim
	for range 3 {
		cl, err := r.Claim("o/r")
		if err != nil {
			t.Fatal(err)
		}
		claims = append(claims, cl)
	}
	if claims[0].NodeID != "b" {
		t.Errorf("first claim on %s, want b", claims[0].NodeID)
	}
	if _, err := r.Claim("o/r"); !errors.Is(err, ErrNoNode) {
		t.Errorf("a fourth claim = %v, want ErrNoNode", err)
	}
	if _, err := r.Claim("o/none"); !errors.Is(err, ErrNoNode) {
		t.Errorf("a claim for an unserved repo = %v, want ErrNoNode", err)
	}

	// A released claim gives its slot back.
	var onB *Claim
	for _, cl := range claims {
		if cl.NodeID == "b" {
			onB = cl
		}
	}
	onB.Release()
	onB.Release()
	if e, _ := r.Get("b"); e.Claimed != 1 {
		t.Errorf("b claimed %d after one release (twice), want 1", e.Claimed)
	}

	// A heartbeat settles the claims; a claim released after it is a
	// no-op, and does not give back a later claim's slot.
	b.Heartbeat(1)
	late, err := r.Claim("o/r")
	if err != nil || late.NodeID != "b" {
		t.Fatalf("claim after heartbeat = %v, %v", late, err)
	}
	for _, cl := range claims {
		cl.Release()
	}
	if e, _ := r.Get("b"); e.Claimed != 1 {
		t.Errorf("b claimed %d, want the late claim's 1", e.Claimed)
	}

	// A stale node is passed over.
	c.advance(31 * time.Second)
	a.Heartbeat(1)
	cl, err := r.Claim("o/r")
	if err != nil || cl.NodeID != "a" {
		t.Errorf("claim with b stale = %v, %v; want a", cl, err)
	}

	// An offline node is passed over, and a claim on it is a no-op.
	b.Close()
	late.Release()
	if e, _ := r.Get("b"); e.Claimed != 0 || e.Status != Offline {
		t.Errorf("b after Close: %+v", e)
	}
}

func TestChanges(t *testing.T) {
	r, _ := newRegistry()
	ch, stop := r.Changes()
	defer stop()
	l, _ := r.Register(Node{ID: "a", Slots: 1}, "alice@example.com")
	l.Heartbeat(1)
	l.Close()
	select {
	case <-ch:
	default:
		t.Fatal("no change notified")
	}
	select {
	case <-ch:
		t.Fatal("changes not coalesced")
	default:
	}
}

func TestClaimFor(t *testing.T) {
	r, _ := newRegistry()
	for _, n := range []Node{
		{ID: "a", Repos: []string{"o/one"}, Slots: 1},
		{ID: "b", Repos: []string{"o/two"}, Slots: 3, Labels: map[string]string{"zone": "eu"}},
		{ID: "c", Repos: []string{"o/three"}, Slots: 5},
	} {
		if _, err := r.Register(n, "alice@example.com"); err != nil {
			t.Fatal(err)
		}
	}
	// Any node serving one of the repositories, the freest first.
	c, err := r.ClaimFor([]string{"o/one", "O/Two"}, nil)
	if err != nil || c.NodeID != "b" {
		t.Fatalf("ClaimFor(one, two) = %v, %v; want b, the freest", c, err)
	}
	// ok passes over a node, and the next freest is claimed.
	c, err = r.ClaimFor([]string{"o/one", "o/two"}, func(e Entry) bool { return e.ID != "b" })
	if err != nil || c.NodeID != "a" {
		t.Fatalf("ClaimFor without b = %v, %v; want a", c, err)
	}
	if _, err := r.ClaimFor([]string{"o/one"}, nil); !errors.Is(err, ErrNoNode) {
		t.Errorf("a's only slot claimed twice: %v", err)
	}
	if _, err := r.ClaimFor([]string{"o/two"}, func(e Entry) bool { return e.Labels["zone"] == "us" }); !errors.Is(err, ErrNoNode) {
		t.Errorf("no node matches: %v, want ErrNoNode", err)
	}
	if _, err := r.ClaimFor(nil, nil); !errors.Is(err, ErrNoNode) {
		t.Errorf("no repositories: %v, want ErrNoNode", err)
	}
}
