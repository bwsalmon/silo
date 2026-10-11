// Package conformance is the silo/v1 conformance suite: the same cases
// for every backend, written against the fence API and the facade, with
// backend-specific helpers to make a probe VM and send a packet. v1 has
// only the GCP helpers (docs/silo/testing.md, "Conformance suite",
// SV-121, SV-122).
//
// The suite itself is behind the silo_conformance build tag, so the
// ordinary go test ./... never runs it: a run installs Silo into a real
// project, creates VMs and takes tens of minutes. Run it with
//
//	go test -tags silo_conformance -timeout 150m -v ./pkg/silo/conformance/
//
// with the gcp-key capability's key at /home/debian/.gcp-service-account.json
// (or SILO_CONFORMANCE_KEY). .grain/workflows/silo-conformance.yml runs it
// nightly and by hand.
//
// This file holds the parts of the harness that need no cloud, so that
// they are tested on every push: choosing a fresh address space, the
// probe VMs' script and its report, the leftover check, and the
// discovery-document drift list.
package conformance

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// DefaultProject is where the suite runs (SV-121).
const DefaultProject = "bsalmon-fun-sandbox"

// MaxVMs is the hard cap on probe VMs that exist at once (SV-121,
// "Cost"). The control plane's own VMs are not counted.
const MaxVMs = 4

// ProbeMachineType is every probe's machine type: the cheapest.
const ProbeMachineType = "e2-micro"

// PickAddressSpace returns a /16 inside 10.0.0.0/8 that overlaps none
// of used, the project's existing subnet ranges, so that install's
// address-space check passes and the run never shares addresses with
// anything else in the project. It never picks from 10.128.0.0/9,
// where GCE's auto-mode subnets are, and starts at 10.100, above the
// low ranges people pick by hand.
func PickAddressSpace(used []netip.Prefix) (netip.Prefix, error) {
	order := make([]int, 0, 256)
	for i := 100; i < 128; i++ {
		order = append(order, i)
	}
	for i := 1; i < 100; i++ {
		order = append(order, i)
	}
	for _, i := range order {
		p := netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(i), 0, 0}), 16)
		free := true
		for _, u := range used {
			if u.Overlaps(p) {
				free = false
				break
			}
		}
		if free {
			return p, nil
		}
	}
	return netip.Prefix{}, fmt.Errorf("no free /16 in 10.0.0.0/8 below 10.128.0.0")
}

// Ranges are install's --gateway-range and --control-range for space:
// its last two /24s, as install's defaults are 10.0.0.0/8's.
func Ranges(space netip.Prefix) (gateway, control netip.Prefix) {
	b := space.Masked().Addr().As4()
	// The space is a /16: its last /24 is x.y.255.0.
	gateway = netip.PrefixFrom(netip.AddrFrom4([4]byte{b[0], b[1], 255, 0}), 24)
	control = netip.PrefixFrom(netip.AddrFrom4([4]byte{b[0], b[1], 254, 0}), 24)
	return gateway, control
}

// A Check is one thing a probe VM tries: a TCP connect, a UDP send that
// expects an answer, an ICMP echo, a DNS lookup, an HTTP request, or a
// shell script of the case's own.
type Check struct {
	// ID names the check in the probe's report.
	ID    string
	Kind  string // "tcp", "udp", "icmp", "dns", "http" or "sh"
	Host  string
	Port  int
	Count int // icmp only; 0 is 2

	// http: Method (GET if ""), URL, an optional JSON Body, Proxy
	// (curl's -x) and Headers ("Name: value"). The check is ok when
	// curl got an answer at all; its detail starts with the HTTP status
	// (HTTPStatus), then the head of the body.
	Method, URL, Body, Proxy string
	Headers                  []string

	// sh: a bash script, ok when it exits 0.
	Script string
}

// Probe report lines on the serial console. A probe can't be reached
// over SSH (the fence drops everything from outside the tree, IAP's
// range included), so it runs its checks from its startup script and
// writes the results where the facade's getSerialPortOutput reads them.
const (
	reportPrefix = "SILO-PROBE "
	reportDone   = "SILO-PROBE-DONE"
)

