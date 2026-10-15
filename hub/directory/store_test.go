package directory

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/hub/identity"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/internal/pgtest"
)

// eachStore runs f against SQLite and, where its binaries are
// installed, PostgreSQL.
func eachStore(t *testing.T, f func(t *testing.T, s *Store)) {
	t.Run("sqlite", func(t *testing.T) {
		s, err := Open(context.Background(), "sqlite:"+t.TempDir()+"/hub.db")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		f(t, s)
	})
	t.Run("postgres", func(t *testing.T) {
		pg := pgtest.Start(t)
		s, err := Open(context.Background(), pg.URL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		// Opening again finds the tables there already.
		s2, err := Open(context.Background(), pg.URL)
		if err != nil {
			t.Fatal(err)
		}
		s2.Close()
		f(t, s)
	})
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// seed makes two teams: "infra", owning bwsalmon/silo, with alice as
// admin and the group eng@example.com as a member; and "web", owning
// bwsalmon/web, with bob.
func seed(t *testing.T, s *Store) {
	ctx := context.Background()
	check(t, s.CreateTeam(ctx, "infra", "Infrastructure"))
	check(t, s.CreateTeam(ctx, "web", ""))
	check(t, s.SetMember(ctx, "infra", "Alice@Example.com", Admin))
	check(t, s.SetMember(ctx, "infra", "eng@example.com", Member))
	check(t, s.SetMember(ctx, "web", "bob@example.com", Member))
	check(t, s.AssignRepo(ctx, "infra", "Bwsalmon/Silo"))
	check(t, s.AssignRepo(ctx, "web", "bwsalmon/web"))
}

func TestTeams(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		seed(t, s)
		if err := s.CreateTeam(ctx, "infra", "again"); !errors.Is(err, ErrExists) {
			t.Errorf("duplicate team: %v, want ErrExists", err)
		}
		for _, id := range []string{"", "Upper", "has space", strings.Repeat("a", 64)} {
			if err := s.CreateTeam(ctx, id, ""); !errors.Is(err, ErrInvalid) {
				t.Errorf("team %q: %v, want ErrInvalid", id, err)
			}
		}
		if err := s.SetMember(ctx, "nope", "x@example.com", Member); !errors.Is(err, ErrNotFound) {
			t.Errorf("member of a missing team: %v, want ErrNotFound", err)
		}
		if err := s.SetMember(ctx, "infra", "x@example.com", "owner"); !errors.Is(err, ErrInvalid) {
			t.Errorf("bad role: %v, want ErrInvalid", err)
		}
		if err := s.AssignRepo(ctx, "web", "bwsalmon/silo"); !errors.Is(err, ErrOwned) {
			t.Errorf("another team's repo: %v, want ErrOwned", err)
		}
		check(t, s.AssignRepo(ctx, "infra", "bwsalmon/silo")) // already its own: no change
		check(t, s.SetMember(ctx, "web", "bob@example.com", Admin))

		teams, err := s.Teams(ctx)
		check(t, err)
		if len(teams) != 2 || teams[0].ID != "infra" || teams[0].Name != "Infrastructure" || teams[1].Name != "web" {
			t.Fatalf("teams %+v", teams)
		}
		if m := teams[0].Members; len(m) != 2 || m["alice@example.com"] != Admin || m["eng@example.com"] != Member {
			t.Errorf("infra's members %v", m)
		}
		if r := teams[0].Repos; len(r) != 1 || r[0] != "bwsalmon/silo" {
			t.Errorf("infra's repos %v", r)
		}
		if teams[1].Members["bob@example.com"] != Admin {
			t.Errorf("bob's role was not changed: %v", teams[1].Members)
		}

		check(t, s.ReleaseRepo(ctx, "web", "bwsalmon/web"))
		if owner, _ := s.RepoOwner(ctx, "bwsalmon/web"); owner != "" {
			t.Errorf("released repo still owned by %q", owner)
		}
		check(t, s.AssignRepo(ctx, "web", "bwsalmon/web"))
		check(t, s.RemoveMember(ctx, "infra", "eng@example.com"))
		if err := s.RemoveMember(ctx, "infra", "eng@example.com"); !errors.Is(err, ErrNotFound) {
			t.Errorf("removing twice: %v", err)
		}
		check(t, s.DeleteTeam(ctx, "web"))
		if owner, _ := s.RepoOwner(ctx, "bwsalmon/web"); owner != "" {
			t.Errorf("a deleted team's repo is still owned by %q", owner)
		}
		check(t, s.CreateTeam(ctx, "web", "")) // its memberships went with it
		if teams, _ := s.Teams(ctx); len(teams[1].Members) != 0 || len(teams[1].Repos) != 0 {
			t.Errorf("recreated team %+v", teams[1])
		}
	})
}

