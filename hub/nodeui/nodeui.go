// Package nodeui carries a Grain node's own UI through the hub
// (docs/hub.md §11): a viewer opens https://<node>.<domain>/..., the hub
// signs them in, checks that the node is theirs, opens a stream to the
// node over its rendezvous connection (rendezvous.Server.Open) and
// proxies the request through it, WebSocket upgrades included. It is
// what grain's Cloud Run relay (grain-relay) does for a node that has
// no hub, over the connection, the credential and the sign-on the node
// already has with one.
//
// The stream starts as the artifact view-through's does (hub/live): one
// JSON line each way. After ok, it carries exactly one HTTP/1.1
// request and its response, as on a TCP connection, and then closes:
//
//	hub → node  {"type":"http","node":"<id>","viewer":"<email>"}
//	node → hub  {"type":"ok"}, then the node reads the request and writes the response
//	            or {"type":"error","error":"..."}   (answered 403)
//
// A response that switches protocols (101) leaves the stream carrying
// the upgraded connection both ways until either end closes it.
//
// The hub keeps nothing: it copies each response to its viewer as the
// node writes it.
package nodeui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/hub/directory"
	"github.com/bwsalmon/silo/hub/identity"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
)

// Lines on an http stream.
const (
	TypeHTTP  = "http"
	TypeOK    = "ok"
	TypeError = "error"
)

// Message is a line on an http stream.
type Message struct {
	Type string `json:"type"`
	// Node is the node's ID, in http.
	Node string `json:"node,omitempty"`
	// Viewer is whom the hub signed the request's sender in as, in
	// http. It is the hub's claim: a node that verifies the viewer
	// itself reads the request's X-Goog-IAP-JWT-Assertion, which the
	// hub passes on as IAP sent it.
	Viewer string `json:"viewer,omitempty"`
	// Error is the reason, in error.
	Error string `json:"error,omitempty"`
}

// openTimeout bounds opening a stream to the node and its answer.
const openTimeout = 30 * time.Second

// maxLine bounds the node's answer line.
const maxLine = 64 << 10

// label is a node ID that is also a DNS label, and so may name the
// node's UI host.
var label = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Server serves node UIs, each on its own host below Domain.
type Server struct {
	// Domain is the domain node UIs are served under: node ws-1's at
	// ws-1.<Domain>. It is its own origin, not the hub's, for each
	// node, so that one node's UI cannot script the hub's API or
	// another node's UI with its viewer's sign-on.
	Domain string
	// Registry says which nodes are online, and whose they are.
	Registry *registry.Registry
	// Directory signs viewers in.
	Directory *directory.API
	// Open opens a stream to a connected node: rendezvous.Server.Open.
	Open func(ctx context.Context, nodeID string) (net.Conn, error)
	// Logf, when set, receives one line per failed request.
	Logf func(format string, args ...any)

	once  sync.Once
	proxy *httputil.ReverseProxy
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// Node is the node a request's Host names, if it is one of Domain's
// node hosts: "ws-1.nodes.example.com" is ws-1, for Domain
// "nodes.example.com". A node ID that is not a DNS label (with
// capitals, '.' or '_' in it) has no host.
func (s *Server) Node(host string) (string, bool) {
	if s.Domain == "" {
		return "", false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	id, ok := strings.CutSuffix(host, "."+strings.ToLower(s.Domain))
	if !ok || !label.MatchString(id) {
		return "", false
	}
	return id, true
}

// Hosts wraps hub, the hub's own handler, so that requests to a node's
// UI host are served by s and every other by hub. A node's host never
// reaches the hub's API.
func (s *Server) Hosts(hub http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.Node(r.Host); ok {
			s.ServeHTTP(w, r)
			return
		}
		hub.ServeHTTP(w, r)
	})
}

type ctxKey struct{}

// target is what Rewrite hands the transport's dial: which node, and
// for whom.
type target struct {
	node, viewer string
}

// refusal is the node's answer error: it does not serve its UI here.
type refusal struct{ why string }

func (r *refusal) Error() string { return r.why }

