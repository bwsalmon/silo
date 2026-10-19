package directory

import (
	"context"
	"errors"
	"fmt"
	"net/url"
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
		case teams[team] != Member && teams[team] != Admin:
			refused = append(refused, fmt.Sprintf("%s (owned by team %s, where %s has role %s, want member or admin)", repo, team, o, teams[team]))
		}
	}
	if len(refused) > 0 {
		return fmt.Errorf("%w: %s may not serve %s", ErrNotAdmitted, o, strings.Join(refused, ", "))
	}
	return nil
}

// AuthorizeOwnerRepo reports whether owner has role member or admin in the
// team that owns repoURL (accepting either an "owner/name" slug or a full
// repository/file URL). Viewer and proposer roles are not authorized to serve
// or subscribe to a team's repositories from a node.
func (s *Store) AuthorizeOwnerRepo(ctx context.Context, owner, repoURL string) bool {
	if s == nil {
		return false
	}
	slug := extractRepoSlug(repoURL)
	if slug == "" {
		return false
	}
	team, err := s.RepoOwner(ctx, slug)
	if err != nil || team == "" {
		return false
	}
	acc, err := s.Access(ctx, identity.Principal{Email: owner}, false)
	if err != nil {
		return false
	}
	return acc.MemberOf(team)
}

func extractRepoSlug(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || u.Host == "" {
			return ""
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) < 2 {
			return ""
		}
		s = parts[0] + "/" + strings.TrimSuffix(parts[1], ".git")
	} else {
		s = strings.TrimSuffix(s, ".git")
	}
	norm, err := normRepo(s)
	if err != nil {
		return ""
	}
	return norm
}

// ValidateNodeUI validates n.UI.Team when n offers a UI with a team
// name (docs/grain-ui-proxy.md §3): it keeps ui.team only if owner is a
// member or admin of ui.team and ui.team owns every repository n lists.
// When that check fails, it drops ui.team (falling back to owner-only
// access) and records ui.reason without refusing the node registration.
func (s *Store) ValidateNodeUI(ctx context.Context, owner string, n *registry.Node) error {
	if n == nil {
		return nil
	}
	if n.UI != nil && !n.UI.Offered {
		return nil
	}
	if n.UI == nil && !n.Supports(registry.FeatureNodeUI) {
		return nil
	}
	if s == nil {
		if n.UI != nil && strings.TrimSpace(n.UI.Team) != "" {
			uiCopy := *n.UI
			uiCopy.Team = ""
			uiCopy.Reason = "directory is unavailable"
			n.UI = &uiCopy
		}
		return nil
	}
	a, err := s.Admission(ctx, []string{owner}, n.Repos)
	if err != nil {
		return err
	}
	return a.ValidateNodeUI(owner, n)
}

