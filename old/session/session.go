// Package session is one silo run session, start to finish (design §3):
// the session id and directory, session.json, setup in §3's order through
// a registry of steps and tool providers, teardown in reverse with each
// step independent of the others, signals (§3.1), exit codes (§3.3) and
// the cheap local part of silo gc.
package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwsalmon/silo/old/agentcli"
	"github.com/bwsalmon/silo/old/control"
	"github.com/bwsalmon/silo/old/credentials"
	"github.com/bwsalmon/silo/old/events"
	"github.com/bwsalmon/silo/old/gateway"
	"github.com/bwsalmon/silo/old/gateway/audit"
	"github.com/bwsalmon/silo/old/gitproxy"
	"github.com/bwsalmon/silo/old/location"
	"github.com/bwsalmon/silo/old/location/local"
	"github.com/bwsalmon/silo/old/mcp"
	"github.com/bwsalmon/silo/old/sandbox"
	"github.com/bwsalmon/silo/old/silofile"

	// The CLI mappings silo run drives.
	_ "github.com/bwsalmon/silo/old/agentcli/claude"
	_ "github.com/bwsalmon/silo/old/agentcli/codex"
	_ "github.com/bwsalmon/silo/old/agentcli/custom"
)

// Exit codes silo owns (design §3.3, call 3).
const (
	ExitUsage        = 64
	ExitInvalid      = 65
	ExitPrerequisite = 69
	ExitConnLost     = 75
	ExitRefused      = 76
	ExitDeadline     = 124
	ExitInterrupted  = 130 // 128 + SIGINT
	ExitTerminated   = 143 // 128 + SIGTERM
)

// DefaultGrace is how long the CLI is given after SIGINT in headless
// mode, and after SIGTERM before SIGKILL (design §3.1).
const DefaultGrace = 10 * time.Second

