// Package mirror is the outbound mirror into the uber repo
// (docs/silo/local-git.md, §8, calls L46 to L53): silod pushes every
// mirrored repo's branches, tags and local archive refs to one external
// git repo, each under its silo's path.
//
// The mirror is state-based. A pass over one repo reads its local
// refs, maps them to the uber repo's names (ref layout, call L46),
// compares them with mirror_refs, the store's record of what the uber
// repo has, and pushes the difference in one atomic push, each ref
// leased against what silod last put there. Passes for one repo never
// overlap; different repos run up to Options.Concurrency at once. A
// failed pass is retried with backoff, forever; a repo that has failed
// for an hour is STALLED and audited. A lease that fails leaves the ref
// DIVERGED until an operator adopts it. Nothing here deletes an archive
// ref, or a live ref whose tip the same push doesn't keep.
//
// The mirror never reads tenant history back into a silo's repo. The
// one thing it fetches is an external tip it is about to archive, into
// mirror/quarantine.git.
package mirror

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/repos"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// Defaults (local-git.md, §8).
const (
	DefaultPrefix      = "silo"
	DefaultIndexBranch = "main"
	DefaultConcurrency = 4
	DefaultMinBackoff  = time.Second
	DefaultMaxBackoff  = 5 * time.Minute
	DefaultStallAfter  = time.Hour
	DefaultPushTimeout = 10 * time.Minute
	// DefaultChunkBytes is the most new objects a first push sends at
	// once: under GitHub's 2 GB push limit with room to spare.
	DefaultChunkBytes = 1 << 30
	// DefaultBatchRefs is the most refs one push changes.
	DefaultBatchRefs = 1000
)

// ProbeRef is the branch silo mirror set tests the uber repo's rules
// with, under the prefix. local-git.md names it .silo-probe, which git
// refuses (a ref component can't start with a dot); no silo or repo
// can be named _silo-probe, so it collides with none.
const ProbeRef = "_silo-probe"

// Repos is what the mirror needs of the repos on disk;
// *gitstore.Host is it.
type Repos interface {
	Path(silo, id string) (string, error)
	Lock(id string) (unlock func())
	Refs(ctx context.Context, silo, id string) (map[string]string, error)
	Delete(ctx context.Context, silo, id string) error
}

// Options configure a Mirror. Zero durations and sizes take the
// defaults above.
type Options struct {
	Store store.Store
	Repos Repos
	// Keys is keys/mirror/, where the credential is kept (mode 0700).
	Keys string
	// Dir is the data directory's mirror/, for quarantine.git.
	Dir string
	// Git is the git binary; "" is git on $PATH.
	Git         string
	Concurrency int
	MinBackoff  time.Duration
	MaxBackoff  time.Duration
	StallAfter  time.Duration
	PushTimeout time.Duration
	ChunkBytes  int64
	BatchRefs   int
	// Poll is how often Run looks for work it wasn't woken for: a
	// backoff that ran out, or the end of report-only mode.
	Poll time.Duration
	// IndexDebounce is how long the submodule index waits after a pass
	// before it commits (local-git.md, "How it is written").
	IndexDebounce time.Duration
	// KeyScan reads an SSH host's keys, to pin them when the mirror is
	// set; nil runs ssh-keyscan.
	KeyScan func(ctx context.Context, host, port string) ([]byte, error)
	Logf    func(format string, args ...any)
}

// Mirror is the outbound mirror. It is fenceapi.Mirror.
type Mirror struct {
	opts Options

	mu     sync.Mutex
	repos  map[string]*state // by repo id
	wake   chan struct{}
	passes sync.Map // repo id -> *sync.Mutex, so passes never overlap
	seq    atomic.Uint64

	// running counts the goroutines Run has started -- the change
	// follower, repo passes and index passes -- so that Run returns only
	// once none of them is left.
	running sync.WaitGroup

	// indexAt is when the index pass is next due (zero: not due), and
	// indexBackoff how long it waits after a failure. indexMu keeps
	// index passes from overlapping.
	indexAt      time.Time
	indexRunning bool
	indexBackoff time.Duration
	indexMu      sync.Mutex
}