// ProbeScript is the startup script that runs checks and reports them on
// the serial console. Each check's line is "SILO-PROBE <id> ok|fail
// <detail>", and the last is SILO-PROBE-DONE. A connect that succeeds is
// "ok"; the cases decide whether ok is what they wanted.
func ProbeScript(checks []Check) string {
	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	b.WriteString("exec >/dev/ttyS0 2>&1\n")
	b.WriteString("command -v nc >/dev/null || (apt-get update -qq && apt-get install -y -qq netcat-openbsd) >/dev/null 2>&1\n")
	b.WriteString("h() { curl -sS -m 20 -o /tmp/body -w '%{http_code} ' \"$@\" && head -c 400 /tmp/body; }\n")
	b.WriteString("r() { if \"${@:2}\" >/tmp/out 2>&1; then echo \"" + reportPrefix + "$1 ok $(head -c 200 /tmp/out | tr '\\n' ' ')\"; else echo \"" + reportPrefix + "$1 fail $(head -c 200 /tmp/out | tr '\\n' ' ')\"; fi; }\n")
	for _, c := range checks {
		switch c.Kind {
		case "tcp":
			fmt.Fprintf(&b, "r %s timeout 8 nc -z -w 5 %s %d\n", shellWord(c.ID), shellWord(c.Host), c.Port)
		case "udp":
			// A UDP "connect" proves nothing on its own: ask for a reply.
			fmt.Fprintf(&b, "r %s bash -c 'echo probe | timeout 6 nc -u -w 3 %s %d | grep -q .'\n", shellWord(c.ID), c.Host, c.Port)
		case "icmp":
			n := c.Count
			if n == 0 {
				n = 2
			}
			fmt.Fprintf(&b, "r %s ping -c %d -W 3 %s\n", shellWord(c.ID), n, shellWord(c.Host))
		case "dns":
			fmt.Fprintf(&b, "r %s getent hosts %s\n", shellWord(c.ID), shellWord(c.Host))
		case "http":
			m := c.Method
			if m == "" {
				m = "GET"
			}
			fmt.Fprintf(&b, "r %s h -X %s", shellWord(c.ID), shellWord(m))
			if c.Proxy != "" {
				fmt.Fprintf(&b, " -x %s", shellWord(c.Proxy))
			}
			for _, h := range c.Headers {
				fmt.Fprintf(&b, " -H %s", shellWord(h))
			}
			if c.Body != "" {
				fmt.Fprintf(&b, " -H 'Content-Type: application/json' --data-binary %s", shellWord(c.Body))
			}
			fmt.Fprintf(&b, " %s\n", shellWord(c.URL))
		case "sh":
			fmt.Fprintf(&b, "r %s bash -c %s\n", shellWord(c.ID), shellWord(c.Script))
		}
	}
	b.WriteString("echo " + reportDone + "\n")
	return b.String()
}

