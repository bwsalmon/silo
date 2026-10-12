package gce

import (
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/store"
)

// Tests of instance templates (gcp-backend.md, "Instance templates",
// SV-70; testing.md, "Template validation" and the templates' bullets
// of "Quota pricing").

// newTemplateEnv is newComputeEnv with instance_templates granted:
// team-a n, team-b 2.
func newTemplateEnv(t *testing.T, n int64) *computeEnv {
	t.Helper()
	e := newComputeEnv(t)
	k := ledger.Key{Kind: "gce", Class: "instance_templates", Region: ledger.Global}
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		if err := ledger.SetCapacity(ctx, tx, e.root, []ledger.Amount{{Key: k, Amount: ledger.Units(20)}}); err != nil {
			return err
		}
		if err := ledger.Grant(ctx, tx, e.a, []ledger.Amount{{Key: k, Amount: ledger.Units(n)}}); err != nil {
			return err
		}
		return ledger.Grant(ctx, tx, e.b, []ledger.Amount{{Key: k, Amount: ledger.Units(2)}})
	}); err != nil {
		t.Fatal(err)
	}
	return e
}

// tpl is a template body: machineType, on the silo's subnet web (the
// silo whose prefix name carries), with a pd-balanced boot disk from
// debian-12, and extra over its properties.
func tpl(name, machineType string, extra obj) obj {
	props := obj{"machineType": machineType,
		"networkInterfaces": []obj{{"subnetwork": "regions/us-central1/subnetworks/" + prefixOf(name) + "web"}},
		"disks":             []obj{{"boot": true, "autoDelete": true, "initializeParams": obj{"sourceImage": debian, "diskType": "pd-balanced"}}}}
	for k, v := range extra {
		props[k] = v
	}
	return obj{"name": name, "properties": props}
}

const templatesPath = "global/instanceTemplates"

func fromTemplate(src string) string { return instancesPath + "?sourceInstanceTemplate=" + src }

func (e *computeEnv) templates(silo string) int64 {
	e.t.Helper()
	return e.used(silo, "instance_templates", ledger.Global)
}

