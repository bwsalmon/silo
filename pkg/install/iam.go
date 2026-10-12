package install

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SilodAccountID is silod's service account's id.
const SilodAccountID = "silod"

// RoleID is silod's custom role's id.
const RoleID = "silod"

// SilodEmail is silod's service account in project.
func SilodEmail(project string) string {
	return SilodAccountID + "@" + project + ".iam.gserviceaccount.com"
}

// RoleName is silod's custom role's full name.
func RoleName(project string) string { return "projects/" + project + "/roles/" + RoleID }

// owned is the description install gives an object with no labels:
// DescriptionPrefix, the install's id, then what it is.
func owned(env *Env, what string) string { return DescriptionPrefix + env.ID + " " + what }

// ownedBy reports whether desc is one install wrote, and whose.
func ownedBy(desc string) (id string, ok bool) {
	rest, ok := strings.CutPrefix(desc, DescriptionPrefix)
	if !ok {
		return "", false
	}
	id, _, _ = strings.Cut(rest, " ")
	return id, id != ""
}

// notOurs is the failure for an object with install's fixed name that
// this install didn't create.
func notOurs(env *Env, object, why, del string) Change {
	return Change{Action: ActionFail, Object: object, Detail: "exists, and this install didn't create it",
		Why: why, Fix: fmt.Sprintf("delete it (%s --project %s), or install into another project", del, env.Config.Project)}
}

// SilodAccount is step 3, service-account: silod@<project>, the
// identity the control VMs run as.
type SilodAccount struct{ Env *Env }

// SilodAccountObserved is silod's account, or Exists false.
type SilodAccountObserved struct {
	IAMEnabled  bool   `json:"iamEnabled"`
	Exists      bool   `json:"exists"`
	DisplayName string `json:"displayName,omitempty"`
	Description string `json:"description,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"`
}

func (s *SilodAccount) Name() string          { return "service-account" }
func (s *SilodAccount) NewObserved() Observed { return &SilodAccountObserved{} }

// want is the account as install makes it.
func (s *SilodAccount) want() *ServiceAccount {
	return &ServiceAccount{Email: SilodEmail(s.Env.Config.Project), DisplayName: "silod",
		Description: owned(s.Env, "silod, the Silo control plane")}
}

func (s *SilodAccount) Observe(ctx context.Context) (Observed, error) {
	obs := &SilodAccountObserved{}
	on, err := s.Env.Enabled(ctx, "iam.googleapis.com")
	if err != nil || !on {
		return obs, err
	}
	obs.IAMEnabled = true
	sa, err := s.Env.Cloud.IAM.GetServiceAccount(ctx, s.Env.Config.Project, SilodEmail(s.Env.Config.Project))
	if IsNotFound(err) {
		return obs, nil
	}
	if err != nil {
		return nil, err
	}
	obs.Exists, obs.DisplayName, obs.Description, obs.Disabled = true, sa.DisplayName, sa.Description, sa.Disabled
	return obs, nil
}

func (s *SilodAccount) Plan(o Observed) []Change {
	obs := o.(*SilodAccountObserved)
	w := s.want()
	if !obs.Exists {
		return []Change{{Action: ActionCreate, Object: w.Email, Estimate: 5 * time.Second}}
	}
	if !isOurs(obs.Description, s.Env.ID) {
		return []Change{notOurs(s.Env, w.Email, "silod's identity is install's: its roles are granted to it and to nothing else",
			"gcloud iam service-accounts delete "+w.Email)}
	}
	var out []Change
	if obs.DisplayName != w.DisplayName || obs.Description != w.Description {
		out = append(out, Change{Action: ActionUpdate, Object: w.Email, Detail: "display name and description"})
	}
	if obs.Disabled {
		out = append(out, Change{Action: ActionUpdate, Object: w.Email, Detail: "enable: it is disabled"})
	}
	if len(out) == 0 {
		return []Change{{Action: ActionOK, Object: w.Email}}
	}
	return out
}

func (s *SilodAccount) PlanRemove(o Observed) []Change {
	obs := o.(*SilodAccountObserved)
	email := SilodEmail(s.Env.Config.Project)
	if !obs.Exists {
		return []Change{{Action: ActionOK, Detail: "no service account " + email}}
	}
	if !isOurs(obs.Description, s.Env.ID) {
		return []Change{{Action: ActionOK, Object: email, Detail: "is not this install's; left alone"}}
	}
	return []Change{{Action: ActionDelete, Object: email}}
}