// ValidateNodeUI is Store.ValidateNodeUI answered from an Admission
// snapshot. When n offers a UI (n.UI.Offered or FeatureNodeUI) and omits
// n.UI.Team, it infers ui.team if all of n.Repos are owned by a single
// team in the directory and owner is a member or admin of that team.
func (a *Admission) ValidateNodeUI(owner string, n *registry.Node) error {
	if n == nil {
		return nil
	}
	if n.UI != nil && !n.UI.Offered {
		return nil
	}
	if n.UI == nil && !n.Supports(registry.FeatureNodeUI) {
		return nil
	}
	var uiCopy registry.NodeUI
	if n.UI != nil {
		uiCopy = *n.UI
	} else {
		uiCopy = registry.NodeUI{Offered: true}
	}
	team := strings.TrimSpace(uiCopy.Team)
	if team == "" {
		if uiCopy.Reason != "" {
			if n.UI == nil {
				n.UI = &uiCopy
			}
			return nil
		}
		u, err := userPrincipal(owner)
		if err != nil {
			return nil
		}
		o := strings.TrimPrefix(u, UserPrefix)
		teams, ok := a.teams[u]
		if !ok {
			return fmt.Errorf("%w: owner %s", ErrNotRead, o)
		}
		if len(n.Repos) == 0 {
			if r := teams[n.ID]; r == Member || r == Admin {
				uiCopy.Offered = true
				uiCopy.Team = n.ID
				n.UI = &uiCopy
			} else if n.UI == nil {
				n.UI = &uiCopy
			}
			return nil
		}
		inferred := ""
		for _, repo := range n.Repos {
			r, err := normRepo(repo)
			if err != nil {
				return nil
			}
			repoTeam, ok := a.repos[r]
			if !ok {
				return fmt.Errorf("%w: repository %s", ErrNotRead, r)
			}
			if repoTeam == "" || (inferred != "" && repoTeam != inferred) {
				return nil
			}
			inferred = repoTeam
		}
		if inferred != "" {
			if r := teams[inferred]; r == Member || r == Admin {
				uiCopy.Offered = true
				uiCopy.Team = inferred
				n.UI = &uiCopy
			}
		}
		return nil
	}
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
	if r := teams[team]; r != Member && r != Admin {
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
	// HubAdmin creates and deletes teams, and sees everything, when
	// admin mode is active on the request.
	HubAdmin bool
	// CanHubAdmin is whether the principal is configured as a hub admin
	// (API.HubAdmins), regardless of the request's X-Silo-Admin-Mode.
	CanHubAdmin bool
	// Teams are the teams the principal is in, as user:<email> or as
	// group:<name> for any of its groups, with the higher role where
	// both match.
	Teams map[string]Role
	// repos maps each repository the principal's teams own to its
	// ownership.
	repos map[string]ownership
	// teamSettings caches explicit team_settings rows for role/proposal
	// resolution.
	teamSettings map[string]Settings
	store        *Store
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
	teamSettings, err := s.AllTeamSettings(ctx)
	if err != nil {
		return nil, err
	}
	return &Access{
		Principal:    p,
		HubAdmin:     hubAdmin,
		CanHubAdmin:  hubAdmin,
		Teams:        teams,
		repos:        repos,
		teamSettings: teamSettings,
		store:        s,
	}, nil
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

// MemberOf is whether the principal has role member or admin in team
// (or is an active hub admin). Viewer and proposer are not members.
func (a *Access) MemberOf(team string) bool {
	if a == nil {
		return false
	}
	if a.HubAdmin {
		return true
	}
	r := a.Teams[team]
	return r == Member || r == Admin
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

// NodeUIRole resolves the caller's effective role on e's proxied Node UI
// ("owner", "admin", "member", "proposer", "viewer", or "" if denied),
// along with the validated/inferred ui.team and that team's
// require_approval policy.
func (a *Access) NodeUIRole(e registry.Entry) (role, team string, requireApproval bool) {
	if a == nil || a.Email == "" {
		return "", "", false
	}
	if e.UI != nil && !e.UI.Offered {
		return "", "", false
	}
	offersUI := (e.UI != nil && e.UI.Offered) || (e.UI == nil && e.Supports(registry.FeatureNodeUI))
	if offersUI && (e.UI == nil || e.UI.Reason == "") && a.store != nil {
		_ = a.store.ValidateNodeUI(context.Background(), e.Owner, &e.Node)
	}
	if offersUI && e.UI != nil && e.UI.Team != "" {
		team = e.UI.Team
		requireApproval = TeamSettingsFrom(a.teamSettings, team).RequireApproval
	}
	if e.Owner != "" && strings.EqualFold(e.Owner, a.Email) {
		return "owner", team, requireApproval
	}
	if !offersUI || team == "" {
		return "", team, requireApproval
	}
	switch a.Teams[team] {
	case Admin:
		return "admin", team, requireApproval
	case Member:
		return "member", team, requireApproval
	case Proposer:
		return "proposer", team, requireApproval
	case Viewer:
		return "viewer", team, requireApproval
	case "":
		readOnlyCap := e.UI != nil && strings.EqualFold(strings.TrimSpace(e.UI.Access), "read-only")
		if !readOnlyCap && TeamSettingsFrom(a.teamSettings, team).AnyoneMayPropose {
			return "proposer", team, requireApproval
		}
	}
	return "", team, requireApproval
}

// ResolveEntry annotates e with the caller's ViewerRole, RequireApproval,
// and inferred UI.Team, and reports whether the caller may see e in the
// node directory.
func (a *Access) ResolveEntry(e registry.Entry) (registry.Entry, bool) {
	if a == nil {
		return e, false
	}
	offersUI := (e.UI != nil && e.UI.Offered) || (e.UI == nil && e.Supports(registry.FeatureNodeUI))
	if offersUI && (e.UI == nil || e.UI.Reason == "") && a.store != nil {
		_ = a.store.ValidateNodeUI(context.Background(), e.Owner, &e.Node)
	}
	uiRole, uiTeam, reqApproval := a.NodeUIRole(e)
	viewerRole := uiRole
	if viewerRole == "" {
		if e.Owner != "" && a.Email != "" && strings.EqualFold(e.Owner, a.Email) {
			viewerRole = "owner"
		} else {
			var best Role
			var bestTeam string
			if role := a.Teams[e.ID]; role != "" {
				best = role
				bestTeam = e.ID
			}
			for _, r := range e.Repos {
				if own, ok := a.repos[strings.ToLower(r)]; ok {
					if role := a.Teams[own.team]; role.rank() > best.rank() {
						best = role
						bestTeam = own.team
					}
				}
			}
			if best != "" {
				viewerRole = string(best)
				if uiTeam == "" && bestTeam != "" {
					reqApproval = TeamSettingsFrom(a.teamSettings, bestTeam).RequireApproval
				}
			}
		}
	}
	e.ViewerRole = viewerRole
	e.RequireApproval = reqApproval
	visible := a.HubAdmin || strings.EqualFold(e.Owner, a.Email) || uiRole != "" || a.Teams[e.ID] != ""
	if !visible {
		for _, r := range e.Repos {
			if a.SeesRepo(r) {
				visible = true
				break
			}
		}
	}
	return e, visible
}

// SeesNode is whether the principal may see a node in the directory:
// their own node, one serving a repository of one of their teams, or a
// node whose validated ui.team allows them access (including org-wide
// proposer access when anyone_may_propose is true).
func (a *Access) SeesNode(e registry.Entry) bool {
	_, ok := a.ResolveEntry(e)
	return ok
}

// DefaultInboxNodes returns the IDs of all deployments in entries where
// the caller's role is owner, admin, or member.
func (a *Access) DefaultInboxNodes(entries []registry.Entry) []string {
	out := []string{}
	if a == nil {
		return out
	}
	seen := map[string]bool{}
	for _, raw := range entries {
		e, ok := a.ResolveEntry(raw)
		if !ok || seen[e.ID] {
			continue
		}
		if e.ViewerRole == "owner" || e.ViewerRole == "admin" || e.ViewerRole == "member" {
			seen[e.ID] = true
			out = append(out, e.ID)
		}
	}
	return out
}

// EffectiveInboxNodes returns the caller's active Unified Inbox deployment
// IDs and whether they were explicitly configured by the user. When not yet
// configured, it defaults to DefaultInboxNodes(entries).
func (a *Access) EffectiveInboxNodes(ctx context.Context, entries []registry.Entry) ([]string, bool, error) {
	if a == nil {
		return []string{}, false, nil
	}
	if a.store == nil {
		return a.DefaultInboxNodes(entries), false, nil
	}
	prefs, err := a.store.UserPreferences(ctx, a.Email)
	if err != nil {
		return nil, false, err
	}
	if !prefs.InboxConfigured {
		return a.DefaultInboxNodes(entries), false, nil
	}
	return prefs.InboxNodes, true, nil
}
