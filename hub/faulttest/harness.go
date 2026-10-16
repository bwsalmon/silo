package faulttest

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // "pgx"

	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
	"github.com/bwsalmon/silo/hub/tasks"
	"github.com/bwsalmon/silo/internal/pgtest"
)

// UserHeader is the dev sign-on header the harness's hub trusts.
const UserHeader = "X-Test-User"

// The harness's humans: Admin is a hub admin, and Ops is a member of
// every team, owns every Grain and submits every task.
const (
	Admin = "admin@example.com"
	Ops   = "ops@example.com"
)

var (
	buildOnce sync.Once
	binary    string
	buildErr  error
)

// siloBinary builds `silo` once per test process.
func siloBinary(t testing.TB) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "silo-faulttest-")
		if err != nil {
			buildErr = err
			return
		}
		binary = filepath.Join(dir, "silo")
		out, err := exec.Command("go", "build", "-o", binary, "github.com/bwsalmon/silo/cmd/silo").CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("building silo: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binary
}

// HubProc is `silo hub serve` running as a process of its own, so that
// it can be sent SIGTERM or SIGKILL.
type HubProc struct {
	Addr string
	URL  string
	args []string
	log  string

	mu  sync.Mutex
	cmd *exec.Cmd
	end chan error
}

// StartHub starts `silo hub serve` on a free port of 127.0.0.1 against
// database, its nodes heartbeating every heartbeat.
func StartHub(t testing.TB, database string, heartbeat time.Duration) *HubProc {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	h := &HubProc{
		Addr: addr,
		URL:  "http://" + addr,
		log:  filepath.Join(t.TempDir(), "hub.log"),
		args: []string{"hub", "serve", "-addr", addr, "-database", database, "-dev-user-header", UserHeader,
			"-admins", Admin, "-heartbeat", heartbeat.String(), "-dispatch-retry", "1s", "-shutdown-grace", "10s"},
	}
	h.Start(t)
	t.Cleanup(func() { h.Kill() })
	return h
}

// Start starts the hub (again), and waits for it to answer /healthz.
func (h *HubProc) Start(t testing.TB) {
	t.Helper()
	logf, err := os.OpenFile(h.log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(logf, "--- starting the hub at %s\n", time.Now().Format(time.RFC3339Nano))
	var cmd *exec.Cmd
	// The port a killed hub held can take a moment to be free again.
	for range 50 {
		cmd = exec.Command(siloBinary(t), h.args...)
		cmd.Stdout, cmd.Stderr = logf, logf
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		end := make(chan error, 1)
		go func() { end <- cmd.Wait(); logf.Close() }()
		ok := false
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if resp, err := http.Get(h.URL + "/healthz"); err == nil {
				resp.Body.Close()
				ok = true
				break
			}
			select {
			case err := <-end:
				end <- err
				deadline = time.Time{}
			case <-time.After(50 * time.Millisecond):
			}
		}
		if ok {
			h.mu.Lock()
			h.cmd, h.end = cmd, end
			h.mu.Unlock()
			return
		}
		cmd.Process.Kill()
		<-end
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the hub did not start; its log:\n%s", h.Log())
}

// Log is everything the hub has logged.
func (h *HubProc) Log() string {
	b, _ := os.ReadFile(h.log)
	return string(b)
}

// Pid is the running hub's process ID, or 0.
func (h *HubProc) Pid() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cmd == nil {
		return 0
	}
	return h.cmd.Process.Pid
}

func (h *HubProc) signal(sig syscall.Signal, wait time.Duration) error {
	h.mu.Lock()
	cmd, end := h.cmd, h.end
	h.cmd = nil
	h.mu.Unlock()
	if cmd == nil {
		return nil
	}
	cmd.Process.Signal(sig)
	select {
	case err := <-end:
		var ee *exec.ExitError
		if sig == syscall.SIGKILL && errors.As(err, &ee) {
			return nil
		}
		return err
	case <-time.After(wait):
		cmd.Process.Kill()
		<-end
		return fmt.Errorf("the hub did not stop within %s of %v", wait, sig)
	}
}

