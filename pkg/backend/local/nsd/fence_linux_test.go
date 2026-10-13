package nsd

import (
	"context"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/backend/local/netfence"
	"github.com/bwsalmon/silo/pkg/fence"
)

// TestHolderFence loads the local fence into a real holder's silo-net
// the way silod does (netfence.Nsenter), replaces it, and lists it back
// (local-backend.md §9 "Applying").
func TestHolderFence(t *testing.T) {
	var nft string
	for _, p := range []string{"nft", "/usr/sbin/nft", "/sbin/nft"} {
		if path, err := exec.LookPath(p); err == nil {
			nft = path
			break
		}
	}
	_, nsErr := exec.LookPath("nsenter")
	if nft == "" || nsErr != nil {
		if os.Getenv("SILO_REQUIRE_NSD") != "" {
			t.Fatalf("SILO_REQUIRE_NSD is set, but nft (%q) or nsenter (%v) is missing", nft, nsErr)
		}
		t.Skip("nft or nsenter is not installed")
	}
	_, _, pid, _ := startHolder(t)
	n := netfence.Nsenter(func() (int, error) { return pid, nil }, nft)
	ctx := context.Background()

	in := &fence.Input{Gateway: netip.MustParsePrefix("10.200.255.0/24"), Control: netip.MustParsePrefix("10.200.254.0/24"),
		Silos: []fence.Silo{{ID: "root", Blocks: []netip.Prefix{netip.MustParsePrefix("10.200.0.0/16")}}}}
	topo := &netfence.Topology{API: netip.MustParseAddr("10.99.0.1"),
		Networks: []netfence.Network{{ID: "n1", Silo: "root", Bridge: "sb-n1", Subnet: netip.MustParsePrefix("10.200.1.0/24")}}}
	for i, sealed := range []bool{false, true} {
		in.Silos[0].Sealed = sealed
		rs, err := netfence.Compile(in, topo, netfence.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err := n.Load(ctx, rs); err != nil {
			t.Fatalf("load %d: %v", i, err)
		}
		got, err := n.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "table inet silo") || !strings.Contains(got, "table bridge silo") {
			t.Fatalf("listed after load %d:\n%s", i, got)
		}
		// Sealing the root empties api_ok: the second load replaced the
		// first rather than adding to it.
		_, apiOK, _ := strings.Cut(got, "set api_ok {")
		apiOK, _, _ = strings.Cut(apiOK, "}")
		if has := strings.Contains(apiOK, "10.200."); has == sealed {
			t.Errorf("load %d (sealed %v): api_ok holds the root's space: %v\n%s", i, sealed, has, got)
		}
	}
}
