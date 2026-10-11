package install

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ControlVM is the control VM's name, which the control step makes.
const ControlVM = "silo-control"

// TunnelRole is the role that lets a member open an IAP tunnel to the
// front door.
const TunnelRole = "roles/iap.tunnelResourceAccessor"

// Admin is step 14, admin: each --admin is mapped to the root through
// silod's members API, and each --admin and --front-door-member gets
// TunnelRole on silo-control, and only there, not on the project, so
// the tunnel reaches the front door and no other VM. The root step's
// bootstrap maps the first install's --admins with the root itself, so
// the root never exists without a member; this step maps an --admin
// added on a later run, as the installer, who must be a root member.
//
// Uninstall removes every grant of TunnelRole on silo-control first of
// install's objects (install.md "silo uninstall"), rather than leaving
// them to go with the VM: nothing then grants the tunnel even while the
// rest of the teardown runs, or if it stops part-way.
type Admin struct{ Env *Env }

// AdminObserved is silo-control's zone ("" if there is no such VM yet)
// and the members holding TunnelRole on it.
type AdminObserved struct {
	ComputeEnabled bool   `json:"computeEnabled"`
	Zone           string `json:"zone,omitempty"`
	// Ours is whether silo-control carries this install's label.
	Ours    bool     `json:"ours,omitempty"`
	Members []string `json:"members,omitempty"`
	// RootMembers are the root's members, if silod has a root and the
	// installer may read them; RootError is why it couldn't.
	Rooted      bool     `json:"rooted,omitempty"`
	RootMembers []string `json:"rootMembers,omitempty"`
	RootError   string   `json:"rootError,omitempty"`
}

func (s *Admin) Name() string          { return "admin" }
func (s *Admin) homeRegion() string    { return s.Env.Config.HomeRegion() }
func (s *Admin) NewObserved() Observed { return &AdminObserved{} }

