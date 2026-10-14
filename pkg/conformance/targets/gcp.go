//go:build silo_conformance

package targets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/client"
	"github.com/bwsalmon/silo/pkg/conformance"
	"github.com/bwsalmon/silo/pkg/conformance/gcpharness"
)

// GCPHarness is whether this build has the GCP harness behind the "gcp"
// target's Fence: it has with the silo_conformance tag.
const GCPHarness = true

// The "gcp" target's Fence is the GCP harness's install
// (pkg/conformance/gcpharness), which a test binary has only if its
// TestMain is gcpharness.Main. A probe is an e2-micro VM whose startup
// script serves TCP and UDP on every port and then polls its own
// instance metadata for checks to run, each round's being a ProbeScript
// the target sets with setMetadata; it reports them on the serial
// console, which the target reads through the facade. So a probe's Run
// with a new checklist is another round on the same VM, and probes
// count against MaxVMs for as long as they live.
//
// A silo made through the Fence's API is given what a probe needs (the
// home region, a quota for a few e2-micros, a "net" subnet, and tenant
// firewall rules that allow everything, so that only the fence can stop
// a packet), and is deleted with its subtree when the test ends, since
// every test makes its own /a, /b and /a/c in the one install.
func (GCP) Fence(t testing.TB) (conformance.Fence, error) {
	run := gcpharness.Run
	switch {
	case !gcpharness.Started:
		return nil, &conformance.NotRunnable{Why: "this test binary's TestMain doesn't start the GCP harness (gcpharness.Main)"}
	case run.InstallErr != nil:
		return nil, &conformance.NotRunnable{Why: fmt.Sprintf("silo install failed, so there is no tree to test: %v", run.InstallErr)}
	}
	root, err := run.API.Describe(context.Background(), "/", false)
	if err != nil {
		return nil, fmt.Errorf("describing the root: %v", err)
	}
	return &gcpFence{t: t, root: root.ID, silos: map[string]*gcpSilo{}}, nil
}

type gcpFence struct {
	t    testing.TB
	root string

	mu    sync.Mutex
	silos map[string]*gcpSilo // by id
}

// gcpSilo is a silo the Fence made, with its probes.
type gcpSilo struct {
	id, path, project string
	probes            []*gcpProbe
}

func (f *gcpFence) Root() string { return f.root }

func (f *gcpFence) Gateway() string { return "silo.internal" }

func (f *gcpFence) Docker(string, *http.Request) (*http.Response, error) {
	return nil, conformance.ErrNotOffered
}

// API calls the fence API as the root's members through the harness's
// front-door proxy, and as any other silo from a probe in it, which the
// gateway names by its address alone.
func (f *gcpFence) API(caller, method, path string, body any) (int, map[string]any) {
	f.t.Helper()
	switch caller {
	case "":
		return 0, map[string]any{"error": "not offered: on GCP, a call from no silo is a call from outside the tree, which can't reach the gateway"}
	case f.root:
		return f.rootAPI(method, path, body)
	}
	return f.siloAPI(caller, method, path, body)
}

// siloQuota is what a silo made through the Fence's API is given, in
// place of the local backend's quota the cases ask for: room for a few
// probes at the top, one or two in a child, out of its parent's.
func siloQuota(nested bool) client.QuotaMap {
	n := func(v int) json.Number { return json.Number(fmt.Sprint(v)) }
	if nested {
		return client.QuotaMap{gcpharness.Run.Region: {
			"vcpu": n(1), "spot_vcpu": n(1), "memory_mb": n(2048), "spot_memory_mb": n(2048),
			"instances": n(2), "standard_disk_gb": n(20), "ssd_disk_gb": n(5),
		}, "global": {"subnetworks": n(1)}}
	}
	return client.QuotaMap{gcpharness.Run.Region: {
		"vcpu": n(4), "spot_vcpu": n(4), "memory_mb": n(8192), "spot_memory_mb": n(8192),
		"instances": n(4), "standard_disk_gb": n(60), "ssd_disk_gb": n(10),
	}, "global": {"subnetworks": n(3)}}
}

func (f *gcpFence) rootAPI(method, path string, body any) (int, map[string]any) {
	f.t.Helper()
	run := gcpharness.Run
	create := method == "POST" && strings.HasSuffix(path, "/children")
	if m, ok := body.(map[string]any); ok && create {
		c := map[string]any{}
		for k, v := range m {
			c[k] = v
		}
		c["regions"] = []string{run.Region}
		c["quota"] = siloQuota(!strings.HasPrefix(path, "/silo/v1/silos/-"))
		body = c
	}
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://"+run.ProxyAddr+path, r)
	req.Header.Set("Content-Type", "application/json")
	res, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := decode(raw)
	if create && res.StatusCode == 200 {
		f.made(out)
	}
	return res.StatusCode, out
}

