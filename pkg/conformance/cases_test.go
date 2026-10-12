//go:build silo_conformance

package conformance

// The cases (docs/silo/testing.md, "Cases", SV-122) this suite has so
// far: attaching outside the silo, subnets, list and get leaks, name
// collisions, refused fields and methods with the discovery drift
// check, quota, instance templates, still refused, and the database's
// reachability. Each test and subtest is one case by name, so grain's
// test history keeps a pass rate for each.
//
// "Refused" is checked as the spec says: the facade's GCE-shaped error,
// and no GCE object appearing (a get of the name returns 404).

import (
	"context"
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
)

// tree is the small tree every case uses: /a, /a/c and /b, each with a
// subnet in the home region.
type siloInfo struct {
	path, id, project string
	block             netip.Prefix
	subnet            netip.Prefix // the "net" subnet, the block's first /24
}

var (
	treeOnce sync.Once
	treeErr  error
	a, c, b  siloInfo
)

const probeImage = "projects/debian-cloud/global/images/family/debian-12"

func tree(t *testing.T) {
	t.Helper()
	installed(t)
	treeOnce.Do(func() { treeErr = makeTree(t) })
	if treeErr != nil {
		t.Fatalf("setting up /a, /a/c and /b: %v", treeErr)
	}
}

func makeTree(t *testing.T) error {
	ctx := context.Background()
	quota := func(n int) client.QuotaMap {
		return client.QuotaMap{run.region: {
			"vcpu": num(n), "spot_vcpu": num(n), "memory_mb": num(n * 2048), "spot_memory_mb": num(n * 2048),
			"instances": num(4), "standard_disk_gb": num(60), "ssd_disk_gb": num(20),
		}, "global": {"subnetworks": num(4), "instance_templates": num(2)}}
	}
	mk := func(p string, prefix, vcpu int) (siloInfo, error) {
		s, err := run.api.Create(ctx, p, client.CreateRequest{Regions: []string{run.region}, Quota: quota(vcpu),
			Address: &client.AddressWant{PrefixLength: prefix}}, client.Mutation{})
		if err != nil {
			return siloInfo{}, fmt.Errorf("create %s: %v", p, err)
		}
		if len(s.Address.Blocks) == 0 {
			return siloInfo{}, fmt.Errorf("%s has no block", p)
		}
		blk := netip.MustParsePrefix(s.Address.Blocks[0])
		info := siloInfo{path: p, id: s.ID, project: "silo-" + s.ID, block: blk,
			subnet: netip.PrefixFrom(blk.Addr(), 24)}
		return info, nil
	}
	var err error
	if a, err = mk("/a", 20, 4); err != nil {
		return err
	}
	if b, err = mk("/b", 20, 4); err != nil {
		return err
	}
	if c, err = mk("/a/c", 23, 1); err != nil {
		return err
	}
	for _, s := range []siloInfo{a, b, c} {
		code, op := facade(t, "POST", "projects/"+s.project+"/regions/"+run.region+"/subnetworks",
			map[string]any{"name": s.id + "-net", "network": "default", "ipCidrRange": s.subnet.String()})
		if code != 200 {
			return fmt.Errorf("subnet in %s: %d %v", s.path, code, op)
		}
		waitOp(t, s.project, op)
	}
	return nil
}

func num(n int) json.Number { return json.Number(fmt.Sprint(n)) }

// refused asserts a facade call is refused with want (403 or 400), and
// that nothing named check appears afterwards.
func refused(t *testing.T, want int, method, path string, body any, check string) {
	t.Helper()
	code, out := facade(t, method, path, body)
	if code != want {
		t.Errorf("%s %s: got %d %v, want %d", method, path, code, out, want)
		if code == 200 {
			waitOp(t, projectOf(path), out)
		}
	}
	if check != "" {
		if code, _ := facade(t, "GET", check, nil); code != 404 {
			t.Errorf("%s exists after a refused call (%d)", check, code)
			facade(t, "DELETE", check, nil)
		}
	}
}

func projectOf(path string) string {
	f := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(f) > 1 {
		return f[1]
	}
	return ""
}

