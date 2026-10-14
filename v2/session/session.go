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

	"github.com/bwsalmon/silo/v2/agentcli"
	"github.com/bwsalmon/silo/v2/credentials"
	"github.com/bwsalmon/silo/v2/gateway"
	"github.com/bwsalmon/silo/v2/gateway/audit"
	"github.com/bwsalmon/silo/v2/location"
	"github.com/bwsalmon/silo/v2/location/local"
	"github.com/bwsalmon/silo/v2/mcp"
	"github.com/bwsalmon/silo/v2/sandbox"
	"github.com/bwsalmon/silo/v2/silofile"

	// The CLI mappings silo run drives.
	_ "github.com/bwsalmon/silo/v2/agentcli/claude"
	_ "github.com/bwsalmon/silo/v2/agentcli/codex"
	_ "github.com/bwsalmon/silo/v2/agentcli/custom"
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
	// Signals delivers SIGINT and SIGTERM; signal.Notify's when nil.
	Signals <-chan os.Signal
	// Grace replaces DefaultGrace.
	Grace time.Duration
	// Sink receives the session's events; progress lines on Stderr when
	// nil. Stage 6a's --json attaches its event stream here.
	Sink Sink
	// NoGC skips the local silo gc at start.
	NoGC bool
}

// Event is one thing that happened in a session.
type Event struct {
	Time time.Time
	// Type is status, warning, step or exit.
	Type    string
	Message string
	// Code is the exit code, for exit.
	Code int
}

// Sink receives events.
type Sink interface{ Event(Event) }

type textSink struct{ w io.Writer }

func (t textSink) Event(e Event) {
	switch e.Type {
	case "warning":
		fmt.Fprintf(t.w, "silo: warning: %s\n", e.Message)
	case "step":
	default:
		fmt.Fprintf(t.w, "silo: %s\n", e.Message)
	}
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

	// Command is the CLI's command, once staged.
	Command location.Command

	opts Options
	rec  *recorder
	sink Sink

	hookMu       sync.Mutex
	gatewayHooks []func(*gateway.Gateway)
}

func (s *Session) getenv(k string) string { return agentcli.Getenv(s.opts.Environ, k) }

func (s *Session) status(format string, args ...any) {
	s.sink.Event(Event{Time: time.Now(), Type: "status", Message: fmt.Sprintf(format, args...)})
}

func (s *Session) warnf(format string, args ...any) {
	s.sink.Event(Event{Time: time.Now(), Type: "warning", Message: fmt.Sprintf(format, args...)})
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
	if opts.File == nil {
		return ExitUsage, errors.New("session: no Silofile")
	}
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
	s := &Session{File: opts.File, opts: opts, sink: opts.Sink, AgentEnv: map[string]string{}}
	if s.sink == nil {
		s.sink = textSink{opts.Stderr}
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
		default:
			return ExitRefused, fmt.Errorf("location kind %s is not supported by silo run yet", f.Location.Kind)
		}
	}

	s.ID = NewID()
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
	kind := f.Location.Kind
	if kind == "" {
		kind = "local"
	}
	s.rec = &recorder{path: filepath.Join(s.Dir, RecordFile), rec: Record{
		ID: s.ID, PID: os.Getpid(), Started: time.Now().UTC(), Silofile: f.File, Name: f.Name,
		CLI: f.Agent.CLI, Location: kind, Mode: s.Mode, Permissions: permString(f.RemotePermissions),
		StateDir: s.StateDir, Resources: []Resource{},
	}}
	if err := s.rec.update(func(*Record) {}); err != nil {
		os.RemoveAll(s.Dir)
		return ExitPrerequisite, err
	}
	s.status("session %s (audit: %s)", s.ID, filepath.Join(s.StateDir, "audit.jsonl"))
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
	reason := ""
	defer func() {
		errs := s.teardown(undos)
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
		s.sink.Event(Event{Time: time.Now(), Type: "exit", Code: code, Message: fmt.Sprintf("session %s ended: %s (exit %d)", s.ID, reason, code)})
	}()

	// Setup, ended early by SIGINT or SIGTERM.
	setupCtx, cancelSetup := context.WithCancel(ctx)
	defer cancelSetup()
	interrupted := make(chan os.Signal, 1)
	stopWatch := make(chan struct{})
	go func() {
		select {
		case sig := <-sigs:
			interrupted <- sig
			cancelSetup()
		case <-stopWatch:
		}
	}()
	for _, st := range Steps() {
		s.sink.Event(Event{Time: time.Now(), Type: "step", Message: st.Name})
		undo, err := st.Run(setupCtx, s)
		if undo != nil {
			undos = append(undos, namedUndo{st.Name, undo})
		}
		if err == nil && setupCtx.Err() != nil {
			err = setupCtx.Err()
		}
		if err != nil {
			close(stopWatch)
			select {
			case sig := <-interrupted:
				reason = "interrupted during setup"
				return signalCode(sig), fmt.Errorf("%s: %w", st.Name, err)
			default:
			}
			reason = "setup failed: " + st.Name
			return exitCode(err), fmt.Errorf("%s: %w", st.Name, err)
		}
	}
	close(stopWatch)
	select {
	case sig := <-interrupted:
		reason = "interrupted during setup"
		return signalCode(sig), errors.New("interrupted during setup")
	default:
	}

	// Step 7: start the CLI.
	io := location.IO{Stdin: s.opts.Stdin, Stdout: s.opts.Stdout, Stderr: s.opts.Stderr}
	agent, err := s.Location.Start(ctx, s.Command, io)
	if err != nil {
		reason = "the agent CLI did not start"
		return ExitPrerequisite, err
	}
	if lp, ok := agent.(interface{ PGID() int }); ok && lp.PGID() > 0 {
		_ = s.AddResource(Resource{Kind: KindProcessGroup, PGID: lp.PGID()})
	}
	undos = append(undos, namedUndo{"agent", func(context.Context) error { return s.stop(agent) }})
	s.status("started %s in %s mode", s.File.Agent.CLI, s.Mode)

	// Step 8: serve until the CLI exits, the deadline passes, or a
	// signal ends the session.
	type exit struct {
		st  location.ExitStatus
		err error
	}
	exited := make(chan exit, 1)
	go func() {
		st, err := agent.Wait()
		exited <- exit{st, err}
	}()
	var deadline <-chan time.Time
	if s.Deadline > 0 {
		t := time.NewTimer(s.Deadline)
		defer t.Stop()
		deadline = t.C
	}
	var grace <-chan time.Time
	silo := 0 // silo's own code once silo has ended the session
	interrupts := 0
	for {
		select {
		case e := <-exited:
			if silo != 0 {
				return silo, nil
			}
			reason = "the agent CLI exited"
			if e.st.Signaled != "" {
				reason += " on " + e.st.Signaled
			}
			return e.st.Code, e.err
		case <-deadline:
			s.warnf("the session deadline (%s) passed; stopping the agent", s.Deadline)
			reason = "deadline"
			return ExitDeadline, nil
		case <-grace:
			s.warnf("the agent did not stop within %s of the interrupt; stopping it", s.opts.Grace)
			return ExitInterrupted, nil
		case sig := <-sigs:
			interrupts++
			switch {
			case sig == syscall.SIGTERM:
				reason = "terminated"
				return ExitTerminated, nil
			case interrupts > 1:
				reason = "interrupted twice"
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
				reason = "interrupted"
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
		}
	}
	return errs
}