func decode(raw []byte) map[string]any {
	out := map[string]any{}
	json.Unmarshal(raw, &out)
	if len(out) == 0 && len(raw) > 0 {
		out["raw"] = string(raw)
	}
	return out
}

// made gives a silo the root just made its net subnet and its allow-all
// tenant rules, and deletes it, with its subtree, when the test ends.
func (f *gcpFence) made(out map[string]any) {
	f.t.Helper()
	id, _ := out["id"].(string)
	path, _ := out["path"].(string)
	if id == "" || path == "" {
		f.t.Fatalf("a made silo with no id or path: %v", out)
	}
	s := &gcpSilo{id: id, path: path, project: "silo-" + id}
	f.mu.Lock()
	f.silos[id] = s
	f.mu.Unlock()
	f.t.Cleanup(func() {
		if err := gcpharness.DeleteSilo(context.Background(), path); err != nil {
			f.t.Errorf("deleting %s: %v", path, err)
		}
	})
	addr, _ := out["address"].(map[string]any)
	blocks, _ := addr["blocks"].([]any)
	if len(blocks) == 0 {
		f.t.Fatalf("%s has no block: %v", path, out)
	}
	block, err := netip.ParsePrefix(fmt.Sprint(blocks[0]))
	if err != nil {
		f.t.Fatalf("%s's block: %v", path, err)
	}
	// The cases' probes all go in the net subnet: the block's first /24,
	// or its first half if it is a /24 or smaller.
	subnet := netip.PrefixFrom(block.Masked().Addr(), max(24, block.Bits()+1))
	f.facadeOK(s.project, "POST", "projects/"+s.project+"/regions/"+gcpharness.Run.Region+"/subnetworks",
		map[string]any{"name": id + "-net", "network": "default", "ipCidrRange": subnet.String()})
	for _, dir := range []string{"INGRESS", "EGRESS"} {
		body := map[string]any{"name": id + "-allow-all-" + strings.ToLower(dir), "network": "default", "direction": dir,
			"priority": 100, "allowed": []any{map[string]any{"IPProtocol": "all"}}}
		if dir == "INGRESS" {
			body["sourceRanges"] = []string{"0.0.0.0/0"}
		} else {
			body["destinationRanges"] = []string{"0.0.0.0/0"}
		}
		f.facadeOK(s.project, "POST", "projects/"+s.project+"/global/firewalls", body)
	}
}

// facadeOK makes a facade call that must start an operation, and waits
// for it.
func (f *gcpFence) facadeOK(project, method, path string, body any) map[string]any {
	f.t.Helper()
	code, op := gcpharness.Facade(f.t, method, path, body)
	if code != 200 {
		f.t.Fatalf("%s %s: %d %v", method, path, code, op)
	}
	return gcpharness.WaitOp(f.t, project, op)
}

// siloAPI makes the call from a probe in caller: one the case made
// there if there is one, or one made for it.
func (f *gcpFence) siloAPI(caller, method, path string, body any) (int, map[string]any) {
	f.t.Helper()
	f.mu.Lock()
	s := f.silos[caller]
	var p *gcpProbe
	if s != nil && len(s.probes) > 0 {
		p = s.probes[0]
	}
	f.mu.Unlock()
	if s == nil {
		f.t.Fatalf("calling as %s: not a silo this Fence made", caller)
	}
	if p == nil {
		p = f.Probe(f.t, caller, "caller").(*gcpProbe)
	}
	script := "curl -sS -m 30 -o /tmp/api -w '%{http_code}' -X " + shellWord(method) +
		" -H 'Content-Type: application/json' http://silo.internal:8080" + shellWord(path)
	if body != nil {
		b, _ := json.Marshal(body)
		script += " --data-binary " + shellWord(string(b))
	}
	// The answer may be longer than a report line's detail carries, so
	// it goes to the console whole, base64 on a line of its own.
	script = "code=$(" + script + "); echo \"" + apiPrefix + "$code $(base64 -w0 /tmp/api)\" >/dev/ttyS0; [ -n \"$code\" ]"
	round, res := p.run([]conformance.Check{{ID: "api", Kind: "sh", Script: script}})
	if r := res["api"]; !r.OK {
		f.t.Fatalf("%s %s as %s: the probe's call failed: %s", method, path, s.path, r.Detail)
	}
	for _, line := range strings.Split(p.serial(), "\n") {
		i := strings.Index(line, apiPrefix)
		if i < 0 || !strings.HasPrefix(line[i+len(apiPrefix):], round+" ") {
			continue
		}
		fields := strings.Fields(line[i+len(apiPrefix)+len(round):])
		code := 0
		if len(fields) > 0 {
			fmt.Sscanf(fields[0], "%d", &code)
		}
		var raw []byte
		if len(fields) > 1 {
			raw, _ = base64.StdEncoding.DecodeString(fields[1])
		}
		return code, decode(raw)
	}
	f.t.Fatalf("%s %s as %s: the probe reported no answer", method, path, s.path)
	return 0, nil
}

