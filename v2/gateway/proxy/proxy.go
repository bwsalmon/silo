// Package proxy fronts MCP servers in the gateway (design §13): it
// launches a server over stdio or connects to one by URL, lists its
// tools within a start timeout, filters them through the entry's
// allowlist, and serves each allowed tool as <server>__<tool>. It owns
// the servers' lifecycle (§13.4): each launched server runs in its own
// process group and is stopped with SIGTERM, then SIGKILL after a
// grace; a url: server is never stopped; a call to a server that has
// died is a tool error naming it; restart: on-failure restarts at most
// once per interval; and a changed tool list is re-filtered and
// re-published, which the gateway announces to the CLI with
// notifications/tools/list_changed.
//
// A Spec is plain data plus an optional Dial or HTTP client, so a server
// given in code rather than read from a Silofile (grain.md §2.2's
// caller-provided servers, session.CallerMCP) is fronted the same way,
// on a Unix socket too, with its blocking tools free of the timeout.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwsalmon/silo/v2/gateway"
	"github.com/bwsalmon/silo/v2/internal/procgroup"
	"github.com/bwsalmon/silo/v2/mcp"
)

// Defaults of design §13.4 and §4.4.
const (
	DefaultStartTimeout    = 30 * time.Second
	DefaultStopGrace       = 5 * time.Second
	DefaultRestartInterval = 60 * time.Second
	DefaultCallTimeout     = gateway.DefaultTimeout
)

// Separator joins a server's name and its tool's: github__get_issue.
const Separator = "__"

// Spec is one server to front. Exactly one of Command, URL and Dial is
// set.
type Spec struct {
	// Name prefixes the server's tools.
	Name string

	// Command is the argv of a server to launch over stdio, with Env as
	// its whole environment and Dir as its working directory.
	Command []string
	Env     []string
	Dir     string
	// Stderr receives a launched server's stderr; discarded when nil.
	Stderr io.Writer

	// Prelaunch runs before each launch of Command, and a launch it
	// fails does not happen: a server on the ssh agent host writes its
	// env file there (design §13.2).
	Prelaunch func(ctx context.Context) error
	// Reap runs after a launched server's local process has stopped,
	// given the stop grace: for a server on the agent host, whose local
	// process is the ssh channel, it waits that long for what the
	// channel ran to go and then kills what is left.
	Reap func(ctx context.Context, grace time.Duration)

	// URL is a running server to connect to, sending Headers.
	URL     string
	Headers map[string]string
	// HTTP is the client a URL server is reached through;
	// http.DefaultClient when nil, mcp.UnixClient's for a Unix socket
	// (a caller's, or the -L forward to a server on the agent host).
	HTTP *http.Client

	// Gone, when set, reports the loss of a connection the server is
	// reached through (the ssh master): a call made after it, and the
	// warning when the server dies of it, name it.
	Gone func() error

	// Dial connects to a server given in code.
	Dial func(ctx context.Context) (mcp.Conn, error)

	// Allow is the allowlist of the server's tool names; nil allows
	// every tool it lists.
	Allow []string
	// Required makes a server that does not start fail Start.
	Required bool
	// Restart restarts a launched server that dies (restart:
	// on-failure).
	Restart bool
	// Blocking names tools (the server's own names) that the gateway
	// lets run without a timeout (grain.md §2.2).
	Blocking []string
}

func (s Spec) launched() bool { return len(s.Command) > 0 }

// Options configure a Proxy; zero values take the defaults.
type Options struct {
	StartTimeout    time.Duration
	StopGrace       time.Duration
	RestartInterval time.Duration
	CallTimeout     time.Duration
	// OnLaunch is called with each launched server's process group id,
	// before the server is connected to: session records it so silo gc
	// can kill the group after a crash.
	OnLaunch func(name string, pgid int)
	// Warn reports what does not fail the session: an optional server
	// that did not start, a death, a restart, a changed tool list.
	Warn func(format string, args ...any)
}

