package gateway

import (
	"bufio"
	"context"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/store"
)

// Tests of gateway.md, "What a sealed silo keeps" (SV-82), at the
// gateway and silod: for sealed (control=none) and inner, its child,
// everything outside the API port keeps working, and grants made for
// them stay ACTIVE while they can't change them. The fence's half is in
// pkg/fence's TestSealedSiloKeeps.

// mkInner makes inner, sealed's child, as the root, with a /26 subnet
// web and a VM address in it.
func (e *env) mkInner() string {
	e.t.Helper()
	inner := e.mk("sealed", "inner", 24, obj{"regions": []string{"us-central1"}})
	e.mkSubnet(inner, true)
	return inner
}

// tcpEcho dials addr from ip and reports whether a line comes back.
func tcpEcho(t *testing.T, addr string, ip netip.Addr) bool {
	t.Helper()
	c, err := dialFrom(ctx, addr, ip)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "ping\n")
	line, err := bufio.NewReader(c).ReadString('\n')
	return err == nil && line == "ping\n"
}

func TestSealedSiloKeeps(t *testing.T) {
	e := newEnv(t, envOptions{})
	inner := e.mkInner()
	e.allowExports("a", "sealed", "sealed/inner")
	e.ok(e.root, "PATCH", "/silo/v1/silos/sealed", obj{"egress": obj{"allow": []string{"*.example.com", "github.com"}}})
	e.ok(e.root, "PATCH", "/silo/v1/silos/sealed/inner", obj{"egress": obj{"allow": []string{"*.example.com", "github.com"}}})
	if d := e.ok(e.root, "GET", "/silo/v1/silos/sealed/inner", nil); d["control"].(obj)["effective"] != "none" {
		t.Fatalf("inner's control: %v", d["control"])
	}

	// Grants in both directions, made for the sealed silos by the root:
	// HTTP from sealed to a and from a to inner, TCP from inner to a and
	// from a to sealed.
	sealedWeb := netip.AddrPortFrom(nth(e.subnet[e.sealed], 20), 8080)
	aWeb := netip.AddrPortFrom(nth(e.subnet[e.a], 20), 8080)
	e.ok(e.root, "POST", "/silo/v1/silos/sealed/exports", obj{"name": "web", "target": sealedWeb.String(), "protocol": "http", "to": []string{"/a"}})
	e.ok(e.a, "POST", "/silo/v1/self/imports", obj{"export": "/sealed/web"})
	e.ok(e.a, "POST", "/silo/v1/self/exports", obj{"name": "web", "target": aWeb.String(), "protocol": "http", "to": []string{"/sealed/inner"}})
	e.ok(e.root, "POST", "/silo/v1/silos/sealed/inner/imports", obj{"export": "/a/web"})
	e.ok(e.root, "POST", "/silo/v1/silos/sealed/inner/exports", obj{"name": "db", "target": netip.AddrPortFrom(nth(e.subnet[inner], 20), 5432).String(), "protocol": "tcp", "to": []string{"/a"}})
	innerDB := int(e.ok(e.a, "POST", "/silo/v1/self/imports", obj{"export": "/sealed/inner/db"})["port"].(float64))
	e.ok(e.a, "POST", "/silo/v1/self/exports", obj{"name": "db", "target": netip.AddrPortFrom(nth(e.subnet[e.a], 21), 5432).String(), "protocol": "tcp", "to": []string{"/sealed"}})
	aDB := int(e.ok(e.root, "POST", "/silo/v1/silos/sealed/imports", obj{"export": "/a/db"})["port"].(float64))
	e.sync()

	// HTTP exports, both ways.
	if code, body := e.from80(e.ip[e.a], "/x/"+e.sealed+"/web/hi"); code != 200 || !strings.Contains(body, "caller="+e.a) {
		t.Errorf("a calling sealed's export: %d %q", code, body)
	}
	if code, body := e.from80(e.ip[inner], "/x/"+e.a+"/web/hi"); code != 200 || !strings.Contains(body, "caller="+inner) {
		t.Errorf("inner calling a's export: %d %q", code, body)
	}
	// TCP exports, both ways.
	if !tcpEcho(t, e.listening(innerDB), e.ip[e.a]) {
		t.Errorf("a's connection to inner's TCP export wasn't carried")
	}
	if !tcpEcho(t, e.listening(aDB), e.ip[e.sealed]) {
		t.Errorf("sealed's connection to a's TCP export wasn't carried")
	}

	// Egress through 3128: plain HTTP and CONNECT.
	for _, s := range []string{e.sealed, inner} {
		if code, body := e.get(e.ip[s], "http://www.example.com/x"); code != 200 {
			t.Errorf("egress from %s: %d %q", s, code, body)
		}
		if code, body, tn := e.connect(e.ip[s], "github.com:443"); code != 200 || !tn.echo("hello") {
			t.Errorf("CONNECT from %s: %d %q", s, code, body)
		}
	}

	// The API port: refused at the gateway, and at silod alone.
	for _, s := range []string{e.sealed, inner} {
		for _, c := range []struct{ method, path string }{
			{"GET", "/silo/v1/self"},
			{"GET", "/silo/v1/self/imports"},
			{"GET", "/compute/v1/projects/" + host + "/regions/us-central1/subnetworks"},
		} {
			if code, out := e.from(e.ip[s], c.method, c.path, nil); code != 403 {
				t.Errorf("%s %s from %s through the gateway: %d %v", c.method, c.path, s, code, out)
			}
			if code, out := e.as(s, c.method, c.path, nil); code != 403 {
				t.Errorf("%s %s as %s at silod: %d %v", c.method, c.path, s, code, out)
			}
		}
	}

	// Grants: they can't offer, accept or withdraw, through the
	// gateway or at silod...
	for _, c := range []struct {
		silo, method, path string
		body               any
	}{
		{e.sealed, "POST", "/silo/v1/self/exports", obj{"name": "more", "target": sealedWeb.String(), "protocol": "http", "to": []string{"/a"}}},
		{e.sealed, "DELETE", "/silo/v1/self/exports/web", nil},
		{e.sealed, "DELETE", "/silo/v1/self/imports/" + e.importID(e.sealed, "/a/db"), nil},
		{inner, "POST", "/silo/v1/self/imports", obj{"export": "/a/web"}},
		{inner, "DELETE", "/silo/v1/self/imports/" + e.importID(inner, "/a/web"), nil},
		{inner, "DELETE", "/silo/v1/self/exports/db", nil},
	} {
		if code, out := e.from(e.ip[c.silo], c.method, c.path, c.body); code != 403 {
			t.Errorf("%s %s from %s through the gateway: %d %v", c.method, c.path, c.silo, code, out)
		}
		if code, out := e.as(c.silo, c.method, c.path, c.body); code != 403 || fenceCode(out) != string(fenceapi.ControlDenied) {
			t.Errorf("%s %s as %s at silod: %d %v", c.method, c.path, c.silo, code, out)
		}
	}
	// ... and every grant is still ACTIVE, and still carries traffic.
	for _, ref := range []string{"sealed", "sealed/inner", "a"} {
		for _, im := range e.ok(e.root, "GET", "/silo/v1/silos/"+ref+"/imports", nil)["imports"].([]any) {
			if im.(obj)["state"] != "ACTIVE" {
				t.Errorf("%s's import: %v", ref, im)
			}
		}
	}
	if code, _ := e.from80(e.ip[e.a], "/x/"+e.sealed+"/web/again"); code != 200 {
		t.Errorf("a calling sealed's export after the refusals: %d", code)
	}
	if !tcpEcho(t, e.listening(aDB), e.ip[e.sealed]) {
		t.Errorf("sealed's TCP import after the refusals wasn't carried")
	}
}

