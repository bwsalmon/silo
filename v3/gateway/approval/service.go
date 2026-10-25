package approval

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Batch states.
const (
	StatusPending     = "pending"
	StatusApproved    = "approved" // decided, not yet running
	StatusRunning     = "running"
	StatusSucceeded   = "succeeded"
	StatusFailed      = "failed"      // a file write or step failed, or the snapshot did
	StatusRefused     = "refused"     // re-verification found a hash mismatch
	StatusRejected    = "rejected"    // the human said no
	StatusCancelled   = "cancelled"   // the submitter or the human withdrew it
	StatusInterrupted = "interrupted" // the gateway restarted while it was approved or running
)

// Terminal reports whether a batch in status s is finished.
func Terminal(s string) bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusRefused, StatusRejected, StatusCancelled, StatusInterrupted:
		return true
	}
	return false
}

// Audit events (design §4.8).
const (
	EvSubmit      = "batch.submit"
	EvDecide      = "batch.decide"
	EvCancel      = "batch.cancel"
	EvStandingUse = "standing.use"
	EvSnapshot    = "snapshot"
	EvExecStart   = "batch.exec.start"
	EvExecFile    = "batch.exec.file"
	EvExecStep    = "batch.exec.step"
	EvExecEnd     = "batch.exec.end"
	EvExecRefused = "batch.exec.refused"
)

// Config is the [run_as_root] section of gateway.toml.
type Config struct {
	// WaitTimeoutS is how long run_as_root.wait blocks by default (and at
	// most): the root agent's CLI tool timeout (design §7.4).
	WaitTimeoutS int `json:"wait_timeout_s"`
	// ShippedDir holds the shipped default instructions that edits to
	// root's instructions are diffed against.
	ShippedDir string `json:"shipped_dir"`
	// Snapshots: "auto" (detect, the default) or "off".
	Snapshots string `json:"snapshots"`
	// Standing approvals, by rule id. None ship, and a rule does nothing
	// until enabled.
	Standing map[string]StandingRule `json:"standing"`
}

// StandingRule pre-approves batches made only of one typed operation
// (design §7.5): scoped by operation, names and agents, expiring, and
// limited in uses.
type StandingRule struct {
	Enabled bool   `json:"enabled"`
	What    string `json:"what"` // pkg.install, pkg.remove, unit.enable, unit.restart, user.add_to_group
	// Names are the packages (pkg.*), units (unit.*) or groups
	// (user.add_to_group) the rule covers; shell globs allowed.
	Names   []string `json:"names"`
	Who     []string `json:"who"`
	Expires string   `json:"expires"` // RFC 3339, required, at most 90 days ahead
	MaxUses int      `json:"max_uses"`
}

// maxRuleLife bounds a standing approval's lifetime.
const maxRuleLife = 90 * 24 * time.Hour

const (
	defaultWaitS  = 600
	maxPending    = 20 // per submitter
	maxRecords    = 500
	recordPrefix  = "batch-"
	batchesSubdir = "batches"
)

// Deps are the service's connections to the gateway and the host.
type Deps struct {
	// Dir is the gateway's state directory; batches are staged under
	// Dir/batches/<hash>/.
	Dir string
	// RootAgent is the id of the only agent that may submit.
	RootAgent string
	Now       func() time.Time
	// Audit appends to the gateway's hash-chained audit log.
	Audit func(actor, event string, payload any)
	Logf  func(format string, args ...any)
	// Trust checks executables and argv paths; RootOwned(0) by default.
	Trust TrustFunc
	// Snapshot is the backend; detected on the real host by default.
	Snapshot Snapshotter
	Steps    StepRunner
	Chown    ChownFunc
}

// Principal is an authenticated caller, as the gateway describes it.
type Principal struct {
	// Actor is the audit log's name for the caller.
	Actor string
	// Agent is set when the caller is an agent.
	Agent string
	// Human is set for the human: an admin on the box or the bridge (UI).
	Human bool
	// Admin is set for a human on the box, identified by SO_PEERCRED.
	// Only admins may decide until the UI's passkey signatures (§4.4)
	// exist.
	Admin bool
}