// apiPrefix starts the console line a probe's fence API call answers
// on: "SILO-API <round> <status> <base64 body>".
const apiPrefix = "SILO-API "

func shellWord(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// gcpProbe is a probe VM.
type gcpProbe struct {
	t       testing.TB
	s       *gcpSilo
	name    string
	addr    netip.Addr
	rounds  int
	mu      sync.Mutex
	deleted bool
}

// probeAgent is a probe VM's startup script: serve TCP (HTTP) on 7000
// and UDP (echo) on 7001, with every other port but SSH's and DHCP's
// redirected to them, so that a check of any port is decided by the
// fence alone; report "ready"; then run each round of checks set in the
// instance's silo-checks metadata as silo-round changes.
const probeAgent = `#!/bin/bash
exec >/dev/ttyS0 2>&1
(python3 -m http.server 7000 >/dev/null 2>&1 &)
(python3 -c '
import socket
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind(("0.0.0.0",7001))
while True:
    d,a=s.recvfrom(2048); s.sendto(d,a)' >/dev/null 2>&1 &)
if command -v nft >/dev/null; then
  nft -f - <<'EOF'
table ip probe {
  chain pre {
    type nat hook prerouting priority -100;
    tcp dport != { 22, 7000 } redirect to :7000
    udp dport != { 68, 7001 } redirect to :7001
  }
}
EOF
elif command -v iptables >/dev/null; then
  iptables -t nat -A PREROUTING -p tcp -m multiport ! --dports 22,7000 -j REDIRECT --to-ports 7000
  iptables -t nat -A PREROUTING -p udp -m multiport ! --dports 68,7001 -j REDIRECT --to-ports 7001
else
  echo "SILO-PROBE every-port fail neither nft nor iptables: only 7000/tcp and 7001/udp answer"
fi
for i in $(seq 20); do (exec 3<>/dev/tcp/127.0.0.1/7000) 2>/dev/null && break; sleep 0.5; done
echo "SILO-PROBE ready ok"
md=http://metadata.google.internal/computeMetadata/v1/instance/attributes
last=
while true; do
  r=$(curl -sf -H Metadata-Flavor:Google "$md/silo-round" 2>/dev/null)
  if [ -n "$r" ] && [ "$r" != "$last" ]; then
    last=$r
    curl -sf -H Metadata-Flavor:Google "$md/silo-checks" | base64 -d >/tmp/round.sh && bash /tmp/round.sh
  fi
  sleep 3
done
`

func (f *gcpFence) Probe(t testing.TB, silo, name string) conformance.Probe {
	t.Helper()
	run := gcpharness.Run
	f.mu.Lock()
	s := f.silos[silo]
	f.mu.Unlock()
	if s == nil {
		t.Fatalf("a probe in %s: not a silo this Fence made", silo)
	}
	select {
	case run.VMs <- struct{}{}:
	case <-time.After(20 * time.Minute):
		t.Fatalf("a probe in %s: still %d probe VMs after 20 minutes (MaxVMs)", s.path, conformance.MaxVMs)
	}
	p := &gcpProbe{t: t, s: s, name: s.id + "-" + name}
	body := gcpharness.InstanceBody(p.name, s.id, func(m map[string]any) {
		m["metadata"] = map[string]any{"items": []any{map[string]any{"key": "startup-script", "value": probeAgent}}}
	})
	code, op := gcpharness.Facade(t, "POST", p.instances(), body)
	if code != 200 {
		<-run.VMs
		t.Fatalf("probe %s in %s: %d %v", p.name, s.path, code, op)
	}
	t.Cleanup(p.delete)
	gcpharness.WaitOp(t, s.project, op)
	_, inst := gcpharness.Facade(t, "GET", p.instances()+"/"+p.name, nil)
	nics, _ := inst["networkInterfaces"].([]any)
	if len(nics) > 0 {
		ip, _ := nics[0].(map[string]any)["networkIP"].(string)
		p.addr, _ = netip.ParseAddr(ip)
	}
	if !p.addr.IsValid() {
		t.Fatalf("probe %s in %s has no address: %v", p.name, s.path, inst)
	}
	deadline := time.Now().Add(10 * time.Minute)
	for {
		if res, _ := conformance.ParseProbeReport(p.serial()); res["ready"].OK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe %s in %s never got ready", p.name, s.path)
		}
		time.Sleep(10 * time.Second)
	}
	f.mu.Lock()
	s.probes = append(s.probes, p)
	f.mu.Unlock()
	return p
}

