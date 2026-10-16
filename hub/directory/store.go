// Package directory is the Silo hub's team directory (docs/hub.md §5):
// which teams exist, who is in each, which team owns each repository,
// and the node tokens the hub has minted. It lives in a store, SQLite
// for one replica and PostgreSQL for several, and it decides:
//
//   - whom a node's token proves it to be (Authenticate, for
//     rendezvous.Server.Authenticate);
//   - whether a node's owner may serve the repositories it lists
//     (Admit, for rendezvous.Server.Admit);
//   - what a human may see and change (Access, and the HTTP API).
//
// A node token is shown once, when it is minted; the store keeps only
// its SHA-256, so a leaked database proves nobody.
package directory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/hub/dbaccess"
	_ "github.com/jackc/pgx/v5/stdlib" // "pgx"
	_ "modernc.org/sqlite"             // "sqlite"
)

// Role is a member's role in a team.
type Role string

const (
	// Member may serve the team's repositories from their nodes and see
	// its tasks and nodes.
	Member Role = "member"
	// Admin is a member who also assigns repositories to the team and
	// manages its members.
	Admin Role = "admin"
)

func (r Role) valid() bool { return r == Member || r == Admin }

var (
	// ErrNotFound is a team, member, repository or token that does not
	// exist.
	ErrNotFound = errors.New("directory: not found")
	// ErrExists is a team that already exists.
	ErrExists = errors.New("directory: already exists")
	// ErrOwned is a repository another team already owns: every
	// repository has exactly one owning team.
	ErrOwned = errors.New("directory: repository is owned by another team")
	// ErrNodeOwner is a node token asked for a node ID that another
	// owner has had a token for, and has not released (SA1-H1).
	ErrNodeOwner = errors.New("directory: node belongs to another owner")
	// ErrInvalid is a malformed team ID, repository or role.
	ErrInvalid = errors.New("directory: invalid")
)

// Team is one team, with its members and repositories.
type Team struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Members map[string]Role `json:"members"`
	Repos   []string        `json:"repos"`
}

// maxConns caps the directory's PostgreSQL connections, all of which it
// keeps open once made, as the backlog's and the event store's are
// (docs/test-runs/2026-10-16-hub-load.md).
const maxConns = 8

// Store is the team directory.
type Store struct {
	db       *sql.DB
	postgres bool
	// Now, when set, is the clock tokens are stamped with.
	Now func() time.Time
}

// Open opens the directory at url: "postgres://..." (or
// "postgresql://...") for PostgreSQL, "sqlite:PATH" or a bare path for
// SQLite (":memory:" for a throwaway one). It creates the tables if
// they are missing.
func Open(ctx context.Context, url string) (*Store, error) { return open(ctx, url, true) }

// OpenExisting opens the directory at url as Open does, but creates
// nothing: its tables were made by Open or `silo hub migrate` as a role
// that may create them, and url's role need only read and write their
// rows (docs/deploy.md). It fails if a table is missing.
func OpenExisting(ctx context.Context, url string) (*Store, error) { return open(ctx, url, false) }

// Tables are the directory's tables.
var Tables = []string{"teams", "team_members", "team_repos", "node_tokens", "team_settings"}