// instanceBody is a probe-sized instance in silo s's net subnet, Spot.
func instanceBody(name string, s siloInfo, mutate func(map[string]any)) map[string]any {
	body := map[string]any{
		"name":        name,
		"machineType": "zones/" + run.zone + "/machineTypes/" + ProbeMachineType,
		"scheduling":  map[string]any{"provisioningModel": "SPOT", "instanceTerminationAction": "DELETE"},
		"disks": []any{map[string]any{"boot": true, "autoDelete": true,
			"initializeParams": map[string]any{"sourceImage": probeImage, "diskSizeGb": "10", "diskType": "zones/" + run.zone + "/diskTypes/pd-standard"}}},
		"networkInterfaces": []any{map[string]any{"subnetwork": "regions/" + run.region + "/subnetworks/" + s.id + "-net"}},
	}
	if mutate != nil {
		mutate(body)
	}
	return body
}

func instances(s siloInfo) string {
	return "projects/" + s.project + "/zones/" + run.zone + "/instances"
}

// probe makes a probe VM, s's prefix and name, in s that runs checks, waits for its report on
// the serial console, and deletes it. At most MaxVMs exist at once.
func probe(t *testing.T, s siloInfo, name string, checks []Check) map[string]ProbeResult {
	t.Helper()
	run.vms <- struct{}{}
	defer func() { <-run.vms }()
	name = s.id + "-" + name
	body := instanceBody(name, s, func(m map[string]any) {
		m["metadata"] = map[string]any{"items": []any{map[string]any{"key": "startup-script", "value": ProbeScript(checks)}}}
	})
	code, op := facade(t, "POST", instances(s), body)
	if code != 200 {
		t.Fatalf("probe %s in %s: %d %v", name, s.path, code, op)
	}
	waitOp(t, s.project, op)
	defer func() {
		_, op := facade(t, "DELETE", instances(s)+"/"+name, nil)
		waitOp(t, s.project, op)
	}()
	deadline := time.Now().Add(8 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(15 * time.Second)
		_, out := facade(t, "GET", instances(s)+"/"+name+"/serialPort?port=1", nil)
		text, _ := out["contents"].(string)
		if res, done := ParseProbeReport(text); done {
			return res
		}
	}
	t.Fatalf("probe %s in %s never reported", name, s.path)
	return nil
}

// TestAttachingOutsideTheSilo: an instance in /a naming /b's subnet, by
// name, full URL and real prefixed name; and an IP outside the subnet.
func TestAttachingOutsideTheSilo(t *testing.T) {
	tree(t)
	forms := map[string]string{
		"SubnetByFullURL":      "https://www.googleapis.com/compute/v1/projects/" + b.project + "/regions/" + run.region + "/subnetworks/" + b.id + "-net",
		"SubnetByRealName":     "projects/" + run.project + "/regions/" + run.region + "/subnetworks/" + b.id + "-net",
		"SubnetByOtherProject": "projects/" + b.project + "/regions/" + run.region + "/subnetworks/" + b.id + "-net",
	}
	for name, ref := range forms {
		t.Run(name, func(t *testing.T) {
			refused(t, 403, "POST", instances(a), instanceBody(a.id+"-x-attach", a, func(m map[string]any) {
				m["networkInterfaces"] = []any{map[string]any{"subnetwork": ref}}
			}), instances(a)+"/"+a.id+"-x-attach")
		})
	}
	t.Run("NetworkIPOutsideSubnet", func(t *testing.T) {
		refused(t, 400, "POST", instances(a), instanceBody(a.id+"-x-ip", a, func(m map[string]any) {
			m["networkInterfaces"] = []any{map[string]any{"subnetwork": "regions/" + run.region + "/subnetworks/" + a.id + "-net", "networkIP": b.subnet.Addr().Next().String()}}
		}), instances(a)+"/"+a.id+"-x-ip")
	})
	t.Run("AliasRangeOutsideSubnet", func(t *testing.T) {
		refused(t, 400, "POST", instances(a), instanceBody(a.id+"-x-alias", a, func(m map[string]any) {
			m["networkInterfaces"] = []any{map[string]any{"subnetwork": "regions/" + run.region + "/subnetworks/" + a.id + "-net",
				"aliasIpRanges": []any{map[string]any{"ipCidrRange": b.subnet.String()}}}}
		}), instances(a)+"/"+a.id+"-x-alias")
	})
}

