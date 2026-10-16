package live

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/websocket"

	"github.com/bwsalmon/silo/hub/directory"
	"github.com/bwsalmon/silo/hub/rendezvous"
	"github.com/bwsalmon/silo/hub/tasks"
)

// Paths the Hub serves.
const (
	// EventsPath is a task's events: Server-Sent Events, or a WebSocket
	// when the request is an upgrade.
	EventsPath = tasks.TasksPath + "/{id}/events"
	// ArtifactsPath is a task's node-only artifacts, by ?ref=.
	ArtifactsPath = tasks.TasksPath + "/{id}/artifacts"
)

// viewTimeout is how long a node has to answer a view.
const viewTimeout = 30 * time.Second

// Handler serves the paths subscribers and viewers use:
//
//	GET /v1/tasks/{id}/events?from=<seq>     the task's events after seq (0, the start, by default)
//	GET /v1/tasks/{id}/artifacts?ref=<ref>   a node-only artifact, read through from its node
//
// The events are Server-Sent Events, each "id: <seq>", "event: <type>"
// and "data: <the event>"; an EventSource's Last-Event-ID is the cursor
// when from is absent. A request that upgrades to a WebSocket gets one
// text message per event instead. Either way the hub replays what it
// holds after the cursor and then follows, and
// ends with {"type":"end"} (SSE: "event: end") once the task's events
// are over. A subscriber that falls behind is dropped without it, and
// reconnects from its last seq. Seqs have gaps: an event the hub
// rejected, or a token event its ring no longer holds, is not replayed.
//
// The caller must see the task, as for GET /v1/tasks/{id}, at every
// subscription and every view: a task they may not see answers 404.
func (h *Hub) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET "+EventsPath, h.serveEvents)
	m.HandleFunc("GET "+ArtifactsPath, h.serveArtifact)
	return m
}

// task signs r's caller in and loads the task its path names, if they
// may see it, or answers the request itself.
func (h *Hub) task(w http.ResponseWriter, r *http.Request) (tasks.Task, bool) {
	acc, err := h.Directory.Access(r)
	if err != nil {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return tasks.Task{}, false
	}
	t, err := h.Tasks.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, tasks.ErrNotFound) || (err == nil && !sees(acc, t)) {
		http.Error(w, "no such task", http.StatusNotFound)
		return tasks.Task{}, false
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return tasks.Task{}, false
	}
	return t, true
}

// sees is the backlog's rule: a member of the task's team, or, for a
// repository's task, of the team that owns the repository now.
func sees(acc *directory.Access, t tasks.Task) bool {
	return acc.SeesTeam(t.Team) || (t.Repo != "" && acc.SeesRepo(t.Repo))
}

// endMessage is the last message of a subscription whose task's events
// are over.
type endMessage struct {
	Type string `json:"type"`
}

func (h *Hub) serveEvents(w http.ResponseWriter, r *http.Request) {
	t, ok := h.task(w, r)
	if !ok {
		return
	}
	cursor := r.URL.Query().Get("from")
	if cursor == "" {
		cursor = r.Header.Get("Last-Event-ID")
	}
	var from int64
	if cursor != "" {
		n, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || n < 0 {
			http.Error(w, "from: want a seq, 0 or more", http.StatusBadRequest)
			return
		}
		from = n
	}
	open := func(ctx context.Context) (follower, error) { return h.open(ctx, t, from) }
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		h.serveWebSocket(w, r, t, open)
		return
	}
	h.serveSSE(w, r, open)
}

// follower delivers a subscription to send, as Hub.follow does, and
// says why it ended.
type follower func(ctx context.Context, send func(Event) error, keepalive func() error) (string, error)

// local is the follower for a subscription this pod serves from its
// own topic.
func (h *Hub) local(ctx context.Context, t tasks.Task, from int64) (follower, error) {
	sn, err := h.subscribe(ctx, t, from)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, send func(Event) error, keepalive func() error) (string, error) {
		return h.follow(ctx, sn, send, keepalive)
	}, nil
}

func (h *Hub) serveSSE(w http.ResponseWriter, r *http.Request, open func(ctx context.Context) (follower, error)) {
	rc := http.NewResponseController(w)
	follow, err := open(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc.Flush()
	why, err := follow(r.Context(), func(e Event) error {
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Type, b); err != nil {
			return err
		}
		return rc.Flush()
	}, func() error {
		if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
			return err
		}
		return rc.Flush()
	})
	if err == nil && why == whyEnd {
		io.WriteString(w, "event: end\ndata: {\"type\":\"end\"}\n\n")
		rc.Flush()
	}
}

func (h *Hub) serveWebSocket(w http.ResponseWriter, r *http.Request, t tasks.Task, open func(ctx context.Context) (follower, error)) {
	ws := websocket.Server{
		// A browser's request must come from the hub's own origin, or
		// another site could subscribe with the viewer's sign-on. The
		// SDK sends no Origin.
		Handshake: func(cfg *websocket.Config, r *http.Request) error {
			o := r.Header.Get("Origin")
			if o == "" {
				return nil
			}
			u, err := url.Parse(o)
			if err != nil || !strings.EqualFold(u.Host, r.Host) {
				return fmt.Errorf("live: origin %q is not this hub's", o)
			}
			return nil
		},
		Handler: func(c *websocket.Conn) {
			defer c.Close()
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			// The subscriber sends nothing; a read ending is its leaving.
			go func() {
				var discard []byte
				for websocket.Message.Receive(c, &discard) == nil {
				}
				cancel()
			}()
			follow, err := open(ctx)
			if err != nil {
				h.logf("live: subscribing to task %s: %v", t.ID, err)
				return
			}
			why, err := follow(ctx, func(e Event) error {
				return websocket.JSON.Send(c, e)
			}, nil)
			if err == nil && why == whyEnd {
				websocket.JSON.Send(c, endMessage{Type: "end"})
			}
		},
	}
	ws.ServeHTTP(w, r)
}

