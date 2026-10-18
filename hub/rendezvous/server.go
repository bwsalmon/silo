package rendezvous

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/websocket"

	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/route"
	"github.com/bwsalmon/silo/internal/logsafe"
	"github.com/bwsalmon/silo/internal/tunnel/mux"
)

// DefaultHeartbeat is the heartbeat interval the hub asks for when
// Server.Heartbeat is zero.
const DefaultHeartbeat = 15 * time.Second

// helloTimeout is how long a node has, after the handshake, to say hello.
const helloTimeout = 30 * time.Second

// ErrOffline is Open naming a node with no connection.
var ErrOffline = errors.New("rendezvous: node is not connected")

// Server is the hub's end of the rendezvous: it serves ConnectPath to
// nodes and DirectoryPath to whatever the caller puts in front of it.
type Server struct {
	// Registry is the directory nodes are registered in.
	Registry *registry.Registry
	// Authenticate proves a node's identity from its handshake request
	// (its bearer token): the owner to register it under and, for a
	// credential bound to one node (a node token, docs/hub.md §5.2),
	// that node's ID. A node is never registered under an identity it
	// merely claims, nor under an ID its credential was not minted for.
	Authenticate func(r *http.Request) (Identity, error)
	// Admit, when set, decides whether owner may register n: the team
	// directory's check (docs/hub.md §5) that the owner may serve the
	// repositories n lists. Nil admits every authenticated node.
	Admit func(owner string, n registry.Node) error
	// Visible, when set, authorizes a request for DirectoryPath and says
	// which entries its caller may see (docs/hub.md §5.3: a human sees
	// the nodes of their own teams). An error is answered 401. Nil
	// serves every entry to every caller.
	Visible func(r *http.Request) (func(registry.Entry) bool, error)
	// NodeStream, when set, is handed each stream a node opens after its
	// control stream, for the event streams of docs/hub.md §7. Nil
	// resets them.
	NodeStream func(nodeID string, c net.Conn)
	// FileTask, when set, files the task a node asks the hub to file on
	// its control stream (TypeFileTask): nodeID and owner are the node
	// as its credential proved and registered it, never as the filing
	// says, and the answer is the task filed, as JSON, or why none was:
	// tasks.Filer.File. Nil refuses every filing.
	FileTask func(ctx context.Context, nodeID, owner string, filing json.RawMessage) (json.RawMessage, error)
	// Heartbeat is the interval the hub asks nodes to heartbeat at;
	// DefaultHeartbeat when zero. Set Registry.StaleAfter to a few of
	// them.
	Heartbeat time.Duration
	// Logf, when set, receives one line per connection event.
	Logf func(format string, args ...any)
	// Observe, when set, is told of each connection event, for the
	// hub's metrics: a node welcomed (EventConnected), its connection
	// ending (EventDisconnected), or a connection refused before a node
	// was registered (EventRefused).
	Observe func(ev Event)
	// InternalKey, when set, is the key the hub's pods share for their
	// internal listeners (docs/hub.md §2.2.3, at least MinInternalKey
	// bytes): Open then reaches a node another pod holds, as the shared
	// registry (Registry.Store) names it, through that pod's
	// InternalHandler, and this pod's InternalHandler admits the others,
	// under Registry.Pod's name. Nil opens only the nodes this pod holds.
	InternalKey []byte
	// DialInternal, when set, dials another pod's internal listener;
	// net.Dialer's when nil.
	DialInternal func(ctx context.Context, network, addr string) (net.Conn, error)

	mu      sync.Mutex
	conns   map[string]*nodeConn
	closing string // why Shutdown is ending every connection, once called

	clientOnce sync.Once
	client     *http.Client // Get's, to the other pods' internal listeners
}

type nodeConn struct {
	m     *mux.Mux
	ctl   *lines
	lease *registry.Lease
}

// ErrUnavailable wraps an Admit error that is not a refusal but the
// hub failing to decide (its database is away): the node is not told it
// is refused, which it would not retry (Client.Run), but has its
// connection closed, which it retries.
var ErrUnavailable = errors.New("rendezvous: the hub cannot admit nodes now")