// Record is a batch and everything that has happened to it.
type Record struct {
	ID           string       `json:"id"`
	Hash         string       `json:"hash"`
	Status       string       `json:"status"`
	Submitter    string       `json:"submitter"`
	SubmittedAt  time.Time    `json:"submitted_at"`
	Bound        *Bound       `json:"bound"`
	Presentation Presentation `json:"presentation"`
	Decision     *Decision    `json:"decision,omitempty"`
	Snapshot     *SnapshotRef `json:"snapshot,omitempty"`
	Files        []FileResult `json:"files,omitempty"`
	Steps        []StepResult `json:"steps,omitempty"`
	Error        string       `json:"error,omitempty"`
	StartedAt    *time.Time   `json:"started_at,omitempty"`
	FinishedAt   *time.Time   `json:"finished_at,omitempty"`
}

// Decision is who decided a batch, how, and why.
type Decision struct {
	Decision string    `json:"decision"` // approve, reject, cancel
	By       string    `json:"by"`
	At       time.Time `json:"at"`
	Comment  string    `json:"comment,omitempty"`
	// Rule is the standing approval that approved it, if any.
	Rule string `json:"rule,omitempty"`
}

// SnapshotRef is the snapshot taken before a batch ran.
type SnapshotRef struct {
	Kind  string `json:"kind"`
	ID    string `json:"id,omitempty"`
	Note  string `json:"note,omitempty"`
	Error string `json:"error,omitempty"`
}

// StepResult is what one step did.
type StepResult struct {
	Index    int      `json:"index"`
	Argv     []string `json:"argv"`
	Exit     int      `json:"exit"`
	Stdout   string   `json:"stdout"`
	Stderr   string   `json:"stderr"`
	Error    string   `json:"error,omitempty"`
	Duration float64  `json:"duration_s"`
}

// Status is what the submitter sees of a batch: no presentation.
type Status struct {
	BatchID     string       `json:"batch_id"`
	Hash        string       `json:"hash"`
	Title       string       `json:"title"`
	Status      string       `json:"status"`
	Submitter   string       `json:"submitter"`
	SubmittedAt time.Time    `json:"submitted_at"`
	Snapshot    string       `json:"snapshot"`
	Flags       []Flag       `json:"flags"`
	Decision    *Decision    `json:"decision,omitempty"`
	SnapshotRef *SnapshotRef `json:"snapshot_taken,omitempty"`
	Files       []FileResult `json:"files,omitempty"`
	Steps       []StepResult `json:"steps,omitempty"`
	Error       string       `json:"error,omitempty"`
}

// Summary is one line of the approval queue.
type Summary struct {
	BatchID     string    `json:"batch_id"`
	ShortHash   string    `json:"short_hash"`
	Title       string    `json:"title"`
	Status      string    `json:"status"`
	Submitter   string    `json:"submitter"`
	SubmittedAt time.Time `json:"submitted_at"`
	Files       int       `json:"files"`
	Steps       int       `json:"steps"`
	Flags       []string  `json:"flags"`
	Snapshot    string    `json:"snapshot"`
	AutoRule    string    `json:"auto_approved_by,omitempty"`
}

// Service is the approval queue and the executor.
type Service struct {
	cfg Config
	d   Deps

	mu      sync.Mutex
	recs    map[string]*Record
	done    map[string]chan struct{}
	uses    map[string]int // standing rule id → uses
	running sync.WaitGroup
}

// usesFile persists standing-rule use counts.
const usesFile = "standing-uses.json"

