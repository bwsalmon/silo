package rendezvous

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/internal/tunnel/mux"
)

// ErrRefused is the hub refusing a node's hello; the message says why.
var ErrRefused = errors.New("rendezvous: hub refused the node")

// Client is a Grain node's end of the rendezvous.
type Client struct {
	// Hub is the hub's base URL, https://... (http:// for tests).
	Hub string
	// Token returns the node's bearer credential for each connection:
	// whatever the hub's Server.Authenticate accepts.
	Token func(ctx context.Context) (string, error)
	// Node is what the node registers as.
	Node registry.Node
	// Free, when set, is asked for the node's free slots at each
	// heartbeat; Node.Slots when nil.
	Free func() int
	// TLS configures the connection to an https hub; nil uses the
	// system roots.
	TLS *tls.Config
}

// Conn is one live connection to the hub.
type Conn struct {
	m     *mux.Mux
	ctl   *lines
	owner string
	once  sync.Once
}

// Owner is the identity the hub registered the node under.
func (c *Conn) Owner() string { return c.owner }

// Accept waits for the next stream the hub opens: a task, or a request
// to view a node-local diff or log through.
func (c *Conn) Accept() (net.Conn, error) { return c.m.AcceptStream() }

// Open opens a stream to the hub, for events.
func (c *Conn) Open(ctx context.Context) (net.Conn, error) { return c.m.OpenStream(ctx) }

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.m.Done() }

// Close ends the connection; the hub marks the node offline.
func (c *Conn) Close() error { return c.m.Close() }

// Connect dials the hub, registers the node and starts heartbeating. It
// returns once the hub has welcomed the node, or with the hub's refusal
// (ErrRefused) or the dial's error.
func (cl *Client) Connect(ctx context.Context) (*Conn, error) {
	cfg, err := cl.config(ctx)
	if err != nil {
		return nil, err
	}
	ws, err := cfg.DialContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("rendezvous: dial %s: %w", cfg.Location, err)
	}
	ws.PayloadType = websocket.BinaryFrame
	m := mux.NewMux(ws, mux.Config{})
	fail := func(err error) (*Conn, error) {
		m.Close()
		return nil, err
	}
	st, err := m.OpenStream(ctx)
	if err != nil {
		return fail(err)
	}
	l := newLines(st)
	node := cl.Node
	if err := l.write(Message{Type: TypeHello, Node: &node}); err != nil {
		return fail(fmt.Errorf("rendezvous: hello: %w", err))
	}
	if dl, ok := ctx.Deadline(); ok {
		st.SetReadDeadline(dl)
	} else {
		st.SetReadDeadline(time.Now().Add(helloTimeout))
	}
	w, err := l.read()
	st.SetReadDeadline(time.Time{})
	switch {
	case err != nil:
		return fail(fmt.Errorf("rendezvous: waiting for welcome: %w", err))
	case w.Type == TypeError:
		return fail(fmt.Errorf("%w: %s", ErrRefused, w.Error))
	case w.Type != TypeWelcome:
		return fail(fmt.Errorf("rendezvous: expected welcome, got %q", w.Type))
	}
	c := &Conn{m: m, ctl: l, owner: w.Owner}
	hb := time.Duration(w.HeartbeatSeconds) * time.Second
	if hb <= 0 {
		hb = DefaultHeartbeat
	}
	go cl.heartbeat(c, hb)
	go func() {
		// The hub sends nothing more on the control stream; a read
		// ending is the stream or the connection ending.
		for {
			if _, err := l.read(); err != nil {
				c.Close()
				return
			}
		}
	}()
	return c, nil
}

func (cl *Client) heartbeat(c *Conn, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		free := cl.Node.Slots
		if cl.Free != nil {
			free = cl.Free()
		}
		if err := c.ctl.write(Message{Type: TypeHeartbeat, Free: &free}); err != nil {
			c.Close()
			return
		}
		select {
		case <-t.C:
		case <-c.Done():
			return
		}
	}
}

func (cl *Client) config(ctx context.Context) (*websocket.Config, error) {
	u, err := url.Parse(strings.TrimSuffix(cl.Hub, "/") + ConnectPath)
	if err != nil {
		return nil, fmt.Errorf("rendezvous: hub URL: %w", err)
	}
	origin := *u
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return nil, fmt.Errorf("rendezvous: hub URL %q: want https:// or http://", cl.Hub)
	}
	origin.Path = ""
	cfg, err := websocket.NewConfig(u.String(), origin.String())
	if err != nil {
		return nil, err
	}
	cfg.Protocol = []string{Protocol}
	cfg.TlsConfig = cl.TLS
	if cl.Token != nil {
		tok, err := cl.Token(ctx)
		if err != nil {
			return nil, fmt.Errorf("rendezvous: node credential: %w", err)
		}
		cfg.Header = http.Header{"Authorization": {"Bearer " + tok}}
	}
	return cfg, nil
}

// Run keeps the node connected until ctx ends: it connects, hands each
// stream the hub opens to handle, and reconnects with backoff (up to a
// minute) when the connection drops. A refusal is returned rather than
// retried, since retrying would be refused the same way.
func (cl *Client) Run(ctx context.Context, handle func(net.Conn)) error {
	backoff := time.Second
	for {
		c, err := cl.Connect(ctx)
		if errors.Is(err, ErrRefused) {
			return err
		}
		if err == nil {
			backoff = time.Second
			go func() {
				select {
				case <-ctx.Done():
					c.Close()
				case <-c.Done():
				}
			}()
			for {
				st, err := c.Accept()
				if err != nil {
					break
				}
				go handle(st)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}