// Stop sends the hub SIGTERM, as Kubernetes does, and waits for it to
// exit.
func (h *HubProc) Stop() error { return h.signal(syscall.SIGTERM, 30*time.Second) }

// Kill sends the hub SIGKILL: a crash.
func (h *HubProc) Kill() error { return h.signal(syscall.SIGKILL, 10*time.Second) }

// Usage is the hub process's CPU time so far and its resident memory,
// now and at its peak, from /proc.
func (h *HubProc) Usage() (cpu time.Duration, rss, peak int64) {
	pid := h.Pid()
	if pid == 0 {
		return 0, 0, 0
	}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		// Fields after the command's closing parenthesis; utime and
		// stime are the 12th and 13th of them, in clock ticks (100/s).
		s := string(b)
		f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
		if len(f) > 13 {
			u, _ := strconv.ParseInt(f[11], 10, 64)
			st, _ := strconv.ParseInt(f[12], 10, 64)
			cpu = time.Duration(u+st) * 10 * time.Millisecond
		}
	}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid)); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Fields(l)
			if len(f) >= 2 && (f[0] == "VmRSS:" || f[0] == "VmHWM:") {
				kb, _ := strconv.ParseInt(f[1], 10, 64)
				if f[0] == "VmRSS:" {
					rss = kb << 10
				} else {
					peak = kb << 10
				}
			}
		}
	}
	return cpu, rss, peak
}

// Options shape a Cluster.
type Options struct {
	Teams         int // teams, each owning one repository
	GrainsPerTeam int
	Slots         int
	// Heartbeat is the hub's heartbeat; StaleAfter is three of it.
	Heartbeat time.Duration
	// Proxied puts a Proxy in front of each Grain's connection to the
	// hub. The hub's connection to PostgreSQL always goes through one.
	Proxied bool
	// Grain is what every Grain does.
	Grain Work
}

// Work is what a Grain does with a task: see Grain's fields of the
// same names.
type Work struct {
	Refuse bool
	Work   time.Duration
	Events int
	Pad    int
	Tokens int
	Logf   func(format string, args ...any)
}

// Cluster is a hub, its database, and Grains connected to it.
type Cluster struct {
	t       testing.TB
	Opts    Options
	PG      *pgtest.Server
	PGProxy *Proxy
	// DB is the hub's database, reached directly, for checking.
	DB      *sql.DB
	Hub     *HubProc
	Ledger  *Ledger
	Grains  []*Grain
	Proxies map[string]*Proxy // by Grain ID, when Proxied

	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	cancels map[string]context.CancelFunc
	mu      sync.Mutex
}

// Repo is team i's repository.
func Repo(i int) string { return fmt.Sprintf("org/repo-%02d", i) }

// Team is team i's ID.
func Team(i int) string { return fmt.Sprintf("team-%02d", i) }