// state is the loop's view of one repo.
type state struct {
	dirty    uint64 // the newest change that touched it
	mirrored uint64 // the change the last good pass covered
	force    bool   // due whatever the sequence numbers say
	running  bool
	next     time.Time // no pass before this, after a failure
	backoff  time.Duration
}

var _ fenceapi.Mirror = (*Mirror)(nil)

// New makes a Mirror.
func New(o Options) *Mirror {
	if o.Concurrency <= 0 {
		o.Concurrency = DefaultConcurrency
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = DefaultMinBackoff
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = DefaultMaxBackoff
	}
	if o.StallAfter <= 0 {
		o.StallAfter = DefaultStallAfter
	}
	if o.PushTimeout <= 0 {
		o.PushTimeout = DefaultPushTimeout
	}
	if o.ChunkBytes <= 0 {
		o.ChunkBytes = DefaultChunkBytes
	}
	if o.BatchRefs <= 0 {
		o.BatchRefs = DefaultBatchRefs
	}
	if o.Poll <= 0 {
		o.Poll = time.Second
	}
	if o.IndexDebounce <= 0 {
		o.IndexDebounce = DefaultIndexDebounce
	}
	if o.Logf == nil {
		o.Logf = log.Printf
	}
	return &Mirror{opts: o, repos: map[string]*state{}, wake: make(chan struct{}, 1)}
}

func (m *Mirror) poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// markDirty records that repo id changed at seq; force makes it due
// even if a pass has covered seq.
func (m *Mirror) markDirty(id string, seq uint64, force bool) {
	m.mu.Lock()
	s := m.repos[id]
	if s == nil {
		s = &state{}
		m.repos[id] = s
	}
	if seq > s.dirty {
		s.dirty = seq
	}
	s.force = s.force || force
	m.mu.Unlock()
	m.poke()
}

