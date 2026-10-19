package grainfiles

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/hub/dbaccess"
	_ "github.com/jackc/pgx/v5/stdlib" // "pgx"
	_ "modernc.org/sqlite"             // "sqlite"
)

// maxConns caps the Grainfile store's PostgreSQL connections, all of which it
// keeps open once made, matching hub/directory and hub/registry.
const maxConns = 8

// Discoverable Grainfile event types recorded in grainfile_events
// (docs/hub.md §6.4, §6.5).
const (
	EventTaskReady           = "task_ready"
	EventTaskRemoved         = "task_removed"
	EventProcessUpdated      = "process_updated"
	EventProcessTriggered    = "process_triggered"
	EventProcessRemoved      = "process_removed"
	EventCapabilityUpdated   = "capability_updated"
	EventCapabilityRemoved   = "capability_removed"
	EventDependencyCompleted = "dependency_completed"
)

var (
	// ErrNotFound is an indexed Grainfile or subscription that does not exist.
	ErrNotFound = errors.New("grainfiles: not found")
	// ErrInvalid is a malformed subscription, event, or store argument.
	ErrInvalid = errors.New("grainfiles: invalid")
	// ErrUnauthorized is a request for a Grainfile or reference that the node
	// or source repository has not authorized.
	ErrUnauthorized = errors.New("grainfiles: unauthorized")
)

// Tables are the Grainfile store's database tables.
var Tables = []string{
	"grainfile_index",
	"grainfile_subscriptions",
	"grainfile_events",
}

// RepoTeamResolver resolves the team owning a repository slug ("owner/repo")
// in hub/directory.
type RepoTeamResolver interface {
	RepoOwner(ctx context.Context, repo string) (string, error)
}

// Store persists indexed Grainfile front-matter metadata, node subscriptions,
// and discoverable Grainfile events in PostgreSQL or SQLite. It stores
// metadata only—never source files or Grainfile Markdown bodies at rest
// (docs/hub.md §1.1, §6.3).
type Store struct {
	db       *sql.DB
	postgres bool
	txMu     sync.Mutex
	// Now, when set, is the clock used to stamp rows and evaluate not_before.
	Now func() time.Time
	// SameTeam, when set, reports whether two non-empty node owners share at
	// least one team in the Hub directory (allowing nodes in the same team to
	// subscribe to the same deployment recipient URL).
	SameTeam func(ctx context.Context, ownerA, ownerB string) bool
	// AuthorizeOwnerRepo, when set, reports whether a node owner is admitted in
	// the Hub directory for the repository of a recipient, repo, folder, or
	// grainfile URL.
	AuthorizeOwnerRepo func(ctx context.Context, owner, repoURL string) bool
	// TeamResolver, when set, resolves the team owning a repository ("owner/repo")
	// in hub/directory so cross-repository `when` triggers only fire across
	// repositories belonging to the same team.
	TeamResolver RepoTeamResolver
	// SameRepoTeam, when set, reports whether two repository URLs belong to the
	// same team in hub/directory.
	SameRepoTeam func(ctx context.Context, repoA, repoB string) bool
	// HubURL, when set, is the base URL of the Hub ("https://<hub-host>") used
	// to construct canonical deployment recipient URLs ("https://<hub-host>/deployments/<id>").
	HubURL string
}

// Open opens the Grainfile store at url: "postgres://..." (or
// "postgresql://...") for PostgreSQL, "sqlite:PATH" or a bare path for SQLite
// (":memory:" for an in-memory database). It creates its tables if they are
// missing.
func Open(ctx context.Context, url string) (*Store, error) {
	return openStore(ctx, url, true)
}

// OpenStore is an alias for Open matching hub/registry.OpenStore.
func OpenStore(ctx context.Context, url string) (*Store, error) {
	return openStore(ctx, url, true)
}

// OpenExisting opens the Grainfile store at url without creating tables,
// failing if any table in Tables is missing or unreadable.
func OpenExisting(ctx context.Context, url string) (*Store, error) {
	return openStore(ctx, url, false)
}

// OpenExistingStore is an alias for OpenExisting matching
// hub/registry.OpenExistingStore.
func OpenExistingStore(ctx context.Context, url string) (*Store, error) {
	return openStore(ctx, url, false)
}

func openStore(ctx context.Context, url string, create bool) (*Store, error) {
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
		db.SetMaxOpenConns(1)
	}
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, postgres: postgres}
	if create {
		err = s.migrate(ctx)
	} else {
		err = dbaccess.Check(ctx, db, "grainfiles", Tables...)
		if err == nil {
			err = dbaccess.CheckColumns(ctx, db, "grainfiles", added...)
		}
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the store's database connection pool.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks that the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Stats returns the store's connection pool statistics.
func (s *Store) Stats() sql.DBStats { return s.db.Stats() }

const storeSchema = `
CREATE TABLE IF NOT EXISTS grainfile_index (
	repo TEXT NOT NULL,
	path TEXT NOT NULL,
	commit_sha TEXT NOT NULL,
	ref TEXT NOT NULL DEFAULT '',
	url TEXT NOT NULL,
	kind TEXT NOT NULL,
	title TEXT NOT NULL,
	recipient TEXT NOT NULL,
	requestor TEXT NOT NULL DEFAULT '',
	from_template TEXT NOT NULL DEFAULT '',
	draft INTEGER NOT NULL DEFAULT 0,
	not_before BIGINT NOT NULL DEFAULT 0,
	created_at BIGINT NOT NULL DEFAULT 0,
	"references" TEXT NOT NULL DEFAULT '[]',
	authorized_references TEXT NOT NULL DEFAULT '[]',
	depends_on TEXT NOT NULL DEFAULT '[]',
	capabilities TEXT NOT NULL DEFAULT '[]',
	trigger_with TEXT NOT NULL DEFAULT '[]',
	on_spec TEXT NOT NULL DEFAULT '{}',
	gate INTEGER NOT NULL DEFAULT 0,
	is_template INTEGER NOT NULL DEFAULT 0,
	template_vars TEXT NOT NULL DEFAULT '[]',
	ready_emitted INTEGER NOT NULL DEFAULT 0,
	removed INTEGER NOT NULL DEFAULT 0,
	updated_at BIGINT NOT NULL,
	PRIMARY KEY (repo, path, commit_sha)
);
CREATE INDEX IF NOT EXISTS grainfile_index_recipient ON grainfile_index (recipient, draft, updated_at);
CREATE INDEX IF NOT EXISTS grainfile_index_url ON grainfile_index (url, commit_sha);
CREATE TABLE IF NOT EXISTS grainfile_subscriptions (
	node_id TEXT PRIMARY KEY,
	owner TEXT NOT NULL DEFAULT '',
	recipients TEXT NOT NULL DEFAULT '[]',
	repos TEXT NOT NULL DEFAULT '[]',
	folders TEXT NOT NULL DEFAULT '[]',
	grainfiles TEXT NOT NULL DEFAULT '[]',
	cursor BIGINT NOT NULL DEFAULT 0,
	revision BIGINT NOT NULL DEFAULT 1,
	updated_at BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS grainfile_events (
	seq BIGINT PRIMARY KEY,
	type TEXT NOT NULL,
	repo TEXT NOT NULL,
	path TEXT NOT NULL DEFAULT '',
	commit_sha TEXT NOT NULL DEFAULT '',
	ref TEXT NOT NULL DEFAULT '',
	grainfile_url TEXT NOT NULL,
	kind TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL DEFAULT '',
	recipient TEXT NOT NULL DEFAULT '',
	trigger_source TEXT NOT NULL DEFAULT '',
	triggered_by TEXT NOT NULL DEFAULT '',
	dependency_url TEXT NOT NULL DEFAULT '',
	dependency_state TEXT NOT NULL DEFAULT '',
	gate INTEGER NOT NULL DEFAULT 0,
	payload TEXT NOT NULL DEFAULT '{}',
	created_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS grainfile_events_recipient ON grainfile_events (recipient, seq);
CREATE INDEX IF NOT EXISTS grainfile_events_url ON grainfile_events (grainfile_url, seq)
`

var added = []dbaccess.Column{
	{Table: "grainfile_index", Name: "from_template", Add: `ALTER TABLE grainfile_index ADD COLUMN from_template TEXT NOT NULL DEFAULT ''`},
	{Table: "grainfile_index", Name: "not_before", Add: `ALTER TABLE grainfile_index ADD COLUMN not_before BIGINT NOT NULL DEFAULT 0`},
	{Table: "grainfile_index", Name: "created_at", Add: `ALTER TABLE grainfile_index ADD COLUMN created_at BIGINT NOT NULL DEFAULT 0`},
	{Table: "grainfile_index", Name: "authorized_references", Add: `ALTER TABLE grainfile_index ADD COLUMN authorized_references TEXT NOT NULL DEFAULT '[]'`},
	{Table: "grainfile_index", Name: "depends_on", Add: `ALTER TABLE grainfile_index ADD COLUMN depends_on TEXT NOT NULL DEFAULT '[]'`},
	{Table: "grainfile_index", Name: "capabilities", Add: `ALTER TABLE grainfile_index ADD COLUMN capabilities TEXT NOT NULL DEFAULT '[]'`},
	{Table: "grainfile_index", Name: "trigger_with", Add: `ALTER TABLE grainfile_index ADD COLUMN trigger_with TEXT NOT NULL DEFAULT '[]'`},
	{Table: "grainfile_index", Name: "on_spec", Add: `ALTER TABLE grainfile_index ADD COLUMN on_spec TEXT NOT NULL DEFAULT '{}'`},
	{Table: "grainfile_index", Name: "gate", Add: `ALTER TABLE grainfile_index ADD COLUMN gate INTEGER NOT NULL DEFAULT 0`},
	{Table: "grainfile_index", Name: "is_template", Add: `ALTER TABLE grainfile_index ADD COLUMN is_template INTEGER NOT NULL DEFAULT 0`},
	{Table: "grainfile_index", Name: "template_vars", Add: `ALTER TABLE grainfile_index ADD COLUMN template_vars TEXT NOT NULL DEFAULT '[]'`},
	{Table: "grainfile_index", Name: "ready_emitted", Add: `ALTER TABLE grainfile_index ADD COLUMN ready_emitted INTEGER NOT NULL DEFAULT 0`},
	{Table: "grainfile_index", Name: "removed", Add: `ALTER TABLE grainfile_index ADD COLUMN removed INTEGER NOT NULL DEFAULT 0`},
	{Table: "grainfile_events", Name: "trigger_source", Add: `ALTER TABLE grainfile_events ADD COLUMN trigger_source TEXT NOT NULL DEFAULT ''`},
	{Table: "grainfile_events", Name: "triggered_by", Add: `ALTER TABLE grainfile_events ADD COLUMN triggered_by TEXT NOT NULL DEFAULT ''`},
	{Table: "grainfile_events", Name: "dependency_url", Add: `ALTER TABLE grainfile_events ADD COLUMN dependency_url TEXT NOT NULL DEFAULT ''`},
	{Table: "grainfile_events", Name: "dependency_state", Add: `ALTER TABLE grainfile_events ADD COLUMN dependency_state TEXT NOT NULL DEFAULT ''`},
	{Table: "grainfile_events", Name: "gate", Add: `ALTER TABLE grainfile_events ADD COLUMN gate INTEGER NOT NULL DEFAULT 0`},
}

func (s *Store) migrate(ctx context.Context) error {
	for _, stmt := range strings.Split(storeSchema, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("grainfiles: creating tables: %w", err)
		}
	}
	return dbaccess.AddColumns(ctx, s.db, "grainfiles", added...)
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

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

func (s *Store) tx(ctx context.Context, f func(*sql.Tx) error) error {
	s.txMu.Lock()
	defer s.txMu.Unlock()
	const maxAttempts = 64
	var err error
	for attempt := range maxAttempts {
		err = dbaccess.InTx(ctx, s.db, f)
		if err == nil || !isRetryableTxError(err) || ctx.Err() != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt%8+1) * time.Millisecond):
		}
	}
	return err
}

func isRetryableTxError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "23505") ||
		strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "40001") ||
		strings.Contains(msg, "40P01") ||
		strings.Contains(msg, "Aborted") ||
		strings.Contains(msg, "serialization") ||
		strings.Contains(msg, "deadlock") ||
		strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "database is locked")
}

// IndexRecord is a row in grainfile_index storing parsed and resolved Grainfile
// front-matter metadata per (repo, path, commit_sha). It never holds the
// Grainfile Markdown prompt body or source code.
type IndexRecord struct {
	Grainfile            string    `json:"grainfile"`
	SpecSummaryPath      string    `json:"spec_summary_path"`
	SpecPath             string    `json:"spec_path"`
	Repo                 string    `json:"repo"`
	Path                 string    `json:"path"`
	CommitSHA            string    `json:"commit_sha"`
	Ref                  string    `json:"ref,omitempty"`
	URL                  string    `json:"url"`
	Kind                 Kind      `json:"kind"`
	Title                string    `json:"title"`
	Recipient            string    `json:"recipient"`
	Requestor            string    `json:"requestor,omitempty"`
	FromTemplate         string    `json:"from_template,omitempty"`
	Draft                bool      `json:"draft"`
	NotBefore            time.Time `json:"not_before,omitempty,omitzero"`
	CreatedAt            time.Time `json:"created_at,omitempty,omitzero"`
	References           []string  `json:"references,omitempty"`
	AuthorizedReferences []string  `json:"authorized_references,omitempty"`
	DependsOn            []string  `json:"depends_on,omitempty"`
	Capabilities         []string  `json:"capabilities,omitempty"`
	When                 Triggers  `json:"when,omitempty,omitzero"`
	On                   Triggers  `json:"on,omitempty,omitzero"`
	OnSpec               Triggers  `json:"on_spec,omitempty,omitzero"`
	Gate                 bool      `json:"gate,omitempty"`
	IsTemplate           bool      `json:"is_template"`
	TemplateVars         []string  `json:"template_vars,omitempty"`
	SubscribedNodes      []string  `json:"subscribed_nodes,omitempty"`
	RecentEvents         []Event   `json:"recent_events,omitempty"`
	UpdatedAt            time.Time `json:"updated_at"`

	readyEmitted bool
	removed      bool
}

// IndexedGrainfile is an alias for IndexRecord.
type IndexedGrainfile = IndexRecord

