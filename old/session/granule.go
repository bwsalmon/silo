package session

import (
	"context"
	"os/exec"
	"strings"

	"github.com/bwsalmon/silo/old/sandbox"
	"github.com/bwsalmon/silo/old/sandbox/granule"
	"github.com/bwsalmon/silo/old/silofile"
)

func init() { RegisterCheck("granule", checkGranule) }

// checkGranule is --dry-run's part for granule sandboxes (design §14,
// journeys 7 and 10): granule is on PATH, `granule doctor` passes --
// /dev/kvm above all -- each sandbox's VM image is local or can be
// pulled, and each Granulefile is either built already (its cache key's
// tag is local) or will be built at run.
func checkGranule(ctx context.Context, f *silofile.Silofile, environ []string) []CheckResult {
	var images []string
	var built []silofile.Sandbox
	for _, sc := range f.Sandboxes {
		switch {
		case sc.Kind != "granule":
		case sc.Image != "":
			// A cluster's nodes pull it; checkK8sSandboxes asks about
			// those.
			if sc.Backend.K8s != nil {
				continue
			}
			images = append(images, sc.Image)
		case sc.Granulefile != "":
			built = append(built, sc)
		}
	}
	if len(images) == 0 && len(built) == 0 {
		return nil
	}
	b, err := granule.NewFrom(func(k string) string { return getenvFrom(environ, k) })
	if err != nil {
		return []CheckResult{{Name: "granule", Detail: err.Error()}}
	}
	where := b.Granule
	if p, err := exec.LookPath(where); err == nil {
		where = p
	}
	out := []CheckResult{{Name: "granule", OK: true, Detail: where}}
	doc := CheckResult{Name: "granule doctor"}
	report, err := b.Doctor(ctx)
	if err != nil {
		doc.Detail = failures(report, err)
	} else {
		doc.OK, doc.Detail = true, "this machine can run granule VMs"
	}
	out = append(out, doc)
	seen := map[string]bool{}
	for _, img := range images {
		if seen[img] {
			continue
		}
		seen[img] = true
		res := CheckResult{Name: "granule image " + img}
		res.Detail, err = b.CheckImage(ctx, img)
		if err != nil {
			res.Detail = err.Error()
		} else {
			res.OK = true
		}
		out = append(out, res)
	}
	for _, sc := range built {
		res := CheckResult{Name: "granulefile " + sc.Granulefile}
		spec, err := sandbox.SpecFor(f, sc.Name, "")
		var key string
		if err == nil {
			key, err = granule.CacheKey(spec.Build, spec.Target, spec.BuildArgs)
		}
		switch {
		case err != nil:
			res.Detail = err.Error()
		case b.Cached(ctx, key) != "":
			res.OK, res.Detail = true, "built already, as "+granule.BuiltTag(key)
		default:
			res.OK, res.Detail = true, "not built yet: silo run builds it with granule granulefile build"
		}
		out = append(out, res)
	}
	return out
}

// failures is what doctor said that failed, or its error when it said
// nothing that did.
func failures(report string, err error) string {
	var lines []string
	for _, l := range strings.Split(report, "\n") {
		if strings.HasPrefix(l, "FAIL") || strings.HasPrefix(strings.TrimSpace(l), "->") {
			lines = append(lines, strings.Join(strings.Fields(l), " "))
		}
	}
	if len(lines) == 0 {
		return err.Error() + ": " + report
	}
	return strings.Join(lines, "; ")
}
