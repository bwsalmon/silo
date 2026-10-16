package session

import (
	"context"
	"io"

	"github.com/bwsalmon/silo/old/agentcli"
	"github.com/bwsalmon/silo/old/location"
	k8sloc "github.com/bwsalmon/silo/old/location/k8s"
	"github.com/bwsalmon/silo/old/silofile"
)

func init() { RegisterCheck("k8s", checkK8s) }

// checkK8s is --dry-run's part for location kind k8s (design §2.4,
// §5.4).
func checkK8s(ctx context.Context, f *silofile.Silofile, environ []string) []CheckResult {
	if f.Location.Kind != "k8s" || f.Location.K8s == nil {
		return nil
	}
	ls := &location.Session{CLI: f.Agent.CLI, Binary: f.Agent.Binary, AmbientAuth: f.Agent.Auth.Ambient()}
	if m, err := agentcli.Lookup(f.Agent.CLI); err == nil {
		ls.VersionArgs = m.VersionArgs()
	}
	var out []CheckResult
	for _, c := range k8sloc.Check(ctx, K8sConfig(f.Location.K8s), ls) {
		out = append(out, CheckResult{Name: c.Name, OK: c.OK, Detail: c.Detail})
	}
	return out
}

// K8sConfig is the k8s location's configuration for location.k8s.
func K8sConfig(c *silofile.K8s) k8sloc.Config {
	cfg := k8sloc.Config{
		Kubectl:     k8sloc.Kubectl{Context: c.Context, Namespace: c.Namespace},
		Image:       c.Image,
		TunnelImage: c.TunnelImage,
	}
	if r := c.Resources; r != nil {
		cfg.Resources.CPU = r.CPU
		if r.Memory != nil {
			cfg.Resources.Memory = int64(*r.Memory)
		}
	}
	return cfg
}

// K8sLocation is the k8s location for location.k8s, which at startup
// removes this machine's stale pods in the namespace (design §5.4).
func K8sLocation(c *silofile.K8s, getenv func(string) string, stderr io.Writer) *k8sloc.Location {
	cfg := K8sConfig(c)
	cfg.GC = k8sloc.GCOptions{Live: Live(getenv)}
	cfg.Stderr = stderr
	return k8sloc.New(cfg)
}
