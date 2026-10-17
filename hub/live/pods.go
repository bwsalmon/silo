package live

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/events"
	"github.com/bwsalmon/silo/hub/notify"
	"github.com/bwsalmon/silo/hub/route"
	"github.com/bwsalmon/silo/hub/tasks"
	"github.com/bwsalmon/silo/internal/logsafe"
)

// Live events across the hub's pods (docs/hub.md §2.2.4). A task's
// events arrive on the pod holding its node, which keeps its ring. A
// subscription that lands on another pod is forwarded to that one over
// the internal listener (InternalEventsPath), and served there as on
// the public route; with no pod holding the node, the subscriber's pod
// serves the stored events from the database and follows the notices
// the ingesting pod sends after each one it stores.

// InternalEventsPath is the internal listener's route for a task's
// events, asked by another pod of the one holding the task's node: the
// public route's Server-Sent Events, from ?from=, with no access check
// (the asking pod has made it). A pod that does not hold the task's
// node answers 404, and a subscription it serves ends, without an end
// event, when it stops holding it.
const InternalEventsPath = "/internal/v1/tasks/{id}/events"

// whyGone ends a subscription whose source went away mid-stream: the
// pod it was forwarded to, or this pod's hold of the task's node. The
// subscriber reconnects from its cursor.
const whyGone = "gone"

// keepaliveEvery is how often a subscription with nothing to send says
// so; a forwarded one silent for forwardIdle has lost its pod.
const (
	keepaliveEvery = 15 * time.Second
	forwardIdle    = 3 * keepaliveEvery
)

// InternalHandler serves InternalEventsPath, for the internal listener,
// behind the listener's own check of the asking pod
// (rendezvous.Server.Internal).
func (h *Hub) InternalHandler() http.Handler {
	m := route.NewMux()
	m.HandleFunc("GET "+InternalEventsPath, h.serveInternalEvents)
	return m
}

func (h *Hub) serveInternalEvents(w http.ResponseWriter, r *http.Request) {
	t, err := h.Tasks.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, tasks.ErrNotFound) {
		// Not 404, which says this pod does not hold the node.
		http.Error(w, "no such task", http.StatusGone)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if t.Node == "" || h.Holds == nil || !h.Holds(t.Node) {
		http.Error(w, "this pod does not hold node "+t.Node, http.StatusNotFound)
		return
	}
	var from int64
	if v := r.URL.Query().Get("from"); v != "" {
		if from, err = strconv.ParseInt(v, 10, 64); err != nil || from < 0 {
			http.Error(w, "from: want a seq, 0 or more", http.StatusBadRequest)
			return
		}
	}
	h.serveSSE(w, r, func(ctx context.Context) (follower, error) { return h.held(ctx, t, from) })
}

// open picks where a subscription to t is served from. A hub that
// cannot forward (Forward is nil: one pod) serves every one from its
// own topic, as does a pod holding t's node. Otherwise a subscription
// is forwarded to the pod holding the node, and with none, or none
// reachable, is served from the store.
func (h *Hub) open(ctx context.Context, t tasks.Task, from int64) (follower, error) {
	if h.Forward == nil {
		return h.local(ctx, t, from)
	}
	if t.Node != "" && h.Holds != nil && h.Holds(t.Node) {
		return h.held(ctx, t, from)
	}
	if t.Node != "" {
		path := strings.Replace(InternalEventsPath, "{id}", t.ID, 1) + "?from=" + strconv.FormatInt(from, 10)
		resp, err := h.Forward(ctx, t.Node, path)
		if err == nil {
			return h.forwarded(resp), nil
		}
		h.logf("live: forwarding task %s's subscription to node %s's pod: %s; serving its stored events", t.ID, t.Node, logsafe.Quote(err.Error()))
	}
	return h.stored(t, from), nil
}

// held is local for a subscription this pod serves because it holds
// t's node: it ends, as whyGone, if this pod stops holding the node,
// whose events then arrive on another pod.
func (h *Hub) held(ctx context.Context, t tasks.Task, from int64) (follower, error) {
	follow, err := h.local(ctx, t, from)
	if err != nil || h.Changes == nil {
		return follow, err
	}
	return func(ctx context.Context, send func(Event) error, keepalive func() error) (string, error) {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		changes, stop := h.Changes()
		defer stop()
		gone := make(chan struct{})
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-changes:
					if !h.Holds(t.Node) {
						close(gone)
						cancel()
						return
					}
				}
			}
		}()
		why, err := follow(ctx, send, keepalive)
		select {
		case <-gone:
			return whyGone, nil
		default:
		}
		return why, err
	}, nil
}

