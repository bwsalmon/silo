//go:build !silo_conformance

package targets

import (
	"testing"

	"github.com/bwsalmon/silo/pkg/conformance"
)

// GCPHarness is whether this build has the GCP harness behind the "gcp"
// target's Fence: it has only with the silo_conformance tag.
const GCPHarness = false

func (GCP) Fence(testing.TB) (conformance.Fence, error) {
	return nil, &conformance.NotRunnable{Why: "the GCP harness installs into a real project behind the silo_conformance tag " +
		"(go test -tags silo_conformance ./pkg/conformance/fence/), which silo-conformance runs nightly"}
}
