//go:build silo_conformance

// Package gcpharness is the real-GCP conformance harness
// (docs/silo/testing.md, "Where and how it runs", SV-121): silo install
// into a fresh --address-space, the cases, then silo uninstall --force
// --delete-data --yes and a check that no labelled object is left. Main
// is a test binary's TestMain, and uninstalls whatever happened in
// between; the workflow's step runs uninstall again from a shell trap,
// for the run that dies too hard for Main to get there (a panic, the go
// test timeout).
//
// It is shared by pkg/conformance's own GCP cases and by the "gcp"
// conformance.Target of pkg/conformance/targets (local-backend.md §13,
// call L24), so the backend-neutral cases run against the same install.
// Only one install can be in the project at a time, so one test binary
// installs and hands the install on (-silo.handoff) and the next attaches
// to it (-silo.attach) and uninstalls it at its end.
package gcpharness

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

	"github.com/bwsalmon/silo/pkg/client"
	"github.com/bwsalmon/silo/pkg/conformance"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

var (
	flagProject = flag.String("silo.project", envOr("SILO_CONFORMANCE_PROJECT", conformance.DefaultProject), "the host project")
	flagRegion  = flag.String("silo.region", envOr("SILO_CONFORMANCE_REGION", "us-central1"), "the home region")
	flagKey     = flag.String("silo.key", envOr("SILO_CONFORMANCE_KEY", "/home/debian/.gcp-service-account.json"), "the gcp-key capability's key")
	flagImage   = flag.String("silo.image", os.Getenv("SILO_CONFORMANCE_IMAGE"), "silod's image, by digest (install --image)")
	flagKeep    = flag.Bool("silo.keep", false, "don't uninstall at the end (debugging only)")

	flagHarnessImage = flag.String("silo.harness-image", os.Getenv("SILO_CONFORMANCE_HARNESS_IMAGE"), "the harness image, by digest (install --harness-image), needed with -silo.image")

	flagHandoff = flag.Bool("silo.handoff", false, "at the end, delete every silo under the root and leave the install for a later -silo.attach, instead of uninstalling")
	flagAttach  = flag.Bool("silo.attach", false, "use the install a -silo.handoff run left in the project instead of installing, and uninstall it at the end")
)