// importID is silo's import of export, as the root sees it.
func (e *env) importID(silo, export string) string {
	e.t.Helper()
	for _, im := range e.ok(e.root, "GET", "/silo/v1/silos/-"+silo+"/imports", nil)["imports"].([]any) {
		if im.(obj)["export"] == export {
			return im.(obj)["id"].(string)
		}
	}
	e.t.Fatalf("%s has no import of %s", silo, export)
	return ""
}

// TestSealedAgentToolChannel: an agent in a sealed silo, made by the
// root, attaches its tool channel on port 80 and takes tool calls.
func TestSealedAgentToolChannel(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.tx(func(tx store.Tx) error {
		if err := ledger.SetCapacity(ctx, tx, e.root, []ledger.Amount{
			{Key: ledger.ModelUSD, Amount: ledger.Units(100)}, {Key: fenceapi.AgentSessions, Amount: ledger.Units(5)}}); err != nil {
			return err
		}
		return ledger.Grant(ctx, tx, e.sealed, []ledger.Amount{
			{Key: ledger.ModelUSD, Amount: ledger.Units(50)}, {Key: fenceapi.AgentSessions, Amount: ledger.Units(3)}})
	})
	e.ok(e.root, "POST", "/silo/v1/silos/sealed/agents", obj{"name": "op", "harness": obj{"adapter": "claude-code", "credential": "anthropic-api"},
		"model": "claude-sonnet-5-5", "budget": obj{"usd": "10"}, "lease": obj{"ttl": "2h"},
		"tool_vm": obj{"zone": "us-central1-a", "subnet": e.sealed + "-web", "machine_type": "e2-standard-2"}})
	e.fakeGCE.RunOperations()
	a := e.ok(e.root, "GET", "/silo/v1/silos/sealed/agents/op", nil)
	id := a["id"].(string)
	vmIP, err := netip.ParseAddr(a["tool_vm"].(obj)["ip"].(string))
	if err != nil {
		t.Fatalf("the agent has no tool VM address: %v", a)
	}

	cctx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan struct{})
	go func() { e.toolsClient(vmIP, e.web80, toolsServer(t)).Run(cctx); close(done) }()
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := e.hub.WaitAttached(wctx, id); err != nil {
		t.Fatal(err)
	}
	if out := mcpCall(t, e.hub, id, "exec", obj{"command": "echo sealed"}); !strings.Contains(resultText(out), "sealed") {
		t.Errorf("a tool call in a sealed silo: %v", out)
	}
	// The same VM still can't reach the API port.
	if code, out := e.from(vmIP, "GET", "/silo/v1/self/agents", nil); code != 403 {
		t.Errorf("the tool VM on the API port: %d %v", code, out)
	}
	stop()
	<-done
}

