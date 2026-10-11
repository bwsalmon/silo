package fenceeval

import (
	"context"
	"testing"

	"github.com/bwsalmon/silo/pkg/silo/gce"
	"github.com/bwsalmon/silo/pkg/silo/gce/fake"
	compute "google.golang.org/api/compute/v1"
)

// TestLoadFromFake writes the hand-written fence and some tenant rules
// into the fake, as silod would, and checks that Load reads back an
// evaluator that decides as the one built from them directly.
func TestLoadFromFake(t *testing.T) {
	ctx := context.Background()
	const proj = "host"
	f := fake.New(fake.Options{Project: proj, ManualOperations: true})
	do := func(op *compute.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		f.RunOperations()
		done, err := f.WaitOperation(ctx, proj, gce.Global, op.Name)
		if err != nil {
			t.Fatal(err)
		}
		if done.Error != nil {
			t.Fatalf("%s failed: %+v", done.OperationType, done.Error.Errors[0])
		}
	}
	do(f.InsertNetwork(ctx, proj, &compute.Network{Name: "silo", NetworkFirewallPolicyEnforcementOrder: BeforeClassic}))
	do(f.InsertNetwork(ctx, proj, &compute.Network{Name: "other"}))
	pol := baseTree().policy()
	do(f.InsertFirewallPolicy(ctx, proj, &compute.FirewallPolicy{Name: pol.Name}))
	for _, r := range pol.Rules {
		if r.Priority < 2147483644 { // the fake has GCE's defaults already
			do(f.AddFirewallPolicyRule(ctx, proj, pol.Name, r))
		}
	}
	do(f.AddFirewallPolicyAssociation(ctx, proj, pol.Name, &compute.FirewallPolicyAssociation{AttachmentTarget: "global/networks/silo"}, false))
	tenant := append(allowAll(siloA), allowAll(siloC)...)
	for _, fw := range tenant {
		fw := *fw
		fw.Network = "global/networks/silo"
		do(f.InsertFirewall(ctx, proj, &fw))
	}
	// A rule in another network must not count.
	do(f.InsertFirewall(ctx, proj, &compute.Firewall{Name: "other-open", Network: "global/networks/other", Allowed: []*compute.FirewallAllowed{{IPProtocol: "all"}}}))

	got, err := Load(ctx, f, proj, "silo")
	if err != nil {
		t.Fatal(err)
	}
	want := newEval(t, baseTree(), tenant...)
	for _, p := range []Packet{
		TCP(vmA1, vmA2, 22),
		TCP(vmC1, vmC2, 22),
		TCP(vmC1, vmA1, 22),
		TCP(vmRoot, vmA1, 22),
		TCP(vmA1, vmB, 5432),
		TCP(vmB, vmGW, 8080),
		TCP(vmA1, vmGW, 8080),
		TCP(vmA1, pscDB, 5432),
		TCP(vmGW, pscDB, 5432),
		TCP(iapHost, vmGW, 443),
		TCP(vmGW, vmB, 30001),
	} {
		g, w := eval(t, got, p), eval(t, want, p)
		if g != w {
			t.Errorf("%s: loaded %s, want %s", p, g, w)
		}
	}
	if !got.Allowed(TCP(vmA1, vmA2, 22)) {
		t.Error("a's tenant rules weren't loaded")
	}

	// The other network has no policy, and its own rule.
	other, err := Load(ctx, f, proj, "other")
	if err != nil {
		t.Fatal(err)
	}
	if v := eval(t, other, TCP(vmA1, vmB, 22)); !v.Allowed || v.Ingress.Rule != "other-open" {
		t.Errorf("other network: %s", v)
	}
	if _, err := Load(ctx, f, proj, "missing"); err == nil {
		t.Error("Load of a missing network succeeded")
	}
}