// ServeHTTP serves one request to a node's UI host. The caller must be
// signed in, and the node must be theirs: a node they do not own, or
// that the hub does not know, is answered 404 whether or not it exists.
// An offline node is answered 503, one that does not serve its UI
// through the hub 403.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, ok := s.Node(r.Host)
	if !ok {
		http.NotFound(w, r)
		return
	}
	acc, err := s.Directory.Access(r)
	if err != nil {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	e, ok := s.Registry.Get(id)
	if !ok || !strings.EqualFold(e.Owner, acc.Email) {
		http.Error(w, "no such node", http.StatusNotFound)
		return
	}
	if e.Status == registry.Offline || s.Open == nil {
		http.Error(w, "unavailable: node "+id+" is "+string(e.Status), http.StatusServiceUnavailable)
		return
	}
	if isUpgrade(r) {
		if err := sameOrigin(r); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
	}
	s.once.Do(s.init)
	s.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, target{node: id, viewer: acc.Email})))
}

func (s *Server) init() {
	s.proxy = &httputil.ReverseProxy{
		Rewrite:        rewrite,
		Transport:      &http.Transport{DialContext: s.dial, DisableKeepAlives: true, DisableCompression: true},
		FlushInterval:  -1,
		ModifyResponse: modifyResponse,
		ErrorHandler:   s.proxyError,
	}
}

// clientHeaders are request headers the hub does not pass on: the
// hub's own credentials (Authorization is how a non-browser signs in to
// the hub, and is not the node's to see), and the plaintext identity
// headers grain's relay strips too, which nothing in front of the hub
// sets and so only a client could have written.
var clientHeaders = []string{
	"Authorization",
	"X-Forwarded-User",
	"X-User-LDAP",
	"X-Grain-User",
	"Grain-User",
}

// hubCookies are the hub's own session cookies (OIDC's), which the node
// is not given.
var hubCookies = map[string]bool{identity.SessionCookie: true, identity.FlowCookie: true}

func rewrite(pr *httputil.ProxyRequest) {
	pr.Out.URL.Scheme = "http"
	pr.Out.URL.Host = "node"
	pr.Out.Host = pr.In.Host
	pr.SetXForwarded()
	for _, h := range clientHeaders {
		pr.Out.Header.Del(h)
	}
	if cs := pr.In.Cookies(); len(cs) > 0 {
		pr.Out.Header.Del("Cookie")
		var keep []string
		for _, c := range cs {
			if !hubCookies[c.Name] {
				keep = append(keep, c.String())
			}
		}
		if len(keep) > 0 {
			pr.Out.Header.Set("Cookie", strings.Join(keep, "; "))
		}
	}
}

// modifyResponse drops a cookie the node sets for a wider domain than
// its own host: one node's UI may not set cookies on its siblings', nor
// on the hub's.
func modifyResponse(resp *http.Response) error {
	sc := resp.Header.Values("Set-Cookie")
	if len(sc) == 0 {
		return nil
	}
	resp.Header.Del("Set-Cookie")
	for _, v := range sc {
		c, err := http.ParseSetCookie(v)
		if err != nil || c.Domain != "" {
			continue
		}
		resp.Header.Add("Set-Cookie", v)
	}
	return nil
}

func (s *Server) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	t, _ := r.Context().Value(ctxKey{}).(target)
	var ref *refusal
	switch {
	case errors.As(err, &ref):
		http.Error(w, "node "+t.node+" does not serve its UI through the hub: "+ref.why, http.StatusForbidden)
	case errors.Is(err, rendezvous.ErrOffline):
		http.Error(w, "unavailable: node "+t.node+" is offline", http.StatusServiceUnavailable)
	case errors.Is(err, context.Canceled):
		// The viewer left.
	default:
		s.logf("nodeui: %s %s on node %s: %v", r.Method, r.URL.Path, t.node, err)
		http.Error(w, "reaching node "+t.node+": "+err.Error(), http.StatusBadGateway)
	}
}