// TestSealedChildAPIPortEachLayer: the API port is refused to a sealed
// silo's child, whose own policy is full, by the gateway alone and by
// silod alone. TestControl does the same for the sealed silo itself.
func TestSealedChildAPIPortEachLayer(t *testing.T) {
	for _, mode := range []struct {
		name string
		o    envOptions
	}{
		{"gateway-alone", envOptions{silodSkipsControl: true}},
		{"silod-alone", envOptions{gatewaySkipsControl: true}},
		// SILO_TEST_SKIP_CONTROL_GATE in silod's conformance build.
		{"silod-alone-by-switch", envOptions{gatewaySkipsNow: func() bool { return true }}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			e := newEnv(t, mode.o)
			inner := e.mkInner()
			e.sync()
			for _, c := range []struct {
				method, path string
				body         any
			}{
				{"GET", "/silo/v1/self", nil},
				{"POST", "/silo/v1/self/exports", obj{"name": "x", "target": netip.AddrPortFrom(e.ip[inner], 80).String(), "protocol": "http", "to": []string{"/a"}}},
				{"GET", "/compute/v1/projects/" + host + "/regions/us-central1/subnetworks", nil},
				{"POST", "/compute/v1/projects/" + host + "/regions/us-central1/subnetworks", obj{"name": inner + "-x", "ipCidrRange": e.subnet[inner].String()}},
			} {
				code, out := e.from(e.ip[inner], c.method, c.path, c.body)
				reason, msg := gceReason(out)
				if code != 403 || (fenceCode(out) != "CONTROL_DENIED" && (reason != "forbidden" || !strings.Contains(msg, "control-plane calls from"))) {
					t.Errorf("%s %s from inner: %d %v", c.method, c.path, code, out)
				}
			}
		})
	}
}