// markAll makes every repo, and every deleted one still being
// archived, due for a pass, as of seq.
func (m *Mirror) markAll(ctx context.Context, seq uint64) error {
	var ids []string
	err := m.opts.Store.Tx(ctx, func(tx store.Tx) error {
		ids = nil
		all, err := repos.All(ctx, tx)
		if err != nil {
			return err
		}
		for _, r := range all {
			ids = append(ids, r.ID)
		}
		rows, err := readRepoRows(ctx, tx)
		if err != nil {
			return err
		}
		for id, r := range rows {
			if r.Deleting != "" {
				ids = append(ids, id)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, id := range ids {
		m.markDirty(id, seq, true)
	}
	return nil
}

// Run runs the mirror loop until ctx ends: at start every repo is
// dirty once, which also covers a crash between a push and its store
// write; then each change to a repo, or to the tree or policies its
// path and mirroring come from, makes it dirty again. Run returns only
// once every pass it started has finished.
func (m *Mirror) Run(ctx context.Context) error {
	var seq uint64
	if err := m.opts.Store.Tx(ctx, func(tx store.Tx) error {
		var err error
		seq, err = lastSeq(ctx, tx)
		return err
	}); err != nil {
		return err
	}
	changes, err := m.opts.Store.Changes(ctx, seq)
	if err != nil {
		return err
	}
	if err := m.markAll(ctx, seq); err != nil {
		return err
	}
	defer m.running.Wait()
	m.indexDue(0)
	m.running.Add(1)
	go func() {
		defer m.running.Done()
		for c := range changes {
			switch c.Kind {
			case repos.PushChangeKind, repos.ChangeKind:
				m.markDirty(c.Key, c.Seq, false)
			case "silo", "policy":
				// A rename, a move or a policy can change any repo's
				// external names or whether it is mirrored; a pass that
				// finds nothing to do costs no network.
				if err := m.markAll(ctx, c.Seq); err != nil && ctx.Err() == nil {
					m.opts.Logf("mirror: %v", err)
				}
			}
		}
	}()
	sem := make(chan struct{}, m.opts.Concurrency)
	tick := time.NewTicker(m.opts.Poll)
	defer tick.Stop()
	for {
		if paused, err := m.paused(ctx); err == nil && paused == "" {
			m.dispatch(ctx, sem)
			m.dispatchIndex(ctx)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-m.wake:
		case <-tick.C:
		}
	}
}

// dispatch starts a pass for every dirty repo that is due and isn't in
// one, as far as sem allows.
func (m *Mirror) dispatch(ctx context.Context, sem chan struct{}) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	var due []string
	for id, s := range m.repos {
		if (s.force || s.dirty > s.mirrored) && !s.running && !now.Before(s.next) {
			due = append(due, id)
		}
	}
	sort.Strings(due)
	for _, id := range due {
		select {
		case sem <- struct{}{}:
		default:
			return
		}
		s := m.repos[id]
		s.running, s.force = true, false
		seq := s.dirty
		m.running.Add(1)
		go func() {
			defer m.running.Done()
			defer func() { <-sem; m.poke() }()
			err := m.pass(ctx, id, seq)
			m.done(id, seq, err)
		}()
	}
}

// done records a pass's outcome in the loop's view: exponential
// backoff from MinBackoff to MaxBackoff after a failure.
func (m *Mirror) done(id string, seq uint64, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.repos[id]
	if s == nil {
		s = &state{}
		m.repos[id] = s
	}
	s.running = false
	if err == nil {
		s.mirrored, s.backoff, s.next = max(s.mirrored, seq), 0, time.Time{}
		m.indexDueLocked(m.opts.IndexDebounce)
		return
	}
	if s.backoff == 0 {
		s.backoff = m.opts.MinBackoff
	} else if s.backoff *= 2; s.backoff > m.opts.MaxBackoff {
		s.backoff = m.opts.MaxBackoff
	}
	s.next, s.force = time.Now().Add(s.backoff), true
}

// indexDue makes the index pass due after wait, unless it is due
// sooner already.
func (m *Mirror) indexDue(wait time.Duration) {
	m.mu.Lock()
	m.indexDueLocked(wait)
	m.mu.Unlock()
	m.poke()
}

func (m *Mirror) indexDueLocked(wait time.Duration) {
	at := time.Now().Add(wait)
	if m.indexAt.IsZero() || at.Before(m.indexAt) {
		m.indexAt = at
	}
}

// dispatchIndex starts the index pass if it is due and no repo pass is
// running, so that one index commit follows each batch of passes.
func (m *Mirror) dispatchIndex(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.indexAt.IsZero() || m.indexRunning || time.Now().Before(m.indexAt) {
		return
	}
	for _, s := range m.repos {
		if s.running {
			return
		}
	}
	m.indexAt, m.indexRunning = time.Time{}, true
	m.running.Add(1)
	go func() {
		defer m.running.Done()
		err := m.Index(ctx)
		m.mu.Lock()
		m.indexRunning = false
		if err == nil {
			m.indexBackoff = 0
		} else {
			if m.indexBackoff == 0 {
				m.indexBackoff = m.opts.MinBackoff
			} else if m.indexBackoff *= 2; m.indexBackoff > m.opts.MaxBackoff {
				m.indexBackoff = m.opts.MaxBackoff
			}
			m.indexDueLocked(m.indexBackoff)
		}
		m.mu.Unlock()
		m.poke()
	}()
}

// paused says why passes don't run, if they don't: report-only mode,
// which a restored silod starts in (local-git.md, "After a restore").
func (m *Mirror) paused(ctx context.Context) (string, error) {
	var why string
	err := m.opts.Store.Tx(ctx, func(tx store.Tx) error {
		mode, ok, err := store.ReadSetting(ctx, tx, store.SettingReconcile)
		if err == nil && ok && mode == store.ReconcileReportOnly {
			why = "report-only: silod was restored; silo reconcile --apply resumes the mirror"
		}
		return err
	})
	return why, err
}

// target is one repo as a pass sees it.
type target struct {
	ID, Silo, Path string // Path is its uber-repo path, team-a/x/foo.git
	Deleting       string
	Mirrored       bool
	Records        map[string]record
	Diverged       map[string]diverged
}

// load reads what a pass over repo id needs. A nil config is no mirror
// set; a nil target is no such repo.
func (m *Mirror) load(ctx context.Context, tx store.Tx, id string) (*config, *target, error) {
	cfg, err := loadConfig(ctx, tx)
	if err != nil || cfg == nil {
		return nil, nil, err
	}
	row, err := readRepoRow(ctx, tx, id)
	if err != nil {
		return nil, nil, err
	}
	t := &target{ID: id}
	r, err := repos.Get(ctx, tx, id)
	switch {
	case err == nil:
		s, err := tree.Get(ctx, tx, r.Silo)
		if err != nil {
			return nil, nil, err
		}
		t.Silo, t.Path = s.ID, fenceapi.GitPath(s.Path, r.Name)
		if t.Mirrored, err = fenceapi.ReposMirrored(ctx, tx, s.ID); err != nil {
			return nil, nil, err
		}
	case errors.Is(err, repos.ErrNotFound) && row != nil && row.Deleting != "":
		t.Silo, t.Path, t.Deleting, t.Mirrored = row.Silo, row.Path, row.Deleting, true
	case errors.Is(err, repos.ErrNotFound):
		return cfg, nil, nil
	default:
		return nil, nil, err
	}
	if t.Records, err = readRecords(ctx, tx, id); err != nil {
		return nil, nil, err
	}
	if t.Diverged, err = readDiverged(ctx, tx, id); err != nil {
		return nil, nil, err
	}
	return cfg, t, nil
}

// lock serializes passes and adoptions of repo id.
func (m *Mirror) lock(id string) func() {
	v, _ := m.passes.LoadOrStore(id, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// pass makes the uber repo's refs for repo id equal its local ones, as
// of change seq.
func (m *Mirror) pass(ctx context.Context, id string, seq uint64) error {
	unlock := m.lock(id)
	defer unlock()
	var cfg *config
	var t *target
	err := m.opts.Store.Tx(ctx, func(tx store.Tx) error {
		var err error
		cfg, t, err = m.load(ctx, tx, id)
		if err == nil && cfg != nil && t == nil {
			// A repo gone without the mirror's help: nothing to keep.
			err = forget(ctx, tx, id)
		}
		return err
	})
	if err != nil || cfg == nil || t == nil || !t.Mirrored {
		return err
	}
	return m.record(ctx, t, seq, m.sync(ctx, cfg, t))
}

// sync pushes until the uber repo matches, setting aside the refs whose
// lease fails.
func (m *Mirror) sync(ctx context.Context, cfg *config, t *target) error {
	dir, err := m.opts.Repos.Path(t.Silo, t.ID)
	if err != nil {
		return err
	}
	// The repo's lock only while its refs are read: the push itself may
	// take minutes, and a local push never waits on the mirror. The
	// objects those refs reach stay, since maintenance prunes nothing a
	// ref (archive refs included) still reaches.
	unlockRepo := m.opts.Repos.Lock(t.ID)
	local, err := m.opts.Repos.Refs(ctx, t.Silo, t.ID)
	unlockRepo()
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		us, held, err := m.plan(ctx, cfg, t, dir, local)
		if err != nil {
			return err
		}
		if len(held) > 0 {
			if err := m.diverge(ctx, cfg, t, &staleError{refs: held}); err != nil {
				return err
			}
		}
		if len(us) == 0 {
			return nil
		}
		if len(t.Records) == 0 {
			if us, err = m.stage(ctx, cfg, t, dir, us); err != nil {
				return err
			}
		}
		err = m.pushAll(ctx, cfg, t, dir, us)
		var se *staleError
		if !errors.As(err, &se) || attempt >= 3 {
			return err
		}
		// The rest of the repo keeps mirroring without the refs whose
		// lease failed (local-git.md, "When the uber repo disagrees").
		if err := m.diverge(ctx, cfg, t, se); err != nil {
			return err
		}
	}
}

// staleError is a push that failed on leases: which refs.
type staleError struct {
	refs []update
}

func (e *staleError) Error() string { return fmt.Sprintf("mirror: %d leases failed", len(e.refs)) }

// external maps a local ref of the repo at path to its uber-repo
// name, or "" for one the mirror doesn't push.
func external(prefix, path, local string) string {
	switch {
	case strings.HasPrefix(local, "refs/heads/"):
		return "refs/heads/" + prefix + "/" + path + "/" + strings.TrimPrefix(local, "refs/heads/")
	case strings.HasPrefix(local, "refs/tags/"):
		return "refs/tags/" + prefix + "/" + path + "/" + strings.TrimPrefix(local, "refs/tags/")
	case strings.HasPrefix(local, "refs/silo-archive/"):
		return archiveRoot(prefix, path) + strings.TrimPrefix(local, "refs/silo-archive/")
	}
	return ""
}

// archiveRoot is where the repo at path's archive refs go.
func archiveRoot(prefix, path string) string { return "refs/" + prefix + "-archive/" + path + "/" }

func live(ref string) bool {
	return strings.HasPrefix(ref, "refs/heads/") || strings.HasPrefix(ref, "refs/tags/")
}

// stamp is a new archive ref's <ts>-<seq> for one the mirror itself
// makes: m and a counter, so it can't collide with the pre-receive
// hook's in the same second.
func (m *Mirror) stamp() string {
	return time.Now().UTC().Format(repos.ArchiveTime) + "-m" + fmt.Sprint(m.seq.Add(1))
}

// plan works out the updates that make the uber repo's refs for t
// equal local.
// held are live refs whose update would lose a tip the uber repo has
// and nothing else keeps.
func (m *Mirror) plan(ctx context.Context, cfg *config, t *target, dir string, local map[string]string) (us, held []update, err error) {
	want := map[string]update{} // by external name
	byLocal := map[string]bool{}
	var stamp string
	newStamp := func() string {
		if stamp == "" {
			stamp = m.stamp()
		}
		return stamp
	}
	for name, sha := range local {
		ext := external(cfg.Prefix, t.Path, name)
		if ext == "" {
			continue
		}
		if t.Deleting != "" && live(name) {
			// A deleted repo's live refs become archives (call L49):
			// every branch and tag, kept forever.
			ext = m.archivedAs(t, name, sha)
			if ext == "" {
				ext = archiveRoot(cfg.Prefix, t.Path) + newStamp() + "/" + strings.TrimPrefix(name, "refs/")
			}
			want[ext] = update{Dst: ext, Src: sha}
			continue
		}
		want[ext] = update{Dst: ext, Src: sha, Local: name}
		byLocal[name] = true
	}
	var overwrites []update
	keep := map[string]bool{} // SHAs the uber repo keeps under a ref after the push
	for ext, w := range want {
		keep[w.Src] = true
		if _, ok := t.Diverged[ext]; ok {
			continue
		}
		rec, ok := t.Records[ext]
		if ok && rec.SHA == w.Src {
			continue
		}
		w.Lease = rec.SHA
		if ok && live(ext) {
			overwrites = append(overwrites, w)
			continue
		}
		us = append(us, w)
	}
	var drop []update
	for ext, rec := range t.Records {
		if _, ok := want[ext]; ok {
			continue
		}
		if _, ok := t.Diverged[ext]; ok {
			keep[rec.SHA] = true
			continue
		}
		switch {
		case live(ext):
			// A live ref deleted locally, or under a path the repo no
			// longer has (a rename or move).
			drop = append(drop, update{Dst: ext, Lease: rec.SHA, Local: rec.Local})
		case rec.Local != "" && byLocal[rec.Local]:
			// An archive ref that moved with its repo: the new name
			// holds it in the same push.
			drop = append(drop, update{Dst: ext, Lease: rec.SHA, Local: rec.Local})
		default:
			// Archive refs stay forever.
			keep[rec.SHA] = true
		}
	}
	// A live ref is deleted only if the push keeps its tip, under
	// another name or reachable from one. The pre-receive hook's
	// archives make that so for every overwrite; this is the check
	// that nothing slipped past it.
	var tips []string
	for sha := range keep {
		tips = append(tips, sha)
	}
	sort.Strings(tips)
	// So is an update: its old tip must be kept, as it is after a
	// fast-forward or a local overwrite the hook archived. One that
	// isn't (an external tip kept by adopt --keep-external, which silod
	// may not even have) is held back as diverged, as a failed lease is.
	for _, w := range overwrites {
		kept := keep[w.Lease]
		if !kept {
			kept, _ = m.reachable(ctx, dir, w.Lease, tips)
		}
		if kept {
			us = append(us, w)
		} else {
			held = append(held, w)
		}
	}
	for _, d := range drop {
		if !keep[d.Lease] && live(d.Dst) {
			kept, err := m.reachable(ctx, dir, d.Lease, tips)
			if err != nil {
				m.opts.Logf("mirror: %s: not deleting %s, whose tip %s silod can't show is kept: %v", t.ID, d.Dst, d.Lease, err)
				continue
			}
			if !kept {
				kind := "heads/"
				if strings.HasPrefix(d.Dst, "refs/tags/") {
					kind = "tags/"
				}
				ext := archiveRoot(cfg.Prefix, t.Path) + newStamp() + "/" + kind + lastPart(d.Dst)
				us = append(us, update{Dst: ext, Src: d.Lease})
			}
		}
		us = append(us, d)
	}
	return us, held, nil
}

// lastPart is a live external ref's branch or tag name: what follows
// the repo's <name>.git.
func lastPart(ext string) string {
	if i := strings.Index(ext, ".git/"); i >= 0 {
		return ext[i+len(".git/"):]
	}
	return ext
}

// archivedAs finds the archive a deleted repo's earlier pass already
// made of local ref name at sha, so that a retry doesn't make another.
func (m *Mirror) archivedAs(t *target, name, sha string) string {
	suffix := "/" + strings.TrimPrefix(name, "refs/")
	for ext, rec := range t.Records {
		if rec.Local == "" && rec.SHA == sha && strings.HasSuffix(ext, suffix) && !strings.Contains(ext, "/external/") {
			return ext
		}
	}
	return ""
}

// reachable reports whether sha is one of tips or reachable from them,
// in the repo at dir.
func (m *Mirror) reachable(ctx context.Context, dir, sha string, tips []string) (bool, error) {
	args := append([]string{"rev-list", "-n1", sha, "--not"}, tips...)
	out, err := m.git(ctx, dir, nil, nil, args...)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "", nil
}

// order puts archives first, then live refs made or moved, then
// deletions, so that every batch of a split push keeps what the
// batches before it promised.
func order(us []update) {
	rank := func(u update) int {
		switch {
		case u.Src == "":
			return 2
		case live(u.Dst):
			return 1
		}
		return 0
	}
	sort.SliceStable(us, func(i, j int) bool {
		if a, b := rank(us[i]), rank(us[j]); a != b {
			return a < b
		}
		return us[i].Dst < us[j].Dst
	})
}

// pushAll pushes us in atomic batches of at most BatchRefs refs,
// recording each batch as it lands.
func (m *Mirror) pushAll(ctx context.Context, cfg *config, t *target, dir string, us []update) error {
	order(us)
	for len(us) > 0 {
		n := min(len(us), m.opts.BatchRefs)
		if n == len(us)-1 && us[n].NoLease {
			n = len(us) // the staging ref's deletion goes with the last batch
		}
		batch := us[:n]
		res, err := m.push(ctx, cfg, m.opts.Keys, dir, batch)
		if errors.Is(err, errStale) {
			se := &staleError{}
			for _, u := range batch {
				for _, s := range res.stale {
					if u.Dst == s {
						se.refs = append(se.refs, u)
					}
				}
			}
			return se
		}
		if err != nil {
			return err
		}
		if err := m.opts.Store.Tx(ctx, func(tx store.Tx) error {
			for _, u := range batch {
				if !u.NoLease {
					if err := setRecord(ctx, tx, t.ID, u.Dst, u.Local, u.Src); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			return err
		}
		for _, u := range batch {
			switch {
			case u.NoLease:
			case u.Src == "":
				delete(t.Records, u.Dst)
			default:
				t.Records[u.Dst] = record{Local: u.Local, SHA: u.Src}
			}
		}
		us = us[n:]
	}
	return nil
}

// diverge records the refs whose lease failed as DIVERGED, with what
// the uber repo has, and audits each.
func (m *Mirror) diverge(ctx context.Context, cfg *config, t *target, se *staleError) error {
	var names []string
	for _, u := range se.refs {
		names = append(names, u.Dst)
	}
	actual, err := m.lsRemote(ctx, cfg, m.opts.Keys, names...)
	if err != nil {
		return err
	}
	return m.opts.Store.Tx(ctx, func(tx store.Tx) error {
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		for _, u := range se.refs {
			if _, err := tx.Exec(ctx, `INSERT INTO mirror_diverged (ref, repo, local, expected, actual, since) VALUES (?, ?, ?, ?, ?, ?)
				ON CONFLICT (ref) DO UPDATE SET repo = excluded.repo, local = excluded.local, expected = excluded.expected, actual = excluded.actual`,
				u.Dst, t.ID, u.Local, u.Lease, actual[u.Dst], store.Millis(now)); err != nil {
				return err
			}
			t.Diverged[u.Dst] = diverged{Ref: u.Dst, Local: u.Local, Expected: u.Lease, Actual: actual[u.Dst]}
			if err := m.audit(ctx, tx, t.Silo, fenceapi.KindMirrorDiverged, map[string]any{
				"repo": t.ID, "ref": u.Dst, "expected": u.Lease, "actual": actual[u.Dst]}); err != nil {
				return err
			}
		}
		return nil
	})
}

// audit records kind under silo, or under the root if silo is gone,
// as silod's own doing.
func (m *Mirror) audit(ctx context.Context, tx store.Tx, silo, kind string, fields map[string]any) error {
	root, err := tree.Root(ctx, tx)
	if err != nil {
		return err
	}
	s, err := tree.Get(ctx, tx, silo)
	if err != nil {
		s = root
	}
	return fenceapi.Audit(ctx, tx, s, fenceapi.Caller{Silo: root.ID, Via: fenceapi.ViaInternal}, kind, "", "OK", fields)
}

// record writes a pass's outcome: in sync as of seq, or how long it
// has been failing, and STALLED, audited once, past StallAfter. A
// deleted repo whose refs are archived is removed and forgotten, and
// its operation finished.
func (m *Mirror) record(ctx context.Context, t *target, seq uint64, passErr error) error {
	if passErr == nil && t.Deleting != "" {
		if err := m.opts.Repos.Delete(ctx, t.Silo, t.ID); err != nil {
			passErr = fmt.Errorf("removing the deleted repo's directory: %w", err)
		} else {
			return m.opts.Store.Tx(ctx, func(tx store.Tx) error {
				if err := forget(ctx, tx, t.ID); err != nil {
					return err
				}
				if t.Deleting != "-" {
					return fenceapi.FinishOperation(ctx, tx, t.Deleting)
				}
				return nil
			})
		}
	}
	err := m.opts.Store.Tx(ctx, func(tx store.Tx) error {
		row, err := readRepoRow(ctx, tx, t.ID)
		if err != nil {
			return err
		}
		if row == nil {
			row = &repoRow{Repo: t.ID}
		}
		row.Silo, row.Path, row.Deleting = t.Silo, t.Path, t.Deleting
		if passErr == nil {
			row.MirroredSeq = max(row.MirroredSeq, seq)
			row.PendingSince, row.Stalled, row.LastError = 0, false, ""
			return writeRepoRow(ctx, tx, row)
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		row.LastError = passErr.Error()
		if row.PendingSince == 0 {
			row.PendingSince = store.Millis(now)
		}
		if !row.Stalled && now.Sub(store.Time(row.PendingSince)) >= m.opts.StallAfter {
			row.Stalled = true
			if err := m.audit(ctx, tx, t.Silo, fenceapi.KindMirrorStalled, map[string]any{
				"repo": t.ID, "path": t.Path, "since": store.Time(row.PendingSince), "error": row.LastError}); err != nil {
				return err
			}
		}
		return writeRepoRow(ctx, tx, row)
	})
	if passErr != nil {
		m.opts.Logf("mirror: %s (%s): %v", t.Path, t.ID, passErr)
		return passErr
	}
	return err
}

// Deleting takes over the deletion of repo r of silo t, if it is
// mirrored: its refs are archived in the uber repo before its directory
// goes (local-git.md, "Deleting a repo or a silo", call L49).
func (m *Mirror) Deleting(ctx context.Context, tx store.Tx, t *tree.Silo, r *repos.Repo, op string) (bool, error) {
	cfg, err := loadConfig(ctx, tx)
	if err != nil || cfg == nil {
		return false, err
	}
	on, err := fenceapi.ReposMirrored(ctx, tx, t.ID)
	if err != nil || !on {
		return false, err
	}
	row, err := readRepoRow(ctx, tx, r.ID)
	if err != nil {
		return false, err
	}
	if row == nil {
		row = &repoRow{Repo: r.ID}
	}
	if op == "" {
		op = "-"
	}
	row.Silo, row.Path, row.Deleting = t.ID, fenceapi.GitPath(t.Path, r.Name), op
	return true, writeRepoRow(ctx, tx, row)
}

// writeKeys writes the credential, and for SSH the pinned host keys,
// into a new directory beside keys, which install moves into place
// once the probe has passed with it.
func (m *Mirror) writeKeys(ctx context.Context, kind, url string, cred []byte) (string, error) {
	dir := m.opts.Keys + ".new"
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, credentialFile), cred, 0o600); err != nil {
		return "", err
	}
	if kind == CredentialSSH {
		host, port, err := sshHost(url)
		if err != nil {
			return "", err
		}
		scan := m.opts.KeyScan
		if scan == nil {
			scan = keyScan
		}
		keys, err := scan(ctx, host, port)
		if err != nil {
			return "", fmt.Errorf("reading %s's host keys to pin: %w", host, err)
		}
		if len(strings.TrimSpace(string(keys))) == 0 {
			return "", fmt.Errorf("%s offered no host keys to pin", host)
		}
		if err := os.WriteFile(filepath.Join(dir, knownHostsFile), keys, 0o600); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func install(staged, keys string) error {
	if err := os.RemoveAll(keys); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(keys), 0o700); err != nil {
		return err
	}
	return os.Rename(staged, keys)
}
