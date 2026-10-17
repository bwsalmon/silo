package rendezvous

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/hub/route"
	"github.com/bwsalmon/silo/internal/logsafe"
	"github.com/bwsalmon/silo/internal/tunnel/mux"
)

// InternalOpenPath is the internal listener's one route (docs/hub.md
// §2.2.3): another pod asking this one, which holds node {id}'s
// connection, for a stream to it. It is an HTTP upgrade to
// OpenUpgrade; the upgraded connection is the stream.
const InternalOpenPath = "/internal/v1/nodes/{id}/open"

// OpenUpgrade is InternalOpenPath's Upgrade protocol.
const OpenUpgrade = "silo-open.v1"

// MinInternalKey is the shortest internal key the hub accepts.
const MinInternalKey = 32

// forwardTimeout bounds dialling the holding pod and its answer.
const forwardTimeout = 10 * time.Second

// errNotHeld is a holding pod answering that it no longer holds the
// node: it lost the connection since the shared registry was read.
var errNotHeld = errors.New("rendezvous: the pod no longer holds the node")

// InternalToken is the bearer token that admits a request to pod's
// internal listener during t's minute: an HMAC, under the key the
// hub's pods share, over the pod's name and the minute, so that it can
// be replayed neither to another pod nor later.
func InternalToken(key []byte, pod string, t time.Time) string {
	return internalToken(key, pod, t.Unix()/60)
}

func internalToken(key []byte, pod string, minute int64) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(OpenUpgrade + "\x00" + pod + "\x00" + strconv.FormatInt(minute, 10)))
	return hex.EncodeToString(m.Sum(nil))
}

// internalAuthorized checks r's bearer token against this pod's name
// and the minutes either side of now, for the pods' clocks and a
// request that straddles a minute.
func (s *Server) internalAuthorized(r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || len(s.InternalKey) == 0 {
		return false
	}
	now := time.Now().Unix() / 60
	for _, m := range []int64{now, now - 1, now + 1} {
		if hmac.Equal([]byte(tok), []byte(internalToken(s.InternalKey, s.Registry.Pod, m))) {
			return true
		}
	}
	return false
}

// InternalHandler serves InternalOpenPath, for the internal listener
// (docs/hub.md §2.2.3), which no Service names: only the hub's other
// pods reach it, with a token from InternalKey for this pod. The pod
// checks it still holds the node's connection, opens a stream to the
// node as Open does, answers 101 and splices the two until either
// ends. A node it does not hold is answered 404. The caller has
// checked its own caller's right to the node; this pod trusts that
// check.
func (s *Server) InternalHandler() http.Handler {
	m := route.NewMux()
	m.HandleFunc("GET "+InternalOpenPath, s.serveInternalOpen)
	return m
}

func (s *Server) serveInternalOpen(w http.ResponseWriter, r *http.Request) {
	if !s.internalAuthorized(r) {
		s.logf("rendezvous: refused an internal open from %s: bad token", r.RemoteAddr)
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), OpenUpgrade) {
		http.Error(w, "want Upgrade: "+OpenUpgrade, http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	s.mu.Lock()
	nc := s.conns[id]
	s.mu.Unlock()
	if nc == nil {
		http.Error(w, "this pod does not hold node "+id, http.StatusNotFound)
		return
	}
	st, err := nc.m.OpenStream(r.Context())
	if err != nil {
		// The connection is ending: as good as not held.
		http.Error(w, "this pod does not hold node "+id, http.StatusNotFound)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		st.Reset()
		http.Error(w, "cannot upgrade", http.StatusInternalServerError)
		return
	}
	c, brw, err := hj.Hijack()
	if err != nil {
		st.Reset()
		s.logf("rendezvous: forwarding a stream to node %s: %s", logsafe.Quote(id), logsafe.Quote(err.Error()))
		return
	}
	c.SetDeadline(time.Time{})
	brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " + OpenUpgrade + "\r\nConnection: Upgrade\r\n\r\n")
	if err := brw.Flush(); err != nil {
		c.Close()
		st.Reset()
		return
	}
	splice(&bufferedConn{Conn: c, r: brw.Reader}, st)
}

// splice copies between the other pod's connection and the node's
// stream until both directions have ended. A direction that ends
// cleanly half-closes the other side, as a local stream's CloseWrite
// would; one that fails ends both.
func splice(c *bufferedConn, st *mux.Stream) {
	var once sync.Once
	abort := func() {
		once.Do(func() {
			c.Close()
			st.Close()
		})
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := io.Copy(st, c); err != nil {
			abort()
			return
		}
		st.CloseWrite()
	}()
	if _, err := io.Copy(c, st); err != nil {
		abort()
	} else {
		c.CloseWrite()
	}
	<-done
	abort()
}

// bufferedConn is a connection whose first bytes may already sit in r,
// read past an HTTP upgrade's header.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// CloseWrite half-closes the connection, as a mux stream's does.
func (c *bufferedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// openRemote opens a stream to nodeID through the pod holding it, as
// the shared registry names it: a holder that has lost the node since
// the read is asked once more after a fresh read, and then the node is
// offline.
func (s *Server) openRemote(ctx context.Context, nodeID string) (net.Conn, error) {
	for attempt := 0; ; attempt++ {
		pod, addr, ok, err := s.Registry.Holder(ctx, nodeID)
		if err != nil {
			return nil, fmt.Errorf("rendezvous: finding the pod holding node %s: %w", nodeID, err)
		}
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrOffline, nodeID)
		}
		c, err := s.forward(ctx, pod, addr, nodeID)
		if errors.Is(err, errNotHeld) {
			if attempt == 0 {
				continue
			}
			return nil, fmt.Errorf("%w: %s (pod %s no longer holds it)", ErrOffline, nodeID, pod)
		}
		return c, err
	}
}