// forwarded follows the Server-Sent Events of a subscription forwarded
// to another pod, passing on its events, its keepalives and its end. A
// stream that ends without an end event, or goes silent, is whyGone.
func (h *Hub) forwarded(resp *http.Response) follower {
	return func(ctx context.Context, send func(Event) error, keepalive func() error) (string, error) {
		defer resp.Body.Close()
		idle := time.AfterFunc(forwardIdle, func() { resp.Body.Close() })
		defer idle.Stop()
		br := bufio.NewReaderSize(resp.Body, 4096)
		var typ string
		var data []byte
		for {
			line, err := readLimited(br, 2*maxLine)
			if err != nil {
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				return whyGone, nil
			}
			idle.Reset(forwardIdle)
			switch {
			case len(line) == 0:
				if typ == "end" {
					return whyEnd, nil
				}
				if data != nil {
					var e Event
					if err := json.Unmarshal(data, &e); err != nil {
						return "", err
					}
					if err := send(e); err != nil {
						return "", err
					}
				}
				typ, data = "", nil
			case line[0] == ':':
				if keepalive != nil {
					if err := keepalive(); err != nil {
						return "", err
					}
				}
			case bytes.HasPrefix(line, []byte("event: ")):
				typ = string(line[len("event: "):])
			case bytes.HasPrefix(line, []byte("data: ")):
				data = append([]byte(nil), line[len("data: "):]...)
			}
		}
	}
}

// stored follows t's stored events from the store, for a task no pod
// holds the node of: each event the store has after the cursor, read
// again at each notice of a new one (Notice), of the task's cancel, of
// the bus reconnecting, and every keepaliveEvery. It ends with the
// task's result, or once the task is over and every event it stored is
// sent. Its token events, which no store keeps, are not seen.
func (h *Hub) stored(t tasks.Task, from int64) follower {
	return func(ctx context.Context, send func(Event) error, keepalive func() error) (string, error) {
		poke, stop := h.wait(t.ID)
		defer stop()
		tick := time.NewTicker(keepaliveEvery)
		defer tick.Stop()
		cursor := from
		for {
			// The task first: an event stored before it was over is then
			// read below.
			task, err := h.Tasks.Get(ctx, t.ID)
			if err != nil {
				return "", err
			}
			evs, err := h.Log.Since(ctx, t.ID, cursor, math.MaxInt64)
			if err != nil {
				return "", err
			}
			for _, e := range evs {
				if err := send(e); err != nil {
					return "", err
				}
				cursor = e.Seq
				if e.Type == events.TypeResult {
					return whyEnd, nil
				}
			}
			if task.State.Terminal() {
				return whyEnd, nil
			}
			select {
			case <-poke:
			case <-tick.C:
				if keepalive != nil {
					if err := keepalive(); err != nil {
						return "", err
					}
				}
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
	}
}

// wait returns a channel poked (coalesced) by each notice that concerns
// task, and a function that stops it.
func (h *Hub) wait(task string) (<-chan struct{}, func()) {
	c := make(chan struct{}, 1)
	h.wmu.Lock()
	if h.waiters == nil {
		h.waiters = map[string]map[chan struct{}]struct{}{}
	}
	if h.waiters[task] == nil {
		h.waiters[task] = map[chan struct{}]struct{}{}
	}
	h.waiters[task][c] = struct{}{}
	h.wmu.Unlock()
	return c, func() {
		h.wmu.Lock()
		defer h.wmu.Unlock()
		delete(h.waiters[task], c)
		if len(h.waiters[task]) == 0 {
			delete(h.waiters, task)
		}
	}
}

// Notice is a notice from the Bus: a stored event, or a cancel, pokes
// the subscriptions this pod serves from the store for that task, and
// the bus reconnecting pokes all of them, since a notice may have been
// lost meanwhile.
func (h *Hub) Notice(m notify.Message) {
	h.wmu.Lock()
	defer h.wmu.Unlock()
	poke := func(cs map[chan struct{}]struct{}) {
		for c := range cs {
			select {
			case c <- struct{}{}:
			default:
			}
		}
	}
	switch {
	case m.Reconnected:
		for _, cs := range h.waiters {
			poke(cs)
		}
	case m.Event != "":
		poke(h.waiters[m.Event])
	case m.Cancel != "":
		poke(h.waiters[m.Cancel])
	}
}

// announce tells the other pods, through the Bus, that task's event seq
// is stored. It does not wait for the database: the bus sends the
// notice soon after, coalesced with the task's later ones (Bus.Post).
// A notice that is lost or late holds a subscription only until it
// reads the store again, within keepaliveEvery.
func (h *Hub) announce(task string, seq int64) {
	if h.Bus == nil {
		return
	}
	h.Bus.Post(notify.Message{Event: task, Seq: seq})
}

// readLimited reads one line, without its end, of at most max bytes.
func readLimited(r *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			if err == io.EOF && len(line) > 0 {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		line = append(line, chunk...)
		if len(line) > max {
			return nil, errors.New("live: line too long")
		}
		if !isPrefix {
			return line, nil
		}
	}
}
