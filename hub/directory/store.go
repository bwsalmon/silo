// Package directory is the Silo hub's team directory (docs/hub.md §5):
// which teams exist, who is in each, which team owns each repository,
// and the node tokens the hub has minted. It lives in a store,
// PostgreSQL (on Cloud SQL, in every deployment; SQLite only in local
// development and tests), and it decides:
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
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwsalmon/silo/hub/dbaccess"
	_ "github.com/jackc/pgx/v5/stdlib" // "pgx"
	_ "modernc.org/sqlite"             // "sqlite"
)

// Role is a member's role in a team.
type Role string

const (
	// Viewer has read-only visibility into the team's tasks, events,
	// artifacts, nodes, and Grainfiles.
	Viewer Role = "viewer"
	// Proposer has read visibility into the team and may submit tasks to
	// the team or its repositories as proposals even when
	// anyone_may_propose is false.
	Proposer Role = "proposer"
	// Member may serve the team's repositories from their nodes, approve
	// or reject proposals, answer in-run approval requests, post to team
	// interactive sessions, and submit tasks.
	Member Role = "member"
	// Admin is a member who also assigns repositories to the team and
	// manages its members and settings.
	Admin Role = "admin"
)

func (r Role) valid() bool {
	return r == Viewer || r == Proposer || r == Member || r == Admin
}

func (r Role) rank() int {
	switch r {
	case Admin:
		return 4
	case Member:
		return 3
	case Proposer:
		return 2
	case Viewer:
		return 1
	default:
		return 0
	}
}

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
	// ErrLastSysAdmin is an attempt to remove or demote the last Hub sys admin.
	ErrLastSysAdmin = errors.New("cannot remove or demote the last hub sys admin")
	// ErrNotAcceptedUser is a sign-in from a user who is not on the Hub's accepted users list.
	ErrNotAcceptedUser = errors.New("user is not on the hub accepted users list")
)

// AcceptedUser is a user on the Hub's accepted users list.
type AcceptedUser struct {
	Email     string    `json:"email"`
	SysAdmin  bool      `json:"sys_admin"`
	AddedBy   string    `json:"added_by"`
	CreatedAt time.Time `json:"created_at"`
}

// Team is one team / deployment, with its members and repositories.
type Team struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	URL          string          `json:"url,omitempty"`
	RecipientURL string          `json:"recipient_url,omitempty"`
	Members      map[string]Role `json:"members"`
	Repos        []string        `json:"repos"`
}

// CanonicalDeploymentURL returns the canonical Hub URL for deployment id
// ("<normalized-hub-url>/deployments/<id>").
func CanonicalDeploymentURL(hubURL, id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	base := strings.TrimRight(strings.TrimSpace(hubURL), "/")
	if base == "" {
		return "/deployments/" + id
	}
	return base + "/deployments/" + id
}

// maxConns caps the directory's PostgreSQL connections, all of which it
// keeps open once made, as the backlog's and the event store's are
// (docs/test-runs/2026-10-16-hub-load.md).
const maxConns = 8

// Store is the team directory.
type Store struct {
	db                   *sql.DB
	postgres             bool
	userMu               sync.Mutex
	enforceAcceptedUsers atomic.Bool
	// Now, when set, is the clock tokens are stamped with.
	Now func() time.Time
}

// SetEnforceAcceptedUsers enables or disables strict accepted_users enforcement
// even when the table is currently empty.
func (s *Store) SetEnforceAcceptedUsers(v bool) {
	s.enforceAcceptedUsers.Store(v)
}

