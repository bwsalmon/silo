package registry

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/bwsalmon/silo/internal/pgtest"
)

// pod is one pod's registry, shared through the store at url.
func pod(t *testing.T, url, name string) *Registry {
	t.Helper()
	s, err := OpenStore(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	r := New()
	r.Store, r.Pod, r.PodAddr = s, name, name+":9092"
	r.Logf = t.Logf
	return r
}

// renew renews r's pod lease and refills its mirror, as one turn of Run.
func renew(t *testing.T, rs ...*Registry) {
	t.Helper()
	for _, r := range rs {
		if err := r.renew(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := r.Refill(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func status(t *testing.T, r *Registry, id string) Status {
	t.Helper()
	e, ok := r.Get(id)
	if !ok {
		t.Fatalf("%s: %s is not in the directory", r.Pod, id)
	}
	return e.Status
}

func register(t *testing.T, r *Registry, id, owner string, slots int) *Lease {
	t.Helper()
	l, err := r.Register(Node{ID: id, Repos: []string{"bwsalmon/silo"}, Slots: slots, Labels: map[string]string{"zone": "a"}}, owner)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// TestSharedSeesEachOther: two pods on one database each see the
// other's nodes, as the node registered them.
func TestSharedSeesEachOther(t *testing.T) {
	pg := pgtest.Start(t)
	a, b := pod(t, pg.URL, "pod-a"), pod(t, pg.URL, "pod-b")
	register(t, a, "ws-1", "alice@example.com", 2)
	register(t, b, "ws-2", "bob@example.com", 3)
	renew(t, a, b, a)

	for _, r := range []*Registry{a, b} {
		got := r.List()
		if len(got) != 2 {
			t.Fatalf("%s lists %d nodes: %+v", r.Pod, len(got), got)
		}
		for i, want := range []struct {
			id, owner string
			slots     int
		}{{"ws-1", "alice@example.com", 2}, {"ws-2", "bob@example.com", 3}} {
			e := got[i]
			if e.ID != want.id || e.Owner != want.owner || e.Slots != want.slots || e.Free != want.slots ||
				e.Status != Online || e.Labels["zone"] != "a" || len(e.Repos) != 1 || e.Repos[0] != "bwsalmon/silo" {
				t.Errorf("%s: %+v", r.Pod, e)
			}
		}
	}
	if e, _ := b.Get("ws-1"); e.Available() != 2 {
		t.Errorf("pod-b: ws-1 has %d available, want 2", e.Available())
	}
}

// TestSharedPodLeaseLapses: a pod whose lease lapses takes its nodes
// offline on every other pod, with no pod having to notice, and a pod
// that shuts down takes them offline at once.
func TestSharedPodLeaseLapses(t *testing.T) {
	pg := pgtest.Start(t)
	c := &clock{t: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)}
	a, b := pod(t, pg.URL, "pod-a"), pod(t, pg.URL, "pod-b")
	a.Now, b.Now = c.now, c.now
	a.PodLease = 300 * time.Millisecond
	l := register(t, a, "ws-1", "alice@example.com", 1)
	renew(t, a, b)
	if st := status(t, b, "ws-1"); st != Online {
		t.Fatalf("pod-b: ws-1 is %s, want online", st)
	}

	// pod-a stops renewing, but its node is still heartbeating.
	c.advance(400 * time.Millisecond)
	if st := status(t, b, "ws-1"); st != Offline {
		t.Errorf("pod-b: ws-1 is %s after pod-a's lease lapsed, want offline", st)
	}
	if !l.Heartbeat(1) {
		t.Fatal("the heartbeat ended the lease")
	}
	renew(t, b)
	if st := status(t, b, "ws-1"); st != Offline {
		t.Errorf("pod-b: ws-1 is %s after a refill, want offline", st)
	}
	if st := status(t, a, "ws-1"); st != Online {
		t.Errorf("pod-a: its own ws-1 is %s, want online", st)
	}
	if _, err := b.Claim("bwsalmon/silo"); !errors.Is(err, ErrNoNode) {
		t.Errorf("pod-b claimed on a lapsed pod's node: %v", err)
	}

	// pod-a renews again, then shuts down: Run deletes its lease.
	a.PodLease = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- a.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for status(t, b, "ws-1") != Online {
		if time.Now().After(deadline) {
			t.Fatal("pod-b never saw ws-1 online again")
		}
		time.Sleep(20 * time.Millisecond)
		renew(t, b)
	}
	cancel()
	<-done
	renew(t, b)
	if st := status(t, b, "ws-1"); st != Offline {
		t.Errorf("pod-b: ws-1 is %s after pod-a shut down, want offline", st)
	}
}

// TestSharedSupersession: a node that reconnects to another pod
// supersedes its old connection there: the old pod's next heartbeat
// finds it superseded and ends the lease, and its late close does not
// take the new connection offline.
func TestSharedSupersession(t *testing.T) {
	pg := pgtest.Start(t)
	a, b := pod(t, pg.URL, "pod-a"), pod(t, pg.URL, "pod-b")
	old := register(t, a, "ws-1", "alice@example.com", 1)
	renew(t, a, b)
	cur := register(t, b, "ws-1", "alice@example.com", 1)
	renew(t, b)

	if old.Heartbeat(1) {
		t.Fatal("the superseded connection's heartbeat succeeded")
	}
	select {
	case <-old.Done():
	default:
		t.Fatal("the superseded lease did not end")
	}
	old.Close()
	if !cur.Heartbeat(1) {
		t.Fatal("the new connection's heartbeat failed")
	}
	renew(t, a, b)
	for _, r := range []*Registry{a, b} {
		if st := status(t, r, "ws-1"); st != Online {
			t.Errorf("%s: ws-1 is %s after the old connection's close, want online", r.Pod, st)
		}
	}
	// pod-a holds no connection for ws-1 any more, so its sweep leaves
	// pod-b's row alone.
	renew(t, a, b)
	if st := status(t, b, "ws-1"); st != Online {
		t.Errorf("pod-b: ws-1 is %s after pod-a's sweep, want online", st)
	}
	cur.Close()
	renew(t, a)
	if st := status(t, a, "ws-1"); st != Offline {
		t.Errorf("pod-a: ws-1 is %s after closing, want offline", st)
	}
}

// TestSharedOwner: a node ID belongs to its first owner on every pod,
// online or not, until it is forgotten.
func TestSharedOwner(t *testing.T) {
	pg := pgtest.Start(t)
	a, b := pod(t, pg.URL, "pod-a"), pod(t, pg.URL, "pod-b")
	l := register(t, a, "ws-1", "alice@example.com", 1)
	renew(t, a, b)
	if _, err := b.Register(Node{ID: "ws-1", Slots: 1}, "mallory@example.com"); !errors.Is(err, ErrOwner) {
		t.Fatalf("another owner on another pod: %v, want ErrOwner", err)
	}
	if st := status(t, a, "ws-1"); st != Online {
		t.Errorf("pod-a: ws-1 is %s after the refusal, want online", st)
	}
	l.Close()
	if _, err := b.Register(Node{ID: "ws-1", Slots: 1}, "mallory@example.com"); !errors.Is(err, ErrOwner) {
		t.Fatalf("another owner, while ws-1 is offline: %v, want ErrOwner", err)
	}
	if b.Forget("ws-1") {
		t.Fatal("pod-b forgot a node it has not yet seen offline")
	}
	renew(t, b)
	if !b.Forget("ws-1") {
		t.Fatal("pod-b did not forget the offline ws-1")
	}
	if _, err := a.Register(Node{ID: "ws-1", Slots: 1}, "bob@example.com"); err != nil {
		t.Fatalf("a new owner after Forget: %v", err)
	}
}

// TestSharedClaims: the lease holder claims on another pod's node; its
// claims last across refills until the node heartbeats, which settles
// them, after which releasing one does nothing.
func TestSharedClaims(t *testing.T) {
	pg := pgtest.Start(t)
	a, b := pod(t, pg.URL, "pod-a"), pod(t, pg.URL, "pod-b")
	l := register(t, a, "ws-1", "alice@example.com", 2)
	renew(t, a, b)
	c, err := b.Claim("bwsalmon/silo")
	if err != nil {
		t.Fatal(err)
	}
	renew(t, b)
	if e, _ := b.Get("ws-1"); e.Claimed != 1 || e.Available() != 1 {
		t.Fatalf("after a refill: %+v, want the claim kept", e)
	}
	if !l.Heartbeat(1) {
		t.Fatal("heartbeat")
	}
	renew(t, b)
	if e, _ := b.Get("ws-1"); e.Claimed != 0 || e.Free != 1 || e.Available() != 1 {
		t.Fatalf("after the heartbeat's refill: %+v, want the claim settled", e)
	}
	c2, err := b.Claim("bwsalmon/silo")
	if err != nil {
		t.Fatal(err)
	}
	c.Release()
	if e, _ := b.Get("ws-1"); e.Claimed != 1 {
		t.Errorf("releasing a settled claim gave back a later one's slot: %+v", e)
	}
	c2.Release()
	if e, _ := b.Get("ws-1"); e.Claimed != 0 {
		t.Errorf("releasing the outstanding claim: %+v", e)
	}
}

// TestSharedSQLite: one process on SQLite. The owner outlasts a
// restart, and a restarted pod sweeps the rows its earlier process left
// online.
func TestSharedSQLite(t *testing.T) {
	url := "sqlite:" + filepath.Join(t.TempDir(), "hub.db")
	r := pod(t, url, "hub")
	register(t, r, "ws-1", "alice@example.com", 1)
	renew(t, r)
	// The process ends without closing its lease.

	r2 := pod(t, url, "hub")
	renew(t, r2)
	if st := status(t, r2, "ws-1"); st != Offline {
		t.Errorf("after a restart: ws-1 is %s, want offline", st)
	}
	var conn string
	if err := r2.Store.db.QueryRow(`SELECT conn FROM hub_nodes WHERE id = 'ws-1'`).Scan(&conn); err != nil || conn != "" {
		t.Errorf("the earlier process's row: conn %q (%v), want swept", conn, err)
	}
	if _, err := r2.Register(Node{ID: "ws-1", Slots: 1}, "bob@example.com"); !errors.Is(err, ErrOwner) {
		t.Errorf("another owner after a restart: %v, want ErrOwner", err)
	}
	l := register(t, r2, "ws-1", "alice@example.com", 1)
	renew(t, r2)
	if st := status(t, r2, "ws-1"); st != Online {
		t.Errorf("reconnected: ws-1 is %s, want online", st)
	}
	l.Close()
	if st := status(t, r2, "ws-1"); st != Offline {
		t.Errorf("closed: ws-1 is %s, want offline", st)
	}
}

// TestSharedUnbind: an ID given up on one pod belongs to nobody on any
// pod, and the connection another pod holds for it ends at its next
// heartbeat.
func TestSharedUnbind(t *testing.T) {
	pg := pgtest.Start(t)
	a, b := pod(t, pg.URL, "pod-a"), pod(t, pg.URL, "pod-b")
	l := register(t, a, "ws-1", "alice@example.com", 1)
	renew(t, a, b)
	if err := b.Unbind("ws-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Get("ws-1"); ok {
		t.Error("pod-b still lists the unbound ws-1")
	}
	if l.Heartbeat(1) {
		t.Fatal("the unbound node's heartbeat succeeded")
	}
	renew(t, a)
	if _, ok := a.Get("ws-1"); ok {
		t.Error("pod-a still lists the unbound ws-1")
	}
	register(t, b, "ws-1", "bob@example.com", 1)
}

// TestSharedRenewDuringRegister: a sweep of the pod's stale rows that
// runs while a node registers does not take the node offline, though
// Register writes the row before it holds the node.
func TestSharedRenewDuringRegister(t *testing.T) {
	r := pod(t, "sqlite:"+filepath.Join(t.TempDir(), "hub.db"), "hub")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			r.renew(ctx)
		}
	}()
	for i := range 100 {
		l := register(t, r, "ws-1", "alice@example.com", 1)
		if !l.Heartbeat(1) {
			t.Fatalf("registration %d: the node no longer holds its row", i)
		}
	}
	cancel()
	<-done
}
