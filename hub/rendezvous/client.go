package rendezvous

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
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
	// Logf, when set, receives one line each time Run's connection
	// ends or fails, saying why, before it reconnects.
	Logf func(format string, args ...any)
}

func (cl *Client) logf(format string, args ...any) {
	if cl.Logf != nil {
		cl.Logf(format, args...)
	}
}

// Conn is one live connection to the hub.
type Conn struct {
	m     *mux.Mux
	ctl   *lines
	owner string
	once  sync.Once

	mu      sync.Mutex
	goodbye string
	filings map[string]chan Message // file_tasks awaiting their filed, by request ID
}

// ErrFileRefused is the hub refusing to file a task (Conn.FileTask);
// the message says why.
var ErrFileRefused = errors.New("rendezvous: hub refused the filing")

// FileTask asks the hub to file a task, on the control stream
// (TypeFileTask), and waits for its answer: the task the hub filed, as
// JSON (tasks.Task), or its refusal, wrapping ErrFileRefused. filing is
// marshalled as the file_task's task: a tasks.Filing.
func (c *Conn) FileTask(ctx context.Context, filing any) (json.RawMessage, error) {
	raw, err := json.Marshal(filing)
	if err != nil {
		return nil, err
	}
	b := make([]byte, 8)
	rand.Read(b)
	id := hex.EncodeToString(b)
	ch := make(chan Message, 1)
	c.mu.Lock()
	if c.filings == nil {
		c.filings = map[string]chan Message{}
	}
	c.filings[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.filings, id)
		c.mu.Unlock()
	}()
	if err := c.ctl.write(Message{Type: TypeFileTask, RequestID: id, Task: raw}); err != nil {
		return nil, err
	}
	select {
	case m := <-ch:
		if m.Error != "" || len(m.Task) == 0 {
			return nil, fmt.Errorf("%w: %s", ErrFileRefused, m.Error)
		}
		return m.Task, nil
	case <-c.Done():
		return nil, errors.New("rendezvous: the connection ended before the hub answered")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// filed hands the hub's answer to the FileTask waiting for it.
func (c *Conn) filed(m Message) {
	c.mu.Lock()
	ch := c.filings[m.RequestID]
	c.mu.Unlock()
	if ch != nil {
		select {
		case ch <- m:
		default:
		}
	}
}

// Goodbye is the reason the hub gave for ending the connection, when
// it said goodbye (it was shutting down); empty otherwise.
func (c *Conn) Goodbye() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.goodbye
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
		// The hub sends nothing more on the control stream but the
		// answers to the node's filings and a goodbye; a read ending is
		// the stream or the connection ending.
		for {
			msg, err := l.read()
			if err != nil {
				c.Close()
				return
			}
			if msg.Type == TypeFiled {
				c.filed(msg)
				continue
			}
			if msg.Type == TypeGoodbye {
				c.mu.Lock()
				c.goodbye = msg.Error
				c.mu.Unlock()
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
// minute) when the connection drops, or when the hub says goodbye
// because it is shutting down (a rolling update replaces it). A refusal
// is returned rather than retried, since retrying would be refused the
// same way.
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
			if why := c.Goodbye(); why != "" {
				cl.logf("rendezvous: the hub closed the connection: %s; reconnecting", why)
			} else if ctx.Err() == nil {
				cl.logf("rendezvous: the connection to the hub dropped; reconnecting")
			}
		} else {
			cl.logf("rendezvous: connecting to the hub: %v; retrying in %s", err, backoff)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}