// Registrar receives a server's tool set each time it changes: the
// gateway.
type Registrar interface {
	Update(remove []string, add []gateway.Tool)
}

// Proxy fronts a set of servers.
type Proxy struct {
	opts Options

	mu      sync.Mutex
	reg     Registrar
	servers []*server
	closed  bool
}

// New returns a proxy.
func New(opts Options) *Proxy {
	if opts.StartTimeout <= 0 {
		opts.StartTimeout = DefaultStartTimeout
	}
	if opts.StopGrace <= 0 {
		opts.StopGrace = DefaultStopGrace
	}
	if opts.RestartInterval <= 0 {
		opts.RestartInterval = DefaultRestartInterval
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = DefaultCallTimeout
	}
	if opts.Warn == nil {
		opts.Warn = func(string, ...any) {}
	}
	return &Proxy{opts: opts}
}

// StartError is a required server that did not start.
type StartError struct {
	Server string
	Err    error
}

func (e *StartError) Error() string { return fmt.Sprintf("MCP server %s: %v", e.Server, e.Err) }
func (e *StartError) Unwrap() error { return e.Err }

// Start starts the servers in parallel and returns the tools of those
// that started. A required server that does not start is an error,
// after which the caller should Close the proxy; an optional one is
// warned about and its tools are absent.
func (p *Proxy) Start(ctx context.Context, specs []Spec) ([]gateway.Tool, error) {
	srvs := make([]*server, len(specs))
	errs := make([]error, len(specs))
	var wg sync.WaitGroup
	for i, sp := range specs {
		s := &server{p: p, spec: sp, stop: make(chan struct{})}
		srvs[i] = s
		p.mu.Lock()
		p.servers = append(p.servers, s)
		p.mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.start(ctx, true)
		}()
	}
	wg.Wait()
	var tools []gateway.Tool
	var failed []error
	for i, s := range srvs {
		switch {
		case errs[i] == nil:
			s.mu.Lock()
			tools = append(tools, s.tools...)
			s.published = names(s.tools)
			s.mu.Unlock()
		case s.spec.Required:
			failed = append(failed, &StartError{Server: s.spec.Name, Err: errs[i]})
		default:
			p.opts.Warn("MCP server %s did not start, and is optional (required: false), so the session goes on without its tools: %v", s.spec.Name, errs[i])
		}
	}
	return tools, errors.Join(failed...)
}

// Attach sends every later change of a server's tool set to reg, and
// brings reg up to date with any change since Start.
func (p *Proxy) Attach(reg Registrar) {
	p.mu.Lock()
	p.reg = reg
	srvs := append([]*server(nil), p.servers...)
	p.mu.Unlock()
	for _, s := range srvs {
		s.publish()
	}
}