func open(ctx context.Context, url string, create bool) (*Store, error) {
	var (
		db       *sql.DB
		err      error
		postgres = strings.HasPrefix(url, "postgres://") || strings.HasPrefix(url, "postgresql://")
	)
	if postgres {
		db, err = sql.Open("pgx", url)
		if err == nil {
			db.SetMaxOpenConns(maxConns)
			db.SetMaxIdleConns(maxConns)
		}
	} else {
		path := strings.TrimPrefix(url, "sqlite:")
		db, err = sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
		// One connection: an in-memory database is per connection, and
		// SQLite serialises writers anyway.
		db.SetMaxOpenConns(1)
	}
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, postgres: postgres}
	if create {
		err = s.migrate(ctx)
	} else {
		err = dbaccess.Check(ctx, db, "directory", Tables...)
		if err == nil {
			err = dbaccess.CheckColumns(ctx, db, "directory", added...)
		}
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the store.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks that the database answers: the hub's readiness.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Stats is the store's connection pool's statistics.
func (s *Store) Stats() sql.DBStats { return s.db.Stats() }

const schema = `
CREATE TABLE IF NOT EXISTS teams (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	created_at BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS team_members (
	team TEXT NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
	principal TEXT NOT NULL,
	role TEXT NOT NULL CHECK (role IN ('member', 'admin')),
	PRIMARY KEY (team, principal)
);
CREATE INDEX IF NOT EXISTS team_members_principal ON team_members (principal);
CREATE TABLE IF NOT EXISTS team_repos (
	repo TEXT PRIMARY KEY,
	team TEXT NOT NULL REFERENCES teams (id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS team_repos_team ON team_repos (team);
CREATE TABLE IF NOT EXISTS node_tokens (
	id TEXT PRIMARY KEY,
	hash TEXT NOT NULL UNIQUE,
	owner TEXT NOT NULL,
	node TEXT NOT NULL,
	created_at BIGINT NOT NULL,
	revoked_at BIGINT,
	released_at BIGINT
);
CREATE INDEX IF NOT EXISTS node_tokens_node ON node_tokens (node);
CREATE UNIQUE INDEX IF NOT EXISTS node_tokens_live ON node_tokens (node) WHERE revoked_at IS NULL;
CREATE TABLE IF NOT EXISTS team_settings (
	team TEXT PRIMARY KEY REFERENCES teams (id) ON DELETE CASCADE,
	lost_requeue_seconds BIGINT NOT NULL
);
`

// legacyPrincipals makes every team_members row from before principals
// had prefixes a user: the only deployment then was behind IAP, whose
// tokens carry no groups, so a bare row could only have matched an
// email. It changes nothing once every row has a prefix.
const legacyPrincipals = `UPDATE team_members SET principal = 'user:' || principal
	WHERE principal NOT LIKE 'user:%' AND principal NOT LIKE 'group:%'`

// added are the columns added to the directory's tables since they
// were first made. node_tokens.released_at (SA1-H1) is null on every
// token minted before it, so each node ID stays bound to every owner
// that ever had a token for it until it is released.
var added = []dbaccess.Column{
	{Table: "node_tokens", Name: "released_at", Add: `ALTER TABLE node_tokens ADD COLUMN released_at BIGINT`},
}

func (s *Store) migrate(ctx context.Context) error {
	for _, stmt := range append(strings.Split(schema, ";"), legacyPrincipals) {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("directory: migrating tables: %w", err)
		}
	}
	return dbaccess.AddColumns(ctx, s.db, "directory", added...)
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// q rewrites the ? placeholders of a query for PostgreSQL. No query
// here has a ? anywhere else.
func (s *Store) q(query string) string {
	if !s.postgres {
		return query
	}
	var b strings.Builder
	n := 0
	for _, c := range query {
		if c == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

// tx runs f in a transaction, committing if it returns nil.
func (s *Store) tx(ctx context.Context, f func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := f(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

var teamID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// repoName is "owner/name", as package registry normalises it.
var repoName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*/[a-z0-9._-]+$`)

func normRepo(repo string) (string, error) {
	r := strings.ToLower(strings.TrimSpace(repo))
	if !repoName.MatchString(r) {
		return "", fmt.Errorf("%w repository %q: want owner/name", ErrInvalid, repo)
	}
	return r, nil
}

// Principals are IAM-style, so a group can never be mistaken for a
// user or the other way round (docs/hub.md §5.3): "user:<email>"
// matches only a signed-in human's email and "group:<name>" only one
// of the groups their identity provider put them in. A group named
// like an email is still a group.
const (
	UserPrefix  = "user:"
	GroupPrefix = "group:"
)

// normPrincipal is how principals are stored and compared:
// "user:<email>" or "group:<name>", lower-cased. A principal with
// neither prefix is refused rather than guessed at: Google groups look
// like emails.
func normPrincipal(p string) (string, error) {
	p = strings.ToLower(strings.TrimSpace(p))
	kind, name, _ := strings.Cut(p, ":")
	switch {
	case kind+":" != UserPrefix && kind+":" != GroupPrefix:
		return "", fmt.Errorf("%w principal %q: want user:EMAIL or group:NAME", ErrInvalid, p)
	case name == "" || strings.ContainsAny(name, " \t\n/"),
		kind+":" == UserPrefix && !strings.Contains(name, "@"):
		return "", fmt.Errorf("%w principal %q", ErrInvalid, p)
	}
	return p, nil
}

// normEmail is how a node token's owner, a signed-in human's email, is
// stored and compared: lower-cased, with no prefix.
func normEmail(e string) (string, error) {
	e = strings.ToLower(strings.TrimSpace(e))
	if e == "" || strings.ContainsAny(e, " \t\n/") {
		return "", fmt.Errorf("%w email %q", ErrInvalid, e)
	}
	return e, nil
}

// userPrincipal is the principal an email is a member as.
func userPrincipal(email string) (string, error) {
	e, err := normEmail(email)
	if err != nil {
		return "", err
	}
	return UserPrefix + e, nil
}

// HubAdminPrincipal is how one entry of API.HubAdmins is read: a
// principal as normPrincipal has it. An entry with no prefix, as
// SILO_HUB_ADMINS took before principals had them, is a user, and
// legacy says so, so the hub can warn that it should be written
// user:EMAIL (or group:NAME, if a group was meant).
func HubAdminPrincipal(entry string) (p string, legacy bool) {
	e := strings.ToLower(strings.TrimSpace(entry))
	if strings.HasPrefix(e, UserPrefix) || strings.HasPrefix(e, GroupPrefix) {
		return e, false
	}
	return UserPrefix + e, true
}

func isUnique(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "unique") || strings.Contains(m, "duplicate key") || strings.Contains(m, "primary key")
}

// CreateTeam creates a team. Its ID is 1-63 characters of [a-z0-9-].
func (s *Store) CreateTeam(ctx context.Context, id, name string) error {
	if !teamID.MatchString(id) {
		return fmt.Errorf("%w team ID %q: want 1-63 of [a-z0-9-]", ErrInvalid, id)
	}
	if name == "" {
		name = id
	}
	_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO teams (id, name, created_at) VALUES (?, ?, ?)`), id, name, s.now().UnixMilli())
	if isUnique(err) {
		return fmt.Errorf("%w: team %s", ErrExists, id)
	}
	return err
}

// DeleteTeam deletes a team, its memberships and its repositories,
// which are left with no owner, so no node serves them until another
// team takes them.
func (s *Store) DeleteTeam(ctx context.Context, id string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{`DELETE FROM team_members WHERE team = ?`, `DELETE FROM team_repos WHERE team = ?`, `DELETE FROM team_settings WHERE team = ?`} {
			if _, err := tx.ExecContext(ctx, s.q(q), id); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, s.q(`DELETE FROM teams WHERE id = ?`), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: team %s", ErrNotFound, id)
		}
		return nil
	})
}

