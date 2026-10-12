package gce

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	gceapi "github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce/fake"
	"github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce/fake/fenceeval"
	"github.com/bwsalmon/silo/pkg/silo/fence"
	"github.com/bwsalmon/silo/pkg/silo/ledger/address"
	"github.com/bwsalmon/silo/pkg/silo/store"
	compute "google.golang.org/api/compute/v1"
)

// vpc makes the shared VPC silo in the fake host project, as install
// does.
func (e *env) vpc() {
	e.t.Helper()
	if _, err := e.gce.InsertNetwork(ctx, host, &compute.Network{Name: "silo",
		NetworkFirewallPolicyEnforcementOrder: fenceeval.BeforeClassic, RoutingConfig: &compute.NetworkRoutingConfig{RoutingMode: "GLOBAL"}}); err != nil {
		e.t.Fatal(err)
	}
	e.gce.RunOperations()
}

func (e *env) blocks(silo string) []netip.Prefix {
	e.t.Helper()
	var out []netip.Prefix
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		var err error
		out, err = address.Blocks(ctx, tx, silo)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// own is silo's own ranges in its space map, by object.
func (e *env) own(silo string) map[string]string {
	e.t.Helper()
	out := map[string]string{}
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		m, err := address.Map(ctx, tx, silo)
		for _, r := range m {
			if r.State == address.StateOwn {
				out[r.Holder] = r.Range.String()
			}
		}
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// usage is silo's usage of a global count, by its GCE metric.
func (e *env) usage(silo, metric string) float64 {
	e.t.Helper()
	_, u, _ := quotaOf(e.ok(silo, "GET", cv("silo-"+silo, ""), nil), metric)
	return u
}

// eventually waits for cond, which a watcher makes true.
func (e *env) eventually(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// sub is the i-th /bits inside p.
func sub(p netip.Prefix, bits, i int) netip.Prefix {
	a := p.Masked().Addr().As4()
	v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	v += uint32(i) << (32 - bits)
	return netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), bits)
}

// freeIn is the first /bits of block that overlaps none of taken.
func freeIn(block netip.Prefix, bits int, taken ...netip.Prefix) netip.Prefix {
	for i := 0; ; i++ {
		p := sub(block, bits, i)
		ok := true
		for _, t := range taken {
			ok = ok && !t.Overlaps(p)
		}
		if ok {
			return p
		}
	}
}

func TestSubnetworks(t *testing.T) {
	e := newEnv(t)
	e.vpc()
	aBlock, ciBlock, sealedBlock, bBlock := e.blocks(e.a)[0], e.blocks(e.ci)[0], e.blocks(e.sealed)[0], e.blocks(e.b)[0]
	// web's /24 and the /23 it expands to lie clear of team-a's children.
	wide := freeIn(aBlock, 23, ciBlock, sealedBlock)
	web := sub(wide, 24, 0)
	pods := freeIn(aBlock, 24, ciBlock, sealedBlock, wide)
	coll := "regions/us-central1/subnetworks"
	insert := func(body obj) obj { return e.ok(e.a, "POST", cv(host, coll), body) }

	// Every check the facade makes (SV-44), before anything is charged.
	e.fail(403, "regionNotAllowed", e.a, "POST", cv(host, "regions/us-east1/subnetworks"), obj{"name": e.a + "-x", "ipCidrRange": web.String()})
	e.fail(400, "invalid", e.a, "POST", cv(host, coll), obj{"name": e.a + "-x", "ipCidrRange": "not-a-range"})
	e.fail(400, "invalid", e.a, "POST", cv(host, coll), obj{"name": e.a + "-x", "ipCidrRange": sub(web, 30, 0).String()})
	e.fail(400, "invalid", e.a, "POST", cv(host, coll), obj{"name": e.a + "-x", "ipCidrRange": "fd00::/64"})
	if msg := e.fail(400, "invalid", e.a, "POST", cv(host, coll), obj{"name": e.a + "-x", "ipCidrRange": sub(bBlock, 24, 0).String()}); !strings.Contains(msg, aBlock.String()) {
		t.Errorf("outside the blocks: %q doesn't name the silo's blocks", msg)
	}
	if msg := e.fail(400, "invalid", e.a, "POST", cv(host, coll), obj{"name": e.a + "-x", "ipCidrRange": ciBlock.String()}); !strings.Contains(msg, "delegated to /team-a/ci") || !strings.Contains(msg, aBlock.String()) {
		t.Errorf("over a delegated block: %q", msg)
	}
	e.fail(400, "invalid", e.a, "POST", cv(host, coll), obj{"name": e.a + "-x", "ipCidrRange": web.String(),
		"secondaryIpRanges": []obj{{"rangeName": "p", "ipCidrRange": sub(bBlock, 24, 0).String()}}})
	e.fail(400, "invalid", e.a, "POST", cv(host, coll), obj{"name": e.a + "-x", "ipCidrRange": web.String(),
		"secondaryIpRanges": []obj{{"rangeName": "p", "ipCidrRange": sub(web, 28, 0).String()}}})
	e.fail(400, "invalid", e.a, "POST", cv(host, coll), obj{"name": e.a + "-x", "ipCidrRange": web.String(),
		"secondaryIpRanges": []obj{{"rangeName": "p", "ipCidrRange": sealedBlock.String()}}})
	// Refusals (gcp-backend.md, "Refused").
	if msg := e.fail(403, "forbidden", e.a, "POST", cv(host, coll), obj{"name": e.a + "-x", "ipCidrRange": web.String(), "privateIpGoogleAccess": true}); msg != "silo: field privateIpGoogleAccess is not allowed" {
		t.Errorf("privateIpGoogleAccess: %q", msg)
	}
	e.fail(403, "forbidden", e.a, "POST", cv(host, coll), obj{"name": e.a + "-x", "ipCidrRange": web.String(), "stackType": "IPV4_IPV6"})
	e.fail(403, "forbidden", e.a, "POST", cv(host, coll), obj{"name": e.a + "-x", "ipCidrRange": web.String(), "purpose": "PRIVATE_SERVICE_CONNECT"})
	e.fail(404, "notFound", e.a, "POST", cv(host, coll), obj{"name": e.a + "-x", "ipCidrRange": web.String(), "network": "global/networks/other"})
	if len(e.own(e.a)) != 0 || e.usage(e.a, "SUBNETWORKS") != 0 {
		t.Fatalf("refusals left %v, usage %v", e.own(e.a), e.usage(e.a, "SUBNETWORKS"))
	}

	// Insert: the own range recorded and the subnet charged, then the
	// real <id>-web in silo, described silo:<id>.
	op := insert(obj{"name": e.a + "-web", "description": "front ends", "network": "global/networks/default", "region": "us-central1",
		"ipCidrRange": web.String(), "privateIpGoogleAccess": false, "stackType": "IPV4_ONLY", "logConfig": obj{"enable": true}})
	if op["targetLink"] != DefaultEndpoint+"projects/silo-"+e.a+"/regions/us-central1/subnetworks/"+e.a+"-web" {
		t.Errorf("operation: %v", op)
	}
	if got := e.own(e.a); got["subnetworks/us-central1/web"] != web.String() || len(got) != 1 {
		t.Errorf("own ranges after insert: %v", got)
	}
	// The space map keys it by the name less the prefix; the fence API
	// shows the subnet's one name.
	ownRanges := e.ok(e.a, "GET", "/silo/v1/self", nil)["address"].(obj)["own_ranges"].([]any)
	if len(ownRanges) != 1 || ownRanges[0].(obj)["subnet"] != "subnetworks/us-central1/"+e.a+"-web" {
		t.Errorf("own_ranges: %v", ownRanges)
	}
	if u := e.usage(e.a, "SUBNETWORKS"); u != 1 {
		t.Errorf("SUBNETWORKS used %v", u)
	}
	e.gce.RunOperations()
	real, err := e.gce.GetSubnetwork(ctx, host, "us-central1", e.a+"-web")
	if err != nil {
		t.Fatal(err)
	}
	if gceapi.LastSegment(real.Network) != "silo" || real.Description != "silo:"+e.a+" front ends" || real.PrivateIpGoogleAccess || real.IpCidrRange != web.String() {
		t.Errorf("real subnet: %+v", real)
	}
	e.fail(409, "alreadyExists", e.a, "POST", cv(host, coll), obj{"name": e.a + "-web", "ipCidrRange": pods.String()})
	if msg := e.fail(400, "invalid", e.a, "POST", cv(host, coll), obj{"name": e.a + "-web2", "ipCidrRange": sub(web, 25, 1).String()}); !strings.Contains(msg, "own range subnetworks/us-central1/web") {
		t.Errorf("over an own range: %q", msg)
	}

	// get, list and aggregatedList show the tenant's subnet on default.
	got := e.ok(e.a, "GET", cv(host, coll+"/"+e.a+"-web"), nil)
	if got["name"] != e.a+"-web" || got["description"] != "front ends" || got["network"] != DefaultEndpoint+"projects/silo-"+e.a+"/global/networks/default" ||
		got["region"] != DefaultEndpoint+"projects/silo-"+e.a+"/regions/us-central1" || got["ipCidrRange"] != web.String() || got["gatewayAddress"] == nil {
		t.Errorf("subnetworks.get: %v", got)
	}
	if l := names(items(e.ok(e.a, "GET", cv(host, coll), nil))); len(l) != 1 || l[0] != e.a+"-web" {
		t.Errorf("subnetworks.list: %v", l)
	}
	agg := e.ok(e.a, "GET", cv(host, "aggregated/subnetworks"), nil)["items"].(obj)
	if g, _ := agg["regions/us-central1"].(obj); g == nil || len(g["subnetworks"].([]any)) != 1 {
		t.Errorf("subnetworks.aggregatedList: %v", agg)
	}
	e.ok(e.root, "GET", cv("silo-"+e.a, coll+"/"+e.a+"-web"), nil)
	e.fail(404, "notFound", e.b, "GET", cv(host, coll+"/"+e.b+"-web"), nil)
	e.fail(403, "forbidden", e.b, "GET", cv("silo-"+e.a, coll+"/"+e.a+"-web"), nil)
	if l := items(e.ok(e.b, "GET", cv(host, coll), nil)); len(l) != 0 {
		t.Errorf("team-b lists %v", l)
	}
	nw := e.ok(e.a, "GET", cv(host, "global/networks/default"), nil)
	if l := nw["subnetworks"].([]any); len(l) != 1 || !strings.HasSuffix(l[0].(string), "/subnetworks/"+e.a+"-web") {
		t.Errorf("networks.get subnetworks: %v", nw["subnetworks"])
	}

	// patch: secondary ranges with the same checks.
	fp := got["fingerprint"]
	e.fail(400, "invalid", e.a, "PATCH", cv(host, coll+"/"+e.a+"-web"), obj{"fingerprint": fp, "secondaryIpRanges": []obj{{"rangeName": "pods", "ipCidrRange": ciBlock.String()}}})
	e.fail(400, "invalid", e.a, "PATCH", cv(host, coll+"/"+e.a+"-web"), obj{"fingerprint": fp, "ipCidrRange": wide.String()})
	e.fail(403, "forbidden", e.a, "PATCH", cv(host, coll+"/"+e.a+"-web"), obj{"fingerprint": fp, "privateIpGoogleAccess": true})
	e.ok(e.a, "PATCH", cv(host, coll+"/"+e.a+"-web"), obj{"fingerprint": fp, "description": "edge", "secondaryIpRanges": []obj{{"rangeName": "pods", "ipCidrRange": pods.String()}}})
	if got := e.own(e.a); got["subnetworks/us-central1/web/pods"] != pods.String() || len(got) != 2 {
		t.Errorf("own ranges after patch: %v", got)
	}
	e.gce.RunOperations()
	got = e.ok(e.a, "GET", cv(host, coll+"/"+e.a+"-web"), nil)
	if sec := got["secondaryIpRanges"].([]any); len(sec) != 1 || got["description"] != "edge" {
		t.Errorf("after patch: %v", got)
	}
	// A failed patch puts the space map back as GCE has the subnet.
	e.gce.FailOperation(fake.Failure{Method: "subnetworks.patch", Kind: fake.FailInternal})
	e.ok(e.a, "PATCH", cv(host, coll+"/"+e.a+"-web"), obj{"fingerprint": got["fingerprint"], "secondaryIpRanges": []any{}})
	if _, ok := e.own(e.a)["subnetworks/us-central1/web/pods"]; ok {
		t.Errorf("patch didn't free pods: %v", e.own(e.a))
	}
	e.gce.RunOperations()
	e.eventually("the failed patch's ranges to come back", func() bool { return e.own(e.a)["subnetworks/us-central1/web/pods"] == pods.String() })

	// expandIpCidrRange checks the new range.
	e.fail(400, "invalid", e.a, "POST", cv(host, coll+"/"+e.a+"-web/expandIpCidrRange"), obj{"ipCidrRange": aBlock.String()})
	e.fail(400, "invalid", e.a, "POST", cv(host, coll+"/"+e.a+"-web/expandIpCidrRange"), obj{"ipCidrRange": sub(bBlock, 23, 0).String()})
	e.ok(e.a, "POST", cv(host, coll+"/"+e.a+"-web/expandIpCidrRange"), obj{"ipCidrRange": wide.String()})
	if got := e.own(e.a); got["subnetworks/us-central1/web"] != wide.String() {
		t.Errorf("own ranges after expand: %v", got)
	}
	// The range keeps its region, which pins us-central1 (SV-37).
	var region string
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		return tx.QueryRow(ctx, `SELECT region FROM address_own WHERE silo = ? AND object = ?`, e.a, "subnetworks/us-central1/web").Scan(&region)
	}); err != nil || region != "us-central1" {
		t.Errorf("web's region after expand: %q %v", region, err)
	}
	e.gce.RunOperations()

	// The refused methods.
	for _, c := range []struct{ verb, path string }{
		{"POST", coll + "/web/setPrivateIpGoogleAccess"},
		{"POST", "global/networks"},
		{"DELETE", "global/networks/default"},
		{"PATCH", "global/networks/default"},
		{"POST", "global/networks/default/addPeering"},
		{"POST", "global/networks/default/switchToCustomMode"},
	} {
		if msg := e.fail(403, "forbidden", e.a, c.verb, cv(host, c.path), obj{"privateIpGoogleAccess": true}); !strings.Contains(msg, "method not allowed") {
			t.Errorf("%s %s: %q", c.verb, c.path, msg)
		}
	}

	// A failed insert refunds and frees its range.
	e.gce.FailOperation(fake.Failure{Method: "subnetworks.insert", Kind: fake.FailQuota})
	db := freeIn(aBlock, 24, ciBlock, sealedBlock, wide, pods)
	insert(obj{"name": e.a + "-db", "ipCidrRange": db.String()})
	if u := e.usage(e.a, "SUBNETWORKS"); u != 2 {
		t.Errorf("SUBNETWORKS used %v before the failure", u)
	}
	// team-a's quota is 2.
	e.fail(403, "quotaExceeded", e.a, "POST", cv(host, coll), obj{"name": e.a + "-more", "ipCidrRange": freeIn(aBlock, 24, ciBlock, sealedBlock, wide, pods, db).String()})
	e.gce.RunOperations()
	e.eventually("the failed insert's refund", func() bool { return e.usage(e.a, "SUBNETWORKS") == 1 })
	e.eventually("the failed insert's range to be freed", func() bool { _, ok := e.own(e.a)["subnetworks/us-central1/db"]; return !ok })

	// Delete frees the own ranges and refunds, once it has worked.
	e.ok(e.a, "DELETE", cv(host, coll+"/"+e.a+"-web"), nil)
	if len(e.own(e.a)) != 2 {
		t.Errorf("ranges freed before the delete finished: %v", e.own(e.a))
	}
	e.gce.RunOperations()
	e.eventually("the delete's refund", func() bool { return e.usage(e.a, "SUBNETWORKS") == 0 && len(e.own(e.a)) == 0 })
	e.fail(404, "notFound", e.a, "GET", cv(host, coll+"/"+e.a+"-web"), nil)
}

