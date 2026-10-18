package grainfiles

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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
	EventProcessUpdated      = "process_updated"
	EventProcessTriggered    = "process_triggered"
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

// Store persists indexed Grainfile front-matter metadata, node subscriptions,
// and discoverable Grainfile events in PostgreSQL or SQLite. It stores
// metadata only—never source files or Grainfile Markdown bodies at rest
// (docs/hub.md §1.1, §6.3).
type Store struct {
	db       *sql.DB
	postgres bool
	// Now, when set, is the clock used to stamp rows and evaluate not_before.
	Now func() time.Time
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
	draft INTEGER NOT NULL DEFAULT 0,
	not_before BIGINT NOT NULL DEFAULT 0,
	created_at BIGINT NOT NULL DEFAULT 0,
	"references" TEXT NOT NULL DEFAULT '[]',
	authorized_references TEXT NOT NULL DEFAULT '[]',
	depends_on TEXT NOT NULL DEFAULT '[]',
	trigger_with TEXT NOT NULL DEFAULT '[]',
	on_spec TEXT NOT NULL DEFAULT '{}',
	gate INTEGER NOT NULL DEFAULT 0,
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

func (s *Store) migrate(ctx context.Context) error {
	for _, stmt := range strings.Split(storeSchema, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("grainfiles: creating tables: %w", err)
		}
	}
	return nil
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
	return dbaccess.InTx(ctx, s.db, f)
}

// IndexRecord is a row in grainfile_index storing parsed and resolved Grainfile
// front-matter metadata per (repo, path, commit_sha). It never holds the
// Grainfile Markdown prompt body or source code.
type IndexRecord struct {
	Repo                 string    `json:"repo"`
	Path                 string    `json:"path"`
	CommitSHA            string    `json:"commit_sha"`
	Ref                  string    `json:"ref,omitempty"`
	URL                  string    `json:"url"`
	Kind                 Kind      `json:"kind"`
	Title                string    `json:"title"`
	Recipient            string    `json:"recipient"`
	Requestor            string    `json:"requestor,omitempty"`
	Draft                bool      `json:"draft"`
	NotBefore            time.Time `json:"not_before,omitempty,omitzero"`
	CreatedAt            time.Time `json:"created_at,omitempty,omitzero"`
	References           []string  `json:"references,omitempty"`
	AuthorizedReferences []string  `json:"authorized_references,omitempty"`
	DependsOn            []string  `json:"depends_on,omitempty"`
	TriggerWith          []string  `json:"trigger_with,omitempty"`
	On                   Triggers  `json:"on,omitempty,omitzero"`
	OnSpec               Triggers  `json:"on_spec,omitempty,omitzero"`
	Gate                 bool      `json:"gate,omitempty"`
	UpdatedAt            time.Time `json:"updated_at"`

	readyEmitted bool
	removed      bool
}

// ToGrainfile reconstructs a metadata-only *Grainfile (with an empty Body) from
// an IndexRecord so trigger and reference-authorization methods can be invoked.
func (r IndexRecord) ToGrainfile() *Grainfile {
	return &Grainfile{
		Kind:        r.Kind,
		Path:        r.Path,
		URL:         r.URL,
		Repo:        r.Repo,
		Ref:         r.Ref,
		Commit:      r.CommitSHA,
		Title:       r.Title,
		Recipient:   r.Recipient,
		Requestor:   r.Requestor,
		Draft:       r.Draft,
		NotBefore:   r.NotBefore,
		CreatedAt:   r.CreatedAt,
		References:  slices.Clone(r.References),
		DependsOn:   slices.Clone(r.DependsOn),
		TriggerWith: slices.Clone(r.TriggerWith),
		On:          r.OnSpec,
		Gate:        r.Gate,
	}
}