// ToGrainfile reconstructs a metadata-only *Grainfile (with an empty Body) from
// an IndexRecord so trigger and reference-authorization methods can be invoked.
func (r IndexRecord) ToGrainfile() *Grainfile {
	ver := r.Grainfile
	if ver == "" {
		ver = VersionV1
	}
	specSummary := r.SpecSummaryPath
	specPath := r.SpecPath
	if spec, ok := LookupVersion(ver); ok {
		if specSummary == "" {
			specSummary = spec.SpecSummaryPath
		}
		if specPath == "" {
			specPath = spec.SpecPath
		}
	}
	triggers := r.When
	if triggers.IsZero() {
		triggers = r.OnSpec
	}
	return &Grainfile{
		Grainfile:            ver,
		SpecSummaryPath:      specSummary,
		SpecPath:             specPath,
		Kind:                 r.Kind,
		Path:                 r.Path,
		URL:                  r.URL,
		Repo:                 r.Repo,
		Ref:                  r.Ref,
		Commit:               r.CommitSHA,
		Title:                r.Title,
		Recipient:            r.Recipient,
		Requestor:            r.Requestor,
		FromTemplate:         r.FromTemplate,
		Draft:                r.Draft,
		NotBefore:            r.NotBefore,
		CreatedAt:            r.CreatedAt,
		References:           slices.Clone(r.References),
		DependsOn:            slices.Clone(r.DependsOn),
		Capabilities:         slices.Clone(r.Capabilities),
		ResolvedCapabilities: slices.Clone(r.Capabilities),
		When:                 triggers,
		On:                   triggers,
		Gate:                 r.Gate,
		IsTemplate:           r.IsTemplate,
		TemplateVars:         slices.Clone(r.TemplateVars),
	}
}