func sameCharges(got, want map[string]int64) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func TestTemplatePricing(t *testing.T) {
	e := newTemplateEnv(t, 2)
	body := tpl(e.a+"-t", "e2-medium", obj{"labels": obj{"env": "dev"}, "tags": obj{"items": []string{e.a + "-web"}},
		"metadata": obj{"items": []obj{{"key": "startup-script", "value": "echo"}}}})

	// A template charges one instance_templates and nothing regional.
	if got := e.preview(e.a, "POST", cv(host, templatesPath), body); !sameCharges(got, map[string]int64{"instance_templates": 1}) {
		t.Errorf("template preview: %v", got)
	}
	e.ok(e.a, "POST", cv(host, templatesPath), body)
	if got := e.templates(e.a); got != 1 {
		t.Errorf("instance_templates used %d, want 1", got)
	}
	e.want("after the template", e.a, map[string]int64{"vcpu": 0, "memory_mb": 0, "instances": 0, "ssd_disk_gb": 0})
	e.run()

	// The real template is rewritten as an instance would be.
	real, err := e.gce.GetInstanceTemplate(ctx, host, "", e.a+"-t")
	if err != nil {
		t.Fatal(err)
	}
	p := real.Properties
	if !strings.HasPrefix(real.Description, "silo:"+e.a) || p.Labels["silo"] != e.a || p.Labels["silo-managed"] != "true" || p.Labels["env"] != "dev" {
		t.Errorf("real template: description %q labels %v", real.Description, p.Labels)
	}
	if strings.Join(p.Tags.Items, ",") != e.a+","+e.a+"-web" || len(p.ServiceAccounts) != 0 || p.MachineType != "e2-medium" {
		t.Errorf("real template: tags %v service accounts %v machine type %s", p.Tags.Items, p.ServiceAccounts, p.MachineType)
	}
	md := map[string]string{}
	for _, it := range p.Metadata.Items {
		md[it.Key] = *it.Value
	}
	if md["block-project-ssh-keys"] != "true" || md["startup-script"] != "echo" {
		t.Errorf("real template metadata: %v", md)
	}
	if sn := p.NetworkInterfaces[0].Subnetwork; !strings.HasSuffix(sn, "/subnetworks/"+e.a+"-web") {
		t.Errorf("real template subnetwork: %s", sn)
	}

	// ... and mapped back.
	got := e.ok(e.a, "GET", cv(host, templatesPath+"/"+e.a+"-t"), nil)
	gp := got["properties"].(obj)
	if got["name"] != e.a+"-t" || got["description"] != nil && got["description"] != "" {
		t.Errorf("get: %v", got)
	}
	if l := gp["labels"].(obj); len(l) != 1 || l["env"] != "dev" {
		t.Errorf("labels shown: %v", l)
	}
	if tags := strs(gp["tags"].(obj)["items"]); len(tags) != 1 || tags[0] != e.a+"-web" {
		t.Errorf("tags shown: %v", tags)
	}
	nic := gp["networkInterfaces"].([]any)[0].(obj)
	if nic["subnetwork"] != DefaultEndpoint+"projects/silo-"+e.a+"/regions/us-central1/subnetworks/"+e.a+"-web" {
		t.Errorf("nic shown: %v", nic)
	}
	if got["selfLink"] != DefaultEndpoint+"projects/silo-"+e.a+"/global/instanceTemplates/"+e.a+"-t" {
		t.Errorf("selfLink: %v", got["selfLink"])
	}
	if l := items(e.ok(e.a, "GET", cv(host, templatesPath), nil)); len(l) != 1 || l[0]["name"] != e.a+"-t" {
		t.Errorf("list: %v", l)
	}

	// An instance from it charges what the effective instance costs,
	// the same as the instance written out whole.
	whole := vm(e.a+"-x", "e2-medium", obj{"labels": obj{"env": "dev"}})
	want := e.preview(e.a, "POST", cv(host, instancesPath), whole)
	if got := e.preview(e.a, "POST", cv(host, fromTemplate("global/instanceTemplates/"+e.a+"-t")), obj{"name": e.a + "-x"}); !sameCharges(got, want) {
		t.Errorf("instance from template preview: %v, want %v", got, want)
	}
	e.ok(e.a, "POST", cv(host, fromTemplate("global/instanceTemplates/"+e.a+"-t")), obj{"name": e.a + "-vm1"})
	e.want("instance from template", e.a, map[string]int64{"vcpu": 2, "memory_mb": 4096, "instances": 1, "ssd_disk_gb": 10})
	// The body wins over the template: a bigger machine, and metadata
	// merged by key.
	big := obj{"name": e.a + "-vm2", "machineType": "zones/" + zoneA + "/machineTypes/e2-standard-4",
		"metadata": obj{"items": []obj{{"key": "extra", "value": "1"}}}}
	if got := e.preview(e.a, "POST", cv(host, fromTemplate("global/instanceTemplates/"+e.a+"-t")), big); got["vcpu"] != 4 || got["memory_mb"] != 16384 {
		t.Errorf("overridden machine type preview: %v", got)
	}
	e.ok(e.a, "POST", cv(host, fromTemplate(DefaultEndpoint+"projects/silo-"+e.a+"/global/instanceTemplates/"+e.a+"-t")), big)
	e.want("second instance", e.a, map[string]int64{"vcpu": 6, "memory_mb": 20480, "instances": 2, "ssd_disk_gb": 20})
	e.run()
	inst, err := e.gce.GetInstance(ctx, host, zoneA, e.a+"-vm2")
	if err != nil {
		t.Fatal(err)
	}
	md = map[string]string{}
	for _, it := range inst.Metadata.Items {
		md[it.Key] = *it.Value
	}
	if md["startup-script"] != "echo" || md["extra"] != "1" || md["block-project-ssh-keys"] != "true" {
		t.Errorf("merged metadata: %v", md)
	}
	if inst.Labels["silo"] != e.a || inst.Labels["env"] != "dev" || strings.Join(inst.Tags.Items, ",") != e.a+","+e.a+"-web" {
		t.Errorf("instance from template: labels %v tags %v", inst.Labels, inst.Tags.Items)
	}
	if !strings.HasSuffix(inst.MachineType, "/e2-standard-4") || !strings.HasSuffix(inst.Disks[0].Source, "/disks/"+e.a+"-vm2") || len(inst.ServiceAccounts) != 0 {
		t.Errorf("instance from template: machine type %s, disk %s, service accounts %v", inst.MachineType, inst.Disks[0].Source, inst.ServiceAccounts)
	}

	// A regional template counts in the same global count.
	e.ok(e.a, "POST", cv(host, "regions/us-central1/instanceTemplates"), tpl(e.a+"-r", "e2-medium", nil))
	if got := e.templates(e.a); got != 2 {
		t.Errorf("instance_templates used %d, want 2", got)
	}
	msg := e.fail(403, "quotaExceeded", e.a, "POST", cv(host, templatesPath), tpl(e.a+"-t3", "e2-medium", nil))
	if !strings.Contains(msg, "instance_templates") {
		t.Errorf("quota message: %q", msg)
	}
	e.run()
	if l := items(e.ok(e.a, "GET", cv(host, "regions/us-central1/instanceTemplates"), nil)); len(l) != 1 || l[0]["name"] != e.a+"-r" {
		t.Errorf("regional list: %v", l)
	}
	if l := items(e.ok(e.a, "GET", cv(host, templatesPath), nil)); len(l) != 1 {
		t.Errorf("global list holds the regional template: %v", l)
	}
	e.ok(e.a, "POST", cv(host, "zones/"+zoneA+"/instances?sourceInstanceTemplate=regions/us-central1/instanceTemplates/"+e.a+"-r"), obj{"name": e.a + "-vm3"})
	e.want("instance from a regional template", e.a, map[string]int64{"vcpu": 8, "instances": 3})

	// A delete gives the count back once it has worked; the instances
	// made from it stay.
	e.ok(e.a, "DELETE", cv(host, templatesPath+"/"+e.a+"-t"), nil)
	if got := e.templates(e.a); got != 2 {
		t.Errorf("before the delete finishes: %d", got)
	}
	e.run()
	if got := e.templates(e.a); got != 1 {
		t.Errorf("after the delete: %d", got)
	}
	e.want("after the delete", e.a, map[string]int64{"vcpu": 8, "instances": 3})
	e.fail(404, "notFound", e.a, "GET", cv(host, templatesPath+"/"+e.a+"-t"), nil)
	e.fail(404, "notFound", e.a, "POST", cv(host, fromTemplate("global/instanceTemplates/"+e.a+"-t")), obj{"name": e.a + "-vm4"})
}