// TestSubnets: ranges outside the silo's blocks, over a child's or
// another silo's, and regions outside the allowed set.
func TestSubnets(t *testing.T) {
	tree(t)
	sub := "projects/" + a.project + "/regions/" + run.region + "/subnetworks"
	cases := map[string]string{
		"OutsideBlocks":        netip.PrefixFrom(run.gateway.Addr(), 24).String(),
		"OverlapsChildsBlock":  c.block.String(),
		"OverlapsOtherSilo":    b.subnet.String(),
		"OverlapsControlRange": run.control.String(),
	}
	for name, cidr := range cases {
		t.Run(name, func(t *testing.T) {
			refused(t, 400, "POST", sub, map[string]any{"name": a.id + "-x-sub", "network": "default", "ipCidrRange": cidr}, sub+"/"+a.id+"-x-sub")
		})
	}
	t.Run("RegionNotAllowed", func(t *testing.T) {
		p := "projects/" + a.project + "/regions/europe-west4/subnetworks"
		refused(t, 403, "POST", p, map[string]any{"name": a.id + "-x-sub", "network": "default", "ipCidrRange": netip.PrefixFrom(a.block.Addr().Next(), 24).String()}, "")
	})
	t.Run("ZoneNotAllowed", func(t *testing.T) {
		refused(t, 403, "POST", "projects/"+a.project+"/zones/europe-west4-a/instances", instanceBody(a.id+"-x-zone", a, nil), "")
	})
}

// TestListAndGetLeaks: lists from /a show nothing of /b's or /a/c's, a
// get of /b's object by any name form is 404, and a made-up or another
// silo's project is 403 from inside /a.
func TestListAndGetLeaks(t *testing.T) {
	tree(t)
	t.Run("ListShowsOwnOnly", func(t *testing.T) {
		for _, f := range []string{"", "?filter=" + "name%3D%22" + b.id + "-net%22"} {
			_, out := facade(t, "GET", "projects/"+a.project+"/regions/"+run.region+"/subnetworks"+f, nil)
			items, _ := out["items"].([]any)
			for _, it := range items {
				m, _ := it.(map[string]any)
				if m["ipCidrRange"] != a.subnet.String() {
					t.Errorf("a's list shows %v", m)
				}
			}
		}
	})
	t.Run("AggregatedListShowsOwnOnly", func(t *testing.T) {
		_, out := facade(t, "GET", "projects/"+a.project+"/aggregated/subnetworks", nil)
		s := fmt.Sprint(out)
		for _, leak := range []string{b.subnet.String(), c.subnet.String(), b.id, c.id} {
			if strings.Contains(s, leak) {
				t.Errorf("aggregatedList from /a mentions %s", leak)
			}
		}
	})
	t.Run("NoSiloLabelsOrHostLinks", func(t *testing.T) {
		_, out := facade(t, "GET", "projects/"+a.project+"/regions/"+run.region+"/subnetworks/"+a.id+"-net", nil)
		s := fmt.Sprint(out)
		for _, leak := range []string{run.project, a.id + "-net", "silo-install"} {
			if strings.Contains(s, leak) {
				t.Errorf("a's subnet shows %q: %s", leak, s)
			}
		}
	})
	t.Run("GetByRealNameIs404", func(t *testing.T) {
		code, _ := facade(t, "GET", "projects/"+a.project+"/regions/"+run.region+"/subnetworks/"+b.id+"-net", nil)
		if code != 404 {
			t.Errorf("got %d, want 404", code)
		}
	})
	t.Run("MadeUpProjectIs403", func(t *testing.T) {
		code, _ := facade(t, "GET", "projects/silo-zzzzzz/regions/"+run.region+"/subnetworks", nil)
		if code != 403 {
			t.Errorf("got %d, want 403", code)
		}
	})
	t.Run("AncestorWritesChild", func(t *testing.T) {
		p := "projects/" + c.project + "/regions/" + run.region + "/subnetworks"
		// c's net is its block's first /24; the next one is free.
		next := netip.PrefixFrom(netip.AddrFrom4([4]byte{c.subnet.Addr().As4()[0], c.subnet.Addr().As4()[1], c.subnet.Addr().As4()[2] + 1, 0}), 24)
		code, op := facade(t, "POST", p, map[string]any{"name": c.id + "-from-a", "network": "default", "ipCidrRange": next.String()})
		if code != 200 {
			t.Fatalf("ancestor write to /a/c: %d %v", code, op)
		}
		waitOp(t, c.project, op)
		_, op = facade(t, "DELETE", p+"/"+c.id+"-from-a", nil)
		waitOp(t, c.project, op)
	})
}