func TestFirewalls(t *testing.T) {
	e := newEnv(t)
	e.vpc()
	aBlock, ciBlock, sealedBlock := e.blocks(e.a)[0], e.blocks(e.ci)[0], e.blocks(e.sealed)[0]
	var own []netip.Prefix
	e.db.Tx(ctx, func(tx store.Tx) error { var err error; own, err = ownSpace(ctx, tx, e.a); return err })
	ownList := []string{}
	for _, o := range own {
		if o.Overlaps(ciBlock) || o.Overlaps(sealedBlock) || !aBlock.Contains(o.Addr()) {
			t.Fatalf("own space %v takes in a child's block", own)
		}
		ownList = append(ownList, o.String())
	}
	coll := "global/firewalls"
	realRule := func(name string) *compute.Firewall {
		t.Helper()
		fw, err := e.gce.GetFirewall(ctx, host, e.a+"-"+name)
		if err != nil {
			t.Fatal(err)
		}
		return fw
	}
	eq := func(a []string, b ...string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

	// Refusals and checks.
	if msg := e.fail(403, "forbidden", e.a, "POST", cv(host, coll), obj{"name": e.a + "-x", "allowed": []obj{{"IPProtocol": "tcp"}}, "targetServiceAccounts": []string{"sa@x"}}); msg != "silo: field targetServiceAccounts is not allowed" {
		t.Errorf("targetServiceAccounts: %q", msg)
	}
	e.fail(403, "forbidden", e.a, "POST", cv(host, coll), obj{"name": e.a + "-x", "allowed": []obj{{"IPProtocol": "tcp"}}, "sourceServiceAccounts": []string{"sa@x"}})
	e.fail(403, "forbidden", e.a, "POST", cv(host, coll), obj{"name": e.a + "-x", "allowed": []obj{{"IPProtocol": "tcp"}}, "targetTags": []string{"silo-x"}})
	if msg := e.fail(400, "invalid", e.a, "POST", cv(host, coll), obj{"name": e.a + "-x", "allowed": []obj{{"IPProtocol": "tcp"}}, "sourceRanges": []string{"192.168.0.0/16"}}); !strings.Contains(msg, ownList[0]) {
		t.Errorf("outside the own space: %q doesn't name it", msg)
	}
	e.fail(400, "invalid", e.a, "POST", cv(host, coll), obj{"name": e.a + "-x", "allowed": []obj{{"IPProtocol": "tcp"}}, "sourceRanges": []string{ciBlock.String()}})
	e.fail(400, "invalid", e.a, "POST", cv(host, coll), obj{"name": e.a + "-x", "direction": "EGRESS", "denied": []obj{{"IPProtocol": "all"}}, "destinationRanges": []string{"8.8.8.8"}})
	e.fail(400, "invalid", e.a, "POST", cv(host, coll), obj{"name": e.a + "-x", "allowed": []obj{{"IPProtocol": "tcp"}}, "sourceRanges": []string{"bogus"}})
	for _, tag := range []string{"web", e.b + "-web"} {
		msg := e.fail(400, "invalid", e.a, "POST", cv(host, coll), obj{"name": e.a + "-x", "allowed": []obj{{"IPProtocol": "tcp"}}, "targetTags": []string{tag}})
		if !strings.Contains(msg, `must start with "`+e.a+`-"`) {
			t.Errorf("tag %s: %q", tag, msg)
		}
	}
	if u := e.usage(e.a, "FIREWALLS"); u != 0 {
		t.Fatalf("FIREWALLS used %v after refusals", u)
	}

	// Pinning: 0.0.0.0/0 is the silo's own space, no target is the
	// silo's <id> tag, and the rest is kept.
	e.ok(e.a, "POST", cv(host, coll), obj{"name": e.a + "-ssh", "description": "let ssh in", "network": "global/networks/default",
		"priority": 900, "sourceRanges": []string{"0.0.0.0/0"}, "allowed": []obj{{"IPProtocol": "tcp", "ports": []string{"22"}}},
		"logConfig": obj{"enable": true}})
	fw := realRule("ssh")
	if !eq(fw.TargetTags, e.a) || !eq(fw.SourceRanges, ownList...) || fw.Priority != 900 || len(fw.Allowed) != 1 || fw.Allowed[0].Ports[0] != "22" ||
		fw.LogConfig == nil || !fw.LogConfig.Enable || gceapi.LastSegment(fw.Network) != "silo" || fw.Description != "silo:"+e.a+" let ssh in" {
		t.Errorf("real rule: %+v", fw)
	}
	inner := sub(aBlock, 28, 0)
	if inner.Overlaps(ciBlock) || inner.Overlaps(sealedBlock) {
		inner = sub(freeIn(aBlock, 24, ciBlock, sealedBlock), 28, 0)
	}
	e.ok(e.a, "POST", cv(host, coll), obj{"name": e.a + "-out", "direction": "EGRESS", "disabled": true,
		"destinationRanges": []string{"10.0.0.0/8", inner.String(), "203.0.113.0/24"}, "denied": []obj{{"IPProtocol": "all"}}, "targetTags": []string{e.a + "-web"}})
	fw = realRule("out")
	if !eq(fw.TargetTags, e.a+"-web") || !eq(fw.DestinationRanges, ownList...) || !fw.Disabled || fw.Direction != "EGRESS" {
		t.Errorf("real egress rule: %+v", fw)
	}
	if u := e.usage(e.a, "FIREWALLS"); u != 2 {
		t.Errorf("FIREWALLS used %v", u)
	}

	// get and list return the rule as the tenant wrote it.
	got := e.ok(e.a, "GET", cv(host, coll+"/"+e.a+"-ssh"), nil)
	if got["name"] != e.a+"-ssh" || got["description"] != "let ssh in" || !eq(strs(got["sourceRanges"]), "0.0.0.0/0") || got["targetTags"] != nil ||
		got["network"] != DefaultEndpoint+"projects/silo-"+e.a+"/global/networks/default" || got["priority"] != float64(900) ||
		got["selfLink"] != DefaultEndpoint+"projects/silo-"+e.a+"/global/firewalls/"+e.a+"-ssh" {
		t.Errorf("firewalls.get: %v", got)
	}
	got = e.ok(e.a, "GET", cv(host, coll+"/"+e.a+"-out"), nil)
	if !eq(strs(got["destinationRanges"]), "10.0.0.0/8", inner.String(), "203.0.113.0/24") || !eq(strs(got["targetTags"]), e.a+"-web") {
		t.Errorf("firewalls.get egress: %v", got)
	}
	if l := names(items(e.ok(e.a, "GET", cv(host, coll), nil))); !eq(l, e.a+"-out", e.a+"-ssh") {
		t.Errorf("firewalls.list: %v", l)
	}
	if l := items(e.ok(e.b, "GET", cv(host, coll), nil)); len(l) != 0 {
		t.Errorf("team-b lists %v", l)
	}
	e.fail(404, "notFound", e.b, "GET", cv(host, coll+"/"+e.b+"-ssh"), nil)
	e.fail(409, "alreadyExists", e.a, "POST", cv(host, coll), obj{"name": e.a + "-ssh", "allowed": []obj{{"IPProtocol": "tcp"}}})
	e.gce.RunOperations()

	// patch merges into the original and pins the result; update
	// replaces it.
	e.fail(400, "invalid", e.a, "PATCH", cv(host, coll+"/"+e.a+"-ssh"), obj{"sourceRanges": []string{"192.168.0.0/24"}})
	e.ok(e.a, "PATCH", cv(host, coll+"/"+e.a+"-ssh"), obj{"sourceRanges": []string{inner.String(), "192.168.0.0/24"}, "targetTags": []string{e.a + "-bastion"}})
	e.gce.RunOperations()
	fw = realRule("ssh")
	if !eq(fw.SourceRanges, inner.String()) || !eq(fw.TargetTags, e.a+"-bastion") || fw.Priority != 900 || fw.Description != "silo:"+e.a+" let ssh in" {
		t.Errorf("real rule after patch: %+v", fw)
	}
	got = e.ok(e.a, "GET", cv(host, coll+"/"+e.a+"-ssh"), nil)
	if !eq(strs(got["sourceRanges"]), inner.String(), "192.168.0.0/24") || !eq(strs(got["targetTags"]), e.a+"-bastion") || got["description"] != "let ssh in" {
		t.Errorf("firewalls.get after patch: %v", got)
	}
	e.ok(e.a, "PUT", cv(host, coll+"/"+e.a+"-ssh"), obj{"name": e.a + "-ssh", "allowed": []obj{{"IPProtocol": "tcp", "ports": []string{"2222"}}}})
	e.gce.RunOperations()
	fw = realRule("ssh")
	if !eq(fw.TargetTags, e.a) || !eq(fw.SourceRanges, ownList...) || fw.Priority != 1000 || fw.Allowed[0].Ports[0] != "2222" || fw.Description != "silo:"+e.a {
		t.Errorf("real rule after update: %+v", fw)
	}
	if got := e.ok(e.a, "GET", cv(host, coll+"/"+e.a+"-ssh"), nil); got["sourceRanges"] != nil || got["targetTags"] != nil {
		t.Errorf("firewalls.get after update: %v", got)
	}
	// A failed patch puts the original back.
	e.gce.FailOperation(fake.Failure{Method: "firewalls.update", Kind: fake.FailInternal})
	e.ok(e.a, "PATCH", cv(host, coll+"/"+e.a+"-ssh"), obj{"description": "doomed"})
	e.gce.RunOperations()
	e.eventually("the failed patch's original to come back", func() bool {
		return e.ok(e.a, "GET", cv(host, coll+"/"+e.a+"-ssh"), nil)["description"] == nil
	})

	// A failed insert refunds and forgets the original.
	e.gce.FailOperation(fake.Failure{Method: "firewalls.insert", Kind: fake.FailInternal})
	e.ok(e.a, "POST", cv(host, coll), obj{"name": e.a + "-doomed", "allowed": []obj{{"IPProtocol": "icmp"}}})
	e.fail(403, "quotaExceeded", e.a, "POST", cv(host, coll), obj{"name": e.a + "-fourth", "allowed": []obj{{"IPProtocol": "icmp"}}})
	e.gce.RunOperations()
	e.eventually("the failed insert's refund", func() bool { return e.usage(e.a, "FIREWALLS") == 2 })
	e.db.Tx(ctx, func(tx store.Tx) error {
		if _, ok, _ := original(ctx, tx, e.a, "doomed"); ok {
			t.Error("the failed insert's original is kept")
		}
		return nil
	})

	// Delete refunds once it has worked.
	e.ok(e.a, "DELETE", cv(host, coll+"/"+e.a+"-out"), nil)
	e.gce.RunOperations()
	e.eventually("the delete's refund", func() bool { return e.usage(e.a, "FIREWALLS") == 1 })
	e.fail(404, "notFound", e.a, "GET", cv(host, coll+"/"+e.a+"-out"), nil)
}

func strs(v any) []string {
	l, _ := v.([]any)
	out := []string{}
	for _, e := range l {
		out = append(out, e.(string))
	}
	return out
}

// TestFirewallPinningFence checks with the fence evaluator that a
// tenant rule with no target can't change what another silo's VMs
// receive, which the pinning is for (gcp-backend.md, "Firewall
// rules"), and that the same rule unpinned would have.
func TestFirewallPinningFence(t *testing.T) {
	e := newEnv(t)
	e.vpc()
	aBlock, ciBlock, sealedBlock, bBlock := e.blocks(e.a)[0], e.blocks(e.ci)[0], e.blocks(e.sealed)[0], e.blocks(e.b)[0]
	aNet := freeIn(aBlock, 24, ciBlock, sealedBlock)
	a1, a2 := fenceeval.VM(sub(aNet, 32, 2).Addr().String(), e.a), fenceeval.VM(sub(aNet, 32, 3).Addr().String(), e.a)
	b1, b2 := fenceeval.VM(sub(bBlock, 32, 2).Addr().String(), e.b), fenceeval.VM(sub(bBlock, 32, 3).Addr().String(), e.b)
	c1 := fenceeval.VM(sub(ciBlock, 32, 2).Addr().String(), e.ci)

	var rules []*compute.FirewallPolicyRule
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		in, err := fence.Load(ctx, tx, nil)
		if err != nil {
			return err
		}
		rules, err = fence.Compile(in, fence.DefaultConfig, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	policy := &compute.FirewallPolicy{Name: "silo-fence", Rules: rules}
	eval := func(extra ...*compute.Firewall) *fenceeval.Evaluator {
		t.Helper()
		fws, err := e.gce.ListFirewalls(ctx, host, gceapi.ListFilter{})
		if err != nil {
			t.Fatal(err)
		}
		ev, err := fenceeval.New(fenceeval.BeforeClassic, policy, append(fws, extra...))
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}
	// team-b allows ssh among its own VMs; team-a writes a rule with no
	// target that allows everything from anywhere, and one that denies
	// everything to anywhere.
	e.ok(e.b, "POST", cv(host, "global/firewalls"), obj{"name": e.b + "-ssh", "allowed": []obj{{"IPProtocol": "tcp", "ports": []string{"22"}}}})
	e.gce.RunOperations()
	before := eval()
	e.ok(e.a, "POST", cv(host, "global/firewalls"), obj{"name": e.a + "-open", "priority": 0, "sourceRanges": []string{"0.0.0.0/0"}, "allowed": []obj{{"IPProtocol": "all"}}})
	e.ok(e.a, "POST", cv(host, "global/firewalls"), obj{"name": e.a + "-shut", "priority": 0, "direction": "INGRESS", "denied": []obj{{"IPProtocol": "tcp", "ports": []string{"22"}}}, "sourceTags": []string{e.a + "-x"}})
	e.gce.RunOperations()
	after := eval()

	packets := []fenceeval.Packet{
		fenceeval.TCP(b1, b2, 22), fenceeval.TCP(b1, b2, 80), fenceeval.TCP(b2, b1, 22),
		fenceeval.TCP(a1, b1, 22), fenceeval.TCP(a1, b1, 80), fenceeval.TCP(c1, b1, 22),
		fenceeval.TCP(b1, c1, 22), fenceeval.TCP(c1, c1, 80), fenceeval.TCP(b1, a1, 22),
	}
	for _, p := range packets {
		v0, err := before.Evaluate(p)
		if err != nil {
			t.Fatal(err)
		}
		v1, _ := after.Evaluate(p)
		if v0.Allowed != v1.Allowed || v0.Ingress.Rule != v1.Ingress.Rule {
			t.Errorf("%s: team-a's rules changed it from %s to %s", p, v0.Ingress, v1.Ingress)
		}
	}
	if !after.Allowed(fenceeval.TCP(b1, b2, 22)) || after.Allowed(fenceeval.TCP(b1, b2, 80)) {
		t.Error("team-b's own rule doesn't decide its own traffic")
	}
	// Inside team-a, its rule applies.
	if before.Allowed(fenceeval.TCP(a1, a2, 80)) || !after.Allowed(fenceeval.TCP(a1, a2, 80)) {
		t.Error("team-a's rule doesn't open team-a")
	}
	// Unpinned, the same rule would let anything into team-b.
	unpinned := eval(&compute.Firewall{Name: "unpinned", Network: e.facade.networkLink(), Direction: "INGRESS", Priority: 0,
		SourceRanges: []string{"0.0.0.0/0"}, Allowed: []*compute.FirewallAllowed{{IPProtocol: "all"}}})
	if !unpinned.Allowed(fenceeval.TCP(b1, b2, 80)) {
		t.Error("an unpinned rule doesn't reach team-b, so this test checks nothing")
	}
}

// TestFirewallLegacyOriginal: an original kept before tenants named
// objects by their real names (SV-76) has the bare name and tags. It
// is still the rule's, keyed by the name less the prefix, and a patch
// keeps the real rule's tags rather than refusing the bare ones.
func TestFirewallLegacyOriginal(t *testing.T) {
	e := newEnv(t)
	e.vpc()
	coll := "global/firewalls"
	e.ok(e.a, "POST", cv(host, coll), obj{"name": e.a + "-web", "allowed": []obj{{"IPProtocol": "tcp"}}, "targetTags": []string{e.a + "-web"}})
	e.gce.RunOperations()
	legacy := map[string]any{"name": "web", "description": "old", "allowed": []any{obj{"IPProtocol": "tcp"}}, "targetTags": []any{"web"}}
	if err := e.db.Tx(ctx, func(tx store.Tx) error { return putOriginal(ctx, tx, e.a, "web", legacy, false) }); err != nil {
		t.Fatal(err)
	}
	got := e.ok(e.a, "GET", cv(host, coll+"/"+e.a+"-web"), nil)
	if got["name"] != e.a+"-web" || fmt.Sprint(got["targetTags"]) != "["+e.a+"-web]" || got["description"] != "old" {
		t.Errorf("get: %v", got)
	}
	e.ok(e.a, "PATCH", cv(host, coll+"/"+e.a+"-web"), obj{"description": "new"})
	e.gce.RunOperations()
	fw, err := e.gce.GetFirewall(ctx, host, e.a+"-web")
	if err != nil || strings.Join(fw.TargetTags, ",") != e.a+"-web" {
		t.Errorf("real rule after patch: %+v %v", fw, err)
	}
}
