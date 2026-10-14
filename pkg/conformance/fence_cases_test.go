//go:build silo_conformance

package conformance_test

// The fence's cases (docs/silo/testing.md, "Cases", SV-122): origin
// spoofing, crossing silos without consent (TCP, UDP and ICMP; the
// export and link sub-cases are in grant_cases_test.go), egress and the
// gateway, the database cases cases_test.go doesn't have, sealed and
// read silos with the conformance build's test switches, control
// inheritance, manage grants, and the fence against the fake's
// evaluator.
//
// A probe can't be reached from outside the tree, so everything a case
// does from inside a silo is a check in a probe's startup script, with
// its result on the serial console. A case that has to act while a
// probe runs (revoke, seal, narrow egress) waits for a "mark" check the
// probe reports first.

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/netip"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/gcp/gce/fake/fenceeval"
	"github.com/bwsalmon/silo/pkg/client"
	. "github.com/bwsalmon/silo/pkg/conformance"
	"github.com/bwsalmon/silo/pkg/conformance/gcpharness"
	"github.com/bwsalmon/silo/pkg/gateway"
	compute "google.golang.org/api/compute/v1"
)

// api is where a probe reaches the gateway's API port.
const api = "http://silo.internal:8080"

// proxy is the gateway's egress proxy, as a probe names it.
const proxy = "http://silo.internal:3128"

// addrIn is the n'th address of s's net subnet, for a probe at a fixed
// address another probe can name.
func addrIn(s siloInfo, n int) string {
	a := s.subnet.Addr()
	for i := 0; i < n; i++ {
		a = a.Next()
	}
	return a.String()
}

// mkSilo creates path with a /prefix block, control (if not ""), egress
// (if not nil) and a net subnet, for the cases that need more than
// /a, /a/c and /b.
func mkSilo(t *testing.T, path string, prefix int, control string, egress *client.Egress) siloInfo {
	t.Helper()
	ctx := context.Background()
	q := client.QuotaMap{run.Region: {
		"vcpu": num(2), "spot_vcpu": num(2), "memory_mb": num(4096), "spot_memory_mb": num(4096),
		"instances": num(3), "standard_disk_gb": num(40), "ssd_disk_gb": num(10),
	}, "global": {"subnetworks": num(2)}}
	s, err := run.API.Create(ctx, path, client.CreateRequest{Regions: []string{run.Region}, Quota: q,
		Address: &client.AddressWant{PrefixLength: prefix}, Control: control, Egress: egress}, client.Mutation{})
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	t.Cleanup(func() { run.API.Delete(context.Background(), path, true, client.Mutation{}) })
	blk := netip.MustParsePrefix(s.Address.Blocks[0])
	info := siloInfo{path: path, id: s.ID, project: "silo-" + s.ID, block: blk, subnet: netOf(blk)}
	code, op := facade(t, "POST", "projects/"+info.project+"/regions/"+run.Region+"/subnetworks",
		map[string]any{"name": s.ID + "-net", "network": "default", "ipCidrRange": info.subnet.String()})
	if code != 200 {
		t.Fatalf("subnet in %s: %d %v", path, code, op)
	}
	waitOp(t, info.project, op)
	return info
}

// vm is a probe started with startProbe.
type vm struct {
	t       *testing.T
	s       siloInfo
	name    string
	stopped bool
}

// startProbe makes a probe VM, s's prefix and name, in s, at ip if not "", running checks, and
// returns at once. wait reads its report and deletes it.
func startProbe(t *testing.T, s siloInfo, name, ip string, checks []Check) *vm {
	t.Helper()
	run.VMs <- struct{}{}
	name = s.id + "-" + name
	body := instanceBody(name, s, func(m map[string]any) {
		m["metadata"] = map[string]any{"items": []any{map[string]any{"key": "startup-script", "value": ProbeScript(checks)}}}
		if ip != "" {
			m["networkInterfaces"].([]any)[0].(map[string]any)["networkIP"] = ip
		}
	})
	code, op := facade(t, "POST", instances(s), body)
	if code != 200 {
		<-run.VMs
		t.Fatalf("probe %s in %s: %d %v", name, s.path, code, op)
	}
	waitOp(t, s.project, op)
	v := &vm{t: t, s: s, name: name}
	t.Cleanup(v.delete)
	return v
}

func (v *vm) serial() string {
	_, out := facade(v.t, "GET", instances(v.s)+"/"+v.name+"/serialPort?port=1", nil)
	text, _ := out["contents"].(string)
	return text
}

// until waits for the probe to report check id, and returns its result.
func (v *vm) until(id string, d time.Duration) (ProbeResult, bool) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		res, _ := ParseProbeReport(v.serial())
		if r, ok := res[id]; ok {
			return r, true
		}
		time.Sleep(10 * time.Second)
	}
	return ProbeResult{}, false
}

// wait waits for the probe's whole report, then deletes it.
func (v *vm) wait() map[string]ProbeResult {
	v.t.Helper()
	defer v.delete()
	deadline := time.Now().Add(12 * time.Minute)
	for time.Now().Before(deadline) {
		if res, done := ParseProbeReport(v.serial()); done {
			return res
		}
		time.Sleep(15 * time.Second)
	}
	v.t.Fatalf("probe %s in %s never reported", v.name, v.s.path)
	return nil
}

func (v *vm) delete() {
	if v.stopped {
		return
	}
	v.stopped = true
	_, op := facade(v.t, "DELETE", instances(v.s)+"/"+v.name, nil)
	waitOp(v.t, v.s.project, op)
	<-run.VMs
}

// mark is a check a probe reports as soon as it boots, so a case can act
// while the rest of its checks run.
var mark = Check{ID: "mark", Kind: "sh", Script: "true"}

// expect asserts res[id] is ok (want true) or failed.
func expect(t *testing.T, res map[string]ProbeResult, id string, want bool) {
	t.Helper()
	r, ok := res[id]
	switch {
	case !ok:
		t.Errorf("%s: no result", id)
	case r.OK != want:
		t.Errorf("%s: ok=%v, want %v: %s", id, r.OK, want, r.Detail)
	}
}

// expectStatus asserts the http check id answered with status, and that
// its body names each of has.
func expectStatus(t *testing.T, res map[string]ProbeResult, id string, status int, has ...string) {
	t.Helper()
	r, ok := res[id]
	if !ok {
		t.Errorf("%s: no result", id)
		return
	}
	if got := HTTPStatus(r); got != status {
		t.Errorf("%s: status %d, want %d: %s", id, got, status, r.Detail)
		return
	}
	for _, h := range has {
		if !strings.Contains(r.Detail, h) {
			t.Errorf("%s: answer doesn't name %s: %s", id, h, r.Detail)
		}
	}
}

