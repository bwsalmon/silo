package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce/fake"
	"github.com/bwsalmon/silo/pkg/silo/fence"
	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/internal/pgtest"
	"github.com/bwsalmon/silo/pkg/silo/leader"
	"github.com/bwsalmon/silo/pkg/silo/store"
	compute "google.golang.org/api/compute/v1"
)

// policyWrites counts the calls that change the firewall policy, so a
// test can tell which replica applied the fence.
type policyWrites struct {
	gce.Client
	n atomic.Int32
}

func (c *policyWrites) PatchFirewallPolicy(ctx context.Context, project, name string, p *compute.FirewallPolicy) (*compute.Operation, error) {
	c.n.Add(1)
	return c.Client.PatchFirewallPolicy(ctx, project, name, p)
}

func (c *policyWrites) AddFirewallPolicyRule(ctx context.Context, project, name string, r *compute.FirewallPolicyRule) (*compute.Operation, error) {
	c.n.Add(1)
	return c.Client.AddFirewallPolicyRule(ctx, project, name, r)
}

func (c *policyWrites) PatchFirewallPolicyRule(ctx context.Context, project, name string, priority int64, r *compute.FirewallPolicyRule) (*compute.Operation, error) {
	c.n.Add(1)
	return c.Client.PatchFirewallPolicyRule(ctx, project, name, priority, r)
}

func (c *policyWrites) RemoveFirewallPolicyRule(ctx context.Context, project, name string, priority int64) (*compute.Operation, error) {
	c.n.Add(1)
	return c.Client.RemoveFirewallPolicyRule(ctx, project, name, priority)
}

// replica is one silod replica in the test's process: its own store
// (its own connections, tagged with its name), fence applier and fence
// API, and the leader loop with the fence as its duty.
type replica struct {
	id    string
	db    *Store
	gce   *policyWrites
	fence *fence.Applier
	api   *httptest.Server
	terms chan *leader.Term
	ended chan *leader.Term
}

// startReplica starts a replica. One that leadsOnce stops its leader
// loop when its first term ends, as a replica that died would, though
// its fence API keeps serving.
func startReplica(t *testing.T, ctx context.Context, srv *pgtest.Server, id string, f *fake.Fake, leadsOnce bool) *replica {
	t.Helper()
	ctx, stop := context.WithCancel(ctx)
	t.Cleanup(stop)
	r := &replica{id: id, db: openMigrated(t, srv, named(id)), gce: &policyWrites{Client: f},
		terms: make(chan *leader.Term, 4), ended: make(chan *leader.Term, 4)}
	r.fence = &fence.Applier{Store: r.db, GCE: r.gce, Project: "host-project", Config: fence.DefaultConfig,
		Poll: 20 * time.Millisecond, Guard: leader.Guard}
	api := fenceapi.New(r.db, fenceapi.Options{Fence: r.fence, Budget: r.fence,
		Caller: func(req *http.Request) (fenceapi.Caller, error) {
			return fenceapi.Caller{Silo: req.Header.Get("Silo-Caller"), Via: fenceapi.ViaInternal}, nil
		}})
	r.api = httptest.NewServer(api)
	t.Cleanup(func() { r.api.Close(); api.Close() })
	done := make(chan struct{})
	go func() {
		defer close(done)
		leader.Run(ctx, leader.Options{Store: r.db, ID: id, Retry: 100 * time.Millisecond, Logf: t.Logf,
			Duties: []leader.Duty{{Name: "fence", Run: func(ctx context.Context) error { return r.fence.Run(ctx, 50*time.Millisecond) }}},
			OnTerm: func(term *leader.Term, started bool) {
				if started {
					r.terms <- term
					return
				}
				if leadsOnce {
					stop()
				}
				r.ended <- term
			}})
	}()
	t.Cleanup(func() { <-done })
	return r
}

func (r *replica) call(t *testing.T, caller, method, path string, body any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, r.api.URL+path, bytes.NewReader(b))
	req.Header.Set("Silo-Caller", caller)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("%s %s on %s: %v", method, path, r.id, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil || resp.StatusCode != 200 {
		t.Fatalf("%s %s on %s: %d %s", method, path, r.id, resp.StatusCode, raw)
	}
	return out
}

