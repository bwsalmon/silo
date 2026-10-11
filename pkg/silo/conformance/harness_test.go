//go:build silo_conformance

package conformance

// The real-GCP harness (docs/silo/testing.md, "Where and how it runs",
// SV-121): silo install into a fresh --address-space, the cases, then
// silo uninstall --force --delete-data --yes and a check that no
// labelled object is left. TestMain uninstalls whatever happened in
// between; the workflow's step runs uninstall again from a shell trap,
// for the run that dies too hard for TestMain to get there (a panic, the
// go test timeout).

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/client"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

var (
	flagProject = flag.String("silo.project", envOr("SILO_CONFORMANCE_PROJECT", DefaultProject), "the host project")
	flagRegion  = flag.String("silo.region", envOr("SILO_CONFORMANCE_REGION", "us-central1"), "the home region")
	flagKey     = flag.String("silo.key", envOr("SILO_CONFORMANCE_KEY", "/home/debian/.gcp-service-account.json"), "the gcp-key capability's key")
	flagImage   = flag.String("silo.image", os.Getenv("SILO_CONFORMANCE_IMAGE"), "silod's image, by digest (install --image)")
	flagKeep    = flag.Bool("silo.keep", false, "don't uninstall at the end (debugging only)")
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// run is the whole run's state, which the cases read.
var run struct {
	project, region, zone string
	key                   string
	silo                  string // the silo CLI built from this tree
	space                 netip.Prefix
	gateway, control      netip.Prefix
	admin                 string
	installOut            string
	installErr            error
	migrateSeen           bool

	proxyAddr string
	api       *client.Client
	compute   *compute.Service
	sql       *sqladmin.Service

	vms chan struct{} // the cap on probe VMs at once
}

// installed says whether the cases have a silo to test, and skips the
// case with install's failure if not: a case can't pass, or fail, on a
// tree that never came up. TestInstall reports install's own failure.
func installed(t *testing.T) {
	t.Helper()
	if run.installErr != nil {
		t.Skipf("silo install failed, so there is no tree to test: %v", run.installErr)
	}
}

func TestMain(m *testing.M) {
	flag.Parse()
	code := func() int {
		ctx := context.Background()
		if err := setup(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "conformance: %v\n", err)
			return 2
		}
		run.installErr = install(ctx)
		code := m.Run()
		if *flagKeep {
			fmt.Fprintln(os.Stderr, "conformance: -silo.keep: leaving the install in place")
			return code
		}
		if err := uninstallAndCheck(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "--- FAIL: TestUninstallLeavesNothing\n    %v\n", err)
			return 1
		}
		fmt.Fprintln(os.Stderr, "--- PASS: TestUninstallLeavesNothing")
		return code
	}()
	stopProxy()
	os.Exit(code)
}