// refusedFromProbe asserts the http check id was refused: an answer of
// 403 (or 404 where the caller mustn't see the target), or none at all.
func refusedFromProbe(t *testing.T, res map[string]ProbeResult, id string) {
	t.Helper()
	r, ok := res[id]
	if !ok {
		t.Errorf("%s: no result", id)
		return
	}
	if c := HTTPStatus(r); c >= 200 && c < 300 {
		t.Errorf("%s: got %d, want a refusal: %s", id, c, r.Detail)
	}
}

// allowAll gives s tenant firewall rules that allow everything both
// ways, so that only the fence can stop a packet.
func allowAll(t *testing.T, s siloInfo) {
	t.Helper()
	for _, dir := range []string{"INGRESS", "EGRESS"} {
		body := map[string]any{"name": s.id + "-allow-all-" + strings.ToLower(dir), "network": "default", "direction": dir,
			"priority": 100, "allowed": []any{map[string]any{"IPProtocol": "all"}}}
		if dir == "INGRESS" {
			body["sourceRanges"] = []string{"0.0.0.0/0"}
		} else {
			body["destinationRanges"] = []string{"0.0.0.0/0"}
		}
		code, op := facade(t, "POST", "projects/"+s.project+"/global/firewalls", body)
		if code != 200 {
			t.Fatalf("allow-all in %s: %d %v", s.path, code, op)
		}
		waitOp(t, s.project, op)
		name := body["name"].(string)
		t.Cleanup(func() {
			_, op := facade(t, "DELETE", "projects/"+s.project+"/global/firewalls/"+name, nil)
			waitOp(t, s.project, op)
		})
	}
}

// listeners is a script that serves TCP 7000 (HTTP) and UDP 7001 (echo)
// for d seconds in the background.
func listeners(d int) Check {
	return Check{ID: "listen", Kind: "sh", Script: fmt.Sprintf(`
(timeout %[1]d python3 -m http.server 7000 >/dev/null 2>&1 &)
(timeout %[1]d python3 -c '
import socket
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind(("0.0.0.0",7001))
while True:
    d,a=s.recvfrom(2048); s.sendto(d,a)' >/dev/null 2>&1 &)
sleep 2; nc -z -w 3 127.0.0.1 7000`, d)}
}

// crossChecks are TCP, UDP and ICMP to host, after waiting for the
// other probes' listeners.
func crossChecks(prefix, host string) []Check {
	return []Check{
		{ID: prefix + "-tcp", Kind: "tcp", Host: host, Port: 7000},
		{ID: prefix + "-udp", Kind: "udp", Host: host, Port: 7001},
		{ID: prefix + "-icmp", Kind: "icmp", Host: host},
	}
}

var settle = Check{ID: "settle", Kind: "sh", Script: "sleep 90"}

// TestOriginSpoofing: whatever a caller claims, the gateway names its
// silo by the packet's source address alone.
func TestOriginSpoofing(t *testing.T) {
	tree(t)
	ctx := context.Background()
	tok, err := exec.Command("gcloud", "auth", "print-access-token").Output()
	if err != nil {
		t.Fatalf("a Google token for the host project: %v", err)
	}
	forged := gateway.FormatOrigin(b.id, 1)
	bList := api + "/compute/v1/projects/" + b.project + "/zones/" + run.Zone + "/instances"
	hostList := api + "/compute/v1/projects/" + run.Project + "/zones/" + run.Zone + "/instances"
	res := probe(t, a, "probe-origin", []Check{
		{ID: "forged-self", Kind: "http", URL: api + "/silo/v1/self", Headers: []string{"Silo-Origin: " + forged}},
		{ID: "forged-b", Kind: "http", URL: bList, Headers: []string{"Silo-Origin: " + forged}},
		{ID: "token-self", Kind: "http", URL: api + "/silo/v1/self", Headers: []string{"Authorization: Bearer " + strings.TrimSpace(string(tok))}},
		{ID: "token-host", Kind: "http", URL: hostList, Headers: []string{"Authorization: Bearer " + strings.TrimSpace(string(tok))}},
		// An address in /a's block but in no subnet of it, added to the
		// probe's own NIC: GCE drops it, or the gateway refuses it.
		{ID: "no-subnet", Kind: "sh", Script: fmt.Sprintf(
			"ip addr add %s/32 dev $(ip route show default | awk '{print $5}') && curl -sS -m 15 --interface %[1]s -o /dev/null -w '%%{http_code}' %s/silo/v1/self | grep -q '^2'",
			addrOf(a.block, 256+5).String(), api)},
	})
	t.Run("ForgedSiloOrigin", func(t *testing.T) {
		expectStatus(t, res, "forged-self", 200, `"path":"/a"`)
		refusedFromProbe(t, res, "forged-b")
	})
	t.Run("RealGoogleToken", func(t *testing.T) {
		expectStatus(t, res, "token-self", 200, `"path":"/a"`)
		refusedFromProbe(t, res, "token-host")
	})
	t.Run("IPInBlockButNoSubnet", func(t *testing.T) {
		expect(t, res, "no-subnet", false)
	})
	t.Run("RawSocketFromAnotherSilosAddress", func(t *testing.T) {
		lis, spoof := addrIn(b, 10), addrIn(b, 11)
		listener := startProbe(t, b, "probe-spoof-rx", lis, []Check{
			{ID: "got", Kind: "sh", Script: "timeout 300 python3 -c '\nimport socket\ns=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind((\"0.0.0.0\",7002))\nprint(s.recvfrom(64))'"},
		})
		sender := startProbe(t, a, "probe-spoof-tx", "", []Check{settle,
			{ID: "sent", Kind: "sh", Script: fmt.Sprintf(`python3 - <<'EOF'
import socket, struct, time
src, dst = socket.inet_aton(%q), socket.inet_aton(%q)
s = socket.socket(socket.AF_INET, socket.SOCK_RAW, socket.IPPROTO_RAW)
udp = struct.pack("!HHHH", 7002, 7002, 8+5, 0) + b"spoof"
ip = struct.pack("!BBHHHBBH4s4s", 0x45, 0, 20+len(udp), 1, 0, 64, 17, 0, src, dst)
for _ in range(60):
    s.sendto(ip+udp, (%[2]q, 0)); time.sleep(2)
EOF`, spoof, lis)}})
		sres := sender.wait()
		expect(t, sres, "sent", true)
		lres := listener.wait()
		expect(t, lres, "got", false)
		refused(t, 400, "POST", instances(a), instanceBody(a.id+"-forwarder", a, func(m map[string]any) { m["canIpForward"] = true }), instances(a)+"/"+a.id+"-forwarder")
	})
	t.Run("JustAfterChildDeleted", func(t *testing.T) {
		d := mkSilo(t, "/a/d", 26, "", nil)
		ip := addrIn(d, 5)
		if _, err := run.API.Delete(ctx, "/a/d", true, client.Mutation{}); err != nil {
			t.Fatalf("delete /a/d: %v", err)
		}
		// /a/d's block is /a's again: a subnet of /a's over it, and a
		// probe at the address /a/d's VM would have had, acts as /a.
		p := "projects/" + a.project + "/regions/" + run.Region + "/subnetworks"
		code, op := facade(t, "POST", p, map[string]any{"name": a.id + "-was-d", "network": "default", "ipCidrRange": d.subnet.String()})
		if code != 200 {
			t.Fatalf("a subnet over /a/d's old block: %d %v", code, op)
		}
		waitOp(t, a.project, op)
		t.Cleanup(func() { _, op := facade(t, "DELETE", p+"/"+a.id+"-was-d", nil); waitOp(t, a.project, op) })
		v := startProbe(t, a, "probe-was-d", ip, []Check{{ID: "self", Kind: "http", URL: api + "/silo/v1/self"}})
		res := v.wait()
		expectStatus(t, res, "self", 200, `"path":"/a"`)
		if strings.Contains(res["self"].Detail, d.id) {
			t.Errorf("a call from /a/d's old address was taken as /a/d's: %s", res["self"].Detail)
		}
	})
}