func waitTerm(t *testing.T, c chan *leader.Term, what string) *leader.Term {
	t.Helper()
	select {
	case term := <-c:
		return term
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
	return nil
}

// TestLeaderReplicas runs two replicas against one database (SV-52):
// one leads and is the only one to apply the fence, while a fence
// change committed through the other waits for it; killing the
// leader's connections moves the lease, with a new epoch, to the
// other; and the old leader's writes are refused by the epoch.
func TestLeaderReplicas(t *testing.T) {
	srv := pgtest.Start(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := fake.New(fake.Options{Project: "host-project"})
	op, err := f.InsertFirewallPolicy(ctx, "host-project", &compute.FirewallPolicy{Name: fence.PolicyName})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gce.Await(ctx, f, "host-project", op); err != nil {
		t.Fatal(err)
	}

	a := startReplica(t, ctx, srv, "replica-a", f, true)
	ta := waitTerm(t, a.terms, "replica-a to lead")
	b := startReplica(t, ctx, srv, "replica-b", f, false)
	if ta.Epoch != 1 {
		t.Errorf("the first leader's epoch is %d", ta.Epoch)
	}

	// Fence changes made through b: b commits them and waits for the
	// leader, a, to apply them.
	root := b.call(t, "", "POST", "/silod/bootstrap", map[string]any{
		"regions":       []string{"us-central1"},
		"address_space": "10.0.0.0/16", "gateway_range": "10.0.255.0/24", "control_range": "10.0.254.0/24",
		"admins": []string{"user:admin@example.com"},
	})["id"].(string)
	child := b.call(t, root, "POST", "/silo/v1/silos/-"+root+"/children",
		map[string]any{"name": "team", "address": map[string]any{"prefix_length": 24}})["id"].(string)
	rules := func() map[string]int {
		t.Helper()
		pol, err := f.GetFirewallPolicy(ctx, "host-project", fence.PolicyName)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int{}
		for _, r := range pol.Rules {
			out[r.RuleName]++
		}
		return out
	}
	rs := rules()
	for _, suffix := range []string{"-intra-in", "-intra-out", "-gw-out", "-api-out", "-deny-in", "-deny-out"} {
		if rs[child+suffix] != 1 {
			t.Errorf("%s%s is in the policy %d times, want once", child, suffix, rs[child+suffix])
		}
	}
	if n := b.gce.n.Load(); n != 0 {
		t.Errorf("replica-b, not the leader, changed the policy %d times", n)
	}
	if a.gce.n.Load() == 0 {
		t.Error("the leader never changed the policy")
	}
	select {
	case term := <-b.terms:
		t.Fatalf("replica-b led (epoch %d) while replica-a held the lease", term.Epoch)
	default:
	}

	// Kill a's connections: its lease's session among them.
	killed := time.Now()
	if _, err := kill(srv, "replica-a", 1); err != nil {
		t.Fatal(err)
	}
	waitTerm(t, a.ended, "replica-a's term to end")
	tb := waitTerm(t, b.terms, "replica-b to take the lease")
	t.Logf("the lease moved in %v", time.Since(killed))
	if tb.Epoch != ta.Epoch+1 {
		t.Errorf("replica-b's epoch is %d, want %d", tb.Epoch, ta.Epoch+1)
	}

	// a's term is stale: its leader-only writes are refused, through
	// the guard and through the fence's applied-sequence record alike.
	stale := leader.WithTerm(ctx, ta)
	err = a.db.Tx(stale, func(tx store.Tx) error { return leader.Guard(stale, tx) })
	if !errors.Is(err, leader.ErrNotLeader) {
		t.Errorf("the stale leader's guarded write: %v, want ErrNotLeader", err)
	}
	if _, err := a.fence.Apply(stale); !errors.Is(err, leader.ErrNotLeader) {
		t.Errorf("the stale leader's fence apply: %v, want ErrNotLeader", err)
	}

	// The next fence change is the new leader's to apply.
	before := a.gce.n.Load()
	second := a.call(t, root, "POST", "/silo/v1/silos/-"+root+"/children",
		map[string]any{"name": "second", "address": map[string]any{"prefix_length": 24}})["id"].(string)
	if rules()[second+"-api-out"] != 1 {
		t.Errorf("%s-api-out not applied by the new leader", second)
	}
	if b.gce.n.Load() == 0 {
		t.Error("the new leader never changed the policy")
	}
	if n := a.gce.n.Load(); n != before {
		t.Errorf("the old leader changed the policy %d more times after losing the lease", n-before)
	}
}
