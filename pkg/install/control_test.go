package install_test

import (
	"bytes"
	"context"
	"net/netip"
	"regexp"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/agents"
	"github.com/bwsalmon/silo/pkg/install"
	"github.com/bwsalmon/silo/pkg/install/fakecloud"
)

// The gateway's container is cut off from the metadata server before the
// gateway runs, in a namespace of its own, and can't undo it; silod alone
// has the host's network.
func TestControlCutsTheGatewayOffTheMetadataServer(t *testing.T) {
	ud := install.ControlUserData(install.ControlParams{Project: project, Region: "us-central1",
		DBConnection: project + ":us-central1:silo-db-t3st", AddressSpace: netip.MustParsePrefix("10.0.0.0/8")})
	at := func(s string) int {
		t.Helper()
		i := strings.Index(ud, s)
		if i < 0 {
			t.Fatalf("user-data lacks %q:\n%s", s, ud)
		}
		return i
	}
	hold := at(`"$img" pause`)
	dns := at("ns iptables -w -A OUTPUT -d 169.254.169.254 -p udp --dport 53 -j ACCEPT")
	reject := at("ns iptables -w -A OUTPUT -d 169.254.169.254 -j REJECT")
	check := at("ns iptables -w -C OUTPUT -d 169.254.169.254 -j REJECT")
	gw := at(`"$img" gateway --silod-socket /run/silo/gateway.sock`)
	if !(hold < dns && dns < reject && reject < check && check < gw) {
		t.Errorf("the gateway's rules aren't in place, in order, before it starts: pause %d, dns %d, reject %d, check %d, gateway %d", hold, dns, reject, check, gw)
	}
	run := ud[strings.LastIndex(ud[:gw], "exec docker run"):gw]
	for _, want := range []string{"--network container:silo-gateway-net", "--user 2002:2000", "--cap-drop ALL", "no-new-privileges"} {
		if !strings.Contains(run, want) {
			t.Errorf("the gateway's container lacks %s: %s", want, run)
		}
	}
	silod := ud[at("exec docker run --rm --name silod"):]
	silod = silod[:strings.Index(silod, "\n- path:")]
	for _, want := range []string{"--network host", "--user 2001:2001", "--group-add 2000", "'--gateway-socket-group' '2000'",
		"'--front-door-listen' ':8443'", "'--db-instance' '" + project + ":us-central1:silo-db-t3st'", "--cap-drop ALL"} {
		if !strings.Contains(silod, want) {
			t.Errorf("silod's container lacks %s: %s", want, silod)
		}
	}
	// The gateway's network stays out of --address-space.
	for _, space := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		n, ip := install.GatewayNetwork(netip.MustParsePrefix(space))
		if n.Overlaps(netip.MustParsePrefix(space)) || !n.Contains(ip) {
			t.Errorf("--address-space %s: gateway network %s, address %s", space, n, ip)
		}
	}
}