// New loads the batches in d.Dir/batches. Batches that were approved or
// running when the gateway stopped are marked interrupted: an approval
// covers one execution, started by the gateway that took it.
func New(cfg Config, d Deps) (*Service, error) {
	if cfg.WaitTimeoutS <= 0 {
		cfg.WaitTimeoutS = defaultWaitS
	}
	if cfg.ShippedDir == "" {
		cfg.ShippedDir = ShippedRoot
	}
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	if d.Logf == nil {
		d.Logf = func(string, ...any) {}
	}
	if d.Audit == nil {
		d.Audit = func(string, string, any) {}
	}
	if d.Trust == nil {
		d.Trust = RootOwned(0)
	}
	if d.Steps == nil {
		d.Steps = DefaultStepRunner()
	}
	if d.Chown == nil {
		d.Chown = RealChown
	}
	if d.RootAgent == "" {
		d.RootAgent = "root"
	}
	if d.Snapshot == nil {
		if cfg.Snapshots == "off" {
			d.Snapshot = noSnapshot{}
		} else {
			d.Snapshot = DetectSnapshotter(context.Background(), RealSnapshotHost())
		}
	}
	if err := ValidateConfig(cfg, d.Now()); err != nil {
		return nil, err
	}
	s := &Service{cfg: cfg, d: d, recs: map[string]*Record{}, done: map[string]chan struct{}{}, uses: map[string]int{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// ValidateConfig checks the [run_as_root] section.
func ValidateConfig(cfg Config, now time.Time) error {
	if cfg.Snapshots != "" && cfg.Snapshots != "auto" && cfg.Snapshots != "off" {
		return fmt.Errorf("run_as_root.snapshots %q: want auto or off", cfg.Snapshots)
	}
	if cfg.ShippedDir != "" && !filepath.IsAbs(cfg.ShippedDir) {
		return fmt.Errorf("run_as_root.shipped_dir %q: want an absolute path", cfg.ShippedDir)
	}
	for id, r := range cfg.Standing {
		if err := r.validate(now); err != nil {
			return fmt.Errorf("run_as_root.standing.%s: %w", id, err)
		}
	}
	return nil
}

func (r StandingRule) validate(now time.Time) error {
	switch r.What {
	case "pkg.install", "pkg.remove", "unit.enable", "unit.restart", "user.add_to_group":
	default:
		return fmt.Errorf("what %q: want a typed operation (pkg.install, pkg.remove, unit.enable, unit.restart, user.add_to_group)", r.What)
	}
	if len(r.Names) == 0 {
		return errors.New("names: list what the rule covers")
	}
	for _, n := range r.Names {
		if _, err := path.Match(n, ""); err != nil || n == "" {
			return fmt.Errorf("names: bad pattern %q", n)
		}
	}
	if len(r.Who) == 0 {
		return errors.New("who: list the agents that may use the rule")
	}
	exp, err := time.Parse(time.RFC3339, r.Expires)
	if err != nil {
		return fmt.Errorf("expires %q: want an RFC 3339 time (required)", r.Expires)
	}
	if r.Enabled && exp.Sub(now) > maxRuleLife {
		return fmt.Errorf("expires %s: at most 90 days ahead", r.Expires)
	}
	if r.MaxUses <= 0 {
		return errors.New("max_uses: want a positive limit")
	}
	return nil
}

func (s *Service) batchesDir() string { return filepath.Join(s.d.Dir, batchesSubdir) }

func (s *Service) recordPath(r *Record) string {
	return filepath.Join(s.batchesDir(), r.Hash, recordPrefix+r.ID+".json")
}

func (s *Service) load() error {
	matches, err := filepath.Glob(filepath.Join(s.batchesDir(), "*", recordPrefix+"*.json"))
	if err != nil {
		return err
	}
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			return err
		}
		var r Record
		if err := json.Unmarshal(data, &r); err != nil {
			return fmt.Errorf("%s: %w", m, err)
		}
		s.recs[r.ID] = &r
		ch := make(chan struct{})
		if r.Status == StatusApproved || r.Status == StatusRunning {
			now := s.d.Now()
			r.Status, r.Error, r.FinishedAt = StatusInterrupted, "the gateway restarted before the batch finished; resubmit it", &now
			s.d.Audit("gateway", EvExecEnd, map[string]any{"batch_id": r.ID, "hash": r.Hash, "status": r.Status, "error": r.Error})
			if err := s.saveLocked(&r); err != nil {
				return err
			}
		}
		if Terminal(r.Status) {
			close(ch)
		}
		s.done[r.ID] = ch
	}
	data, err := os.ReadFile(filepath.Join(s.batchesDir(), usesFile))
	if err == nil {
		if err := json.Unmarshal(data, &s.uses); err != nil {
			return fmt.Errorf("%s: %w", usesFile, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Service) saveLocked(r *Record) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.recordPath(r), data)
}

func writeAtomic(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// Error is an error with a JSON-RPC code; the gateway passes it through.
type Error struct {
	Code    int
	Message string
}

func (e *Error) Error() string { return e.Message }

// Codes, the same numbers as the gateway's.
const (
	CodeInvalidParams = -32602
	CodeForbidden     = -32001
	CodeNotFound      = -32002
	CodeConflict      = -32003
)

func invalid(format string, args ...any) error {
	return &Error{Code: CodeInvalidParams, Message: fmt.Sprintf(format, args...)}
}

func (s *Service) forbid(p Principal, method, why string) error {
	s.d.Audit(p.Actor, "auth.deny", map[string]any{"method": method, "reason": why})
	return &Error{Code: CodeForbidden, Message: "permission denied: " + why}
}

// Methods are the gateway API methods the service answers.
var Methods = map[string]bool{
	"run_as_root.submit": true, "run_as_root.wait": true, "run_as_root.status": true, "run_as_root.cancel": true,
	"approval.list": true, "approval.show": true, "approval.decide": true,
}

// SubmitParams is run_as_root.submit.
type SubmitParams struct {
	Batch Batch `json:"batch"`
}

// SubmitResult is what submit returns.
type SubmitResult struct {
	BatchID   string `json:"batch_id"`
	Hash      string `json:"hash"`
	ShortHash string `json:"short_hash"`
	Status    string `json:"status"`
	Snapshot  string `json:"snapshot"`
	Flags     []Flag `json:"flags"`
	// AutoApprovedBy names the standing approval that approved it.
	AutoApprovedBy string `json:"auto_approved_by,omitempty"`
}

// IDParams names a batch.
type IDParams struct {
	BatchID string `json:"batch_id"`
}

// WaitParams is run_as_root.wait.
type WaitParams struct {
	BatchID  string `json:"batch_id"`
	TimeoutS int    `json:"timeout_s,omitempty"`
}

// ListParams is approval.list.
type ListParams struct {
	// All includes decided batches; by default only pending ones.
	All bool `json:"all,omitempty"`
}

// DecideParams is approval.decide.
type DecideParams struct {
	BatchID  string `json:"batch_id"`
	Decision string `json:"decision"` // approve or reject
	Comment  string `json:"comment,omitempty"`
	// Confirm is the batch's short hash, typed back by the human; needed to
	// approve a batch that edits the root agent's own instructions.
	Confirm string `json:"confirm,omitempty"`
}

// Detail is approval.show: the whole record.
type Detail = Record

// Call answers one API call. raw is the request's params; the gateway's
// own authentication fields in it are ignored here.
func (s *Service) Call(ctx context.Context, p Principal, method string, raw json.RawMessage) (any, error) {
	switch method {
	case "run_as_root.submit":
		var q SubmitParams
		if err := decode(raw, &q); err != nil {
			return nil, err
		}
		if p.Agent == "" || p.Agent != s.d.RootAgent {
			return nil, s.forbid(p, method, "only the root agent may submit run_as_root batches")
		}
		return s.Submit(p, q.Batch)
	case "run_as_root.wait", "run_as_root.status":
		var q WaitParams
		if err := decode(raw, &q); err != nil {
			return nil, err
		}
		if err := s.mayFollow(p, method, q.BatchID); err != nil {
			return nil, err
		}
		if method == "run_as_root.status" {
			q.TimeoutS = -1
		}
		return s.Wait(ctx, q.BatchID, q.TimeoutS)
	case "run_as_root.cancel":
		var q IDParams
		if err := decode(raw, &q); err != nil {
			return nil, err
		}
		if err := s.mayFollow(p, method, q.BatchID); err != nil {
			return nil, err
		}
		return s.Cancel(p, q.BatchID)
	case "approval.list":
		var q ListParams
		if err := decode(raw, &q); err != nil {
			return nil, err
		}
		if !p.Human {
			return nil, s.forbid(p, method, "only the human may read the approval queue")
		}
		return s.List(q.All), nil
	case "approval.show":
		var q IDParams
		if err := decode(raw, &q); err != nil {
			return nil, err
		}
		if !p.Human {
			return nil, s.forbid(p, method, "only the human may inspect batches in the approval queue")
		}
		return s.Show(q.BatchID)
	case "approval.decide":
		var q DecideParams
		if err := decode(raw, &q); err != nil {
			return nil, err
		}
		if !p.Human {
			return nil, s.forbid(p, method, "only the human may approve or reject batches")
		}
		return s.Decide(p, q)
	}
	return nil, &Error{Code: -32601, Message: fmt.Sprintf("no method %q", method)}
}

// decode strictly decodes params, ignoring the gateway's auth fields.
func decode(raw json.RawMessage, v any) error {
	m := map[string]json.RawMessage{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &m); err != nil {
			return invalid("params: %v", err)
		}
	}
	delete(m, "agent_token")
	delete(m, "on_behalf_of")
	clean, _ := json.Marshal(m)
	dec := json.NewDecoder(bytes.NewReader(clean))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalid("params: %v", err)
	}
	return nil
}

// mayFollow allows the human and the batch's submitter.
func (s *Service) mayFollow(p Principal, method, id string) error {
	s.mu.Lock()
	r := s.recs[id]
	s.mu.Unlock()
	if r == nil {
		if p.Human || p.Agent == s.d.RootAgent {
			return &Error{Code: CodeNotFound, Message: fmt.Sprintf("no batch %q", id)}
		}
		return s.forbid(p, method, "only the submitter and the human may follow a batch")
	}
	if p.Human || (p.Agent != "" && p.Agent == r.Submitter) {
		return nil
	}
	return s.forbid(p, method, "only the submitter and the human may follow a batch")
}

func newID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "b" + hex.EncodeToString(b)
}

