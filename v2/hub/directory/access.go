package directory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bwsalmon/silo/v2/hub/identity"
	"github.com/bwsalmon/silo/v2/hub/registry"
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
// Membership is by the owner's email: a node token proves a principal
// but not the groups the identity provider put it in when it signed in.
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
	o, err := normPrincipal(owner)
	if err != nil {
		return err
	}
	teams, err := s.memberships(ctx, []string{o})
	if err != nil {
		return err
	}
	var refused []string
	for _, repo := range n.Repos {
		team, err := s.RepoOwner(ctx, repo)
		if err != nil {
			return err
		}
		switch {
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

// Access is what one signed-in human may see and change: the
// visibility rules of docs/hub.md §5.3.
type Access struct {
	identity.Principal
	// HubAdmin creates and deletes teams, and sees everything.
	HubAdmin bool
	// Teams are the teams the principal is in, by email or by any of
	// its groups, with the higher role where both match.
	Teams map[string]Role
	// repos maps each repository the principal's teams own to its team.
	repos map[string]string
}

// Access loads p's teams. hubAdmin is the hub's own configuration's
// say, not the store's.
func (s *Store) Access(ctx context.Context, p identity.Principal, hubAdmin bool) (*Access, error) {
	principals := []string{}
	for _, x := range append([]string{p.Email}, p.Groups...) {
		if n, err := normPrincipal(x); err == nil {
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

// SeesRepo is whether the principal may see repo's tasks and events:
// it is one of their teams' repositories.
func (a *Access) SeesRepo(repo string) bool {
	if a.HubAdmin {
		return true
	}
	_, ok := a.repos[strings.ToLower(repo)]
	return ok
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