// NewCluster starts PostgreSQL, a proxy in front of it, the hub, the
// teams, and the Grains, and waits for every Grain to be online.
func NewCluster(t testing.TB, o Options) *Cluster {
	t.Helper()
	if o.Heartbeat <= 0 {
		o.Heartbeat = time.Second
	}
	if o.Slots <= 0 {
		o.Slots = 2
	}
	c := &Cluster{t: t, Opts: o, Ledger: NewLedger(), Proxies: map[string]*Proxy{}, cancels: map[string]context.CancelFunc{}}
	c.PG = pgtest.Start(t)
	var err error
	if c.PGProxy, err = NewProxy(fmt.Sprintf("127.0.0.1:%d", c.PG.Port)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.PGProxy.Close() })
	if c.DB, err = sql.Open("pgx", c.PG.URL); err != nil {
		t.Fatal(err)
	}
	c.DB.SetMaxOpenConns(4)
	t.Cleanup(func() { c.DB.Close() })
	c.Hub = StartHub(t, fmt.Sprintf("postgres://postgres@%s/postgres?sslmode=disable", c.PGProxy.Addr()), o.Heartbeat)
	c.ctx, c.cancel = context.WithCancel(context.Background())
	t.Cleanup(func() {
		c.cancel()
		c.wg.Wait()
	})

	for i := range o.Teams {
		c.Call("POST", "/v1/teams", Admin, map[string]string{"id": Team(i), "name": Team(i)}, &struct{}{})
		c.Call("PUT", "/v1/teams/"+Team(i)+"/members/"+Ops, Admin, map[string]string{"role": "member"}, &struct{}{})
		c.Call("PUT", "/v1/teams/"+Team(i)+"/repos/"+Repo(i), Admin, nil, &struct{}{})
	}
	for i := range o.Teams {
		for j := range o.GrainsPerTeam {
			g := o.Grain
			c.AddGrain(&Grain{ID: fmt.Sprintf("g-%02d-%02d", i, j), Repos: []string{Repo(i)}, Slots: o.Slots,
				Refuse: g.Refuse, Work: g.Work, Events: g.Events, Pad: g.Pad, Tokens: g.Tokens, Logf: g.Logf})
		}
	}
	c.Eventually("every Grain online", 2*time.Minute, func() bool { return c.AllOnline() == nil })
	return c
}

// AddGrain mints g's token, puts a proxy in front of it if the cluster
// is Proxied, and runs it.
func (c *Cluster) AddGrain(g *Grain) {
	var minted struct {
		Token string `json:"token"`
	}
	c.Call("POST", "/v1/node-tokens", Ops, map[string]string{"node": g.ID}, &minted)
	g.Token, g.Ledger = minted.Token, c.Ledger
	g.Hub = c.Hub.URL
	if c.Opts.Proxied {
		p, err := NewProxy(c.Hub.Addr)
		if err != nil {
			c.t.Fatal(err)
		}
		c.t.Cleanup(func() { p.Close() })
		c.Proxies[g.ID] = p
		g.Hub = "http://" + p.Addr()
	}
	c.mu.Lock()
	c.Grains = append(c.Grains, g)
	c.mu.Unlock()
	c.StartGrain(g)
}

// StartGrain runs g until StopGrain or the cluster's end.
func (c *Cluster) StartGrain(g *Grain) {
	ctx, cancel := context.WithCancel(c.ctx)
	c.mu.Lock()
	c.cancels[g.ID] = cancel
	c.mu.Unlock()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		g.Run(ctx)
	}()
}

// StopGrain stops g's process: it disconnects, and forgets nothing.
func (c *Cluster) StopGrain(g *Grain) {
	c.mu.Lock()
	cancel := c.cancels[g.ID]
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Call makes an API request to the hub, directly, as user, decoding a
// JSON answer into out when it is set; a failure with out set fails
// the test.
func (c *Cluster) Call(method, path, user string, body, out any) int {
	c.t.Helper()
	code, b, err := call(method, c.Hub.URL+path, user, body)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	if out != nil {
		if code >= 300 {
			c.t.Fatalf("%s %s: %d: %s", method, path, code, b)
		}
		if len(b) > 0 {
			if err := json.Unmarshal(b, out); err != nil {
				c.t.Fatalf("%s %s: %v: %s", method, path, err, b)
			}
		}
	}
	return code
}

var client = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 256}}