// TestNameCollisions: the same name in two silos, the 56-character limit
// and silo-prefixed labels.
func TestNameCollisions(t *testing.T) {
	tree(t)
	t.Run("SameNameInTwoSilos", func(t *testing.T) {
		for _, s := range []siloInfo{a, b} {
			code, _ := facade(t, "GET", "projects/"+s.project+"/regions/"+run.region+"/subnetworks/"+s.id+"-net", nil)
			if code != 200 {
				t.Errorf("%s's own net: %d", s.path, code)
			}
		}
	})
	t.Run("Name63Allowed64Refused", func(t *testing.T) {
		// The whole name is the tenant's: GCE's 63, the prefix included.
		p := "projects/" + a.project + "/global/firewalls"
		fw := func(name string) map[string]any {
			return map[string]any{"name": name, "network": "global/networks/default",
				"allowed": []any{map[string]any{"IPProtocol": "tcp", "ports": []string{"22"}}}, "sourceRanges": []string{a.subnet.String()}}
		}
		refused(t, 400, "POST", p, fw(a.id+"-"+strings.Repeat("x", 57)), "")
		n63 := a.id + "-" + strings.Repeat("y", 56)
		code, op := facade(t, "POST", p, fw(n63))
		if code != 200 {
			t.Fatalf("63 characters: %d %v", code, op)
		}
		waitOp(t, a.project, op)
		_, op = facade(t, "DELETE", p+"/"+n63, nil)
		waitOp(t, a.project, op)
	})
	t.Run("NameWithoutThePrefixRefused", func(t *testing.T) {
		// A bare name, or another silo's prefix, is 400 naming the prefix.
		p := "projects/" + a.project + "/global/firewalls"
		for _, name := range []string{"x-bare", b.id + "-x-other"} {
			code, out := facade(t, "POST", p, map[string]any{"name": name, "network": "global/networks/default",
				"allowed": []any{map[string]any{"IPProtocol": "tcp", "ports": []string{"22"}}}, "sourceRanges": []string{a.subnet.String()}})
			if code != 400 || !strings.Contains(fmt.Sprint(out), `must start with "`+a.id+`-"`) {
				t.Errorf("%s: %d %v, want 400 naming the prefix %s-", name, code, out, a.id)
			}
		}
	})
	t.Run("SiloLabelRefused", func(t *testing.T) {
		refused(t, 400, "POST", instances(a), instanceBody(a.id+"-x-label", a, func(m map[string]any) {
			m["labels"] = map[string]string{"silo-x": "y"}
		}), instances(a)+"/"+a.id+"-x-label")
	})
	t.Run("SiloTagRefused", func(t *testing.T) {
		refused(t, 400, "POST", instances(a), instanceBody(a.id+"-x-tag", a, func(m map[string]any) {
			m["tags"] = map[string]any{"items": []string{"silo-gw"}}
		}), instances(a)+"/"+a.id+"-x-tag")
	})
}