// Teams lists every team, with its members and repositories, by ID.
func (s *Store) Teams(ctx context.Context) ([]Team, error) {
	byID := map[string]*Team{}
	var ids []string
	rows, err := s.db.QueryContext(ctx, `SELECT id, name FROM teams`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		t := &Team{Members: map[string]Role{}, Repos: []string{}}
		if err := rows.Scan(&t.ID, &t.Name); err != nil {
			rows.Close()
			return nil, err
		}
		byID[t.ID] = t
		ids = append(ids, t.ID)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT team, principal, role FROM team_members`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var team, p, role string
		if err := rows.Scan(&team, &p, &role); err != nil {
			rows.Close()
			return nil, err
		}
		if t := byID[team]; t != nil {
			t.Members[p] = Role(role)
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT team, repo FROM team_repos ORDER BY repo`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var team, repo string
		if err := rows.Scan(&team, &repo); err != nil {
			rows.Close()
			return nil, err
		}
		if t := byID[team]; t != nil {
			t.Repos = append(t.Repos, repo)
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	sort.Strings(ids)
	out := make([]Team, len(ids))
	for i, id := range ids {
		out[i] = *byID[id]
	}
	return out, nil
}

func (s *Store) teamExists(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) error {
	var one int
	err := q.QueryRowContext(ctx, s.q(`SELECT 1 FROM teams WHERE id = ?`), id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: team %s", ErrNotFound, id)
	}
	return err
}

// SetMember adds principal, "user:<email>" or "group:<name>" for a
// group the identity provider reports, to team with role, or changes
// their role.
func (s *Store) SetMember(ctx context.Context, team, principal string, role Role) error {
	p, err := normPrincipal(principal)
	if err != nil {
		return err
	}
	if !role.valid() {
		return fmt.Errorf("%w role %q: want member or admin", ErrInvalid, role)
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := s.teamExists(ctx, tx, team); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, s.q(`INSERT INTO team_members (team, principal, role) VALUES (?, ?, ?)
			ON CONFLICT (team, principal) DO UPDATE SET role = excluded.role`), team, p, string(role))
		return err
	})
}