// Submit validates, stages, binds and queues a batch (design §4.3). If a
// standing approval covers it, it is approved at once and starts running.
func (s *Service) Submit(p Principal, b Batch) (*SubmitResult, error) {
	files, attachments, err := b.normalize()
	if err != nil {
		return nil, invalid("batch: %v", err)
	}
	for i, f := range b.Files {
		if inside(f.Path, s.d.Dir) {
			return nil, invalid("batch: files[%d].path %s is in the gateway's own state", i, f.Path)
		}
	}
	bd, err := bind(&b, files, attachments, s.d.Trust)
	if err != nil {
		return nil, invalid("batch: %v", err)
	}
	hash, err := bd.Hash()
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	pending := 0
	for _, r := range s.recs {
		if r.Submitter != p.Agent || r.Status != StatusPending {
			continue
		}
		if r.Hash == hash {
			// The same batch is already waiting: don't queue it twice.
			return s.submitResult(r), nil
		}
		pending++
	}
	if pending >= maxPending {
		return nil, &Error{Code: CodeConflict, Message: fmt.Sprintf("%d batches are already pending; wait for the human", pending)}
	}
	if _, err := stage(s.batchesDir(), hash, bd, files, attachments); err != nil {
		return nil, fmt.Errorf("staging: %w", err)
	}
	now := s.d.Now()
	r := &Record{
		ID: newID(), Hash: hash, Status: StatusPending, Submitter: p.Agent, SubmittedAt: now, Bound: bd,
		Presentation: present(bd, hash, p.Agent, now, s.d.Snapshot.Kind(), files, attachments, s.cfg.ShippedDir),
	}
	if err := s.saveLocked(r); err != nil {
		return nil, err
	}
	s.recs[r.ID] = r
	s.done[r.ID] = make(chan struct{})
	// The full batch as submitted, content included, plus what silo
	// computed of it.
	s.d.Audit(p.Actor, EvSubmit, map[string]any{
		"batch_id": r.ID, "hash": hash, "batch": b, "bound": bd, "flags": r.Presentation.Flags, "snapshot": r.Presentation.Snapshot,
	})
	if id := s.matchStandingLocked(r); id != "" {
		s.uses[id]++
		if err := s.saveUsesLocked(); err != nil {
			s.d.Logf("saving standing approval uses: %v", err)
		}
		r.Status = StatusApproved
		r.Decision = &Decision{Decision: "approve", By: "standing:" + id, At: now, Rule: id}
		s.d.Audit("gateway", EvStandingUse, map[string]any{"batch_id": r.ID, "hash": hash, "rule": id, "uses": s.uses[id], "max_uses": s.cfg.Standing[id].MaxUses})
		if err := s.saveLocked(r); err != nil {
			return nil, err
		}
		s.startLocked(r)
	}
	s.pruneLocked()
	return s.submitResult(r), nil
}

