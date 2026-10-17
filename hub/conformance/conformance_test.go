package conformance_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/bwsalmon/silo/hub/conformance"
	"github.com/bwsalmon/silo/hub/conformance/refgrain"
)

// run runs the kit against a reference Grain listing features.
func run(t *testing.T, features []string, only ...string) *conformance.Report {
	t.Helper()
	return runGrain(t, features, false, only...)
}

// runGrain runs the kit against a reference Grain listing features,
// whose policy allows every repository it registers for when
// allowOutside is set.
func runGrain(t *testing.T, features []string, allowOutside bool, only ...string) *conformance.Report {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	grainCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	defer func() { stop(); <-done }()
	rep, err := conformance.Run(ctx, conformance.Config{
		Dir:     t.TempDir(),
		Timeout: 30 * time.Second,
		Only:    only,
		Logf:    t.Logf,
		Start: func(env conformance.Env) error {
			allow := []string{env.Repo}
			if allowOutside {
				allow = append(allow, env.OutsideRepo)
			}
			g := &refgrain.Grain{Hub: env.Hub, Token: env.Token, ID: env.Node,
				Repos: []string{env.Repo, env.OutsideRepo}, Allow: allow,
				Slots: 2, Features: features, Logf: t.Logf}
			go func() { defer close(done); g.Run(grainCtx) }()
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var text bytes.Buffer
	rep.WriteText(&text)
	t.Logf("report:\n%s", text.String())
	var js bytes.Buffer
	if err := rep.WriteJSON(&js); err != nil {
		t.Fatal(err)
	}
	var back conformance.Report
	if err := json.Unmarshal(js.Bytes(), &back); err != nil || len(back.Scenarios) != len(rep.Scenarios) {
		t.Errorf("the JSON verdict does not read back: %v\n%s", err, js.String())
	}
	return rep
}

func statuses(rep *conformance.Report) map[string]conformance.Status {
	out := map[string]conformance.Status{}
	for _, v := range rep.Scenarios {
		out[v.Name] = v.Status
	}
	return out
}

// The reference Grain, with every feature it has, passes every scenario
// but upward filing, which this hub does not implement.
func TestReferenceGrain(t *testing.T) {
	rep := run(t, []string{refgrain.FeatureInteractive, refgrain.FeatureApprovals})
	got := statuses(rep)
	for _, name := range conformance.Scenarios {
		want := conformance.Pass
		if name == "filing" {
			want = conformance.Skip
		}
		if got[name] != want {
			t.Errorf("scenario %s: %s, want %s", name, got[name], want)
		}
	}
	if !rep.Passed {
		t.Error("the reference grain does not conform")
	}
}

// A Grain advertising no optional feature has those scenarios skipped,
// not failed, and still conforms.
func TestOptionalFeaturesSkipped(t *testing.T) {
	rep := run(t, nil, "approve", "interactive")
	got := statuses(rep)
	want := map[string]conformance.Status{"register": conformance.Pass, "approve": conformance.Skip, "interactive": conformance.Skip}
	for name, s := range want {
		if got[name] != s {
			t.Errorf("scenario %s: %s, want %s", name, got[name], s)
		}
	}
	if len(got) != len(want) || !rep.Passed {
		t.Errorf("scenarios %v, passed %v", got, rep.Passed)
	}
}

// A Grain that runs what its policy should refuse fails the refusal
// scenario, and does not conform.
func TestNonconformingGrainFails(t *testing.T) {
	rep := runGrain(t, nil, true, "refuse")
	if got := statuses(rep); got["refuse"] != conformance.Fail || rep.Passed {
		t.Errorf("scenarios %v, passed %v; want refuse failed", got, rep.Passed)
	}
}