// EnforceAcceptedUsers reports whether strict accepted_users enforcement is enabled.
func (s *Store) EnforceAcceptedUsers() bool {
	return s.enforceAcceptedUsers.Load()
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
var Tables = []string{"teams", "team_members", "team_repos", "node_tokens", "team_settings", "user_preferences", "accepted_users"}

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
	role TEXT NOT NULL CHECK (role IN ('viewer', 'proposer', 'member', 'admin')),
	PRIMARY KEY (team, principal)
);
CREATE INDEX IF NOT EXISTS team_members_principal ON team_members (principal);
CREATE TABLE IF NOT EXISTS team_repos (
	repo TEXT PRIMARY KEY,
	team TEXT NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
	history INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS team_repos_team ON team_repos (team);
CREATE TABLE IF NOT EXISTS node_tokens (
	id TEXT PRIMARY KEY,
	hash TEXT NOT NULL,
	owner TEXT NOT NULL,
	node TEXT NOT NULL,
	created_at BIGINT NOT NULL,
	revoked_at BIGINT,
	released_at BIGINT
);
CREATE UNIQUE INDEX IF NOT EXISTS node_tokens_hash ON node_tokens (hash);
CREATE INDEX IF NOT EXISTS node_tokens_node ON node_tokens (node);
CREATE TABLE IF NOT EXISTS team_settings (
	team TEXT PRIMARY KEY REFERENCES teams (id) ON DELETE CASCADE,
	lost_requeue_seconds BIGINT NOT NULL DEFAULT 0,
	anyone_may_propose INTEGER NOT NULL DEFAULT 1,
	require_approval INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS user_preferences (
	email TEXT PRIMARY KEY,
	theme TEXT NOT NULL DEFAULT 'light',
	inbox_nodes TEXT NOT NULL DEFAULT '[]',
	inbox_configured INTEGER NOT NULL DEFAULT 0,
	updated_at BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS accepted_users (
	email TEXT PRIMARY KEY,
	sys_admin INTEGER NOT NULL DEFAULT 0,
	added_by TEXT NOT NULL DEFAULT '',
	created_at BIGINT NOT NULL
);
`

// liveTokens lets a node have one live token at a time, however two
// mints for it race (MintNodeToken). Spanner has no partial index but
// a null-filtered one, and needs none: its transactions are
// serializable, so of two racing mints, which both read the node's
// tokens before inserting, one is aborted and, run again, sees the
// other's token.
const liveTokens = `CREATE UNIQUE INDEX IF NOT EXISTS node_tokens_live ON node_tokens (node) WHERE revoked_at IS NULL`

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
// team_repos.history (SA1-M2) is 0 on every assignment made before it,
// so a repository's tasks filed under an earlier team stop showing to
// the team that owns it now until a hub admin transfers it again with
// its history.
var added = []dbaccess.Column{
	{Table: "node_tokens", Name: "released_at", Add: `ALTER TABLE node_tokens ADD COLUMN released_at BIGINT`},
	{Table: "team_repos", Name: "history", Add: `ALTER TABLE team_repos ADD COLUMN history INTEGER NOT NULL DEFAULT 0`},
	{Table: "team_settings", Name: "anyone_may_propose", Add: `ALTER TABLE team_settings ADD COLUMN anyone_may_propose INTEGER NOT NULL DEFAULT 1`},
	{Table: "team_settings", Name: "require_approval", Add: `ALTER TABLE team_settings ADD COLUMN require_approval INTEGER NOT NULL DEFAULT 0`},
}

func (s *Store) migrate(ctx context.Context) error {
	stmts := strings.Split(schema, ";")
	if !dbaccess.Spanner(ctx, s.db) {
		stmts = append(stmts, liveTokens)
	}
	for _, stmt := range append(stmts, legacyPrincipals) {
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

// tx runs f in a transaction, committing if it returns nil, and runs
// it again if the database aborts it (dbaccess.InTx).
func (s *Store) tx(ctx context.Context, f func(*sql.Tx) error) error {
	return dbaccess.InTx(ctx, s.db, f)
}

var teamID = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

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

// SeedUsers populates accepted_users from configured hubAdmins (with sys_admin = 1)
// and acceptedUsers (with sys_admin = 0). Existing sys_admin privileges are never
// downgraded by seeding.
func (s *Store) SeedUsers(ctx context.Context, hubAdmins, acceptedUsers []string) error {
	if len(hubAdmins) > 0 || len(acceptedUsers) > 0 {
		s.SetEnforceAcceptedUsers(true)
	}
	type seedEntry struct {
		email    string
		sysAdmin int
	}
	var entries []seedEntry
	seen := map[string]int{}
	add := func(raw string, sysAdmin int) {
		e := strings.ToLower(strings.TrimSpace(raw))
		if strings.HasPrefix(e, GroupPrefix) {
			return
		}
		e = strings.TrimPrefix(e, UserPrefix)
		norm, err := normEmail(e)
		if err != nil || norm == "" {
			return
		}
		if idx, ok := seen[norm]; ok {
			if sysAdmin > entries[idx].sysAdmin {
				entries[idx].sysAdmin = sysAdmin
			}
			return
		}
		seen[norm] = len(entries)
		entries = append(entries, seedEntry{email: norm, sysAdmin: sysAdmin})
	}
	for _, adm := range hubAdmins {
		p, _ := HubAdminPrincipal(adm)
		if strings.HasPrefix(p, UserPrefix) {
			add(strings.TrimPrefix(p, UserPrefix), 1)
		}
	}
	for _, u := range acceptedUsers {
		add(u, 0)
	}
	if len(hubAdmins) > 0 || len(acceptedUsers) > 0 {
		rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT principal FROM team_members`)
		if err == nil {
			for rows.Next() {
				var p string
				if rows.Scan(&p) == nil && strings.HasPrefix(p, UserPrefix) {
					add(strings.TrimPrefix(p, UserPrefix), 0)
				}
			}
			rows.Close()
		}
	}
	if len(entries) == 0 {
		return nil
	}
	s.userMu.Lock()
	defer s.userMu.Unlock()
	now := s.now().UnixMilli()
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, ent := range entries {
			if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO accepted_users (email, sys_admin, added_by, created_at)
				VALUES (?, ?, 'bootstrap', ?)
				ON CONFLICT (email) DO UPDATE SET sys_admin = CASE WHEN excluded.sys_admin > accepted_users.sys_admin THEN excluded.sys_admin ELSE accepted_users.sys_admin END`),
				ent.email, ent.sysAdmin, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// Users returns all accepted users ordered by email.
func (s *Store) Users(ctx context.Context) ([]AcceptedUser, error) {
	var out []AcceptedUser
	err := dbaccess.Retry(ctx, func() error {
		rows, err := s.db.QueryContext(ctx, `SELECT email, sys_admin, added_by, created_at FROM accepted_users ORDER BY email ASC`)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = []AcceptedUser{}
		for rows.Next() {
			var (
				u        AcceptedUser
				sysAdmin int
				created  int64
			)
			if err := rows.Scan(&u.Email, &sysAdmin, &u.AddedBy, &created); err != nil {
				return err
			}
			u.SysAdmin = sysAdmin != 0
			u.CreatedAt = time.UnixMilli(created).UTC()
			out = append(out, u)
		}
		return rows.Err()
	})
	return out, err
}

// SetUser adds or updates email in accepted_users. It returns ErrLastSysAdmin
// if demoting an existing sys admin would leave zero sys admins in accepted_users.
func (s *Store) SetUser(ctx context.Context, email string, sysAdmin bool, addedBy string) error {
	e, err := normEmail(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(email)), UserPrefix))
	if err != nil {
		return err
	}
	addedBy = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(addedBy)), UserPrefix)
	sysInt := 0
	if sysAdmin {
		sysInt = 1
	}
	s.userMu.Lock()
	defer s.userMu.Unlock()
	now := s.now().UnixMilli()
	return s.tx(ctx, func(tx *sql.Tx) error {
		wasSysAdmin := false
		if !sysAdmin {
			var curSys int
			err := tx.QueryRowContext(ctx, s.q(`SELECT sys_admin FROM accepted_users WHERE email = ?`), e).Scan(&curSys)
			if err == nil && curSys != 0 {
				wasSysAdmin = true
				var count int
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM accepted_users WHERE sys_admin <> 0`).Scan(&count); err != nil {
					return err
				}
				if count <= 1 {
					return ErrLastSysAdmin
				}
			} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO accepted_users (email, sys_admin, added_by, created_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (email) DO UPDATE SET sys_admin = excluded.sys_admin`),
			e, sysInt, addedBy, now); err != nil {
			return err
		}
		if wasSysAdmin {
			var remaining int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM accepted_users WHERE sys_admin <> 0`).Scan(&remaining); err != nil {
				return err
			}
			if remaining < 1 {
				return ErrLastSysAdmin
			}
		}
		return nil
	})
}

