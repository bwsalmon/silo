package directory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bwsalmon/silo/hub/identity"
	"github.com/bwsalmon/silo/hub/registry"
)

// ErrNotAdmitted is a node whose owner may not serve every repository
// it lists.
var ErrNotAdmitted = errors.New("directory: node not admitted")

// admitTimeout bounds Admit's queries: it runs inside a node's
// handshake, which has no context of its own to cancel.
const admitTimeout = 10 * time.Second

// Admit is rendezvous.Server.Admit: owner may register n only if, for
// every repository n lists, owner is a member (or admin) of the team
// that owns it. A repository with no owning team is refused, so nothing
// is served by accident.
//
// A node that may serve only some of its repositories is refused
// whole, not registered for the subset (docs/hub.md §5.3), and the
// refusal names every repository it may not serve, so the node's owner
// fixes its list, or asks a team admin, in one round. A node registered
// for less than it asked for would sit in the directory looking healthy
// while the work it was set up for went nowhere.
//
// Membership is by the owner's email, as user:<email>: a node token
// proves a principal but not the groups the identity provider put it
// in when it signed in.
func (s *Store) Admit(owner string, n registry.Node) error {
	ctx, cancel := context.WithTimeout(context.Background(), admitTimeout)
	defer cancel()
	return s.AdmitContext(ctx, owner, n)
}

// AdmitContext is Admit with a context.
func (s *Store) AdmitContext(ctx context.Context, owner string, n registry.Node) error {
	if len(n.Repos) == 0 {
		return nil
	}
	a, err := s.Admission(ctx, []string{owner}, n.Repos)
	if err != nil {
		return err
	}
	return a.Admit(owner, n)
}

// Admission is one read of the directory for many admission checks:
// the teams of some owners, and the owning team of some repositories.
// It answers Admit as the store would have when it was read, so a
// caller that must check afresh (docs/hub.md §5.3) reads a new one
// rather than keeping it.
type Admission struct {
	// teams maps each owner's user:<email> to the teams it is in.
	teams map[string]map[string]Role
	// repos maps each repository to its owning team, "" for none.
	repos map[string]string
}

// ErrNotRead is an Admission asked about an owner or a repository it
// was not read for: not a refusal, but no answer.
var ErrNotRead = errors.New("directory: admission not read for it")

// maxAdmissionArgs bounds the placeholders in one of Admission's
// queries; more repositories than this take another query each.
const maxAdmissionArgs = 500

// Admission reads, in two queries whatever their number, the teams of
// owners and the owning team of repos, for Admission.Admit. An owner or
// repository that is not a valid one is left out, for Admit to refuse
// with the error AdmitContext gives for it.
func (s *Store) Admission(ctx context.Context, owners, repos []string) (*Admission, error) {
	a := &Admission{teams: map[string]map[string]Role{}, repos: map[string]string{}}
	var principals []string
	for _, o := range owners {
		if u, err := userPrincipal(o); err == nil && a.teams[u] == nil {
			a.teams[u] = map[string]Role{}
			principals = append(principals, u)
		}
	}
	var rs []string
	for _, r := range repos {
		if n, err := normRepo(r); err == nil {
			if _, ok := a.repos[n]; !ok {
				a.repos[n] = ""
				rs = append(rs, n)
			}
		}
	}
	if err := s.membershipsOf(ctx, principals, a.teams); err != nil {
		return nil, err
	}
	if err := s.repoOwners(ctx, rs, a.repos); err != nil {
		return nil, err
	}
	return a, nil
}

// Admit is Store.AdmitContext, answered from what a read: owner may
// register n only if, for every repository n lists, owner is a member
// (or admin) of the team that owns it. A refusal wraps ErrNotAdmitted
// and names every repository refused; an owner or repository a was not
// read for is ErrNotRead.
func (a *Admission) Admit(owner string, n registry.Node) error {
	if len(n.Repos) == 0 {
		return nil
	}
	u, err := userPrincipal(owner)
	if err != nil {
		return err
	}
	o := strings.TrimPrefix(u, UserPrefix)
	teams, ok := a.teams[u]
	if !ok {
		return fmt.Errorf("%w: owner %s", ErrNotRead, o)
	}
	var refused []string
	for _, repo := range n.Repos {
		r, err := normRepo(repo)
		if err != nil {
			return err
		}
		team, ok := a.repos[r]
		switch {
		case !ok:
			return fmt.Errorf("%w: repository %s", ErrNotRead, r)
		case team == "":
			refused = append(refused, repo+" (no team owns it)")
		case teams[team] == "":
			refused = append(refused, fmt.Sprintf("%s (owned by team %s, which %s is not in)", repo, team, o))
		}
	}
	if len(refused) > 0 {
		return fmt.Errorf("%w: %s may not serve %s", ErrNotAdmitted, o, strings.Join(refused, ", "))
	}
	return nil
}