// TestRefusedFieldsAndMethods: each item of gcp-backend.md "Refused",
// and the discovery-document drift check.
func TestRefusedFieldsAndMethods(t *testing.T) {
	tree(t)
	inst := func(name string, f func(map[string]any)) func(t *testing.T) {
		return func(t *testing.T) {
			refused(t, 403, "POST", instances(a), instanceBody(name, a, f), instances(a)+"/"+name)
		}
	}
	nic := func(extra map[string]any) func(map[string]any) {
		return func(m map[string]any) {
			n := map[string]any{"subnetwork": "regions/" + run.region + "/subnetworks/" + a.id + "-net"}
			for k, v := range extra {
				n[k] = v
			}
			m["networkInterfaces"] = []any{n}
		}
	}
	meta := func(k, v string) func(map[string]any) {
		return func(m map[string]any) {
			m["metadata"] = map[string]any{"items": []any{map[string]any{"key": k, "value": v}}}
		}
	}
	t.Run("AccessConfigs", inst("x-ac", nic(map[string]any{"accessConfigs": []any{map[string]any{"type": "ONE_TO_ONE_NAT"}}})))
	t.Run("Ipv6AccessConfigs", inst("x-ac6", nic(map[string]any{"ipv6AccessConfigs": []any{map[string]any{"type": "DIRECT_IPV6"}}})))
	t.Run("CanIpForward", inst("x-fwd", func(m map[string]any) { m["canIpForward"] = true }))
	t.Run("ServiceAccounts", inst("x-sa", func(m map[string]any) {
		m["serviceAccounts"] = []any{map[string]any{"email": "default", "scopes": []string{"https://www.googleapis.com/auth/cloud-platform"}}}
	}))
	t.Run("EnableOSLogin", inst("x-osl", meta("enable-oslogin", "TRUE")))
	t.Run("EnableOSLogin2FA", inst("x-osl2", meta("enable-oslogin-2fa", "TRUE")))
	t.Run("EnableWindowsSSH", inst("x-wssh", meta("enable-windows-ssh", "TRUE")))
	t.Run("BlockProjectSSHKeysFalse", inst("x-bpk", meta("block-project-ssh-keys", "false")))
	methods := []struct{ name, method, path string }{
		{"Routes", "POST", "global/routes"},
		{"ExternalAddress", "POST", "regions/" + run.region + "/addresses"},
		{"Routers", "POST", "regions/" + run.region + "/routers"},
		{"VPNGateways", "POST", "regions/" + run.region + "/vpnGateways"},
		{"Interconnects", "POST", "global/interconnects"},
		{"ForwardingRules", "POST", "regions/" + run.region + "/forwardingRules"},
		{"BackendServices", "POST", "global/backendServices"},
		{"SetCommonInstanceMetadata", "POST", "setCommonInstanceMetadata"},
		{"SetUsageExportBucket", "POST", "setUsageExportBucket"},
		{"NetworksInsert", "POST", "global/networks"},
		{"NetworksDelete", "DELETE", "global/networks/default"},
		{"NetworksPatch", "PATCH", "global/networks/default"},
		{"NetworksAddPeering", "POST", "global/networks/default/addPeering"},
		{"NetworksSwitchToCustomMode", "POST", "global/networks/default/switchToCustomMode"},
		{"SubnetSetPrivateIpGoogleAccess", "POST", "regions/" + run.region + "/subnetworks/" + a.id + "-net/setPrivateIpGoogleAccess"},
		{"ReservationsInsert", "POST", "zones/" + run.zone + "/reservations"},
		{"NodeGroupsInsert", "POST", "zones/" + run.zone + "/nodeGroups"},
		{"NodeTemplatesInsert", "POST", "regions/" + run.region + "/nodeTemplates"},
		{"MachineImagesInsert", "POST", "global/machineImages"},
		{"StoragePoolsInsert", "POST", "zones/" + run.zone + "/storagePools"},
		{"InstanceGroupManagersInsert", "POST", "zones/" + run.zone + "/instanceGroupManagers"},
		{"RegionInstanceGroupManagersInsert", "POST", "regions/" + run.region + "/instanceGroupManagers"},
		{"AutoscalersInsert", "POST", "zones/" + run.zone + "/autoscalers"},
		{"ServiceAttachments", "POST", "regions/" + run.region + "/serviceAttachments"},
	}
	for _, m := range methods {
		t.Run(m.name, func(t *testing.T) {
			refused(t, 403, m.method, "projects/"+a.project+"/"+m.path, map[string]any{"name": a.id + "-x-refused"}, "")
		})
	}
	t.Run("SubnetPrivateGoogleAccess", func(t *testing.T) {
		p := "projects/" + a.project + "/regions/" + run.region + "/subnetworks"
		refused(t, 403, "POST", p, map[string]any{"name": a.id + "-x-pga", "network": "default", "privateIpGoogleAccess": true,
			"ipCidrRange": netip.PrefixFrom(a.block.Addr(), 20).String()}, p+"/"+a.id+"-x-pga")
	})
	t.Run("FirewallServiceAccounts", func(t *testing.T) {
		p := "projects/" + a.project + "/global/firewalls"
		refused(t, 403, "POST", p, map[string]any{"name": a.id + "-x-fwsa", "network": "global/networks/default",
			"allowed": []any{map[string]any{"IPProtocol": "tcp"}}, "targetServiceAccounts": []string{"x@y.iam.gserviceaccount.com"}}, p+"/"+a.id+"-x-fwsa")
	})
	t.Run("DiscoveryDrift", func(t *testing.T) {
		resp, err := http.Get("https://compute.googleapis.com/$discovery/rest?version=v1")
		if err != nil {
			t.Fatal(err)
		}
		doc, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		drift, err := DriftFields(doc, "Instance", FacadeInstanceFields)
		if err != nil {
			t.Fatal(err)
		}
		if len(drift) == 0 {
			t.Skip("every settable Instance field in today's discovery document is one the facade knows")
		}
		for _, f := range drift {
			t.Run(f, func(t *testing.T) {
				code, out := facade(t, "POST", instances(a), instanceBody(a.id+"-x-drift", a, func(m map[string]any) { m[f] = map[string]any{} }))
				if code != 403 || !strings.Contains(fmt.Sprint(out), f) {
					t.Errorf("%s: got %d %v, want 403 naming the field", f, code, out)
					if code == 200 {
						waitOp(t, a.project, out)
						facade(t, "DELETE", instances(a)+"/"+a.id+"-x-drift", nil)
					}
				}
			})
		}
	})
}