// The agent cases' install settings: harness sessions on the control
// VMs, a machine type that carries them (agents.SessionSlots gives an
// e2-standard-2, 2 vCPU and 8 GiB, five slots, and the cases run two
// agents at once), and a small root model budget, so that a runaway
// agent can't spend much.
const (
	agentSessions      = "4"
	controlMachineType = "e2-standard-2"
	rootModelBudget    = "5"
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// State is the whole run's state, which the cases read.
type State struct {
	Project, Region, Zone string
	Key                   string
	Silo                  string // the silo CLI built from this tree
	Space                 netip.Prefix
	Gateway, Control      netip.Prefix
	Admin                 string
	InstallOut            string
	InstallErr            error
	MigrateSeen           bool
	// Attached is whether this binary attached to a handed-on install
	// rather than installing: install's own checks then have nothing to
	// look at.
	Attached bool

	ProxyAddr string
	API       *client.Client
	Compute   *compute.Service
	SQL       *sqladmin.Service

	VMs chan struct{} // the cap on probe VMs at once
}

// Run is this binary's run. Started is false until Main has set it up,
// so a package whose TestMain isn't Main has none.
var (
	Run     = &State{}
	Started bool
)

var run = Run

// Installed says whether the cases have a silo to test, and skips the
// case with install's failure if not: a case can't pass, or fail, on a
// tree that never came up. TestInstall reports install's own failure.
func Installed(t testing.TB) {
	t.Helper()
	if !Started {
		t.Skip("the GCP harness isn't running in this test binary (its TestMain isn't gcpharness.Main)")
	}
	if run.InstallErr != nil {
		t.Skipf("silo install failed, so there is no tree to test: %v", run.InstallErr)
	}
}

// Main is a test binary's TestMain: set up, install (or attach), run the
// tests, then uninstall and check that nothing is left (or hand the
// install on).
func Main(m *testing.M) {
	flag.Parse()
	code := func() int {
		ctx := context.Background()
		if err := setup(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "conformance: %v\n", err)
			return 2
		}
		Started = true
		if *flagAttach {
			run.Attached = true
			// A run that died before its handoff leaves its silos: clear
			// them, so that the cases start from an empty tree either way.
			if err := startProxy(ctx); err != nil {
				run.InstallErr = fmt.Errorf("attaching to the handed-on install: %v", err)
			} else if err := handoff(ctx); err != nil {
				run.InstallErr = fmt.Errorf("clearing the handed-on install's silos: %v", err)
			}
		} else {
			run.InstallErr = install(ctx)
		}
		code := m.Run()
		switch {
		case *flagKeep:
			fmt.Fprintln(os.Stderr, "conformance: -silo.keep: leaving the install in place")
			return code
		case *flagHandoff && run.InstallErr == nil:
			if err := handoff(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "--- FAIL: TestHandoff\n    %v\n", err)
				return 1
			}
			fmt.Fprintln(os.Stderr, "conformance: -silo.handoff: leaving the install for -silo.attach")
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

// handoff deletes every silo under the root, so that the binary that
// attaches next starts from an empty tree, as a fresh install would.
func handoff(ctx context.Context) error {
	root, err := run.API.Describe(ctx, "/", false)
	if err != nil {
		return err
	}
	kids, err := run.API.Children(ctx, root.Path)
	if err != nil {
		return err
	}
	for _, k := range kids {
		if err := DeleteSilo(ctx, k.Path); err != nil {
			return fmt.Errorf("deleting %s: %v", k.Path, err)
		}
	}
	return nil
}

// DeleteSilo deletes path and its subtree, and waits for it to go.
func DeleteSilo(ctx context.Context, path string) error {
	op, err := run.API.Delete(ctx, path, true, client.Mutation{})
	if err != nil {
		return err
	}
	if op.ID == "" || op.Done {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	done, err := run.API.WaitOperation(ctx, op.ID, 5*time.Second)
	if err != nil {
		return err
	}
	if done.Error != nil {
		return done.Error
	}
	return nil
}

// setup checks the key, points gcloud and the SDKs at it, builds the CLI
// and picks a fresh address space.
func setup(ctx context.Context) error {
	if _, err := os.Stat(*flagKey); err != nil {
		return fmt.Errorf("no GCP key at %s: this suite needs the gcp-key capability (%v)", *flagKey, err)
	}
	run.Project, run.Region, run.Zone, run.Key = *flagProject, *flagRegion, *flagRegion+"-a", *flagKey
	os.Setenv("GOOGLE_APPLICATION_CREDENTIALS", run.Key)
	os.Setenv("GOOGLE_CLOUD_PROJECT", run.Project)
	os.Setenv("CLOUDSDK_CORE_PROJECT", run.Project)
	if out, err := exec.Command("gcloud", "auth", "activate-service-account", "--key-file="+run.Key).CombinedOutput(); err != nil {
		return fmt.Errorf("gcloud auth: %v: %s", err, out)
	}
	var k struct {
		ClientEmail string `json:"client_email"`
	}
	if b, err := os.ReadFile(run.Key); err == nil && json.Unmarshal(b, &k) == nil {
		run.Admin = "serviceAccount:" + k.ClientEmail
	}
	var err error
	if run.Compute, err = compute.NewService(ctx, option.WithCredentialsFile(run.Key)); err != nil {
		return err
	}
	if run.SQL, err = sqladmin.NewService(ctx, option.WithCredentialsFile(run.Key)); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "silo-conformance")
	if err != nil {
		return err
	}
	run.Silo = filepath.Join(dir, "silo")
	if out, err := exec.Command("go", "build", "-o", run.Silo, "github.com/bwsalmon/silo/cmd/silo").CombinedOutput(); err != nil {
		return fmt.Errorf("building silo: %v: %s", err, out)
	}
	run.VMs = make(chan struct{}, conformance.MaxVMs)
	if *flagAttach {
		return nil
	}
	var used []netip.Prefix
	err = run.Compute.Subnetworks.AggregatedList(run.Project).Pages(ctx, func(l *compute.SubnetworkAggregatedList) error {
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
	if run.Space, err = conformance.PickAddressSpace(used); err != nil {
		return err
	}
	run.Gateway, run.Control = conformance.Ranges(run.Space)
	return nil
}

// silo runs the CLI.
func silo(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, run.Silo, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.MultiWriter(&out, os.Stderr), io.MultiWriter(&out, os.Stderr)
	err := cmd.Run()
	return out.String(), err
}

// install is the real install (SV-97), with the run's cost settings:
// the cheapest database tier and two control replicas, so that
// failover is tested, and agent sessions for the agent cases.
func install(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Minute)
	defer cancel()
	args := []string{"install", "--project", run.Project, "--regions", run.Region,
		"--admin", run.Admin, "--credentials", run.Key,
		"--address-space", run.Space.String(), "--gateway-range", run.Gateway.String(),
		"--control-range", run.Control.String(),
		"--db-tier", "db-f1-micro", "--control-replicas", "2",
		"--agent-sessions", agentSessions, "--control-machine-type", controlMachineType,
		"--root-model-budget", rootModelBudget, "--yes"}
	if *flagImage != "" {
		args = append(args, "--image", *flagImage)
	}
	if *flagHarnessImage != "" {
		args = append(args, "--harness-image", *flagHarnessImage)
	}
	stop := watchMigrateVM(ctx)
	out, err := silo(ctx, args...)
	stop()
	run.InstallOut = out
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
			l, err := run.Compute.Instances.AggregatedList(run.Project).Filter(`name = "silo-migrate"`).Context(ctx).Do()
			if err == nil {
				for _, sc := range l.Items {
					if len(sc.Instances) > 0 {
						run.MigrateSeen = true
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
	run.ProxyAddr = l.Addr().String()
	l.Close()
	proxyCmd = exec.Command(run.Silo, "proxy", "--project", run.Project, "--listen", run.ProxyAddr)
	proxyCmd.Stdout, proxyCmd.Stderr = os.Stderr, os.Stderr
	if err := proxyCmd.Start(); err != nil {
		return err
	}
	if run.API, err = client.New(client.Options{Endpoint: "http://" + run.ProxyAddr + "/silo/v1/", Timeout: 2 * time.Minute}); err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		_, err := run.API.Describe(ctx, "/", false)
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
	_, uerr := silo(ctx, "uninstall", "--project", run.Project, "--credentials", run.Key, "--force", "--delete-data", "--yes")
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
func labelledObjects(ctx context.Context) ([]conformance.Object, error) {
	var objs []conformance.Object
	p := run.Project
	add := func(kind, name string, labels map[string]string, desc string) {
		objs = append(objs, conformance.Object{Kind: kind, Name: name, Labels: labels, Description: desc})
	}
	if err := run.Compute.Instances.AggregatedList(p).Pages(ctx, func(l *compute.InstanceAggregatedList) error {
		for _, sc := range l.Items {
			for _, o := range sc.Instances {
				add("instance", o.Name, o.Labels, o.Description)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := run.Compute.Disks.AggregatedList(p).Pages(ctx, func(l *compute.DiskAggregatedList) error {
		for _, sc := range l.Items {
			for _, o := range sc.Disks {
				add("disk", o.Name, o.Labels, o.Description)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := run.Compute.Subnetworks.AggregatedList(p).Pages(ctx, func(l *compute.SubnetworkAggregatedList) error {
		for _, sc := range l.Items {
			for _, o := range sc.Subnetworks {
				add("subnetwork", o.Name, nil, o.Description)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := run.Compute.Addresses.AggregatedList(p).Pages(ctx, func(l *compute.AddressAggregatedList) error {
		for _, sc := range l.Items {
			for _, o := range sc.Addresses {
				add("address", o.Name, o.Labels, o.Description)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := run.Compute.ForwardingRules.AggregatedList(p).Pages(ctx, func(l *compute.ForwardingRuleAggregatedList) error {
		for _, sc := range l.Items {
			for _, o := range sc.ForwardingRules {
				add("forwardingRule", o.Name, o.Labels, o.Description)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := run.Compute.InstanceTemplates.List(p).Pages(ctx, func(l *compute.InstanceTemplateList) error {
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
	if err := run.Compute.Images.List(p).Pages(ctx, func(l *compute.ImageList) error {
		for _, o := range l.Items {
			add("image", o.Name, o.Labels, o.Description)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := run.Compute.Snapshots.List(p).Pages(ctx, func(l *compute.SnapshotList) error {
		for _, o := range l.Items {
			add("snapshot", o.Name, o.Labels, o.Description)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := run.Compute.Firewalls.List(p).Pages(ctx, func(l *compute.FirewallList) error {
		for _, o := range l.Items {
			add("firewall", o.Name, nil, o.Description)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if l, err := run.Compute.NetworkFirewallPolicies.List(p).Context(ctx).Do(); err != nil {
		return nil, err
	} else {
		for _, o := range l.Items {
			add("firewallPolicy", o.Name, nil, o.Description)
		}
	}
	if l, err := run.Compute.Networks.List(p).Context(ctx).Do(); err != nil {
		return nil, err
	} else {
		for _, o := range l.Items {
			add("network", o.Name, nil, o.Description)
		}
	}
	if l, err := run.SQL.Instances.List(p).Context(ctx).Do(); err != nil {
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
	return conformance.Leftovers(objs), nil
}

func Tail(s string, n int) string {
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}

// Facade calls the compute/v1 facade through the front door, as a root
// member: the caller may read and write every silo's facade project.
// It returns the HTTP status and the decoded body.
func Facade(t testing.TB, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://"+run.ProxyAddr+"/compute/v1/"+strings.TrimPrefix(path, "/"), r)
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

// WaitOp waits for a facade operation to finish.
func WaitOp(t testing.TB, project string, op map[string]any) map[string]any {
	t.Helper()
	name, _ := op["name"].(string)
	if name == "" {
		return op
	}
	scope := "global"
	if z, _ := op["zone"].(string); z != "" {
		scope = "zones/" + LastSegment(z)
	} else if rg, _ := op["region"].(string); rg != "" {
		scope = "regions/" + LastSegment(rg)
	}
	for i := 0; i < 60; i++ {
		_, o := Facade(t, "POST", "projects/"+project+"/"+scope+"/operations/"+name+"/wait", nil)
		if o["status"] == "DONE" {
			return o
		}
	}
	t.Fatalf("operation %s never finished", name)
	return nil
}

func LastSegment(s string) string { return s[strings.LastIndex(s, "/")+1:] }

// ProbeImage is every probe VM's boot image.
const ProbeImage = "projects/debian-cloud/global/images/family/debian-12"

// InstanceBody is a probe-sized instance in the "<siloID>-net" subnet,
// Spot.
func InstanceBody(name, siloID string, mutate func(map[string]any)) map[string]any {
	body := map[string]any{
		"name":        name,
		"machineType": "zones/" + run.Zone + "/machineTypes/" + conformance.ProbeMachineType,
		"scheduling":  map[string]any{"provisioningModel": "SPOT", "instanceTerminationAction": "DELETE"},
		"disks": []any{map[string]any{"boot": true, "autoDelete": true,
			"initializeParams": map[string]any{"sourceImage": ProbeImage, "diskSizeGb": "10", "diskType": "zones/" + run.Zone + "/diskTypes/pd-standard"}}},
		"networkInterfaces": []any{map[string]any{"subnetwork": "regions/" + run.Region + "/subnetworks/" + siloID + "-net"}},
	}
	if mutate != nil {
		mutate(body)
	}
	return body
}