// ValidateNodeUI validates n.UI.Team when n offers a UI with a team
// name (docs/grain-ui-proxy.md §3): it keeps ui.team only if owner is a
// member or admin of ui.team and ui.team owns every repository n lists.
// When that check fails, it drops ui.team (falling back to owner-only
// access) and records ui.reason without refusing the node registration.
func (s *Store) ValidateNodeUI(ctx context.Context, owner string, n *registry.Node) error {
	if n == nil || n.UI == nil || !n.UI.Offered || strings.TrimSpace(n.UI.Team) == "" {
		return nil
	}
	if s == nil {
		uiCopy := *n.UI
		uiCopy.Team = ""
		uiCopy.Reason = "directory is unavailable"
		n.UI = &uiCopy
		return nil
	}
	a, err := s.Admission(ctx, []string{owner}, n.Repos)
	if err != nil {
		return err
	}
	return a.ValidateNodeUI(owner, n)
}

// ValidateNodeUI is Store.ValidateNodeUI answered from an Admission
// snapshot.
func (a *Admission) ValidateNodeUI(owner string, n *registry.Node) error {
	if n == nil || n.UI == nil || !n.UI.Offered {
		return nil
	}
	team := strings.TrimSpace(n.UI.Team)
	if team == "" {
		return nil
	}
	uiCopy := *n.UI
	u, err := userPrincipal(owner)
	if err != nil {
		uiCopy.Team = ""
		uiCopy.Reason = fmt.Sprintf("invalid owner %q", owner)
		n.UI = &uiCopy
		return nil
	}
	o := strings.TrimPrefix(u, UserPrefix)
	teams, ok := a.teams[u]
	if !ok {
		return fmt.Errorf("%w: owner %s", ErrNotRead, o)
	}
	if teams[team] == "" {
		uiCopy.Team = ""
		uiCopy.Reason = fmt.Sprintf("owner %s is not a member of team %s", o, team)
		n.UI = &uiCopy
		return nil
	}
	for _, repo := range n.Repos {
		r, err := normRepo(repo)
		if err != nil {
			uiCopy.Team = ""
			uiCopy.Reason = fmt.Sprintf("invalid repository %q", repo)
			n.UI = &uiCopy
			return nil
		}
		repoTeam, ok := a.repos[r]
		if !ok {
			return fmt.Errorf("%w: repository %s", ErrNotRead, r)
		}
		if repoTeam != team {
			uiCopy.Team = ""
			uiCopy.Reason = fmt.Sprintf("repository %s is not owned by team %s", r, team)
			n.UI = &uiCopy
			return nil
		}
	}
	uiCopy.Team = team
	n.UI = &uiCopy
	return nil
}

// Access is what one signed-in human may see and change: the
// visibility rules of docs/hub.md §5.3.
type Access struct {
	identity.Principal
	// HubAdmin creates and deletes teams, and sees everything.
	HubAdmin bool
	// Teams are the teams the principal is in, as user:<email> or as
	// group:<name> for any of its groups, with the higher role where
	// both match.
	Teams map[string]Role
	// repos maps each repository the principal's teams own to its
	// ownership.
	repos map[string]ownership
}

// ownership is one team_repos row: the team that owns a repository, and
// whether it was given the repository's history (Store.TransferRepo).
type ownership struct {
	team    string
	history bool
}

// Access loads p's teams. hubAdmin is the hub's own configuration's
// say, not the store's.
func (s *Store) Access(ctx context.Context, p identity.Principal, hubAdmin bool) (*Access, error) {
	principals := []string{}
	if u, err := userPrincipal(p.Email); err == nil {
		principals = append(principals, u)
	}
	for _, g := range p.Groups {
		if n, err := normPrincipal(GroupPrefix + g); err == nil {
			principals = append(principals, n)
		}
	}
	teams, err := s.memberships(ctx, principals)
	if err != nil {
		return nil, err
	}
	repos, err := s.reposOf(ctx, teams)
	if err != nil {
		return nil, err
	}
	return &Access{Principal: p, HubAdmin: hubAdmin, Teams: teams, repos: repos}, nil
}

// SeesTeam is whether the principal may see team and its tasks: a
// team-targeted task is visible to the team's members.
func (a *Access) SeesTeam(team string) bool {
	return a.HubAdmin || a.Teams[team] != ""
}

// AdminOf is whether the principal manages team.
func (a *Access) AdminOf(team string) bool {
	return a.HubAdmin || a.Teams[team] == Admin
}

// SeesRepo is whether repo is one of the principal's teams'
// repositories: they may file tasks in it, and see the nodes serving
// it.
func (a *Access) SeesRepo(repo string) bool {
	if a.HubAdmin {
		return true
	}
	_, ok := a.repos[strings.ToLower(repo)]
	return ok
}

// SeesTask is whether the principal may see a task, its events and its
// artifacts, given the team it was filed under and its repository, ""
// for none: they are in that team, or their team owns the repository
// and was given its history by a hub admin (Store.TransferRepo).
// Owning a repository is not enough on its own, so a team that is
// assigned one after a release or a team's deletion does not inherit
// the tasks filed in it before (SA1-M2).
func (a *Access) SeesTask(team, repo string) bool {
	if a.SeesTeam(team) {
		return true
	}
	if repo == "" {
		return false
	}
	o, ok := a.repos[strings.ToLower(repo)]
	return ok && o.history
}

// SeesNode is whether the principal may see a node in the directory:
// their own node, or one serving a repository of one of their teams.
func (a *Access) SeesNode(e registry.Entry) bool {
	if a.HubAdmin || strings.EqualFold(e.Owner, a.Email) {
		return true
	}
	for _, r := range e.Repos {
		if a.SeesRepo(r) {
			return true
		}
	}
	return false
}