// Options configure Run. Only File is required.
type Options struct {
	File *silofile.Silofile
	// Prompt, when set, replaces agent.prompt (silo run --prompt).
	Prompt string

	// The terminal (interactive) or where headless output goes.
	Stdin          io.Reader
	Stdout, Stderr io.Writer

	// Environ is the invoking environment; os.Environ() when nil.
	Environ []string
	// Location is where the CLI runs; chosen from location.kind when nil.
	Location location.Location
	// Backend runs sandboxes; the container backend when nil.
	Backend sandbox.Backend
	// Resolver resolves credentials; one reading Environ when nil.
	Resolver *credentials.Resolver
	// Credentials are values the caller already holds, by credential
	// name: a credential named here takes this value, and its from: is
	// not read (the library's way to pass credentials, grain.md §2.1).
	Credentials map[string]string
	// Signals delivers SIGINT and SIGTERM; signal.Notify's when nil.
	Signals <-chan os.Signal
	// Grace replaces DefaultGrace.
	Grace time.Duration
	// Sink receives the session's events (docs/events.md); TextSink on
	// Stderr when nil.
	Sink Sink
	// Cancel ends the session, as cancelled, with the reason sent.
	Cancel <-chan string
	// ExtendDeadline moves the session's deadline to the time sent.
	ExtendDeadline <-chan time.Time
	// NoGC skips the local silo gc at start.
	NoGC bool
	// CallerMCP are MCP servers the caller serves, which the gateway
	// connects to and fronts as <name>__<tool> (grain.md §2.2).
	CallerMCP []CallerMCP
	// Rename lists gateway tools under other names: the key is silo's
	// name (sandbox_exec, local_<script>, <server>__<tool>), the value
	// the name the agent sees (grain.md §2.2, design open question 19).
	// Silo chooses no names of its own.
	Rename map[string]string
	// Labels are the caller's, kept in session.json so that it can find
	// its sessions again with silo ls (grain.md §2.7): grain's run ids.
	Labels map[string]string
	// Detached records in session.json that the session runs under its
	// own supervisor, silo run --detach.
	Detached bool
	// GitHosts is where the git proxy sends each git host's requests,
	// when that is not https://<host> (gitproxy.GitProxy.Hosts): a git
	// host on another port.
	GitHosts map[string]gitproxy.Host
	// GitForwarderFor, when set, replaces the git proxy's upstream for a
	// host (gitproxy.GitProxy.ForwarderFor), for tests.
	GitForwarderFor func(host string) (gitproxy.Forwarder, error)
	// Started, when set, is called with the session once it has an id
	// and a directory, before setup: the library's handle on it.
	Started func(*Session)
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// Session is one session in progress. Steps read and fill it.
type Session struct {
	ID string
	// Dir is the session directory, $XDG_RUNTIME_DIR/silo/<id>/.
	Dir string
	// StateDir keeps the audit log after the session.
	StateDir string
	File     *silofile.Silofile
	Mode     string
	Deadline time.Duration

	Prompt, SystemPrompt string

	Mapping  agentcli.Mapping
	Location location.Location
	Host     location.HostInfo

	Credentials map[string]credentials.Value
	// AgentEnv is set in the CLI's environment: API-key auth and
	// credentials delivered {agent: env}.
	AgentEnv map[string]string

	// Tools are what the providers started; ToolNames what the gateway
	// lists.
	Tools      []gateway.Tool
	ToolNames  []string
	Audit      *audit.Log
	Gateway    *gateway.Gateway
	Server     *mcp.Server
	GatewayURL string
	// Control is the session's control socket.
	Control *control.Server

	// Command is the CLI's command, once staged.
	Command location.Command

	opts Options
	// resolver resolves credentials, at start and for the git proxy.
	resolver *credentials.Resolver
	// git is the session's git proxy, when a sandbox has repos.
	git *gitSession
	// sandboxes is the sandbox backend (backend()).
	backendMu sync.Mutex
	sandboxes sandbox.Backend
	// inj delivers injected messages; cancel and extend carry Cancel
	// and ExtendDeadline, from Options and from the control socket.
	inj    *injector
	cancel chan string
	extend chan time.Time
	// resume and stageDir build a resumed turn's command.
	resume   func(dir, id string) location.Command
	stageDir string
	rec      *recorder
	em       *emitter
	// parser reads the CLI's headless output; transcript is the tee
	// that feeds it.
	parser     agentcli.Parser
	transcript *transcript
	result     *events.Result

	hookMu       sync.Mutex
	gatewayHooks []func(*gateway.Gateway)
}

func (s *Session) getenv(k string) string { return agentcli.Getenv(s.opts.Environ, k) }

func (s *Session) status(format string, args ...any) {
	s.emitStatus("", events.StateInfo, fmt.Sprintf(format, args...))
}

func (s *Session) warnf(format string, args ...any) {
	s.emitStatus("", events.StateWarning, fmt.Sprintf(format, args...))
}

func (s *Session) emitStatus(step, state, msg string) {
	s.em.emit(events.Event{Type: events.TypeStatus, Status: &events.Status{Step: step, State: state, Message: msg}})
}

// RequestApproval emits an approval_request event: a gateway call with
// approve: true waiting for its answer, which arrives over the control
// socket's approve method (design §12.3).
func (s *Session) RequestApproval(r events.ApprovalRequest) {
	s.em.emit(events.Event{Type: events.TypeApprovalRequest, ApprovalRequest: &r})
}

// AddResource records something set up outside the session directory
// in session.json, so that silo gc can remove it after a crash.
func (s *Session) AddResource(r Resource) error {
	return s.rec.update(func(rec *Record) { rec.Resources = append(rec.Resources, r) })
}

// Options returns the options the session was started with.
func (s *Session) Options() Options { return s.opts }

// Error is an error with the exit code it ends silo run with.
type Error struct {
	Code int
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// exitCode picks the exit code for a setup error.
func exitCode(err error) int {
	var e *Error
	var pre *location.PrerequisiteError
	switch {
	case errors.As(err, &e):
		return e.Code
	case agentcli.IsRefusal(err):
		return ExitRefused
	case errors.As(err, &pre):
		return ExitPrerequisite
	}
	// Anything else that stops setup -- a sandbox that would not start,
	// an image that would not pull -- is a prerequisite the session
	// could not get.
	return ExitPrerequisite
}

// Run runs one session and returns silo run's exit code, and the error
// that ended it, if one did.
func Run(ctx context.Context, opts Options) (int, error) {
	res, err := Execute(ctx, opts)
	return res.Exit.Code, err
}

// Execute runs one session and returns its result, which it has also
// emitted as the session's last event.
func Execute(ctx context.Context, opts Options) (events.Result, error) {
	if opts.Environ == nil {
		opts.Environ = os.Environ()
	}
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	if opts.Grace <= 0 {
		opts.Grace = DefaultGrace
	}
	// silo's own lines and the CLI's stderr share the writer.
	if _, ok := opts.Stderr.(*os.File); !ok {
		opts.Stderr = &syncWriter{w: opts.Stderr}
	}
	if _, ok := opts.Stdout.(*os.File); !ok {
		opts.Stdout = &syncWriter{w: opts.Stdout}
	}
	sink := opts.Sink
	if sink == nil {
		sink = TextSink{opts.Stderr}
	}
	s := &Session{File: opts.File, opts: opts, em: &emitter{sink: sink, phase: events.PhaseSetup}, AgentEnv: map[string]string{},
		cancel: make(chan string, 1), extend: make(chan time.Time, 1)}
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case r := <-opts.Cancel:
				s.requestCancel(r)
			case t := <-opts.ExtendDeadline:
				s.requestExtend(t)
			case <-done:
				return
			}
		}
	}()
	code, err := s.start(ctx)
	defer s.em.closeLog()
	if s.result == nil {
		// It ended before there was a session to tear down.
		r := events.Result{Exit: events.Exit{Code: code, Reason: events.ReasonSetupFailed}}
		if err != nil {
			r.Exit.Detail = err.Error()
		}
		s.em.emit(events.Event{Type: events.TypeResult, Result: &r})
		s.result = &r
	}
	return *s.result, err
}