func (p *gcpProbe) instances() string {
	return "projects/" + p.s.project + "/zones/" + gcpharness.Run.Zone + "/instances"
}

func (p *gcpProbe) Addr() netip.Addr { return p.addr }

func (p *gcpProbe) serial() string {
	_, out := gcpharness.Facade(p.t, "GET", p.instances()+"/"+p.name+"/serialPort?port=1", nil)
	text, _ := out["contents"].(string)
	return text
}

func (p *gcpProbe) Run(checks []conformance.Check) map[string]conformance.ProbeResult {
	p.t.Helper()
	_, res := p.run(checks)
	return res
}

// run runs checks as the probe's next round, and returns the round's
// name and its report. Each check's id is the round's name, a dot and
// the case's id, and a last check marks the round done, so that earlier
// rounds' lines on the console are told apart.
func (p *gcpProbe) run(checks []conformance.Check) (string, map[string]conformance.ProbeResult) {
	p.t.Helper()
	p.mu.Lock()
	p.rounds++
	round := fmt.Sprintf("r%d", p.rounds)
	p.mu.Unlock()
	var cs []conformance.Check
	for _, c := range checks {
		c.ID = round + "." + c.ID
		if c.Kind == "sh" {
			c.Script = strings.ReplaceAll(c.Script, apiPrefix, apiPrefix+round+" ")
		}
		cs = append(cs, c)
	}
	cs = append(cs, conformance.Check{ID: round + ".done", Kind: "sh", Script: "true"})
	p.setMetadata(map[string]string{
		"silo-checks": base64.StdEncoding.EncodeToString([]byte(conformance.ProbeScript(cs))),
		"silo-round":  round,
	})
	deadline := time.Now().Add(12 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Second)
		res, _ := conformance.ParseProbeReport(p.serial())
		if _, ok := res[round+".done"]; !ok {
			continue
		}
		out := map[string]conformance.ProbeResult{}
		for id, r := range res {
			if k, ok := strings.CutPrefix(id, round+"."); ok && k != "done" {
				out[k] = r
			}
		}
		return round, out
	}
	p.t.Fatalf("probe %s in %s never reported round %s", p.name, p.s.path, round)
	return "", nil
}

// setMetadata sets keys in the probe's instance metadata, keeping the
// rest (the startup script, and the silo-name-prefix the facade sets).
func (p *gcpProbe) setMetadata(keys map[string]string) {
	p.t.Helper()
	_, inst := gcpharness.Facade(p.t, "GET", p.instances()+"/"+p.name, nil)
	md, _ := inst["metadata"].(map[string]any)
	items, _ := md["items"].([]any)
	var out []any
	for _, it := range items {
		m, _ := it.(map[string]any)
		if _, ours := keys[fmt.Sprint(m["key"])]; !ours {
			out = append(out, it)
		}
	}
	for k, v := range keys {
		out = append(out, map[string]any{"key": k, "value": v})
	}
	code, op := gcpharness.Facade(p.t, "POST", p.instances()+"/"+p.name+"/setMetadata",
		map[string]any{"fingerprint": md["fingerprint"], "items": out})
	if code != 200 {
		p.t.Fatalf("setting probe %s's checks: %d %v", p.name, code, op)
	}
	gcpharness.WaitOp(p.t, p.s.project, op)
}

// Spoof isn't offered: GCE drops a packet whose source isn't its NIC's
// (gcp-backend.md, "What the fence does not cover"), and a sender's
// report can't show whether a spoofed packet arrived. pkg/conformance's
// TestOriginSpoofing has the GCP form, with a listening probe.
func (p *gcpProbe) Spoof(netip.Addr, []conformance.Check) (map[string]conformance.ProbeResult, error) {
	return nil, conformance.ErrNotOffered
}

func (p *gcpProbe) delete() {
	p.mu.Lock()
	if p.deleted {
		p.mu.Unlock()
		return
	}
	p.deleted = true
	p.mu.Unlock()
	_, op := gcpharness.Facade(p.t, "DELETE", p.instances()+"/"+p.name, nil)
	gcpharness.WaitOp(p.t, p.s.project, op)
	<-gcpharness.Run.VMs
}
