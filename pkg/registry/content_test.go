package registry_test

import (
	"testing"

	"github.com/bwsalmon/silo/pkg/registry"
	"github.com/bwsalmon/silo/pkg/registry/contenttest"
)

// TestMemoryContract holds the fake to the contract silo-containerd's
// implementation keeps (containerdstore, in the nsd job).
func TestMemoryContract(t *testing.T) {
	contenttest.Run(t, registry.NewMemory())
}