func TestAdmit(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		seed(t, s)
		node := func(repos ...string) registry.Node {
			n, err := registry.Validate(registry.Node{ID: "ws-1", Repos: repos, Slots: 1})
			check(t, err)
			return n
		}
		check(t, s.Admit("alice@example.com", node("bwsalmon/silo")))
		check(t, s.Admit("ALICE@example.com", node("BWSALMON/SILO")))
		check(t, s.Admit("bob@example.com", node("bwsalmon/web")))
		check(t, s.Admit("nobody@example.com", node())) // serves nothing

		// Partial sets are refused whole, naming every repository the
		// owner may not serve, and only those.
		err := s.Admit("alice@example.com", node("bwsalmon/silo", "bwsalmon/web", "bwsalmon/orphan"))
		if !errors.Is(err, ErrNotAdmitted) {
			t.Fatalf("partial set: %v, want ErrNotAdmitted", err)
		}
		msg := err.Error()
		if !strings.Contains(msg, "bwsalmon/web (owned by team web") || !strings.Contains(msg, "bwsalmon/orphan (no team owns it)") ||
			strings.Contains(msg, "bwsalmon/silo") {
			t.Errorf("refusal %q should name web and orphan, and not silo", msg)
		}
		// A member through a group is not admitted: a node token proves
		// an email, not the groups its owner was in when they signed in.
		if err := s.Admit("carol@example.com", node("bwsalmon/silo")); !errors.Is(err, ErrNotAdmitted) {
			t.Errorf("carol, in no team: %v", err)
		}
		_ = ctx
	})
}

func TestNodeTokens(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		tok, rec, err := s.MintNodeToken(ctx, "Alice@example.com", "ws-1")
		check(t, err)
		if !strings.HasPrefix(tok, tokenPrefix) || rec.Owner != "alice@example.com" || rec.Node != "ws-1" {
			t.Fatalf("minted %q %+v", tok, rec)
		}
		owner, node, err := s.VerifyNodeToken(ctx, tok)
		if err != nil || owner != "alice@example.com" || node != "ws-1" {
			t.Fatalf("verify: %q %q %v", owner, node, err)
		}
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		if id, err := s.Authenticate(r); err != nil || id.Owner != "alice@example.com" || id.Node != "ws-1" {
			t.Errorf("Authenticate: %+v %v", id, err)
		}
		for _, bad := range []string{"", "Basic " + tok, "Bearer " + tok + "x", "Bearer " + strings.TrimPrefix(tok, tokenPrefix)} {
			r.Header.Set("Authorization", bad)
			if id, err := s.Authenticate(r); err == nil {
				t.Errorf("Authorization %q proved %+v", bad, id)
			}
		}

		// Only the hash is stored.
		var stored string
		check(t, s.db.QueryRowContext(ctx, `SELECT hash FROM node_tokens WHERE id = '`+rec.ID+`'`).Scan(&stored))
		if strings.Contains(tok, stored) || stored != hashToken(tok) {
			t.Errorf("stored %q for token %q", stored, tok)
		}

		if _, _, err := s.MintNodeToken(ctx, "bob@example.com", "ws-1"); !errors.Is(err, ErrNodeOwner) {
			t.Errorf("bob minting alice's node: %v, want ErrNodeOwner", err)
		}
		if _, _, err := s.MintNodeToken(ctx, "bob@example.com", "bad id"); !errors.Is(err, ErrInvalid) {
			t.Errorf("bad node ID: %v, want ErrInvalid", err)
		}

		// Minting again retires the first.
		tok2, rec2, err := s.MintNodeToken(ctx, "alice@example.com", "ws-1")
		check(t, err)
		if _, _, err := s.VerifyNodeToken(ctx, tok); err == nil {
			t.Error("the first token still verifies after a second was minted")
		}
		if _, _, err := s.VerifyNodeToken(ctx, tok2); err != nil {
			t.Error(err)
		}
		toks, err := s.NodeTokens(ctx, "alice@example.com")
		check(t, err)
		if len(toks) != 2 || (toks[0].Revoked == nil) == (toks[1].Revoked == nil) {
			t.Errorf("alice's tokens %+v", toks)
		}

		check(t, s.RevokeNodeToken(ctx, rec2.ID))
		if _, _, err := s.VerifyNodeToken(ctx, tok2); err == nil {
			t.Error("a revoked token verifies")
		}
		if err := s.RevokeNodeToken(ctx, rec2.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("revoking twice: %v", err)
		}
		if got, err := s.NodeToken(ctx, rec2.ID); err != nil || got.Revoked == nil {
			t.Errorf("revoked record %+v %v", got, err)
		}
		// With every token for it revoked, the node ID is free again.
		if _, _, err := s.MintNodeToken(ctx, "bob@example.com", "ws-1"); err != nil {
			t.Errorf("bob minting a released node ID: %v", err)
		}
		if all, _ := s.NodeTokens(ctx, ""); len(all) != 3 {
			t.Errorf("all tokens %+v", all)
		}
	})
}