// ExporterServers is a script for an exporter's probe that serves, for
// d seconds in the background:
//
//   - HTTP on 7000, answering every GET with the path it was asked for
//     and the Silo-* headers it got, one per line;
//   - TCP on 7003, which reads a PROXY v2 header if the connection
//     starts with one and answers "pp2 src=<address>", or answers
//     "noproxy src=<peer>" if not, then holds the connection open,
//     sending a byte every 100ms, until the other end goes.
//
// It also serves plain HTTP on 7002, a port the link cases' links
// don't name.
func ExporterServers(d int) Check {
	return Check{ID: "serve", Kind: "sh", Script: fmt.Sprintf(`
cat >/tmp/web.py <<'EOF'
import http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        b = ("path=%%s\n" %% self.path + "".join("%%s: %%s\n" %% (k, v) for k, v in self.headers.items() if k.lower().startswith("silo"))).encode()
        self.send_response(200); self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def log_message(self, *a): pass
http.server.ThreadingHTTPServer(("0.0.0.0", 7000), H).serve_forever()
EOF
cat >/tmp/tcp.py <<'EOF'
import socket, struct, threading, time
SIG = b"\r\n\r\n\x00\r\nQUIT\n"
def recv(c, n):
    d = b""
    while len(d) < n:
        x = c.recv(n - len(d))
        if not x: raise EOFError
        d += x
    return d
def serve(c, peer):
    try:
        c.settimeout(2)
        try:
            head = recv(c, 16)
        except socket.timeout:
            head = b""
        if head[:12] == SIG:
            rest = recv(c, struct.unpack("!H", head[14:16])[0])
            src = socket.inet_ntoa(rest[0:4]) if head[13] == 0x11 else "family-%%d" %% head[13]
            c.sendall(("pp2 src=%%s\n" %% src).encode())
        else:
            c.sendall(("noproxy src=%%s\n" %% peer[0]).encode())
        c.settimeout(None)
        while True:
            c.sendall(b"."); time.sleep(0.1)
    except Exception:
        pass
    finally:
        c.close()
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("0.0.0.0", 7003)); s.listen(16)
while True:
    c, peer = s.accept()
    threading.Thread(target=serve, args=(c, peer), daemon=True).start()
EOF
(timeout %[1]d python3 /tmp/web.py >/dev/null 2>&1 &)
(timeout %[1]d python3 /tmp/tcp.py >/dev/null 2>&1 &)
(timeout %[1]d python3 -m http.server 7002 >/dev/null 2>&1 &)
for i in $(seq 20); do (exec 3<>/dev/tcp/127.0.0.1/7000) 2>/dev/null && (exec 3<>/dev/tcp/127.0.0.1/7003) 2>/dev/null && exit 0; sleep 0.5; done; exit 1`, d)}
}

// TCPHello is a check that connects to host:port and reports the first
// line the far end sends.
func TCPHello(id, host string, port int) Check {
	return Check{ID: id, Kind: "sh", Script: fmt.Sprintf(`python3 -c '
import socket
s = socket.create_connection((%q, %d), 8); s.settimeout(8)
d = b""
while b"\n" not in d:
    x = s.recv(64)
    if not x: break
    d += x
print(d.split(b"\n")[0].decode())
assert b"\n" in d'`, host, port)}
}