func inside(p, dir string) bool {
	if dir == "" {
		return false
	}
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func (s *Service) submitResult(r *Record) *SubmitResult {
	res := &SubmitResult{BatchID: r.ID, Hash: r.Hash, ShortHash: r.Hash[:16], Status: r.Status,
		Snapshot: r.Presentation.Snapshot, Flags: r.Presentation.Flags}
	if r.Decision != nil {
		res.AutoApprovedBy = r.Decision.Rule
	}
	return res
}

func (s *Service) saveUsesLocked() error {
	data, err := json.Marshal(s.uses)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.batchesDir(), usesFile), data)
}

// matchStandingLocked returns the first (by id) enabled, unexpired,
// unexhausted standing approval that covers every step of r, or "".
// Batches that write files, carry attachments, or raise a blocking flag
// never match (design §7.5).
func (s *Service) matchStandingLocked(r *Record) string {
	bd := r.Bound
	if len(bd.Files) > 0 || len(bd.Attachments) > 0 || len(bd.Steps) == 0 {
		return ""
	}
	for _, f := range r.Presentation.Flags {
		if f.Blocking {
			return ""
		}
	}
	ids := make([]string, 0, len(s.cfg.Standing))
	for id := range s.cfg.Standing {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	now := s.d.Now()
	for _, id := range ids {
		rule := s.cfg.Standing[id]
		exp, err := time.Parse(time.RFC3339, rule.Expires)
		if !rule.Enabled || err != nil || !now.Before(exp) || s.uses[id] >= rule.MaxUses || !contains(rule.Who, r.Submitter) {
			continue
		}
		if ruleCovers(rule, bd.Steps) {
			return id
		}
	}
	return ""
}

func ruleCovers(rule StandingRule, steps []BoundStep) bool {
	for _, st := range steps {
		if st.Op != rule.What || len(st.Env) > 0 || st.StdinAttachment != "" || st.Cwd != "/" {
			return false
		}
		var subjects []string
		switch st.Op {
		case "pkg.install", "pkg.remove":
			subjects = st.Names
		case "unit.enable", "unit.restart":
			subjects = []string{st.Unit}
		case "user.add_to_group":
			subjects = []string{st.Group}
		}
		for _, sub := range subjects {
			if !globAny(rule.Names, sub) {
				return false
			}
		}
	}
	return true
}

func globAny(patterns []string, s string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, s); ok {
			return true
		}
	}
	return false
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// pruneLocked forgets the oldest finished batches beyond maxRecords (their
// staging directories stay, for undo, until removed by hand).
func (s *Service) pruneLocked() {
	if len(s.recs) <= maxRecords {
		return
	}
	var done []*Record
	for _, r := range s.recs {
		if Terminal(r.Status) {
			done = append(done, r)
		}
	}
	sort.Slice(done, func(i, j int) bool { return done[i].SubmittedAt.Before(done[j].SubmittedAt) })
	for _, r := range done[:max(0, min(len(done), len(s.recs)-maxRecords))] {
		delete(s.recs, r.ID)
		delete(s.done, r.ID)
	}
}

