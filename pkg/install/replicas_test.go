package install_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/fence"
	"github.com/bwsalmon/silo/pkg/gwtls"
	"github.com/bwsalmon/silo/pkg/install"
	"github.com/bwsalmon/silo/pkg/install/fakecloud"
	compute "google.golang.org/api/compute/v1"
)

var replicaArgs = []string{"--control-replicas", "3", "--gateway-replicas", "2", "--agent-sessions", "4"}

func apply(t *testing.T, f *fakecloud.Fake, args ...string) []install.Step {
	t.Helper()
	st := steps(t, f, args...)
	if err := install.Apply(context.Background(), st, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	return st
}

func siloInternal(t *testing.T, f *fakecloud.Fake) string {
	t.Helper()
	rs, err := f.Cloud().DNS.GetRecordSet(context.Background(), project, install.ZoneName, install.ZoneDNSName, "A")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(rs.Rrdatas, " ")
}

func templateMetadata(t *testing.T, f *fakecloud.Fake, mig *compute.InstanceGroupManager) (*compute.InstanceTemplate, map[string]string) {
	t.Helper()
	tm, err := f.Compute.GetInstanceTemplate(context.Background(), project, "us-central1", lastSegment(mig.InstanceTemplate))
	if err != nil {
		t.Fatal(err)
	}
	md := map[string]string{}
	for _, it := range tm.Properties.Metadata.Items {
		md[it.Key] = *it.Value
	}
	return tm, md
}

func lastSegment(s string) string { return s[strings.LastIndex(s, "/")+1:] }

// TestReplicas (gateway.md "Placement", SV-89): --control-replicas and
// --gateway-replicas make the MIGs and their internal passthrough NLBs
// with global access, move silo.internal, let the health checkers in,
// roll an upgrade out with none unavailable, and come down again.
func TestReplicas(t *testing.T) {
	ctx := context.Background()
	f := fakecloud.New(project)
	st := apply(t, f, replicaArgs...)

	for _, s := range install.PlanAll(ctx, st) {
		if s.Changing() || s.Failed() {
			t.Errorf("re-run: %s plans %v", s.Step, s.Changes)
		}
	}
	ctl, gw := f.MIGs["us-central1/silo-control-mig"], f.MIGs["us-central1/silo-gateway-mig"]
	if ctl == nil || gw == nil {
		t.Fatalf("MIGs: %v", f.MIGs)
	}
	// silo-control is the third control VM.
	if ctl.TargetSize != 2 || gw.TargetSize != 2 {
		t.Errorf("MIG sizes %d and %d, want 2 (with silo-control, 3) and 2", ctl.TargetSize, gw.TargetSize)
	}
	for _, m := range []*compute.InstanceGroupManager{ctl, gw} {
		u := m.UpdatePolicy
		if u == nil || u.Type != "PROACTIVE" || u.MaxUnavailable == nil || u.MaxUnavailable.Fixed != 0 || u.MaxSurge == nil || u.MaxSurge.Fixed < 1 {
			t.Errorf("%s rolls out with %+v, want proactive with none unavailable", m.Name, u)
		}
		if len(m.AutoHealingPolicies) != 1 {
			t.Errorf("%s has no autohealing health check", m.Name)
		}
	}
	tmpl, md := templateMetadata(t, f, ctl)
	if sas := tmpl.Properties.ServiceAccounts; len(sas) != 1 || sas[0].Email != install.SilodEmail(project) {
		t.Errorf("control VMs run as %v", sas)
	}
	if md[install.CredentialKeyKey] == "" || !strings.Contains(md["user-data"], "'--control-replicas' '3'") ||
		!strings.Contains(md["user-data"], "'--gateway-tls-listen' ':8444'") {
		t.Errorf("control template metadata lacks the credential key or silod's replica flags:\n%s", md["user-data"])
	}
	tmpl, md = templateMetadata(t, f, gw)
	if len(tmpl.Properties.ServiceAccounts) != 0 {
		t.Errorf("gateway VMs have a service account: %v", tmpl.Properties.ServiceAccounts)
	}
	if tmpl.Properties.Labels[gwtls.LabelRole] != gwtls.RoleGateway {
		t.Errorf("gateway VMs are labeled %v, which the leader looks for", tmpl.Properties.Labels)
	}
	if _, ok := md[install.CredentialKeyKey]; ok {
		t.Error("gateway VMs hold the agent credentials key")
	}
	ctlLB, gwLB := f.ForwardingRules["us-central1/silo-control-lb"], f.ForwardingRules["us-central1/silo-gateway-lb"]
	if ctlLB == nil || gwLB == nil {
		t.Fatalf("forwarding rules: %v", f.ForwardingRules)
	}
	if md[install.SilodAddressKey] != ctlLB.IPAddress {
		t.Errorf("gateway VMs reach silod at %q, not the control NLB's %s", md[install.SilodAddressKey], ctlLB.IPAddress)
	}
	for _, r := range []*compute.ForwardingRule{ctlLB, gwLB} {
		if r.LoadBalancingScheme != "INTERNAL" || !r.AllowGlobalAccess || !r.AllPorts || r.BackendService == "" {
			t.Errorf("%s is not an internal passthrough NLB with global access: %+v", r.Name, r)
		}
	}
	be := f.BackendServices["us-central1/silo-control-lb"]
	if be == nil || len(be.Backends) != 2 || len(be.HealthChecks) != 1 {
		t.Fatalf("the control backend service: %+v", be)
	}
	if got := f.GroupMembers; len(got) != 1 {
		t.Errorf("anchor groups: %v", got)
	} else {
		for _, m := range got {
			if len(m) != 1 || m[0] != install.ControlVM {
				t.Errorf("the anchor group holds %v", m)
			}
		}
	}
	if got := siloInternal(t, f); got != gwLB.IPAddress {
		t.Errorf("silo.internal is %s, not the gateway NLB's %s", got, gwLB.IPAddress)
	}
	pol, err := f.Compute.GetFirewallPolicy(ctx, project, fence.PolicyName)
	if err != nil {
		t.Fatal(err)
	}
	health := false
	for _, r := range pol.Rules {
		health = health || r.RuleName == fence.RuleGatewayHealth
	}
	if !health {
		t.Error("the fence has no gw-health-in")
	}

	// An upgrade: a new template, rolled out.
	before := ctl.InstanceTemplate
	st = apply(t, f, append(append([]string{}, replicaArgs...), "--version", "v1.0.1")...)
	if after := f.MIGs["us-central1/silo-control-mig"].InstanceTemplate; after == before {
		t.Error("the upgrade left the control MIG on its old template")
	}
	n := 0
	for _, tm := range mustTemplates(t, f) {
		if tm.Properties.Labels[gwtls.LabelRole] == gwtls.RoleControl {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d control templates after the upgrade, want the old one deleted", n)
	}

	// Without --gateway-replicas, silo.internal moves to the control
	// NLB, and the gateway group goes.
	apply(t, f, "--control-replicas", "3", "--agent-sessions", "4", "--version", "v1.0.1")
	if f.MIGs["us-central1/silo-gateway-mig"] != nil || f.ForwardingRules["us-central1/silo-gateway-lb"] != nil {
		t.Error("the gateway group outlived --gateway-replicas")
	}
	if got := siloInternal(t, f); got != ctlLB.IPAddress {
		t.Errorf("silo.internal is %s, not the control NLB's %s", got, ctlLB.IPAddress)
	}
	// Back to one control VM: silo.internal is silo-gateway-ip again.
	st = apply(t, f, "--agent-sessions", "4", "--version", "v1.0.1")
	if len(f.MIGs) != 0 || len(f.BackendServices) != 0 || len(f.HealthChecks) != 0 || len(f.InstanceGroups) != 0 || len(mustTemplates(t, f)) != 0 {
		t.Errorf("left behind: MIGs %v, backends %v, checks %v, groups %v", f.MIGs, f.BackendServices, f.HealthChecks, f.InstanceGroups)
	}
	if got := siloInternal(t, f); got != f.Addresses["us-central1/"+install.GatewayAddress].Address {
		t.Errorf("silo.internal is %s, not silo-gateway-ip", got)
	}
	for _, s := range install.PlanAll(ctx, st) {
		if s.Changing() || s.Failed() {
			t.Errorf("re-run at one VM: %s plans %v", s.Step, s.Changes)
		}
	}
}

// TestReplicasUninstall: uninstall takes the groups down before
// silo-control.
func TestReplicasUninstall(t *testing.T) {
	f := fakecloud.New(project)
	st := apply(t, f, replicaArgs...)
	if err := install.Remove(context.Background(), st, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(f.MIGs) != 0 || len(f.ForwardingRules) != 0 || len(f.BackendServices) != 0 || len(mustTemplates(t, f)) != 0 {
		t.Errorf("uninstall left: MIGs %v, rules %v, backends %v", f.MIGs, f.ForwardingRules, f.BackendServices)
	}
}

func mustTemplates(t *testing.T, f *fakecloud.Fake) []*compute.InstanceTemplate {
	t.Helper()
	ts, err := f.Compute.ListInstanceTemplates(context.Background(), project, "us-central1", gce.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// TestGatewayUserData: a gateway VM makes its own key, asks on its
// console, reaches silod over mTLS, and keeps NLB packets' sources.
func TestGatewayUserData(t *testing.T) {
	ud := install.GatewayUserData(install.GatewayParams{Region: "us-central1", AddressSpace: install.Defaults().AddressSpace})
	for _, want := range []string{
		"openssl ecparam -name prime256v1 -genkey",
		`echo "silo-gateway-csr: $csr" >/dev/ttyS0`,
		"attributes/silo-gateway-cert?wait_for_change=true",
		`gateway --silod-address "$silod:8444"`,
		"--tls-cert /tls/cert.pem --tls-key /tls/key.pem --tls-ca /tls/ca.pem",
		"-m addrtype --dst-type LOCAL",
		"--cap-drop ALL",
	} {
		if !strings.Contains(ud, want) {
			t.Errorf("gateway user-data lacks %q", want)
		}
	}
	if strings.Contains(ud, "docker-credential-gcr") {
		t.Error("a gateway VM has no token to give docker")
	}
}