// setup checks the key, points gcloud and the SDKs at it, builds the CLI
// and picks a fresh address space.
func setup(ctx context.Context) error {
	if _, err := os.Stat(*flagKey); err != nil {
		return fmt.Errorf("no GCP key at %s: this suite needs the gcp-key capability (%v)", *flagKey, err)
	}
	run.project, run.region, run.zone, run.key = *flagProject, *flagRegion, *flagRegion+"-a", *flagKey
	os.Setenv("GOOGLE_APPLICATION_CREDENTIALS", run.key)
	os.Setenv("GOOGLE_CLOUD_PROJECT", run.project)
	os.Setenv("CLOUDSDK_CORE_PROJECT", run.project)
	if out, err := exec.Command("gcloud", "auth", "activate-service-account", "--key-file="+run.key).CombinedOutput(); err != nil {
		return fmt.Errorf("gcloud auth: %v: %s", err, out)
	}
	var k struct {
		ClientEmail string `json:"client_email"`
	}
	if b, err := os.ReadFile(run.key); err == nil && json.Unmarshal(b, &k) == nil {
		run.admin = "serviceAccount:" + k.ClientEmail
	}
	var err error
	if run.compute, err = compute.NewService(ctx, option.WithCredentialsFile(run.key)); err != nil {
		return err
	}
	if run.sql, err = sqladmin.NewService(ctx, option.WithCredentialsFile(run.key)); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "silo-conformance")
	if err != nil {
		return err
	}
	run.silo = filepath.Join(dir, "silo")
	if out, err := exec.Command("go", "build", "-o", run.silo, "github.com/bwsalmon/silo/cmd/silo").CombinedOutput(); err != nil {
		return fmt.Errorf("building silo: %v: %s", err, out)
	}
	var used []netip.Prefix
	err = run.compute.Subnetworks.AggregatedList(run.project).Pages(ctx, func(l *compute.SubnetworkAggregatedList) error {
		for _, sc := range l.Items {
			for _, s := range sc.Subnetworks {
				if p, err := netip.ParsePrefix(s.IpCidrRange); err == nil {
					used = append(used, p)
				}
				for _, r := range s.SecondaryIpRanges {
					if p, err := netip.ParsePrefix(r.IpCidrRange); err == nil {
						used = append(used, p)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("listing the project's subnets: %v", err)
	}
	if run.space, err = PickAddressSpace(used); err != nil {
		return err
	}
	run.gateway, run.control = Ranges(run.space)
	run.vms = make(chan struct{}, MaxVMs)
	return nil
}

// silo runs the CLI.
func silo(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, run.silo, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.MultiWriter(&out, os.Stderr), io.MultiWriter(&out, os.Stderr)
	err := cmd.Run()
	return out.String(), err
}

// install is the real install (SV-97), with the run's cost settings:
// the cheapest database tier and two control replicas, so that
// failover is tested.
func install(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Minute)
	defer cancel()
	args := []string{"install", "--project", run.project, "--regions", run.region,
		"--admin", run.admin, "--credentials", run.key,
		"--address-space", run.space.String(), "--gateway-range", run.gateway.String(),
		"--control-range", run.control.String(),
		"--db-tier", "db-f1-micro", "--control-replicas", "2", "--yes"}
	if *flagImage != "" {
		args = append(args, "--image", *flagImage)
	}
	stop := watchMigrateVM(ctx)
	out, err := silo(ctx, args...)
	stop()
	run.installOut = out
	if err != nil {
		return fmt.Errorf("silo install: %v", err)
	}
	return startProxy(ctx)
}

// watchMigrateVM polls for silo-migrate while install runs, to show the
// migration VM came (and, in TestInstall, went).
func watchMigrateVM(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			l, err := run.compute.Instances.AggregatedList(run.project).Filter(`name = "silo-migrate"`).Context(ctx).Do()
			if err == nil {
				for _, sc := range l.Items {
					if len(sc.Instances) > 0 {
						run.migrateSeen = true
					}
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()
	return func() { cancel(); wg.Wait() }
}

var proxyCmd *exec.Cmd

// startProxy serves the front door on localhost with silo proxy, so that
// the fence API and the facade are both a local URL.
func startProxy(ctx context.Context) error {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	run.proxyAddr = l.Addr().String()
	l.Close()
	proxyCmd = exec.Command(run.silo, "proxy", "--project", run.project, "--listen", run.proxyAddr)
	proxyCmd.Stdout, proxyCmd.Stderr = os.Stderr, os.Stderr
	if err := proxyCmd.Start(); err != nil {
		return err
	}
	if run.api, err = client.New(client.Options{Endpoint: "http://" + run.proxyAddr + "/silo/v1/", Timeout: 2 * time.Minute}); err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		_, err := run.api.Describe(ctx, "/", false)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the front door never answered: %v", err)
		}
		time.Sleep(3 * time.Second)
	}
}

func stopProxy() {
	if proxyCmd != nil && proxyCmd.Process != nil {
		proxyCmd.Process.Kill()
		proxyCmd.Wait()
	}
}

// uninstallAndCheck is the end of every run: uninstall everything, then
// look for anything labelled that is left.
func uninstallAndCheck(ctx context.Context) error {
	stopProxy()
	ctx, cancel := context.WithTimeout(ctx, 45*time.Minute)
	defer cancel()
	_, uerr := silo(ctx, "uninstall", "--project", run.project, "--credentials", run.key, "--force", "--delete-data", "--yes")
	left, lerr := labelledObjects(ctx)
	switch {
	case lerr != nil:
		return fmt.Errorf("listing what is left: %v (uninstall: %v)", lerr, uerr)
	case len(left) > 0:
		return fmt.Errorf("left behind after uninstall (uninstall: %v): %v", uerr, left)
	case uerr != nil:
		return fmt.Errorf("silo uninstall: %v", uerr)
	}
	return nil
}

// labelledObjects is every object in the project that Leftovers counts:
// install's and the tenants'.
func labelledObjects(ctx context.Context) ([]Object, error) {
	var objs []Object
	p := run.project
	add := func(kind, name string, labels map[string]string, desc string) {
		objs = append(objs, Object{Kind: kind, Name: name, Labels: labels, Description: desc})
	}
	if err := run.compute.Instances.AggregatedList(p).Pages(ctx, func(l *compute.InstanceAggregatedList) error {
		for _, sc := range l.Items {
			for _, o := range sc.Instances {
				add("instance", o.Name, o.Labels, o.Description)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := run.compute.Disks.AggregatedList(p).Pages(ctx, func(l *compute.DiskAggregatedList) error {
		for _, sc := range l.Items {
			for _, o := range sc.Disks {
				add("disk", o.Name, o.Labels, o.Description)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := run.compute.Subnetworks.AggregatedList(p).Pages(ctx, func(l *compute.SubnetworkAggregatedList) error {
		for _, sc := range l.Items {
			for _, o := range sc.Subnetworks {
				add("subnetwork", o.Name, nil, o.Description)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := run.compute.Addresses.AggregatedList(p).Pages(ctx, func(l *compute.AddressAggregatedList) error {
		for _, sc := range l.Items {
			for _, o := range sc.Addresses {
				add("address", o.Name, o.Labels, o.Description)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := run.compute.ForwardingRules.AggregatedList(p).Pages(ctx, func(l *compute.ForwardingRuleAggregatedList) error {
		for _, sc := range l.Items {
			for _, o := range sc.ForwardingRules {
				add("forwardingRule", o.Name, o.Labels, o.Description)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := run.compute.InstanceTemplates.List(p).Pages(ctx, func(l *compute.InstanceTemplateList) error {
		for _, o := range l.Items {
			var labels map[string]string
			if o.Properties != nil {
				labels = o.Properties.Labels
			}
			add("instanceTemplate", o.Name, labels, o.Description)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := run.compute.Images.List(p).Pages(ctx, func(l *compute.ImageList) error {
		for _, o := range l.Items {
			add("image", o.Name, o.Labels, o.Description)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := run.compute.Snapshots.List(p).Pages(ctx, func(l *compute.SnapshotList) error {
		for _, o := range l.Items {
			add("snapshot", o.Name, o.Labels, o.Description)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := run.compute.Firewalls.List(p).Pages(ctx, func(l *compute.FirewallList) error {
		for _, o := range l.Items {
			add("firewall", o.Name, nil, o.Description)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if l, err := run.compute.NetworkFirewallPolicies.List(p).Context(ctx).Do(); err != nil {
		return nil, err
	} else {
		for _, o := range l.Items {
			add("firewallPolicy", o.Name, nil, o.Description)
		}
	}
	if l, err := run.compute.Networks.List(p).Context(ctx).Do(); err != nil {
		return nil, err
	} else {
		for _, o := range l.Items {
			add("network", o.Name, nil, o.Description)
		}
	}
	if l, err := run.sql.Instances.List(p).Context(ctx).Do(); err != nil {
		return nil, fmt.Errorf("Cloud SQL: %v", err)
	} else {
		for _, o := range l.Items {
			var labels map[string]string
			if o.Settings != nil {
				labels = o.Settings.UserLabels
			}
			add("sqlInstance", o.Name, labels, "")
		}
	}
	return Leftovers(objs), nil
}

// TestInstall reports the install itself (SV-97): that it finished, used
// Cloud SQL with a PSC endpoint and IAM login as in production, and that
// the silo-migrate VM came and went.
func TestInstall(t *testing.T) {
	t.Run("Finished", func(t *testing.T) {
		if run.installErr != nil {
			t.Fatalf("%v\n%s", run.installErr, tail(run.installOut, 6000))
		}
	})
	t.Run("CloudSQLWithPSCAndIAMLogin", func(t *testing.T) {
		installed(t)
		ctx := context.Background()
		l, err := run.sql.Instances.List(run.project).Context(ctx).Do()
		if err != nil {
			t.Fatal(err)
		}
		var db *sqladmin.DatabaseInstance
		for _, o := range l.Items {
			if o.Settings != nil && o.Settings.UserLabels["silo-install"] != "" {
				db = o
			}
		}
		if db == nil {
			t.Fatal("no Cloud SQL instance labelled silo-install")
		}
		ip := db.Settings.IpConfiguration
		if ip == nil || ip.PscConfig == nil || !ip.PscConfig.PscEnabled {
			t.Errorf("%s: PSC isn't on", db.Name)
		}
		if ip != nil && ip.Ipv4Enabled {
			t.Errorf("%s has a public IP", db.Name)
		}
		iam := false
		for _, f := range db.Settings.DatabaseFlags {
			if f.Name == "cloudsql.iam_authentication" && f.Value == "on" {
				iam = true
			}
		}
		if !iam {
			t.Errorf("%s: cloudsql.iam_authentication isn't on", db.Name)
		}
		if db.Settings.Tier != "db-f1-micro" {
			t.Errorf("%s: tier %s, want db-f1-micro", db.Name, db.Settings.Tier)
		}
		users, err := run.sql.Users.List(run.project, db.Name).Context(ctx).Do()
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, u := range users.Items {
			if u.Type == "CLOUD_IAM_SERVICE_ACCOUNT" && strings.HasPrefix(u.Name, "silod@") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no IAM user for silod", db.Name)
		}
		if _, err := run.compute.ForwardingRules.Get(run.project, run.region, "silo-db-psc").Context(ctx).Do(); err != nil {
			t.Errorf("the PSC endpoint silo-db-psc: %v", err)
		}
	})
	t.Run("MigrateVMCameAndWent", func(t *testing.T) {
		installed(t)
		if !run.migrateSeen {
			t.Error("silo-migrate never appeared while install ran")
		}
		_, err := run.compute.Instances.Get(run.project, run.zone, "silo-migrate").Do()
		if err == nil {
			t.Error("silo-migrate is still there after install")
		}
	})
}

func tail(s string, n int) string {
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}

// facade calls the compute/v1 facade through the front door, as a root
// member: the caller may read and write every silo's facade project.
// It returns the HTTP status and the decoded body.
func facade(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://"+run.proxyAddr+"/compute/v1/"+strings.TrimPrefix(path, "/"), r)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	b, _ := io.ReadAll(resp.Body)
	json.Unmarshal(b, &out)
	if len(out) == 0 && len(b) > 0 {
		out["raw"] = string(b)
	}
	return resp.StatusCode, out
}

// waitOp waits for a facade operation to finish.
func waitOp(t *testing.T, project string, op map[string]any) map[string]any {
	t.Helper()
	name, _ := op["name"].(string)
	if name == "" {
		return op
	}
	scope := "global"
	if z, _ := op["zone"].(string); z != "" {
		scope = "zones/" + lastSegment(z)
	} else if rg, _ := op["region"].(string); rg != "" {
		scope = "regions/" + lastSegment(rg)
	}
	for i := 0; i < 60; i++ {
		_, o := facade(t, "POST", "projects/"+project+"/"+scope+"/operations/"+name+"/wait", nil)
		if o["status"] == "DONE" {
			return o
		}
	}
	t.Fatalf("operation %s never finished", name)
	return nil
}

func lastSegment(s string) string { return s[strings.LastIndex(s, "/")+1:] }