// TestCrossingSilosWithoutConsent: /a → /b, /a → /a/c and /a/c → /a, on
// TCP, UDP and ICMP, with tenant rules on every side that allow
// everything: unreachable. Each probe also reaches its own listener at
// its own address, which shows the listeners were up.
func TestCrossingSilosWithoutConsent(t *testing.T) {
	tree(t)
	for _, s := range []siloInfo{a, b, c} {
		allowAll(t, s)
	}
	ipA, ipB, ipC := addrIn(a, 20), addrIn(b, 20), addrIn(c, 20)
	pa := startProbe(t, a, "probe-cross-a", ipA, append(append([]Check{listeners(600), settle,
		{ID: "own-tcp", Kind: "tcp", Host: ipA, Port: 7000}}, crossChecks("a-b", ipB)...), crossChecks("a-c", ipC)...))
	pb := startProbe(t, b, "probe-cross-b", ipB, []Check{listeners(600), {ID: "hold", Kind: "sh", Script: "sleep 300"}})
	pc := startProbe(t, c, "probe-cross-c", ipC, append([]Check{listeners(600), settle,
		{ID: "own-tcp", Kind: "tcp", Host: ipC, Port: 7000}}, crossChecks("c-a", ipA)...))
	ra, rc := pa.wait(), pc.wait()
	pb.wait()
	expect(t, ra, "own-tcp", true)
	expect(t, rc, "own-tcp", true)
	for _, c := range []struct {
		name string
		res  map[string]ProbeResult
		pfx  string
	}{{"AToB", ra, "a-b"}, {"AToChild", ra, "a-c"}, {"ChildToA", rc, "c-a"}} {
		t.Run(c.name, func(t *testing.T) {
			for _, p := range []string{"tcp", "udp", "icmp"} {
				expect(t, c.res, c.pfx+"-"+p, false)
			}
		})
	}
}

// setEgress sets s's own egress policy.
func setEgress(t *testing.T, path string, e client.Egress) {
	t.Helper()
	if _, err := run.API.Patch(context.Background(), path, client.Patch{"egress": e}, client.Mutation{}); err != nil {
		t.Fatalf("egress of %s: %v", path, err)
	}
}

// TestEgressAndGateway: through HTTPS_PROXY an allowed host works; a
// disallowed one, one that resolves inside, and a direct connection
// don't. Narrowing an ancestor's egress closes the child's open CONNECT.
func TestEgressAndGateway(t *testing.T) {
	tree(t)
	setEgress(t, "/a", client.Egress{Allow: []string{"example.com"}})
	setEgress(t, "/a/c", client.Egress{Allow: []string{"example.com"}})
	t.Cleanup(func() {
		setEgress(t, "/a/c", client.Egress{})
		setEgress(t, "/a", client.Egress{})
	})
	res := probe(t, a, "probe-egress", []Check{
		{ID: "allowed", Kind: "http", URL: "https://example.com/", Proxy: proxy},
		{ID: "disallowed", Kind: "http", URL: "https://www.wikipedia.org/", Proxy: proxy},
		{ID: "internal", Kind: "http", URL: "http://metadata.google.internal/", Proxy: proxy},
		{ID: "direct", Kind: "http", URL: "https://example.com/"},
	})
	t.Run("AllowedHost", func(t *testing.T) { expectStatus(t, res, "allowed", 200) })
	t.Run("DisallowedHost", func(t *testing.T) { refusedFromProbe(t, res, "disallowed") })
	t.Run("InternalAddress", func(t *testing.T) { refusedFromProbe(t, res, "internal") })
	t.Run("DirectWithoutProxy", func(t *testing.T) { refusedFromProbe(t, res, "direct") })
	t.Run("NarrowingAncestorClosesConnect", func(t *testing.T) {
		v := startProbe(t, c, "probe-narrow", "", []Check{settle, mark,
			{ID: "held", Kind: "sh", Script: `s=$(date +%s); (sleep 200) | timeout 210 nc -X connect -x silo.internal:3128 example.com 443 >/dev/null; e=$(( $(date +%s)-s )); echo "held ${e}s"; [ $e -lt 120 ]`}})
		if _, ok := v.until("mark", 10*time.Minute); !ok {
			t.Fatal("the probe never started its CONNECT")
		}
		time.Sleep(20 * time.Second)
		setEgress(t, "/a", client.Egress{})
		res := v.wait()
		expect(t, res, "held", true)
	})
}