// Subscriptions records what recipient URL(s), repository URLs, folder URLs, or
// specific .task.md / .process.md URLs a registered node subscribes to, along
// with its monotonic delivery cursor and subscription revision.
type Subscriptions struct {
	NodeID      string    `json:"node_id"`
	Owner       string    `json:"owner,omitempty"`
	Recipients  []string  `json:"recipients,omitempty"`
	Repos       []string  `json:"repos,omitempty"`
	Folders     []string  `json:"folders,omitempty"`
	Grainfiles  []string  `json:"grainfiles,omitempty"`
	Cursor      int64     `json:"cursor"`
	ResetCursor bool      `json:"reset_cursor,omitempty"`
	Revision    int64     `json:"revision"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Event is a discoverable Grainfile event stored in grainfile_events
// (task_ready, task_removed, process_updated, process_triggered, process_removed, dependency_completed).
type Event struct {
	Seq                  int64     `json:"seq"`
	Type                 string    `json:"type"`
	Action               string    `json:"action,omitempty"`
	Grainfile            string    `json:"grainfile,omitempty"`
	SpecSummaryPath      string    `json:"spec_summary_path,omitempty"`
	SpecPath             string    `json:"spec_path,omitempty"`
	Repo                 string    `json:"repo"`
	Path                 string    `json:"path,omitempty"`
	CommitSHA            string    `json:"commit_sha,omitempty"`
	Ref                  string    `json:"ref,omitempty"`
	Branch               string    `json:"branch,omitempty"`
	PRNumber             int       `json:"pr_number,omitempty"`
	Paths                []string  `json:"paths,omitempty"`
	TriggerRepo          string    `json:"trigger_repo,omitempty"`
	TriggerCommitSHA     string    `json:"trigger_commit_sha,omitempty"`
	GrainfileURL         string    `json:"grainfile_url"`
	Kind                 Kind      `json:"kind,omitempty"`
	Title                string    `json:"title,omitempty"`
	Recipient            string    `json:"recipient,omitempty"`
	Requestor            string    `json:"requestor,omitempty"`
	FromTemplate         string    `json:"from_template,omitempty"`
	NotBefore            time.Time `json:"not_before,omitempty,omitzero"`
	References           []string  `json:"references,omitempty"`
	AuthorizedReferences []string  `json:"authorized_references,omitempty"`
	DependsOn            []string  `json:"depends_on,omitempty"`
	Capabilities         []string  `json:"capabilities,omitempty"`
	When                 Triggers  `json:"when,omitempty,omitzero"`
	On                   Triggers  `json:"on,omitempty,omitzero"`
	OnSpec               Triggers  `json:"on_spec,omitempty,omitzero"`
	Gate                 bool      `json:"gate,omitempty"`
	IsTemplate           bool      `json:"is_template,omitempty"`
	TemplateVars         []string  `json:"template_vars,omitempty"`
	TriggerSource        string    `json:"trigger_source,omitempty"`
	TriggeredBy          string    `json:"triggered_by,omitempty"`
	WorkflowName         string    `json:"workflow_name,omitempty"`
	WorkflowPath         string    `json:"workflow_path,omitempty"`
	WorkflowEvent        string    `json:"workflow_event,omitempty"`
	WorkflowURL          string    `json:"workflow_url,omitempty"`
	ChangedPaths         []string  `json:"changed_paths,omitempty"`
	DependencyURL        string    `json:"dependency_url,omitempty"`
	DependencyState      string    `json:"dependency_state,omitempty"`
	Reason               string    `json:"reason,omitempty"`
	CreatedAt            time.Time `json:"created_at,omitzero"`
}

// GrainfileEvent is an alias for Event.
type GrainfileEvent = Event

type eventPayload struct {
	Action               string   `json:"action,omitempty"`
	Grainfile            string   `json:"grainfile,omitempty"`
	Requestor            string   `json:"requestor,omitempty"`
	FromTemplate         string   `json:"from_template,omitempty"`
	Branch               string   `json:"branch,omitempty"`
	PRNumber             int      `json:"pr_number,omitempty"`
	Paths                []string `json:"paths,omitempty"`
	TriggerRepo          string   `json:"trigger_repo,omitempty"`
	TriggerCommitSHA     string   `json:"trigger_commit_sha,omitempty"`
	WorkflowName         string   `json:"workflow_name,omitempty"`
	WorkflowPath         string   `json:"workflow_path,omitempty"`
	WorkflowEvent        string   `json:"workflow_event,omitempty"`
	WorkflowURL          string   `json:"workflow_url,omitempty"`
	NotBeforeMillis      int64    `json:"not_before_ms,omitempty"`
	References           []string `json:"references,omitempty"`
	AuthorizedReferences []string `json:"authorized_references,omitempty"`
	DependsOn            []string `json:"depends_on,omitempty"`
	Capabilities         []string `json:"capabilities,omitempty"`
	OnSpec               Triggers `json:"on_spec,omitempty"`
	IsTemplate           bool     `json:"is_template,omitempty"`
	TemplateVars         []string `json:"template_vars,omitempty"`
	ChangedPaths         []string `json:"changed_paths,omitempty"`
	Reason               string   `json:"reason,omitempty"`
}

// IndexGrainfile stores or updates the resolved metadata of g in
// grainfile_index (never storing g.Body). If g is not a draft and its
// not_before timestamp (if any) has been reached at s.Now(), IndexGrainfile
// also records the corresponding discoverable event (task_ready for .task.md,
// process_updated for .process.md) in grainfile_events.
func (s *Store) IndexGrainfile(ctx context.Context, g *Grainfile) (IndexRecord, error) {
	return s.indexGrainfileAt(ctx, g, s.now())
}

func (s *Store) indexGrainfileAt(ctx context.Context, g *Grainfile, at time.Time) (IndexRecord, error) {
	if g == nil {
		return IndexRecord{}, fmt.Errorf("%w: nil grainfile", ErrInvalid)
	}
	if !g.Kind.Valid() {
		return IndexRecord{}, fmt.Errorf("%w: invalid grainfile kind %q", ErrInvalid, g.Kind)
	}
	if g.Repo == "" || g.Path == "" {
		return IndexRecord{}, fmt.Errorf("%w: grainfile must have repo and path set", ErrInvalid)
	}

	cp := *g
	cp.Body = ""
	if s.HubURL != "" && strings.HasPrefix(strings.TrimSpace(cp.Recipient), "/deployments/") {
		if depID := DeploymentIDFromRecipient(cp.Recipient); depID != "" {
			cp.Recipient = CanonicalDeploymentURL(s.HubURL, depID)
		}
	}
	hasLocalRef := false
	for _, c := range cp.Capabilities {
		if !strings.Contains(c, "://") {
			hasLocalRef = true
			break
		}
	}
	if cp.URL == "" || hasLocalRef || (cp.Kind == KindTask && !strings.Contains(cp.Recipient, "://")) || (cp.Recipient != "" && !strings.Contains(cp.Recipient, "://")) || (cp.FromTemplate != "" && !strings.Contains(cp.FromTemplate, "://")) {
		if err := cp.ResolveSource(Source{
			RepoURL: cp.Repo,
			Ref:     cp.Ref,
			Commit:  cp.Commit,
			Path:    cp.Path,
		}); err != nil {
			return IndexRecord{}, err
		}
	}
	canonRepo, err := CanonicalRepoURL(cp.Repo)
	if err != nil {
		return IndexRecord{}, err
	}
	cp.Repo = canonRepo
	cp.Path = cleanPosixPath(strings.TrimPrefix(filepath.ToSlash(cp.Path), "/"))
	commitSHA := strings.TrimSpace(cp.Commit)
	if commitSHA == "" {
		commitSHA = normalizeURLRef(cp.Ref, "")
	}
	cp.Commit = commitSHA

	var authRefs []string
	if !cp.Draft {
		sameRepo, _ := cp.AuthorizedReferences(cp.Repo)
		authRefs = sameRepo
	}
	if authRefs == nil {
		authRefs = []string{}
	}
	refs := cp.References
	if refs == nil {
		refs = []string{}
	}
	deps := cp.DependsOn
	if deps == nil {
		deps = []string{}
	}
	caps := cp.Capabilities
	if len(caps) == 0 && len(cp.ResolvedCapabilities) > 0 {
		caps = slices.Clone(cp.ResolvedCapabilities)
	}
	if caps == nil {
		caps = []string{}
	}
	if IsTemplatePath(cp.Path) {
		cp.IsTemplate = true
	}
	tvars := slices.Clone(cp.TemplateVars)
	if tvars == nil {
		tvars = []string{}
	}

	if at.IsZero() {
		at = s.now()
	}
	now := at.UTC().Truncate(time.Millisecond)
	activeNow := cp.ActiveAt(now)
	if cp.Kind == KindTask && cp.Ref != "" && !isDefaultOrProtectedBranch(cp.Ref, "") {
		activeNow = false
	}

	ver := cp.Grainfile
	if ver == "" {
		ver = VersionV1
	}
	specSummary := cp.SpecSummaryPath
	specPath := cp.SpecPath
	if spec, ok := LookupVersion(ver); ok {
		if specSummary == "" {
			specSummary = spec.SpecSummaryPath
		}
		if specPath == "" {
			specPath = spec.SpecPath
		}
	}
	triggers := cp.When
	if triggers.IsZero() {
		triggers = cp.On
	}

	rec := IndexRecord{
		Grainfile:            ver,
		SpecSummaryPath:      specSummary,
		SpecPath:             specPath,
		Repo:                 cp.Repo,
		Path:                 cp.Path,
		CommitSHA:            commitSHA,
		Ref:                  cp.Ref,
		URL:                  cp.URL,
		Kind:                 cp.Kind,
		Title:                cp.Title,
		Recipient:            cp.Recipient,
		Requestor:            cp.Requestor,
		FromTemplate:         cp.FromTemplate,
		Draft:                cp.Draft,
		NotBefore:            cp.NotBefore.UTC(),
		CreatedAt:            cp.CreatedAt.UTC(),
		References:           refs,
		AuthorizedReferences: authRefs,
		DependsOn:            deps,
		Capabilities:         caps,
		When:                 triggers,
		On:                   triggers,
		OnSpec:               triggers,
		Gate:                 cp.Gate,
		IsTemplate:           cp.IsTemplate,
		TemplateVars:         tvars,
		UpdatedAt:            now,
		readyEmitted:         activeNow,
		removed:              false,
	}

	refsJSON, err := json.Marshal(rec.References)
	if err != nil {
		return IndexRecord{}, err
	}
	authRefsJSON, err := json.Marshal(rec.AuthorizedReferences)
	if err != nil {
		return IndexRecord{}, err
	}
	depsJSON, err := json.Marshal(rec.DependsOn)
	if err != nil {
		return IndexRecord{}, err
	}
	capsJSON, err := json.Marshal(rec.Capabilities)
	if err != nil {
		return IndexRecord{}, err
	}
	onJSON, err := json.Marshal(rec.OnSpec)
	if err != nil {
		return IndexRecord{}, err
	}
	tvarsJSON, err := json.Marshal(rec.TemplateVars)
	if err != nil {
		return IndexRecord{}, err
	}

	var notBeforeMS, createdAtMS int64
	if !rec.NotBefore.IsZero() {
		notBeforeMS = rec.NotBefore.UnixMilli()
	}
	if !rec.CreatedAt.IsZero() {
		createdAtMS = rec.CreatedAt.UnixMilli()
	}

	err = s.tx(ctx, func(tx *sql.Tx) error {
		var prevLatestCommit, prevLatestRecipient string
		var prevLatestDraft, prevLatestReady int
		var prevLatestUpdatedMS int64
		hasPrevLatest := false
		if err := tx.QueryRowContext(ctx, s.q(`SELECT commit_sha, draft, ready_emitted, recipient, updated_at FROM grainfile_index WHERE LOWER(repo) = LOWER(?) AND path = ? AND removed = 0 ORDER BY updated_at DESC LIMIT 1`),
			rec.Repo, rec.Path).Scan(&prevLatestCommit, &prevLatestDraft, &prevLatestReady, &prevLatestRecipient, &prevLatestUpdatedMS); err == nil {
			hasPrevLatest = true
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if hasPrevLatest && prevLatestCommit != rec.CommitSHA {
			if minNext := time.UnixMilli(prevLatestUpdatedMS + 1).UTC(); !rec.UpdatedAt.After(time.UnixMilli(prevLatestUpdatedMS).UTC()) {
				rec.UpdatedAt = minNext
			}
		}

		var prevReady int
		var prevTitle, prevRecipient string
		var prevNotBeforeMS int64
		err := tx.QueryRowContext(ctx, s.q(`SELECT ready_emitted, title, recipient, not_before FROM grainfile_index WHERE repo = ? AND path = ? AND commit_sha = ?`),
			rec.Repo, rec.Path, rec.CommitSHA).Scan(&prevReady, &prevTitle, &prevRecipient, &prevNotBeforeMS)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		editedIncomplete := prevReady != 0 && (prevTitle != rec.Title || prevRecipient != rec.Recipient || prevNotBeforeMS != notBeforeMS)
		emitNow := activeNow && (prevReady == 0 || editedIncomplete)
		readyFlag := 0
		if activeNow || prevReady != 0 {
			if !rec.Draft && activeNow {
				readyFlag = 1
			}
		}

		// Task idempotency rule (§9): once a .task.grain.md has completed (done),
		// subsequent pushes, merges, or edits to that (repo_url, file_path) do not
		// re-trigger or re-queue it.
		var taskDone bool
		if rec.Kind == KindTask {
			done, err := s.isTaskCompletedTx(tx, ctx, rec.Repo, rec.Path)
			if err != nil {
				return err
			}
			taskDone = done
			if done {
				emitNow = false
				readyFlag = 1
				rec.readyEmitted = true
			}
		}

		_, err = tx.ExecContext(ctx, s.q(`INSERT INTO grainfile_index (
			repo, path, commit_sha, ref, url, kind, title, recipient, requestor, from_template,
			draft, not_before, created_at, "references", authorized_references,
			depends_on, capabilities, trigger_with, on_spec, gate, is_template, template_vars,
			ready_emitted, removed, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '[]', ?, ?, ?, ?, ?, 0, ?)
		ON CONFLICT (repo, path, commit_sha) DO UPDATE SET
			ref = EXCLUDED.ref,
			url = EXCLUDED.url,
			kind = EXCLUDED.kind,
			title = EXCLUDED.title,
			recipient = EXCLUDED.recipient,
			requestor = EXCLUDED.requestor,
			from_template = EXCLUDED.from_template,
			draft = EXCLUDED.draft,
			not_before = EXCLUDED.not_before,
			created_at = EXCLUDED.created_at,
			"references" = EXCLUDED."references",
			authorized_references = EXCLUDED.authorized_references,
			depends_on = EXCLUDED.depends_on,
			capabilities = EXCLUDED.capabilities,
			trigger_with = EXCLUDED.trigger_with,
			on_spec = EXCLUDED.on_spec,
			gate = EXCLUDED.gate,
			is_template = EXCLUDED.is_template,
			template_vars = EXCLUDED.template_vars,
			ready_emitted = EXCLUDED.ready_emitted,
			removed = 0,
			updated_at = EXCLUDED.updated_at`),
			rec.Repo, rec.Path, rec.CommitSHA, rec.Ref, rec.URL, string(rec.Kind),
			rec.Title, rec.Recipient, rec.Requestor, rec.FromTemplate, boolToInt(rec.Draft),
			notBeforeMS, createdAtMS, string(refsJSON), string(authRefsJSON),
			string(depsJSON), string(capsJSON), string(onJSON), boolToInt(rec.Gate),
			boolToInt(rec.IsTemplate), string(tvarsJSON),
			readyFlag, rec.UpdatedAt.UnixMilli())
		if err != nil {
			return err
		}
		if hasPrevLatest && prevLatestCommit != rec.CommitSHA {
			if rec.Kind != KindTask || rec.Ref == "" || isDefaultOrProtectedBranch(rec.Ref, "") {
				if _, err := tx.ExecContext(ctx, s.q(`UPDATE grainfile_index SET ready_emitted = 1 WHERE LOWER(repo) = LOWER(?) AND path = ? AND commit_sha <> ?`),
					rec.Repo, rec.Path, rec.CommitSHA); err != nil {
					return err
				}
			}
		}

		if depID := DeploymentIDFromRecipient(rec.Recipient); depID != "" && (strings.HasPrefix(rec.Recipient, "https://") || isLoopbackDeploymentHTTPURL(rec.Recipient)) {
			var subRecJSON string
			if err := tx.QueryRowContext(ctx, s.q(`SELECT recipients FROM grainfile_subscriptions WHERE LOWER(node_id) = LOWER(?)`), depID).Scan(&subRecJSON); err == nil {
				var subRecs []string
				_ = json.Unmarshal([]byte(subRecJSON), &subRecs)
				if !slices.Contains(subRecs, rec.Recipient) {
					subRecs = append(subRecs, rec.Recipient)
					if b, mErr := json.Marshal(subRecs); mErr == nil {
						_, _ = tx.ExecContext(ctx, s.q(`UPDATE grainfile_subscriptions SET recipients = ? WHERE LOWER(node_id) = LOWER(?)`), string(b), depID)
					}
				}
			}
		}

		if hasPrevLatest && prevLatestDraft == 0 && rec.Draft {
			if rec.Kind == KindCapability {
				_, err = s.insertEventTx(tx, ctx, Event{
					Type:                 EventCapabilityRemoved,
					Action:               "removed",
					Grainfile:            rec.Grainfile,
					SpecSummaryPath:      rec.SpecSummaryPath,
					SpecPath:             rec.SpecPath,
					Repo:                 rec.Repo,
					Path:                 rec.Path,
					CommitSHA:            rec.CommitSHA,
					Ref:                  rec.Ref,
					GrainfileURL:         rec.URL,
					Kind:                 rec.Kind,
					Title:                rec.Title,
					FromTemplate:         rec.FromTemplate,
					NotBefore:            rec.NotBefore,
					References:           rec.References,
					AuthorizedReferences: rec.AuthorizedReferences,
					IsTemplate:           rec.IsTemplate,
					TemplateVars:         rec.TemplateVars,
					Reason:               "capability grainfile marked draft",
					CreatedAt:            now,
				})
				return err
			}
			if rec.Kind == KindProcess {
				_, err = s.insertEventTx(tx, ctx, Event{
					Type:                 EventProcessRemoved,
					Action:               "removed",
					Grainfile:            rec.Grainfile,
					SpecSummaryPath:      rec.SpecSummaryPath,
					SpecPath:             rec.SpecPath,
					Repo:                 rec.Repo,
					Path:                 rec.Path,
					CommitSHA:            rec.CommitSHA,
					Ref:                  rec.Ref,
					GrainfileURL:         rec.URL,
					Kind:                 rec.Kind,
					Title:                rec.Title,
					FromTemplate:         rec.FromTemplate,
					NotBefore:            rec.NotBefore,
					References:           rec.References,
					AuthorizedReferences: rec.AuthorizedReferences,
					DependsOn:            rec.DependsOn,
					Capabilities:         rec.Capabilities,
					When:                 rec.When,
					OnSpec:               rec.OnSpec,
					Gate:                 rec.Gate,
					IsTemplate:           rec.IsTemplate,
					TemplateVars:         rec.TemplateVars,
					Reason:               "process grainfile marked draft",
					CreatedAt:            now,
				})
				return err
			}
			if rec.Kind == KindTask && !taskDone {
				cancelled, cErr := s.isTaskCancelledTx(tx, ctx, rec.Repo, rec.Path)
				if cErr != nil || cancelled {
					return cErr
				}
				_, err = s.insertEventTx(tx, ctx, Event{
					Type:                 EventTaskRemoved,
					Action:               "removed",
					Grainfile:            rec.Grainfile,
					SpecSummaryPath:      rec.SpecSummaryPath,
					SpecPath:             rec.SpecPath,
					Repo:                 rec.Repo,
					Path:                 rec.Path,
					CommitSHA:            rec.CommitSHA,
					Ref:                  rec.Ref,
					GrainfileURL:         rec.URL,
					Kind:                 rec.Kind,
					Title:                rec.Title,
					Recipient:            firstNonEmpty(rec.Recipient, prevLatestRecipient),
					Requestor:            rec.Requestor,
					FromTemplate:         rec.FromTemplate,
					NotBefore:            rec.NotBefore,
					References:           rec.References,
					AuthorizedReferences: rec.AuthorizedReferences,
					DependsOn:            rec.DependsOn,
					Capabilities:         rec.Capabilities,
					IsTemplate:           rec.IsTemplate,
					TemplateVars:         rec.TemplateVars,
					DependencyURL:        rec.URL,
					DependencyState:      "cancelled",
					Reason:               "task grainfile marked draft",
					CreatedAt:            now,
				})
				return err
			}
			return nil
		}

		if emitNow {
			evType := EventTaskReady
			switch rec.Kind {
			case KindProcess:
				evType = EventProcessUpdated
			case KindCapability:
				evType = EventCapabilityUpdated
			}
			_, err = s.insertEventTx(tx, ctx, Event{
				Type:                 evType,
				Grainfile:            rec.Grainfile,
				SpecSummaryPath:      rec.SpecSummaryPath,
				SpecPath:             rec.SpecPath,
				Repo:                 rec.Repo,
				Path:                 rec.Path,
				CommitSHA:            rec.CommitSHA,
				Ref:                  rec.Ref,
				GrainfileURL:         rec.URL,
				Kind:                 rec.Kind,
				Title:                rec.Title,
				Recipient:            rec.Recipient,
				Requestor:            rec.Requestor,
				FromTemplate:         rec.FromTemplate,
				NotBefore:            rec.NotBefore,
				References:           rec.References,
				AuthorizedReferences: rec.AuthorizedReferences,
				DependsOn:            rec.DependsOn,
				Capabilities:         rec.Capabilities,
				When:                 rec.When,
				OnSpec:               rec.OnSpec,
				Gate:                 rec.Gate,
				IsTemplate:           rec.IsTemplate,
				TemplateVars:         rec.TemplateVars,
				CreatedAt:            now,
			})
			return err
		}
		return nil
	})
	if err != nil {
		return IndexRecord{}, err
	}
	return rec, nil
}

func (s *Store) isTaskCompletedTx(tx *sql.Tx, ctx context.Context, canonRepo, cleanPath string) (bool, error) {
	rows, err := tx.QueryContext(ctx, s.q(`SELECT repo, path, dependency_url, dependency_state FROM grainfile_events WHERE type = ?`), EventDependencyCompleted)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var evRepo, evPath, depURL, depState string
		if err := rows.Scan(&evRepo, &evPath, &depURL, &depState); err != nil {
			return false, err
		}
		if depState != "done" && depState != "completed" && depState != "success" {
			continue
		}
		if depURL != "" {
			if matchesRepoAndPath(canonRepo, cleanPath, depURL) {
				return true, nil
			}
		} else if strings.EqualFold(evRepo, canonRepo) && cleanPosixPath(strings.TrimPrefix(evPath, "/")) == cleanPath {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) isTaskCancelledTx(tx *sql.Tx, ctx context.Context, canonRepo, cleanPath string) (bool, error) {
	rows, err := tx.QueryContext(ctx, s.q(`SELECT repo, path, dependency_url, dependency_state FROM grainfile_events WHERE type IN (?, ?)`), EventDependencyCompleted, EventTaskRemoved)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var evRepo, evPath, depURL, depState string
		if err := rows.Scan(&evRepo, &evPath, &depURL, &depState); err != nil {
			return false, err
		}
		if depState != "cancelled" {
			continue
		}
		if depURL != "" {
			if matchesRepoAndPath(canonRepo, cleanPath, depURL) {
				return true, nil
			}
		} else if strings.EqualFold(evRepo, canonRepo) && cleanPosixPath(strings.TrimPrefix(evPath, "/")) == cleanPath {
			return true, nil
		}
	}
	return false, rows.Err()
}

// UpsertIndex is an alias for IndexGrainfile.
func (s *Store) UpsertIndex(ctx context.Context, g *Grainfile) (IndexRecord, error) {
	return s.IndexGrainfile(ctx, g)
}

// RemoveGrainfile marks all indexed versions of (repo, path) as removed so they
// no longer fire triggers or authorize references.
// Per §9:
//   - Deleting an active .task.grain.md while its task is still queued, deferred,
//     or running cancels the task (emitting task_removed with action "removed"
//     and dependency_state "cancelled").
//   - Deleting an active .process.grain.md emits process_removed with action "removed".
//   - Deleting a .task.grain.md after its task has already completed (done) is
//     a clean-up no-op on task completion state.
func (s *Store) RemoveGrainfile(ctx context.Context, repo, filePath string) error {
	canonRepo, err := canonicalRepoFromAnyRef(repo)
	if err != nil {
		return err
	}
	cleanPath := cleanPosixPath(strings.TrimPrefix(filepath.ToSlash(filePath), "/"))
	now := s.now().Truncate(time.Millisecond)
	return s.tx(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, s.q(`SELECT `+selectIndexCols+` FROM grainfile_index WHERE LOWER(repo) = LOWER(?) AND path = ? AND removed = 0 ORDER BY updated_at DESC LIMIT 1`),
			canonRepo, cleanPath)
		latest, scanErr := scanIndexRecord(row)

		if _, err := tx.ExecContext(ctx, s.q(`UPDATE grainfile_index SET removed = 1, updated_at = ? WHERE LOWER(repo) = LOWER(?) AND path = ?`),
			now.UnixMilli(), canonRepo, cleanPath); err != nil {
			return err
		}
		if scanErr != nil || latest.Draft {
			return nil
		}
		if latest.Kind == KindCapability {
			_, err = s.insertEventTx(tx, ctx, Event{
				Type:                 EventCapabilityRemoved,
				Action:               "removed",
				Grainfile:            latest.Grainfile,
				SpecSummaryPath:      latest.SpecSummaryPath,
				SpecPath:             latest.SpecPath,
				Repo:                 latest.Repo,
				Path:                 latest.Path,
				CommitSHA:            latest.CommitSHA,
				Ref:                  latest.Ref,
				GrainfileURL:         latest.URL,
				Kind:                 latest.Kind,
				Title:                latest.Title,
				NotBefore:            latest.NotBefore,
				FromTemplate:         latest.FromTemplate,
				References:           latest.References,
				AuthorizedReferences: latest.AuthorizedReferences,
				IsTemplate:           latest.IsTemplate,
				TemplateVars:         latest.TemplateVars,
				Reason:               "capability grainfile deleted",
				CreatedAt:            now,
			})
			return err
		}
		if latest.Kind == KindProcess {
			_, err = s.insertEventTx(tx, ctx, Event{
				Type:                 EventProcessRemoved,
				Action:               "removed",
				Grainfile:            latest.Grainfile,
				SpecSummaryPath:      latest.SpecSummaryPath,
				SpecPath:             latest.SpecPath,
				Repo:                 latest.Repo,
				Path:                 latest.Path,
				CommitSHA:            latest.CommitSHA,
				Ref:                  latest.Ref,
				GrainfileURL:         latest.URL,
				Kind:                 latest.Kind,
				Title:                latest.Title,
				NotBefore:            latest.NotBefore,
				FromTemplate:         latest.FromTemplate,
				References:           latest.References,
				AuthorizedReferences: latest.AuthorizedReferences,
				DependsOn:            latest.DependsOn,
				Capabilities:         latest.Capabilities,
				When:                 latest.When,
				OnSpec:               latest.OnSpec,
				Gate:                 latest.Gate,
				IsTemplate:           latest.IsTemplate,
				TemplateVars:         latest.TemplateVars,
				Reason:               "process grainfile deleted",
				CreatedAt:            now,
			})
			return err
		}
		if latest.Kind != KindTask {
			return nil
		}
		done, err := s.isTaskCompletedTx(tx, ctx, canonRepo, cleanPath)
		if err != nil {
			return err
		}
		if done {
			// Deleting a completed task is a clean-up no-op on task completion state.
			return nil
		}
		cancelled, err := s.isTaskCancelledTx(tx, ctx, canonRepo, cleanPath)
		if err != nil || cancelled {
			return err
		}
		_, err = s.insertEventTx(tx, ctx, Event{
			Type:                 EventTaskRemoved,
			Action:               "removed",
			Grainfile:            latest.Grainfile,
			SpecSummaryPath:      latest.SpecSummaryPath,
			SpecPath:             latest.SpecPath,
			Repo:                 latest.Repo,
			Path:                 latest.Path,
			CommitSHA:            latest.CommitSHA,
			Ref:                  latest.Ref,
			GrainfileURL:         latest.URL,
			Kind:                 latest.Kind,
			Title:                latest.Title,
			Recipient:            latest.Recipient,
			Requestor:            latest.Requestor,
			NotBefore:            latest.NotBefore,
			FromTemplate:         latest.FromTemplate,
			References:           latest.References,
			AuthorizedReferences: latest.AuthorizedReferences,
			DependsOn:            latest.DependsOn,
			Capabilities:         latest.Capabilities,
			IsTemplate:           latest.IsTemplate,
			TemplateVars:         latest.TemplateVars,
			DependencyURL:        latest.URL,
			DependencyState:      "cancelled",
			Reason:               "task grainfile deleted",
			CreatedAt:            now,
		})
		return err
	})
}

// PromoteDeferred checks indexed non-draft Grainfiles whose not_before
// timestamp was in the future when indexed and has now been reached at `at`,
// emitting their task_ready or process_updated event into grainfile_events.
func (s *Store) PromoteDeferred(ctx context.Context, at time.Time) ([]Event, error) {
	if at.IsZero() {
		at = s.now()
	}
	at = at.UTC()
	var hasDue int
	err := dbaccess.Retry(ctx, func() error {
		hasDue = 0
		return s.db.QueryRowContext(ctx, s.q(`
		SELECT 1 FROM grainfile_index
		WHERE removed = 0 AND draft = 0 AND ready_emitted = 0 AND not_before <= ?
		LIMIT 1
	`), at.UnixMilli()).Scan(&hasDue)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var precheck []IndexRecord
	err = dbaccess.Retry(ctx, func() error {
		var err error
		precheck, err = s.loadLatestIndexQ(s.db, ctx, "")
		return err
	})
	if err != nil {
		return nil, err
	}
	hasWork := false
	for _, rec := range precheck {
		if !rec.removed && !rec.Draft && !rec.readyEmitted && rec.ToGrainfile().ActiveAt(at) {
			if rec.Kind == KindTask && rec.Ref != "" && !isDefaultOrProtectedBranch(rec.Ref, "") {
				continue
			}
			hasWork = true
			break
		}
	}
	if !hasWork {
		return nil, nil
	}
	var emitted []Event
	err = s.tx(ctx, func(tx *sql.Tx) error {
		emitted = nil
		records, err := s.loadLatestIndexQ(tx, ctx, "")
		if err != nil {
			return err
		}
		for _, rec := range records {
			if rec.removed || rec.Draft || rec.readyEmitted {
				continue
			}
			if !rec.ToGrainfile().ActiveAt(at) {
				continue
			}
			if rec.Kind == KindTask {
				if rec.Ref != "" && !isDefaultOrProtectedBranch(rec.Ref, "") {
					continue
				}
				done, err := s.isTaskCompletedTx(tx, ctx, rec.Repo, rec.Path)
				if err != nil {
					return err
				}
				if done {
					if _, err := tx.ExecContext(ctx, s.q(`UPDATE grainfile_index SET ready_emitted = 1, updated_at = ? WHERE repo = ? AND path = ? AND commit_sha = ?`),
						at.UnixMilli(), rec.Repo, rec.Path, rec.CommitSHA); err != nil {
						return err
					}
					continue
				}
			}
			evType := EventTaskReady
			switch rec.Kind {
			case KindProcess:
				evType = EventProcessUpdated
			case KindCapability:
				evType = EventCapabilityUpdated
			}
			ev, err := s.insertEventTx(tx, ctx, Event{
				Type:                 evType,
				Grainfile:            rec.Grainfile,
				SpecSummaryPath:      rec.SpecSummaryPath,
				SpecPath:             rec.SpecPath,
				Repo:                 rec.Repo,
				Path:                 rec.Path,
				CommitSHA:            rec.CommitSHA,
				Ref:                  rec.Ref,
				GrainfileURL:         rec.URL,
				Kind:                 rec.Kind,
				Title:                rec.Title,
				Recipient:            rec.Recipient,
				Requestor:            rec.Requestor,
				NotBefore:            rec.NotBefore,
				FromTemplate:         rec.FromTemplate,
				References:           rec.References,
				AuthorizedReferences: rec.AuthorizedReferences,
				DependsOn:            rec.DependsOn,
				Capabilities:         rec.Capabilities,
				When:                 rec.When,
				OnSpec:               rec.OnSpec,
				Gate:                 rec.Gate,
				IsTemplate:           rec.IsTemplate,
				TemplateVars:         rec.TemplateVars,
				CreatedAt:            at.Truncate(time.Millisecond),
			})
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, s.q(`UPDATE grainfile_index SET ready_emitted = 1, updated_at = ? WHERE repo = ? AND path = ? AND commit_sha = ?`),
				at.UnixMilli(), rec.Repo, rec.Path, rec.CommitSHA); err != nil {
				return err
			}
			emitted = append(emitted, ev)
		}
		return nil
	})
	return emitted, err
}

const selectIndexCols = `repo, path, commit_sha, ref, url, kind, title, recipient, requestor,
	draft, not_before, created_at, from_template, "references", authorized_references,
	depends_on, capabilities, trigger_with, on_spec, gate, is_template, template_vars, ready_emitted, removed, updated_at`

func scanIndexRecord(r interface{ Scan(...any) error }) (IndexRecord, error) {
	var (
		rec                                      IndexRecord
		kindStr                                  string
		draftInt, gateInt                        int
		isTemplateInt                            int
		readyInt, removedInt                     int
		notBeforeMS, createdMS, updatMS          int64
		refsStr, authRefsStr                     string
		depsStr, capsStr, twStr, onStr, tvarsStr string
	)
	if err := r.Scan(
		&rec.Repo, &rec.Path, &rec.CommitSHA, &rec.Ref, &rec.URL, &kindStr,
		&rec.Title, &rec.Recipient, &rec.Requestor,
		&draftInt, &notBeforeMS, &createdMS, &rec.FromTemplate, &refsStr, &authRefsStr,
		&depsStr, &capsStr, &twStr, &onStr, &gateInt, &isTemplateInt, &tvarsStr,
		&readyInt, &removedInt, &updatMS,
	); err != nil {
		return IndexRecord{}, err
	}
	rec.Grainfile = VersionV1
	rec.SpecSummaryPath = SpecSummaryPathV1
	rec.SpecPath = SpecPathV1
	rec.Kind = Kind(kindStr)
	rec.Draft = draftInt != 0
	rec.Gate = gateInt != 0
	rec.IsTemplate = isTemplateInt != 0 || IsTemplatePath(rec.Path)
	rec.readyEmitted = readyInt != 0
	rec.removed = removedInt != 0
	if notBeforeMS > 0 {
		rec.NotBefore = time.UnixMilli(notBeforeMS).UTC()
	}
	if createdMS > 0 {
		rec.CreatedAt = time.UnixMilli(createdMS).UTC()
	}
	rec.UpdatedAt = time.UnixMilli(updatMS).UTC()
	_ = json.Unmarshal([]byte(refsStr), &rec.References)
	_ = json.Unmarshal([]byte(authRefsStr), &rec.AuthorizedReferences)
	_ = json.Unmarshal([]byte(depsStr), &rec.DependsOn)
	_ = json.Unmarshal([]byte(capsStr), &rec.Capabilities)
	_ = json.Unmarshal([]byte(onStr), &rec.OnSpec)
	_ = json.Unmarshal([]byte(tvarsStr), &rec.TemplateVars)
	rec.When = rec.OnSpec
	rec.On = rec.OnSpec
	return rec, nil
}

// GetIndex returns the indexed Grainfile metadata for (repo, path, commitSHA).
// When commitSHA is empty, it returns the most recently updated non-removed
// record for (repo, path). When commitSHA is non-empty, it requires the latest
// non-removed record for (repo, path) to match commitSHA so superseded commits
// cannot bypass draft transitions or reference removals.
func (s *Store) GetIndex(ctx context.Context, repo, filePath, commitSHA string) (IndexRecord, error) {
	canonRepo, err := canonicalRepoFromAnyRef(repo)
	if err != nil {
		return IndexRecord{}, err
	}
	cleanPath := cleanPosixPath(strings.TrimPrefix(filepath.ToSlash(filePath), "/"))
	var rec IndexRecord
	err = dbaccess.Retry(ctx, func() error {
		row := s.db.QueryRowContext(ctx, s.q(`SELECT `+selectIndexCols+` FROM grainfile_index WHERE LOWER(repo) = LOWER(?) AND path = ? AND removed = 0 ORDER BY updated_at DESC, commit_sha DESC LIMIT 1`),
			canonRepo, cleanPath)
		var scanErr error
		rec, scanErr = scanIndexRecord(row)
		return scanErr
	})
	if errors.Is(err, sql.ErrNoRows) {
		return IndexRecord{}, ErrNotFound
	}
	if err != nil {
		return IndexRecord{}, err
	}
	if commitSHA != "" && !refMatchesIndexRecord(commitSHA, rec) {
		return IndexRecord{}, ErrNotFound
	}
	return rec, nil
}

func refMatchesIndexRecord(refOrSHA string, rec IndexRecord) bool {
	refOrSHA = strings.TrimSpace(refOrSHA)
	if refOrSHA == "" {
		return true
	}
	if rec.CommitSHA != "" && refOrSHA == rec.CommitSHA {
		return true
	}
	normReq := normalizeURLRef(refOrSHA, "")
	normRec := normalizeURLRef(rec.Ref, "")
	return normReq != "" && normReq == normRec
}

// GetIndexByURL looks up an indexed Grainfile by its canonical URL (or by the
// (repo, path) parsed from rawURL) and optional commitSHA, verifying against
// the latest non-removed record for (repo, path).
func (s *Store) GetIndexByURL(ctx context.Context, rawURL, commitSHA string) (IndexRecord, error) {
	parsed, err := ParseRef(rawURL)
	if err == nil && parsed.Form == RefRemote {
		if parsed.Repo != "" && parsed.Path != "" {
			rec, getErr := s.GetIndex(ctx, parsed.Repo, parsed.Path, "")
			if getErr != nil {
				return IndexRecord{}, getErr
			}
			if !refMatchesIndexRecord(parsed.GitRef, rec) || !refMatchesIndexRecord(commitSHA, rec) {
				return IndexRecord{}, ErrNotFound
			}
			return rec, nil
		}
		canonURL := parsed.CanonicalURL()
		var rec IndexRecord
		scanErr := dbaccess.Retry(ctx, func() error {
			row := s.db.QueryRowContext(ctx, s.q(`SELECT `+selectIndexCols+` FROM grainfile_index WHERE url = ? AND removed = 0 ORDER BY updated_at DESC, commit_sha DESC LIMIT 1`),
				canonURL)
			var err error
			rec, err = scanIndexRecord(row)
			return err
		})
		if scanErr == nil {
			if refMatchesIndexRecord(parsed.GitRef, rec) && refMatchesIndexRecord(commitSHA, rec) {
				return rec, nil
			}
		}
	}
	return IndexRecord{}, ErrNotFound
}

// ListIndexByRepo returns the latest non-removed IndexRecord per (repo, path)
// in repoURL (or across all repositories when repoURL is empty).
func (s *Store) ListIndexByRepo(ctx context.Context, repoURL string) ([]IndexRecord, error) {
	var canonRepo string
	if repoURL != "" {
		var err error
		canonRepo, err = canonicalRepoFromAnyRef(repoURL)
		if err != nil {
			return nil, err
		}
	}
	var out []IndexRecord
	err := dbaccess.Retry(ctx, func() error {
		var err error
		out, err = s.loadLatestIndexQ(s.db, ctx, canonRepo)
		return err
	})
	return out, err
}

// ListIndexByRecipient returns the latest non-removed IndexRecord per
// (repo, path) whose resolved recipient matches recipientURL. When
// includeDrafts is false, draft: true records are excluded.
func (s *Store) ListIndexByRecipient(ctx context.Context, recipientURL string, includeDrafts bool) ([]IndexRecord, error) {
	normRec, err := normalizeRecipientURL(recipientURL)
	if err != nil {
		return nil, err
	}
	all, err := s.ListIndexByRepo(ctx, "")
	if err != nil {
		return nil, err
	}
	var out []IndexRecord
	for _, r := range all {
		if r.removed || !sameRecipient(r.Recipient, normRec) {
			continue
		}
		if !includeDrafts && r.Draft {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

type rowQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (s *Store) loadLatestIndexQ(q rowQueryer, ctx context.Context, canonRepo string) ([]IndexRecord, error) {
	query := `SELECT ` + selectIndexCols + ` FROM grainfile_index`
	var args []any
	if canonRepo != "" {
		query += ` WHERE LOWER(repo) = LOWER(?)`
		args = append(args, canonRepo)
	}
	query += ` ORDER BY updated_at DESC, repo ASC, path ASC, commit_sha DESC`

	rows, err := q.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type key struct{ repo, path string }
	seen := map[key]bool{}
	var out []IndexRecord
	for rows.Next() {
		rec, err := scanIndexRecord(rows)
		if err != nil {
			return nil, err
		}
		k := key{rec.Repo, rec.Path}
		if seen[k] {
			continue
		}
		seen[k] = true
		if !rec.removed {
			out = append(out, rec)
		}
	}
	return out, rows.Err()
}

func (s *Store) sameTeamForCrossRepo(ctx context.Context, repoA, repoB string) bool {
	if IsSameRepo(repoA, repoB) {
		return true
	}
	if s.SameRepoTeam != nil {
		return s.SameRepoTeam(ctx, repoA, repoB)
	}
	if s.TeamResolver != nil {
		slugA := RepoSlug(repoA)
		if slugA == "" {
			slugA = strings.TrimSpace(repoA)
		}
		slugB := RepoSlug(repoB)
		if slugB == "" {
			slugB = strings.TrimSpace(repoB)
		}
		teamA, errA := s.TeamResolver.RepoOwner(ctx, slugA)
		if errA != nil || teamA == "" {
			return false
		}
		teamB, errB := s.TeamResolver.RepoOwner(ctx, slugB)
		if errB != nil || teamB == "" {
			return false
		}
		return strings.EqualFold(teamA, teamB)
	}
	return true
}

// EvaluateForgeEvent is an alias for EvaluateTriggers that evaluates when:
// triggers (push, pull_request, merge, workflow) against ev.
func (s *Store) EvaluateForgeEvent(ctx context.Context, repoURL, commitSHA string, ev ForgeEvent) ([]Event, error) {
	return s.EvaluateTriggers(ctx, repoURL, commitSHA, ev)
}

// EvaluateTriggers evaluates when: triggers (push, pull_request, merge, workflow) for all
// active non-draft .process.grain.md files targeting repoURL (local or same-team
// cross-repo) against ev, records process_triggered events, and transitively
// evaluates depends_on co-triggers across all active indexed Grainfiles.
func (s *Store) EvaluateTriggers(ctx context.Context, repoURL, commitSHA string, ev ForgeEvent) ([]Event, error) {
	canonRepo, err := canonicalRepoFromAnyRef(repoURL)
	if err != nil {
		return nil, err
	}
	if ev.Repo == "" {
		ev.Repo = canonRepo
	} else if r, rErr := canonicalRepoFromAnyRef(ev.Repo); rErr == nil {
		ev.Repo = r
	}
	if ev.CommitSHA == "" {
		ev.CommitSHA = commitSHA
	} else if commitSHA == "" {
		commitSHA = ev.CommitSHA
	}
	at := ev.At
	if at.IsZero() {
		at = s.now()
	}
	at = at.UTC()
	ev.At = at

	changedTaskSet := map[string]bool{}
	if ev.Source == TriggerPush || ev.Source == TriggerMerge {
		for _, p := range ev.ChangedPaths {
			if clean := normalizeRepoRelPath(p); clean != "" {
				changedTaskSet[clean] = true
			}
		}
	}

	precheckRecords, err := s.loadLatestIndexQ(s.db, ctx, "")
	if err != nil {
		return nil, err
	}
	hasMatch := false
	for _, rec := range precheckRecords {
		if rec.removed || rec.Draft {
			continue
		}
		gf := rec.ToGrainfile()
		if rec.Kind == KindProcess {
			if (IsSameRepo(rec.Repo, canonRepo) || s.sameTeamForCrossRepo(ctx, rec.Repo, canonRepo)) && gf.MatchesEvent(ev) {
				hasMatch = true
				break
			}
		} else if rec.Kind == KindTask && IsSameRepo(rec.Repo, canonRepo) && changedTaskSet[rec.Path] && gf.ActiveAt(at) {
			hasMatch = true
			break
		}
	}
	if !hasMatch {
		return nil, nil
	}

	var triggeredEvents []Event
	err = s.tx(ctx, func(tx *sql.Tx) error {
		triggeredEvents = nil
		allRecords, err := s.loadLatestIndexQ(tx, ctx, "")
		if err != nil {
			return err
		}
		slices.SortFunc(allRecords, func(a, b IndexRecord) int {
			if c := strings.Compare(a.Repo, b.Repo); c != 0 {
				return c
			}
			return strings.Compare(a.Path, b.Path)
		})

		type trigItem struct {
			rec          IndexRecord
			source       string
			triggeredBy  string
			matchedPaths []string
			emit         bool
			depth        int
		}
		var queue []trigItem
		var discovered []trigItem
		visited := map[string]bool{}
		coTriggeredCount := 0

		for _, rec := range allRecords {
			if rec.removed || rec.Draft {
				continue
			}
			gf := rec.ToGrainfile()
			if rec.Kind == KindProcess && (IsSameRepo(rec.Repo, canonRepo) || s.sameTeamForCrossRepo(ctx, rec.Repo, canonRepo)) {
				if ok, paths := gf.MatchedEventPaths(ev); ok {
					key := rec.Repo + "\x00" + rec.Path
					visited[key] = true
					item := trigItem{
						rec:          rec,
						source:       ev.Source,
						matchedPaths: paths,
						emit:         true,
						depth:        0,
					}
					queue = append(queue, item)
					discovered = append(discovered, item)
				}
			} else if rec.Kind == KindTask && IsSameRepo(rec.Repo, canonRepo) && changedTaskSet[rec.Path] && gf.ActiveAt(at) {
				key := rec.Repo + "\x00" + rec.Path
				visited[key] = true
				queue = append(queue, trigItem{
					rec:    rec,
					source: ev.Source,
					emit:   false,
					depth:  0,
				})
			}
		}

		for len(queue) > 0 {
			curr := queue[0]
			queue = queue[1:]

			// Enforce bounded depends_on co-trigger depth (MaxDependsOnDepth = 8) and
			// fan-out (MaxDependsOnFanOut = 32) per §8.3 and §11.4.
			if curr.depth >= MaxDependsOnDepth || coTriggeredCount >= MaxDependsOnFanOut {
				continue
			}

			// Evaluate depends_on co-triggers referencing curr.rec when a dependent
			// .process.grain.md has manual/empty triggers or resides in another repository
			// whose event matches ev.
			for _, candidate := range allRecords {
				if coTriggeredCount >= MaxDependsOnFanOut {
					break
				}
				if candidate.removed || candidate.Draft || candidate.Kind != KindProcess || len(candidate.DependsOn) == 0 {
					continue
				}
				candGF := candidate.ToGrainfile()
				if !candGF.ActiveAt(at) {
					continue
				}
				if candidate.Gate && !IsSameRepo(candidate.Repo, canonRepo) && !s.sameTeamForCrossRepo(ctx, candidate.Repo, canonRepo) {
					continue
				}
				canCoTrigger := candidate.When.IsZero() || candidate.When.Manual != nil || (candidate.Repo != canonRepo && s.sameTeamForCrossRepo(ctx, candidate.Repo, canonRepo) && candGF.MatchesEvent(ev))
				if !canCoTrigger {
					continue
				}
				candKey := candidate.Repo + "\x00" + candidate.Path
				if visited[candKey] {
					continue
				}
				for _, depURL := range candidate.DependsOn {
					if matchesGrainfileRef(curr.rec, depURL) {
						visited[candKey] = true
						coTriggeredCount++
						item := trigItem{
							rec:          candidate,
							source:       TriggerDependsOn,
							triggeredBy:  curr.rec.URL,
							matchedPaths: slices.Clone(ev.ChangedPaths),
							emit:         true,
							depth:        curr.depth + 1,
						}
						queue = append(queue, item)
						discovered = append(discovered, item)
						break
					}
				}
			}
		}

		if len(discovered) == 0 {
			return nil
		}

		// Topologically sort discovered items so any item listed in another
		// item's depends_on fires first.
		n := len(discovered)
		inDegree := make([]int, n)
		edges := make([][]int, n)
		for i := 0; i < n; i++ {
			for j := 0; j < n; j++ {
				if i == j {
					continue
				}
				// Does j depend on i?
				depends := false
				for _, depURL := range discovered[j].rec.DependsOn {
					if matchesGrainfileRef(discovered[i].rec, depURL) {
						depends = true
						break
					}
				}
				if depends {
					edges[i] = append(edges[i], j)
					inDegree[j]++
				}
			}
		}

		ordered := make([]trigItem, 0, n)
		used := make([]bool, n)
		for len(ordered) < n {
			picked := -1
			for i := 0; i < n; i++ {
				if !used[i] && inDegree[i] == 0 {
					picked = i
					break
				}
			}
			if picked < 0 {
				// Fallback if a cycle exists: pick the first unused item in discovery order.
				for i := 0; i < n; i++ {
					if !used[i] {
						picked = i
						break
					}
				}
			}
			used[picked] = true
			ordered = append(ordered, discovered[picked])
			for _, next := range edges[picked] {
				inDegree[next]--
			}
		}

		branchName := strings.TrimPrefix(strings.TrimSpace(ev.Branch), "refs/heads/")
		for _, curr := range ordered {
			cSHA := commitSHA
			if curr.source == TriggerDependsOn {
				cSHA = curr.rec.CommitSHA
			} else if IsSameRepo(curr.rec.Repo, canonRepo) && ev.Source == TriggerPullRequest && curr.rec.CommitSHA != "" {
				cSHA = curr.rec.CommitSHA
			} else if cSHA == "" {
				cSHA = curr.rec.CommitSHA
			}
			ref := ev.Branch
			if curr.source == TriggerDependsOn || ref == "" || (IsSameRepo(curr.rec.Repo, canonRepo) && ev.Source == TriggerPullRequest) {
				ref = curr.rec.Ref
			}
			evRepo := curr.rec.Repo
			if curr.source != TriggerDependsOn && canonRepo != "" {
				evRepo = canonRepo
			}
			touchedPaths := curr.matchedPaths
			if len(touchedPaths) == 0 && len(ev.ChangedPaths) > 0 {
				touchedPaths = slices.Clone(ev.ChangedPaths)
			}

			recorded, err := s.insertEventTx(tx, ctx, Event{
				Type:                 EventProcessTriggered,
				Grainfile:            curr.rec.Grainfile,
				SpecSummaryPath:      curr.rec.SpecSummaryPath,
				SpecPath:             curr.rec.SpecPath,
				Repo:                 evRepo,
				Path:                 curr.rec.Path,
				CommitSHA:            cSHA,
				Ref:                  ref,
				Branch:               branchName,
				PRNumber:             ev.PRNumber,
				GrainfileURL:         curr.rec.URL,
				Kind:                 curr.rec.Kind,
				Title:                curr.rec.Title,
				Recipient:            curr.rec.Recipient,
				Requestor:            curr.rec.Requestor,
				NotBefore:            curr.rec.NotBefore,
				FromTemplate:         curr.rec.FromTemplate,
				References:           curr.rec.References,
				AuthorizedReferences: curr.rec.AuthorizedReferences,
				DependsOn:            curr.rec.DependsOn,
				Capabilities:         curr.rec.Capabilities,
				When:                 curr.rec.When,
				OnSpec:               curr.rec.OnSpec,
				Gate:                 curr.rec.Gate,
				IsTemplate:           curr.rec.IsTemplate,
				TemplateVars:         curr.rec.TemplateVars,
				TriggerSource:        curr.source,
				TriggeredBy:          curr.triggeredBy,
				TriggerRepo:          canonRepo,
				TriggerCommitSHA:     commitSHA,
				WorkflowName:         ev.WorkflowName,
				WorkflowPath:         ev.WorkflowPath,
				WorkflowEvent:        ev.WorkflowEvent,
				WorkflowURL:          ev.WorkflowURL,
				Paths:                touchedPaths,
				ChangedPaths:         slices.Clone(ev.ChangedPaths),
				CreatedAt:            at.Truncate(time.Millisecond),
			})
			if err != nil {
				return err
			}
			triggeredEvents = append(triggeredEvents, recorded)
		}
		return nil
	})
	return triggeredEvents, err
}

// RecordDependencyCompleted records dependency_completed events when a
// Grainfile run finishes (with state such as "done", "failed", or "cancelled"),
// notifying the recipient deployments of any active Grainfile whose depends_on
// references completedURL as well as subscribers watching completedURL.
func (s *Store) RecordDependencyCompleted(ctx context.Context, completedURL, commitSHA, state, reason string) ([]Event, error) {
	if completedURL == "" {
		return nil, fmt.Errorf("%w: empty completed grainfile URL", ErrInvalid)
	}
	state = strings.TrimSpace(state)
	if state == "" {
		state = "done"
	}
	switch state {
	case "done", "completed", "success", "failed", "cancelled":
	default:
		return nil, fmt.Errorf("%w: invalid completion state %q", ErrInvalid, state)
	}
	if len(reason) > 1024 {
		return nil, fmt.Errorf("%w: completion reason exceeds 1024 bytes", ErrInvalid)
	}
	now := s.now().Truncate(time.Millisecond)

	var recorded []Event
	err := s.tx(ctx, func(tx *sql.Tx) error {
		recorded = nil
		allRecords, err := s.loadLatestIndexQ(tx, ctx, "")
		if err != nil {
			return err
		}

		var completedRec *IndexRecord
		for i := range allRecords {
			if matchesGrainfileRef(allRecords[i], completedURL) {
				completedRec = &allRecords[i]
				break
			}
		}
		if completedRec == nil {
			return fmt.Errorf("%w: completed grainfile %q is not indexed", ErrNotFound, completedURL)
		}
		if completedRec.Draft {
			return fmt.Errorf("%w: draft grainfile %q cannot be completed", ErrInvalid, completedURL)
		}

		canonCompleted := completedURL
		if completedRec != nil {
			canonCompleted = completedRec.URL
			if commitSHA == "" {
				commitSHA = completedRec.CommitSHA
			}
		} else if r, err := ParseRef(completedURL); err == nil {
			canonCompleted = r.CanonicalURL()
		}

		// Emit a dependency_completed event for every active dependent Grainfile
		// whose depends_on lists completedURL.
		for _, dep := range allRecords {
			if dep.removed || dep.Draft || len(dep.DependsOn) == 0 {
				continue
			}
			matched := false
			for _, dURL := range dep.DependsOn {
				if completedRec != nil && matchesGrainfileRef(*completedRec, dURL) {
					matched = true
					break
				}
				if sameURLOrRepoPath(canonCompleted, dURL) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			ev, err := s.insertEventTx(tx, ctx, Event{
				Type:                 EventDependencyCompleted,
				Grainfile:            dep.Grainfile,
				SpecSummaryPath:      dep.SpecSummaryPath,
				SpecPath:             dep.SpecPath,
				Repo:                 dep.Repo,
				Path:                 dep.Path,
				CommitSHA:            dep.CommitSHA,
				Ref:                  dep.Ref,
				GrainfileURL:         dep.URL,
				Kind:                 dep.Kind,
				Title:                dep.Title,
				Recipient:            dep.Recipient,
				Requestor:            dep.Requestor,
				NotBefore:            dep.NotBefore,
				References:           dep.References,
				AuthorizedReferences: dep.AuthorizedReferences,
				DependsOn:            dep.DependsOn,
				Capabilities:         dep.Capabilities,
				When:                 dep.When,
				OnSpec:               dep.OnSpec,
				Gate:                 dep.Gate,
				IsTemplate:           dep.IsTemplate,
				TemplateVars:         dep.TemplateVars,
				DependencyURL:        canonCompleted,
				DependencyState:      state,
				Reason:               reason,
				CreatedAt:            now,
			})
			if err != nil {
				return err
			}
			recorded = append(recorded, ev)
		}

		// If no dependent was indexed yet, still record the completion event on
		// the completed Grainfile itself so direct subscribers to completedURL
		// and future DependenciesSatisfied checks see it.
		if len(recorded) == 0 {
			ev := Event{
				Type:            EventDependencyCompleted,
				Grainfile:       VersionV1,
				SpecSummaryPath: SpecSummaryPathV1,
				SpecPath:        SpecPathV1,
				GrainfileURL:    canonCompleted,
				CommitSHA:       commitSHA,
				DependencyURL:   canonCompleted,
				DependencyState: state,
				Reason:          reason,
				CreatedAt:       now,
			}
			if completedRec != nil {
				ev.Grainfile = completedRec.Grainfile
				ev.SpecSummaryPath = completedRec.SpecSummaryPath
				ev.SpecPath = completedRec.SpecPath
				ev.Repo = completedRec.Repo
				ev.Path = completedRec.Path
				ev.Ref = completedRec.Ref
				ev.Kind = completedRec.Kind
				ev.Title = completedRec.Title
				ev.Recipient = completedRec.Recipient
			} else if r, err := ParseRef(canonCompleted); err == nil && r.Repo != "" {
				ev.Repo = r.Repo
				ev.Path = r.Path
				ev.Ref = r.GitRef
			}
			ins, err := s.insertEventTx(tx, ctx, ev)
			if err != nil {
				return err
			}
			recorded = append(recorded, ins)
		}
		return nil
	})
	return recorded, err
}

// RecordDependencyCompletedByNode verifies that nodeID is a registered
// subscriber authorized for completedURL's recipient deployment (including
// directory ownership admission when AuthorizeOwnerRepo is configured) before
// recording dependency_completed.
func (s *Store) RecordDependencyCompletedByNode(ctx context.Context, nodeID, completedURL, commitSHA, state, reason string) ([]Event, error) {
	sub, err := s.GetSubscriptions(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	rec, err := s.GetIndexByURL(ctx, completedURL, commitSHA)
	if errors.Is(err, ErrNotFound) && commitSHA != "" {
		rec, err = s.GetIndexByURL(ctx, completedURL, "")
	}
	if err != nil {
		return nil, err
	}
	authorized := false
	if rec.Recipient != "" {
		if depID := DeploymentIDFromRecipient(rec.Recipient); depID != "" && strings.EqualFold(depID, sub.NodeID) {
			authorized = true
		}
		for _, r := range sub.Recipients {
			if sameRecipient(r, rec.Recipient) {
				if depID := DeploymentIDFromRecipient(rec.Recipient); depID != "" && strings.EqualFold(depID, sub.NodeID) {
					authorized = true
					break
				}
				if s.AuthorizeOwnerRepo != nil && sub.Owner != "" {
					recRepo, rErr := canonicalRepoFromAnyRef(rec.Recipient)
					if rErr != nil || !s.AuthorizeOwnerRepo(ctx, sub.Owner, recRepo) {
						return nil, fmt.Errorf("%w: owner %q is not admitted for recipient repository %q", ErrUnauthorized, sub.Owner, rec.Recipient)
					}
				}
				authorized = true
				break
			}
		}
	} else if sub.MatchesRecord(rec) {
		matchedByGF := false
		for _, gf := range sub.Grainfiles {
			if matchesGrainfileRef(rec, gf) {
				matchedByGF = true
				break
			}
		}
		if !matchedByGF && !isTeamHostedRepoForNode(rec.Repo, sub.NodeID) && s.AuthorizeOwnerRepo != nil && sub.Owner != "" && !s.AuthorizeOwnerRepo(ctx, sub.Owner, rec.Repo) {
			return nil, fmt.Errorf("%w: owner %q is not admitted for repository %q", ErrUnauthorized, sub.Owner, rec.Repo)
		}
		authorized = true
	}
	if !authorized {
		return nil, fmt.Errorf("%w: node %q is not recipient of %q", ErrUnauthorized, nodeID, completedURL)
	}
	return s.RecordDependencyCompleted(ctx, completedURL, commitSHA, state, reason)
}

// DependenciesSatisfied reports whether every target in grainfileURL's
// depends_on has a recorded dependency_completed event with state "done".
func (s *Store) DependenciesSatisfied(ctx context.Context, grainfileURL string) (bool, error) {
	rec, err := s.GetIndexByURL(ctx, grainfileURL, "")
	if err != nil {
		return false, err
	}
	if len(rec.DependsOn) == 0 {
		return true, nil
	}
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT grainfile_url, dependency_url, dependency_state FROM grainfile_events WHERE type = ? ORDER BY seq ASC`), EventDependencyCompleted)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	type compEntry struct {
		gfURL  string
		depURL string
		state  string
	}
	var completed []compEntry
	for rows.Next() {
		var e compEntry
		if err := rows.Scan(&e.gfURL, &e.depURL, &e.state); err != nil {
			return false, err
		}
		completed = append(completed, e)
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, depURL := range rec.DependsOn {
		done := false
		for _, ev := range completed {
			if ev.state != "done" && ev.state != "completed" && ev.state != "success" {
				continue
			}
			if sameURLOrRepoPath(ev.depURL, depURL) || sameURLOrRepoPath(ev.gfURL, depURL) {
				done = true
				break
			}
		}
		if !done {
			return false, nil
		}
	}
	return true, nil
}

