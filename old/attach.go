package silo

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/bwsalmon/silo/old/agentcli"
	"github.com/bwsalmon/silo/old/control"
	"github.com/bwsalmon/silo/old/events"
	"github.com/bwsalmon/silo/old/session"
)

// AttachOptions configure Attach and List. All are optional.
type AttachOptions struct {
	// Environ decides where session and state directories are
	// (XDG_RUNTIME_DIR, XDG_STATE_HOME), as for Options.Environ;
	// os.Environ() when nil.
	Environ []string
}

func (o AttachOptions) getenv(k string) string {
	if o.Environ == nil {
		return os.Getenv(k)
	}
	return agentcli.Getenv(o.Environ, k)
}

// SessionInfo is a live session as List finds it: its session.json.
type SessionInfo = session.Record

// List returns the sessions running under this user, oldest first:
// those carrying every label in labels, or all when labels is empty
// (silo ls). A caller finds its own by the labels it started them with
// (grain.md §2.7).
func List(labels map[string]string, opts AttachOptions) ([]SessionInfo, error) {
	return session.List(opts.getenv, labels)
}

// ErrNoResult is an attached session whose events end without a
// result: its process was killed. silo gc removes what it left.
var ErrNoResult = session.ErrNoResult

// Attached is a session followed from its events.jsonl, as by silo
// attach: one started by silo run --detach, or by another process. Its
// methods are safe for concurrent use.
type Attached struct {
	// ID is the session's id.
	ID string

	getenv func(string) string
	cancel context.CancelFunc
	done   chan struct{}
	res    Result
	err    error
	q      *queue
}

// Attach follows session id from the cursor from: its events whose seq
// is after from are delivered by Events, those already written first,
// then the rest as the session emits them, ending with result. A caller
// that keeps the seq of the last event it handled, and attaches from it
// after a restart, sees every event once. A session that has ended is
// replayed to its result. Inject, Cancel, ExtendDeadline and Approve go
// to the session's control socket.
func Attach(ctx context.Context, id string, from int64, opts AttachOptions) (*Attached, error) {
	if !session.ValidID(id) {
		return nil, errors.New("silo: " + id + " is not a session id")
	}
	if _, err := os.Stat(session.EventsPath(opts.getenv, id)); err != nil {
		return nil, errors.New("silo: no session " + id + ": " + err.Error())
	}
	ctx, cancel := context.WithCancel(ctx)
	a := &Attached{ID: id, getenv: opts.getenv, cancel: cancel, done: make(chan struct{}), q: newQueue()}
	go func() {
		defer close(a.done)
		defer a.q.end()
		a.err = session.Follow(ctx, opts.getenv, id, from, func(e events.Event) error {
			if e.Result != nil {
				a.res = *e.Result
			}
			a.q.push(e)
			return nil
		})
	}()
	return a, nil
}

// Events returns the session's events, in order, ending with result,
// after which the channel is closed; it is closed early when following
// fails (Wait says why). Nothing is lost by calling it late; once
// called, it must be drained.
func (a *Attached) Events() <-chan events.Event { return a.q.channel() }

// Wait waits for the result event and returns it. Its error is
// ErrNoResult when the session's process died without one, or the
// error that stopped the following.
func (a *Attached) Wait() (Result, error) {
	<-a.done
	return a.res, a.err
}

// Detach stops following the session, which goes on, and closes the
// Events channel, which need not then be drained. Wait then returns
// context.Canceled unless the result had already arrived.
func (a *Attached) Detach() {
	a.cancel()
	a.q.stop()
}

func (a *Attached) call(method string, params, result any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return control.Call(ctx, session.ControlSocket(a.getenv, a.ID), method, params, result)
}

// Inject gives the CLI a user message, as Session.Inject does.
func (a *Attached) Inject(msg string) (Delivery, error) {
	var r session.InjectResult
	err := a.call("inject", session.InjectParams{Message: msg}, &r)
	return r.Delivery, err
}

// Cancel ends the session, as cancelled with reason.
func (a *Attached) Cancel(reason string) error {
	return a.call("cancel", session.CancelParams{Reason: reason}, nil)
}

// ExtendDeadline moves the session's deadline to t.
func (a *Attached) ExtendDeadline(t time.Time) error {
	return a.call("extend_deadline", session.ExtendParams{Deadline: t}, nil)
}

// Approve answers a gateway call waiting for approval, as
// Session.Approve does.
func (a *Attached) Approve(id string, allow bool) error {
	return a.call("approve", session.ApproveParams{ID: id, Deny: !allow}, nil)
}

// queue hands events from the goroutine that has them to a channel that
// may be read late, without holding the producer up.
type queue struct {
	mu    sync.Mutex
	cond  *sync.Cond
	items []events.Event
	ended bool
	once  sync.Once
	ch    chan events.Event
	quit  chan struct{}
	qonce sync.Once
}

func newQueue() *queue {
	q := &queue{ch: make(chan events.Event), quit: make(chan struct{})}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *queue) push(e events.Event) {
	q.mu.Lock()
	q.items = append(q.items, e)
	q.mu.Unlock()
	q.cond.Signal()
}

func (q *queue) end() {
	q.mu.Lock()
	q.ended = true
	q.mu.Unlock()
	q.cond.Signal()
}

// stop ends the channel at once, dropping what it has not delivered,
// for a reader that has stopped reading.
func (q *queue) stop() {
	q.qonce.Do(func() { close(q.quit) })
	q.end()
}

func (q *queue) channel() <-chan events.Event {
	q.once.Do(func() { go q.pump() })
	return q.ch
}

func (q *queue) pump() {
	defer close(q.ch)
	for {
		q.mu.Lock()
		for len(q.items) == 0 && !q.ended {
			q.cond.Wait()
		}
		if len(q.items) == 0 {
			q.mu.Unlock()
			return
		}
		e := q.items[0]
		q.items = q.items[1:]
		q.mu.Unlock()
		select {
		case q.ch <- e:
		case <-q.quit:
			return
		}
	}
}