func (s *Session) start(ctx context.Context) (int, error) {
	opts := s.opts
	if opts.File == nil {
		return ExitUsage, errors.New("session: no Silofile")
	}
	getenv := s.getenv

	if !opts.NoGC {
		rep, err := GC(ctx, GCOptions{Getenv: getenv, Backend: opts.Backend})
		for _, line := range rep.Removed {
			s.status("gc: %s", line)
		}
		if err != nil {
			s.warnf("gc: %v", err)
		}
	}

	// Step 1: load. The Silofile is already parsed and validated.
	f := opts.File
	s.Mode = f.Agent.Mode
	if s.Mode == "" {
		s.Mode = agentcli.Interactive
	}
	if err := agentcli.CheckMode(s.Mode); err != nil {
		return ExitRefused, err
	}
	m, err := agentcli.Lookup(f.Agent.CLI)
	if err != nil {
		return ExitRefused, err
	}
	s.Mapping = m
	s.inj = newInjector(s.Mode, f.Agent.CLI, m)
	if f.Agent.Limits != nil && f.Agent.Limits.Deadline != nil {
		s.Deadline = time.Duration(*f.Agent.Limits.Deadline)
	}
	s.Prompt = opts.Prompt
	if s.Prompt == "" {
		if s.Prompt, err = readText(f.Folder, f.Agent.Prompt); err != nil {
			return ExitInvalid, fmt.Errorf("agent.prompt: %w", err)
		}
	}
	if s.Mode == agentcli.Headless && s.Prompt == "" {
		return ExitUsage, errors.New("mode headless needs a prompt: set agent.prompt or pass --prompt")
	}
	if s.SystemPrompt, err = readText(f.Folder, f.Agent.SystemPrompt); err != nil {
		return ExitInvalid, fmt.Errorf("agent.system_prompt: %w", err)
	}
	s.Location = opts.Location
	if s.Location == nil {
		switch f.Location.Kind {
		case "", "local":
			s.Location = local.New()
		case "ssh":
			s.Location = SSHLocation(f.Location.SSH, getenv, opts.Stderr)
		case "k8s":
			s.Location = K8sLocation(f.Location.K8s, getenv, opts.Stderr)
		default:
			return ExitRefused, fmt.Errorf("location kind %s is not supported by silo run yet", f.Location.Kind)
		}
	}

	s.ID = NewID()
	s.em.mu.Lock()
	s.em.session = s.ID
	s.em.mu.Unlock()
	root := RuntimeRoot(getenv)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return ExitPrerequisite, err
	}
	s.Dir = filepath.Join(root, s.ID)
	if err := os.Mkdir(s.Dir, 0o700); err != nil {
		return ExitPrerequisite, err
	}
	s.StateDir = filepath.Join(StateRoot(getenv), s.ID)
	if err := os.MkdirAll(s.StateDir, 0o700); err != nil {
		os.RemoveAll(s.Dir)
		return ExitPrerequisite, err
	}
	if err := s.em.openLog(filepath.Join(s.StateDir, EventsFile)); err != nil {
		os.RemoveAll(s.Dir)
		return ExitPrerequisite, err
	}
	kind := f.Location.Kind
	if kind == "" {
		kind = "local"
	}
	s.rec = &recorder{path: filepath.Join(s.Dir, RecordFile), rec: Record{
		ID: s.ID, PID: os.Getpid(), Started: time.Now().UTC(), Silofile: f.File, Name: f.Name,
		CLI: f.Agent.CLI, Location: kind, Mode: s.Mode, Permissions: permString(f.RemotePermissions),
		StateDir: s.StateDir, Events: filepath.Join(s.StateDir, EventsFile), Labels: opts.Labels, Detached: opts.Detached,
		Resources: []Resource{},
	}}
	if err := s.rec.update(func(*Record) {}); err != nil {
		os.RemoveAll(s.Dir)
		return ExitPrerequisite, err
	}
	s.status("session %s (audit: %s)", s.ID, filepath.Join(s.StateDir, "audit.jsonl"))
	if opts.Started != nil {
		opts.Started(s)
	}
	return s.run(ctx)
}