func (s *Store) insertEventTx(tx *sql.Tx, ctx context.Context, ev Event) (Event, error) {
	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM grainfile_events`).Scan(&seq); err != nil {
		return Event{}, err
	}
	ev.Seq = seq
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = s.now().Truncate(time.Millisecond)
	}
	if ev.Grainfile == "" {
		ev.Grainfile = VersionV1
	}
	if spec, ok := LookupVersion(ev.Grainfile); ok {
		if ev.SpecSummaryPath == "" {
			ev.SpecSummaryPath = spec.SpecSummaryPath
		}
		if ev.SpecPath == "" {
			ev.SpecPath = spec.SpecPath
		}
	}
	if ev.OnSpec.IsZero() && !ev.When.IsZero() {
		ev.OnSpec = ev.When
	}
	ev.When = ev.OnSpec
	ev.On = ev.OnSpec

	if ev.Action == "" && (ev.Type == EventTaskRemoved || ev.Type == EventProcessRemoved || ev.Type == EventCapabilityRemoved) {
		ev.Action = "removed"
	}

	var nbMS int64
	if !ev.NotBefore.IsZero() {
		nbMS = ev.NotBefore.UnixMilli()
	}
	pl := eventPayload{
		Action:               ev.Action,
		Grainfile:            ev.Grainfile,
		Requestor:            ev.Requestor,
		NotBeforeMillis:      nbMS,
		FromTemplate:         ev.FromTemplate,
		References:           ev.References,
		AuthorizedReferences: ev.AuthorizedReferences,
		DependsOn:            ev.DependsOn,
		Capabilities:         ev.Capabilities,
		OnSpec:               ev.OnSpec,
		IsTemplate:           ev.IsTemplate,
		TemplateVars:         ev.TemplateVars,
		Branch:               ev.Branch,
		PRNumber:             ev.PRNumber,
		Paths:                ev.Paths,
		ChangedPaths:         ev.ChangedPaths,
		TriggerRepo:          ev.TriggerRepo,
		TriggerCommitSHA:     ev.TriggerCommitSHA,
		WorkflowName:         ev.WorkflowName,
		WorkflowPath:         ev.WorkflowPath,
		WorkflowEvent:        ev.WorkflowEvent,
		WorkflowURL:          ev.WorkflowURL,
		Reason:               ev.Reason,
	}
	plBytes, err := json.Marshal(pl)
	if err != nil {
		return Event{}, err
	}

	_, err = tx.ExecContext(ctx, s.q(`INSERT INTO grainfile_events (
		seq, type, repo, path, commit_sha, ref, grainfile_url, kind, title,
		recipient, trigger_source, triggered_by, dependency_url, dependency_state,
		gate, payload, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		ev.Seq, ev.Type, ev.Repo, ev.Path, ev.CommitSHA, ev.Ref,
		ev.GrainfileURL, string(ev.Kind), ev.Title, ev.Recipient,
		ev.TriggerSource, ev.TriggeredBy, ev.DependencyURL, ev.DependencyState,
		boolToInt(ev.Gate), string(plBytes), ev.CreatedAt.UnixMilli())
	if err != nil {
		return Event{}, err
	}
	return ev, nil
}

