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
//     local-conformance installs the local backend (FindInstall finds
//     it), but no probe container is made through it yet, so their
//     Fence and Registry report themselves not runnable; the vm class
//     also waits on step 10's runtime.Driver.
//   - "gcp": the GCP backend, which has no registry, so it reports the
//     registry cases as not offered, never as passing. Its Fence is the
//     GCP harness's install (pkg/conformance/gcpharness), with e2-micro
//     probe VMs (gcp.go), behind the silo_conformance tag and in a test
//     binary whose TestMain is gcpharness.Main; anywhere else it reports
//     itself not runnable. The harness's own forms of the fence cases
//     are still in pkg/conformance until the nightly has run these
//     against it.
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
	why := "the registry cases' Registry isn't built over an installed silod yet (pkg/conformance/local's TestInstalledSandbox is the installed target's case so far)"
	if l.Class == "vm" {
		why += "; the vm class also waits on item 10"
	}
	return nil, &conformance.NotRunnable{Why: why}
}

func (l Local) Fence(testing.TB) (conformance.Fence, error) {
	why := "no probe containers are made through an installed silod's facade yet (local-conformance installs the local backend, and pkg/conformance/local's TestInstalledSandbox runs a sandbox container in it)"
	if l.Class == "vm" {
		why += "; the vm class also waits on step 10's runtime.Driver"
	}
	return nil, &conformance.NotRunnable{Why: why}
}
