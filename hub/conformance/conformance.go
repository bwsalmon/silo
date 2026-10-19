// Package conformance is the task-interface conformance kit: it proves
// that a Grain, stock or customised or different code altogether,
// speaks the task interface of docs/hub-interface.md
// (docs/conformance.md).
//
// The kit runs a real hub in this process, on a database of its own,
// makes a team owning the repositories it files in, and mints a node
// token for the Grain under test, which it hands to Config.Start. Once
// the Grain has registered it drives scripted scenarios through the
// hub's public API only -- the directory, the backlog, the event and
// artifact paths, as a human or a custom UI would -- and checks what
// the Grain does. Every task it files is a conformance workload
// (package workload), so nothing depends on a model's behaviour.
//
// A scenario for an optional part of the interface the Grain does not
// advertise in its hello's features is skipped and reported as such,
// not failed.
package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/bwsalmon/silo/events"
	"github.com/bwsalmon/silo/hub/conformance/workload"
	"github.com/bwsalmon/silo/hub/live"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/tasks"
	"github.com/bwsalmon/silo/internal/tunnel/mux"
)

// Features a Grain lists in its hello that the kit reads.
const (
	// FeatureInteractive is the interface's interactive sessions.
	FeatureInteractive = "interactive"
	// FeatureApprovals says the Grain sends approval_request events and
	// takes the approve answers. The hub does not need it said: the
	// kit does, to tell a Grain without approvals from a broken one.
	FeatureApprovals = "approvals"
	// FeatureFiling says the Grain files tasks upward and takes
	// dependency_done (docs/hub-interface.md, filing a task). The hub
	// does not read it; the kit does.
	FeatureFiling = "filing"
)

// Defaults for Config.
const (
	DefaultNode        = "conformance-grain"
	DefaultRepo        = "conformance/allowed"
	DefaultOutsideRepo = "conformance/outside"
	DefaultMaxSlots    = 8
)

// Env is what the Grain under test is given to connect with.
type Env struct {
	// Hub is the hub's base URL, http://...; Token the node token
	// minted for Node.
	Hub, Token, Node string
	// Repo is the repository the kit's tasks are in, which the Grain's
	// local policy allows; OutsideRepo one it registers for but its
	// policy refuses.
	Repo, OutsideRepo string
}

// Config shapes a run of the kit.
type Config struct {
	// Addr is where the kit's hub listens: 127.0.0.1 on a free port
	// when empty.
	Addr string
	// Dir holds the hub's database; a temporary directory when empty.
	Dir string
	// Node is the node ID the token is minted for: DefaultNode when
	// empty.
	Node string
	// Repo and OutsideRepo are Env's: DefaultRepo when Repo is empty.
	// The refusal scenario is skipped when OutsideRepo is "-".
	Repo, OutsideRepo string
	// Heartbeat is the interval the hub asks for: a second when zero.
	Heartbeat time.Duration
	// Register is how long the Grain has to register: two minutes when
	// zero.
	Register time.Duration
	// Timeout bounds each wait inside a scenario: 90s when zero, which
	// is past the hub's minute for an answer to an offer.
	Timeout time.Duration
	// MaxSlots bounds the capacity scenario: a Grain with more slots
	// is not filled, and that scenario is skipped. DefaultMaxSlots
	// when zero.
	MaxSlots int
	// Start starts the Grain under test, pointed at env, and returns:
	// the Grain runs until the caller stops it, after Run returns.
	Start func(env Env) error
	// Logf receives the hub's log and the kit's progress, when set.
	Logf func(format string, args ...any)
	// Only, when set, runs only the named scenarios (and the
	// registration they all need).
	Only []string
}

// Status is a scenario's verdict.
type Status string

const (
	Pass Status = "pass"
	Fail Status = "fail"
	Skip Status = "skip"
)

// Verdict is one scenario's outcome.
type Verdict struct {
	Name     string        `json:"name"`
	Status   Status        `json:"status"`
	Detail   string        `json:"detail,omitempty"`
	Duration time.Duration `json:"duration_ns"`
}

// Report is a run's outcome: the Grain as it registered, and a verdict
// per scenario.
type Report struct {
	Node      *registry.Node `json:"node,omitempty"`
	StartedAt time.Time      `json:"started_at"`
	Passed    bool           `json:"passed"`
	Scenarios []Verdict      `json:"scenarios"`
}

// Scenarios are the scenarios' names, in the order they run.
var Scenarios = []string{
	"register", "heartbeat", "refuse", "run", "events", "view",
	"unlisted", "capacity", "cancel", "approve", "interactive", "restart", "filing",
}