func TestTemplateValidation(t *testing.T) {
	e := newTemplateEnv(t, 10)
	// team-b's own image and disk.
	e.ok(e.b, "POST", cv(host, "global/images"), obj{"name": e.b + "-img", "sourceImage": debian})
	e.ok(e.b, "POST", cv(host, "zones/"+zoneA+"/disks"), obj{"name": e.b + "-d", "sizeGb": "10"})
	e.run()
	silob := "projects/silo-" + e.b + "/"
	hostp := "projects/" + host + "/"
	boot := func(src obj) obj {
		return obj{"disks": []obj{{"boot": true, "initializeParams": src}}}
	}

	// Another silo's subnetwork, image or disk, by any name form.
	for _, extra := range []obj{
		{"networkInterfaces": []obj{{"subnetwork": silob + "regions/us-central1/subnetworks/" + e.b + "-web"}}},
		{"networkInterfaces": []obj{{"subnetwork": "https://www.googleapis.com/compute/v1/" + silob + "regions/us-central1/subnetworks/" + e.b + "-web"}}},
		{"networkInterfaces": []obj{{"subnetwork": hostp + "regions/us-central1/subnetworks/" + e.b + "-web"}}},
		{"networkInterfaces": []obj{{"subnetwork": "regions/us-central1/subnetworks/" + e.b + "-web"}}},
		boot(obj{"sourceImage": silob + "global/images/" + e.b + "-img"}),
		boot(obj{"sourceImage": hostp + "global/images/" + e.b + "-img"}),
		boot(obj{"sourceImage": "global/images/" + e.b + "-img"}),
		{"disks": []obj{{"boot": true, "source": silob + "zones/" + zoneA + "/disks/" + e.b + "-d"}}},
		{"disks": []obj{{"boot": true, "source": "zones/" + zoneA + "/disks/" + e.b + "-d"}}},
		{"disks": []obj{{"boot": true, "source": hostp + "zones/" + zoneA + "/disks/" + e.b + "-d"}}},
	} {
		// 403 for team-b's project, 400 for team-b's prefix in team-a's.
		if code, out := e.do(e.a, "POST", cv(host, templatesPath), tpl(e.a+"-x", "e2-medium", extra)); code != 400 && code != 403 {
			t.Errorf("%v: %d %v, want 400 or 403", extra, code, out)
		}
	}

	// A refused field or metadata key, named.
	for _, c := range []struct {
		body  obj
		names string
	}{
		{tpl(e.a+"-x", "e2-medium", obj{"canIpForward": true}), "properties.canIpForward"},
		{tpl(e.a+"-x", "e2-medium", obj{"serviceAccounts": []obj{{"email": "x@example.com"}}}), "properties.serviceAccounts"},
		{tpl(e.a+"-x", "e2-medium", obj{"networkInterfaces": []obj{{"subnetwork": "regions/us-central1/subnetworks/" + e.a + "-web", "accessConfigs": []obj{{"name": "nat"}}}}}), "accessConfigs"},
		{tpl(e.a+"-x", "e2-medium", obj{"scheduling": obj{"nodeAffinities": []obj{{"key": "k"}}}}), "nodeAffinities"},
		{tpl(e.a+"-x", "e2-medium", obj{"metadata": obj{"items": []obj{{"key": "enable-oslogin", "value": "TRUE"}}}}), "enable-oslogin"},
		{tpl(e.a+"-x", "e2-medium", obj{"metadata": obj{"items": []obj{{"key": "block-project-ssh-keys", "value": "false"}}}}), "block-project-ssh-keys"},
		{tpl(e.a+"-x", "e2-medium", obj{"reservationAffinity": obj{"consumeReservationType": "ANY_RESERVATION"}}), "reservationAffinity"},
		{tpl(e.a+"-x", "e2-medium", obj{"labels": obj{"silo": "x"}}), "silo"},
		{tpl(e.a+"-x", "e2-medium", boot(obj{"sourceImage": debian, "storagePool": "p"})), "storagePool"},
		{obj{"name": e.a + "-x", "sourceInstance": "zones/" + zoneA + "/instances/" + e.a + "-vm"}, "sourceInstance"},
		{obj{"name": e.a + "-x", "sourceInstanceParams": obj{"diskConfigs": []obj{{"deviceName": "d"}}}}, "sourceInstanceParams"},
	} {
		msg := e.fail(403, "forbidden", e.a, "POST", cv(host, templatesPath), c.body)
		if !strings.Contains(msg, c.names) {
			t.Errorf("%v: %q doesn't name %s", c.body, msg, c.names)
		}
	}

	// Regions: a regional template's, and every one a global template
	// names, must be allowed.
	e.fail(403, "regionNotAllowed", e.a, "POST", cv(host, "regions/us-east1/instanceTemplates"), tpl(e.a+"-x", "e2-medium", obj{"networkInterfaces": []obj{{}}}))
	e.fail(403, "regionNotAllowed", e.a, "POST", cv(host, templatesPath), tpl(e.a+"-x", "e2-medium", obj{"networkInterfaces": []obj{{"subnetwork": "regions/us-east1/subnetworks/" + e.a + "-web"}}}))
	e.fail(403, "regionNotAllowed", e.a, "POST", cv(host, templatesPath), tpl(e.a+"-x", "e2-medium", obj{"disks": []obj{{"boot": true, "source": "zones/us-east1-b/disks/" + e.a + "-d"}}}))
	// A regional template's subnetwork is in its region.
	e.fail(400, "invalid", e.b, "POST", cv(host, "regions/us-east1/instanceTemplates"), tpl(e.b+"-x", "e2-medium", nil))

	// GPUs, local SSD and disk types, against the machine type.
	for _, extra := range []obj{
		{"guestAccelerators": []obj{{"acceleratorType": "nvidia-tesla-t4", "acceleratorCount": 1}}, "scheduling": obj{"onHostMaintenance": "TERMINATE"}},
		{"machineType": "n1-standard-4", "guestAccelerators": []obj{{"acceleratorType": "nvidia-tesla-t4", "acceleratorCount": 1}}},
		{"machineType": "n1-standard-4", "guestAccelerators": []obj{{"acceleratorType": "nvidia-tesla-t4", "acceleratorCount": 3}}, "scheduling": obj{"onHostMaintenance": "TERMINATE"}},
		boot(obj{"sourceImage": debian, "diskType": "hyperdisk-balanced"}),
		{"machineType": "c3-standard-4-lssd", "disks": []obj{
			{"boot": true, "initializeParams": obj{"sourceImage": debian, "diskType": "hyperdisk-balanced"}},
			{"type": "SCRATCH", "interface": "NVME", "initializeParams": obj{"diskType": "local-ssd"}}}},
		{"machineType": "no-such-type"},
	} {
		body := tpl(e.a+"-x", "e2-medium", extra)
		if code, out := e.do(e.a, "POST", cv(host, templatesPath), body); code != 400 && code != 404 {
			t.Errorf("%v: %d %v, want 400 or 404", extra, code, out)
		}
	}
	if got := e.templates(e.a); got != 0 {
		t.Fatalf("refused templates charged %d", got)
	}

	// What passes: a GPU offered in some zone of the region, a global
	// template that leaves its subnetwork to the instance, the silo's own
	// disk, a regional template.
	e.ok(e.a, "POST", cv(host, templatesPath), tpl(e.a+"-gpu", "e2-medium", obj{"machineType": "n1-standard-4",
		"guestAccelerators": []obj{{"acceleratorType": "nvidia-tesla-v100", "acceleratorCount": 1}}, "scheduling": obj{"onHostMaintenance": "TERMINATE"}}))
	e.ok(e.a, "POST", cv(host, templatesPath), tpl(e.a+"-nosubnet", "e2-medium", obj{"networkInterfaces": []obj{{"network": "global/networks/default"}}}))
	e.ok(e.a, "POST", cv(host, "zones/"+zoneA+"/disks"), obj{"name": e.a + "-data", "sizeGb": "10"})
	e.run()
	e.ok(e.a, "POST", cv(host, templatesPath), tpl(e.a+"-own", "e2-medium", obj{"disks": []obj{
		{"boot": true, "initializeParams": obj{"sourceImage": debian}}, {"source": "zones/" + zoneA + "/disks/" + e.a + "-data"}}}))
	e.ok(e.a, "POST", cv(host, "regions/us-central1/instanceTemplates"), tpl(e.a+"-regional", "e2-medium", obj{"networkInterfaces": []obj{{}}}))
	e.fail(409, "alreadyExists", e.a, "POST", cv(host, templatesPath), tpl(e.a+"-own", "e2-medium", nil))
	if got := e.templates(e.a); got != 4 {
		t.Errorf("instance_templates used %d, want 4", got)
	}
	e.run()
	real, err := e.gce.GetInstanceTemplate(ctx, host, "", e.a+"-own")
	if err != nil {
		t.Fatal(err)
	}
	if d := real.Properties.Disks[1]; d.Source != e.a+"-data" {
		t.Errorf("own disk source: %q", d.Source)
	}
	got := e.ok(e.a, "GET", cv(host, templatesPath+"/"+e.a+"-own"), nil)
	if d := got["properties"].(obj)["disks"].([]any)[1].(obj); d["source"] != e.a+"-data" {
		t.Errorf("own disk shown: %v", d)
	}
	// An instance from the global template with no subnetwork gets the
	// silo's one in the zone's region.
	e.ok(e.a, "POST", cv(host, fromTemplate("global/instanceTemplates/"+e.a+"-nosubnet")), obj{"name": e.a + "-ns"})
	e.run()
	inst, err := e.gce.GetInstance(ctx, host, zoneA, e.a+"-ns")
	if err != nil {
		t.Fatal(err)
	}
	if sn := inst.NetworkInterfaces[0].Subnetwork; !strings.HasSuffix(sn, "/subnetworks/"+e.a+"-web") {
		t.Errorf("instance from a template with no subnetwork: %s", sn)
	}
}

