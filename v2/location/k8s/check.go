package k8s

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bwsalmon/silo/v2/location"
	sshloc "github.com/bwsalmon/silo/v2/location/ssh"
)

// CheckResult is one line of silo run --dry-run's checklist.
type CheckResult struct {
	Name   string
	OK     bool
	Detail string
}

// Check is silo run --dry-run's part for a k8s location (design §2.4):
// the context is reachable, the namespace exists, silo may make what a
// session makes there, and both images can be pulled and the CLI is in
// the agent's. The images are tried with a short-lived pod, labelled as
// silo's check and deleted at once, because only the cluster's own
// nodes can say whether they can pull an image; it is the one thing
// --dry-run makes.
func Check(ctx context.Context, cfg Config, s *location.Session) []CheckResult {
	if cfg.TunnelImage == "" {
		cfg.TunnelImage = DefaultTunnelImage
	}
	k := &cfg.Kubectl
	name := "k8s " + k.Where()
	reach := CheckResult{Name: name + ": reachable"}
	res, err := k.run(ctx, nil, "version", "-o", "json", "--request-timeout=20s")
	switch {
	case err != nil:
		reach.Detail = err.Error()
	case res.Code != 0:
		reach.Detail = res.err("kubectl version").Error()
	default:
		var v struct {
			Server struct {
				GitVersion string `json:"gitVersion"`
			} `json:"serverVersion"`
		}
		json.Unmarshal([]byte(res.Stdout), &v)
		reach.OK, reach.Detail = true, "server "+v.Server.GitVersion
	}
	out := []CheckResult{reach}
	if !reach.OK {
		return out
	}
	ns := k.Namespace
	if ns == "" {
		r, _ := k.run(ctx, nil, "config", "view", "--minify", "-o", "jsonpath={..namespace}")
		if ns = strings.TrimSpace(r.Stdout); ns == "" {
			ns = "default"
		}
	}
	nsc := CheckResult{Name: name + ": namespace " + ns + " exists"}
	res, err = k.run(ctx, nil, "get", "namespace", ns, "-o", "name")
	switch {
	case err != nil:
		nsc.Detail = err.Error()
	case res.Code == 0:
		nsc.OK, nsc.Detail = true, strings.TrimSpace(res.Stdout)
	case strings.Contains(res.Stderr, "Forbidden") || strings.Contains(res.Stderr, "forbidden"):
		nsc.OK, nsc.Detail = true, "not checked: you may not read namespaces"
	default:
		nsc.Detail = res.err("kubectl get namespace " + ns).Error()
	}
	out = append(out, nsc)
	if !nsc.OK {
		return out
	}
	can := CheckResult{Name: name + ": may run sessions", OK: true, Detail: "create pods, secrets, pods/exec and pods/portforward"}
	for _, verb := range []string{"create pods", "create secrets", "create pods/exec", "create pods/portforward", "delete pods", "delete secrets"} {
		r, err := k.run(ctx, nil, append([]string{"auth", "can-i"}, strings.Fields(verb)...)...)
		if err != nil || strings.TrimSpace(r.Stdout) != "yes" {
			can.OK, can.Detail = false, "you may not "+verb+" in namespace "+ns
			break
		}
	}
	out = append(out, can)
	return append(out, checkImages(ctx, cfg, s, name)...)
}

// checkImages runs the check pod and reads what the probe found.
func checkImages(ctx context.Context, cfg Config, s *location.Session, name string) []CheckResult {
	k := &cfg.Kubectl
	img := CheckResult{Name: name + ": images can be pulled", Detail: cfg.Image + ", " + cfg.TunnelImage}
	bin := CheckResult{Name: name + ": agent CLI " + s.CLI + " in " + cfg.Image}
	b := make([]byte, 5)
	rand.Read(b)
	pod := fmt.Sprintf("silo-check-%x", b)
	if _, err := k.must(ctx, "creating the check pod", checkPodSpec(pod, sshloc.Origin(), cfg.Image, cfg.TunnelImage, sshloc.ProbeScript(s)), "create", "-f", "-", "-o", "name"); err != nil {
		img.Detail = err.Error()
		return []CheckResult{img}
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		k.run(dctx, nil, "delete", "pod", pod, "--ignore-not-found", "--wait=false")
	}()
	l := &Location{cfg: cfg, k: k, pod: pod}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	for {
		p, err := l.status(wctx)
		if err != nil {
			img.Detail = err.Error()
			return []CheckResult{img}
		}
		if c, reason, msg := p.failure(); c != "" {
			im := cfg.Image
			if c == TunnelContainer {
				im = cfg.TunnelImage
			}
			img.Detail = fmt.Sprintf("%s: %s: %s", im, reason, msg)
			return []CheckResult{img}
		}
		if p.Status.Phase == "Succeeded" || p.Status.Phase == "Failed" {
			break
		}
		select {
		case <-wctx.Done():
			img.Detail = "the check pod did not finish within 3 minutes"
			return []CheckResult{img}
		case <-time.After(500 * time.Millisecond):
		}
	}
	img.OK = true
	logs, err := k.must(ctx, "reading the check pod's output", nil, "logs", pod, "-c", AgentContainer)
	if err != nil {
		bin.Detail = err.Error()
		return []CheckResult{img, bin}
	}
	p := sshloc.ParseProbe(logs)
	perr := l.checkProbe(p, s)
	bin.OK = p.Binary != "" && perr == nil
	if bin.OK {
		bin.Detail = strings.TrimSpace(p.Binary + " " + p.Version)
	} else if perr != nil {
		bin.Detail = perr.Error()
	}
	return []CheckResult{img, bin}
}