const selectEventCols = `seq, type, repo, path, commit_sha, ref, grainfile_url, kind, title,
	recipient, trigger_source, triggered_by, dependency_url, dependency_state,
	gate, payload, created_at`

func scanEvent(r interface{ Scan(...any) error }) (Event, error) {
	var (
		ev        Event
		kindStr   string
		gateInt   int
		payloadJS string
		createdMS int64
	)
	if err := r.Scan(
		&ev.Seq, &ev.Type, &ev.Repo, &ev.Path, &ev.CommitSHA, &ev.Ref,
		&ev.GrainfileURL, &kindStr, &ev.Title, &ev.Recipient,
		&ev.TriggerSource, &ev.TriggeredBy, &ev.DependencyURL, &ev.DependencyState,
		&gateInt, &payloadJS, &createdMS,
	); err != nil {
		return Event{}, err
	}
	ev.Kind = Kind(kindStr)
	ev.Gate = gateInt != 0
	ev.CreatedAt = time.UnixMilli(createdMS).UTC()

	var pl eventPayload
	if err := json.Unmarshal([]byte(payloadJS), &pl); err == nil {
		ev.Action = pl.Action
		ev.Grainfile = pl.Grainfile
		ev.Requestor = pl.Requestor
		if pl.NotBeforeMillis > 0 {
			ev.NotBefore = time.UnixMilli(pl.NotBeforeMillis).UTC()
		}
		ev.FromTemplate = pl.FromTemplate
		ev.References = pl.References
		ev.AuthorizedReferences = pl.AuthorizedReferences
		ev.DependsOn = pl.DependsOn
		ev.Capabilities = pl.Capabilities
		ev.OnSpec = pl.OnSpec
		ev.When = pl.OnSpec
		ev.On = pl.OnSpec
		ev.IsTemplate = pl.IsTemplate || IsTemplatePath(ev.Path)
		ev.TemplateVars = pl.TemplateVars
		ev.Branch = pl.Branch
		ev.PRNumber = pl.PRNumber
		ev.Paths = pl.Paths
		ev.ChangedPaths = pl.ChangedPaths
		ev.TriggerRepo = pl.TriggerRepo
		ev.TriggerCommitSHA = pl.TriggerCommitSHA
		ev.WorkflowName = pl.WorkflowName
		ev.WorkflowPath = pl.WorkflowPath
		ev.WorkflowEvent = pl.WorkflowEvent
		ev.WorkflowURL = pl.WorkflowURL
		ev.Reason = pl.Reason
	}
	if ev.Action == "" && (ev.Type == EventTaskRemoved || ev.Type == EventProcessRemoved || ev.Type == EventCapabilityRemoved) {
		ev.Action = "removed"
	}
	if ev.Grainfile == "" {
		ev.Grainfile = VersionV1
	}
	if spec, ok := LookupVersion(ev.Grainfile); ok {
		ev.SpecSummaryPath = spec.SpecSummaryPath
		ev.SpecPath = spec.SpecPath
	}
	return ev, nil
}