// members are the identities granted the tunnel, each once, in flag
// order.
func (s *Admin) members() []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range append(append([]string{}, s.Env.Config.Admins...), s.Env.Config.FrontDoorMembers...) {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

func (s *Admin) Observe(ctx context.Context) (Observed, error) {
	obs := &AdminObserved{}
	on, err := computeOn(ctx, s.Env)
	if err != nil || !on {
		return obs, err
	}
	obs.ComputeEnabled = true
	if _, h, _, err := observeSilod(ctx, s.Env); err != nil {
		return nil, err
	} else if h != nil && h.Root {
		obs.Rooted = true
		if obs.RootMembers, err = s.Env.Cloud.Silod.Members(ctx, "/"); err != nil {
			obs.RootError = err.Error()
		}
	}
	x := s.Env.Cloud.ComputeExtras
	project := s.Env.Config.Project
	if obs.Zone, err = x.InstanceZone(ctx, project, s.Env.Config.HomeRegion(), ControlVM); err != nil || obs.Zone == "" {
		return obs, err
	}
	vm, err := s.Env.Cloud.Compute.GetInstance(ctx, project, obs.Zone, ControlVM)
	if err != nil {
		return nil, err
	}
	obs.Ours = vm.Labels[LabelKey] == s.Env.ID
	p, err := x.GetInstanceIamPolicy(ctx, project, obs.Zone, ControlVM)
	if err != nil {
		return nil, err
	}
	for _, b := range p.Bindings {
		if b.Role == TunnelRole && b.Condition == nil {
			obs.Members = append(obs.Members, b.Members...)
		}
	}
	return obs, nil
}

func (s *Admin) Plan(o Observed) []Change {
	obs := o.(*AdminObserved)
	has := map[string]bool{}
	for _, m := range obs.Members {
		has[m] = true
	}
	on := TunnelRole + " on " + ControlVM
	var out []Change
	mapped := map[string]bool{}
	for _, m := range obs.RootMembers {
		mapped[m] = true
	}
	for _, a := range s.Env.Config.Admins {
		if !mapped[a] && obs.RootError == "" {
			out = append(out, Change{Action: ActionCreate, Object: a, Detail: "-> /", Estimate: 5 * time.Second})
		}
	}
	if obs.RootError != "" && len(s.Env.Config.Admins) > 0 {
		out = append(out, Change{Action: ActionFail, Object: "/", Detail: "reading the root's members: " + obs.RootError,
			Why: "install maps each --admin to the root as the installer", Fix: "install as a root member (one of the root's --admin identities)"})
	}
	for _, m := range s.members() {
		if !has[m] {
			out = append(out, Change{Action: ActionCreate, Object: m, Detail: on, Estimate: 5 * time.Second})
		}
	}
	if len(out) > 0 {
		return out
	}
	if len(s.members()) == 0 {
		return []Change{{Action: ActionOK, Detail: "no --admin or --front-door-member to grant"}}
	}
	if n := len(s.members()); n > 1 {
		return []Change{{Action: ActionOK, Detail: fmt.Sprintf("%d members hold %s", n, on)}}
	}
	return []Change{{Action: ActionOK, Object: s.members()[0], Detail: "holds " + on}}
}

// PlanRemove revokes TunnelRole on silo-control from every member that
// holds it there: the VM is install's, and every grant on it is the
// front door's.
func (s *Admin) PlanRemove(o Observed) []Change {
	obs := o.(*AdminObserved)
	switch {
	case obs.Zone == "":
		return []Change{{Action: ActionOK, Detail: "no VM " + ControlVM + " to hold " + TunnelRole}}
	case !obs.Ours:
		return []Change{{Action: ActionOK, Object: ControlVM, Detail: "is not this install's; left alone"}}
	case len(obs.Members) == 0:
		return []Change{{Action: ActionOK, Detail: "nobody holds " + TunnelRole + " on " + ControlVM}}
	}
	members := append([]string{}, obs.Members...)
	sort.Strings(members)
	return []Change{{Action: ActionDelete, Object: TunnelRole + " on " + ControlVM, Detail: strings.Join(members, ", "), Estimate: 5 * time.Second}}
}

// revoke removes every unconditional TunnelRole binding on silo-control.
func (s *Admin) revoke(ctx context.Context) error {
	x := s.Env.Cloud.ComputeExtras
	project := s.Env.Config.Project
	zone, err := x.InstanceZone(ctx, project, s.Env.Config.HomeRegion(), ControlVM)
	if err != nil || zone == "" {
		return err
	}
	return updatePolicy(ctx,
		func(ctx context.Context) (*Policy, error) {
			return x.GetInstanceIamPolicy(ctx, project, zone, ControlVM)
		},
		func(ctx context.Context, p *Policy) error {
			return x.SetInstanceIamPolicy(ctx, project, zone, ControlVM, p)
		},
		func(p *Policy) bool {
			var members []string
			for _, b := range p.Bindings {
				if b.Role == TunnelRole && b.Condition == nil {
					members = append(members, b.Members...)
				}
			}
			changed := false
			for _, m := range members {
				changed = p.Revoke(TunnelRole, m) || changed
			}
			return changed
		})
}

func (s *Admin) Apply(ctx context.Context, changes []Change) error {
	var grant []string
	for _, ch := range changes {
		switch {
		case ch.Action == ActionDelete:
			return s.revoke(ctx)
		case ch.Action == ActionCreate && ch.Detail == "-> /":
			if _, err := waitSilod(ctx, s.Env); err != nil {
				return err
			}
			// The root step's bootstrap may have mapped it already.
			ms, err := s.Env.Cloud.Silod.Members(ctx, "/")
			if err != nil {
				return fmt.Errorf("reading the root's members: %w", err)
			}
			if !contains(ms, ch.Object) {
				if err := s.Env.Cloud.Silod.PutMember(ctx, "/", ch.Object); err != nil {
					return fmt.Errorf("mapping %s to /: %w", ch.Object, err)
				}
			}
		case ch.Action == ActionCreate:
			grant = append(grant, ch.Object)
		}
	}
	if len(grant) == 0 {
		return nil
	}
	x := s.Env.Cloud.ComputeExtras
	project := s.Env.Config.Project
	zone, err := x.InstanceZone(ctx, project, s.Env.Config.HomeRegion(), ControlVM)
	if err != nil {
		return err
	}
	if zone == "" {
		return fmt.Errorf("there is no VM %s in %s to grant the tunnel on: the control step makes it", ControlVM, s.Env.Config.HomeRegion())
	}
	return updatePolicy(ctx,
		func(ctx context.Context) (*Policy, error) {
			return x.GetInstanceIamPolicy(ctx, project, zone, ControlVM)
		},
		func(ctx context.Context, p *Policy) error {
			return x.SetInstanceIamPolicy(ctx, project, zone, ControlVM, p)
		},
		func(p *Policy) bool {
			changed := false
			for _, m := range grant {
				changed = p.Grant(TunnelRole, m) || changed
			}
			return changed
		})
}
