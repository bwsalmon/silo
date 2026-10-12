package gateway

import (
	"net/netip"
	"testing"
	"time"
)

// TestServesFromCacheWhileSilodUnreachable (gateway.md "Placement"):
// with silod unreachable altogether, not only its stream ended, the
// gateway keeps serving egress and the export tables from what it has,
// and a facade or fence call waits for silod, then fails with 503.
func TestServesFromCacheWhileSilodUnreachable(t *testing.T) {
	e := newEnv(t, envOptions{silodWait: 300 * time.Millisecond})
	e.ok(e.root, "PATCH", "/silo/v1/silos/a", obj{"egress": obj{"allow": []string{"*.example.com"}}})
	e.sync()
	e.stopFollow()
	have := e.gw.Tables()
	e.silodDown.Store(true)

	// Egress, from the cached egress policy.
	if code, body, tn := e.connect(e.ip[e.a], "www.example.com:443"); code != 200 {
		t.Errorf("CONNECT with silod unreachable: %d %q", code, body)
	} else if tn != nil {
		if !tn.echo("ping") {
			t.Error("the tunnel doesn't carry bytes")
		}
		tn.c.Close()
	}
	if code, _, _ := e.connect(e.ip[e.a], "evil.org:443"); code != 403 {
		t.Errorf("a denied CONNECT with silod unreachable: %d", code)
	}
	// Origins and control policies, from the cached tables.
	if s, _, ok := e.gw.Tables().Resolve(e.ip[e.a]); !ok || s != e.a || e.gw.Tables() != have {
		t.Errorf("the cached origin table changed or lost a: %q %v", s, ok)
	}

	// The facade and fence wait, then 503.
	start := time.Now()
	code, out := e.from(e.ip[e.a], "GET", "/silo/v1/self", nil)
	if code != 503 {
		t.Errorf("fence call with silod unreachable: %d %v, want 503", code, out)
	}
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Errorf("the fence call failed after %v, before the wait", d)
	}
	code, _ = e.from(e.ip[e.a], "GET", "/compute/v1/projects/silo-"+e.a+"/regions/us-central1/subnetworks", nil)
	if code != 503 {
		t.Errorf("facade call with silod unreachable: %d, want 503", code)
	}
	// A control-policy refusal needs no silod: still answered at once.
	start = time.Now()
	if code, _ := e.from(netip.MustParseAddr("192.0.2.7"), "GET", "/silo/v1/self", nil); code != 403 {
		t.Errorf("from no block with silod unreachable: %d, want 403", code)
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Errorf("a refusal waited %v for silod", d)
	}
}

// TestFacadeWaitsForSilod: a call made while silod is unreachable goes
// through when silod comes back within the wait.
func TestFacadeWaitsForSilod(t *testing.T) {
	e := newEnv(t, envOptions{silodWait: 10 * time.Second})
	e.silodDown.Store(true)
	time.AfterFunc(400*time.Millisecond, func() { e.silodDown.Store(false) })
	start := time.Now()
	code, out := e.from(e.ip[e.a], "GET", "/silo/v1/self", nil)
	if code != 200 || out["id"] != e.a {
		t.Errorf("a call across silod's return: %d %v", code, out)
	}
	if d := time.Since(start); d < 400*time.Millisecond {
		t.Errorf("answered after %v, before silod came back", d)
	}
}

// TestSilodWaitDefault: 30 seconds, as gateway.md says.
func TestSilodWaitDefault(t *testing.T) {
	if SilodWaitDefault != 30*time.Second {
		t.Errorf("SilodWaitDefault = %v", SilodWaitDefault)
	}
	g := New(Options{})
	if g.opts.SilodWait != 30*time.Second {
		t.Errorf("a gateway's wait is %v by default", g.opts.SilodWait)
	}
	h := netip.MustParseAddr("10.255.255.7")
	if got := New(Options{HostAddress: h}).HostAddress(); got != h {
		t.Errorf("HostAddress = %v", got)
	}
}