// MaxEventSeq returns the highest event sequence number recorded in
// grainfile_events, or 0 when none exist.
func (s *Store) MaxEventSeq(ctx context.Context) (int64, error) {
	var seq int64
	err := dbaccess.Retry(ctx, func() error {
		seq = 0
		return s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM grainfile_events`).Scan(&seq)
	})
	return seq, err
}

// EventsSince returns all discoverable Grainfile events with seq > sinceSeq,
// promoting any deferred not_before Grainfiles whose timestamp has been reached
// at s.Now() first.
func (s *Store) EventsSince(ctx context.Context, sinceSeq int64) ([]Event, error) {
	if _, err := s.PromoteDeferred(ctx, s.now()); err != nil {
		return nil, err
	}
	var out []Event
	err := dbaccess.Retry(ctx, func() error {
		out = nil
		rows, err := s.db.QueryContext(ctx, s.q(`SELECT `+selectEventCols+` FROM grainfile_events WHERE seq > ? ORDER BY seq ASC`), sinceSeq)
		if err != nil {
			return err
		}
		defer rows.Close()
		now := s.now()
		for rows.Next() {
			ev, err := scanEvent(rows)
			if err != nil {
				return err
			}
			if !ev.NotBefore.IsZero() && now.Before(ev.NotBefore) {
				continue
			}
			out = append(out, ev)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// EventsForRecipient returns all discoverable Grainfile events for recipientURL
// with seq > sinceSeq, promoting any deferred not_before Grainfiles whose
// timestamp has been reached at s.Now() first.
func (s *Store) EventsForRecipient(ctx context.Context, recipientURL string, sinceSeq int64) ([]Event, error) {
	normRec, err := normalizeRecipientURL(recipientURL)
	if err != nil {
		return nil, err
	}
	all, err := s.EventsSince(ctx, sinceSeq)
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, ev := range all {
		if sameRecipient(ev.Recipient, normRec) {
			out = append(out, ev)
		}
	}
	return out, nil
}

// SetSubscriptions stores or updates a node's Grainfile subscriptions in
// grainfile_subscriptions, normalizing all recipient, repository, folder, and
// Grainfile URLs to canonical form.
func (s *Store) SetSubscriptions(ctx context.Context, sub Subscriptions) (Subscriptions, error) {
	nodeID := strings.TrimSpace(sub.NodeID)
	if nodeID == "" {
		return Subscriptions{}, fmt.Errorf("%w: empty node_id", ErrInvalid)
	}

	rawRecipients := slices.Clone(sub.Recipients)
	if s.HubURL != "" {
		if canonDeploy := CanonicalDeploymentURL(s.HubURL, nodeID); canonDeploy != "" && (strings.HasPrefix(canonDeploy, "https://") || isLoopbackDeploymentHTTPURL(canonDeploy)) {
			rawRecipients = append(rawRecipients, canonDeploy)
		}
	}
	recipients, err := normalizeSubscriptionList(rawRecipients, normalizeRecipientURL)
	if err != nil {
		return Subscriptions{}, err
	}
	repos, err := normalizeSubscriptionList(sub.Repos, normalizeSubRepoURL)
	if err != nil {
		return Subscriptions{}, err
	}
	folders, err := normalizeSubscriptionList(sub.Folders, normalizeRecipientURL)
	if err != nil {
		return Subscriptions{}, err
	}
	gfs, err := normalizeSubscriptionList(sub.Grainfiles, normalizeGrainfileURL)
	if err != nil {
		return Subscriptions{}, err
	}

	now := s.now().Truncate(time.Millisecond)
	recJSON, _ := json.Marshal(recipients)
	repoJSON, _ := json.Marshal(repos)
	foldJSON, _ := json.Marshal(folders)
	gfJSON, _ := json.Marshal(gfs)

	out := Subscriptions{
		NodeID:     nodeID,
		Owner:      strings.TrimSpace(sub.Owner),
		Recipients: recipients,
		Repos:      repos,
		Folders:    folders,
		Grainfiles: gfs,
		Cursor:     sub.Cursor,
		Revision:   1,
		UpdatedAt:  now,
	}

	sameTeamOwners := map[string]bool{}
	if len(out.Recipients) > 0 && out.Owner != "" && s.SameTeam != nil {
		var otherOwners []string
		err := dbaccess.Retry(ctx, func() error {
			otherOwners = nil
			rows, err := s.db.QueryContext(ctx, s.q(`SELECT DISTINCT owner FROM grainfile_subscriptions WHERE node_id <> ? AND owner <> ''`), nodeID)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var oOwner string
				if err := rows.Scan(&oOwner); err != nil {
					return err
				}
				if !strings.EqualFold(oOwner, out.Owner) {
					otherOwners = append(otherOwners, oOwner)
				}
			}
			return rows.Err()
		})
		if err != nil {
			return Subscriptions{}, err
		}
		for _, oOwner := range otherOwners {
			if s.SameTeam(ctx, out.Owner, oOwner) {
				sameTeamOwners[strings.ToLower(oOwner)] = true
			}
		}
	}

	err = s.tx(ctx, func(tx *sql.Tx) error {
		out.Owner = strings.TrimSpace(sub.Owner)
		out.Cursor = sub.Cursor
		out.Revision = 1
		var prevCursor, prevRev int64
		var prevOwner string
		err := tx.QueryRowContext(ctx, s.q(`SELECT owner, cursor, revision FROM grainfile_subscriptions WHERE node_id = ?`), nodeID).
			Scan(&prevOwner, &prevCursor, &prevRev)
		if err == nil {
			if prevOwner != "" && out.Owner != "" && !strings.EqualFold(prevOwner, out.Owner) {
				sameTeam, cached := sameTeamOwners[strings.ToLower(prevOwner)]
				if !cached && s.SameTeam != nil {
					sameTeam = s.SameTeam(ctx, out.Owner, prevOwner)
					sameTeamOwners[strings.ToLower(prevOwner)] = sameTeam
				}
				if !sameTeam {
					return fmt.Errorf("%w: node %q is already registered to another owner", ErrUnauthorized, nodeID)
				}
			}
			out.Revision = prevRev + 1
			if !sub.ResetCursor && out.Cursor == 0 && prevCursor > 0 {
				out.Cursor = prevCursor
			}
			if out.Owner == "" {
				out.Owner = prevOwner
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		idxRows, idxErr := tx.QueryContext(ctx, s.q(`SELECT DISTINCT recipient FROM grainfile_index WHERE removed = 0 AND recipient <> ''`))
		if idxErr == nil {
			for idxRows.Next() {
				var idxRec string
				if err := idxRows.Scan(&idxRec); err == nil && strings.HasPrefix(idxRec, "https://") {
					if depID := DeploymentIDFromRecipient(idxRec); depID != "" && strings.EqualFold(depID, nodeID) && !slices.Contains(out.Recipients, idxRec) {
						out.Recipients = append(out.Recipients, idxRec)
					}
				}
			}
			idxRows.Close()
			recJSON, _ = json.Marshal(out.Recipients)
		}

		if s.AuthorizeOwnerRepo != nil && out.Owner != "" {
			for _, u := range out.Recipients {
				if depID := DeploymentIDFromRecipient(u); depID != "" {
					if !strings.EqualFold(depID, nodeID) {
						return fmt.Errorf("%w: node %q cannot subscribe to deployment URL %q", ErrUnauthorized, nodeID, u)
					}
					continue
				}
				repoURL, rErr := canonicalRepoFromAnyRef(u)
				if rErr != nil || !s.AuthorizeOwnerRepo(ctx, out.Owner, repoURL) {
					return fmt.Errorf("%w: owner %q is not admitted for recipient repository %q", ErrUnauthorized, out.Owner, u)
				}
			}
			for _, u := range out.Repos {
				if isTeamHostedRepoForNode(u, nodeID) {
					continue
				}
				if !s.AuthorizeOwnerRepo(ctx, out.Owner, u) {
					return fmt.Errorf("%w: owner %q is not admitted for repository %q", ErrUnauthorized, out.Owner, u)
				}
			}
			for _, u := range out.Folders {
				repoURL, rErr := canonicalRepoFromAnyRef(u)
				if rErr != nil || !s.AuthorizeOwnerRepo(ctx, out.Owner, repoURL) {
					return fmt.Errorf("%w: owner %q is not admitted for folder repository %q", ErrUnauthorized, out.Owner, u)
				}
			}
		}

		if len(out.Recipients) > 0 && out.Owner != "" {
			rows, err := tx.QueryContext(ctx, s.q(`SELECT owner, recipients FROM grainfile_subscriptions WHERE node_id <> ?`), nodeID)
			if err != nil {
				return err
			}
			type existingClaim struct {
				owner      string
				recipients []string
			}
			var claims []existingClaim
			for rows.Next() {
				var oOwner, oRecJSON string
				if err := rows.Scan(&oOwner, &oRecJSON); err != nil {
					rows.Close()
					return err
				}
				if oOwner == "" || strings.EqualFold(oOwner, out.Owner) {
					continue
				}
				var oRecs []string
				_ = json.Unmarshal([]byte(oRecJSON), &oRecs)
				if len(oRecs) > 0 {
					claims = append(claims, existingClaim{owner: oOwner, recipients: oRecs})
				}
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()

			for _, cl := range claims {
				for _, wanted := range out.Recipients {
					for _, claimed := range cl.recipients {
						if strings.EqualFold(wanted, claimed) || IsSameRepo(wanted, claimed) {
							sameTeam, cached := sameTeamOwners[strings.ToLower(cl.owner)]
							if !cached && s.SameTeam != nil {
								sameTeam = s.SameTeam(ctx, out.Owner, cl.owner)
								sameTeamOwners[strings.ToLower(cl.owner)] = sameTeam
							}
							if !sameTeam {
								return fmt.Errorf("%w: recipient %q is already claimed by another owner", ErrUnauthorized, wanted)
							}
						}
					}
				}
			}
		}

		_, err = tx.ExecContext(ctx, s.q(`INSERT INTO grainfile_subscriptions (
			node_id, owner, recipients, repos, folders, grainfiles, cursor, revision, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (node_id) DO UPDATE SET
			owner = EXCLUDED.owner,
			recipients = EXCLUDED.recipients,
			repos = EXCLUDED.repos,
			folders = EXCLUDED.folders,
			grainfiles = EXCLUDED.grainfiles,
			cursor = EXCLUDED.cursor,
			revision = EXCLUDED.revision,
			updated_at = EXCLUDED.updated_at`),
			out.NodeID, out.Owner, string(recJSON), string(repoJSON),
			string(foldJSON), string(gfJSON), out.Cursor, out.Revision,
			out.UpdatedAt.UnixMilli())
		return err
	})
	if err != nil {
		return Subscriptions{}, err
	}
	return out, nil
}

// Subscribe is a convenience wrapper around SetSubscriptions for nodeID.
func (s *Store) Subscribe(ctx context.Context, nodeID string, sub Subscriptions) (Subscriptions, error) {
	sub.NodeID = nodeID
	return s.SetSubscriptions(ctx, sub)
}

// GetSubscriptions returns the stored Subscriptions for nodeID.
func (s *Store) GetSubscriptions(ctx context.Context, nodeID string) (Subscriptions, error) {
	var sub Subscriptions
	err := dbaccess.Retry(ctx, func() error {
		row := s.db.QueryRowContext(ctx, s.q(`SELECT node_id, owner, recipients, repos, folders, grainfiles, cursor, revision, updated_at
			FROM grainfile_subscriptions WHERE node_id = ?`), nodeID)
		var err error
		sub, err = scanSubscriptions(row)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return Subscriptions{}, ErrNotFound
	}
	return sub, err
}

// ListSubscriptions returns all registered node subscriptions ordered by node_id.
func (s *Store) ListSubscriptions(ctx context.Context) ([]Subscriptions, error) {
	var out []Subscriptions
	err := dbaccess.Retry(ctx, func() error {
		out = nil
		rows, err := s.db.QueryContext(ctx, `SELECT node_id, owner, recipients, repos, folders, grainfiles, cursor, revision, updated_at
			FROM grainfile_subscriptions ORDER BY node_id ASC`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			sub, err := scanSubscriptions(rows)
			if err != nil {
				return err
			}
			out = append(out, sub)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AdvanceCursor monotonically advances nodeID's delivery cursor in
// grainfile_subscriptions.
func (s *Store) AdvanceCursor(ctx context.Context, nodeID string, cursor int64) error {
	if cursor < 0 {
		return fmt.Errorf("%w: negative cursor %d", ErrInvalid, cursor)
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var cur int64
		err := tx.QueryRowContext(ctx, s.q(`SELECT cursor FROM grainfile_subscriptions WHERE node_id = ?`), nodeID).Scan(&cur)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if cursor > cur {
			cur = cursor
		}
		_, err = tx.ExecContext(ctx, s.q(`UPDATE grainfile_subscriptions SET cursor = ?, updated_at = ? WHERE node_id = ?`),
			cur, s.now().UnixMilli(), nodeID)
		return err
	})
}

// DeleteSubscriptions deletes nodeID's subscription record.
func (s *Store) DeleteSubscriptions(ctx context.Context, nodeID string) error {
	return dbaccess.Retry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, s.q(`DELETE FROM grainfile_subscriptions WHERE node_id = ?`), nodeID)
		return err
	})
}

// EventsForNode returns all discoverable events with seq > sinceSeq that match
// nodeID's subscriptions. If sinceSeq < 0, nodeID's stored subscription cursor
// is used.
func (s *Store) EventsForNode(ctx context.Context, nodeID string, sinceSeq int64) ([]Event, error) {
	sub, err := s.GetSubscriptions(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	if sinceSeq < 0 {
		sinceSeq = sub.Cursor
	}
	all, err := s.EventsSince(ctx, sinceSeq)
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, ev := range all {
		if sub.MatchesEvent(ev) {
			out = append(out, ev)
		}
	}
	return out, nil
}

// CatchupNode returns all missed discoverable events since nodeID's stored
// cursor (promoting any newly ready not_before Grainfiles first) and the new
// high-water cursor. When advance is true, it also persists the new cursor in
// grainfile_subscriptions.
func (s *Store) CatchupNode(ctx context.Context, nodeID string, advance bool) ([]Event, int64, error) {
	sub, err := s.GetSubscriptions(ctx, nodeID)
	if err != nil {
		return nil, 0, err
	}
	events, err := s.EventsForNode(ctx, nodeID, sub.Cursor)
	if err != nil {
		return nil, 0, err
	}
	newCursor := sub.Cursor
	for _, ev := range events {
		if ev.Seq > newCursor {
			newCursor = ev.Seq
		}
	}
	if advance && newCursor > sub.Cursor {
		if err := s.AdvanceCursor(ctx, nodeID, newCursor); err != nil {
			return nil, 0, err
		}
	}
	return events, newCursor, nil
}

// ReplayForNode resets nodeID's subscription delivery cursor to the current
// high-water mark before replay (and clears any prior owner binding so a
// replacement or reinstalled host can reconnect) and emits fresh replay events
// for all active .process.grain.md (process_updated) and active uncompleted
// .task.grain.md (task_ready) Grainfiles targeted at nodeID.
func (s *Store) ReplayForNode(ctx context.Context, nodeID string) ([]Event, error) {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return nil, fmt.Errorf("%w: empty node_id", ErrInvalid)
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	var replayed []Event
	err := s.tx(ctx, func(tx *sql.Tx) error {
		replayed = nil
		var maxSeq int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM grainfile_events`).Scan(&maxSeq); err != nil {
			return err
		}
		var sub Subscriptions
		row := tx.QueryRowContext(ctx, s.q(`SELECT node_id, owner, recipients, repos, folders, grainfiles, cursor, revision, updated_at
			FROM grainfile_subscriptions WHERE node_id = ?`), nodeID)
		loaded, err := scanSubscriptions(row)
		if errors.Is(err, sql.ErrNoRows) {
			var recs []string
			if s.HubURL != "" {
				if u := CanonicalDeploymentURL(s.HubURL, nodeID); u != "" && (strings.HasPrefix(u, "https://") || isLoopbackDeploymentHTTPURL(u)) {
					recs = append(recs, u)
				}
			}
			recJSON, _ := json.Marshal(recs)
			_, err = tx.ExecContext(ctx, s.q(`INSERT INTO grainfile_subscriptions (
				node_id, owner, recipients, repos, folders, grainfiles, cursor, revision, updated_at
			) VALUES (?, '', ?, '[]', '[]', '[]', ?, 1, ?)`),
				nodeID, string(recJSON), maxSeq, now.UnixMilli())
			if err != nil {
				return err
			}
			sub = Subscriptions{
				NodeID:     nodeID,
				Recipients: recs,
				Cursor:     maxSeq,
				Revision:   1,
				UpdatedAt:  now,
			}
		} else if err != nil {
			return err
		} else {
			sub = loaded
			if s.HubURL != "" {
				if u := CanonicalDeploymentURL(s.HubURL, nodeID); u != "" && (strings.HasPrefix(u, "https://") || isLoopbackDeploymentHTTPURL(u)) && !slices.Contains(sub.Recipients, u) {
					sub.Recipients = append(sub.Recipients, u)
				}
			}
			recJSON, _ := json.Marshal(sub.Recipients)
			sub.Cursor = maxSeq
			sub.Owner = ""
			sub.Revision = loaded.Revision + 1
			sub.UpdatedAt = now
			_, err = tx.ExecContext(ctx, s.q(`UPDATE grainfile_subscriptions SET owner = '', recipients = ?, cursor = ?, revision = ?, updated_at = ? WHERE node_id = ?`),
				string(recJSON), maxSeq, sub.Revision, now.UnixMilli(), nodeID)
			if err != nil {
				return err
			}
		}

		allRecords, err := s.loadLatestIndexQ(tx, ctx, "")
		if err != nil {
			return err
		}

		for _, rec := range allRecords {
			if rec.removed || rec.Draft || !sub.MatchesRecord(rec) {
				continue
			}
			gf := rec.ToGrainfile()
			if !gf.ActiveAt(now) {
				continue
			}
			if rec.Kind == KindTask {
				done, dErr := s.isTaskCompletedTx(tx, ctx, rec.Repo, rec.Path)
				if dErr != nil {
					return dErr
				}
				if done {
					continue
				}
				cancelled, cErr := s.isTaskCancelledTx(tx, ctx, rec.Repo, rec.Path)
				if cErr != nil {
					return cErr
				}
				if cancelled {
					continue
				}
			}
			wantType := EventTaskReady
			switch rec.Kind {
			case KindProcess:
				wantType = EventProcessUpdated
			case KindCapability:
				wantType = EventCapabilityUpdated
			}
			ins, err := s.insertEventTx(tx, ctx, Event{
				Type:                 wantType,
				Grainfile:            rec.Grainfile,
				SpecSummaryPath:      rec.SpecSummaryPath,
				SpecPath:             rec.SpecPath,
				Repo:                 rec.Repo,
				Path:                 rec.Path,
				CommitSHA:            rec.CommitSHA,
				Ref:                  rec.Ref,
				GrainfileURL:         rec.URL,
				Kind:                 rec.Kind,
				Title:                rec.Title,
				Recipient:            rec.Recipient,
				Requestor:            rec.Requestor,
				FromTemplate:         rec.FromTemplate,
				NotBefore:            rec.NotBefore,
				References:           rec.References,
				AuthorizedReferences: rec.AuthorizedReferences,
				DependsOn:            rec.DependsOn,
				Capabilities:         rec.Capabilities,
				When:                 rec.When,
				OnSpec:               rec.OnSpec,
				Gate:                 rec.Gate,
				IsTemplate:           rec.IsTemplate,
				TemplateVars:         rec.TemplateVars,
				CreatedAt:            now,
			})
			if err != nil {
				return err
			}
			replayed = append(replayed, ins)
		}
		return nil
	})
	return replayed, err
}