// ErrShuttingDown is a node connecting to a hub that Shutdown has
// been called on: answered 503, which the node retries.
var ErrShuttingDown = errors.New("rendezvous: the hub is shutting down")

// Event is a connection event, for Server.Observe.
type Event string

// The connection events.
const (
	EventConnected    Event = "connected"
	EventDisconnected Event = "disconnected"
	EventRefused      Event = "refused"
)

func (s *Server) observe(ev Event) {
	if s.Observe != nil {
		s.Observe(ev)
	}
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// Handler serves ConnectPath and DirectoryPath. Without Visible the
// directory is served unauthenticated: the caller fronts it with the
// hub's human sign-on, or serves only ConnectPath.
func (s *Server) Handler() http.Handler {
	m := route.NewMux()
	m.Handle("GET "+ConnectPath, http.HandlerFunc(s.serveConnect))
	m.Handle("GET "+DirectoryPath, http.HandlerFunc(s.serveDirectory))
	return m
}

func (s *Server) serveDirectory(w http.ResponseWriter, r *http.Request) {
	nodes := s.Registry.List()
	if s.Visible != nil {
		keep, err := s.Visible(r)
		if err != nil {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		seen := nodes[:0]
		for _, e := range nodes {
			if keep(e) {
				seen = append(seen, e)
			}
		}
		nodes = seen
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Nodes []registry.Entry `json:"nodes"`
	}{nodes})
}

func (s *Server) serveConnect(w http.ResponseWriter, r *http.Request) {
	if s.Authenticate == nil {
		http.Error(w, "node authentication is not configured", http.StatusServiceUnavailable)
		return
	}
	s.mu.Lock()
	closing := s.closing != ""
	s.mu.Unlock()
	if closing {
		http.Error(w, ErrShuttingDown.Error(), http.StatusServiceUnavailable)
		return
	}
	id, err := s.Authenticate(r)
	if err == nil && id.Owner == "" {
		err = errors.New("no owner")
	}
	if err != nil {
		s.logf("rendezvous: refused a node from %s: %v", r.RemoteAddr, err)
		s.observe(EventRefused)
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	ws := websocket.Server{
		Handshake: func(cfg *websocket.Config, r *http.Request) error {
			for _, p := range cfg.Protocol {
				if p == Protocol {
					cfg.Protocol = []string{Protocol}
					return nil
				}
			}
			return fmt.Errorf("rendezvous: subprotocol %q not offered", Protocol)
		},
		Handler: func(c *websocket.Conn) {
			c.PayloadType = websocket.BinaryFrame
			s.serveNode(r.Context(), id, c)
		},
	}
	ws.ServeHTTP(w, r)
}

// serveNode runs one node's connection until it ends.
func (s *Server) serveNode(ctx context.Context, id Identity, c *websocket.Conn) {
	owner := id.Owner
	m := mux.NewMux(c, mux.Config{IsServer: true})
	defer m.Close()

	ctl, err := acceptTimeout(m, helloTimeout)
	if err != nil {
		s.logf("rendezvous: no control stream from %s (%s): %v", owner, c.Request().RemoteAddr, err)
		return
	}
	l := newLines(ctl)
	ctl.SetReadDeadline(time.Now().Add(helloTimeout))
	hello, err := l.read()
	ctl.SetReadDeadline(time.Time{})
	if err != nil || hello.Type != TypeHello || hello.Node == nil {
		s.logf("rendezvous: no hello from %s: %s", owner, logsafe.Quote(err.Error()))
		l.write(Message{Type: TypeError, Error: "expected hello"})
		return
	}
	refuse := func(err error) {
		s.observe(EventRefused)
		s.logf("rendezvous: refused node %s of %s: %s", logsafe.Quote(hello.Node.ID), owner, logsafe.Quote(err.Error()))
		l.write(Message{Type: TypeError, Error: err.Error()})
		ctl.CloseWrite()
		// Let the refusal reach the node before the connection goes.
		select {
		case <-m.Done():
		case <-time.After(time.Second):
		}
	}
	node, err := registry.Validate(*hello.Node)
	if err == nil && id.Node != "" && node.ID != id.Node {
		err = fmt.Errorf("rendezvous: this credential is for node %q, not %q", id.Node, node.ID)
	}
	if err == nil && s.Admit != nil {
		err = s.Admit(owner, node)
	}
	if errors.Is(err, ErrUnavailable) {
		s.logf("rendezvous: could not admit node %q of %s: %s", hello.Node.ID, owner, logsafe.Quote(err.Error()))
		return
	}
	if err != nil {
		refuse(err)
		return
	}
	lease, err := s.Registry.Register(node, owner)
	if err != nil && !errors.Is(err, registry.ErrOwner) && !errors.Is(err, registry.ErrInvalid) {
		// The shared registry could not be written: not a refusal, so
		// the node retries.
		s.logf("rendezvous: could not register node %q of %s: %s", hello.Node.ID, owner, logsafe.Quote(err.Error()))
		return
	}
	if err != nil {
		refuse(err)
		return
	}
	defer lease.Close()

	nc := &nodeConn{m: m, ctl: l, lease: lease}
	s.mu.Lock()
	if s.closing != "" {
		// Shutdown has already said goodbye to the connections it saw.
		why := s.closing
		s.mu.Unlock()
		l.write(Message{Type: TypeGoodbye, Error: why})
		return
	}
	if s.conns == nil {
		s.conns = map[string]*nodeConn{}
	}
	old := s.conns[node.ID]
	s.conns[node.ID] = nc
	s.mu.Unlock()
	if old != nil {
		old.m.Close()
	}
	defer func() {
		s.mu.Lock()
		if s.conns[node.ID] == nc {
			delete(s.conns, node.ID)
		}
		s.mu.Unlock()
	}()

	hb := s.Heartbeat
	if hb <= 0 {
		hb = DefaultHeartbeat
	}
	if err := l.write(Message{Type: TypeWelcome, Owner: owner, HeartbeatSeconds: int((hb + time.Second - 1) / time.Second)}); err != nil {
		return
	}
	s.logf("rendezvous: node %q of %s online: %d slots for %v", node.ID, owner, node.Slots, node.Repos)
	s.observe(EventConnected)
	defer s.observe(EventDisconnected)

	var filing atomic.Int32
	go s.acceptNodeStreams(node.ID, m)
	go func() {
		select {
		case <-lease.Done(): // superseded by a newer connection
		case <-ctx.Done():
		case <-m.Done():
		}
		m.Close()
	}()
	for {
		msg, err := l.read()
		if err != nil {
			s.logf("rendezvous: node %q of %s offline: %v", node.ID, owner, err)
			return
		}
		switch msg.Type {
		case TypeHeartbeat:
			free := node.Slots
			if msg.Free != nil {
				free = *msg.Free
			}
			if !lease.Heartbeat(free) {
				s.logf("rendezvous: node %q of %s no longer holds its registration", node.ID, owner)
				return
			}
		case TypeFileTask:
			s.fileTask(ctx, node.ID, owner, l, msg, &filing)
		default:
			s.logf("rendezvous: node %q sent an unknown %s message", node.ID, logsafe.Quote(msg.Type))
		}
	}
}

// Bounds on a node's filings (TypeFileTask). How often a node may file
// is the FileTask hook's to limit; these keep one node from holding
// more than a few of the hub's goroutines, and database connections,
// at once.
const (
	maxFilingsInFlight = 4
	maxRequestID       = 128
	fileTimeout        = 30 * time.Second
)

// fileTask files the task a node's file_task asks for, in the
// background, and answers it on the control stream with filed.
func (s *Server) fileTask(ctx context.Context, nodeID, owner string, l *lines, msg Message, inFlight *atomic.Int32) {
	reply := func(task json.RawMessage, err error) {
		m := Message{Type: TypeFiled, RequestID: msg.RequestID, Task: task}
		if err != nil {
			m.Task, m.Error = nil, err.Error()
		}
		l.write(m)
	}
	switch {
	case msg.RequestID == "" || len(msg.RequestID) > maxRequestID:
		reply(nil, fmt.Errorf("rendezvous: a file_task needs a request_id of 1-%d bytes", maxRequestID))
		return
	case s.FileTask == nil:
		reply(nil, errors.New("rendezvous: this hub files no tasks from nodes"))
		return
	}
	if inFlight.Add(1) > maxFilingsInFlight {
		inFlight.Add(-1)
		reply(nil, fmt.Errorf("rendezvous: over %d filings in flight; wait for their answers", maxFilingsInFlight))
		return
	}
	go func() {
		defer inFlight.Add(-1)
		fctx, cancel := context.WithTimeout(ctx, fileTimeout)
		defer cancel()
		task, err := s.FileTask(fctx, nodeID, owner, msg.Task)
		if err != nil {
			s.logf("rendezvous: node %q of %s: filing refused: %s", nodeID, owner, logsafe.Quote(err.Error()))
		}
		reply(task, err)
	}()
}

func (s *Server) acceptNodeStreams(nodeID string, m *mux.Mux) {
	for {
		st, err := m.AcceptStream()
		if err != nil {
			return
		}
		if s.NodeStream == nil {
			st.Reset()
			continue
		}
		go s.NodeStream(nodeID, st)
	}
}

// acceptTimeout accepts m's first stream, or gives up after d.
func acceptTimeout(m *mux.Mux, d time.Duration) (*mux.Stream, error) {
	type res struct {
		st  *mux.Stream
		err error
	}
	ch := make(chan res, 1)
	go func() {
		st, err := m.AcceptStream()
		ch <- res{st, err}
	}()
	select {
	case r := <-ch:
		return r.st, r.err
	case <-time.After(d):
		m.Close()
		return nil, errors.New("timed out")
	}
}

// Open opens a stream to a connected node over its reverse tunnel: the
// path the dispatcher hands a node its task on, and the view-through
// path to a node's diffs and logs (docs/hub.md §6, §7). A node another
// pod holds is reached through that pod, with InternalKey set
// (docs/hub.md §2.2.3); the stream is then the upgraded connection to
// it, and a node no pod holds is ErrOffline.
func (s *Server) Open(ctx context.Context, nodeID string) (net.Conn, error) {
	s.mu.Lock()
	nc := s.conns[nodeID]
	s.mu.Unlock()
	if nc != nil {
		return nc.m.OpenStream(ctx)
	}
	if len(s.InternalKey) == 0 || s.Registry.Store == nil {
		return nil, fmt.Errorf("%w: %s", ErrOffline, nodeID)
	}
	return s.openRemote(ctx, nodeID)
}

// Disconnect ends nodeID's connection, if it has one, and says whether
// it had: how a revoked credential stops serving at once rather than at
// its next reconnect, which the revocation then refuses.
func (s *Server) Disconnect(nodeID string) bool {
	s.mu.Lock()
	nc := s.conns[nodeID]
	s.mu.Unlock()
	if nc == nil {
		return false
	}
	nc.m.Close()
	return true
}

// Shutdown ends every node's connection, as a hub that is going away
// does: new connections are answered 503, and each welcomed node is
// sent a goodbye saying reason, so that it reconnects (to this hub's
// replacement) rather than wondering. It waits for the nodes to close
// their connections, until ctx ends, and then closes what is left.
// The registry marks each node offline as its connection ends.
func (s *Server) Shutdown(ctx context.Context, reason string) error {
	if reason == "" {
		reason = ErrShuttingDown.Error()
	}
	s.mu.Lock()
	s.closing = reason
	conns := make([]*nodeConn, 0, len(s.conns))
	for _, nc := range s.conns {
		conns = append(conns, nc)
	}
	s.mu.Unlock()

	// All at once, each with a deadline, so a node that has stopped
	// reading delays nobody's goodbye but its own (SA1-L7).
	var wg sync.WaitGroup
	for _, nc := range conns {
		wg.Go(func() {
			nc.ctl.c.SetWriteDeadline(time.Now().Add(time.Second))
			if err := nc.ctl.write(Message{Type: TypeGoodbye, Error: reason}); err != nil {
				nc.m.Close()
			}
		})
	}
	wg.Wait()
	var err error
	for _, nc := range conns {
		select {
		case <-nc.m.Done():
		case <-ctx.Done():
			err = ctx.Err()
		}
		nc.m.Close()
	}
	if err == nil {
		s.logf("rendezvous: said goodbye to %d nodes: %s", len(conns), reason)
	}
	return err
}
