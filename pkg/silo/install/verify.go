package install

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Verify is step 15, verify: from the installer, through the front
// door, describe / and a facade regions.list, and the root's regions,
// blocks and quota printed. It changes nothing; it is apply until it
// has passed, so an install whose verify failed runs it again.
type Verify struct{ Env *Env }

// VerifyObserved is what the two calls returned, or why they failed.
type VerifyObserved struct {
	Root          *RootSilo `json:"root,omitempty"`
	FacadeRegions []string  `json:"facadeRegions,omitempty"`
	Error         string    `json:"error,omitempty"`
}

func (s *Verify) Name() string          { return "verify" }
func (s *Verify) NewObserved() Observed { return &VerifyObserved{} }

func (s *Verify) Observe(ctx context.Context) (Observed, error) {
	obs := &VerifyObserved{}
	_, h, why, err := observeSilod(ctx, s.Env)
	switch {
	case err != nil:
		return nil, err
	case h == nil || !h.Root:
		obs.Error = why
		return obs, nil
	}
	obs.Root, obs.FacadeRegions, err = s.check(ctx)
	if err != nil {
		obs.Error = err.Error()
	}
	return obs, nil
}

// check is the two calls.
func (s *Verify) check(ctx context.Context) (*RootSilo, []string, error) {
	root, err := s.Env.Cloud.Silod.Root(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("describe /: %w", err)
	}
	regions, err := s.Env.Cloud.Silod.Regions(ctx)
	if err != nil {
		return root, nil, fmt.Errorf("the facade's regions.list: %w", err)
	}
	return root, regions, nil
}

// verified is what was found wrong with a passing pair of calls, or "".
func (s *Verify) verified(root *RootSilo, regions []string) string {
	have := map[string]bool{}
	for _, r := range regions {
		have[r] = true
	}
	var missing []string
	for _, r := range root.Regions {
		if !have[r] {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		return "the facade lists no " + strings.Join(missing, ", ")
	}
	return ""
}

func (s *Verify) Plan(o Observed) []Change {
	obs := o.(*VerifyObserved)
	if obs.Root == nil || obs.FacadeRegions == nil {
		detail := "describe / and regions.list through the front door"
		if obs.Root != nil && obs.Error != "" {
			detail += " (last time: " + obs.Error + ")"
		}
		return []Change{{Action: ActionApply, Detail: detail, Estimate: 10 * time.Second}}
	}
	if bad := s.verified(obs.Root, obs.FacadeRegions); bad != "" {
		return []Change{{Action: ActionApply, Detail: "describe / and regions.list (last time: " + bad + ")", Estimate: 10 * time.Second}}
	}
	return []Change{{Action: ActionOK, Object: "/", Detail: RootSummary(obs.Root)}}
}

// RootSummary is the root's regions, blocks and quota on one line.
func RootSummary(r *RootSilo) string {
	parts := []string{"regions " + strings.Join(r.Regions, ", "), "blocks " + strings.Join(r.Blocks, ", ")}
	var regions []string
	for reg := range r.Quota {
		regions = append(regions, reg)
	}
	sort.Strings(regions)
	for _, reg := range regions {
		var q []string
		for _, c := range summaryClasses {
			if v, ok := r.Quota[reg][c]; ok {
				q = append(q, c+" "+v)
			}
		}
		if len(q) > 0 {
			parts = append(parts, reg+" "+strings.Join(q, ", "))
		}
	}
	return strings.Join(parts, "; ")
}

func (s *Verify) Apply(ctx context.Context, changes []Change) error {
	w := s.Env.Progress
	if w == nil {
		w = io.Discard
	}
	return s.Report(ctx, w)
}

// Report runs the two calls and prints the root's regions, blocks and
// quota to w.
func (s *Verify) Report(ctx context.Context, w io.Writer) error {
	if _, err := waitSilod(ctx, s.Env); err != nil {
		return err
	}
	root, regions, err := s.check(ctx)
	if err != nil {
		return fmt.Errorf("%w (the installer's identity must be a root member: give it as --admin)", err)
	}
	if bad := s.verified(root, regions); bad != "" {
		return fmt.Errorf("verify: %s", bad)
	}
	fmt.Fprintf(w, "verify: root %s\n", root.Path)
	fmt.Fprintf(w, "  regions: %s\n", strings.Join(root.Regions, ", "))
	fmt.Fprintf(w, "  blocks:  %s\n", strings.Join(root.Blocks, ", "))
	var regs []string
	for reg := range root.Quota {
		regs = append(regs, reg)
	}
	sort.Strings(regs)
	for _, reg := range regs {
		var classes []string
		for c := range root.Quota[reg] {
			classes = append(classes, c)
		}
		sort.Strings(classes)
		var q []string
		for _, c := range classes {
			q = append(q, c+" "+root.Quota[reg][c])
		}
		fmt.Fprintf(w, "  quota %s: %s\n", reg, strings.Join(q, ", "))
	}
	return nil
}
