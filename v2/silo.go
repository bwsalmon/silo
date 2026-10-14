package silo

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/v2/events"
	"github.com/bwsalmon/silo/v2/location"
	"github.com/bwsalmon/silo/v2/sandbox"
	"github.com/bwsalmon/silo/v2/session"
	"github.com/bwsalmon/silo/v2/silofile"
)

// Options configure a library session (grain.md §2.1). All are
// optional.
type Options struct {
	// Prompt replaces agent.prompt.
	Prompt string
	// Credentials are the caller's credentials as values, by the name a
	// Silofile credential has; that credential's from: is then not read.
	Credentials map[string]string
	// Environ is the environment the session is set up from, which
	// decides the session and state directories (XDG_RUNTIME_DIR,
	// XDG_STATE_HOME) and what the CLI inherits; os.Environ() when nil.
	Environ []string
	// Sink, when set, is called with every event, in order, as it is
	// emitted. It must not block for long: the session waits for it.
	Sink func(events.Event)
	// Stderr receives the CLI's stderr; discarded when nil.
	Stderr io.Writer
	// Location is where the CLI runs; chosen from location.kind when nil.
	Location location.Location
	// Backend runs sandboxes; the container backend when nil.
	Backend sandbox.Backend
	// Grace replaces session.DefaultGrace.
	Grace time.Duration
	// GC runs the local silo gc before the session starts.
	GC bool
}

// Result is a session's outcome, as its result event has it.
type Result = events.Result

// ErrNotYet is returned by what a later stage provides.
var ErrNotYet = errors.New("silo: not available until stage 6b")

// Session is one session started by Start. Its methods are safe for
// concurrent use.
type Session struct {
	cancel chan string
	extend chan time.Time
	extMu  sync.Mutex
	done   chan struct{}
	res    Result
	err    error

	qmu    sync.Mutex
	qcond  *sync.Cond
	queue  []events.Event
	ended  bool
	once   sync.Once
	events chan events.Event
}

// Start validates f and starts a session in the background: one process
// can run many at once, each with its own session directory, gateway,
// token and audit log. The Silofile is a Go value; its mode defaults to
// headless, the only mode a library session runs. The session ends
// when the CLI exits, its deadline passes, Cancel is called or ctx is
// done; Wait returns its result.
func Start(ctx context.Context, f silofile.Silofile, opts Options) (*Session, error) {
	if f.Agent.Mode == "" {
		f.Agent.Mode = "headless"
	}
	if f.Agent.Mode != "headless" {
		return nil, errors.New("silo: a library session runs in mode headless")
	}
	var errs silofile.Errors
	for _, e := range f.Validate(silofile.Options{}).Errors {
		if opts.Prompt != "" && e.Path == "agent" && e.Msg == "mode headless needs a prompt" {
			continue
		}
		errs = append(errs, e)
	}
	if len(errs) > 0 {
		return nil, errs
	}
	s := &Session{
		cancel: make(chan string, 1),
		extend: make(chan time.Time, 1),
		done:   make(chan struct{}),
		events: make(chan events.Event),
	}
	s.qcond = sync.NewCond(&s.qmu)
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	so := session.Options{
		File: &f, Prompt: opts.Prompt, Credentials: opts.Credentials,
		Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: stderr,
		Environ: opts.Environ, Location: opts.Location, Backend: opts.Backend, Grace: opts.Grace,
		// A channel nothing sends on: a library session never installs
		// a process-wide signal handler.
		Signals: make(chan os.Signal),
		Cancel:  s.cancel, ExtendDeadline: s.extend,
		NoGC: !opts.GC,
		Sink: session.SinkFunc(func(e events.Event) {
			if opts.Sink != nil {
				opts.Sink(e)
			}
			s.qmu.Lock()
			s.queue = append(s.queue, e)
			s.qmu.Unlock()
			s.qcond.Signal()
		}),
	}
	go func() {
		s.res, s.err = session.Execute(ctx, so)
		s.qmu.Lock()
		s.ended = true
		s.qmu.Unlock()
		s.qcond.Signal()
		close(s.done)
	}()
	return s, nil
}

// Events returns the session's events, in order, ending with result,
// after which the channel is closed. Nothing is lost by calling it
// late; once called, it must be drained.
func (s *Session) Events() <-chan events.Event {
	s.once.Do(func() { go s.pump() })
	return s.events
}

func (s *Session) pump() {
	defer close(s.events)
	for {
		s.qmu.Lock()
		for len(s.queue) == 0 && !s.ended {
			s.qcond.Wait()
		}
		if len(s.queue) == 0 {
			s.qmu.Unlock()
			return
		}
		e := s.queue[0]
		s.queue = s.queue[1:]
		s.qmu.Unlock()
		s.events <- e
	}
}

// Cancel ends the session, as cancelled with reason: the CLI is stopped
// and the session torn down. It returns at once; Wait returns once it
// has ended.
func (s *Session) Cancel(reason string) {
	select {
	case s.cancel <- reason:
	default: // already asked
	}
}

// ExtendDeadline moves the session's deadline to t; a session without
// one gets one. It takes effect once the CLI has started.
func (s *Session) ExtendDeadline(t time.Time) {
	s.extMu.Lock()
	defer s.extMu.Unlock()
	select {
	case <-s.extend: // replace a move not yet taken
	default:
	}
	s.extend <- t
}

// Inject delivers a user message to the CLI mid-run. It is stage 6b's,
// and returns ErrNotYet until then.
func (s *Session) Inject(msg string) error { return ErrNotYet }

// Wait waits for the session to end and returns its result, and the
// error that ended it, if one did.
func (s *Session) Wait() (Result, error) {
	<-s.done
	return s.res, s.err
}