// EffectiveRecipients returns sub's configured Recipients plus the canonical
// Hub deployment URL ("https://<hub-host>/deployments/<node_id>") when NodeID
// is set.
func (sub Subscriptions) EffectiveRecipients(hubURL string) []string {
	out := slices.Clone(sub.Recipients)
	if sub.NodeID != "" {
		if u := CanonicalDeploymentURL(hubURL, sub.NodeID); u != "" && !slices.Contains(out, u) {
			out = append(out, u)
		}
	}
	return out
}

// MatchesRecord reports whether sub subscribes to rec by recipient URL
// (including canonical Hub deployment URLs /deployments/<node_id>),
// repository URL (including Hub-hosted /team/<node_id> repos), folder prefix,
// or explicit Grainfile URL.
func (sub Subscriptions) MatchesRecord(rec IndexRecord) bool {
	if rec.Recipient != "" {
		if slices.Contains(sub.Recipients, rec.Recipient) {
			return true
		}
		if depID := DeploymentIDFromRecipient(rec.Recipient); depID != "" && strings.EqualFold(depID, sub.NodeID) {
			return true
		}
		for _, r := range sub.Recipients {
			if sameRecipient(r, rec.Recipient) {
				return true
			}
		}
	}
	if sub.NodeID != "" && isTeamHostedRepoForNode(rec.Repo, sub.NodeID) {
		return true
	}
	for _, r := range sub.Repos {
		if IsSameRepo(r, rec.Repo) || isSameTeamHostedRepo(r, rec.Repo) {
			return true
		}
	}
	for _, f := range sub.Folders {
		if folderContainsRecord(f, rec.Repo, rec.Path) {
			return true
		}
	}
	for _, gf := range sub.Grainfiles {
		if matchesGrainfileRef(rec, gf) {
			return true
		}
	}
	return false
}