func (s *SilodAccount) Apply(ctx context.Context, changes []Change) error {
	c := s.Env.Cloud.IAM
	project := s.Env.Config.Project
	w := s.want()
	for _, ch := range changes {
		var err error
		switch {
		case ch.Action == ActionCreate:
			_, err = c.CreateServiceAccount(ctx, project, SilodAccountID, w)
			if IsAlreadyExists(err) {
				err = nil
			}
		case ch.Action == ActionUpdate && strings.HasPrefix(ch.Detail, "enable"):
			err = c.EnableServiceAccount(ctx, project, w.Email)
		case ch.Action == ActionUpdate:
			err = c.PatchServiceAccount(ctx, project, w)
		case ch.Action == ActionDelete:
			err = c.DeleteServiceAccount(ctx, project, w.Email)
			if IsNotFound(err) {
				err = nil
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// SilodRole is step 4, role: the custom role silod, holding exactly
// SilodPermissions, bound to silod's account with
// SilodPredefinedRoles.
type SilodRole struct{ Env *Env }

// SilodRoleObserved is the role, if any, and which of the roles silod
// should hold its account holds.
type SilodRoleObserved struct {
	IAMEnabled bool  `json:"iamEnabled"`
	CRMEnabled bool  `json:"crmEnabled"`
	Role       *Role `json:"role,omitempty"`
	// Bound are the wanted roles (the custom role by its full name) the
	// account holds unconditionally on the project.
	Bound []string `json:"bound,omitempty"`
}

func (s *SilodRole) Name() string          { return "role" }
func (s *SilodRole) NewObserved() Observed { return &SilodRoleObserved{} }

func (s *SilodRole) want() *Role {
	return &Role{Title: "silod", Stage: "GA", Permissions: SilodPermissions,
		Description: owned(s.Env, "silod's role: only what its facade, fence and reconcile need")}
}

// roles are the roles silod's account is bound to.
func (s *SilodRole) roles() []string {
	return append([]string{RoleName(s.Env.Config.Project)}, SilodPredefinedRoles...)
}

func (s *SilodRole) member() string { return "serviceAccount:" + SilodEmail(s.Env.Config.Project) }

func (s *SilodRole) Observe(ctx context.Context) (Observed, error) {
	obs := &SilodRoleObserved{}
	var err error
	project := s.Env.Config.Project
	if obs.IAMEnabled, err = s.Env.Enabled(ctx, "iam.googleapis.com"); err != nil {
		return nil, err
	}
	if obs.CRMEnabled, err = s.Env.Enabled(ctx, "cloudresourcemanager.googleapis.com"); err != nil {
		return nil, err
	}
	if obs.IAMEnabled {
		r, err := s.Env.Cloud.IAM.GetRole(ctx, project, RoleID)
		switch {
		case IsNotFound(err):
		case err != nil:
			return nil, err
		default:
			obs.Role = r
		}
	}
	if obs.CRMEnabled {
		p, err := s.Env.Cloud.ResourceManager.GetIamPolicy(ctx, project)
		if err != nil {
			return nil, err
		}
		for _, role := range s.roles() {
			if p.Has(role, s.member()) {
				obs.Bound = append(obs.Bound, role)
			}
		}
	}
	return obs, nil
}

func (s *SilodRole) Plan(o Observed) []Change {
	obs := o.(*SilodRoleObserved)
	w := s.want()
	name := RoleName(s.Env.Config.Project)
	var out []Change
	switch r := obs.Role; {
	case r == nil:
		out = append(out, Change{Action: ActionCreate, Object: name,
			Detail: fmt.Sprintf("(%d permissions)", len(w.Permissions)), Estimate: 5 * time.Second})
	case r.Deleted:
		out = append(out, Change{Action: ActionUpdate, Object: name,
			Detail: fmt.Sprintf("undelete it, with %d permissions", len(w.Permissions)), Estimate: 5 * time.Second})
	default:
		if !isOurs(r.Description, s.Env.ID) {
			return []Change{notOurs(s.Env, name, "silod's role is install's, and holds exactly what install.md lists",
				"gcloud iam roles delete "+RoleID)}
		}
		if add, drop := diffSets(r.Permissions, w.Permissions); len(add)+len(drop) > 0 {
			var d []string
			for _, p := range add {
				d = append(d, "+"+p)
			}
			for _, p := range drop {
				d = append(d, "-"+p)
			}
			out = append(out, Change{Action: ActionUpdate, Object: name, Detail: "permissions " + strings.Join(d, ", ")})
		} else if r.Title != w.Title || r.Description != w.Description || r.Stage != w.Stage {
			out = append(out, Change{Action: ActionUpdate, Object: name, Detail: "title, description and stage"})
		}
	}
	bound := map[string]bool{}
	for _, b := range obs.Bound {
		bound[b] = true
	}
	for _, role := range s.roles() {
		if !bound[role] {
			out = append(out, Change{Action: ActionCreate, Object: role, Detail: "bound to " + SilodAccountID})
		}
	}
	if len(out) == 0 {
		return []Change{{Action: ActionOK, Object: name, Detail: fmt.Sprintf("(%d permissions), bound with %s",
			len(w.Permissions), strings.Join(SilodPredefinedRoles, " and "))}}
	}
	return out
}

func (s *SilodRole) PlanRemove(o Observed) []Change {
	obs := o.(*SilodRoleObserved)
	name := RoleName(s.Env.Config.Project)
	var out []Change
	for _, role := range obs.Bound {
		out = append(out, Change{Action: ActionDelete, Object: role, Detail: "bound to " + SilodAccountID})
	}
	switch r := obs.Role; {
	case r == nil || r.Deleted:
	case !isOurs(r.Description, s.Env.ID):
		out = append(out, Change{Action: ActionOK, Object: name, Detail: "is not this install's; left alone"})
	default:
		out = append(out, Change{Action: ActionDelete, Object: name})
	}
	if len(out) == 0 {
		return []Change{{Action: ActionOK, Detail: "no role " + name}}
	}
	return out
}

func (s *SilodRole) Apply(ctx context.Context, changes []Change) error {
	project := s.Env.Config.Project
	name := RoleName(project)
	var grant, revoke []string
	deleteRole := false
	for _, ch := range changes {
		var err error
		binding := strings.HasPrefix(ch.Detail, "bound to ")
		switch {
		case binding && ch.Action == ActionCreate:
			grant = append(grant, ch.Object)
		case binding && ch.Action == ActionDelete:
			revoke = append(revoke, ch.Object)
		case ch.Object == name && ch.Action == ActionCreate:
			err = s.Env.Cloud.IAM.CreateRole(ctx, project, RoleID, s.want())
			if IsAlreadyExists(err) {
				err = s.Env.Cloud.IAM.UpdateRole(ctx, project, RoleID, s.want())
			}
		case ch.Object == name && ch.Action == ActionUpdate:
			err = s.Env.Cloud.IAM.UpdateRole(ctx, project, RoleID, s.want())
		case ch.Object == name && ch.Action == ActionDelete:
			deleteRole = true
		}
		if err != nil {
			return err
		}
	}
	if len(grant)+len(revoke) > 0 {
		rm := s.Env.Cloud.ResourceManager
		err := updatePolicy(ctx,
			func(ctx context.Context) (*Policy, error) { return rm.GetIamPolicy(ctx, project) },
			func(ctx context.Context, p *Policy) error { return rm.SetIamPolicy(ctx, project, p) },
			func(p *Policy) bool {
				changed := false
				for _, r := range grant {
					changed = p.Grant(r, s.member()) || changed
				}
				for _, r := range revoke {
					changed = p.Revoke(r, s.member()) || changed
				}
				return changed
			})
		if err != nil {
			return err
		}
	}
	if deleteRole {
		if err := s.Env.Cloud.IAM.DeleteRole(ctx, project, RoleID); err != nil && !IsNotFound(err) {
			return err
		}
	}
	return nil
}

// isOurs reports whether desc is install id's.
func isOurs(desc, id string) bool {
	got, ok := ownedBy(desc)
	return ok && got == id
}

// updatePolicy is IAM's read-modify-write: it reads the policy, lets
// mutate change it, and writes it back, starting over when the etag
// went stale. mutate returning false writes nothing.
func updatePolicy(ctx context.Context, get func(context.Context) (*Policy, error),
	set func(context.Context, *Policy) error, mutate func(*Policy) bool) error {
	for attempt := 1; ; attempt++ {
		p, err := get(ctx)
		if err != nil {
			return err
		}
		if !mutate(p) {
			return nil
		}
		err = set(ctx, p)
		if err == nil || !IsConflict(err) || attempt == 5 {
			return err
		}
	}
}

// diffSets is what want has that have lacks, and what have has that
// want lacks, each sorted.
func diffSets(have, want []string) (add, drop []string) {
	h, w := map[string]bool{}, map[string]bool{}
	for _, x := range have {
		h[x] = true
	}
	for _, x := range want {
		w[x] = true
		if !h[x] {
			add = append(add, x)
		}
	}
	for _, x := range have {
		if !w[x] {
			drop = append(drop, x)
		}
	}
	sort.Strings(add)
	sort.Strings(drop)
	return add, drop
}