func TestControlVM(t *testing.T) {
	ctx := context.Background()
	f := fakecloud.New(project)
	if err := install.Apply(ctx, steps(t, f), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	zone, err := f.Cloud().ComputeExtras.InstanceZone(ctx, project, "us-central1", install.ControlVM)
	if err != nil || zone == "" {
		t.Fatalf("no %s: %v", install.ControlVM, err)
	}
	vm, err := f.Compute.GetInstance(ctx, project, zone, install.ControlVM)
	if err != nil {
		t.Fatal(err)
	}
	addr := f.Addresses["us-central1/"+install.GatewayAddress].Address
	ni := vm.NetworkInterfaces[0]
	if ni.NetworkIP != addr || len(ni.AccessConfigs) != 0 || !strings.HasSuffix(ni.Subnetwork, "/"+install.GatewaySubnetName) {
		t.Errorf("network interface %+v, want %s in %s with no external IP", ni, addr, install.GatewaySubnetName)
	}
	if len(vm.Disks) != 1 || !vm.Disks[0].Boot {
		t.Errorf("disks %+v: want the boot disk only", vm.Disks)
	}
	if vm.ServiceAccounts[0].Email != install.SilodEmail(project) {
		t.Errorf("service account %s", vm.ServiceAccounts[0].Email)
	}
	md := map[string]string{}
	for _, it := range vm.Metadata.Items {
		md[it.Key] = *it.Value
	}
	want := install.DeployedImage(&install.Config{Project: project, Regions: []string{"us-central1"}}, fakecloud.ReleaseDigest("v1.0.0"))
	if md[install.ImageKey] != want || !strings.HasPrefix(md["user-data"], "#cloud-config") || md["block-project-ssh-keys"] != "TRUE" {
		t.Errorf("metadata %v, want image %s", md, want)
	}
	if !f.Images[want] {
		t.Errorf("the image step didn't copy %s: %v", want, f.Images)
	}
}

// A newer --version copies its digest, and moves the control VM's
// silo-image to it without a reset.
func TestControlUpgrade(t *testing.T) {
	ctx := context.Background()
	f := fakecloud.New(project)
	if err := install.Apply(ctx, steps(t, f), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	st := install.Steps(newEnv(config(t, append(append([]string{}, baseArgs...), "--version", "v1.1.0")...), f))
	out := render(install.PlanAll(ctx, st))
	short := fakecloud.ReleaseDigest("v1.1.0")[:len("sha256:")+12]
	for _, want := range []string{"image             apply   silod@" + short + " copy from ghcr.io/bwsalmon/silod",
		"control           update  silo-control silod@" + short + ", not silod@", "(restarts its containers)", "root              ok      / exists"} {
		if !strings.Contains(out, want) {
			t.Errorf("upgrade plan lacks %q:\n%s", want, out)
		}
	}
	if err := install.Apply(ctx, st, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	zone, _ := f.Cloud().ComputeExtras.InstanceZone(ctx, project, "us-central1", install.ControlVM)
	vm, _ := f.Compute.GetInstance(ctx, project, zone, install.ControlVM)
	for _, it := range vm.Metadata.Items {
		if it.Key == install.ImageKey && !strings.HasSuffix(*it.Value, fakecloud.ReleaseDigest("v1.1.0")) {
			t.Errorf("silo-image is %s after the upgrade", *it.Value)
		}
	}
	for _, p := range install.PlanAll(ctx, st) {
		if p.Changing() {
			t.Errorf("after the upgrade, %s plans %v", p.Step, p.Changes)
		}
	}
}

func TestImageNotFound(t *testing.T) {
	ctx := context.Background()
	f := fakecloud.New(project)
	f.MissingReleases["v1.0.0"] = true
	out := render(install.PlanAll(ctx, steps(t, f)))
	if !strings.Contains(out, "image             fail    ghcr.io/bwsalmon/silod:v1.0.0 resolving") || !strings.Contains(out, "--image") {
		t.Errorf("plan:\n%s", out)
	}
	// --image skips the release registry.
	img := "europe-docker.pkg.dev/dev/silo/silod@" + fakecloud.ReleaseDigest("dev")
	out = render(install.PlanAll(ctx, install.Steps(newEnv(config(t, append(append([]string{}, baseArgs...), "--image", img)...), f))))
	if !strings.Contains(out, "copy from europe-docker.pkg.dev/dev/silo/silod") {
		t.Errorf("--image plan:\n%s", out)
	}
}

// The root step bootstraps once, with every flag that shapes the root;
// an --admin added later is mapped through the members API.
func TestRootBootstrap(t *testing.T) {
	ctx := context.Background()
	f := fakecloud.New(project)
	args := append(append([]string{}, baseArgs...), "--egress-allow", "pypi.org", "--root-grant-policy", "exports=deny,links=allow",
		"--root-memory", "64GB", "--root-model-budget", "12.5", "--budget-period", "week", "--agent-sessions", "2")
	if err := install.Apply(ctx, install.Steps(newEnv(config(t, args...), f)), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	r := f.Root
	if r == nil {
		t.Fatal("no root")
	}
	if r.AddressSpace != "10.0.0.0/8" || r.GatewayRange != "10.255.255.0/24" || r.ControlRange != "10.255.254.0/24" ||
		strings.Join(r.Regions, ",") != "us-central1,europe-west4" || strings.Join(r.Admins, ",") != "user:admin@example.com" ||
		strings.Join(r.Egress.Allow, ",") != "pypi.org" || r.GrantPolicy["exports"] != "deny" || r.GrantPolicy["links"] != "allow" ||
		r.ModelBudgetUSD != "12.5" || r.BudgetPeriod != "week" || r.Capacity["global"]["agent_sessions"] != "2" ||
		r.Capacity["us-central1"]["vcpu"] == "" || r.Capacity["europe-west4"]["memory_mb"] != "65536" {
		t.Errorf("bootstrap request %+v", r)
	}
	// A second run doesn't bootstrap again, and maps a new --admin.
	f.Root.Name = "first"
	args = append(args, "--admin", "serviceAccount:ci@"+project+".iam.gserviceaccount.com")
	st := install.Steps(newEnv(config(t, args...), f))
	out := render(install.PlanAll(ctx, st))
	if !strings.Contains(out, "admin             create  serviceAccount:ci@"+project+".iam.gserviceaccount.com -> /") {
		t.Errorf("plan:\n%s", out)
	}
	if err := install.Apply(ctx, st, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if f.Root.Name != "first" || len(f.Members) != 2 {
		t.Errorf("root %q, members %v", f.Root.Name, f.Members)
	}
}

func TestHeldBack(t *testing.T) {
	for _, c := range []struct{ gw, ctl, want string }{
		{"10.255.255.0/24", "10.255.254.0/24", "10.255.254.0/23"},
		{"10.255.253.0/24", "10.255.254.0/24", "10.255.253.0/24 10.255.254.0/24"},
		{"10.255.255.0/24", "10.255.254.0/25", "10.255.254.0/25 10.255.255.0/24"},
	} {
		cfg := install.Config{GatewayRange: netip.MustParsePrefix(c.gw), ControlRange: netip.MustParsePrefix(c.ctl)}
		var got []string
		for _, p := range install.HeldBack(&cfg) {
			got = append(got, p.String())
		}
		if strings.Join(got, " ") != c.want {
			t.Errorf("%s, %s: got %v, want %s", c.gw, c.ctl, got, c.want)
		}
	}
}

// The whole install's --dry-run against a fully faked project (every
// API on, quotas read) prints install.md's "Plan output": a row per
// step in install.md's order, in its shapes.
func TestDryRunPlanMatchesInstallMD(t *testing.T) {
	ctx := context.Background()
	f := fakecloud.New(project)
	f.EnableAll()
	f.Compute.SetQuota("us-central1", "NVIDIA_L4_GPUS", 0)
	st := steps(t, f)
	var b bytes.Buffer
	if err := install.RenderTable(&b, "silo install: project "+project+", home region us-central1", install.PlanAll(ctx, st)); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	// install.md's plan, with this project's names and amounts.
	want := []string{
		`^silo install: project silo-test-project, home region us-central1$`,
		`^  prereqs           ok      `,
		`^  db-instance       create  silo-db-\S+ POSTGRES_17 db-g1-small zonal us-central1`,
		`^  service-account   create  silod@silo-test-project\.iam\.gserviceaccount\.com$`,
		`^  role              create  projects/silo-test-project/roles/silod \(\d+ permissions\)$`,
		`^  network           create  silo \(custom, global routing, firewall policy first\)$`,
		`^  gateway-subnet    create  silo-gateway 10\.255\.255\.0/24 us-central1`,
		`^  control-subnet    create  silo-ctl 10\.255\.254\.0/24 us-central1`,
		`^  nat               create  `,
		`^  fence             create  silo-fence`,
		`^  address           create  silo-gateway-ip`,
		`^  dns               create  silo-internal`,
		`^  image             create  silo docker repository in us-central1$`,
		`^  db                create  database silo, IAM user silod@silo-test-project\.iam$`,
		`^  db-endpoint       create  silo-db-psc`,
		`^  migrate           apply   schema 0 -> \d+$`,
		`^  control           create  silo-control e2-standard-2 cos-stable`,
		`^  root              create  / blocks 10\.0\.0\.0/8 \(minus 10\.255\.254\.0/23\)$`,
		`^                            us-central1 vcpu \d+, ssd_disk_gb \d+, local_ssd_gb \d+,`,
		`nvidia_l4_gpus 0 \(no\s+quota\)`,
		`;\s+europe-west4 vcpu \d+,`,
		`^  admin             create  user:admin@example\.com -> /$`,
		`^  verify            apply   describe / and regions\.list through the front door$`,
		`^  20 steps: \d+ to create or apply, 0 to update, \d+ ok$`,
		`^  Estimated time: about \d+ minutes`,
	}
	pos := 0
	for _, w := range want {
		re := regexp.MustCompile("(?m)" + w)
		loc := re.FindStringIndex(out[pos:])
		if loc == nil {
			t.Fatalf("after offset %d, the plan lacks /%s/:\n%s", pos, w, out)
		}
		pos += loc[1]
	}
}

// TestControlRunsTheSessionLauncher: with --agent-sessions the control
// VM also runs silod sessiond, the one container with the runtime's
// socket, and silod reaches it on a socket of its own, without it.
func TestControlRunsTheSessionLauncher(t *testing.T) {
	p := install.ControlParams{Project: project, Region: "us-central1",
		DBConnection: project + ":us-central1:silo-db-t3st", AddressSpace: netip.MustParsePrefix("10.0.0.0/8")}
	if ud := install.ControlUserData(p); strings.Contains(ud, "sessiond") || strings.Contains(ud, "docker.sock") {
		t.Errorf("without agent sessions, the VM runs a session launcher:\n%s", ud)
	}
	p.AgentSessions, p.HarnessImage = 30, "us-central1-docker.pkg.dev/"+project+"/silo/silod-harness@sha256:abc"
	ud := install.ControlUserData(p)
	at := func(s string) string {
		t.Helper()
		i := strings.Index(ud, s)
		if i < 0 {
			t.Fatalf("user-data lacks %q:\n%s", s, ud)
		}
		rest := ud[i:]
		if j := strings.Index(rest, "\n- path:"); j > 0 {
			rest = rest[:j]
		}
		return rest
	}
	sd := at("exec docker run --rm --name silo-sessiond")
	for _, want := range []string{"--network host", "--cap-drop ALL", "--cap-add CHOWN", "no-new-privileges",
		"-v /var/run/docker.sock:/var/run/docker.sock", "--socket /run/silo/sessiond.sock", "--owner 2001", `--harness-image "$harness"`} {
		if !strings.Contains(sd, want) {
			t.Errorf("sessiond's container lacks %s: %s", want, sd)
		}
	}
	if !strings.Contains(ud, "iptables -w -A INPUT -i silo-egress -p tcp --dport 3128 -j ACCEPT") {
		t.Errorf("the egress proxy's port isn't open to the egress bridge")
	}
	if !strings.Contains(ud, "harness='"+p.HarnessImage+"'") {
		t.Errorf("the harness image isn't the deployed one")
	}
	silod := at("exec docker run --rm --name silod")
	if strings.Contains(silod, "docker.sock") {
		t.Errorf("silod holds the container runtime's socket: %s", silod)
	}
	for _, want := range []string{"'--sessiond-socket' '/run/silo/sessiond.sock'", "'--agent-sessions' '30'", "--user 2001:2001", "--cap-drop ALL"} {
		if !strings.Contains(silod, want) {
			t.Errorf("silod lacks %s: %s", want, silod)
		}
	}
	if !strings.Contains(ud, "systemctl start silo-sessiond.service silod.service") || !strings.Contains(ud, "systemctl restart silo-sessiond.service silod.service") {
		t.Errorf("sessiond isn't started and restarted with silod:\n%s", ud)
	}
}

// TestSessionSlotsAgree: the slots silod counts for itself are the
// per-VM capacity install's prerequisite check uses, capped at the
// fleet's --agent-sessions (the root's agent_sessions), so that the
// control VMs' slots add up to at least what install checked.
func TestSessionSlotsAgree(t *testing.T) {
	for _, c := range []struct {
		cpus, mem     int64
		sessions, per int
	}{
		{8, 32 << 10, 100, 35}, {8, 32 << 10, 12, 12}, {16, 64 << 10, 100, 75}, {2, 8 << 10, 10, 5}, {1, 4 << 10, 10, 0},
	} {
		if got := install.SessionsPerVM(c.cpus, c.mem); got != agents.SessionsPerVM(c.cpus, c.mem) {
			t.Errorf("install counts %d, silod %d", got, agents.SessionsPerVM(c.cpus, c.mem))
		}
		if got := agents.SessionSlots(c.sessions, c.cpus, c.mem); got != c.per {
			t.Errorf("%d vCPU, %d MiB, --agent-sessions %d: %d slots, want %d", c.cpus, c.mem, c.sessions, got, c.per)
		}
	}
}