func TestTemplateRecheck(t *testing.T) {
	e := newTemplateEnv(t, 10)
	tmp := freeIn(e.blocks(e.a)[0], 24, e.blocks(e.ci)[0], e.blocks(e.sealed)[0], e.webA, e.pods)
	e.ok(e.a, "POST", cv(host, "regions/us-central1/subnetworks"), obj{"name": e.a + "-tmp", "ipCidrRange": tmp.String()})
	e.run()
	e.ok(e.a, "POST", cv(host, templatesPath), tpl(e.a+"-ontmp", "e2-medium", obj{"networkInterfaces": []obj{{"subnetwork": "regions/us-central1/subnetworks/" + e.a + "-tmp"}}}))
	e.ok(e.a, "POST", cv(host, templatesPath), tpl(e.a+"-ok", "e2-medium", nil))
	e.run()

	// The template's subnet is deleted: an instance from it is refused.
	e.ok(e.a, "DELETE", cv(host, "regions/us-central1/subnetworks/"+e.a+"-tmp"), nil)
	e.run()
	e.fail(404, "notFound", e.a, "POST", cv(host, fromTemplate("global/instanceTemplates/"+e.a+"-ontmp")), obj{"name": e.a + "-x"})

	// A body that adds a refused field over a valid template.
	for _, body := range []obj{
		{"name": e.a + "-x", "canIpForward": true},
		{"name": e.a + "-x", "serviceAccounts": []obj{{"email": "x@example.com"}}},
		{"name": e.a + "-x", "networkInterfaces": []obj{{"subnetwork": "regions/us-central1/subnetworks/" + e.a + "-web", "accessConfigs": []obj{{"name": "nat"}}}}},
		{"name": e.a + "-x", "metadata": obj{"items": []obj{{"key": "enable-oslogin", "value": "TRUE"}}}},
		{"name": e.a + "-x", "labels": obj{"silo": e.b}},
	} {
		e.fail(403, "forbidden", e.a, "POST", cv(host, fromTemplate("global/instanceTemplates/"+e.a+"-ok")), body)
	}
	// ... or another silo's subnetwork, or a GPU the machine type can't take.
	if code, out := e.do(e.a, "POST", cv(host, fromTemplate("global/instanceTemplates/"+e.a+"-ok")), obj{"name": e.a + "-x",
		"networkInterfaces": []obj{{"subnetwork": "projects/silo-" + e.b + "/regions/us-central1/subnetworks/web"}}}); code != 403 {
		t.Errorf("another silo's subnetwork over a template: %d %v", code, out)
	}
	e.fail(400, "invalid", e.a, "POST", cv(host, fromTemplate("global/instanceTemplates/"+e.a+"-ok")), obj{"name": e.a + "-x",
		"guestAccelerators": []obj{{"acceleratorType": "nvidia-tesla-t4", "acceleratorCount": 1}}})
	e.want("nothing charged", e.a, map[string]int64{"vcpu": 0, "instances": 0, "ssd_disk_gb": 0})

	// team-b can't use team-a's template, by any name form, nor see it.
	// Team-a's project is 404 to it; team-a's prefix in its own, 400.
	for src, code := range map[string]int{
		"global/instanceTemplates/ok":                                                                         400,
		"projects/silo-" + e.a + "/global/instanceTemplates/" + e.a + "-ok":                                   404,
		DefaultEndpoint + "projects/silo-" + e.a + "/global/instanceTemplates/" + e.a + "-ok":                 404,
		"projects/" + host + "/global/instanceTemplates/" + e.a + "-ok":                                       400,
		"https://www.googleapis.com/compute/v1/projects/" + host + "/global/instanceTemplates/" + e.a + "-ok": 400,
		"global/instanceTemplates/" + e.a + "-ok":                                                             400,
		"global/instanceTemplates/" + e.b + "-ok":                                                             404,
	} {
		e.fail(code, map[int]string{400: "invalid", 404: "notFound"}[code], e.b, "POST", cv(host, fromTemplate(src)), obj{"name": e.b + "-x"})
	}
	e.fail(404, "notFound", e.b, "GET", cv(host, templatesPath+"/"+e.b+"-ok"), nil)
	e.fail(404, "notFound", e.b, "DELETE", cv(host, templatesPath+"/"+e.b+"-ok"), nil)
	if l := items(e.ok(e.b, "GET", cv(host, templatesPath), nil)); len(l) != 0 {
		t.Errorf("team-b lists %v", l)
	}
	if got := e.used(e.b, "instances", central); got != 0 {
		t.Errorf("team-b charged %d instances", got)
	}

	// team-b's templates in us-east1 and global, then us-east1 is taken
	// from its allowed regions: an instance from either there is refused.
	e.ok(e.b, "POST", cv(host, "regions/us-east1/instanceTemplates"), tpl(e.b+"-east", "e2-medium", obj{"networkInterfaces": []obj{{}}}))
	e.ok(e.b, "POST", cv(host, templatesPath), tpl(e.b+"-g", "e2-medium", obj{"networkInterfaces": []obj{{}}}))
	e.run()
	if err := e.db.Tx(ctx, func(tx store.Tx) error { return ledger.SetRegions(ctx, tx, e.b, []string{central}) }); err != nil {
		t.Fatal(err)
	}
	e.fail(403, "regionNotAllowed", e.b, "POST", cv(host, "zones/us-east1-b/instances?sourceInstanceTemplate=regions/us-east1/instanceTemplates/"+e.b+"-east"), obj{"name": e.b + "-x"})
	e.fail(403, "regionNotAllowed", e.b, "POST", cv(host, "zones/us-east1-b/instances?sourceInstanceTemplate=global/instanceTemplates/"+e.b+"-g"), obj{"name": e.b + "-x"})
	// A regional template is only for its own region.
	e.fail(404, "notFound", e.b, "POST", cv(host, fromTemplate("regions/us-east1/instanceTemplates/"+e.b+"-east")), obj{"name": e.b + "-x"})
	e.ok(e.b, "POST", cv(host, fromTemplate("global/instanceTemplates/"+e.b+"-g")), obj{"name": e.b + "-y"})
	e.want("team-b's instance from its global template", e.b, map[string]int64{"vcpu": 2, "instances": 1})
}