// TestQuota: past vcpu, a child's grant past the parent's free amount,
// and the GPU case, which reports skipped where the project has no GPU
// quota.
func TestQuota(t *testing.T) {
	tree(t)
	ctx := context.Background()
	t.Run("InstancePastVCPU", func(t *testing.T) {
		// /a/c has 1 vCPU; e2-standard-4 is 4.
		refused(t, 403, "POST", instances(c), instanceBody(c.id+"-x-big", c, func(m map[string]any) {
			m["machineType"] = "zones/" + run.zone + "/machineTypes/e2-standard-4"
		}), instances(c)+"/"+c.id+"-x-big")
	})
	t.Run("DiskPastSSDGrant", func(t *testing.T) {
		p := "projects/" + c.project + "/zones/" + run.zone + "/disks"
		refused(t, 403, "POST", p, map[string]any{"name": c.id + "-x-ssd", "sizeGb": "500", "type": "zones/" + run.zone + "/diskTypes/pd-ssd"}, p+"/"+c.id+"-x-ssd")
	})
	t.Run("ChildGrantPastParentFree", func(t *testing.T) {
		_, err := run.api.Create(ctx, "/a/greedy", client.CreateRequest{Regions: []string{run.region},
			Quota: client.QuotaMap{run.region: {"vcpu": num(1000)}}, Address: &client.AddressWant{PrefixLength: 28}}, client.Mutation{})
		if err == nil {
			run.api.Delete(ctx, "/a/greedy", true, client.Mutation{})
			t.Fatal("a child got more vcpu than its parent has free")
		}
	})
	t.Run("GPU", func(t *testing.T) {
		body := instanceBody(a.id+"-x-gpu", a, func(m map[string]any) {
			m["machineType"] = "zones/" + run.zone + "/machineTypes/n1-standard-1"
			m["guestAccelerators"] = []any{map[string]any{"acceleratorType": "zones/" + run.zone + "/acceleratorTypes/nvidia-tesla-t4", "acceleratorCount": 1}}
			m["scheduling"] = map[string]any{"provisioningModel": "SPOT", "onHostMaintenance": "TERMINATE"}
		})
		var preview map[string]any
		err := run.api.Do(ctx, "POST", "quota:preview", nil, map[string]any{"facade_request": map[string]any{
			"method": "POST", "path": "/compute/v1/" + instances(a), "body": body}}, &preview)
		if err != nil {
			t.Fatalf("quota:preview: %v", err)
		}
		if !strings.Contains(fmt.Sprint(preview), "preemptible_nvidia_t4") && !strings.Contains(fmt.Sprint(preview), "spot_gpus") {
			t.Errorf("a Spot T4's preview doesn't charge a Spot GPU class: %v", preview)
		}
		t.Run("WithoutTerminateRefused", func(t *testing.T) {
			bad := instanceBody(a.id+"-x-gpu2", a, func(m map[string]any) {
				m["machineType"] = "zones/" + run.zone + "/machineTypes/n1-standard-1"
				m["guestAccelerators"] = body["guestAccelerators"]
			})
			refused(t, 400, "POST", instances(a), bad, instances(a)+"/"+a.id+"-x-gpu2")
		})
		t.Run("RealGPUInstance", func(t *testing.T) {
			r, err := run.compute.Regions.Get(run.project, run.region).Do()
			if err != nil {
				t.Fatal(err)
			}
			for _, q := range r.Quotas {
				if (q.Metric == "PREEMPTIBLE_NVIDIA_T4_GPUS" || q.Metric == "NVIDIA_T4_GPUS") && q.Limit-q.Usage >= 1 {
					t.Skip("TODO: a real GPU instance is the next conformance task's; the project has T4 quota")
				}
			}
			t.Skip("bsalmon-fun-sandbox has no T4 GPU quota in " + run.region + ": the real GPU instance is skipped, not passed")
		})
	})
}