// Close stops every server silo launched and disconnects from the rest.
func (p *Proxy) Close(ctx context.Context) error {
	p.mu.Lock()
	p.closed = true
	srvs := append([]*server(nil), p.servers...)
	p.mu.Unlock()
	errs := make([]error, len(srvs))
	var wg sync.WaitGroup
	for i, s := range srvs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.close(ctx)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// server is one fronted server, across restarts.
type server struct {
	p    *Proxy
	spec Spec
	stop chan struct{}

	mu        sync.Mutex
	conn      mcp.Conn
	proc      *process
	dead      error
	closed    bool
	lastStart time.Time
	// tools is the current filtered set; published what the registrar
	// was last given.
	tools     []gateway.Tool
	published []string
}

// process is one launch of a server.
type process struct {
	cmd    *exec.Cmd
	exited chan struct{}
}

func (s *server) name() string { return s.spec.Name }

// start connects to the server, lists its tools and starts watching it.
// At session start (first) an allowlisted tool the server does not
// offer is an error; after a restart it is a warning.
func (s *server) start(ctx context.Context, first bool) error {
	ctx, cancel := context.WithTimeout(ctx, s.p.opts.StartTimeout)
	defer cancel()
	s.mu.Lock()
	s.lastStart = time.Now()
	s.mu.Unlock()
	conn, proc, err := s.connect(ctx)
	if err == nil {
		var listed []mcp.ToolInfo
		if listed, err = conn.ListTools(ctx); err == nil {
			var tools []gateway.Tool
			if tools, err = s.filter(listed, first); err == nil {
				s.mu.Lock()
				if s.closed {
					err = errors.New("the session is ending")
				} else {
					s.conn, s.proc, s.dead, s.tools = conn, proc, nil, tools
				}
				s.mu.Unlock()
				if err == nil {
					go s.watch(conn, proc)
					return nil
				}
			}
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("did not connect and list its tools within %s", s.p.opts.StartTimeout)
	}
	if conn != nil {
		_ = conn.Close()
	}
	if proc != nil {
		s.terminate(context.Background(), proc)
	}
	return err
}

// connect launches or dials the server and initializes the connection.
func (s *server) connect(ctx context.Context) (mcp.Conn, *process, error) {
	switch {
	case s.spec.Dial != nil:
		c, err := s.spec.Dial(ctx)
		if err != nil {
			return nil, nil, err
		}
		if _, err := c.Initialize(ctx); err != nil {
			return c, nil, err
		}
		return c, nil, nil
	case s.spec.URL != "":
		c, err := mcp.DialHTTPClient(ctx, s.spec.URL, s.spec.Headers, s.spec.HTTP)
		return c, nil, err
	case s.spec.launched():
		if s.spec.Prelaunch != nil {
			if err := s.spec.Prelaunch(ctx); err != nil {
				return nil, nil, err
			}
		}
		proc, conn, err := s.launch()
		if err != nil {
			return nil, nil, err
		}
		type res struct{ err error }
		done := make(chan res, 1)
		go func() {
			_, err := conn.Initialize(ctx)
			done <- res{err}
		}()
		select {
		case r := <-done:
			if r.err != nil {
				return conn, proc, fmt.Errorf("initialize: %w", r.err)
			}
		case <-proc.exited:
			return conn, proc, fmt.Errorf("%s exited at start (%s)", s.spec.Command[0], proc.cmd.ProcessState)
		}
		return conn, proc, nil
	}
	return nil, nil, errors.New("no command, url or dial")
}

// launch starts the server's process in a process group of its own,
// talking MCP over its stdin and stdout.
func (s *server) launch() (*process, mcp.Conn, error) {
	argv := s.spec.Command
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env, cmd.Dir = s.spec.Env, s.spec.Dir
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A child the server leaves holding stderr open must not hold up
	// noticing that the server itself exited.
	cmd.WaitDelay = time.Second
	procgroup.DieWithParent(cmd)
	// Pipes made here rather than by StdinPipe and StdoutPipe, which
	// Wait closes: the reader must see the server's last line.
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, nil, err
	}
	cmd.Stdin, cmd.Stdout = inR, outW
	if s.spec.Stderr != nil {
		cmd.Stderr = s.spec.Stderr
	}
	err = cmd.Start()
	inR.Close()
	outW.Close()
	if err != nil {
		inW.Close()
		outR.Close()
		return nil, nil, err
	}
	if s.p.opts.OnLaunch != nil {
		s.p.opts.OnLaunch(s.spec.Name, cmd.Process.Pid)
	}
	proc := &process{cmd: cmd, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		outR.Close()
		close(proc.exited)
	}()
	return proc, mcp.NewStdioConn(outR, inW), nil
}

// filter applies the allowlist to a tools/list answer.
func (s *server) filter(listed []mcp.ToolInfo, strict bool) ([]gateway.Tool, error) {
	offered := map[string]mcp.ToolInfo{}
	var all []string
	for _, t := range listed {
		offered[t.Name] = t
		all = append(all, t.Name)
	}
	sort.Strings(all)
	want := s.spec.Allow
	if want == nil {
		want = all
	}
	var missing []string
	var tools []gateway.Tool
	for _, n := range want {
		t, ok := offered[n]
		if !ok {
			missing = append(missing, n)
			continue
		}
		tools = append(tools, s.tool(t))
	}
	if len(missing) > 0 {
		msg := fmt.Sprintf("tools %s are in its allowlist (tools:) but the server does not offer them; it offers %s",
			strings.Join(missing, ", "), orNone(all))
		if strict {
			return nil, errors.New(msg)
		}
		s.p.opts.Warn("MCP server %s: %s", s.name(), msg)
	}
	return tools, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func orNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// tool is the gateway tool for one of the server's.
func (s *server) tool(t mcp.ToolInfo) gateway.Tool {
	schema := t.InputSchema
	if schema == nil {
		schema = map[string]any{"type": "object"}
	}
	orig := t.Name
	return gateway.Tool{
		Name:        s.spec.Name + Separator + orig,
		Description: t.Description,
		InputSchema: schema,
		Timeout:     s.p.opts.CallTimeout,
		Blocking:    contains(s.spec.Blocking, orig),
		Handler: func(ctx context.Context, args map[string]any) (mcp.Result, error) {
			conn, err := s.live()
			if err != nil {
				return mcp.Result{}, err
			}
			r, err := conn.CallTool(ctx, orig, args)
			if err != nil {
				if ctx.Err() != nil {
					return mcp.Result{}, ctx.Err()
				}
				var rpcErr *mcp.Error
				if errors.As(err, &rpcErr) {
					return mcp.Result{}, fmt.Errorf("MCP server %s refused %s: %s", s.name(), orig, rpcErr.Message)
				}
				return mcp.Result{}, fmt.Errorf("MCP server %s: %v", s.name(), err)
			}
			return mcp.Result{Text: r.Text(), IsError: r.IsError}, nil
		},
	}
}

// live returns the connection, or the error a call to a dead server
// gets: a tool error naming the server.
func (s *server) live() (mcp.Conn, error) {
	if err := s.gone(0); err != nil {
		return nil, fmt.Errorf("MCP server %s is unreachable: %v", s.name(), err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.dead != nil:
		return nil, s.dead
	case s.conn == nil:
		return nil, fmt.Errorf("MCP server %s is not running", s.name())
	}
	return s.conn, nil
}

// gone is Spec.Gone's answer, waiting up to wait for it: the end of a
// channel and that of the connection it runs on are noticed in either
// order.
func (s *server) gone(wait time.Duration) error {
	if s.spec.Gone == nil {
		return nil
	}
	end := time.Now().Add(wait)
	for {
		err := s.spec.Gone()
		if err != nil || !time.Now().Before(end) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// watch follows one connection until it ends.
func (s *server) watch(conn mcp.Conn, proc *process) {
	var exited <-chan struct{}
	if proc != nil {
		exited = proc.exited
	}
	for {
		select {
		case <-s.stop:
			return
		case <-conn.Changed():
			s.relist(conn)
		case <-conn.Done():
			if proc != nil {
				// Give the exit status a moment to arrive, for the error.
				select {
				case <-proc.exited:
				case <-time.After(time.Second):
				}
			}
			s.died(conn, proc, conn.Err())
			return
		case <-exited:
			s.died(conn, proc, nil)
			return
		}
	}
}

// relist re-reads the tool list after tools/list_changed.
func (s *server) relist(conn mcp.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), s.p.opts.StartTimeout)
	defer cancel()
	listed, err := conn.ListTools(ctx)
	if err != nil {
		s.p.opts.Warn("MCP server %s: listing its tools after tools/list_changed: %v", s.name(), err)
		return
	}
	tools, _ := s.filter(listed, false)
	s.mu.Lock()
	if s.conn != conn {
		s.mu.Unlock()
		return
	}
	s.tools = tools
	s.mu.Unlock()
	s.publish()
}

// publish gives the registrar the current tool set, removing what it no
// longer holds.
func (s *server) publish() {
	s.p.mu.Lock()
	reg := s.p.reg
	s.p.mu.Unlock()
	if reg == nil {
		return
	}
	s.mu.Lock()
	cur := append([]gateway.Tool(nil), s.tools...)
	keep := map[string]bool{}
	for _, t := range cur {
		keep[t.Name] = true
	}
	var remove []string
	for _, n := range s.published {
		if !keep[n] {
			remove = append(remove, n)
		}
	}
	s.published = names(cur)
	s.mu.Unlock()
	reg.Update(remove, cur)
}

func names(tools []gateway.Tool) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name
	}
	return out
}

// died records that a connection ended without silo ending it, and
// restarts the server when its spec says to.
func (s *server) died(conn mcp.Conn, proc *process, cause error) {
	why := "its connection ended"
	if proc != nil {
		select {
		case <-proc.exited:
			why = "it exited (" + proc.cmd.ProcessState.String() + ")"
		default:
			if cause != nil {
				why = cause.Error()
			}
		}
	} else if cause != nil {
		why = cause.Error()
	}
	lost := s.gone(2 * time.Second)
	if lost != nil {
		why = lost.Error()
	}
	_ = conn.Close()
	s.mu.Lock()
	if s.closed || s.conn != conn {
		s.mu.Unlock()
		return
	}
	restart := s.spec.Restart && s.spec.launched() && lost == nil
	msg := fmt.Sprintf("MCP server %s is not running: %s", s.name(), why)
	if lost != nil {
		msg += "; it is not restarted"
	} else if restart {
		msg += "; it is restarted at most once per " + s.p.opts.RestartInterval.String()
	} else {
		msg += "; it is not restarted (restart: never)"
	}
	s.dead = errors.New(msg)
	s.conn, s.proc = nil, nil
	s.mu.Unlock()
	if proc != nil {
		// Its children, if it left any.
		s.terminate(context.Background(), proc)
	}
	s.p.opts.Warn("%s", msg)
	if restart {
		go s.restart()
	}
}

// restart starts the server again, no sooner than RestartInterval
// after its last start, and keeps trying at that rate until it starts
// or the session ends.
func (s *server) restart() {
	for {
		s.mu.Lock()
		wait := time.Until(s.lastStart.Add(s.p.opts.RestartInterval))
		s.mu.Unlock()
		if wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-s.stop:
				t.Stop()
				return
			case <-t.C:
			}
		}
		select {
		case <-s.stop:
			return
		default:
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			select {
			case <-s.stop:
				cancel()
			case <-ctx.Done():
			}
		}()
		err := s.start(ctx, false)
		cancel()
		if err == nil {
			s.p.opts.Warn("MCP server %s restarted (restart: on-failure)", s.name())
			s.publish()
			return
		}
		s.mu.Lock()
		s.dead = fmt.Errorf("MCP server %s is not running: restarting it failed: %v", s.name(), err)
		s.mu.Unlock()
		s.p.opts.Warn("%v", s.dead)
	}
}

// close ends the server: a launched one is stopped, a url: one only
// disconnected from.
func (s *server) close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.stop)
	conn, proc := s.conn, s.proc
	s.conn = nil
	s.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	if proc != nil {
		s.terminate(ctx, proc)
	}
	return nil
}

// terminate stops a launched server's process group: SIGTERM, then
// SIGKILL after the grace, and SIGKILL to whatever is left of the group
// once the server itself has gone.
func (s *server) terminate(ctx context.Context, proc *process) {
	pgid := proc.cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	t := time.NewTimer(s.p.opts.StopGrace)
	defer t.Stop()
	select {
	case <-proc.exited:
	case <-t.C:
	case <-ctx.Done():
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	<-proc.exited
	if s.spec.Reap != nil {
		s.spec.Reap(ctx, s.p.opts.StopGrace)
	}
}
