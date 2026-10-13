// Package fence is the backend-neutral half of the fence: Input, what
// the fence is compiled from, and how it is read from silod's database
// (the silos with their blocks and whether each is sealed, the gateway
// and control ranges, and the grants that exports and links add), the
// range arithmetic both use, and the applied sequence number. Each
// backend compiles and applies Input its own way: GCE's is
// pkg/backend/gcp/gce/gcefence.
package fence

import "net/netip"

// Silo is one silo, as the fence sees it.
type Silo struct {
	ID     string
	Depth  int
	Blocks []netip.Prefix
	// Sealed is set when the silo's effective control policy is none.
	Sealed bool
}

// Grant is one rule a grant adds to the grants band: an export's
// export-<x>-in or one direction of a link. Its action is allow. Grants
// are built by later tasks; the compiler takes them as they are.
type Grant struct {
	Name      string // the rule name, unique: "export-x-in", "link-l-out"
	Direction string // "INGRESS" or "EGRESS"
	Src, Dst  []netip.Prefix
	L4        []L4
}

// L4 is a protocol and its ports, as a layer4Config.
type L4 struct {
	Protocol string
	Ports    []string
}

// Input is everything the policy is compiled from.
type Input struct {
	// Gateway and Control are the gateway and control ranges, and Root
	// the root's block less both. A database with no root yet has none
	// of them, and its policy is the catch-all rules alone.
	Gateway, Control netip.Prefix
	Root             []netip.Prefix
	Silos            []Silo
	Grants           []Grant
}