// TestInstanceTemplates: a template naming /b's objects or a refused
// field is refused; /b can't see or use /a's.
func TestInstanceTemplates(t *testing.T) {
	tree(t)
	tpl := func(s siloInfo) string { return "projects/" + s.project + "/global/instanceTemplates" }
	props := func(subnet string) map[string]any {
		body := instanceBody("", a, nil)
		delete(body, "name")
		body["networkInterfaces"] = []any{map[string]any{"subnetwork": subnet}}
		return body
	}
	t.Run("OthersSubnetRefused", func(t *testing.T) {
		refused(t, 403, "POST", tpl(a), map[string]any{"name": a.id + "-x-t1", "properties": props("projects/" + b.project + "/regions/" + run.region + "/subnetworks/" + b.id + "-net")}, tpl(a)+"/"+a.id+"-x-t1")
	})
	t.Run("RefusedFieldRefused", func(t *testing.T) {
		p := props("regions/" + run.region + "/subnetworks/" + a.id + "-net")
		p["canIpForward"] = true
		refused(t, 403, "POST", tpl(a), map[string]any{"name": a.id + "-x-t2", "properties": p}, tpl(a)+"/"+a.id+"-x-t2")
	})
	t.Run("SourceInstanceRefused", func(t *testing.T) {
		refused(t, 403, "POST", tpl(a), map[string]any{"name": a.id + "-x-t3", "sourceInstance": "zones/" + run.zone + "/instances/" + a.id + "-x"}, tpl(a)+"/"+a.id+"-x-t3")
	})
	t.Run("OtherSiloCantUseOrSee", func(t *testing.T) {
		code, op := facade(t, "POST", tpl(a), map[string]any{"name": a.id + "-tmpl", "properties": props("regions/" + run.region + "/subnetworks/" + a.id + "-net")})
		if code != 200 {
			t.Fatalf("a valid template in /a: %d %v", code, op)
		}
		waitOp(t, a.project, op)
		defer func() { _, op := facade(t, "DELETE", tpl(a)+"/"+a.id+"-tmpl", nil); waitOp(t, a.project, op) }()
		_, l := facade(t, "GET", tpl(b), nil)
		if items, _ := l["items"].([]any); len(items) > 0 {
			t.Errorf("/b lists templates: %v", items)
		}
		refused(t, 403, "POST", instances(b)+"?sourceInstanceTemplate=projects/"+a.project+"/global/instanceTemplates/"+a.id+"-tmpl",
			map[string]any{"name": b.id + "-x-from-a"}, instances(b)+"/"+b.id+"-x-from-a")
		t.Run("InsertAddingRefusedField", func(t *testing.T) {
			refused(t, 403, "POST", instances(a)+"?sourceInstanceTemplate=global/instanceTemplates/"+a.id+"-tmpl",
				map[string]any{"name": a.id + "-x-tf", "canIpForward": true}, instances(a)+"/"+a.id+"-x-tf")
		})
	})
}