// kit is one run.
type kit struct {
	cfg   Config
	h     *hub
	entry registry.Entry
	logf  func(string, ...any)

	run  string       // the run scenario's task
	runW *watcher     // its events
	hold []tasks.Task // the capacity scenario's tasks, for cancel
	more tasks.Task   // the capacity scenario's task beyond the slots
	ws   []*watcher
}

// errSkip ends a scenario as skipped, saying why.
type errSkip struct{ why string }

func (e errSkip) Error() string { return e.why }

func skip(format string, args ...any) error { return errSkip{fmt.Sprintf(format, args...)} }

// Run runs the kit: a hub, the Grain under test started against it,
// and every scenario. Its error is a failure of the kit itself; the
// Grain's failures are the Report's.
func Run(ctx context.Context, cfg Config) (*Report, error) {
	if cfg.Start == nil {
		return nil, errors.New("conformance: no Start")
	}
	if cfg.Node == "" {
		cfg.Node = DefaultNode
	}
	if cfg.Repo == "" {
		cfg.Repo = DefaultRepo
	}
	if cfg.OutsideRepo == "" {
		cfg.OutsideRepo = DefaultOutsideRepo
	}
	if cfg.OutsideRepo == "-" {
		cfg.OutsideRepo = ""
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = time.Second
	}
	if cfg.Register <= 0 {
		cfg.Register = 2 * time.Minute
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 90 * time.Second
	}
	if cfg.MaxSlots <= 0 {
		cfg.MaxSlots = DefaultMaxSlots
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	dir := cfg.Dir
	if dir == "" {
		d, err := os.MkdirTemp("", "silo-conformance-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(d)
		dir = d
	}
	h, err := startHub(dir, cfg.Addr, cfg.Heartbeat, logf)
	if err != nil {
		return nil, fmt.Errorf("conformance: starting the hub: %w", err)
	}
	defer h.Close()
	repos := []string{cfg.Repo}
	if cfg.OutsideRepo != "" {
		repos = append(repos, cfg.OutsideRepo)
	}
	token, err := h.setup(ctx, cfg.Node, repos)
	if err != nil {
		return nil, fmt.Errorf("conformance: setting the hub up: %w", err)
	}
	if err := cfg.Start(Env{Hub: h.url, Token: token, Node: cfg.Node, Repo: cfg.Repo, OutsideRepo: cfg.OutsideRepo}); err != nil {
		return nil, fmt.Errorf("conformance: starting the grain: %w", err)
	}
	k := &kit{cfg: cfg, h: h, logf: logf}
	defer func() {
		for _, w := range k.ws {
			w.Stop()
		}
	}()
	rep := &Report{StartedAt: time.Now().UTC(), Passed: true}
	steps := map[string]func(context.Context) (string, error){
		"register": k.register, "heartbeat": k.heartbeat, "refuse": k.refuse,
		"run": k.runToResult, "events": k.events, "view": k.view,
		"unlisted": k.unlisted,
		"capacity": k.capacity, "cancel": k.cancel, "approve": k.approve,
		"interactive": k.interactive, "restart": k.restart, "filing": k.filing,
	}
	registered := false
	for _, name := range Scenarios {
		if name != "register" && len(cfg.Only) > 0 && !slices.Contains(cfg.Only, name) {
			continue
		}
		v := Verdict{Name: name}
		start := time.Now()
		var detail string
		var err error
		if name != "register" && !registered {
			err = errors.New("the grain never registered")
		} else {
			logf("conformance: scenario %s", name)
			detail, err = steps[name](ctx)
		}
		v.Duration = time.Since(start).Round(time.Millisecond)
		var s errSkip
		switch {
		case errors.As(err, &s):
			v.Status, v.Detail = Skip, s.why
		case err != nil:
			v.Status, v.Detail = Fail, err.Error()
			rep.Passed = false
		default:
			v.Status, v.Detail = Pass, detail
		}
		if name == "register" {
			registered = err == nil
			if registered {
				n := k.entry.Node
				rep.Node = &n
			}
		}
		logf("conformance: %s: %s %s", name, v.Status, v.Detail)
		rep.Scenarios = append(rep.Scenarios, v)
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
	}
	return rep, nil
}

// eventually polls cond until it is true, fails for good, or d passes.
func eventually(ctx context.Context, d time.Duration, what string, cond func() (bool, error)) error {
	deadline := time.Now().Add(d)
	var last error
	for {
		ok, err := cond()
		if ok {
			return nil
		}
		var fatal fatalError
		if errors.As(err, &fatal) {
			return fatal.err
		}
		if err != nil {
			last = err
		}
		if time.Now().After(deadline) {
			if last != nil {
				return fmt.Errorf("timed out after %s waiting for %s (last error: %v)", d, what, last)
			}
			return fmt.Errorf("timed out after %s waiting for %s", d, what)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// fatalError ends an eventually at once.
type fatalError struct{ err error }

func (f fatalError) Error() string { return f.err.Error() }

func fatal(format string, args ...any) error { return fatalError{fmt.Errorf(format, args...)} }

// submit files workload work, and follows its events from the start.
func (k *kit) submit(ctx context.Context, repo, kind, work string) (tasks.Task, *watcher, error) {
	return k.submitAs(ctx, operator, repo, kind, work, workload.Prompt(work))
}

// submitAs files a conformance task with prompt as user, and follows
// its events from the start.
func (k *kit) submitAs(ctx context.Context, user, repo, kind, work, prompt string) (tasks.Task, *watcher, error) {
	t, err := k.h.submit(ctx, user, repo, kind, "silo conformance: "+work, prompt)
	if err != nil {
		return t, nil, err
	}
	w := k.h.watch(t.ID)
	k.ws = append(k.ws, w)
	return t, w, nil
}

// await waits for task id to reach state, on the Grain under test.
func (k *kit) await(ctx context.Context, id string, state tasks.State) (tasks.Task, error) {
	var t tasks.Task
	err := eventually(ctx, k.cfg.Timeout, fmt.Sprintf("task %s to be %s", id, state), func() (bool, error) {
		var err error
		if t, err = k.h.task(ctx, id); err != nil {
			return false, err
		}
		switch {
		case t.State == state:
			if t.Node != "" && t.Node != k.cfg.Node {
				return false, fatal("task %s is %s on node %q, not the grain under test", id, t.State, t.Node)
			}
			return true, nil
		case t.State.Terminal():
			return false, fatal("task %s ended %s (%s), want %s", id, t.State, t.Reason, state)
		}
		return false, nil
	})
	return t, err
}

// free waits for the Grain's heartbeat to say it has n slots free.
func (k *kit) free(ctx context.Context, n int, d time.Duration) error {
	last := -1
	err := eventually(ctx, d, fmt.Sprintf("the grain to report %d free slots", n), func() (bool, error) {
		e, ok, err := k.h.node(ctx, k.cfg.Node)
		if err != nil || !ok {
			return false, err
		}
		last = e.Free
		return e.Status == registry.Online && e.Free == n, nil
	})
	if err != nil && last >= 0 {
		err = fmt.Errorf("%w; it last reported %d", err, last)
	}
	return err
}

// beats is n heartbeat intervals, with three seconds' slack.
func (k *kit) beats(n int) time.Duration { return time.Duration(n)*k.cfg.Heartbeat + 3*time.Second }

func (k *kit) register(ctx context.Context) (string, error) {
	err := eventually(ctx, k.cfg.Register, "the grain to register", func() (bool, error) {
		e, ok, err := k.h.node(ctx, k.cfg.Node)
		if ok && e.Status == registry.Online {
			k.entry = e
		}
		return ok && e.Status == registry.Online, err
	})
	if err != nil {
		return "", err
	}
	e := k.entry
	var problems []string
	if e.Slots < 1 {
		problems = append(problems, fmt.Sprintf("slots %d, want at least 1", e.Slots))
	}
	if !slices.Contains(e.Repos, k.cfg.Repo) {
		problems = append(problems, fmt.Sprintf("repos %v do not include %s, the kit's repository", e.Repos, k.cfg.Repo))
	}
	if e.Owner != operator {
		problems = append(problems, fmt.Sprintf("owner %q, want the token's %q", e.Owner, operator))
	}
	if len(problems) > 0 {
		return "", errors.New(strings.Join(problems, "; "))
	}
	return fmt.Sprintf("node %s, version %q, %d slots, repos %v, features %v", e.ID, e.Version, e.Slots, e.Repos, e.Features), nil
}

func (k *kit) heartbeat(ctx context.Context) (string, error) {
	first := k.entry.LastSeen
	var e registry.Entry
	err := eventually(ctx, k.beats(3), "a heartbeat", func() (bool, error) {
		var ok bool
		var err error
		e, ok, err = k.h.node(ctx, k.cfg.Node)
		if err != nil || !ok {
			return false, err
		}
		if e.Status != registry.Online {
			return false, fatal("the grain went %s", e.Status)
		}
		return e.LastSeen.After(first), nil
	})
	if err != nil {
		return "", err
	}
	// Idle, every slot is free.
	if err := k.free(ctx, e.Slots, k.beats(3)); err != nil {
		return "", fmt.Errorf("idle: %w", err)
	}
	return fmt.Sprintf("heartbeats every %s, %d of %d slots free while idle", k.cfg.Heartbeat, e.Slots, e.Slots), nil
}

func (k *kit) refuse(ctx context.Context) (string, error) {
	if k.cfg.OutsideRepo == "" {
		return "", skip("no repository outside the grain's policy was named")
	}
	if !slices.Contains(k.entry.Repos, k.cfg.OutsideRepo) {
		return "", skip("the grain does not register for %s, so the hub never offers it a task there", k.cfg.OutsideRepo)
	}
	t, _, err := k.submit(ctx, k.cfg.OutsideRepo, "", workload.Run)
	if err != nil {
		return "", err
	}
	defer k.h.cancel(context.WithoutCancel(ctx), t.ID)
	err = eventually(ctx, k.cfg.Timeout, "the grain to refuse the offer", func() (bool, error) {
		t, err := k.h.task(ctx, t.ID)
		if err != nil {
			return false, err
		}
		if t.NodeTask != "" || t.State == tasks.Running || t.State == tasks.Done {
			return false, fatal("the grain accepted task %s in %s, outside its policy (node task %q, %s)", t.ID, k.cfg.OutsideRepo, t.NodeTask, t.State)
		}
		return slices.Contains(t.Excluded, k.cfg.Node), nil
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("refused task %s in %s", t.ID, k.cfg.OutsideRepo), nil
}

func (k *kit) runToResult(ctx context.Context) (string, error) {
	t, w, err := k.submit(ctx, k.cfg.Repo, "", workload.Run)
	if err != nil {
		return "", err
	}
	k.run, k.runW = t.ID, w
	if t, err = k.await(ctx, t.ID, tasks.Done); err != nil {
		return "", err
	}
	if t.NodeTask == "" {
		return "", fmt.Errorf("task %s is done with no node task: the grain never accepted it", t.ID)
	}
	if _, ok := w.Await(k.cfg.Timeout, func(e live.Event) bool { return e.Type == events.TypeResult }); !ok {
		return "", fmt.Errorf("task %s is done but no result event was streamed", t.ID)
	}
	return fmt.Sprintf("task %s accepted as %s and done", t.ID, t.NodeTask), nil
}

// runEvents is the run scenario's task's events, once its result is in.
func (k *kit) runEvents() ([]live.Event, error) {
	if k.run == "" {
		return nil, skip("the run scenario did not run")
	}
	evs := k.runW.Events()
	if len(evs) == 0 {
		return nil, fmt.Errorf("no events were streamed for task %s", k.run)
	}
	return evs, nil
}

func (k *kit) events(ctx context.Context) (string, error) {
	evs, err := k.runEvents()
	if err != nil {
		return "", err
	}
	var types []string
	for i, e := range evs {
		types = append(types, e.Type)
		if i > 0 && e.Seq <= evs[i-1].Seq {
			return "", fmt.Errorf("event %d (%s) has seq %d after seq %d: seq must increase", i, e.Type, e.Seq, evs[i-1].Seq)
		}
	}
	for _, want := range []string{events.TypeStatus, live.TypeArtifact, events.TypeResult} {
		if !slices.Contains(types, want) {
			return "", fmt.Errorf("no %s event among %v", want, types)
		}
	}
	if types[len(types)-1] != events.TypeResult {
		return "", fmt.Errorf("events %v do not end with the result", types)
	}
	return fmt.Sprintf("%d events, seq %d to %d: %v", len(evs), evs[0].Seq, evs[len(evs)-1].Seq, types), nil
}

func (k *kit) view(ctx context.Context) (string, error) {
	evs, err := k.runEvents()
	if err != nil {
		return "", err
	}
	var art *live.Artifact
	for _, e := range evs {
		if e.Type == live.TypeArtifact && e.Artifact != nil && e.Artifact.Kind == "diff" {
			art = e.Artifact
		}
	}
	if art == nil || art.Ref == "" {
		return "", errors.New("the run streamed no diff artifact with a ref")
	}
	b, err := k.h.view(ctx, k.run, art.Ref)
	if err != nil {
		return "", fmt.Errorf("viewing %s: %w", art.Ref, err)
	}
	if string(b) != workload.Diff {
		return "", fmt.Errorf("the view of %s served %d bytes that are not the conformance diff: %q", art.Ref, len(b), truncate(string(b)))
	}
	if art.Size != 0 && art.Size != int64(len(b)) {
		return "", fmt.Errorf("the artifact event says %d bytes, its view served %d", art.Size, len(b))
	}
	return fmt.Sprintf("diff by ref %q, %d bytes served on the view stream", art.Ref, len(b)), nil
}

// unlisted asks the Grain itself, on view streams the kit opens past
// the hub's check of listed refs (SA1-M3), for what the hub would never
// ask: a conforming Grain serves a view only of a ref it listed for
// that task (INV-11). A stream closed unanswered counts as a refusal.
func (k *kit) unlisted(ctx context.Context) (string, error) {
	evs, err := k.runEvents()
	if err != nil {
		return "", err
	}
	var ref string
	for _, e := range evs {
		if e.Type == live.TypeArtifact && e.Artifact != nil && e.Artifact.Ref != "" {
			ref = e.Artifact.Ref
		}
	}
	if ref == "" {
		return "", errors.New("the run listed no artifact ref")
	}
	run, err := k.h.task(ctx, k.run)
	if err != nil {
		return "", err
	}
	// A task of its own, listing nothing, to ask the run's ref under.
	other, _, err := k.submit(ctx, k.cfg.Repo, "", workload.Hold)
	if err != nil {
		return "", err
	}
	if other, err = k.await(ctx, other.ID, tasks.Running); err != nil {
		k.h.cancel(context.WithoutCancel(ctx), other.ID)
		return "", err
	}
	defer func() {
		k.h.cancel(context.WithoutCancel(ctx), other.ID)
		k.free(context.WithoutCancel(ctx), k.entry.Slots, k.cfg.Timeout)
	}()

	b, served, err := k.viewNode(ctx, live.ViewMessage{Type: live.TypeView, Task: run.ID, NodeTask: run.NodeTask, Ref: ref})
	switch {
	case err != nil:
		return "", fmt.Errorf("the listed ref %q: %w", ref, err)
	case !served:
		return "", fmt.Errorf("the listed ref %q was not served on a view stream the kit opened, so the refusals prove nothing", ref)
	case len(b) == 0:
		return "", fmt.Errorf("the listed ref %q was served with no bytes", ref)
	}
	refuse := []struct {
		what string
		m    live.ViewMessage
	}{
		{"a ref never listed", live.ViewMessage{Task: run.ID, NodeTask: run.NodeTask, Ref: "diff-999"}},
		{"a traversal ref", live.ViewMessage{Task: run.ID, NodeTask: run.NodeTask, Ref: "../../.git-credentials"}},
		{"a traversal past a listed ref", live.ViewMessage{Task: run.ID, NodeTask: run.NodeTask, Ref: ref + "/../../.git-credentials"}},
		{"an absolute path", live.ViewMessage{Task: run.ID, NodeTask: run.NodeTask, Ref: "/etc/passwd"}},
		{"another task's listed ref", live.ViewMessage{Task: other.ID, NodeTask: other.NodeTask, Ref: ref}},
		{"a listed ref under another task's node_task", live.ViewMessage{Task: run.ID, NodeTask: other.NodeTask, Ref: ref}},
	}
	closed := 0
	for _, c := range refuse {
		c.m.Type = live.TypeView
		b, served, err := k.viewNode(ctx, c.m)
		switch {
		case err != nil:
			return "", fmt.Errorf("%s (task %s, node_task %q, ref %q): %w", c.what, c.m.Task, c.m.NodeTask, c.m.Ref, err)
		case served:
			return "", fmt.Errorf("%s (task %s, node_task %q, ref %q) was served, %d bytes: a Grain serves only a ref it listed for that task", c.what, c.m.Task, c.m.NodeTask, c.m.Ref, len(b))
		case b == nil:
			closed++
		}
	}
	detail := fmt.Sprintf("listed ref %q served, %d bytes; refused %d unlisted, traversal and cross-task refs", ref, len(b), len(refuse))
	if closed > 0 {
		detail += fmt.Sprintf(" (%d by closing the stream unanswered)", closed)
	}
	return detail, nil
}

// viewNode opens a view stream to the Grain itself and sends it m. It
// says whether the Grain answered ok, and the bytes after the answer;
// those are nil when it refused by closing the stream unanswered. An
// error is an answer outside the interface: no answer before the
// timeout, a line that is not one, or bytes after an error.
func (k *kit) viewNode(ctx context.Context, m live.ViewMessage) ([]byte, bool, error) {
	c, err := k.h.open(ctx, k.cfg.Node)
	if err != nil {
		return nil, false, fmt.Errorf("opening a stream to the grain: %w", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(k.cfg.Timeout))
	line, _ := json.Marshal(m)
	if _, err := c.Write(append(line, '\n')); err != nil {
		// Closed before it was asked anything: refused, unread.
		return nil, false, nil
	}
	all, err := io.ReadAll(c)
	if err != nil && !isClosed(err) {
		return nil, false, fmt.Errorf("reading the answer: %w", err)
	}
	if len(all) == 0 {
		return nil, false, nil
	}
	head, rest, ok := bytes.Cut(all, []byte("\n"))
	if !ok {
		return nil, false, fmt.Errorf("the answer %q is not a line", truncate(string(all)))
	}
	var ans live.ViewMessage
	if err := json.Unmarshal(head, &ans); err != nil {
		return nil, false, fmt.Errorf("the answer %q: %v", truncate(string(head)), err)
	}
	switch ans.Type {
	case live.TypeOK:
		return rest, true, nil
	case live.TypeError:
		if len(rest) > 0 {
			return nil, false, fmt.Errorf("answered an error, then %d bytes", len(rest))
		}
		return []byte{}, false, nil
	}
	return nil, false, fmt.Errorf("answered %q, want ok or error", ans.Type)
}

// isClosed is a read error that is the stream's end rather than a
// failure: the Grain resetting it rather than closing it.
func isClosed(err error) bool {
	return errors.Is(err, mux.ErrStreamReset) || errors.Is(err, mux.ErrStreamClosed)
}

func truncate(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

func (k *kit) capacity(ctx context.Context) (string, error) {
	slots := k.entry.Slots
	if slots > k.cfg.MaxSlots {
		return "", skip("the grain has %d slots, more than the %d the kit fills", slots, k.cfg.MaxSlots)
	}
	for range slots {
		t, _, err := k.submit(ctx, k.cfg.Repo, "", workload.Hold)
		if err != nil {
			return "", err
		}
		k.hold = append(k.hold, t)
	}
	for _, t := range k.hold {
		if _, err := k.await(ctx, t.ID, tasks.Running); err != nil {
			return "", err
		}
	}
	if err := k.free(ctx, 0, k.beats(3)); err != nil {
		return "", fmt.Errorf("with every slot running: %w", err)
	}
	more, _, err := k.submit(ctx, k.cfg.Repo, "", workload.Hold)
	if err != nil {
		return "", err
	}
	k.more = more
	// The hub offers it nothing while it says it has no free slot.
	time.Sleep(k.beats(3))
	if more, err = k.h.task(ctx, more.ID); err != nil {
		return "", err
	}
	if more.State == tasks.Running || more.NodeTask != "" {
		return "", fmt.Errorf("task %s is %s on %d slots already full", more.ID, more.State, slots)
	}
	return fmt.Sprintf("%d tasks running on %d slots, free 0, one more left %s", slots, slots, more.State), nil
}

func (k *kit) cancel(ctx context.Context) (string, error) {
	if len(k.hold) == 0 {
		// The capacity scenario was skipped: one held task will do.
		t, _, err := k.submit(ctx, k.cfg.Repo, "", workload.Hold)
		if err != nil {
			return "", err
		}
		if _, err := k.await(ctx, t.ID, tasks.Running); err != nil {
			return "", err
		}
		k.hold = append(k.hold, t)
	}
	first := k.hold[0]
	if err := k.h.cancel(ctx, first.ID); err != nil {
		return "", err
	}
	if err := eventually(ctx, k.cfg.Timeout, "the cancel to be delivered", func() (bool, error) {
		t, err := k.h.task(ctx, first.ID)
		return err == nil && t.State == tasks.Cancelled && !t.CancelPending, err
	}); err != nil {
		return "", err
	}
	detail := fmt.Sprintf("task %s cancelled", first.ID)
	if k.more.ID != "" {
		// Its slot, freed, takes the task that was waiting.
		if _, err := k.await(ctx, k.more.ID, tasks.Running); err != nil {
			return "", fmt.Errorf("the cancelled task's slot was not freed: %w", err)
		}
		detail += fmt.Sprintf(", and its slot took task %s", k.more.ID)
		k.hold = append(k.hold, k.more)
	}
	for _, t := range k.hold[1:] {
		if err := k.h.cancel(ctx, t.ID); err != nil {
			return "", err
		}
	}
	if err := k.free(ctx, k.entry.Slots, k.cfg.Timeout); err != nil {
		return "", fmt.Errorf("after cancelling every task: %w", err)
	}
	k.hold = nil
	return detail + "; every slot free again", nil
}

func (k *kit) approve(ctx context.Context) (string, error) {
	if !slices.Contains(k.entry.Features, FeatureApprovals) {
		return "", skip("the grain does not list the %q feature", FeatureApprovals)
	}
	var said []string
	for _, c := range []struct {
		decision string
		want     tasks.State
	}{{"allow", tasks.Done}, {"deny", tasks.Failed}} {
		t, w, err := k.submit(ctx, k.cfg.Repo, "", workload.Approve)
		if err != nil {
			return "", err
		}
		// The hub answers only a running task's requests, and a Grain may
		// ask before the hub has its accept.
		if _, err := k.await(ctx, t.ID, tasks.Running); err != nil {
			return "", err
		}
		if _, ok := w.Await(k.cfg.Timeout, func(e live.Event) bool {
			return e.Type == events.TypeApprovalRequest && e.ApprovalRequest != nil && e.ApprovalRequest.ID == workload.ApprovalID
		}); !ok {
			k.h.cancel(ctx, t.ID)
			return "", fmt.Errorf("task %s sent no approval_request %q", t.ID, workload.ApprovalID)
		}
		if err := k.h.call(ctx, "POST", tasks.TasksPath+"/"+t.ID+"/approvals/"+workload.ApprovalID, reviewer,
			map[string]string{"decision": c.decision, "reason": "conformance"}, nil); err != nil {
			k.h.cancel(ctx, t.ID)
			return "", err
		}
		if _, err := k.await(ctx, t.ID, c.want); err != nil {
			k.h.cancel(ctx, t.ID)
			return "", fmt.Errorf("answered %s: %w", c.decision, err)
		}
		said = append(said, fmt.Sprintf("%s ended %s", c.decision, c.want))
	}
	return strings.Join(said, ", "), nil
}

func (k *kit) interactive(ctx context.Context) (string, error) {
	if !slices.Contains(k.entry.Features, FeatureInteractive) {
		return "", skip("the grain does not list the %q feature", FeatureInteractive)
	}
	t, w, err := k.submit(ctx, k.cfg.Repo, "interactive", workload.Interactive)
	if err != nil {
		return "", err
	}
	defer k.h.cancel(context.WithoutCancel(ctx), t.ID)
	if _, ok := w.Await(k.cfg.Timeout, func(e live.Event) bool {
		return e.Type == events.TypeAwaitingReply && e.AwaitingReply != nil && e.AwaitingReply.State == events.AwaitingWaiting
	}); !ok {
		return "", errors.New("the session never sent awaiting_reply waiting")
	}
	const id = "m-silo-conformance"
	if err := k.h.call(ctx, "POST", tasks.TasksPath+"/"+t.ID+"/messages", operator, map[string]string{"id": id, "text": "Carry on."}, nil); err != nil {
		return "", err
	}
	if _, ok := w.Await(k.cfg.Timeout, func(e live.Event) bool {
		return e.Type == events.TypeMessage && e.Message != nil && e.Message.ID == id
	}); !ok {
		return "", fmt.Errorf("the session sent no message event for %s", id)
	}
	if _, ok := w.Await(k.cfg.Timeout, func(e live.Event) bool {
		return e.Type == events.TypeAwaitingReply && e.AwaitingReply != nil && e.AwaitingReply.State == events.AwaitingResumed && e.AwaitingReply.MessageID == id
	}); !ok {
		return "", fmt.Errorf("the session sent no awaiting_reply resumed for %s", id)
	}
	if _, err := k.await(ctx, t.ID, tasks.Done); err != nil {
		return "", err
	}
	return fmt.Sprintf("session %s waited, took message %s, resumed and closed", t.ID, id), nil
}

func (k *kit) restart(ctx context.Context) (string, error) {
	t, w, err := k.submit(ctx, k.cfg.Repo, "", workload.Hold)
	if err != nil {
		return "", err
	}
	defer k.h.cancel(context.WithoutCancel(ctx), t.ID)
	if t, err = k.await(ctx, t.ID, tasks.Running); err != nil {
		return "", err
	}
	started := func(e live.Event) bool {
		return e.Type == events.TypeStatus && e.Status != nil && e.Status.Message == workload.Started
	}
	if _, ok := w.Await(k.cfg.Timeout, started); !ok {
		return "", fmt.Errorf("task %s never said %q", t.ID, workload.Started)
	}
	before := w.last()
	at := time.Now()
	if err := k.h.Restart(); err != nil {
		return "", fmt.Errorf("restarting the hub: %w", err)
	}
	if err := eventually(ctx, k.cfg.Register, "the grain to reconnect", func() (bool, error) {
		e, ok, err := k.h.node(ctx, k.cfg.Node)
		return ok && e.Status == registry.Online && e.ConnectedAt.After(at), err
	}); err != nil {
		return "", err
	}
	if _, ok := w.Await(k.cfg.Timeout, func(e live.Event) bool { return e.Seq > before }); !ok {
		return "", fmt.Errorf("no event after seq %d reached the restarted hub", before)
	}
	after, err := k.await(ctx, t.ID, tasks.Running)
	if err != nil {
		return "", fmt.Errorf("after the restart: %w", err)
	}
	if after.NodeTask != t.NodeTask {
		return "", fmt.Errorf("after the restart task %s is node task %q, was %q: a second run", t.ID, after.NodeTask, t.NodeTask)
	}
	n := 0
	for _, e := range w.Events() {
		if started(e) {
			n++
		}
	}
	if n != 1 {
		return "", fmt.Errorf("task %s said %q %d times: it ran more than once", t.ID, workload.Started, n)
	}
	if err := k.free(ctx, k.entry.Slots-1, k.beats(3)); err != nil {
		return "", fmt.Errorf("holding one task after the restart: %w", err)
	}
	return fmt.Sprintf("reconnected; task %s still running once, as %s, its events going on past seq %d", t.ID, t.NodeTask, before), nil
}

// filing has the Grain file a task upward from the hub task it runs,
// in a repository a team outside the submitter's owns, and checks what
// the hub recorded: the Grain as filer, the origin's submitter as
// submitter, and the task proposed. A member of that team approves it
// and the kit, standing in for that team's Grain, ends it done; the
// Grain is then told (dependency_done) and unblocks its local task.
func (k *kit) filing(ctx context.Context) (string, error) {
	if !slices.Contains(k.entry.Features, FeatureFiling) {
		return "", skip("the grain does not list the %q feature", FeatureFiling)
	}
	// The reviewer submits the origin, so that its submitter is not the
	// Grain's owner: the filed task's submitter must be the origin's.
	o, w, err := k.submitAs(ctx, reviewer, k.cfg.Repo, "", workload.File, workload.FilePrompt(elsewhereRepo))
	if err != nil {
		return "", err
	}
	defer k.h.cancel(context.WithoutCancel(ctx), o.ID)
	if o, err = k.await(ctx, o.ID, tasks.Running); err != nil {
		return "", err
	}
	said := func(prefix string) (string, error) {
		e, ok := w.Await(k.cfg.Timeout, func(e live.Event) bool {
			return e.Type == events.TypeStatus && e.Status != nil && strings.HasPrefix(e.Status.Message, prefix+" ")
		})
		if !ok {
			if t, err := k.h.task(ctx, o.ID); err == nil && t.State.Terminal() {
				return "", fmt.Errorf("task %s ended %s (%s) without saying %q", o.ID, t.State, t.Reason, prefix)
			}
			return "", fmt.Errorf("task %s never said %q", o.ID, prefix)
		}
		return strings.TrimSpace(strings.TrimPrefix(e.Status.Message, prefix)), nil
	}
	id, err := said(workload.Filed)
	if err != nil {
		return "", err
	}
	f, err := k.h.taskAs(ctx, outsider, id)
	if err != nil {
		return "", fmt.Errorf("the filed task %q: %w", id, err)
	}
	var problems []string
	for _, c := range []struct{ what, got, want string }{
		{"filed_by", f.FiledBy, k.cfg.Node},
		{"filed_by_owner", f.FiledByOwner, k.entry.Owner},
		{"submitter", f.Submitter, o.Submitter},
		{"origin_task", f.OriginTask, o.ID},
		{"repo", f.Repo, elsewhereRepo},
		{"team", f.Team, elsewhere},
		{"state", string(f.State), string(tasks.Proposed)},
		{"approved_by", f.ApprovedBy, ""},
	} {
		if c.got != c.want {
			problems = append(problems, fmt.Sprintf("%s %q, want %q", c.what, c.got, c.want))
		}
	}
	if len(problems) > 0 {
		return "", fmt.Errorf("the filed task %s: %s", id, strings.Join(problems, "; "))
	}
	if err := k.h.call(ctx, "POST", tasks.TasksPath+"/"+id+"/approve", outsider, nil, nil); err != nil {
		return "", fmt.Errorf("approving the filed task: %w", err)
	}
	if err := k.h.complete(ctx, id, outsider); err != nil {
		return "", fmt.Errorf("ending the filed task: %w", err)
	}
	got, err := said(workload.Unblocked)
	if err != nil {
		return "", fmt.Errorf("after the filed task %s ended done: %w", id, err)
	}
	if got != id {
		return "", fmt.Errorf("task %s said it was unblocked by %q, but it filed %s", o.ID, got, id)
	}
	if _, err := k.await(ctx, o.ID, tasks.Done); err != nil {
		return "", err
	}
	return fmt.Sprintf("task %s filed %s, proposed in team %s for %s, filed by %s; ended done, its dependency_done unblocked %s", o.ID, id, f.Team, f.Submitter, f.FiledBy, o.ID), nil
}