func call(method, url, user string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return 0, nil, err
	}
	if user != "" {
		req.Header.Set(UserHeader, user)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

// Submit submits a task to team i's repository, retrying while the hub
// is away (restarting, or without its database), and records it.
func (c *Cluster) Submit(team int) (string, error) {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		code, b, err := call("POST", c.Hub.URL+tasks.TasksPath, Ops, map[string]string{"repo": Repo(team), "prompt": "work"})
		if err == nil && (code == http.StatusCreated || code == http.StatusOK) {
			var tk tasks.Task
			if err := json.Unmarshal(b, &tk); err != nil {
				return "", err
			}
			c.Ledger.submitted(tk.ID)
			return tk.ID, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("submitting: %d %s %v", code, b, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// SubmitN submits n tasks round the teams, failing the test on error.
func (c *Cluster) SubmitN(n int) []string {
	c.t.Helper()
	var ids []string
	for i := range n {
		id, err := c.Submit(i % c.Opts.Teams)
		if err != nil {
			c.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// Eventually waits up to d for cond.
func (c *Cluster) Eventually(what string, d time.Duration, cond func() bool) {
	c.t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			c.t.Fatalf("timed out after %s waiting for %s", d, what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// States counts the backlog's tasks by state, from the database.
func (c *Cluster) States() map[tasks.State]int {
	out := map[tasks.State]int{}
	rows, err := c.DB.Query(`SELECT state, count(*) FROM tasks GROUP BY state`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		var n int
		rows.Scan(&s, &n)
		out[tasks.State(s)] = n
	}
	return out
}

// State is one task's state, from the database.
func (c *Cluster) State(id string) tasks.State {
	var s string
	c.DB.QueryRow(`SELECT state FROM tasks WHERE id = $1`, id).Scan(&s)
	return tasks.State(s)
}

// Nodes is the hub's node directory, as Ops sees it.
func (c *Cluster) Nodes() ([]registry.Entry, error) {
	code, b, err := call("GET", c.Hub.URL+rendezvous.DirectoryPath, Ops, nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("%s: %d %s", rendezvous.DirectoryPath, code, b)
	}
	var dir struct {
		Nodes []registry.Entry `json:"nodes"`
	}
	err = json.Unmarshal(b, &dir)
	return dir.Nodes, err
}

// AllOnline is nil when the hub has every Grain online with no claim
// outstanding, the registry's converged state.
func (c *Cluster) AllOnline() error {
	nodes, err := c.Nodes()
	if err != nil {
		return err
	}
	byID := map[string]registry.Entry{}
	for _, e := range nodes {
		byID[e.ID] = e
	}
	c.mu.Lock()
	gs := slices.Clone(c.Grains)
	c.mu.Unlock()
	for _, g := range gs {
		e, ok := byID[g.ID]
		switch {
		case !ok:
			return fmt.Errorf("node %s is not registered", g.ID)
		case e.Status != registry.Online:
			return fmt.Errorf("node %s is %s", g.ID, e.Status)
		case e.Claimed != 0:
			return fmt.Errorf("node %s has %d slots claimed", g.ID, e.Claimed)
		}
	}
	return nil
}

// Settle waits up to d for every submitted task to finish, and then
// checks the invariants: Verify.
func (c *Cluster) Settle(d time.Duration) {
	c.t.Helper()
	ids := c.Ledger.Submitted()
	deadline := time.Now().Add(d)
	for {
		left := 0
		for _, id := range ids {
			if !c.State(id).Terminal() {
				left++
			}
		}
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			c.t.Errorf("%d of %d tasks unfinished after %s: %v", left, len(ids), d, c.States())
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	c.Verify()
}

// Verify checks, for every task submitted: that it is done, not lost
// or stuck; that exactly one Grain ran it, once; and that its stored
// events' seqs are every one of 1..N, in order, ending in its result.
// Then that the hub has converged: every Grain online, none with a
// slot claimed, nothing offered, running or lost.
func (c *Cluster) Verify() {
	c.t.Helper()
	for _, v := range c.Ledger.Violations() {
		c.t.Error(v)
	}
	want := c.Opts.Grain.Events
	if want <= 0 {
		want = 1
	}
	// A task's result and its events reach the hub by different streams,
	// so a task can be done while the last of its events are still on
	// their way (a slow or lossy link, a loaded runner): its events are
	// given until eventsBy to be stored, then checked.
	eventsBy := time.Now().Add(30 * time.Second)
	for _, id := range c.Ledger.Submitted() {
		if s := c.State(id); s != tasks.Done {
			var reason string
			c.DB.QueryRow(`SELECT reason FROM tasks WHERE id = $1`, id).Scan(&reason)
			c.t.Errorf("task %s is %s (%s), want done", id, s, reason)
			continue
		}
		if runs := c.Ledger.Runs(id); len(runs) != 1 {
			c.t.Errorf("task %s ran %d times, on %v; want once", id, len(runs), runs)
		}
		seqs, last, err := c.events(id)
		for err == nil && len(seqs) < want+1 && time.Now().Before(eventsBy) {
			time.Sleep(200 * time.Millisecond)
			seqs, last, err = c.events(id)
		}
		if err != nil {
			c.t.Errorf("task %s's events: %v", id, err)
			continue
		}
		stride := int64(1 + c.Opts.Grain.Tokens)
		ok := len(seqs) == want+1 && last == "result"
		for i := 0; ok && i < want; i++ {
			ok = seqs[i] == int64(i)*stride+1
		}
		if ok {
			ok = seqs[want] == int64(want)*stride+1
		}
		if !ok {
			c.t.Errorf("task %s's stored events: seqs %v ending in %q; want %d status events every %d seqs and a result", id, seqs, last, want, stride)
		}
	}
	c.Eventually("the hub to converge", 30*time.Second, func() bool {
		s := c.States()
		return c.AllOnline() == nil && s[tasks.Offered] == 0 && s[tasks.Running] == 0 && s[tasks.Lost] == 0
	})
}

// events is task id's stored events' seqs, in order, and the last one's
// type.
func (c *Cluster) events(id string) (seqs []int64, last string, err error) {
	rows, err := c.DB.Query(`SELECT seq, type FROM task_events WHERE task = $1 ORDER BY seq`, id)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		rows.Scan(&seq, &last)
		seqs = append(seqs, seq)
	}
	return seqs, last, rows.Err()
}

// Ledger is what the Grains ran, and what the tests saw, against which
// the invariants are checked.
type Ledger struct {
	mu         sync.Mutex
	sub        []string
	subAt      map[string]time.Time
	starts     map[string][]string
	dispatch   []time.Duration
	refusals   map[string]int
	resumes    int
	finishes   map[string]int
	violations []string
}

// NewLedger returns an empty ledger.
func NewLedger() *Ledger {
	return &Ledger{subAt: map[string]time.Time{}, starts: map[string][]string{}, refusals: map[string]int{}, finishes: map[string]int{}}
}

func (l *Ledger) submitted(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sub = append(l.sub, id)
	l.subAt[id] = time.Now()
}

func (l *Ledger) started(id, grain string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.starts[id] = append(l.starts[id], grain)
	if at, ok := l.subAt[id]; ok && len(l.starts[id]) == 1 {
		l.dispatch = append(l.dispatch, time.Since(at))
	}
}

func (l *Ledger) refused(id, grain string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refusals[grain]++
}

func (l *Ledger) resumed(id, grain string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.resumes++
}

func (l *Ledger) finished(id, grain string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.finishes[id]++
}

// Violation records a broken invariant a watcher saw.
func (l *Ledger) Violation(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.violations) < 50 {
		l.violations = append(l.violations, fmt.Sprintf(format, args...))
	}
}

// Violations are the broken invariants recorded so far.
func (l *Ledger) Violations() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.violations)
}

// Submitted is every task submitted, in order.
func (l *Ledger) Submitted() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.sub)
}

// Runs is the Grains that started task id, once each time.
func (l *Ledger) Runs(id string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.starts[id])
}

// Refusals is how many offers grain refused.
func (l *Ledger) Refusals(grain string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.refusals[grain]
}

// Resumes is how many resumes the Grains accepted.
func (l *Ledger) Resumes() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.resumes
}

// DispatchLatencies are, for each task started, the time from its
// submission to its first offer reaching a Grain.
func (l *Ledger) DispatchLatencies() []time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.dispatch)
}