func permString(r silofile.RemotePermissions) string {
	if r.Effective() == silofile.PermSubset {
		return "[" + strings.Join(r.Tools, ", ") + "]"
	}
	return r.Effective()
}

func (s *Session) run(ctx context.Context) (code int, runErr error) {
	sigs := s.opts.Signals
	if sigs == nil {
		ch := make(chan os.Signal, 4)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(ch)
		sigs = ch
	}

	var undos []namedUndo
	// reason is the human account session.json keeps; why is the
	// result's exit reason, decided here for every end but the CLI's
	// own, which the deferred function reads from its output.
	reason, why := "", ""
	cliExited := false
	defer func() {
		s.inj.end()
		s.em.setPhase(events.PhaseTeardown)
		errs := s.teardown(undos)
		s.transcript.close()
		out := s.transcript.outcome()
		var cliErr *events.CLIError
		if cliExited {
			why = events.ReasonCompleted
			switch {
			case out.Failure != nil:
				why, cliErr = events.ReasonCLIError, out.Failure
			case code != 0:
				why = events.ReasonCLIError
				cliErr = &events.CLIError{Kind: events.CLIErrorOther, Message: fmt.Sprintf("the agent CLI exited %d", code)}
				if code == ExitConnLost {
					why, cliErr = events.ReasonConnectionLost, nil
				}
			}
		}
		res := &Result{Ended: time.Now().UTC(), ExitCode: code, Reason: reason}
		if runErr != nil {
			res.Errors = append(res.Errors, runErr.Error())
		}
		for _, e := range errs {
			res.Errors = append(res.Errors, e.Error())
			s.warnf("teardown: %v", e)
		}
		_ = s.rec.update(func(r *Record) { r.Result = res })
		_ = writeJSON(filepath.Join(s.StateDir, RecordFile), s.rec.snapshot())
		// The session directory goes last; the audit log and the final
		// session.json stay under the state directory.
		if err := os.RemoveAll(s.Dir); err != nil {
			s.warnf("teardown: %v", err)
		}
		r := &events.Result{
			Exit:        events.Exit{Code: code, Reason: why, Detail: reason, CLI: cliErr},
			FinalText:   out.FinalText,
			CLISession:  out.CLISession,
			UsageTotals: s.em.totals(),
			AuditPath:   filepath.Join(s.StateDir, audit.FileName),
			Errors:      res.Errors,
		}
		if s.transcript != nil {
			r.TranscriptPath = filepath.Join(s.StateDir, RawTranscriptFile)
			if s.parser != nil {
				r.NormalizedTranscriptPath = filepath.Join(s.StateDir, TranscriptFile)
			}
		}
		e := s.em.emit(events.Event{Type: events.TypeResult, Result: r})
		s.result = e.Result
	}()

	// Setup, ended early by SIGINT, SIGTERM or Cancel.
	setupCtx, cancelSetup := context.WithCancel(ctx)
	defer cancelSetup()
	interrupted := make(chan string, 1)
	stopWatch := make(chan struct{})
	go func() {
		select {
		case sig := <-sigs:
			interrupted <- sig.String()
			cancelSetup()
		case r := <-s.cancel:
			interrupted <- "cancelled: " + r
			cancelSetup()
		case <-stopWatch:
		}
	}()
	setupEnd := func() (int, string, bool) {
		select {
		case what := <-interrupted:
			c := ExitInterrupted
			if strings.HasPrefix(what, "cancelled") || what == syscall.SIGTERM.String() {
				c = ExitTerminated
			}
			return c, what, true
		default:
		}
		return 0, "", false
	}
	for _, st := range Steps() {
		s.emitStatus(st.Name, events.StateStarted, "")
		undo, err := st.Run(setupCtx, s)
		if undo != nil {
			undos = append(undos, namedUndo{st.Name, undo})
		}
		if err == nil && setupCtx.Err() != nil {
			err = setupCtx.Err()
		}
		if err != nil {
			s.emitStatus(st.Name, events.StateFailed, err.Error())
			close(stopWatch)
			if c, what, ok := setupEnd(); ok {
				reason, why = "interrupted during setup ("+what+")", events.ReasonCancelled
				return c, fmt.Errorf("%s: %w", st.Name, err)
			}
			reason, why = "setup failed: "+st.Name, events.ReasonSetupFailed
			return exitCode(err), fmt.Errorf("%s: %w", st.Name, err)
		}
		s.emitStatus(st.Name, events.StateDone, "")
	}
	close(stopWatch)
	if c, what, ok := setupEnd(); ok {
		reason, why = "interrupted during setup ("+what+")", events.ReasonCancelled
		return c, errors.New("interrupted during setup")
	}

	// Step 7: start the CLI. In headless mode its output is also kept
	// and parsed into transcript events.
	stdout := s.opts.Stdout
	if s.Mode == agentcli.Headless {
		t, err := newTranscript(s.em, s.parser, filepath.Join(s.StateDir, RawTranscriptFile), filepath.Join(s.StateDir, TranscriptFile))
		if err != nil {
			reason, why = "the transcript could not be written", events.ReasonSetupFailed
			return ExitPrerequisite, err
		}
		t.onTurn = s.inj.turnEnded
		s.transcript = t
		stdout = io.MultiWriter(stdout, t)
	}
	io := location.IO{Stdin: s.opts.Stdin, Stdout: stdout, Stderr: s.opts.Stderr}
	// start starts one CLI process: the first, or a resumed turn
	// carrying injected messages.
	start := func(cmd location.Command, msgs []string) (location.Agent, error) {
		started, err := s.inj.begin(&cmd, msgs)
		if err != nil {
			return nil, err
		}
		a, err := s.Location.Start(ctx, cmd, io)
		started()
		if err != nil {
			return nil, err
		}
		if lp, ok := a.(interface{ PGID() int }); ok && lp.PGID() > 0 {
			_ = s.AddResource(Resource{Kind: KindProcessGroup, PGID: lp.PGID()})
		}
		return a, nil
	}
	agent, err := start(s.Command, nil)
	if err != nil {
		reason, why = "the agent CLI did not start", events.ReasonSetupFailed
		return ExitPrerequisite, err
	}
	undos = append(undos, namedUndo{"agent", func(context.Context) error { return s.stop(agent) }})
	s.em.setPhase(events.PhaseRunning)
	s.emitStatus("agent", events.StateStarted, fmt.Sprintf("started %s in %s mode", s.File.Agent.CLI, s.Mode))

	// Step 8: serve until the CLI exits, the deadline passes, or a
	// signal or Cancel ends the session.
	type exit struct {
		st  location.ExitStatus
		err error
	}
	exited := make(chan exit, 1)
	wait := func(a location.Agent) {
		st, err := a.Wait()
		exited <- exit{st, err}
	}
	go wait(agent)
	var deadline <-chan time.Time
	var timer *time.Timer
	if s.Deadline > 0 {
		timer = time.NewTimer(s.Deadline)
		deadline = timer.C
	}
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	var grace <-chan time.Time
	silo := 0 // silo's own code once silo has ended the session
	interrupts := 0
	for {
		select {
		case e := <-exited:
			if silo != 0 {
				return silo, nil
			}
			if e.st.ConnLost {
				s.inj.end()
				reason = "the connection to the agent host dropped"
				return ExitConnLost, e.err
			}
			// A message injected for the next turn resumes the CLI's
			// session with it, once this turn has ended well.
			out := s.transcript.outcome()
			clean := e.err == nil && e.st.Code == 0 && out.Failure == nil
			msgs, dropped := s.inj.next(out, clean)
			if dropped != "" {
				s.warnf("%s", dropped)
			}
			if len(msgs) > 0 && s.resume != nil {
				cmd := s.resume(s.stageDir, out.CLISession)
				cmd.Workdir = s.Command.Workdir
				a, err := start(cmd, msgs)
				if err == nil {
					agent = a
					s.status("resumed %s session %s with %d injected message(s)", s.File.Agent.CLI, out.CLISession, len(msgs))
					go wait(agent)
					continue
				}
				s.warnf("resuming the agent CLI with %d injected message(s): %v", len(msgs), err)
			}
			cliExited = true
			reason = "the agent CLI exited"
			if e.st.Signaled != "" {
				reason += " on " + e.st.Signaled
			}
			return e.st.Code, e.err
		case <-deadline:
			s.warnf("the session deadline passed; stopping the agent")
			reason, why = "deadline", events.ReasonDeadline
			return ExitDeadline, nil
		case t := <-s.extend:
			if timer != nil {
				timer.Stop()
			}
			timer = time.NewTimer(time.Until(t))
			deadline = timer.C
			s.status("deadline moved to %s", t.UTC().Format(time.RFC3339))
		case r := <-s.cancel:
			reason, why = "cancelled: "+r, events.ReasonCancelled
			return ExitTerminated, nil
		case <-ctx.Done():
			reason, why = "cancelled: "+ctx.Err().Error(), events.ReasonCancelled
			return ExitTerminated, nil
		case <-grace:
			s.warnf("the agent did not stop within %s of the interrupt; stopping it", s.opts.Grace)
			return ExitInterrupted, nil
		case sig := <-sigs:
			interrupts++
			switch {
			case sig == syscall.SIGTERM:
				reason, why = "terminated", events.ReasonCancelled
				return ExitTerminated, nil
			case interrupts > 1:
				reason, why = "interrupted twice", events.ReasonCancelled
				return ExitInterrupted, nil
			case s.Command.TTY:
				// The CLI shares the terminal, which has delivered the
				// same Ctrl-C to it: the first one is the CLI's, and its
				// exit code stands. A SIGINT sent to silo alone is passed
				// on.
				if !isTerminal(s.opts.Stdin) {
					_ = agent.Signal(location.Interrupt)
				}
				s.status("interrupt passed to the agent; press Ctrl-C again to end the session")
			default:
				_ = agent.Signal(location.Interrupt)
				s.status("interrupted; giving the agent %s to stop (Ctrl-C again to end now)", s.opts.Grace)
				reason, why = "interrupted", events.ReasonCancelled
				silo = ExitInterrupted
				t := time.NewTimer(s.opts.Grace)
				defer t.Stop()
				grace = t.C
			}
		}
	}
}

func signalCode(sig os.Signal) int {
	if sig == syscall.SIGTERM {
		return ExitTerminated
	}
	return ExitInterrupted
}

func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// stop ends the CLI if it is still running: SIGTERM, then SIGKILL after
// the grace period.
func (s *Session) stop(a location.Agent) error {
	done := make(chan struct{})
	go func() {
		a.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	default:
	}
	if err := a.Signal(location.Terminate); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-time.After(s.opts.Grace):
	}
	if err := a.Signal(location.Kill); err != nil {
		return err
	}
	<-done
	return nil
}

type namedUndo struct {
	name string
	undo Undo
}

// teardown runs the undos in reverse (step 9), each with its own time
// limit and regardless of the others' failures.
func (s *Session) teardown(undos []namedUndo) []error {
	var errs []error
	for i := len(undos) - 1; i >= 0; i-- {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		err := func() (err error) {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("panic: %v", p)
				}
			}()
			return undos[i].undo(ctx)
		}()
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", undos[i].name, err))
			s.emitStatus(undos[i].name, events.StateFailed, err.Error())
		} else {
			s.emitStatus(undos[i].name, events.StateDone, "")
		}
	}
	return errs
}