func TestAccess(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		seed(t, s)
		alice, err := s.Access(ctx, identity.Principal{Email: "alice@example.com"}, false)
		check(t, err)
		carol, err := s.Access(ctx, identity.Principal{Email: "carol@example.com", Groups: []string{"ENG@example.com"}}, false)
		check(t, err)
		dave, err := s.Access(ctx, identity.Principal{Email: "dave@example.com"}, false)
		check(t, err)
		root, err := s.Access(ctx, identity.Principal{Email: "root@example.com"}, true)
		check(t, err)

		if !alice.AdminOf("infra") || alice.AdminOf("web") || !alice.SeesTeam("infra") || alice.SeesTeam("web") {
			t.Errorf("alice %+v", alice)
		}
		if !carol.SeesTeam("infra") || carol.AdminOf("infra") || !carol.SeesRepo("bwsalmon/silo") || carol.SeesRepo("bwsalmon/web") {
			t.Errorf("carol, in infra through a group: %+v", carol)
		}
		if dave.SeesTeam("infra") || dave.SeesRepo("bwsalmon/silo") {
			t.Errorf("dave, in no team: %+v", dave)
		}
		if !root.AdminOf("web") || !root.SeesRepo("anything/at-all") {
			t.Errorf("hub admin %+v", root)
		}

		webNode := registry.Entry{Node: registry.Node{ID: "w", Repos: []string{"bwsalmon/web"}}, Owner: "bob@example.com"}
		siloNode := registry.Entry{Node: registry.Node{ID: "s", Repos: []string{"bwsalmon/silo"}}, Owner: "erin@example.com"}
		daveNode := registry.Entry{Node: registry.Node{ID: "d"}, Owner: "Dave@example.com"}
		for _, c := range []struct {
			who  *Access
			node registry.Entry
			want bool
		}{
			{alice, siloNode, true}, {alice, webNode, false}, {carol, siloNode, true},
			{dave, daveNode, true}, {dave, siloNode, false}, {root, webNode, true},
		} {
			if got := c.who.SeesNode(c.node); got != c.want {
				t.Errorf("%s sees node %s: %v, want %v", c.who.Email, c.node.ID, got, c.want)
			}
		}
	})
}

func TestTeamRepos(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		seed(t, s)
		check(t, s.AssignRepo(ctx, "infra", "bwsalmon/grain"))
		repos, err := s.TeamRepos(ctx, "infra")
		if err != nil || strings.Join(repos, " ") != "bwsalmon/grain bwsalmon/silo" {
			t.Errorf("TeamRepos(infra) = %v, %v", repos, err)
		}
		check(t, s.CreateTeam(ctx, "empty", ""))
		if repos, err := s.TeamRepos(ctx, "empty"); err != nil || len(repos) != 0 {
			t.Errorf("TeamRepos(empty) = %v, %v", repos, err)
		}
		if _, err := s.TeamRepos(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Errorf("TeamRepos(nope) = %v, want ErrNotFound", err)
		}
	})
}