// forward dials pod at addr and upgrades to a stream to nodeID.
func (s *Server) forward(ctx context.Context, pod, addr, nodeID string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, forwardTimeout)
	defer cancel()
	dial := s.DialInternal
	if dial == nil {
		var d net.Dialer
		dial = d.DialContext
	}
	c, err := dial(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("rendezvous: dialling pod %s (%s) for node %s: %w", pod, addr, nodeID, err)
	}
	fail := func(err error) (net.Conn, error) {
		c.Close()
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, fmt.Errorf("rendezvous: pod %s (%s) for node %s: %w", pod, addr, nodeID, err)
	}
	stop := context.AfterFunc(ctx, func() { c.SetDeadline(time.Unix(1, 0)) })
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+strings.Replace(InternalOpenPath, "{id}", nodeID, 1), nil)
	if err != nil {
		stop()
		return fail(err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", OpenUpgrade)
	req.Header.Set("Authorization", "Bearer "+InternalToken(s.InternalKey, pod, time.Now()))
	br := bufio.NewReader(c)
	err = req.Write(c)
	var resp *http.Response
	if err == nil {
		resp, err = http.ReadResponse(br, req)
	}
	if !stop() {
		return fail(ctx.Err())
	}
	if err != nil {
		return fail(err)
	}
	switch {
	case resp.StatusCode == http.StatusSwitchingProtocols && strings.EqualFold(resp.Header.Get("Upgrade"), OpenUpgrade):
		c.SetDeadline(time.Time{})
		return &bufferedConn{Conn: c, r: br}, nil
	case resp.StatusCode == http.StatusNotFound:
		c.Close()
		return nil, errNotHeld
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fail(fmt.Errorf("answered %s: %s", resp.Status, strings.TrimSpace(string(msg))))
}

// Internal wraps a handler for another of the internal listener's
// routes (docs/hub.md §2.2.4) in the same check InternalOpenPath makes:
// a token from InternalKey for this pod.
func (s *Server) Internal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.internalAuthorized(r) {
			s.logf("rendezvous: refused an internal request from %s: bad token", r.RemoteAddr)
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Forwards is whether Get can reach other pods: there is an internal
// key and a shared registry to find them in.
func (s *Server) Forwards() bool { return len(s.InternalKey) > 0 && s.Registry.Store != nil }

// Get asks the pod holding nodeID, as the shared registry names it, for
// path on its internal listener, and returns its 200 answer, whose body
// the caller reads and closes; ctx bounds the whole of it. A pod that
// answers 404 (it no longer holds the node) is asked once more after a
// fresh read, and then, as when no other pod holds the node, Get fails
// with ErrOffline.
func (s *Server) Get(ctx context.Context, nodeID, path string) (*http.Response, error) {
	if !s.Forwards() {
		return nil, fmt.Errorf("%w: %s", ErrOffline, nodeID)
	}
	for attempt := 0; ; attempt++ {
		pod, addr, ok, err := s.Registry.Holder(ctx, nodeID)
		if err != nil {
			return nil, fmt.Errorf("rendezvous: finding the pod holding node %s: %w", nodeID, err)
		}
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrOffline, nodeID)
		}
		resp, err := s.get(ctx, pod, addr, path)
		if errors.Is(err, errNotHeld) {
			if attempt == 0 {
				continue
			}
			return nil, fmt.Errorf("%w: %s (pod %s no longer holds it)", ErrOffline, nodeID, pod)
		}
		return resp, err
	}
}

func (s *Server) get(ctx context.Context, pod, addr, path string) (*http.Response, error) {
	s.clientOnce.Do(func() {
		dial := s.DialInternal
		if dial == nil {
			d := net.Dialer{Timeout: forwardTimeout}
			dial = d.DialContext
		}
		s.client = &http.Client{Transport: &http.Transport{
			DialContext:           dial,
			ResponseHeaderTimeout: forwardTimeout,
			MaxIdleConnsPerHost:   4,
		}}
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+InternalToken(s.InternalKey, pod, time.Now()))
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rendezvous: pod %s (%s): %w", pod, addr, err)
	}
	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotHeld
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return nil, fmt.Errorf("rendezvous: pod %s (%s) answered %s: %s", pod, addr, resp.Status, strings.TrimSpace(string(msg)))
}