func (s *Service) status(r *Record) *Status {
	return &Status{
		BatchID: r.ID, Hash: r.Hash, Title: r.Bound.Title, Status: r.Status, Submitter: r.Submitter, SubmittedAt: r.SubmittedAt,
		Snapshot: r.Presentation.Snapshot, Flags: r.Presentation.Flags, Decision: r.Decision, SnapshotRef: r.Snapshot,
		Files: r.Files, Steps: r.Steps, Error: r.Error,
	}
}

// Wait blocks until the batch is finished or timeoutS passes (the
// configured wait timeout if 0, not at all if negative), and returns its
// status.
func (s *Service) Wait(ctx context.Context, id string, timeoutS int) (*Status, error) {
	s.mu.Lock()
	r, ch := s.recs[id], s.done[id]
	s.mu.Unlock()
	if r == nil {
		return nil, &Error{Code: CodeNotFound, Message: fmt.Sprintf("no batch %q", id)}
	}
	if timeoutS == 0 || timeoutS > s.cfg.WaitTimeoutS {
		timeoutS = s.cfg.WaitTimeoutS
	}
	if timeoutS > 0 {
		t := time.NewTimer(time.Duration(timeoutS) * time.Second)
		defer t.Stop()
		select {
		case <-ch:
		case <-t.C:
		case <-ctx.Done():
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status(r), nil
}

// List returns the queue, oldest first: pending batches, or all.
func (s *Service) List(all bool) []Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Summary{}
	for _, r := range s.recs {
		if !all && r.Status != StatusPending {
			continue
		}
		sum := Summary{BatchID: r.ID, ShortHash: r.Hash[:16], Title: r.Bound.Title, Status: r.Status, Submitter: r.Submitter,
			SubmittedAt: r.SubmittedAt, Files: len(r.Bound.Files), Steps: len(r.Bound.Steps), Snapshot: r.Presentation.Snapshot, Flags: []string{}}
		for _, f := range r.Presentation.Flags {
			if !contains(sum.Flags, f.Code) {
				sum.Flags = append(sum.Flags, f.Code)
			}
		}
		if r.Decision != nil {
			sum.AutoRule = r.Decision.Rule
		}
		out = append(out, sum)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].SubmittedAt.Equal(out[j].SubmittedAt) {
			return out[i].SubmittedAt.Before(out[j].SubmittedAt)
		}
		return out[i].BatchID < out[j].BatchID
	})
	return out
}

