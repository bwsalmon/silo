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