// Subscriptions records what recipient URL(s), repository URLs, folder URLs, or
// specific .task.md / .process.md URLs a registered node subscribes to, along
// with its monotonic delivery cursor and subscription revision.
type Subscriptions struct {
	NodeID     string    `json:"node_id"`
	Owner      string    `json:"owner,omitempty"`
	Recipients []string  `json:"recipients,omitempty"`
	Repos      []string  `json:"repos,omitempty"`
	Folders    []string  `json:"folders,omitempty"`
	Grainfiles []string  `json:"grainfiles,omitempty"`
	Cursor     int64     `json:"cursor"`
	Revision   int64     `json:"revision"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Event is a discoverable Grainfile event stored in grainfile_events
// (task_ready, process_updated, process_triggered, dependency_completed).
type Event struct {
	Seq                  int64     `json:"seq"`
	Type                 string    `json:"type"`
	Repo                 string    `json:"repo"`
	Path                 string    `json:"path,omitempty"`
	CommitSHA            string    `json:"commit_sha,omitempty"`
	Ref                  string    `json:"ref,omitempty"`
	GrainfileURL         string    `json:"grainfile_url"`
	Kind                 Kind      `json:"kind,omitempty"`
	Title                string    `json:"title,omitempty"`
	Recipient            string    `json:"recipient,omitempty"`
	Requestor            string    `json:"requestor,omitempty"`
	NotBefore            time.Time `json:"not_before,omitempty,omitzero"`
	References           []string  `json:"references,omitempty"`
	AuthorizedReferences []string  `json:"authorized_references,omitempty"`
	DependsOn            []string  `json:"depends_on,omitempty"`
	TriggerWith          []string  `json:"trigger_with,omitempty"`
	On                   Triggers  `json:"on,omitempty,omitzero"`
	OnSpec               Triggers  `json:"on_spec,omitempty,omitzero"`
	Gate                 bool      `json:"gate,omitempty"`
	TriggerSource        string    `json:"trigger_source,omitempty"`
	TriggeredBy          string    `json:"triggered_by,omitempty"`
	ChangedPaths         []string  `json:"changed_paths,omitempty"`
	DependencyURL        string    `json:"dependency_url,omitempty"`
	DependencyState      string    `json:"dependency_state,omitempty"`
	Reason               string    `json:"reason,omitempty"`
	CreatedAt            time.Time `json:"created_at,omitzero"`
}

type eventPayload struct {
	Requestor            string   `json:"requestor,omitempty"`
	NotBeforeMillis      int64    `json:"not_before_ms,omitempty"`
	References           []string `json:"references,omitempty"`
	AuthorizedReferences []string `json:"authorized_references,omitempty"`
	DependsOn            []string `json:"depends_on,omitempty"`
	TriggerWith          []string `json:"trigger_with,omitempty"`
	OnSpec               Triggers `json:"on_spec,omitempty"`
	ChangedPaths         []string `json:"changed_paths,omitempty"`
	Reason               string   `json:"reason,omitempty"`
}

// IndexGrainfile stores or updates the resolved metadata of g in
// grainfile_index (never storing g.Body). If g is not a draft and its
// not_before timestamp (if any) has been reached at s.Now(), IndexGrainfile
// also records the corresponding discoverable event (task_ready for .task.md,
// process_updated for .process.md) in grainfile_events.
func (s *Store) IndexGrainfile(ctx context.Context, g *Grainfile) (IndexRecord, error) {
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
	if cp.URL == "" || !strings.Contains(cp.Recipient, "://") {
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
	tw := cp.TriggerWith
	if tw == nil {
		tw = []string{}
	}

	now := s.now().Truncate(time.Millisecond)
	activeNow := cp.ActiveAt(now)

	rec := IndexRecord{
		Repo:                 cp.Repo,
		Path:                 cp.Path,
		CommitSHA:            commitSHA,
		Ref:                  cp.Ref,
		URL:                  cp.URL,
		Kind:                 cp.Kind,
		Title:                cp.Title,
		Recipient:            cp.Recipient,
		Requestor:            cp.Requestor,
		Draft:                cp.Draft,
		NotBefore:            cp.NotBefore.UTC(),
		CreatedAt:            cp.CreatedAt.UTC(),
		References:           refs,
		AuthorizedReferences: authRefs,
		DependsOn:            deps,
		TriggerWith:          tw,
		On:                   cp.On,
		OnSpec:               cp.On,
		Gate:                 cp.Gate,
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
	twJSON, err := json.Marshal(rec.TriggerWith)
	if err != nil {
		return IndexRecord{}, err
	}
	onJSON, err := json.Marshal(rec.OnSpec)
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
		var prevReady int
		err := tx.QueryRowContext(ctx, s.q(`SELECT ready_emitted FROM grainfile_index WHERE repo = ? AND path = ? AND commit_sha = ?`),
			rec.Repo, rec.Path, rec.CommitSHA).Scan(&prevReady)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		emitNow := activeNow && prevReady == 0
		readyFlag := 0
		if activeNow || prevReady != 0 {
			if !rec.Draft && activeNow {
				readyFlag = 1
			}
		}

		_, err = tx.ExecContext(ctx, s.q(`INSERT INTO grainfile_index (
			repo, path, commit_sha, ref, url, kind, title, recipient, requestor,
			draft, not_before, created_at, "references", authorized_references,
			depends_on, trigger_with, on_spec, gate, ready_emitted, removed, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)
		ON CONFLICT (repo, path, commit_sha) DO UPDATE SET
			ref = EXCLUDED.ref,
			url = EXCLUDED.url,
			kind = EXCLUDED.kind,
			title = EXCLUDED.title,
			recipient = EXCLUDED.recipient,
			requestor = EXCLUDED.requestor,
			draft = EXCLUDED.draft,
			not_before = EXCLUDED.not_before,
			created_at = EXCLUDED.created_at,
			"references" = EXCLUDED."references",
			authorized_references = EXCLUDED.authorized_references,
			depends_on = EXCLUDED.depends_on,
			trigger_with = EXCLUDED.trigger_with,
			on_spec = EXCLUDED.on_spec,
			gate = EXCLUDED.gate,
			ready_emitted = EXCLUDED.ready_emitted,
			removed = 0,
			updated_at = EXCLUDED.updated_at`),
			rec.Repo, rec.Path, rec.CommitSHA, rec.Ref, rec.URL, string(rec.Kind),
			rec.Title, rec.Recipient, rec.Requestor, boolToInt(rec.Draft),
			notBeforeMS, createdAtMS, string(refsJSON), string(authRefsJSON),
			string(depsJSON), string(twJSON), string(onJSON), boolToInt(rec.Gate),
			readyFlag, rec.UpdatedAt.UnixMilli())
		if err != nil {
			return err
		}

		if emitNow {
			evType := EventTaskReady
			if rec.Kind == KindProcess {
				evType = EventProcessUpdated
			}
			_, err = s.insertEventTx(tx, ctx, Event{
				Type:                 evType,
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
				References:           rec.References,
				AuthorizedReferences: rec.AuthorizedReferences,
				DependsOn:            rec.DependsOn,
				TriggerWith:          rec.TriggerWith,
				OnSpec:               rec.OnSpec,
				Gate:                 rec.Gate,
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

// UpsertIndex is an alias for IndexGrainfile.
func (s *Store) UpsertIndex(ctx context.Context, g *Grainfile) (IndexRecord, error) {
	return s.IndexGrainfile(ctx, g)
}

// RemoveGrainfile marks all indexed versions of (repo, path) as removed so they
// no longer fire triggers or authorize references.
func (s *Store) RemoveGrainfile(ctx context.Context, repo, filePath string) error {
	canonRepo, err := canonicalRepoFromAnyRef(repo)
	if err != nil {
		return err
	}
	cleanPath := cleanPosixPath(strings.TrimPrefix(filepath.ToSlash(filePath), "/"))
	now := s.now().UnixMilli()
	_, err = s.db.ExecContext(ctx, s.q(`UPDATE grainfile_index SET removed = 1, updated_at = ? WHERE repo = ? AND path = ?`),
		now, canonRepo, cleanPath)
	return err
}

// PromoteDeferred checks indexed non-draft Grainfiles whose not_before
// timestamp was in the future when indexed and has now been reached at `at`,
// emitting their task_ready or process_updated event into grainfile_events.
func (s *Store) PromoteDeferred(ctx context.Context, at time.Time) ([]Event, error) {
	if at.IsZero() {
		at = s.now()
	}
	at = at.UTC()
	precheck, err := s.loadLatestIndexQ(s.db, ctx, "")
	if err != nil {
		return nil, err
	}
	hasWork := false
	for _, rec := range precheck {
		if !rec.removed && !rec.Draft && !rec.readyEmitted && rec.ToGrainfile().ActiveAt(at) {
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
			evType := EventTaskReady
			if rec.Kind == KindProcess {
				evType = EventProcessUpdated
			}
			ev, err := s.insertEventTx(tx, ctx, Event{
				Type:                 evType,
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
				References:           rec.References,
				AuthorizedReferences: rec.AuthorizedReferences,
				DependsOn:            rec.DependsOn,
				TriggerWith:          rec.TriggerWith,
				OnSpec:               rec.OnSpec,
				Gate:                 rec.Gate,
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
	draft, not_before, created_at, "references", authorized_references,
	depends_on, trigger_with, on_spec, gate, ready_emitted, removed, updated_at`

func scanIndexRecord(r interface{ Scan(...any) error }) (IndexRecord, error) {
	var (
		rec                             IndexRecord
		kindStr                         string
		draftInt, gateInt               int
		readyInt, removedInt            int
		notBeforeMS, createdMS, updatMS int64
		refsStr, authRefsStr            string
		depsStr, twStr, onStr           string
	)
	if err := r.Scan(
		&rec.Repo, &rec.Path, &rec.CommitSHA, &rec.Ref, &rec.URL, &kindStr,
		&rec.Title, &rec.Recipient, &rec.Requestor,
		&draftInt, &notBeforeMS, &createdMS, &refsStr, &authRefsStr,
		&depsStr, &twStr, &onStr, &gateInt, &readyInt, &removedInt, &updatMS,
	); err != nil {
		return IndexRecord{}, err
	}
	rec.Kind = Kind(kindStr)
	rec.Draft = draftInt != 0
	rec.Gate = gateInt != 0
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
	_ = json.Unmarshal([]byte(twStr), &rec.TriggerWith)
	_ = json.Unmarshal([]byte(onStr), &rec.OnSpec)
	rec.On = rec.OnSpec
	return rec, nil
}

// GetIndex returns the indexed Grainfile metadata for (repo, path, commitSHA).
// When commitSHA is empty, it returns the most recently updated non-removed
// record for (repo, path).
func (s *Store) GetIndex(ctx context.Context, repo, filePath, commitSHA string) (IndexRecord, error) {
	canonRepo, err := canonicalRepoFromAnyRef(repo)
	if err != nil {
		return IndexRecord{}, err
	}
	cleanPath := cleanPosixPath(strings.TrimPrefix(filepath.ToSlash(filePath), "/"))
	var row *sql.Row
	if commitSHA != "" {
		row = s.db.QueryRowContext(ctx, s.q(`SELECT `+selectIndexCols+` FROM grainfile_index WHERE repo = ? AND path = ? AND commit_sha = ? AND removed = 0`),
			canonRepo, cleanPath, commitSHA)
	} else {
		row = s.db.QueryRowContext(ctx, s.q(`SELECT `+selectIndexCols+` FROM grainfile_index WHERE repo = ? AND path = ? AND removed = 0 ORDER BY updated_at DESC LIMIT 1`),
			canonRepo, cleanPath)
	}
	rec, err := scanIndexRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return IndexRecord{}, ErrNotFound
	}
	return rec, err
}

