package session

import (
	"testing"

	"github.com/bwsalmon/silo/v2/sandbox"
	k8ssb "github.com/bwsalmon/silo/v2/sandbox/k8s"
	"github.com/bwsalmon/silo/v2/silofile"
)

// A Silofile whose sandboxes are all on a cluster needs no local
// container runtime or granule.
func TestNewBackendK8sOnly(t *testing.T) {
	f := &silofile.Silofile{Sandboxes: []silofile.Sandbox{
		{Name: "a", Kind: "granule", Image: "g", Backend: silofile.Backend{K8s: &silofile.K8sBackend{Context: "c", Namespace: "n"}}},
		{Name: "b", Kind: "granule", Image: "g", Backend: silofile.Backend{K8s: &silofile.K8sBackend{Context: "c", Namespace: "n", KVMGroup: 7}}},
	}}
	b, err := newBackend(f)
	if err != nil {
		t.Fatal(err)
	}
	k, ok := b.(*sandbox.Kinds)
	if !ok || k.Default != nil || k.K8s == nil {
		t.Fatalf("newBackend = %#v", b)
	}
	kb := k.K8s.(*k8ssb.Backend)
	if len(kb.Targets) != 1 || kb.Targets[0] != (sandbox.K8sTarget{Context: "c", Namespace: "n"}) {
		t.Errorf("targets = %v", kb.Targets)
	}
}