// Percentile is the p'th percentile (0-100) of ds.
func Percentile(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := slices.Clone(ds)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(float64(len(s)-1) * p / 100)
	return s[i]
}

// Watcher follows one task's events over SSE as a subscriber does,
// reconnecting from its last seq (Last-Event-ID) whenever its stream
// ends early, and records a violation for any seq that does not go up.
// It measures each status event's fan-out latency, from the Grain
// writing it to the subscriber reading it.
type Watcher struct {
	c    *Cluster
	task string
	// Slow, when set, is a pause after each event: a subscriber that
	// cannot keep up.
	Slow time.Duration

	mu        sync.Mutex
	last      int64
	got       int
	ended     bool
	reconnect int
	lat       []time.Duration
	done      chan struct{}
}

// Watch starts a Watcher on task.
func (c *Cluster) Watch(task string, slow time.Duration) *Watcher {
	w := &Watcher{c: c, task: task, Slow: slow, done: make(chan struct{})}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer close(w.done)
		w.run(c.ctx)
	}()
	return w
}

// Done is closed once the watcher has seen the task's end.
func (w *Watcher) Done() <-chan struct{} { return w.done }

// Stats are what the watcher saw.
func (w *Watcher) Stats() (events, reconnects int, ended bool, lat []time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.got, w.reconnect, w.ended, slices.Clone(w.lat)
}