// The view stream (docs/hub.md §7.2) is a stream the hub opens to a
// node (rendezvous.Server.Open) to read one artifact through. The hub
// writes one line; the node answers one line, then, after ok, the
// artifact's bytes until it closes the stream:
//
//	hub → node  {"type":"view","task":"<hub id>","node_task":"<local id>","ref":"<ref>"}
//	node → hub  {"type":"ok"} and the bytes
//	            {"type":"error","error":"..."}
//
// A node serves only refs it has itself listed in its events; the hub
// passes the bytes to its viewer as they come and keeps none of them.
const (
	TypeView  = "view"
	TypeOK    = "ok"
	TypeError = "error"
)

// ViewMessage is a line on a view stream.
type ViewMessage struct {
	Type     string `json:"type"`
	Task     string `json:"task,omitempty"`
	NodeTask string `json:"node_task,omitempty"`
	Ref      string `json:"ref,omitempty"`
	Error    string `json:"error,omitempty"`
}

func (h *Hub) serveArtifact(w http.ResponseWriter, r *http.Request) {
	t, ok := h.task(w, r)
	if !ok {
		return
	}
	ref := r.URL.Query().Get("ref")
	if !validRef(ref) {
		http.Error(w, "ref: want an artifact's ref", http.StatusBadRequest)
		return
	}
	if t.Node == "" || h.Open == nil {
		http.Error(w, "no node holds this task's artifacts", http.StatusNotFound)
		return
	}
	if err := h.holds(t); err != nil {
		// Not the node the task ran on, though it may have its ID.
		h.logf("live: not asking node %q for task %s's artifact: %v", t.Node, t.ID, err)
		http.Error(w, "unavailable: node "+t.Node+", which holds it, is offline", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), viewTimeout)
	c, err := h.Open(ctx, t.Node)
	cancel()
	if errors.Is(err, rendezvous.ErrOffline) {
		http.Error(w, "unavailable: node "+t.Node+", which holds it, is offline", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		http.Error(w, "reaching node "+t.Node+": "+err.Error(), http.StatusBadGateway)
		return
	}
	defer c.Close()
	// The viewer leaving ends the read from the node.
	stop := context.AfterFunc(r.Context(), func() { c.Close() })
	defer stop()

	b, _ := json.Marshal(ViewMessage{Type: TypeView, Task: t.ID, NodeTask: t.NodeTask, Ref: ref})
	c.SetDeadline(time.Now().Add(viewTimeout))
	if _, err := c.Write(append(b, '\n')); err != nil {
		http.Error(w, "asking node "+t.Node+": "+err.Error(), http.StatusBadGateway)
		return
	}
	br := bufio.NewReaderSize(c, 4096)
	line, err := readLine(br)
	var ans ViewMessage
	if err == nil {
		err = json.Unmarshal(line, &ans)
	}
	switch {
	case err != nil:
		http.Error(w, "node "+t.Node+" did not answer: "+err.Error(), http.StatusBadGateway)
		return
	case ans.Type == TypeError:
		http.Error(w, "node "+t.Node+": "+ans.Error, http.StatusNotFound)
		return
	case ans.Type != TypeOK:
		http.Error(w, fmt.Sprintf("node %s answered %q", t.Node, ans.Type), http.StatusBadGateway)
		return
	}
	c.SetDeadline(time.Time{})
	// The bytes are the node's: never rendered as the hub's own page,
	// never cached on the way.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(flushWriter{w, http.NewResponseController(w)}, br); err != nil {
		h.logf("live: viewing %s of task %s: %v", ref, t.ID, err)
	}
}

// flushWriter flushes each write, so the viewer sees the bytes as the
// node sends them and the hub holds none.
type flushWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (f flushWriter) Write(b []byte) (int, error) {
	n, err := f.w.Write(b)
	if err == nil {
		f.rc.Flush()
	}
	return n, err
}

// ServeView answers one view stream on a node: what a node's grain does
// with a stream the hub opened whose first line is a view (it reads that
// line itself, to tell a view from a task stream, and passes it in).
// open returns the artifact's bytes, or an error the viewer is shown;
// it must refuse any ref the node has not itself listed.
func ServeView(c net.Conn, m ViewMessage, open func(m ViewMessage) (io.ReadCloser, error)) error {
	defer c.Close()
	rc, err := open(m)
	if err != nil {
		b, _ := json.Marshal(ViewMessage{Type: TypeError, Error: err.Error()})
		_, werr := c.Write(append(b, '\n'))
		return werr
	}
	defer rc.Close()
	b, _ := json.Marshal(ViewMessage{Type: TypeOK})
	if _, err := c.Write(append(b, '\n')); err != nil {
		return err
	}
	_, err = io.Copy(c, rc)
	return err
}
