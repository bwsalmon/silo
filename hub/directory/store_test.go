package directory

import (
	"context"
	"errors"
	"fmt"
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
	check(t, s.SetMember(ctx, "infra", "user:Alice@Example.com", Admin))
	check(t, s.SetMember(ctx, "infra", "group:eng@example.com", Member))
	check(t, s.SetMember(ctx, "web", "user:bob@example.com", Member))
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
		if err := s.SetMember(ctx, "nope", "user:x@example.com", Member); !errors.Is(err, ErrNotFound) {
			t.Errorf("member of a missing team: %v, want ErrNotFound", err)
		}
		if err := s.SetMember(ctx, "infra", "user:x@example.com", "owner"); !errors.Is(err, ErrInvalid) {
			t.Errorf("bad role: %v, want ErrInvalid", err)
		}
		// A principal says whether it is a user or a group (SA1-L6).
		for _, p := range []string{"x@example.com", "eng", "robot:x@example.com", "user:", "user:no-at", "group:", "group:a b", "user:a/b@example.com"} {
			if err := s.SetMember(ctx, "infra", p, Member); !errors.Is(err, ErrInvalid) {
				t.Errorf("SetMember(%q): %v, want ErrInvalid", p, err)
			}
		}
		if err := s.RemoveMember(ctx, "infra", "alice@example.com"); !errors.Is(err, ErrInvalid) {
			t.Errorf("RemoveMember with no prefix: %v, want ErrInvalid", err)
		}
		if err := s.AssignRepo(ctx, "web", "bwsalmon/silo"); !errors.Is(err, ErrOwned) {
			t.Errorf("another team's repo: %v, want ErrOwned", err)
		}
		check(t, s.AssignRepo(ctx, "infra", "bwsalmon/silo")) // already its own: no change
		check(t, s.SetMember(ctx, "web", "user:bob@example.com", Admin))

		teams, err := s.Teams(ctx)
		check(t, err)
		if len(teams) != 2 || teams[0].ID != "infra" || teams[0].Name != "Infrastructure" || teams[1].Name != "web" {
			t.Fatalf("teams %+v", teams)
		}
		if m := teams[0].Members; len(m) != 2 || m["user:alice@example.com"] != Admin || m["group:eng@example.com"] != Member {
			t.Errorf("infra's members %v", m)
		}
		if r := teams[0].Repos; len(r) != 1 || r[0] != "bwsalmon/silo" {
			t.Errorf("infra's repos %v", r)
		}
		if teams[1].Members["user:bob@example.com"] != Admin {
			t.Errorf("bob's role was not changed: %v", teams[1].Members)
		}

		check(t, s.ReleaseRepo(ctx, "web", "bwsalmon/web"))
		if owner, _ := s.RepoOwner(ctx, "bwsalmon/web"); owner != "" {
			t.Errorf("released repo still owned by %q", owner)
		}
		check(t, s.AssignRepo(ctx, "web", "bwsalmon/web"))
		check(t, s.RemoveMember(ctx, "infra", "group:eng@example.com"))
		if err := s.RemoveMember(ctx, "infra", "group:eng@example.com"); !errors.Is(err, ErrNotFound) {
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

// TestAdmission checks that one read for many owners and repositories
// answers each check as AdmitContext does, refusals and their messages
// included, and that it does not answer for what it was not read for.
func TestAdmission(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		seed(t, s)
		node := func(repos ...string) registry.Node {
			n, err := registry.Validate(registry.Node{ID: "ws-1", Repos: repos, Slots: 1})
			check(t, err)
			return n
		}
		a, err := s.Admission(ctx,
			[]string{"Alice@example.com", "bob@example.com", "carol@example.com", "alice@example.com"},
			[]string{"bwsalmon/silo", "BWSALMON/WEB", "bwsalmon/orphan", "not a repo"})
		check(t, err)
		for _, c := range []struct {
			owner string
			n     registry.Node
			want  string // "" for admitted
		}{
			{"alice@example.com", node("bwsalmon/silo"), ""},
			{"ALICE@example.com", node("BWSALMON/SILO"), ""},
			{"bob@example.com", node("bwsalmon/web"), ""},
			{"carol@example.com", node(), ""},
			{"alice@example.com", node("bwsalmon/silo", "bwsalmon/web", "bwsalmon/orphan"),
				"directory: node not admitted: alice@example.com may not serve bwsalmon/orphan (no team owns it), bwsalmon/web (owned by team web, which alice@example.com is not in)"},
			{"carol@example.com", node("bwsalmon/silo"),
				"directory: node not admitted: carol@example.com may not serve bwsalmon/silo (owned by team infra, which carol@example.com is not in)"},
		} {
			err := a.Admit(c.owner, c.n)
			if want := s.AdmitContext(ctx, c.owner, c.n); fmt.Sprint(err) != fmt.Sprint(want) {
				t.Errorf("%s %v: Admission says %v, AdmitContext %v", c.owner, c.n.Repos, err, want)
			}
			switch {
			case c.want == "" && err != nil:
				t.Errorf("%s %v: %v, want admitted", c.owner, c.n.Repos, err)
			case c.want != "" && (!errors.Is(err, ErrNotAdmitted) || err.Error() != c.want):
				t.Errorf("%s %v: %v, want %q", c.owner, c.n.Repos, err, c.want)
			}
		}
		// An invalid repository is the error AdmitContext gives, not a
		// refusal.
		bad := registry.Node{ID: "ws-1", Repos: []string{"not a repo"}}
		if err, want := a.Admit("alice@example.com", bad), s.AdmitContext(ctx, "alice@example.com", bad); !errors.Is(err, ErrInvalid) || err.Error() != want.Error() {
			t.Errorf("invalid repository: %v, want %v", err, want)
		}
		// An owner or a repository it was not read for has no answer.
		if err := a.Admit("dave@example.com", node("bwsalmon/silo")); !errors.Is(err, ErrNotRead) || errors.Is(err, ErrNotAdmitted) {
			t.Errorf("owner not read: %v, want ErrNotRead", err)
		}
		if err := a.Admit("alice@example.com", node("bwsalmon/grain")); !errors.Is(err, ErrNotRead) || errors.Is(err, ErrNotAdmitted) {
			t.Errorf("repository not read: %v, want ErrNotRead", err)
		}
		// It is what the store said when it was read: a later change is
		// seen by the next read, not by this one.
		check(t, s.RemoveMember(ctx, "infra", "user:alice@example.com"))
		if err := a.Admit("alice@example.com", node("bwsalmon/silo")); err != nil {
			t.Errorf("the read made before alice left: %v", err)
		}
		b, err := s.Admission(ctx, []string{"alice@example.com"}, []string{"bwsalmon/silo"})
		check(t, err)
		if err := b.Admit("alice@example.com", node("bwsalmon/silo")); !errors.Is(err, ErrNotAdmitted) {
			t.Errorf("a read after alice left: %v, want ErrNotAdmitted", err)
		}
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
		// A bearer that is not shaped like a token is refused without
		// a query: on a canceled context, a query would fail otherwise.
		dead, cancel := context.WithCancel(ctx)
		cancel()
		for _, bad := range []string{tokenPrefix + "x", tok[:len(tok)-1], tok + "A", strings.ToUpper(tok[:26]) + tok[26:], tok[:len(tok)-1] + "="} {
			if _, _, err := s.VerifyNodeToken(dead, bad); err == nil || !strings.Contains(err.Error(), "not a node token") {
				t.Errorf("VerifyNodeToken(%q) = %v, want refused by shape", bad, err)
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
		// With every token for it revoked, the node ID is still alice's
		// (SA1-H1): a task lost on it must not be resumed by bob's node.
		if _, _, err := s.MintNodeToken(ctx, "bob@example.com", "ws-1"); !errors.Is(err, ErrNodeOwner) {
			t.Errorf("bob minting alice's revoked node ID: %v, want ErrNodeOwner", err)
		}
		// Bob cannot release it; alice can, and then bob may have it.
		if err := s.ReleaseNodeID(ctx, "bob@example.com", "ws-1"); !errors.Is(err, ErrNotFound) {
			t.Errorf("bob releasing alice's node ID: %v, want ErrNotFound", err)
		}
		check(t, s.ReleaseNodeID(ctx, "alice@example.com", "ws-1"))
		if got, err := s.NodeToken(ctx, rec2.ID); err != nil || got.Released == nil {
			t.Errorf("released record %+v %v", got, err)
		}
		if _, _, err := s.MintNodeToken(ctx, "bob@example.com", "ws-1"); err != nil {
			t.Errorf("bob minting a released node ID: %v", err)
		}
		if _, _, err := s.MintNodeToken(ctx, "alice@example.com", "ws-1"); !errors.Is(err, ErrNodeOwner) {
			t.Errorf("alice minting the node ID she released to bob: %v, want ErrNodeOwner", err)
		}
		if all, _ := s.NodeTokens(ctx, ""); len(all) != 3 {
			t.Errorf("all tokens %+v", all)
		}
		// A hub admin releases it from every owner, live tokens included.
		check(t, s.ReleaseNodeID(ctx, "", "ws-1"))
		if _, _, err := s.MintNodeToken(ctx, "alice@example.com", "ws-1"); err != nil {
			t.Errorf("alice minting a node ID a hub admin released: %v", err)
		}
	})
}

// TestNodeIDReusableAfterRevoke was audit SA1-H1's exploit: once its
// owner's tokens were revoked, anyone could mint a token for a node ID
// and be handed the lost tasks on it. The ID stays its owner's.
func TestNodeIDReusableAfterRevoke(t *testing.T) {
	ctx := context.Background()
	dir, err := Open(ctx, "sqlite::memory:")
	check(t, err)
	defer dir.Close()
	_, rec, err := dir.MintNodeToken(ctx, "alice@example.com", "ws-1")
	check(t, err)
	check(t, dir.RevokeNodeToken(ctx, rec.ID))
	if _, _, err := dir.MintNodeToken(ctx, "mallory@example.com", "ws-1"); !errors.Is(err, ErrNodeOwner) {
		t.Fatalf("mallory minting alice's revoked node ID: %v, want ErrNodeOwner", err)
	}
}

// TestReleasedColumn: node_tokens from before released_at gain it, and
// their node IDs stay their owners'; OpenExisting refuses a table
// without it.
func TestReleasedColumn(t *testing.T) {
	run := func(t *testing.T, url string) {
		ctx := context.Background()
		s, err := Open(ctx, url)
		check(t, err)
		_, err = s.db.ExecContext(ctx, `DROP TABLE node_tokens`)
		check(t, err)
		_, err = s.db.ExecContext(ctx, `CREATE TABLE node_tokens (id TEXT PRIMARY KEY, hash TEXT NOT NULL UNIQUE, owner TEXT NOT NULL,
			node TEXT NOT NULL, created_at BIGINT NOT NULL, revoked_at BIGINT)`)
		check(t, err)
		_, err = s.db.ExecContext(ctx, `INSERT INTO node_tokens (id, hash, owner, node, created_at, revoked_at) VALUES ('t1', 'h1', 'alice@example.com', 'ws-1', 1, 2)`)
		check(t, err)
		s.Close()
		if _, err := OpenExisting(ctx, url); err == nil || !strings.Contains(err.Error(), "released_at") {
			t.Errorf("OpenExisting on the old table: %v", err)
		}
		for range 2 {
			s, err = Open(ctx, url)
			check(t, err)
			if _, _, err := s.MintNodeToken(ctx, "bob@example.com", "ws-1"); !errors.Is(err, ErrNodeOwner) {
				t.Errorf("bob minting alice's node ID: %v, want ErrNodeOwner", err)
			}
			s.Close()
		}
		s, err = OpenExisting(ctx, url)
		check(t, err)
		s.Close()
	}
	t.Run("sqlite", func(t *testing.T) { run(t, "sqlite:"+t.TempDir()+"/hub.db") })
	t.Run("postgres", func(t *testing.T) { run(t, pgtest.Start(t).URL) })
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
		// A group named like a member's email is not that member, and
		// a user whose email is a group's name is not in the group
		// (SA1-L6).
		mallory, err := s.Access(ctx, identity.Principal{Email: "mallory@example.com", Groups: []string{"alice@example.com"}}, false)
		check(t, err)
		eng, err := s.Access(ctx, identity.Principal{Email: "eng@example.com"}, false)
		check(t, err)
		if len(mallory.Teams) != 0 || len(eng.Teams) != 0 {
			t.Errorf("a group as a user, or a user as a group: mallory %v, eng %v", mallory.Teams, eng.Teams)
		}

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

// TestLegacyPrincipals: rows from before principals had prefixes
// become users when the tables are migrated, and stay put after.
func TestLegacyPrincipals(t *testing.T) {
	run := func(t *testing.T, url string) {
		ctx := context.Background()
		s, err := Open(ctx, url)
		check(t, err)
		check(t, s.CreateTeam(ctx, "infra", ""))
		for _, p := range []string{"alice@example.com", "user:bob@example.com", "group:eng@example.com"} {
			_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO team_members (team, principal, role) VALUES ('infra', ?, 'member')`), p)
			check(t, err)
		}
		s.Close()
		for range 2 {
			s, err = Open(ctx, url)
			check(t, err)
			teams, err := s.Teams(ctx)
			check(t, err)
			s.Close()
			if m := teams[0].Members; len(m) != 3 || m["user:alice@example.com"] != Member || m["user:bob@example.com"] != Member || m["group:eng@example.com"] != Member {
				t.Fatalf("migrated members %v", m)
			}
		}
	}
	t.Run("sqlite", func(t *testing.T) { run(t, "sqlite:"+t.TempDir()+"/hub.db") })
	t.Run("postgres", func(t *testing.T) { run(t, pgtest.Start(t).URL) })
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
