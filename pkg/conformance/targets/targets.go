// Package targets are the conformance.Targets the backend-neutral
// cases run against (local-backend.md §13, call L24):
//
//   - "local/fake": silod's store, fence API, registry, pull-through
//     cache and Docker facade in process, over the fake runtime drivers
//     and registry.Memory, with the egress proxy's policy check in
//     front of a test upstream. It runs everywhere, on every push.
//     Its Fence compiles the local fence (local/netfence) from the
//     store and the facade's topology, and decides each probe's check
//     with the nftables evaluator, as silo-net's kernel would. It
//     also runs silod's git service, gitstore and mirror, with the
//     gateway in front (conformance.Git), for the repo cases; an uber
//     repo is git http-backend, and a restore restarts that part of
//     silod on a copy of its store and repos. Its containers run a
//     scripted shell (fakeshell.go) for the compose and testcontainers
//     cases, not their images, which it says as conformance.Simulated.
//   - "local/sandbox" and "local/vm": silod installed with --backend
//     local, probes made through the facade in each isolation class.
//     They report themselves not runnable until silod serves the
//     registry (local-registry.md step 27b) and the facade's host
//     sockets (local-backend.md §4, "As built in step 6", "left to
//     wiring"), which local-conformance needs to make probes; the vm
//     class also waits on step 10's runtime.Driver.
//   - "gcp": the GCP backend, which has no registry, so it reports the
//     registry cases as not offered, never as passing. Its fence cases
//     are still the GCP harness's own, in pkg/conformance behind the
//     silo_conformance tag, unchanged; its Fence reports itself not
//     runnable from these packages until that harness is moved behind
//     it.
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

func (GCP) Fence(testing.TB) (conformance.Fence, error) {
	return nil, &conformance.NotRunnable{Why: "the GCP harness installs into a real project behind the silo_conformance tag " +
		"(go test -tags silo_conformance ./pkg/conformance/), whose own forms of these cases run nightly in silo-conformance"}
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

func (l Local) Fence(testing.TB) (conformance.Fence, error) {
	why := "silod doesn't serve the Docker facade's host sockets yet (local-backend.md §4, \"As built in step 6\": left to wiring), so no probe container can be made"
	if l.Class == "vm" {
		why += "; the vm class also waits on step 10's runtime.Driver"
	}
	return nil, &conformance.NotRunnable{Why: why}
}