// dial opens the http stream to the request's node and waits for its
// ok: the transport then writes the request on it and reads the
// response, as on a TCP connection of its own.
func (s *Server) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	t, ok := ctx.Value(ctxKey{}).(target)
	if !ok {
		return nil, errors.New("nodeui: no node for this request")
	}
	octx, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	c, err := s.Open(octx, t.node)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(Message{Type: TypeHTTP, Node: t.node, Viewer: t.viewer})
	c.SetDeadline(time.Now().Add(openTimeout))
	if _, err := c.Write(append(b, '\n')); err != nil {
		c.Close()
		return nil, fmt.Errorf("asking node %s: %w", t.node, err)
	}
	br := bufio.NewReaderSize(c, 4096)
	var ans Message
	line, err := readLine(br)
	if err == nil {
		err = json.Unmarshal(line, &ans)
	}
	switch {
	case err != nil:
		err = fmt.Errorf("node %s did not answer: %w", t.node, err)
	case ans.Type == TypeError:
		err = &refusal{ans.Error}
	case ans.Type != TypeOK:
		err = fmt.Errorf("node %s answered %q", t.node, ans.Type)
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	c.SetDeadline(time.Time{})
	return &bufConn{Conn: c, r: br}, nil
}

func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return nil, err
		}
		line = append(line, chunk...)
		if len(line) > maxLine {
			return nil, errors.New("answer too long")
		}
		if !isPrefix {
			return line, nil
		}
	}
}

// bufConn is a stream whose first bytes were read into r.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(b []byte) (int, error) { return c.r.Read(b) }

func isUpgrade(r *http.Request) bool {
	for _, v := range r.Header.Values("Connection") {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), "upgrade") {
				return true
			}
		}
	}
	return false
}

// sameOrigin refuses an upgrade a browser sent from another origin than
// the node's own host: another site, or another node's UI, opening a
// WebSocket with the viewer's sign-on. A client that sends no Origin is
// not a browser.
func sameOrigin(r *http.Request) error {
	o := r.Header.Get("Origin")
	if o == "" {
		return nil
	}
	u, err := url.Parse(o)
	if err != nil || !strings.EqualFold(u.Host, r.Host) {
		return fmt.Errorf("origin %q is not this node's", o)
	}
	return nil
}

// Serve answers one http stream on a node: what a node's grain does with
// a stream the hub opened whose first line is http (it reads that line
// itself, to tell it from a task or a view stream, and passes it in).
// It answers ok and serves the one request that follows with h, as a
// connection of its own would be served, upgrades and streamed
// responses included. The hub writes nothing after its line until it
// has the answer, so a caller's reader holds nothing of the request.
//
// A node that does not serve its UI through the hub calls Refuse
// instead.
func Serve(c net.Conn, m Message, h http.Handler) error {
	b, _ := json.Marshal(Message{Type: TypeOK})
	if _, err := c.Write(append(b, '\n')); err != nil {
		c.Close()
		return err
	}
	l := &oneConn{c: c, done: make(chan struct{})}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: openTimeout,
		ConnState: func(_ net.Conn, st http.ConnState) {
			if st == http.StateClosed || st == http.StateHijacked {
				l.Close()
			}
		},
	}
	srv.SetKeepAlivesEnabled(false)
	if err := srv.Serve(l); !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

// Refuse answers an http stream on a node that does not serve its UI
// through the hub, saying why: the viewer is answered 403.
func Refuse(c net.Conn, why string) error {
	defer c.Close()
	b, _ := json.Marshal(Message{Type: TypeError, Error: why})
	_, err := c.Write(append(b, '\n'))
	return err
}

// oneConn is a listener that accepts c once, and then nothing until it
// is closed.
type oneConn struct {
	c    net.Conn
	once sync.Once
	took bool
	mu   sync.Mutex
	done chan struct{}
}

func (l *oneConn) Accept() (net.Conn, error) {
	l.mu.Lock()
	if !l.took {
		l.took = true
		l.mu.Unlock()
		return l.c, nil
	}
	l.mu.Unlock()
	<-l.done
	return nil, net.ErrClosed
}

func (l *oneConn) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *oneConn) Addr() net.Addr { return l.c.LocalAddr() }