func (w *Watcher) run(ctx context.Context) {
	for ctx.Err() == nil {
		if w.once(ctx) {
			return
		}
		w.mu.Lock()
		w.reconnect++
		w.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// once is one subscription; it reports whether it saw the end.
func (w *Watcher) once(ctx context.Context) bool {
	req, _ := http.NewRequestWithContext(ctx, "GET", w.c.Hub.URL+"/v1/tasks/"+w.task+"/events", nil)
	req.Header.Set(UserHeader, Ops)
	w.mu.Lock()
	if w.last > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(w.last, 10))
	}
	w.mu.Unlock()
	resp, err := sseClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 2<<20)
	var id int64 = -1
	var typ string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			id, _ = strconv.ParseInt(line[4:], 10, 64)
		case strings.HasPrefix(line, "event: "):
			typ = line[7:]
		case strings.HasPrefix(line, "data: "):
			if typ == "end" {
				w.mu.Lock()
				w.ended = true
				w.mu.Unlock()
				return true
			}
			w.mu.Lock()
			if id <= w.last {
				w.mu.Unlock()
				w.c.Ledger.Violation("task %s: subscriber saw seq %d after %d", w.task, id, w.last)
				w.mu.Lock()
			}
			w.last = id
			w.got++
			if typ == "status" {
				if at := sentAt(line[6:]); !at.IsZero() {
					w.lat = append(w.lat, time.Since(at))
				}
			}
			w.mu.Unlock()
			if w.Slow > 0 {
				time.Sleep(w.Slow)
			}
			id, typ = -1, ""
		}
	}
	return false
}

var sseClient = &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 2048, MaxConnsPerHost: 0}}

// sentAt is the time a Grain wrote a status event, from its "time".
func sentAt(data string) time.Time {
	var e struct {
		Time time.Time `json:"time"`
	}
	json.Unmarshal([]byte(data), &e)
	return e.Time
}

// DBSize is the hub's database's size on disk, in bytes.
func (c *Cluster) DBSize() int64 {
	var n int64
	c.DB.QueryRow(`SELECT pg_database_size(current_database())`).Scan(&n)
	return n
}
