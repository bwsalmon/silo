//go:build silo_conformance

package fence_test

import (
	"testing"

	"github.com/bwsalmon/silo/pkg/conformance/gcpharness"
)

// With the silo_conformance tag, this binary runs the GCP harness, so
// that the "gcp" target's Fence has an install: its own, or, with
// -silo.attach, the one pkg/conformance's run handed on.
func TestMain(m *testing.M) { gcpharness.Main(m) }