// TestDatabaseThroughTheGateway: CONNECT to the endpoint's address or
// name is refused even with * in the egress policy, and an export
// targeting it is refused.
func TestDatabaseThroughTheGateway(t *testing.T) {
	tree(t)
	addr, err := run.Compute.Addresses.Get(run.Project, run.Region, "silo-db-psc").Do()
	if err != nil {
		t.Fatalf("the PSC endpoint's address: %v", err)
	}
	ep := addr.Address
	dnsName := ""
	if l, err := run.SQL.Instances.List(run.Project).Do(); err == nil {
		for _, o := range l.Items {
			if o.Settings != nil && o.Settings.UserLabels["silo-install"] != "" {
				dnsName = strings.TrimSuffix(o.DnsName, ".")
			}
		}
	}
	setEgress(t, "/a", client.Egress{Allow: []string{"*"}})
	t.Cleanup(func() { setEgress(t, "/a", client.Egress{}) })
	checks := []Check{{ID: "connect-addr", Kind: "sh", Script: "timeout 15 nc -X connect -x silo.internal:3128 -z " + ep + " 5432"}}
	if dnsName != "" {
		checks = append(checks, Check{ID: "connect-name", Kind: "sh", Script: "timeout 15 nc -X connect -x silo.internal:3128 -z " + dnsName + " 5432"})
	}
	res := probe(t, a, "probe-db-connect", checks)
	t.Run("ConnectToAddressRefused", func(t *testing.T) { expect(t, res, "connect-addr", false) })
	t.Run("ConnectToNameRefused", func(t *testing.T) {
		if dnsName == "" {
			t.Skip("the instance has no DNS name")
		}
		expect(t, res, "connect-name", false)
	})
	t.Run("ExportTargetingEndpointRefused", func(t *testing.T) {
		_, err := run.API.OfferExport(context.Background(), "/a", client.OfferRequest{Name: "db", Target: ep + ":5432", Protocol: "tcp", To: []string{"/b"}}, client.Mutation{})
		if err == nil {
			run.API.WithdrawExport(context.Background(), "/a", "db", "", client.Mutation{})
			t.Fatal("an export of the database endpoint was offered")
		}
	})
}

// controlVMs are the control plane's VMs in the home zone: silo-control,
// or the replicas of silo-control-mig.
func controlVMs(t *testing.T) []*compute.Instance {
	t.Helper()
	l, err := run.Compute.Instances.List(run.Project, run.Zone).Filter(`name = "silo-control*"`).Do()
	if err != nil {
		t.Fatalf("listing the control VMs: %v", err)
	}
	var out []*compute.Instance
	for _, i := range l.Items {
		if i.Name != "silo-control-anchor" {
			out = append(out, i)
		}
	}
	return out
}

// TestLeaderFailover: with --control-replicas 2, stopping the leader's
// VM moves the lease, and a fence change made just after is applied
// exactly once. Nothing outside silod names the leader, so the case
// stops each replica in turn: one of the two stops was the leader's.
func TestLeaderFailover(t *testing.T) {
	installed(t)
	ctx := context.Background()
	vms := controlVMs(t)
	if len(vms) < 2 {
		t.Fatalf("%d control VMs, want 2 (--control-replicas 2)", len(vms))
	}
	for i, v := range vms {
		t.Run("Stop_"+v.Name, func(t *testing.T) {
			op, err := run.Compute.Instances.Stop(run.Project, run.Zone, v.Name).Do()
			if err != nil {
				t.Fatalf("stopping %s: %v", v.Name, err)
			}
			defer func() {
				run.Compute.Instances.Start(run.Project, run.Zone, v.Name).Do()
				waitHealthy(t, 10*time.Minute)
			}()
			_ = op
			path := fmt.Sprintf("/failover%d", i)
			var s *client.Silo
			deadline := time.Now().Add(5 * time.Minute)
			for {
				s, err = run.API.Create(ctx, path, client.CreateRequest{Regions: []string{run.Region},
					Address: &client.AddressWant{PrefixLength: 26}}, client.Mutation{RequestID: fmt.Sprintf("failover-%d-%d", i, time.Now().Unix()/3600)})
				if err == nil || time.Now().After(deadline) {
					break
				}
				time.Sleep(10 * time.Second)
			}
			if err != nil {
				t.Fatalf("a fence change after stopping %s: %v", v.Name, err)
			}
			defer run.API.Delete(ctx, path, true, client.Mutation{})
			// Applied: the silo's rules are in the policy, each once.
			deadline = time.Now().Add(3 * time.Minute)
			for {
				n, dup := fenceRulesOf(t, s.ID)
				if dup != "" {
					t.Fatalf("rule %s is in the fence twice", dup)
				}
				if n > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s's fence rules never appeared after stopping %s", path, v.Name)
				}
				time.Sleep(10 * time.Second)
			}
		})
	}
}

// fenceRulesOf counts the fence policy's rules for silo id, and names
// any rule (by its match and action) that appears twice.
func fenceRulesOf(t *testing.T, id string) (int, string) {
	t.Helper()
	pol, err := run.Compute.NetworkFirewallPolicies.Get(run.Project, "silo-fence").Do()
	if err != nil {
		t.Fatalf("the fence policy: %v", err)
	}
	n, seen := 0, map[string]bool{}
	for _, r := range pol.Rules {
		if !strings.HasPrefix(r.RuleName, id+"-") {
			continue
		}
		n++
		key := fmt.Sprintf("%s %s %v", r.Direction, r.Action, r.Match)
		if seen[r.RuleName] || seen[key] {
			return n, r.RuleName
		}
		seen[r.RuleName], seen[key] = true, true
	}
	return n, ""
}

func waitHealthy(t *testing.T, d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := run.API.Describe(context.Background(), "/", false); err == nil {
			return
		}
		time.Sleep(10 * time.Second)
	}
	t.Errorf("silod didn't come back within %v", d)
}

// setSwitches sets the conformance build's test switches on every
// control VM (testswitch_conformance.go in cmd/silod), and waits for
// silod to mirror them and the fence to apply.
func setSwitches(t *testing.T, names ...string) {
	t.Helper()
	val := strings.Join(names, ",")
	for _, v := range controlVMs(t) {
		md := v.Metadata
		var items []*compute.MetadataItems
		for _, it := range md.Items {
			if it.Key != "silo-test-switches" {
				items = append(items, it)
			}
		}
		if val != "" {
			items = append(items, &compute.MetadataItems{Key: "silo-test-switches", Value: &val})
		}
		md.Items = items
		if _, err := run.Compute.Instances.SetMetadata(run.Project, run.Zone, v.Name, md).Do(); err != nil {
			t.Fatalf("switches on %s: %v", v.Name, err)
		}
	}
	time.Sleep(45 * time.Second)
}

