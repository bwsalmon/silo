package targets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/netfence"
	"github.com/bwsalmon/silo/pkg/conformance"
	"github.com/bwsalmon/silo/pkg/egress"
	"github.com/bwsalmon/silo/pkg/facade/local/docker"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/store"
)

// gw0's addresses in the fake, as silod's --local-gateway default.
var (
	gatewayAPI    = netip.MustParseAddr("10.255.255.1")
	gatewayDocker = netip.MustParseAddr("10.255.255.2")
)

// What answers on gw0, by port: the gateway's DNS, HTTP (exports and
// silo.internal's routes), egress proxy and export ports, the fence API
// on silo.internal and the Docker API on docker.silo.internal
// (local-backend.md §9). Published ports answer on docker.silo.internal
// too; the fake has no listeners behind them, so the fence alone
// decides.
func gatewayServes(dst netip.Addr, proto uint8, port uint16) bool {
	switch {
	case proto == 1:
		return true
	case port == netfence.PortDNS || port == netfence.PortHTTP || port == netfence.PortEgress:
		return true
	case port >= netfence.DefaultExportPorts[0] && port <= netfence.DefaultExportPorts[1]:
		return true
	case dst == gatewayAPI:
		return port == netfence.PortAPI
	default:
		return dst == gatewayDocker
	}
}

var (
	_ conformance.Fence    = (*fakeRegistry)(nil)
	_ conformance.Unpinned = (*fakeRegistry)(nil)
)

func (Fake) Fence(t testing.TB) (conformance.Fence, error) {
	f, err := newFake(t, true)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// fakeProbe is a container the facade made over the fake sandbox
// driver: a record, a pinned port in the fence's topology and an
// address, which the evaluator then sends from.
type fakeProbe struct {
	f    *fakeRegistry
	silo string
	addr netip.Addr
}

func (f *fakeRegistry) Probe(t testing.TB, silo, name string) conformance.Probe {
	t.Helper()
	// The root's members make it, as the GCP harness makes its probe
	// VMs as the install's admin: through the facade's
	// <id>.docker.silo.internal form, which works for a sealed silo too.
	call := func(method, path string, body any) map[string]any {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, f.dockSrv.URL+path, rd)
		req.Host = silo + "." + docker.Domain
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Silo-Caller", f.root)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("probe %s: %s %s: %v", name, method, path, err)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		if res.StatusCode/100 != 2 {
			t.Fatalf("probe %s: %s %s: %d %s", name, method, path, res.StatusCode, raw)
		}
		out := map[string]any{}
		json.Unmarshal(raw, &out)
		return out
	}
	call("POST", "/v1.47/images/create?fromImage=alpine&tag=3", nil)
	id, _ := call("POST", "/v1.47/containers/create?name="+url.QueryEscape(name), map[string]any{
		"Image": "alpine:3", "Cmd": []string{"sleep", "infinity"},
		"HostConfig": map[string]any{"Runtime": "sandbox"}})["Id"].(string)
	t.Cleanup(func() { call("DELETE", "/v1.47/containers/"+id+"?force=1", nil) })
	call("POST", "/v1.47/containers/"+id+"/start", nil)
	in := call("GET", "/v1.47/containers/"+id+"/json", nil)
	nets, _ := in["NetworkSettings"].(map[string]any)["Networks"].(map[string]any)
	for _, n := range nets {
		ip, _ := n.(map[string]any)["IPAddress"].(string)
		if a, err := netip.ParseAddr(ip); err == nil {
			return &fakeProbe{f: f, silo: silo, addr: a}
		}
	}
	t.Fatalf("probe %s has no address: %v", name, nets)
	return nil
}

// probeImages is the fence install's puller: every reference is the
// same small image, with nothing in the content store, as the fake
// drivers need nothing more to run it.
type probeImages struct{}

func (probeImages) Pull(_ context.Context, _, ref, _ string) (*docker.Pulled, error) {
	name, _, _ := strings.Cut(ref, ":")
	cfg := docker.ImageConfig{Cmd: []string{"sh"}, Env: []string{"PATH=/bin"}}
	b, _ := json.Marshal(cfg)
	return &docker.Pulled{ID: conformance.Digest(b), RepoDigest: name + "@" + conformance.Digest([]byte(ref)),
		Size: 8 << 20, Created: time.Unix(1700000000, 0), Config: cfg}, nil
}