// Show returns a batch's whole record, presentation included.
func (s *Service) Show(id string) (*Detail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.recs[id]
	if r == nil {
		return nil, &Error{Code: CodeNotFound, Message: fmt.Sprintf("no batch %q", id)}
	}
	cp := *r
	return &cp, nil
}

// Decide approves or rejects a pending batch. An approval is for this
// batch id and hash, once: the batch starts running and can never be
// approved again.
func (s *Service) Decide(p Principal, q DecideParams) (*Status, error) {
	if q.Decision != "approve" && q.Decision != "reject" {
		return nil, invalid("decision %q: want approve or reject", q.Decision)
	}
	if len(q.Comment) > 4096 {
		return nil, invalid("comment: at most 4096 bytes")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.recs[q.BatchID]
	if r == nil {
		return nil, &Error{Code: CodeNotFound, Message: fmt.Sprintf("no batch %q", q.BatchID)}
	}
	if r.Status != StatusPending {
		return nil, &Error{Code: CodeConflict, Message: fmt.Sprintf("batch %s is %s, not pending; an approval is used once", r.ID, r.Status)}
	}
	if q.Decision == "approve" && r.Presentation.NeedsHashConfirmation && !strings.EqualFold(q.Confirm, r.Hash[:16]) {
		return nil, invalid("batch %s %s: confirm it by giving its short hash %s", r.ID, RootInstructionsMessage, r.Hash[:16])
	}
	now := s.d.Now()
	r.Decision = &Decision{Decision: q.Decision, By: p.Actor, At: now, Comment: q.Comment}
	if q.Decision == "approve" {
		r.Status = StatusApproved
	} else {
		r.Status = StatusRejected
		r.FinishedAt = &now
	}
	s.d.Audit(p.Actor, EvDecide, map[string]any{"batch_id": r.ID, "hash": r.Hash, "decision": q.Decision, "comment": q.Comment})
	if err := s.saveLocked(r); err != nil {
		return nil, err
	}
	if r.Status == StatusApproved {
		s.startLocked(r)
	} else {
		close(s.done[r.ID])
	}
	return s.status(r), nil
}

// Cancel withdraws a pending batch.
func (s *Service) Cancel(p Principal, id string) (*Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.recs[id]
	if r == nil {
		return nil, &Error{Code: CodeNotFound, Message: fmt.Sprintf("no batch %q", id)}
	}
	if r.Status != StatusPending {
		return nil, &Error{Code: CodeConflict, Message: fmt.Sprintf("batch %s is %s; only a pending batch can be cancelled", id, r.Status)}
	}
	now := s.d.Now()
	r.Status, r.FinishedAt = StatusCancelled, &now
	r.Decision = &Decision{Decision: "cancel", By: p.Actor, At: now}
	s.d.Audit(p.Actor, EvCancel, map[string]any{"batch_id": r.ID, "hash": r.Hash})
	if err := s.saveLocked(r); err != nil {
		return nil, err
	}
	close(s.done[r.ID])
	return s.status(r), nil
}

// WaitIdle waits for every running batch to finish (for tests and
// shutdown).
func (s *Service) WaitIdle() { s.running.Wait() }

func (s *Service) startLocked(r *Record) {
	s.running.Add(1)
	go func() {
		defer s.running.Done()
		s.execute(r.ID)
	}()
}

// execute runs an approved batch: re-verify, snapshot, files, steps.
func (s *Service) execute(id string) {
	s.mu.Lock()
	r := s.recs[id]
	if r == nil || r.Status != StatusApproved {
		s.mu.Unlock()
		return
	}
	now := s.d.Now()
	r.Status, r.StartedAt = StatusRunning, &now
	if err := s.saveLocked(r); err != nil {
		s.d.Logf("batch %s: %v", id, err)
	}
	bd, hash := r.Bound, r.Hash
	s.mu.Unlock()

	dir := filepath.Join(s.batchesDir(), hash)
	finish := func(status, msg string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		t := s.d.Now()
		r.Status, r.Error, r.FinishedAt = status, msg, &t
		if err := s.saveLocked(r); err != nil {
			s.d.Logf("batch %s: %v", id, err)
		}
		s.d.Audit("gateway", EvExecEnd, map[string]any{"batch_id": id, "hash": hash, "status": status, "error": msg})
		close(s.done[id])
	}

	if err := verifyBound(dir, hash, bd, s.d.Trust); err != nil {
		s.d.Audit("gateway", EvExecRefused, map[string]any{"batch_id": id, "hash": hash, "error": err.Error()})
		finish(StatusRefused, "refused to run: "+err.Error())
		return
	}
	s.d.Audit("gateway", EvExecStart, map[string]any{"batch_id": id, "hash": hash})

	snap := &SnapshotRef{Kind: s.d.Snapshot.Kind()}
	if snap.Kind == SnapNone {
		snap.Note = NoSnapshotMessage
	} else {
		snapID, err := s.d.Snapshot.Take(context.Background(), id)
		snap.ID = snapID
		if err != nil {
			snap.Error = err.Error()
		}
	}
	s.d.Audit("gateway", EvSnapshot, map[string]any{"batch_id": id, "hash": hash, "kind": snap.Kind, "id": snap.ID, "note": snap.Note, "error": snap.Error})
	s.mu.Lock()
	r.Snapshot = snap
	s.mu.Unlock()
	if snap.Error != "" {
		// The human approved a batch that said a snapshot would be taken.
		finish(StatusFailed, "snapshot failed, nothing was changed: "+snap.Error)
		return
	}

	undo := filepath.Join(dir, "undo-"+id)
	for i, f := range bd.Files {
		res, err := applyFile(f, filepath.Join(dir, filesDir, strconv.Itoa(i)), filepath.Join(undo, strconv.Itoa(i)), s.d.Chown)
		if err != nil {
			res.Error = err.Error()
		}
		s.d.Audit("gateway", EvExecFile, map[string]any{"batch_id": id, "hash": hash, "index": i, "path": f.Path, "delete": f.Delete, "sha256": f.SHA256, "result": res})
		s.mu.Lock()
		r.Files = append(r.Files, res)
		s.mu.Unlock()
		if err != nil {
			finish(StatusFailed, fmt.Sprintf("files[%d] %s: %v", i, f.Path, err))
			return
		}
	}

	for i, st := range bd.Steps {
		spec := stepSpec(dir, id, i, st)
		start := time.Now()
		out := s.d.Steps.RunStep(context.Background(), spec)
		res := StepResult{Index: i, Argv: spec.Argv, Exit: out.Exit, Stdout: string(out.Stdout), Stderr: string(out.Stderr),
			Duration: time.Since(start).Round(time.Millisecond).Seconds()}
		if out.Err != nil {
			res.Error = out.Err.Error()
		}
		s.d.Audit("gateway", EvExecStep, map[string]any{"batch_id": id, "hash": hash, "index": i, "unit": spec.Unit,
			"exe": spec.Path, "argv": spec.Argv, "exit": res.Exit, "error": res.Error, "stdout": res.Stdout, "stderr": res.Stderr, "duration_s": res.Duration})
		s.mu.Lock()
		r.Steps = append(r.Steps, res)
		s.mu.Unlock()
		if out.Err != nil || out.Exit != 0 {
			msg := fmt.Sprintf("steps[%d] exited %d", i, out.Exit)
			if out.Err != nil {
				msg = fmt.Sprintf("steps[%d]: %v", i, out.Err)
			}
			finish(StatusFailed, msg+"; later steps were not run")
			return
		}
	}
	finish(StatusSucceeded, "")
}