// TestSealedAndReadSilos: /s (sealed) and /r (read), each with a probe.
func TestSealedAndReadSilos(t *testing.T) {
	tree(t)
	ctx := context.Background()
	s := mkSilo(t, "/s", 26, "none", &client.Egress{Allow: []string{"example.com"}})
	r := mkSilo(t, "/r", 26, "read", nil)
	sList := "/compute/v1/projects/" + s.project + "/zones/" + run.Zone + "/instances"
	apiChecks := []Check{
		{ID: "facade-read", Kind: "http", URL: api + sList},
		{ID: "facade-write", Kind: "http", Method: "POST", URL: api + "/compute/v1/projects/" + s.project + "/regions/" + run.Region + "/subnetworks",
			Body: `{"name":"x","network":"default","ipCidrRange":"` + netip.PrefixFrom(addrOf(s.block, 32), 28).String() + `"}`},
		{ID: "fence-read", Kind: "http", URL: api + "/silo/v1/self"},
		{ID: "fence-write", Kind: "http", Method: "PATCH", URL: api + "/silo/v1/self", Body: `{"labels":{"x":"y"}}`},
	}
	t.Run("SealedProbe", func(t *testing.T) {
		res := probe(t, s, "probe-sealed", []Check{
			{ID: "api-port", Kind: "tcp", Host: "silo.internal", Port: 8080},
			{ID: "http-port", Kind: "tcp", Host: "silo.internal", Port: 80},
			{ID: "proxy-port", Kind: "tcp", Host: "silo.internal", Port: 3128},
			{ID: "egress", Kind: "http", URL: "https://example.com/", Proxy: proxy},
		})
		expect(t, res, "api-port", false)
		expect(t, res, "http-port", true)
		expect(t, res, "proxy-port", true)
		expectStatus(t, res, "egress", 200)
	})
	gate := func(t *testing.T, name string) {
		res := probe(t, s, name, apiChecks)
		expectStatus(t, res, "facade-read", 403, "forbidden")
		expectStatus(t, res, "facade-write", 403, "forbidden")
		expectStatus(t, res, "fence-read", 403, "CONTROL_DENIED")
		expectStatus(t, res, "fence-write", 403, "CONTROL_DENIED")
		code, out := facade(t, "GET", "projects/"+s.project+"/regions/"+run.Region+"/subnetworks/x", nil)
		if code != 404 {
			t.Errorf("a refused subnet exists: %d %v", code, out)
		}
	}
	t.Run("KeepControlPort", func(t *testing.T) {
		setSwitches(t, "SILO_TEST_KEEP_CONTROL_PORT")
		defer setSwitches(t)
		gate(t, "probe-sealed-port")
	})
	t.Run("SkipControlGate", func(t *testing.T) {
		setSwitches(t, "SILO_TEST_KEEP_CONTROL_PORT", "SILO_TEST_SKIP_CONTROL_GATE")
		defer setSwitches(t)
		gate(t, "probe-sealed-silod")
	})
	t.Run("FrontDoorMemberMappedToSealed", func(t *testing.T) {
		t.Skip("needs a second identity the suite's key can mint tokens for; not covered yet")
	})
	t.Run("ReadSilo", func(t *testing.T) {
		rp := "/compute/v1/projects/" + r.project
		writes := map[string][2]string{
			"instances.insert":   {"POST", rp + "/zones/" + run.Zone + "/instances"},
			"disks.insert":       {"POST", rp + "/zones/" + run.Zone + "/disks"},
			"subnetworks.insert": {"POST", rp + "/regions/" + run.Region + "/subnetworks"},
			"firewalls.insert":   {"POST", rp + "/global/firewalls"},
			"images.insert":      {"POST", rp + "/global/images"},
			"subnetworks.delete": {"DELETE", rp + "/regions/" + run.Region + "/subnetworks/" + r.id + "-net"},
			"fence.create":       {"POST", "/silo/v1/self/children"},
			"fence.patch":        {"PATCH", "/silo/v1/self"},
			"fence.grow":         {"POST", "/silo/v1/self:grow"},
			"fence.exportOffer":  {"POST", "/silo/v1/self/exports"},
			"fence.linkPropose":  {"POST", "/silo/v1/self/links"},
			"fence.linkDelete":   {"DELETE", "/silo/v1/self/links/x"},
			"control.full":       {"PATCH", "/silo/v1/self"},
			"control.none":       {"PATCH", "/silo/v1/self"},
		}
		bodies := map[string]string{
			"instances.insert": `{"name":"` + r.id + `-x"}`, "disks.insert": `{"name":"` + r.id + `-x","sizeGb":"10"}`,
			"subnetworks.insert": `{"name":"` + r.id + `-x","network":"default","ipCidrRange":"` + netip.PrefixFrom(addrOf(r.block, 32), 28).String() + `"}`,
			"firewalls.insert":   `{"name":"` + r.id + `-x","network":"default","allowed":[{"IPProtocol":"tcp"}]}`, "images.insert": `{"name":"` + r.id + `-x"}`,
			"fence.create": `{"name":x","address":{"prefix_length":28}}`, "fence.patch": `{"labels":{"x":"y"}}`,
			"fence.grow": `{"to_prefix_length":25}`, "fence.exportOffer": `{"name":x","target":"` + addrIn(r, 5) + `:80","protocol":"http","to":["/a"]}`,
			"fence.linkPropose": `{"name":x","peer":"/a","direction":"both","rules":[{"protocol":"tcp","ports":["22"]}]}`,
			"control.full":      `{"control":"full"}`, "control.none": `{"control":"none"}`,
		}
		checks := []Check{
			{ID: "read-list", Kind: "http", URL: api + rp + "/zones/" + run.Zone + "/instances"},
			{ID: "read-subnets", Kind: "http", URL: api + rp + "/regions/" + run.Region + "/subnetworks"},
			{ID: "read-self", Kind: "http", URL: api + "/silo/v1/self"},
			{ID: "read-wait", Kind: "http", Method: "POST", URL: api + rp + "/zones/" + run.Zone + "/operations/no-such-op/wait"},
		}
		for id, w := range writes {
			checks = append(checks, Check{ID: id, Kind: "http", Method: w[0], URL: api + w[1], Body: bodies[id]})
		}
		res := probe(t, r, "probe-read", checks)
		expectStatus(t, res, "read-list", 200)
		expectStatus(t, res, "read-subnets", 200)
		expectStatus(t, res, "read-self", 200, `"path":"/r"`)
		if c := HTTPStatus(res["read-wait"]); c == 403 {
			t.Errorf("zoneOperations.wait refused from a read silo: %s", res["read-wait"].Detail)
		}
		for id := range writes {
			if strings.HasPrefix(id, "control.") {
				expectStatus(t, res, id, 403, "PERMISSION_DENIED")
				continue
			}
			refusedFromProbe(t, res, id)
		}
		for _, p := range []string{rp + "/zones/" + run.Zone + "/instances/" + r.id + "-x", rp + "/zones/" + run.Zone + "/disks/" + r.id + "-x",
			rp + "/regions/" + run.Region + "/subnetworks/" + r.id + "-x", rp + "/global/firewalls/" + r.id + "-x", rp + "/global/images/" + r.id + "-x"} {
			if code, _ := facade(t, "GET", strings.TrimPrefix(p, "/compute/v1/"), nil); code != 404 {
				t.Errorf("%s exists after a refused write (%d)", p, code)
			}
		}
		if kids, err := run.API.Children(ctx, "/r"); err == nil && len(kids) > 0 {
			t.Errorf("/r has children after a refused create: %d", len(kids))
		}
		if d, err := run.API.Describe(ctx, "/r", false); err == nil && d.Control.Own != "read" {
			t.Errorf("/r's control is %q after refused PATCHes", d.Control.Own)
		}
	})
	t.Run("AncestorWritesAndAllocations", func(t *testing.T) {
		for _, x := range []siloInfo{s, r} {
			p := "projects/" + x.project + "/regions/" + run.Region + "/subnetworks"
			code, op := facade(t, "POST", p, map[string]any{"name": x.id + "-from-root", "network": "default",
				"ipCidrRange": netip.PrefixFrom(addrOf(x.block, 32), 28).String()})
			if code != 200 {
				t.Errorf("the root writing %s's facade: %d %v", x.path, code, op)
				continue
			}
			waitOp(t, x.project, op)
			_, op = facade(t, "DELETE", p+"/"+x.id+"-from-root", nil)
			waitOp(t, x.project, op)
			if _, err := run.API.Patch(ctx, x.path, client.Patch{"quota": client.QuotaMap{run.Region: {"vcpu": num(3)}}}, client.Mutation{}); err != nil {
				t.Errorf("the root changing %s's allocation: %v", x.path, err)
			}
		}
	})
}