// TestStillRefused: storage pools, reservations, sole tenancy, machine
// images and MIGs, on the fields as well as the methods.
func TestStillRefused(t *testing.T) {
	tree(t)
	t.Run("DiskInStoragePool", func(t *testing.T) {
		p := "projects/" + a.project + "/zones/" + run.zone + "/disks"
		refused(t, 403, "POST", p, map[string]any{"name": a.id + "-x-pool", "sizeGb": "10", "storagePool": "zones/" + run.zone + "/storagePools/p"}, p+"/"+a.id+"-x-pool")
	})
	t.Run("ReservationAffinity", func(t *testing.T) {
		refused(t, 403, "POST", instances(a), instanceBody(a.id+"-x-res", a, func(m map[string]any) {
			m["reservationAffinity"] = map[string]any{"consumeReservationType": "ANY_RESERVATION"}
		}), instances(a)+"/"+a.id+"-x-res")
	})
	t.Run("NodeAffinities", func(t *testing.T) {
		refused(t, 403, "POST", instances(a), instanceBody(a.id+"-x-node", a, func(m map[string]any) {
			m["scheduling"] = map[string]any{"nodeAffinities": []any{map[string]any{"key": "compute.googleapis.com/node-group-name", "operator": "IN", "values": []string{"g"}}}}
		}), instances(a)+"/"+a.id+"-x-node")
	})
}

// TestDatabase: from probes in the root's tree (/a and /a/c), the
// database's PSC endpoint is unreachable on 5432, 3307, a sample of
// other ports and ICMP; its DNS name resolves but doesn't connect; and
// silod stays healthy throughout.
func TestDatabase(t *testing.T) {
	tree(t)
	ctx := context.Background()
	addr, err := run.compute.Addresses.Get(run.project, run.region, "silo-db-psc").Do()
	if err != nil {
		t.Fatalf("the PSC endpoint's address: %v", err)
	}
	ep := addr.Address
	dnsName := ""
	if l, err := run.sql.Instances.List(run.project).Do(); err == nil {
		for _, o := range l.Items {
			if o.Settings != nil && o.Settings.UserLabels["silo-install"] != "" {
				dnsName = o.DnsName
			}
		}
	}
	checks := []Check{{ID: "icmp", Kind: "icmp", Host: ep}}
	for _, port := range []int{5432, 3307, 22, 80, 443, 8080} {
		checks = append(checks, Check{ID: fmt.Sprintf("tcp-%d", port), Kind: "tcp", Host: ep, Port: port})
	}
	if dnsName != "" {
		checks = append(checks, Check{ID: "dns", Kind: "dns", Host: dnsName},
			Check{ID: "name-5432", Kind: "tcp", Host: dnsName, Port: 5432})
	}
	healthy := make(chan error, 1)
	go func() {
		var bad error
		for i := 0; i < 40; i++ {
			if _, err := run.api.Describe(ctx, "/", false); err != nil {
				bad = err
				break
			}
			time.Sleep(15 * time.Second)
		}
		healthy <- bad
	}()
	var wg sync.WaitGroup
	results := map[string]map[string]ProbeResult{}
	var mu sync.Mutex
	for _, s := range []siloInfo{a, c} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := probe(t, s, "probe-db", checks)
			mu.Lock()
			results[s.path] = r
			mu.Unlock()
		}()
	}
	wg.Wait()
	for path, r := range results {
		t.Run("From"+strings.ReplaceAll(path, "/", "_"), func(t *testing.T) {
			for _, ch := range checks {
				got, ok := r[ch.ID]
				switch {
				case !ok:
					t.Errorf("%s: no result", ch.ID)
				case ch.Kind == "dns" && !got.OK:
					t.Errorf("the endpoint's DNS name doesn't resolve from %s: %s", path, got.Detail)
				case ch.Kind != "dns" && got.OK:
					t.Errorf("%s reached the database endpoint (%s %s:%d): %s", path, ch.Kind, ch.Host, ch.Port, got.Detail)
				}
			}
		})
	}
	t.Run("TenantSubnetOverControlRangeRefused", func(t *testing.T) {
		p := "projects/" + a.project + "/regions/" + run.region + "/subnetworks"
		refused(t, 400, "POST", p, map[string]any{"name": a.id + "-x-ctl", "network": "default", "ipCidrRange": run.control.String()}, p+"/"+a.id+"-x-ctl")
	})
	t.Run("SilodHealthyThroughout", func(t *testing.T) {
		select {
		case err := <-healthy:
			if err != nil {
				t.Errorf("silod stopped answering during the case: %v", err)
			}
		case <-time.After(15 * time.Minute):
			t.Error("the health watch never finished")
		}
	})
}