// DeleteUser removes email from accepted_users. It returns ErrNotFound if email
// is not in accepted_users, and ErrLastSysAdmin if removing email would leave
// zero sys admins in accepted_users.
func (s *Store) DeleteUser(ctx context.Context, email string) error {
	e, err := normEmail(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(email)), UserPrefix))
	if err != nil {
		return err
	}
	s.userMu.Lock()
	defer s.userMu.Unlock()
	return s.tx(ctx, func(tx *sql.Tx) error {
		var curSys int
		err := tx.QueryRowContext(ctx, s.q(`SELECT sys_admin FROM accepted_users WHERE email = ?`), e).Scan(&curSys)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: user %s", ErrNotFound, e)
		}
		if err != nil {
			return err
		}
		if curSys != 0 {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM accepted_users WHERE sys_admin <> 0`).Scan(&count); err != nil {
				return err
			}
			if count <= 1 {
				return ErrLastSysAdmin
			}
		}
		if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM accepted_users WHERE email = ?`), e); err != nil {
			return err
		}
		if curSys != 0 {
			var remaining int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM accepted_users WHERE sys_admin <> 0`).Scan(&remaining); err != nil {
				return err
			}
			if remaining < 1 {
				return ErrLastSysAdmin
			}
		}
		return nil
	})
}

// CheckAcceptedUser checks whether email is in accepted_users, whether they have
// sys_admin = 1, and whether accepted_users has any rows at all.
func (s *Store) CheckAcceptedUser(ctx context.Context, email string) (accepted bool, sysAdmin bool, hasAnyUsers bool, err error) {
	e := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(email)), UserPrefix)
	err = dbaccess.Retry(ctx, func() error {
		accepted, sysAdmin, hasAnyUsers = false, false, false
		var sysInt int
		qErr := s.db.QueryRowContext(ctx, s.q(`SELECT sys_admin FROM accepted_users WHERE email = ?`), e).Scan(&sysInt)
		if qErr == nil {
			accepted, sysAdmin, hasAnyUsers = true, sysInt != 0, true
			return nil
		}
		if !errors.Is(qErr, sql.ErrNoRows) {
			return qErr
		}
		var one int
		anyErr := s.db.QueryRowContext(ctx, `SELECT 1 FROM accepted_users LIMIT 1`).Scan(&one)
		if anyErr == nil {
			hasAnyUsers = true
			return nil
		}
		if errors.Is(anyErr, sql.ErrNoRows) {
			return nil
		}
		return anyErr
	})
	return accepted, sysAdmin, hasAnyUsers, err
}

// CreateTeam creates a team. Its ID is a valid DNS-safe deployment slug
// (^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$).
func (s *Store) CreateTeam(ctx context.Context, id, name string) error {
	if !teamID.MatchString(id) {
		return fmt.Errorf("%w team ID %q: want DNS-safe slug ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$", ErrInvalid, id)
	}
	if name == "" {
		name = id
	}
	err := dbaccess.Retry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO teams (id, name, created_at) VALUES (?, ?, ?)`), id, name, s.now().UnixMilli())
		return err
	})
	if isUnique(err) {
		return fmt.Errorf("%w: team %s", ErrExists, id)
	}
	return err
}