// addrOf is block's n'th address.
func addrOf(block netip.Prefix, n int) netip.Addr {
	a := block.Masked().Addr()
	for i := 0; i < n; i++ {
		a = a.Next()
	}
	return a
}

// apiCode is the fence API's error code in err, "" if none.
func apiCode(err error) string {
	var e *client.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// TestControlInheritance: a child can't be looser than its parent, and
// sealing an ancestor seals the subtree until it is unsealed.
func TestControlInheritance(t *testing.T) {
	tree(t)
	ctx := context.Background()
	t.Run("LooserThanParent", func(t *testing.T) {
		mkSilo(t, "/ri", 27, "read", nil)
		_, err := run.API.Create(ctx, "/ri/c", client.CreateRequest{Control: "full", Address: &client.AddressWant{PrefixLength: 29}}, client.Mutation{})
		if apiCode(err) != "LOOSER_THAN_PARENT" {
			t.Errorf("creating a full child of a read silo: %v", err)
			run.API.Delete(ctx, "/ri/c", true, client.Mutation{})
		}
		if _, err := run.API.Create(ctx, "/ri/k", client.CreateRequest{Control: "read", Address: &client.AddressWant{PrefixLength: 29}}, client.Mutation{}); err != nil {
			t.Fatalf("a read child of a read silo: %v", err)
		}
		defer run.API.Delete(ctx, "/ri/k", true, client.Mutation{})
		if _, err := run.API.Patch(ctx, "/ri/k", client.Patch{"control": "full"}, client.Mutation{}); apiCode(err) != "LOOSER_THAN_PARENT" {
			t.Errorf("PATCHing a read child of a read silo to full: %v", err)
		}
	})
	t.Run("SealingAncestorSealsChild", func(t *testing.T) {
		v := startProbe(t, c, "probe-inherit", "", []Check{settle,
			{ID: "before", Kind: "tcp", Host: "silo.internal", Port: 8080}, mark,
			{ID: "held", Kind: "sh", Script: `s=$(date +%s); (sleep 200) | timeout 210 nc silo.internal 8080 >/dev/null; e=$(( $(date +%s)-s )); echo "held ${e}s"; [ $e -lt 120 ]`},
			{ID: "after", Kind: "tcp", Host: "silo.internal", Port: 8080}})
		if _, ok := v.until("mark", 10*time.Minute); !ok {
			t.Fatal("the probe never got to its mark")
		}
		time.Sleep(20 * time.Second)
		if _, err := run.API.Patch(ctx, "/a", client.Patch{"control": "none"}, client.Mutation{}); err != nil {
			t.Fatalf("sealing /a: %v", err)
		}
		unsealed := false
		unseal := func() {
			if !unsealed {
				unsealed = true
				run.API.Patch(ctx, "/a", client.Patch{"control": "full"}, client.Mutation{})
			}
		}
		defer unseal()
		d, err := run.API.Describe(ctx, "/a/c", false)
		if err != nil {
			t.Fatal(err)
		}
		if d.Control.Own != "full" || d.Control.Effective != "none" || d.Control.EffectiveFrom != "/a" {
			t.Errorf("/a/c under a sealed /a: %+v", d.Control)
		}
		res := v.wait()
		expect(t, res, "before", true)
		expect(t, res, "held", true)
		expect(t, res, "after", false)
		unseal()
		if d, err = run.API.Describe(ctx, "/a/c", false); err != nil || d.Control.Effective != "full" {
			t.Errorf("/a/c after unsealing /a: %+v %v", d, err)
		}
	})
	t.Run("DescribeShowsOwnEffectiveFrom", func(t *testing.T) {
		d, err := run.API.Describe(ctx, "/a/c", false)
		if err != nil {
			t.Fatal(err)
		}
		if d.Control.Own == "" || d.Control.Effective == "" || d.Control.EffectiveFrom == "" {
			t.Errorf("describe /a/c: control %+v", d.Control)
		}
	})
}

// TestManageGrants: / offers /p a grant on /s for instances and disks,
// write, fence read, short ttl.
func TestManageGrants(t *testing.T) {
	tree(t)
	ctx := context.Background()
	s := mkSilo(t, "/ms", 26, "none", nil)
	p := mkSilo(t, "/mp", 26, "", &client.Egress{Allow: []string{"releases.hashicorp.com", "registry.terraform.io", "github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com"}})
	offer := func(t *testing.T, target, holder string) (map[string]any, error) {
		out := map[string]any{}
		err := run.API.Do(ctx, "POST", "silos/"+strings.TrimPrefix(target, "/")+"/manageGrants", nil, map[string]any{
			"name": "rollout", "holder": holder,
			"scope": map[string]any{"facade": map[string]any{"resources": []string{"instances", "disks"}, "access": "write"}, "fence": []string{"read"}},
			"lease": map[string]any{"ttl": "1800s", "renew_until": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
		}, &out)
		return out, err
	}
	g, err := offer(t, s.path, p.path)
	if err != nil {
		t.Fatalf("the offer: %v", err)
	}
	id, _ := g["id"].(string)
	defer run.API.Do(ctx, "DELETE", "silos/"+strings.TrimPrefix(s.path, "/")+"/manageGrants/rollout", nil, nil, nil)
	sp := "/compute/v1/projects/" + s.project
	sInstances := api + sp + "/zones/" + run.Zone + "/instances"
	inst := func(name string) string {
		return `{"name":"` + name + `","machineType":"zones/` + run.Zone + `/machineTypes/e2-micro","scheduling":{"provisioningModel":"SPOT","instanceTerminationAction":"DELETE"},"disks":[{"boot":true,"autoDelete":true,"initializeParams":{"sourceImage":"` + gcpharness.ProbeImage + `","diskSizeGb":"10"}}],"networkInterfaces":[{"subnetwork":"regions/` + run.Region + `/subnetworks/` + s.id + `-net"}]}`
	}
	res := probe(t, p, "probe-holder", []Check{
		{ID: "pending-get", Kind: "http", URL: api + "/silo/v1/silos/" + strings.TrimPrefix(s.path, "/")},
		{ID: "pending-facade", Kind: "http", URL: sInstances},
		{ID: "accept", Kind: "http", Method: "POST", URL: api + "/silo/v1/self/manageGrants/" + id + ":accept"},
		{ID: "insert", Kind: "http", Method: "POST", URL: sInstances, Body: inst(s.id + "-held")},
		{ID: "subnet", Kind: "http", Method: "POST", URL: api + sp + "/regions/" + run.Region + "/subnetworks", Body: `{"name":"` + s.id + `-x","network":"default","ipCidrRange":"` + netip.PrefixFrom(addrOf(s.block, 32), 28).String() + `"}`},
		{ID: "firewall", Kind: "http", Method: "POST", URL: api + sp + "/global/firewalls", Body: `{"name":"` + s.id + `-x","network":"default","allowed":[{"IPProtocol":"tcp"}]}`},
		{ID: "quota", Kind: "http", Method: "PATCH", URL: api + "/silo/v1/silos/ms", Body: `{"quota":{"` + run.Region + `":{"vcpu":"1"}}}`},
		{ID: "child", Kind: "http", Method: "POST", URL: api + "/silo/v1/silos/ms/children", Body: `{"name":"x","address":{"prefix_length":28}}`},
		{ID: "member", Kind: "http", Method: "PUT", URL: api + "/silo/v1/silos/ms/members/user:x@example.com", Body: `{}`},
		{ID: "control", Kind: "http", Method: "PATCH", URL: api + "/silo/v1/silos/ms", Body: `{"control":"full"}`},
		{ID: "reoffer", Kind: "http", Method: "POST", URL: api + "/silo/v1/silos/ms/manageGrants", Body: `{"name":"again","holder":"/b","scope":{"facade":{"resources":["instances"],"access":"read"}},"lease":{"ttl":"600s"}}`},
	})
	t.Run("BeforeAccept", func(t *testing.T) {
		expectStatus(t, res, "pending-get", 404, "NOT_FOUND")
		expectStatus(t, res, "pending-facade", 403)
	})
	t.Run("AfterAccept", func(t *testing.T) {
		expectStatus(t, res, "accept", 200)
		expectStatus(t, res, "insert", 200)
		code, _ := facade(t, "GET", "projects/"+s.project+"/zones/"+run.Zone+"/instances/"+s.id+"-held", nil)
		if code != 200 {
			t.Errorf("the holder's instance isn't /ms's: %d", code)
		}
		recs, err := run.API.Audit(ctx, s.path, client.AuditQuery{})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, r := range recs {
			if strings.Contains(fmt.Sprint(r), "manage:"+p.id) {
				found = true
			}
		}
		if !found {
			t.Errorf("no audit record of /ms's with via=manage:%s", p.id)
		}
		_, op := facade(t, "DELETE", "projects/"+s.project+"/zones/"+run.Zone+"/instances/"+s.id+"-held", nil)
		waitOp(t, s.project, op)
	})
	t.Run("OutOfScope", func(t *testing.T) {
		expectStatus(t, res, "subnet", 403, "rollout")
		expectStatus(t, res, "firewall", 403, "rollout")
		for _, id := range []string{"quota", "child", "member", "control", "reoffer"} {
			expectStatus(t, res, id, 403, "PERMISSION_DENIED")
		}
	})
	t.Run("GrantOnItselfOrFromDescendant", func(t *testing.T) {
		if _, err := offer(t, s.path, s.path); apiCode(err) != "INVALID_ARGUMENT" {
			t.Errorf("a grant on /ms to itself: %v", err)
		}
		if _, err := run.API.Create(ctx, "/ms/k", client.CreateRequest{Address: &client.AddressWant{PrefixLength: 28}, Control: "none"}, client.Mutation{}); err == nil {
			defer run.API.Delete(ctx, "/ms/k", true, client.Mutation{})
			if _, err := offer(t, s.path, "/ms/k"); apiCode(err) != "INVALID_ARGUMENT" {
				t.Errorf("a grant on /ms to its child: %v", err)
			}
		}
	})
	t.Run("RevokeStopsInFlightTerraform", func(t *testing.T) {
		tf := fmt.Sprintf(`
set -e
export HTTPS_PROXY=%s
cd /tmp && curl -sSfo tf.zip https://releases.hashicorp.com/terraform/1.9.8/terraform_1.9.8_linux_amd64.zip
python3 -c 'import zipfile; zipfile.ZipFile("tf.zip").extractall(".")' && chmod +x terraform
mkdir -p w && cd w && cat >main.tf <<'EOF'
provider "google" {
  project = "%s"
  zone = "%s"
  access_token = "unused"
  compute_custom_endpoint = "%s/compute/v1/"
}
resource "google_compute_disk" "d" {
  count = 8
  name = "tf-${count.index}"
  size = 10
  type = "pd-standard"
}
EOF
../terraform init -input=false >/dev/null
echo "SILO-PROBE mark ok"
../terraform apply -auto-approve -input=false -parallelism=1 >/tmp/tf.log 2>&1 && exit 1
grep -q 403 /tmp/tf.log`, proxy, s.project, run.Zone, api)
		v := startProbe(t, p, "probe-terraform", "", []Check{{ID: "tf", Kind: "sh", Script: tf}})
		if _, ok := v.until("mark", 10*time.Minute); !ok {
			t.Fatal("terraform never got to apply")
		}
		// Revoke once the first disk exists.
		deadline := time.Now().Add(5 * time.Minute)
		for time.Now().Before(deadline) {
			if code, _ := facade(t, "GET", "projects/"+s.project+"/zones/"+run.Zone+"/disks/tf-0", nil); code == 200 {
				break
			}
			time.Sleep(3 * time.Second)
		}
		if err := run.API.Do(ctx, "DELETE", "silos/ms/manageGrants/rollout", nil, nil, nil); err != nil {
			t.Fatalf("revoking: %v", err)
		}
		revoked := time.Now()
		res := v.wait()
		expect(t, res, "tf", true)
		_, list := facade(t, "GET", "projects/"+s.project+"/zones/"+run.Zone+"/disks", nil)
		items, _ := list["items"].([]any)
		for _, it := range items {
			m := it.(map[string]any)
			name, _ := m["name"].(string)
			ct, _ := time.Parse(time.RFC3339, fmt.Sprint(m["creationTimestamp"]))
			if strings.HasPrefix(name, "tf-") && ct.After(revoked.Add(2*time.Second)) {
				t.Errorf("%s was created after the revoke", name)
			}
			_, op := facade(t, "DELETE", "projects/"+s.project+"/zones/"+run.Zone+"/disks/"+name, nil)
			waitOp(t, s.project, op)
		}
	})
	t.Run("SealedHolder", func(t *testing.T) {
		g, err := offer(t, s.path, p.path)
		if err != nil {
			t.Fatalf("offering again: %v", err)
		}
		id, _ := g["id"].(string)
		if _, err := run.API.Patch(ctx, p.path, client.Patch{"control": "none"}, client.Mutation{}); err != nil {
			t.Fatalf("sealing /mp: %v", err)
		}
		res := probe(t, p, "probe-sealed-holder", []Check{
			{ID: "accept", Kind: "http", Method: "POST", URL: api + "/silo/v1/self/manageGrants/" + id + ":accept"},
			{ID: "list", Kind: "http", URL: sInstances}})
		refusedFromProbe(t, res, "accept")
		refusedFromProbe(t, res, "list")
	})
}

// TestFenceMatchesEvaluator: on the real tree, a sample of (source,
// destination, port) triples between probe VMs, each compared with what
// the fake's evaluator predicts from the policy GCE holds.
func TestFenceMatchesEvaluator(t *testing.T) {
	tree(t)
	// The generated part of the tree: a few children of /b with random
	// control policies, on top of /a, /a/c and /b.
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	silos := []siloInfo{a, b, c}
	for i := 0; i < 2; i++ {
		ctl := []string{"full", "read", "none"}[rng.Intn(3)]
		silos = append(silos, mkSilo(t, fmt.Sprintf("/b/g%d", i), 26, ctl, nil))
	}
	for _, s := range silos {
		allowAll(t, s)
	}
	// Three probes at fixed addresses (the VM cap is four), each serving
	// the sampled ports and probing the other two.
	rng.Shuffle(len(silos), func(i, j int) { silos[i], silos[j] = silos[j], silos[i] })
	picked := silos[:3]
	ports := []int{22, 80, 7000, 8080}
	ips := make([]string, len(picked))
	for i, s := range picked {
		ips[i] = addrIn(s, 30)
	}
	type triple struct {
		src, dst int
		port     int
		id       string
	}
	var triples []triple
	for i := range picked {
		for j := range picked {
			if i == j {
				continue
			}
			port := ports[rng.Intn(len(ports))]
			triples = append(triples, triple{i, j, port, fmt.Sprintf("t-%d-%d-%d", i, j, port)})
		}
	}
	serve := Check{ID: "serve", Kind: "sh", Script: `for p in 80 7000 8080; do (timeout 600 python3 -m http.server $p >/dev/null 2>&1 &); done; sleep 2`}
	var mu sync.Mutex
	results := map[string]ProbeResult{}
	var wg sync.WaitGroup
	for i, s := range picked {
		checks := []Check{serve, settle}
		for _, tr := range triples {
			if tr.src == i {
				checks = append(checks, Check{ID: tr.id, Kind: "tcp", Host: ips[tr.dst], Port: tr.port})
			}
		}
		checks = append(checks, Check{ID: "hold", Kind: "sh", Script: "sleep 60"})
		v := startProbe(t, s, fmt.Sprintf("probe-eval-%d", i), ips[i], checks)
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := v.wait()
			mu.Lock()
			for k, x := range r {
				results[k] = x
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	pol, err := run.Compute.NetworkFirewallPolicies.Get(run.Project, "silo-fence").Do()
	if err != nil {
		t.Fatalf("the fence policy: %v", err)
	}
	ev, err := fenceeval.New("", pol, nil)
	if err != nil {
		t.Fatalf("the evaluator: %v", err)
	}
	for _, tr := range triples {
		t.Run(tr.id, func(t *testing.T) {
			pkt := fenceeval.TCP(fenceeval.VM(ips[tr.src]), fenceeval.VM(ips[tr.dst]), tr.port)
			want := ev.Allowed(pkt)
			got, ok := results[tr.id]
			if !ok {
				t.Fatalf("%s: no result", pkt)
			}
			// sshd listens on 22; the others are served above. A port
			// nothing listens on can't tell "allowed" from "dropped".
			if got.OK != want {
				v, _ := ev.Evaluate(pkt)
				t.Errorf("%s (%s → %s): real %v, evaluator %v: %s", pkt, picked[tr.src].path, picked[tr.dst].path, got.OK, want, v)
			}
		})
	}
}
