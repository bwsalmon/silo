// Package targets are the conformance.Targets the backend-neutral
// cases run against (local-backend.md §13, call L24):
//
//   - "local/fake": silod's store, fence API, registry, pull-through
//     cache and Docker facade in process, over the fake runtime drivers
//     and registry.Memory, with the egress proxy's policy check in
//     front of a test upstream. It runs everywhere, on every push.
//   - "local/sandbox" and "local/vm": silod installed with --backend
//     local, probes made through the facade in each isolation class.
//     They report themselves not runnable until silod serves the
//     registry (local-registry.md step 27b) and the local CI job of
//     local-backend.md item 12 installs it; the vm class also waits on
//     item 10.
//   - "gcp": the GCP backend, which has no registry, so it reports the
//     registry cases as not offered, never as passing.
package targets

import (
	"testing"

	"github.com/bwsalmon/silo/pkg/conformance"
)

// All are every target, in the order cases run them.
func All() []conformance.Target {
	return []conformance.Target{GCP{}, Local{Class: "sandbox"}, Local{Class: "vm"}, Fake{}}
}

// GCP is the GCP backend's target. Its registry cases are not offered:
// the registry is the local backend's only (local-registry.md §12).
type GCP struct{}

func (GCP) Name() string { return "gcp" }

func (GCP) Registry(testing.TB) (conformance.Registry, error) {
	return nil, conformance.ErrNotOffered
}

// Local is the local backend, installed, in one isolation class.
type Local struct{ Class string }

func (l Local) Name() string { return "local/" + l.Class }

func (l Local) Registry(testing.TB) (conformance.Registry, error) {
	why := "silod doesn't serve the registry until local-registry.md step 27b wires it, and no CI job installs the local backend until local-backend.md item 12's local-conformance"
	if l.Class == "vm" {
		why += "; the vm class also waits on item 10"
	}
	return nil, &conformance.NotRunnable{Why: why}
}