// RemoveMember removes principal from team.
func (s *Store) RemoveMember(ctx context.Context, team, principal string) error {
	p, err := normPrincipal(principal)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM team_members WHERE team = ? AND principal = ?`), team, p)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s in team %s", ErrNotFound, p, team)
	}
	return nil
}

// AssignRepo makes team the owner of repo. A repository another team
// owns is refused (ErrOwned): it has to be released first, so one
// team's admin cannot take another team's repository.
func (s *Store) AssignRepo(ctx context.Context, team, repo string) error {
	r, err := normRepo(repo)
	if err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := s.teamExists(ctx, tx, team); err != nil {
			return err
		}
		var owner string
		err := tx.QueryRowContext(ctx, s.q(`SELECT team FROM team_repos WHERE repo = ?`), r).Scan(&owner)
		switch {
		case err == nil && owner == team:
			return nil
		case err == nil:
			return fmt.Errorf("%w: %s belongs to team %s", ErrOwned, r, owner)
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		_, err = tx.ExecContext(ctx, s.q(`INSERT INTO team_repos (repo, team) VALUES (?, ?)`), r, team)
		if isUnique(err) {
			return fmt.Errorf("%w: %s", ErrOwned, r)
		}
		return err
	})
}

// ReleaseRepo removes team's ownership of repo, leaving it with no
// owner.
func (s *Store) ReleaseRepo(ctx context.Context, team, repo string) error {
	r, err := normRepo(repo)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM team_repos WHERE repo = ? AND team = ?`), r, team)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s in team %s", ErrNotFound, r, team)
	}
	return nil
}

// RepoOwner is the team that owns repo, or "" if none does.
func (s *Store) RepoOwner(ctx context.Context, repo string) (string, error) {
	r, err := normRepo(repo)
	if err != nil {
		return "", err
	}
	var team string
	err = s.db.QueryRowContext(ctx, s.q(`SELECT team FROM team_repos WHERE repo = ?`), r).Scan(&team)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return team, err
}

// TeamRepos are the repositories team owns, sorted: the ones a
// team-targeted task may run in (docs/hub.md §6.2).
func (s *Store) TeamRepos(ctx context.Context, team string) ([]string, error) {
	if err := s.teamExists(ctx, s.db, team); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT repo FROM team_repos WHERE team = ? ORDER BY repo`), team)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	repos := []string{}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		repos = append(repos, r)
	}
	return repos, rows.Err()
}

// memberships are the teams any of principals is in, with the highest
// role any of them has there.
func (s *Store) memberships(ctx context.Context, principals []string) (map[string]Role, error) {
	out := map[string]Role{}
	if len(principals) == 0 {
		return out, nil
	}
	args := make([]any, len(principals))
	for i, p := range principals {
		args[i] = p
	}
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT team, role FROM team_members WHERE principal IN (?`+strings.Repeat(", ?", len(principals)-1)+`)`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var team, role string
		if err := rows.Scan(&team, &role); err != nil {
			return nil, err
		}
		if out[team] != Admin {
			out[team] = Role(role)
		}
	}
	return out, rows.Err()
}

// reposOf maps each repository the teams own to its team.
func (s *Store) reposOf(ctx context.Context, teams map[string]Role) (map[string]string, error) {
	out := map[string]string{}
	if len(teams) == 0 {
		return out, nil
	}
	var args []any
	for t := range teams {
		args = append(args, t)
	}
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT repo, team FROM team_repos WHERE team IN (?`+strings.Repeat(", ?", len(args)-1)+`)`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var repo, team string
		if err := rows.Scan(&repo, &team); err != nil {
			return nil, err
		}
		out[repo] = team
	}
	return out, rows.Err()
}
