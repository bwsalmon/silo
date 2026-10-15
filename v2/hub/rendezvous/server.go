package rendezvous

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"github.com/bwsalmon/silo/v2/hub/registry"
	"github.com/bwsalmon/silo/v2/internal/tunnel/mux"
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
	// (its bearer token), and returns the owner to register it under.
	// A node is never registered under an identity it merely claims.
	Authenticate func(r *http.Request) (owner string, err error)
	// Admit, when set, decides whether owner may register n: the team
	// directory's check (docs/hub.md §5) that the owner may serve the
	// repositories n lists. Nil admits every authenticated node.
	Admit func(owner string, n registry.Node) error
	// NodeStream, when set, is handed each stream a node opens after its
	// control stream, for the event streams of docs/hub.md §7. Nil
	// resets them.
	NodeStream func(nodeID string, c net.Conn)
	// Heartbeat is the interval the hub asks nodes to heartbeat at;
	// DefaultHeartbeat when zero. Set Registry.StaleAfter to a few of
	// them.
	Heartbeat time.Duration
	// Logf, when set, receives one line per connection event.
	Logf func(format string, args ...any)

	mu    sync.Mutex
	conns map[string]*nodeConn
}

type nodeConn struct {
	m     *mux.Mux
	lease *registry.Lease
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// Handler serves ConnectPath and DirectoryPath. The directory is
// served unauthenticated: the caller fronts it with the hub's human
// sign-on (IAP), or serves only ConnectPath.
func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.Handle("GET "+ConnectPath, http.HandlerFunc(s.serveConnect))
	m.Handle("GET "+DirectoryPath, http.HandlerFunc(s.serveDirectory))
	return m
}

func (s *Server) serveDirectory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Nodes []registry.Entry `json:"nodes"`
	}{s.Registry.List()})
}

func (s *Server) serveConnect(w http.ResponseWriter, r *http.Request) {
	if s.Authenticate == nil {
		http.Error(w, "node authentication is not configured", http.StatusServiceUnavailable)
		return
	}
	owner, err := s.Authenticate(r)
	if err != nil || owner == "" {
		s.logf("rendezvous: refused a node from %s: %v", r.RemoteAddr, err)
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
			s.serveNode(r.Context(), owner, c)
		},
	}
	ws.ServeHTTP(w, r)
}

// serveNode runs one node's connection until it ends.
func (s *Server) serveNode(ctx context.Context, owner string, c *websocket.Conn) {
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
		s.logf("rendezvous: no hello from %s: %v", owner, err)
		l.write(Message{Type: TypeError, Error: "expected hello"})
		return
	}
	refuse := func(err error) {
		s.logf("rendezvous: refused node %q of %s: %v", hello.Node.ID, owner, err)
		l.write(Message{Type: TypeError, Error: err.Error()})
		ctl.CloseWrite()
		// Let the refusal reach the node before the connection goes.
		select {
		case <-m.Done():
		case <-time.After(time.Second):
		}
	}
	node, err := registry.Validate(*hello.Node)
	if err == nil && s.Admit != nil {
		err = s.Admit(owner, node)
	}
	if err != nil {
		refuse(err)
		return
	}
	lease, err := s.Registry.Register(node, owner)
	if err != nil {
		refuse(err)
		return
	}
	defer lease.Close()

	nc := &nodeConn{m: m, lease: lease}
	s.mu.Lock()
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
				return
			}
		default:
			s.logf("rendezvous: node %q sent an unknown %q message", node.ID, msg.Type)
		}
	}
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
// path to a node's diffs and logs (docs/hub.md §6, §7).
func (s *Server) Open(ctx context.Context, nodeID string) (net.Conn, error) {
	s.mu.Lock()
	nc := s.conns[nodeID]
	s.mu.Unlock()
	if nc == nil {
		return nil, fmt.Errorf("%w: %s", ErrOffline, nodeID)
	}
	return nc.m.OpenStream(ctx)
}
