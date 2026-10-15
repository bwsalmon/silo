// Package tunnel carries the gateway into a Kubernetes pod (design §5.4).
// Kubernetes has no reverse port-forward, so the k8s location runs a
// sidecar in the agent's pod that listens on the pod's loopback twice:
// once where the CLI's MCP config points (the gateway address), and once
// for silo, which reaches it with kubectl port-forward (the control
// address). Silo's one connection carries a mux (package mux, grain's
// pkg/tunnel model); each connection the CLI makes to the gateway
// address becomes a stream on it, which silo splices to the real
// gateway on the invoking machine.
package tunnel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/old/internal/tunnel/mux"
)

// Hello is the first line silo writes on the control connection, so
// that a stray connection (a probe, a curious agent) is not taken for
// silo's.
const Hello = "silo-tunnel 1\n"

// Ready is what the sidecar answers to Hello before the mux starts.
const Ready = "ok\n"

// Sidecar serves the gateway address on the pod's loopback and carries
// each connection to it over silo's control connection.
type Sidecar struct {
	// Wait is how long a gateway connection waits for silo's control
	// connection before it is dropped; 30s when zero.
	Wait time.Duration
	// Logf, when set, receives one line per event worth logging.
	Logf func(format string, args ...any)

	mu      sync.Mutex
	cur     *mux.Mux
	changed chan struct{}
}

func (s *Sidecar) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// Serve accepts silo's control connections on control and the CLI's on
// gateway until ctx ends. One control connection is current at a time:
// a newer one (silo reconnecting after its port-forward dropped)
// replaces it.
func (s *Sidecar) Serve(ctx context.Context, gateway, control net.Listener) error {
	s.mu.Lock()
	if s.changed == nil {
		s.changed = make(chan struct{})
	}
	s.mu.Unlock()
	go func() {
		<-ctx.Done()
		gateway.Close()
		control.Close()
	}()
	errc := make(chan error, 2)
	go func() { errc <- s.acceptControl(control) }()
	go func() { errc <- s.acceptGateway(ctx, gateway) }()
	err := <-errc
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (s *Sidecar) acceptControl(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go s.handshake(c)
	}
}

func (s *Sidecar) handshake(c net.Conn) {
	c.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil || line != Hello {
		s.logf("control: refused a connection from %s that did not say hello", c.RemoteAddr())
		c.Close()
		return
	}
	if _, err := io.WriteString(c, Ready); err != nil {
		c.Close()
		return
	}
	c.SetDeadline(time.Time{})
	m := mux.NewMux(&bufferedConn{Conn: c, r: br}, mux.Config{IsServer: true})
	s.mu.Lock()
	old := s.cur
	s.cur = m
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
	if old != nil {
		old.Close()
	}
	s.logf("control: silo connected")
	<-m.Done()
	s.mu.Lock()
	if s.cur == m {
		s.cur = nil
	}
	s.mu.Unlock()
	s.logf("control: silo disconnected")
}

// current returns the current mux, waiting up to Wait for one.
func (s *Sidecar) current(ctx context.Context) (*mux.Mux, error) {
	wait := s.Wait
	if wait == 0 {
		wait = 30 * time.Second
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		s.mu.Lock()
		m, ch := s.cur, s.changed
		s.mu.Unlock()
		if m != nil {
			select {
			case <-m.Done():
			default:
				return m, nil
			}
		}
		select {
		case <-ch:
		case <-time.After(100 * time.Millisecond):
		case <-t.C:
			return nil, errors.New("silo is not connected")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (s *Sidecar) acceptGateway(ctx context.Context, l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go func() {
			m, err := s.current(ctx)
			if err != nil {
				s.logf("gateway: dropped a connection: %v", err)
				c.Close()
				return
			}
			st, err := m.OpenStream(ctx)
			if err != nil {
				s.logf("gateway: dropped a connection: %v", err)
				c.Close()
				return
			}
			splice(c, st)
		}()
	}
}

// Client is silo's end: Run serves streams from the sidecar by dialing
// the gateway.
type Client struct {
	m *mux.Mux
}

// Connect says hello on c, an open connection to the sidecar's control
// address, and returns the client once the sidecar has answered.
func Connect(ctx context.Context, c net.Conn) (*Client, error) {
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	} else {
		c.SetDeadline(time.Now().Add(30 * time.Second))
	}
	if _, err := io.WriteString(c, Hello); err != nil {
		return nil, err
	}
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("the tunnel sidecar did not answer: %w", err)
	}
	if line != Ready {
		return nil, fmt.Errorf("the tunnel sidecar answered %q", strings.TrimSpace(line))
	}
	c.SetDeadline(time.Time{})
	return &Client{m: mux.NewMux(&bufferedConn{Conn: c, r: br}, mux.Config{})}, nil
}

// Done is closed when the control connection ends.
func (c *Client) Done() <-chan struct{} { return c.m.Done() }

// Close ends the control connection.
func (c *Client) Close() error { return c.m.Close() }

// Serve splices every stream the sidecar opens to a new connection to
// target, until the control connection ends.
func (c *Client) Serve(target string) {
	for {
		st, err := c.m.AcceptStream()
		if err != nil {
			return
		}
		go func() {
			g, err := net.DialTimeout("tcp", target, 10*time.Second)
			if err != nil {
				st.Reset()
				return
			}
			splice(g, st)
		}()
	}
}

// splice copies both ways, passing each half-close on, and closes both
// once both directions are done.
func splice(c net.Conn, st *mux.Stream) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(st, c)
		st.CloseWrite()
	}()
	go func() {
		defer wg.Done()
		io.Copy(c, st)
		if cw, ok := c.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			c.Close()
		}
	}()
	wg.Wait()
	c.Close()
	st.Close()
}

// bufferedConn reads through the reader that read the handshake, which
// may hold the first bytes of the mux.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }
