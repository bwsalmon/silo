package session

import (
	"context"

	"github.com/bwsalmon/silo/old/sandbox"
	k8ssb "github.com/bwsalmon/silo/old/sandbox/k8s"
	"github.com/bwsalmon/silo/old/silofile"
)

func init() { RegisterCheck("k8s sandboxes", checkK8sSandboxes) }

// checkK8sSandboxes is --dry-run's part for sandboxes with backend
// k8s (design §14, journey 9): for each cluster and namespace, some
// node offers devic.es/kvm, and devic.es/tun too when one of its
// sandboxes has network open -- the resources granule's KVM device
// plugin publishes. It creates nothing and pulls nothing: whether the
// node can pull the guest image is not asked, since asking means
// scheduling a pod that pulls gigabytes.
func checkK8sSandboxes(ctx context.Context, f *silofile.Silofile, environ []string) []CheckResult {
	var targets []sandbox.K8sTarget
	open := map[sandbox.K8sTarget]bool{}
	for _, sc := range f.Sandboxes {
		k := sc.Backend.K8s
		if k == nil {
			continue
		}
		t := sandbox.K8sTarget{Context: k.Context, Namespace: k.Namespace}
		if _, seen := open[t]; !seen {
			targets = append(targets, t)
			open[t] = false
		}
		open[t] = open[t] || sc.Network.Mode == "open"
	}
	b := &k8ssb.Backend{}
	var out []CheckResult
	for _, t := range targets {
		name := "k8s sandbox nodes"
		if t.Context != "" {
			name += " in " + t.Context
		}
		res := CheckResult{Name: name}
		detail, err := b.CheckDevices(ctx, t, open[t])
		if err != nil {
			res.Detail = err.Error()
		} else {
			res.OK, res.Detail = true, detail
		}
		out = append(out, res)
	}
	return out
}