// shellWord quotes s for the probe's shell.
func shellWord(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// ProbeResult is one check's outcome as the probe reported it.
type ProbeResult struct {
	OK     bool
	Detail string
}

// ParseProbeReport reads a probe's serial output: the results by check
// id, and whether the probe got to the end.
func ParseProbeReport(serial string) (map[string]ProbeResult, bool) {
	out := map[string]ProbeResult{}
	done := false
	for _, line := range strings.Split(serial, "\n") {
		line = strings.TrimRight(line, "\r")
		// The console prefixes lines with the startup script's own tag.
		i := strings.Index(line, reportPrefix)
		if strings.Contains(line, reportDone) {
			done = true
			continue
		}
		if i < 0 {
			continue
		}
		f := strings.SplitN(line[i+len(reportPrefix):], " ", 3)
		if len(f) < 2 {
			continue
		}
		r := ProbeResult{OK: f[1] == "ok"}
		if len(f) == 3 {
			r.Detail = strings.TrimSpace(f[2])
		}
		out[f[0]] = r
	}
	return out, done
}

// HTTPStatus is the HTTP status an "http" check's detail starts with, 0
// if it has none (curl got no answer).
func HTTPStatus(r ProbeResult) int {
	var code int
	if !r.OK {
		return 0
	}
	fmt.Sscanf(r.Detail, "%d", &code)
	return code
}

// Object is a cloud object the leftover check found.
type Object struct {
	Kind        string
	Name        string
	Labels      map[string]string
	Description string
}

func (o Object) String() string { return o.Kind + " " + o.Name }

// Leftovers are the objects that a finished uninstall should have
// removed: anything labelled silo-install (install's own), anything
// labelled silo or silo-id (a tenant's, made through the facade), and,
// where GCE has no labels, a description starting "silo-install:".
func Leftovers(objs []Object) []Object {
	var out []Object
	for _, o := range objs {
		_, inst := o.Labels["silo-install"]
		_, tenant := o.Labels["silo"]
		_, tenantID := o.Labels["silo-id"]
		if inst || tenant || tenantID || strings.HasPrefix(o.Description, "silo-install:") {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// DriftFields are the fields of a compute/v1 discovery document's schema
// that a caller can set (not "[Output Only]") and that known doesn't
// list: fields GCE takes and the facade was never taught. Default-deny
// says the facade refuses each of them (SV-63), which is how the suite
// checks that API drift is denied rather than passed through.
func DriftFields(doc []byte, schema string, known map[string]bool) ([]string, error) {
	var d struct {
		Schemas map[string]struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		} `json:"schemas"`
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		return nil, fmt.Errorf("reading the discovery document: %v", err)
	}
	s, ok := d.Schemas[schema]
	if !ok {
		return nil, fmt.Errorf("the discovery document has no schema %s", schema)
	}
	var out []string
	for name, p := range s.Properties {
		if known[name] || strings.Contains(p.Description, "[Output Only]") {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// FacadeInstanceFields are the Instance fields the facade covers
// (docs/silo/gcp-backend.md, "Covered" and "Instances") or refuses by
// name ("Refused"): everything else on Instance is drift.
var FacadeInstanceFields = map[string]bool{
	"name": true, "description": true, "machineType": true, "disks": true,
	"networkInterfaces": true, "metadata": true, "labels": true, "tags": true,
	"scheduling": true, "guestAccelerators": true, "serviceAccounts": true,
	"canIpForward": true, "minCpuPlatform": true, "shieldedInstanceConfig": true,
	"reservationAffinity": true, "deletionProtection": true, "hostname": true,
	"advancedMachineFeatures": true, "confidentialInstanceConfig": true,
	"displayDevice": true, "labelFingerprint": true, "resourcePolicies": true,
	"sourceMachineImage": true, "sourceMachineImageEncryptionKey": true,
	"params": true, "networkPerformanceConfig": true, "zone": true,
	"keyRevocationActionType": true, "instanceEncryptionKey": true,
	"shieldedVmConfig": true, "privateIpv6GoogleAccess": true,
}

// SessionEvent is one event of an agent's session, as GET
// .../agents/{agent}/session shows it (agents.md, §8).
type SessionEvent struct {
	Seq  int64           `json:"seq"`
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// ToolOutcome is one of a session's tool calls and, once it has one,
// its result.
type ToolOutcome struct {
	ID        string
	Tool      string
	Arguments map[string]any
	Done      bool // a tool_result came
	IsError   bool
	Content   string
}

// ToolOutcomes pairs a session's tool_call events with their
// tool_result events by id, in the order the calls were made. A result
// with no call before it is dropped.
func ToolOutcomes(events []SessionEvent) []ToolOutcome {
	var out []ToolOutcome
	at := map[string]int{}
	for _, e := range events {
		var d struct {
			ID        string         `json:"id"`
			Tool      string         `json:"tool"`
			Arguments map[string]any `json:"arguments"`
			IsError   bool           `json:"is_error"`
			Content   string         `json:"content"`
		}
		if json.Unmarshal(e.Data, &d) != nil {
			continue
		}
		switch e.Kind {
		case "tool_call":
			at[d.ID] = len(out)
			out = append(out, ToolOutcome{ID: d.ID, Tool: d.Tool, Arguments: d.Arguments})
		case "tool_result":
			if i, ok := at[d.ID]; ok {
				out[i].Done, out[i].IsError, out[i].Content = true, d.IsError, d.Content
			}
		}
	}
	return out
}

// CountEvents is how many of events are of kind.
func CountEvents(events []SessionEvent, kind string) int {
	n := 0
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}