// CreateDeployment atomically creates a deployment (team) with a DNS-safe slug id
// and inserts creatorEmail ("user:<email>") as Admin of that deployment.
func (s *Store) CreateDeployment(ctx context.Context, id, name, creatorEmail string) error {
	if !teamID.MatchString(id) {
		return fmt.Errorf("%w deployment ID %q: want DNS-safe slug ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$", ErrInvalid, id)
	}
	if name == "" {
		name = id
	}
	var creatorPrincipal string
	if strings.TrimSpace(creatorEmail) != "" {
		e, err := normEmail(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(creatorEmail)), UserPrefix))
		if err != nil {
			return err
		}
		creatorPrincipal = UserPrefix + e
	}
	now := s.now().UnixMilli()
	return s.tx(ctx, func(tx *sql.Tx) error {
		if creatorPrincipal != "" {
			// A self-service deployment's ID is also its node ID. Refuse an ID
			// that another owner already holds an unreleased node token for:
			// otherwise whoever minted a token for the name first would be
			// connected as the new deployment's node, and be delivered the
			// tasks and Grainfiles addressed to it, until its creator happened
			// to mint a replacement token.
			var other string
			err := tx.QueryRowContext(ctx, s.q(`SELECT owner FROM node_tokens WHERE node = ? AND owner <> ? AND released_at IS NULL LIMIT 1`),
				id, strings.TrimPrefix(creatorPrincipal, UserPrefix)).Scan(&other)
			if err == nil {
				return fmt.Errorf("%w: deployment %s: node ID is held by another owner's node token", ErrExists, id)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, s.q(`INSERT INTO teams (id, name, created_at) VALUES (?, ?, ?)`), id, name, now)
		if isUnique(err) {
			return fmt.Errorf("%w: deployment %s", ErrExists, id)
		}
		if err != nil {
			return err
		}
		if creatorPrincipal != "" {
			if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO team_members (team, principal, role) VALUES (?, ?, ?)
				ON CONFLICT (team, principal) DO UPDATE SET role = excluded.role`), id, creatorPrincipal, string(Admin)); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteTeam deletes a team, its memberships and its repositories,
// which are left with no owner, so no node serves them until a hub
// admin assigns them to another team. Their tasks stay filed under the
// deleted team: they go to the next owner only if it is given them
// with TransferRepo.
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
	var out []Team
	err := dbaccess.Retry(ctx, func() error {
		byID := map[string]*Team{}
		var ids []string
		rows, err := s.db.QueryContext(ctx, `SELECT id, name FROM teams`)
		if err != nil {
			return err
		}
		for rows.Next() {
			t := &Team{Members: map[string]Role{}, Repos: []string{}}
			if err := rows.Scan(&t.ID, &t.Name); err != nil {
				rows.Close()
				return err
			}
			byID[t.ID] = t
			ids = append(ids, t.ID)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		rows, err = s.db.QueryContext(ctx, `SELECT team, principal, role FROM team_members`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var team, p, role string
			if err := rows.Scan(&team, &p, &role); err != nil {
				rows.Close()
				return err
			}
			if t := byID[team]; t != nil {
				t.Members[p] = Role(role)
			}
		}
		if err := rows.Close(); err != nil {
			return err
		}
		rows, err = s.db.QueryContext(ctx, `SELECT team, repo FROM team_repos ORDER BY repo`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var team, repo string
			if err := rows.Scan(&team, &repo); err != nil {
				rows.Close()
				return err
			}
			if t := byID[team]; t != nil {
				t.Repos = append(t.Repos, repo)
			}
		}
		if err := rows.Close(); err != nil {
			return err
		}
		sort.Strings(ids)
		out = make([]Team, len(ids))
		for i, id := range ids {
			out[i] = *byID[id]
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) teamExists(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) error {
	var one int
	err := dbaccess.Retry(ctx, func() error {
		return q.QueryRowContext(ctx, s.q(`SELECT 1 FROM teams WHERE id = ?`), id).Scan(&one)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: team %s", ErrNotFound, id)
	}
	return err
}

// HasTeam reports whether a team / deployment with id exists.
func (s *Store) HasTeam(ctx context.Context, id string) (bool, error) {
	err := s.teamExists(ctx, s.db, id)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
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
		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO team_members (team, principal, role) VALUES (?, ?, ?)
			ON CONFLICT (team, principal) DO UPDATE SET role = excluded.role`), team, p, string(role)); err != nil {
			return err
		}
		if strings.HasPrefix(p, UserPrefix) {
			enforce := s.enforceAcceptedUsers.Load()
			if !enforce {
				var one int
				enforce = tx.QueryRowContext(ctx, `SELECT 1 FROM accepted_users LIMIT 1`).Scan(&one) == nil
			}
			if enforce {
				email := strings.TrimPrefix(p, UserPrefix)
				if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO accepted_users (email, sys_admin, added_by, created_at)
					VALUES (?, 0, 'team-member', ?)
					ON CONFLICT (email) DO NOTHING`), email, s.now().UnixMilli()); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// RemoveMember removes principal from team.
func (s *Store) RemoveMember(ctx context.Context, team, principal string) error {
	p, err := normPrincipal(principal)
	if err != nil {
		return err
	}
	var res sql.Result
	err = dbaccess.Retry(ctx, func() error {
		var err error
		res, err = s.db.ExecContext(ctx, s.q(`DELETE FROM team_members WHERE team = ? AND principal = ?`), team, p)
		return err
	})
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s in team %s", ErrNotFound, p, team)
	}
	return nil
}

// AssignRepo makes team the owner of repo, from now on: the team's
// members' nodes may serve it and the team files tasks in it, but the
// tasks earlier teams filed in it stay visible only to those teams
// (Access.SeesTask). A repository another team owns is refused
// (ErrOwned): it has to be released first, so no team can take another
// team's repository. Through the API, a non-sys-admin deployment admin
// must prove write access to an unowned repository via .grain/hub-claim
// on its default branch before claiming it (SA1-M2).
func (s *Store) AssignRepo(ctx context.Context, team, repo string) error {
	return s.assignRepo(ctx, team, repo, false)
}

// TransferRepo is AssignRepo carrying the repository's history with
// it: team's members also see the tasks earlier teams filed in repo.
// It is a hub admin's explicit decision (T-TB1-2), never a side effect
// of a release or of deleting a team.
func (s *Store) TransferRepo(ctx context.Context, team, repo string) error {
	return s.assignRepo(ctx, team, repo, true)
}

var errAssignConflict = errors.New("directory: concurrent repo assignment conflict")

func (s *Store) assignRepo(ctx context.Context, team, repo string, history bool) error {
	r, err := normRepo(repo)
	if err != nil {
		return err
	}
	h := 0
	if history {
		h = 1
	}
	for attempt := 0; attempt < 4; attempt++ {
		err = s.tx(ctx, func(tx *sql.Tx) error {
			if err := s.teamExists(ctx, tx, team); err != nil {
				return err
			}
			var owner string
			err := tx.QueryRowContext(ctx, s.q(`SELECT team FROM team_repos WHERE repo = ?`), r).Scan(&owner)
			switch {
			case err == nil && owner == team:
				// Already its own: only whether it carries the history
				// changes.
				_, err = tx.ExecContext(ctx, s.q(`UPDATE team_repos SET history = ? WHERE repo = ?`), h, r)
				return err
			case err == nil:
				return fmt.Errorf("%w: %s belongs to team %s", ErrOwned, r, owner)
			case !errors.Is(err, sql.ErrNoRows):
				return err
			}
			_, err = tx.ExecContext(ctx, s.q(`INSERT INTO team_repos (repo, team, history) VALUES (?, ?, ?)`), r, team, h)
			if isUnique(err) {
				return errAssignConflict
			}
			return err
		})
		if errors.Is(err, errAssignConflict) {
			continue
		}
		return err
	}
	return fmt.Errorf("%w: %s", ErrOwned, r)
}

// TransferRepoByAdmin assigns repo to team if it is unowned, or transfers it
// from its existing owner if that owner is in allowedSourceTeams (or allowedAll is true).
func (s *Store) TransferRepoByAdmin(ctx context.Context, team, repo string, history bool, allowedSourceTeams map[string]bool, allowedAll bool) error {
	r, err := normRepo(repo)
	if err != nil {
		return err
	}
	h := 0
	if history {
		h = 1
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := s.teamExists(ctx, tx, team); err != nil {
			return err
		}
		var owner string
		err := tx.QueryRowContext(ctx, s.q(`SELECT team FROM team_repos WHERE repo = ?`), r).Scan(&owner)
		switch {
		case err == nil && owner == team:
			_, err = tx.ExecContext(ctx, s.q(`UPDATE team_repos SET history = ? WHERE repo = ?`), h, r)
			return err
		case err == nil:
			if !allowedAll && !allowedSourceTeams[owner] {
				return fmt.Errorf("%w: %s belongs to team %s", ErrOwned, r, owner)
			}
			_, err = tx.ExecContext(ctx, s.q(`UPDATE team_repos SET team = ?, history = ? WHERE repo = ?`), team, h, r)
			return err
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		_, err = tx.ExecContext(ctx, s.q(`INSERT INTO team_repos (repo, team, history) VALUES (?, ?, ?)`), r, team, h)
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
	var res sql.Result
	err = dbaccess.Retry(ctx, func() error {
		var err error
		res, err = s.db.ExecContext(ctx, s.q(`DELETE FROM team_repos WHERE repo = ? AND team = ?`), r, team)
		return err
	})
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
	err = dbaccess.Retry(ctx, func() error {
		team = ""
		return s.db.QueryRowContext(ctx, s.q(`SELECT team FROM team_repos WHERE repo = ?`), r).Scan(&team)
	})
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
	var repos []string
	err := dbaccess.Retry(ctx, func() error {
		repos = []string{}
		rows, err := s.db.QueryContext(ctx, s.q(`SELECT repo FROM team_repos WHERE team = ? ORDER BY repo`), team)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r string
			if err := rows.Scan(&r); err != nil {
				return err
			}
			repos = append(repos, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return repos, nil
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
	err := dbaccess.Retry(ctx, func() error {
		out = map[string]Role{}
		rows, err := s.db.QueryContext(ctx, s.q(`SELECT team, role FROM team_members WHERE principal IN (?`+strings.Repeat(", ?", len(principals)-1)+`)`), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var team, role string
			if err := rows.Scan(&team, &role); err != nil {
				return err
			}
			if r := Role(role); r.rank() > out[team].rank() {
				out[team] = r
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// membershipsOf is memberships for each of principals apart: it adds
// each one's teams to out[principal], which must exist, in one query.
func (s *Store) membershipsOf(ctx context.Context, principals []string, out map[string]map[string]Role) error {
	if len(principals) == 0 {
		return nil
	}
	args := make([]any, len(principals))
	for i, p := range principals {
		args[i] = p
	}
	return dbaccess.Retry(ctx, func() error {
		rows, err := s.db.QueryContext(ctx, s.q(`SELECT principal, team, role FROM team_members WHERE principal IN (?`+strings.Repeat(", ?", len(principals)-1)+`)`), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p, team, role string
			if err := rows.Scan(&p, &team, &role); err != nil {
				return err
			}
			if teams := out[p]; teams != nil {
				if r := Role(role); r.rank() > teams[team].rank() {
					teams[team] = r
				}
			}
		}
		return rows.Err()
	})
}

// repoOwners sets out[repo] to the team that owns each of repos, which
// are normalized, leaving the ones no team owns as they are: one query
// for each maxAdmissionArgs of them.
func (s *Store) repoOwners(ctx context.Context, repos []string, out map[string]string) error {
	for len(repos) > 0 {
		chunk := repos[:min(len(repos), maxAdmissionArgs)]
		repos = repos[len(chunk):]
		args := make([]any, len(chunk))
		for i, r := range chunk {
			args[i] = r
		}
		err := dbaccess.Retry(ctx, func() error {
			rows, err := s.db.QueryContext(ctx, s.q(`SELECT repo, team FROM team_repos WHERE repo IN (?`+strings.Repeat(", ?", len(chunk)-1)+`)`), args...)
			if err != nil {
				return err
			}
			for rows.Next() {
				var r, team string
				if err := rows.Scan(&r, &team); err != nil {
					rows.Close()
					return err
				}
				out[r] = team
			}
			err = rows.Err()
			rows.Close()
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// reposOf maps each repository the teams own to its ownership.
func (s *Store) reposOf(ctx context.Context, teams map[string]Role) (map[string]ownership, error) {
	out := map[string]ownership{}
	if len(teams) == 0 {
		return out, nil
	}
	var args []any
	for t := range teams {
		args = append(args, t)
	}
	err := dbaccess.Retry(ctx, func() error {
		out = map[string]ownership{}
		rows, err := s.db.QueryContext(ctx, s.q(`SELECT repo, team, history FROM team_repos WHERE team IN (?`+strings.Repeat(", ?", len(args)-1)+`)`), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				repo, team string
				history    int64
			)
			if err := rows.Scan(&repo, &team, &history); err != nil {
				return err
			}
			out[repo] = ownership{team: team, history: history != 0}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