// MatchesEvent reports whether sub subscribes to ev.
func (sub Subscriptions) MatchesEvent(ev Event) bool {
	rec := IndexRecord{
		Repo:      ev.Repo,
		Path:      ev.Path,
		CommitSHA: ev.CommitSHA,
		Ref:       ev.Ref,
		URL:       ev.GrainfileURL,
		Recipient: ev.Recipient,
	}
	if sub.MatchesRecord(rec) {
		return true
	}
	if ev.GrainfileURL != "" {
		if parsed, err := ParseRef(ev.GrainfileURL); err == nil && parsed.Repo != "" && !IsSameRepo(parsed.Repo, ev.Repo) {
			gfRec := IndexRecord{
				Repo:      parsed.Repo,
				Path:      parsed.Path,
				CommitSHA: ev.CommitSHA,
				Ref:       parsed.GitRef,
				URL:       ev.GrainfileURL,
				Recipient: ev.Recipient,
			}
			if sub.MatchesRecord(gfRec) {
				return true
			}
		}
	}
	if ev.DependencyURL != "" {
		for _, gf := range sub.Grainfiles {
			if sameURLOrRepoPath(ev.DependencyURL, gf) {
				return true
			}
		}
	}
	if ev.TriggeredBy != "" {
		for _, gf := range sub.Grainfiles {
			if sameURLOrRepoPath(ev.TriggeredBy, gf) {
				return true
			}
		}
	}
	return false
}

func scanSubscriptions(r interface{ Scan(...any) error }) (Subscriptions, error) {
	var (
		sub                         Subscriptions
		recJS, repoJS, foldJS, gfJS string
		updatedMS                   int64
	)
	if err := r.Scan(
		&sub.NodeID, &sub.Owner, &recJS, &repoJS, &foldJS, &gfJS,
		&sub.Cursor, &sub.Revision, &updatedMS,
	); err != nil {
		return Subscriptions{}, err
	}
	sub.UpdatedAt = time.UnixMilli(updatedMS).UTC()
	_ = json.Unmarshal([]byte(recJS), &sub.Recipients)
	_ = json.Unmarshal([]byte(repoJS), &sub.Repos)
	_ = json.Unmarshal([]byte(foldJS), &sub.Folders)
	_ = json.Unmarshal([]byte(gfJS), &sub.Grainfiles)
	return sub, nil
}

func normalizeSubscriptionList(items []string, normFn func(string) (string, error)) ([]string, error) {
	if len(items) == 0 {
		return []string{}, nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for _, raw := range items {
		u, err := normFn(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out, nil
}

func isTeamHostedRepoForNode(repoURL, nodeID string) bool {
	if repoURL == "" || nodeID == "" {
		return false
	}
	team := teamFromHostedRepo(repoURL)
	return team != "" && strings.EqualFold(team, strings.TrimSpace(nodeID))
}

func isSameTeamHostedRepo(a, b string) bool {
	ta := teamFromHostedRepo(a)
	tb := teamFromHostedRepo(b)
	return ta != "" && strings.EqualFold(ta, tb)
}

func teamFromHostedRepo(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var p string
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		p = u.Path
	} else {
		p = raw
	}
	p = strings.Trim(p, "/")
	if rest, ok := strings.CutPrefix(p, "v1/grainfiles/team/"); ok && rest != "" && !strings.Contains(rest, "/") {
		return strings.ToLower(rest)
	}
	if rest, ok := strings.CutPrefix(p, "team/"); ok && rest != "" && !strings.Contains(rest, "/") {
		return strings.ToLower(rest)
	}
	return ""
}

func normalizeSubRepoURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("empty repository URL")
	}
	if team := teamFromHostedRepo(raw); team != "" {
		if !strings.Contains(raw, "://") {
			raw = "https://hub.local/team/" + team
		} else if u, err := url.Parse(raw); err == nil {
			u.Path = "/team/" + team
			raw = u.String()
		}
	} else if !strings.Contains(raw, "://") && strings.Count(raw, "/") == 1 {
		raw = "https://github.com/" + raw
	}
	return CanonicalRepoURL(raw)
}

func sameRecipient(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if strings.EqualFold(a, b) || IsSameRepo(a, b) {
		return true
	}
	idA := DeploymentIDFromRecipient(a)
	idB := DeploymentIDFromRecipient(b)
	return idA != "" && strings.EqualFold(idA, idB)
}

func normalizeRecipientURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("empty recipient URL")
	}
	if !strings.Contains(raw, "://") {
		if depID := DeploymentIDFromRecipient(raw); depID != "" {
			return "/deployments/" + depID, nil
		}
		if strings.Count(raw, "/") == 1 {
			raw = "https://github.com/" + raw
		}
	}
	r, err := ParseRef(raw)
	if err != nil {
		return "", err
	}
	if r.Form != RefRemote {
		return "", fmt.Errorf("recipient subscription %q must be a remote URL", raw)
	}
	if r.Anchor != "" || r.Mode == "blob" {
		return "", fmt.Errorf("recipient subscription %q must be a repository or folder URL", raw)
	}
	return r.CanonicalURL(), nil
}

func normalizeGrainfileURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	r, err := ParseRef(raw)
	if err != nil {
		return "", err
	}
	if r.Form != RefRemote {
		return "", fmt.Errorf("grainfile subscription %q must be a remote URL", raw)
	}
	return r.CanonicalURL(), nil
}

func folderContainsRecord(folderURL, repoURL, relPath string) bool {
	r, err := ParseRef(folderURL)
	if err != nil || r.Form != RefRemote {
		return false
	}
	if !IsSameRepo(r.Repo, repoURL) {
		return false
	}
	folderPath := cleanPosixPath(strings.Trim(r.Path, "/"))
	if folderPath == "" || folderPath == "." {
		return true
	}
	cleanRel := cleanPosixPath(strings.Trim(relPath, "/"))
	return cleanRel == folderPath || strings.HasPrefix(cleanRel, folderPath+"/")
}

func matchesGrainfileRef(rec IndexRecord, targetURL string) bool {
	if rec.URL == targetURL {
		return true
	}
	return sameURLOrRepoPath(rec.URL, targetURL) || (rec.Repo != "" && rec.Path != "" && matchesRepoAndPath(rec.Repo, rec.Path, targetURL))
}

func sameURLOrRepoPath(urlA, urlB string) bool {
	if urlA == "" || urlB == "" {
		return false
	}
	if urlA == urlB {
		return true
	}
	ra, errA := ParseRef(urlA)
	rb, errB := ParseRef(urlB)
	if errA != nil || errB != nil || ra.Form != RefRemote || rb.Form != RefRemote {
		return false
	}
	if ra.CanonicalURL() == rb.CanonicalURL() {
		return true
	}
	pa := cleanPosixPath(strings.Trim(ra.Path, "/"))
	pb := cleanPosixPath(strings.Trim(rb.Path, "/"))
	return ra.Repo != "" && ra.Repo == rb.Repo && pa != "" && pa == pb
}

func matchesRepoAndPath(repoURL, relPath, targetURL string) bool {
	r, err := ParseRef(targetURL)
	if err != nil || r.Form != RefRemote {
		return false
	}
	if !IsSameRepo(repoURL, r.Repo) {
		return false
	}
	p1 := cleanPosixPath(strings.Trim(relPath, "/"))
	p2 := cleanPosixPath(strings.Trim(r.Path, "/"))
	return p1 != "" && p1 == p2
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func isDefaultOrProtectedBranch(ref, defaultBranch string) bool {
	branch := normalizeBranchName(ref)
	if branch == "" {
		return true
	}
	def := normalizeBranchName(defaultBranch)
	if def != "" {
		return strings.EqualFold(branch, def) || isProtectedBranchPattern(branch)
	}
	return strings.EqualFold(branch, "main") ||
		strings.EqualFold(branch, "master") ||
		strings.EqualFold(branch, "trunk") ||
		isProtectedBranchPattern(branch)
}

func isProtectedBranchPattern(branch string) bool {
	low := strings.ToLower(branch)
	return strings.HasPrefix(low, "release/") ||
		strings.HasPrefix(low, "releases/") ||
		strings.HasPrefix(low, "candidate/") ||
		strings.HasPrefix(low, "hotfix/") ||
		low == "production" ||
		low == "prod"
}