func (p *fakeProbe) Addr() netip.Addr { return p.addr }

func (p *fakeProbe) Run(checks []conformance.Check) map[string]conformance.ProbeResult {
	return p.f.probeSend(p, "", p.addr, checks)
}

func (p *fakeProbe) Spoof(src netip.Addr, checks []conformance.Check) (map[string]conformance.ProbeResult, error) {
	return p.f.probeSend(p, "", src, checks), nil
}

// FromUnpinnedPort sends p's checks through a port on p's bridge that
// has no pin entry.
func (f *fakeRegistry) FromUnpinnedPort(p conformance.Probe, checks []conformance.Check) map[string]conformance.ProbeResult {
	fp := p.(*fakeProbe)
	return f.probeSend(fp, "svunpinned0", fp.addr, checks)
}

// probeSend judges checks sent from p's port (or the port named via, on
// the same bridge) with source address src, with the ruleset the
// applier loads for the store as it is now.
func (f *fakeRegistry) probeSend(p *fakeProbe, via string, src netip.Addr, checks []conformance.Check) map[string]conformance.ProbeResult {
	f.t.Helper()
	ctx := context.Background()
	if _, err := f.applier.Apply(ctx); err != nil {
		f.t.Fatalf("applying the fence: %v", err)
	}
	rs := f.nft.Loaded()
	var topo *netfence.Topology
	if err := f.db.Tx(ctx, func(tx store.Tx) error {
		var err error
		topo, err = f.applier.Topology(ctx, tx)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	nets := map[string]netfence.Network{}
	for _, n := range topo.Networks {
		nets[n.ID] = n
	}
	var from netfence.Port
	held := map[netip.Addr]netfence.Port{}
	for _, pt := range topo.Ports {
		held[pt.Addr] = pt
		if pt.Addr == p.addr {
			from = pt
		}
	}
	if from.Name == "" {
		f.t.Fatalf("probe %s has no port in the fence's topology", p.addr)
	}
	if via != "" {
		from.Name = via
	}
	out := map[string]conformance.ProbeResult{}
	for _, c := range checks {
		out[c.ID] = f.deliver(rs, nets, held, from, src, p.silo, c)
	}
	return out
}

// deliver is one check's packet through silo-net as the kernel would
// take it: the bridge family's pins on the port it leaves by, then, if
// it isn't switched on its own bridge, inet's prerouting and either
// input (for gw0) or forward. A check is ok when every hook accepts
// and something holds the destination: a probe, a service on gw0, or
// for an address outside every silo and gw0, the host's uplink.
func (f *fakeRegistry) deliver(rs *netfence.Ruleset, nets map[string]netfence.Network, held map[netip.Addr]netfence.Port,
	from netfence.Port, src netip.Addr, silo string, c conformance.Check) conformance.ProbeResult {
	fail := func(format string, a ...any) conformance.ProbeResult {
		return conformance.ProbeResult{Detail: fmt.Sprintf(format, a...)}
	}
	var proto uint8
	port := uint16(c.Port)
	switch c.Kind {
	case "tcp":
		proto = 6
	case "udp":
		proto = 17
	case "icmp":
		proto, port = 1, 0
	case "http":
		return f.viaProxy(rs, nets, held, from, src, silo, c)
	default:
		return fail("%s: kind %s", conformance.Unsupported, c.Kind)
	}
	dst, err := netip.ParseAddr(c.Host)
	if err != nil {
		return fail("%s: %q is a name; the fake resolves none", conformance.Unsupported, c.Host)
	}
	net := nets[from.Network]
	pkt := netfence.Packet{IIF: from.Name, EtherType: "ip", EtherSrc: from.MAC.String(), Src: src, Dst: dst, Proto: proto, DPort: port}
	if v, rule := rs.Eval("bridge", "prerouting", pkt); v != "accept" {
		return fail("%s by bridge prerouting rule %d", v, rule)
	}
	if net.Subnet.Contains(dst) {
		if pt, ok := held[dst]; ok && pt.Network == net.ID {
			return conformance.ProbeResult{OK: true, Detail: "switched on " + net.Bridge}
		}
		return fail("no host %s on %s", dst, net.Bridge)
	}
	pkt = netfence.Packet{IIF: net.Bridge, Src: src, Dst: dst, Proto: proto, DPort: port}
	if v, rule := rs.Eval("inet", "prerouting", pkt); v != "accept" {
		return fail("%s by inet prerouting rule %d", v, rule)
	}
	if dst == gatewayAPI || dst == gatewayDocker {
		if v, rule := rs.Eval("inet", "input", pkt); v != "accept" {
			return fail("%s by input rule %d", v, rule)
		}
		if !gatewayServes(dst, proto, port) {
			return fail("connection refused: nothing on %s port %d", dst, port)
		}
		return conformance.ProbeResult{OK: true, Detail: "accepted by gw0"}
	}
	dstPort, isProbe := held[dst]
	if isProbe {
		pkt.OIF = nets[dstPort.Network].Bridge
	} else {
		pkt.OIF = "uplink"
	}
	if v, rule := rs.Eval("inet", "forward", pkt); v != "accept" {
		return fail("%s by forward rule %d", v, rule)
	}
	if isProbe {
		return conformance.ProbeResult{OK: true, Detail: "forwarded to " + pkt.OIF}
	}
	if hostOnly(dst) {
		return fail("no route to %s", dst)
	}
	return conformance.ProbeResult{OK: true, Detail: "forwarded to the uplink"}
}

// hostOnly is whether a is an address that nothing past gw0 answers:
// loopback and link-local. The fence must never forward such a packet;
// this only keeps the fake from claiming an answer it couldn't get.
func hostOnly(a netip.Addr) bool { return a.IsLoopback() || a.IsLinkLocalUnicast() }

// viaProxy is an http check through the egress proxy: the connection
// to the proxy's port on gw0, then the proxy's verdict for the URL's
// host under silo's effective egress. Its detail starts with the HTTP
// status the probe would see: 200 when the proxy lets it through (the
// fake has no upstream behind it), 403 when it refuses.
func (f *fakeRegistry) viaProxy(rs *netfence.Ruleset, nets map[string]netfence.Network, held map[netip.Addr]netfence.Port,
	from netfence.Port, src netip.Addr, silo string, c conformance.Check) conformance.ProbeResult {
	if c.Proxy == "" {
		return conformance.ProbeResult{Detail: conformance.Unsupported + ": http without the egress proxy"}
	}
	pu, err := url.Parse("http://" + c.Proxy)
	if err != nil {
		return conformance.ProbeResult{Detail: conformance.Unsupported + ": proxy " + c.Proxy}
	}
	pport, _ := strconv.Atoi(pu.Port())
	conn := f.deliver(rs, nets, held, from, src, silo, conformance.Check{Kind: "tcp", Host: pu.Hostname(), Port: pport})
	if !conn.OK {
		return conn
	}
	if pport != netfence.PortEgress {
		return conformance.ProbeResult{Detail: conformance.Unsupported + ": a proxy other than the egress proxy"}
	}
	u, err := url.Parse(c.URL)
	if err != nil {
		return conformance.ProbeResult{Detail: "bad URL " + c.URL}
	}
	port, _ := strconv.Atoi(u.Port())
	if port == 0 {
		port = 80
		if u.Scheme == "https" {
			port = 443
		}
	}
	var levels []egress.Policy
	if err := f.db.Tx(context.Background(), func(tx store.Tx) error {
		var err error
		levels, _, err = fenceapi.EffectiveEgress(context.Background(), tx, silo)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	// https is a CONNECT through the proxy, http a plain request.
	def := egress.DefaultHTTPPort
	if u.Scheme == "https" {
		def = egress.DefaultConnectPort
	}
	if ok, _, by := egress.Allowed(levels, u.Hostname(), port, def); !ok {
		return conformance.ProbeResult{OK: true, Detail: "403 denied by the egress proxy " + by}
	}
	// The proxy dials only what egress.Internal passes; a name is
	// resolved first, which the fake can't, so only literals are judged.
	if a, err := netip.ParseAddr(u.Hostname()); err == nil && egress.Internal(a, nil) {
		return conformance.ProbeResult{OK: true, Detail: "403 the egress proxy refuses internal destinations"}
	}
	return conformance.ProbeResult{OK: true, Detail: "200 allowed by egress"}
}