// GetIndexByURL looks up an indexed Grainfile by its canonical URL (or by the
// (repo, path) parsed from rawURL) and optional commitSHA.
func (s *Store) GetIndexByURL(ctx context.Context, rawURL, commitSHA string) (IndexRecord, error) {
	parsed, err := ParseRef(rawURL)
	if err == nil && parsed.Form == RefRemote {
		canonURL := parsed.CanonicalURL()
		var row *sql.Row
		if commitSHA != "" {
			row = s.db.QueryRowContext(ctx, s.q(`SELECT `+selectIndexCols+` FROM grainfile_index WHERE url = ? AND commit_sha = ? AND removed = 0`),
				canonURL, commitSHA)
		} else {
			row = s.db.QueryRowContext(ctx, s.q(`SELECT `+selectIndexCols+` FROM grainfile_index WHERE url = ? AND removed = 0 ORDER BY updated_at DESC LIMIT 1`),
				canonURL)
		}
		if rec, scanErr := scanIndexRecord(row); scanErr == nil {
			return rec, nil
		}
		if parsed.Repo != "" && parsed.Path != "" {
			commit := commitSHA
			if commit == "" && parsed.GitRef != "" {
				if rec, getErr := s.GetIndex(ctx, parsed.Repo, parsed.Path, parsed.GitRef); getErr == nil {
					return rec, nil
				}
			}
			return s.GetIndex(ctx, parsed.Repo, parsed.Path, commit)
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
	return s.loadLatestIndexQ(s.db, ctx, canonRepo)
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
		if r.removed || r.Recipient != normRec {
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
		query += ` WHERE repo = ?`
		args = append(args, canonRepo)
	}
	query += ` ORDER BY updated_at DESC, commit_sha DESC`

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

// EvaluateTriggers evaluates on: triggers (push, pull_request, merge) for all
// active non-draft .process.md files in repoURL against ev, records
// process_triggered events, and transitively evaluates trigger_with co-triggers
// across all active indexed Grainfiles.
func (s *Store) EvaluateTriggers(ctx context.Context, repoURL, commitSHA string, ev ForgeEvent) ([]Event, error) {
	canonRepo, err := canonicalRepoFromAnyRef(repoURL)
	if err != nil {
		return nil, err
	}
	at := ev.At
	if at.IsZero() {
		at = s.now()
	}
	at = at.UTC()
	ev.At = at

	repoRecords, err := s.loadLatestIndexQ(s.db, ctx, canonRepo)
	if err != nil {
		return nil, err
	}
	hasMatch := false
	for _, rec := range repoRecords {
		if !rec.removed && !rec.Draft && rec.Kind == KindProcess && rec.ToGrainfile().MatchesEvent(ev) {
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

		type trigItem struct {
			rec         IndexRecord
			source      string
			triggeredBy string
		}
		var queue []trigItem
		visited := map[string]bool{}

		for _, rec := range allRecords {
			if rec.removed || rec.Draft || rec.Repo != canonRepo || rec.Kind != KindProcess {
				continue
			}
			gf := rec.ToGrainfile()
			if gf.MatchesEvent(ev) {
				key := rec.Repo + "\x00" + rec.Path
				visited[key] = true
				queue = append(queue, trigItem{
					rec:    rec,
					source: ev.Source,
				})
			}
		}

		for len(queue) > 0 {
			curr := queue[0]
			queue = queue[1:]

			cSHA := commitSHA
			if curr.rec.Repo != canonRepo || cSHA == "" {
				cSHA = curr.rec.CommitSHA
			}
			ref := ev.Branch
			if curr.rec.Repo != canonRepo || ref == "" {
				ref = curr.rec.Ref
			}

			recorded, err := s.insertEventTx(tx, ctx, Event{
				Type:                 EventProcessTriggered,
				Repo:                 curr.rec.Repo,
				Path:                 curr.rec.Path,
				CommitSHA:            cSHA,
				Ref:                  ref,
				GrainfileURL:         curr.rec.URL,
				Kind:                 curr.rec.Kind,
				Title:                curr.rec.Title,
				Recipient:            curr.rec.Recipient,
				Requestor:            curr.rec.Requestor,
				NotBefore:            curr.rec.NotBefore,
				References:           curr.rec.References,
				AuthorizedReferences: curr.rec.AuthorizedReferences,
				DependsOn:            curr.rec.DependsOn,
				TriggerWith:          curr.rec.TriggerWith,
				OnSpec:               curr.rec.OnSpec,
				Gate:                 curr.rec.Gate,
				TriggerSource:        curr.source,
				TriggeredBy:          curr.triggeredBy,
				ChangedPaths:         slices.Clone(ev.ChangedPaths),
				CreatedAt:            at.Truncate(time.Millisecond),
			})
			if err != nil {
				return err
			}
			triggeredEvents = append(triggeredEvents, recorded)

			// Evaluate trigger_with co-triggers referencing curr.rec.
			for _, candidate := range allRecords {
				if candidate.removed || candidate.Draft || len(candidate.TriggerWith) == 0 {
					continue
				}
				if !candidate.ToGrainfile().ActiveAt(at) {
					continue
				}
				candKey := candidate.Repo + "\x00" + candidate.Path
				if visited[candKey] {
					continue
				}
				for _, twURL := range candidate.TriggerWith {
					if matchesGrainfileRef(curr.rec, twURL) {
						visited[candKey] = true
						queue = append(queue, trigItem{
							rec:         candidate,
							source:      TriggerWith,
							triggeredBy: curr.rec.URL,
						})
						break
					}
				}
			}
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
	if state == "" {
		state = "done"
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
				TriggerWith:          dep.TriggerWith,
				OnSpec:               dep.OnSpec,
				Gate:                 dep.Gate,
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
				GrainfileURL:    canonCompleted,
				CommitSHA:       commitSHA,
				DependencyURL:   canonCompleted,
				DependencyState: state,
				Reason:          reason,
				CreatedAt:       now,
			}
			if completedRec != nil {
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
	events, err := s.EventsSince(ctx, 0)
	if err != nil {
		return false, err
	}
	for _, depURL := range rec.DependsOn {
		done := false
		for _, ev := range events {
			if ev.Type != EventDependencyCompleted || ev.DependencyState != "done" {
				continue
			}
			if sameURLOrRepoPath(ev.DependencyURL, depURL) || sameURLOrRepoPath(ev.GrainfileURL, depURL) {
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
	ev.On = ev.OnSpec

	var nbMS int64
	if !ev.NotBefore.IsZero() {
		nbMS = ev.NotBefore.UnixMilli()
	}
	pl := eventPayload{
		Requestor:            ev.Requestor,
		NotBeforeMillis:      nbMS,
		References:           ev.References,
		AuthorizedReferences: ev.AuthorizedReferences,
		DependsOn:            ev.DependsOn,
		TriggerWith:          ev.TriggerWith,
		OnSpec:               ev.OnSpec,
		ChangedPaths:         ev.ChangedPaths,
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
		ev.Requestor = pl.Requestor
		if pl.NotBeforeMillis > 0 {
			ev.NotBefore = time.UnixMilli(pl.NotBeforeMillis).UTC()
		}
		ev.References = pl.References
		ev.AuthorizedReferences = pl.AuthorizedReferences
		ev.DependsOn = pl.DependsOn
		ev.TriggerWith = pl.TriggerWith
		ev.OnSpec = pl.OnSpec
		ev.On = pl.OnSpec
		ev.ChangedPaths = pl.ChangedPaths
		ev.Reason = pl.Reason
	}
	return ev, nil
}

// MaxEventSeq returns the highest event sequence number recorded in
// grainfile_events, or 0 when none exist.
func (s *Store) MaxEventSeq(ctx context.Context) (int64, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM grainfile_events`).Scan(&seq)
	return seq, err
}

// EventsSince returns all discoverable Grainfile events with seq > sinceSeq,
// promoting any deferred not_before Grainfiles whose timestamp has been reached
// at s.Now() first.
func (s *Store) EventsSince(ctx context.Context, sinceSeq int64) ([]Event, error) {
	if _, err := s.PromoteDeferred(ctx, s.now()); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT `+selectEventCols+` FROM grainfile_events WHERE seq > ? ORDER BY seq ASC`), sinceSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	now := s.now()
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		if !ev.NotBefore.IsZero() && now.Before(ev.NotBefore) {
			continue
		}
		out = append(out, ev)
	}
	return out, rows.Err()
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
		if ev.Recipient == normRec {
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

	recipients, err := normalizeSubscriptionList(sub.Recipients, normalizeRecipientURL)
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

	err = s.tx(ctx, func(tx *sql.Tx) error {
		var prevCursor, prevRev int64
		var prevOwner string
		err := tx.QueryRowContext(ctx, s.q(`SELECT owner, cursor, revision FROM grainfile_subscriptions WHERE node_id = ?`), nodeID).
			Scan(&prevOwner, &prevCursor, &prevRev)
		if err == nil {
			out.Revision = prevRev + 1
			if out.Cursor == 0 && prevCursor > 0 {
				out.Cursor = prevCursor
			}
			if out.Owner == "" {
				out.Owner = prevOwner
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
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
	row := s.db.QueryRowContext(ctx, s.q(`SELECT node_id, owner, recipients, repos, folders, grainfiles, cursor, revision, updated_at
		FROM grainfile_subscriptions WHERE node_id = ?`), nodeID)
	sub, err := scanSubscriptions(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Subscriptions{}, ErrNotFound
	}
	return sub, err
}

// ListSubscriptions returns all registered node subscriptions ordered by node_id.
func (s *Store) ListSubscriptions(ctx context.Context) ([]Subscriptions, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node_id, owner, recipients, repos, folders, grainfiles, cursor, revision, updated_at
		FROM grainfile_subscriptions ORDER BY node_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subscriptions
	for rows.Next() {
		sub, err := scanSubscriptions(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
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
	_, err := s.db.ExecContext(ctx, s.q(`DELETE FROM grainfile_subscriptions WHERE node_id = ?`), nodeID)
	return err
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

// MatchesRecord reports whether sub subscribes to rec by recipient URL,
// repository URL, folder prefix, or explicit Grainfile URL.
func (sub Subscriptions) MatchesRecord(rec IndexRecord) bool {
	if slices.Contains(sub.Recipients, rec.Recipient) {
		return true
	}
	for _, r := range sub.Repos {
		if IsSameRepo(r, rec.Repo) {
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

func normalizeSubRepoURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("empty repository URL")
	}
	if !strings.Contains(raw, "://") && strings.Count(raw, "/") == 1 {
		raw = "https://github.com/" + raw
	}
	return CanonicalRepoURL(raw)
}

func normalizeRecipientURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("empty recipient URL")
	}
	if !strings.Contains(raw, "://") && strings.Count(raw, "/") == 1 {
		raw = "https://github.com/" + raw
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
