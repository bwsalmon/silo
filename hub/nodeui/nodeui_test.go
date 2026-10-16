package nodeui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/bwsalmon/silo/hub/directory"
	"github.com/bwsalmon/silo/hub/identity"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
)

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// headerAuth is a test sign-on: X-User is the email.
type headerAuth struct{}

func (headerAuth) Principal(r *http.Request) (identity.Principal, error) {
	u := r.Header.Get("X-User")
	if u == "" {
		return identity.Principal{}, identity.ErrUnauthenticated
	}
	return identity.Principal{Email: u}, nil
}

const domain = "nodes.hub.test"

type harness struct {
	ui    *Server
	rvURL string
	url   string // the hub, node UI hosts and all
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir, err := directory.Open(context.Background(), "sqlite::memory:")
	check(t, err)
	t.Cleanup(func() { dir.Close() })
	rv := &rendezvous.Server{
		Registry: registry.New(),
		Authenticate: func(r *http.Request) (rendezvous.Identity, error) {
			tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if tok == "" {
				return rendezvous.Identity{}, errors.New("no token")
			}
			return rendezvous.Identity{Owner: tok}, nil
		},
		Heartbeat: time.Second,
		Logf:      t.Logf,
	}
	ui := &Server{
		Domain:    domain,
		Registry:  rv.Registry,
		Directory: &directory.API{Store: dir, Auth: headerAuth{}},
		Open:      rv.Open,
		Logf:      t.Logf,
	}
	rvs := httptest.NewServer(rv.Handler())
	t.Cleanup(rvs.Close)
	hub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "the hub's own API", http.StatusTeapot)
	})
	hs := httptest.NewServer(ui.Hosts(hub))
	t.Cleanup(hs.Close)
	return &harness{ui: ui, rvURL: rvs.URL, url: hs.URL}
}

// connect connects node id to the hub as owner, and serves each http
// stream the hub opens on it with ui, or refuses it when ui is nil. The
// first lines the node is sent arrive on asked.
func (h *harness) connect(t *testing.T, id, owner string, ui http.Handler) (*rendezvous.Conn, <-chan Message) {
	t.Helper()
	cl := &rendezvous.Client{
		Hub:   h.rvURL,
		Token: func(context.Context) (string, error) { return owner, nil },
		Node:  registry.Node{ID: id, Slots: 1},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := cl.Connect(ctx)
	check(t, err)
	t.Cleanup(func() { c.Close() })
	asked := make(chan Message, 100)
	go func() {
		for {
			s, err := c.Accept()
			if err != nil {
				return
			}
			go func() {
				line, err := readLine(bufio.NewReader(s))
				var m Message
				if err == nil {
					err = json.Unmarshal(line, &m)
				}
				if err != nil || m.Type != TypeHTTP {
					s.Close()
					return
				}
				asked <- m
				if ui == nil {
					Refuse(s, "its owner has not turned it on")
					return
				}
				Serve(s, m, ui)
			}()
		}
	}()
	return c, asked
}

// do sends r to host, as user ("" for nobody).
func (h *harness) do(t *testing.T, r *http.Request, host, user string) (*http.Response, string) {
	t.Helper()
	u, _ := url.Parse(h.url)
	r.URL.Scheme, r.URL.Host = u.Scheme, u.Host
	r.Host = host
	if user != "" {
		r.Header.Set("X-User", user)
	}
	resp, err := http.DefaultClient.Do(r)
	check(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	check(t, err)
	return resp, string(b)
}

func (h *harness) get(t *testing.T, host, path, user string) (*http.Response, string) {
	t.Helper()
	r, err := http.NewRequest("GET", "http://x"+path, nil)
	check(t, err)
	return h.do(t, r, host, user)
}

// nodeUI is a node's UI, as a test sees it.
func nodeUI(sse <-chan struct{}) http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/api/hello", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "grain_session", Value: "s1", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "toss", Value: "x", Domain: domain})
		fmt.Fprintf(w, "host=%s path=%s query=%s cookie=%q authz=%q user=%q xff=%t iap=%q",
			r.Host, r.URL.Path, r.URL.RawQuery, r.Header.Get("Cookie"), r.Header.Get("Authorization"),
			r.Header.Get("X-Forwarded-User"), r.Header.Get("X-Forwarded-For") != "", r.Header.Get(identity.IAPHeader))
	})
	m.HandleFunc("POST /api/echo", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(w, r.Body)
	})
	m.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		<-sse
		io.WriteString(w, "data: two\n\n")
	})
	m.Handle("/ws", websocket.Handler(func(c *websocket.Conn) {
		io.Copy(c, c)
	}))
	return m
}

func TestNode(t *testing.T) {
	s := &Server{Domain: "Nodes.Example.com"}
	for host, want := range map[string]string{
		"ws-1.nodes.example.com":      "ws-1",
		"WS-1.nodes.example.com:443":  "ws-1",
		"ws-1.nodes.example.com.":     "ws-1",
		"nodes.example.com":           "",
		"a.b.nodes.example.com":       "",
		"ws_1.nodes.example.com":      "",
		"ws-1.nodes.example.com.evil": "",
		"ws-1.example.com":            "",
		"-x.nodes.example.com":        "",
	} {
		got, ok := s.Node(host)
		if got != want || ok != (want != "") {
			t.Errorf("Node(%q) = %q, %t; want %q", host, got, ok, want)
		}
	}
	if _, ok := (&Server{}).Node("ws-1.nodes.example.com"); ok {
		t.Errorf("a server with no domain serves a node host")
	}
}

func TestViewThrough(t *testing.T) {
	h := newHarness(t)
	_, asked := h.connect(t, "ws-1", "alice@example.com", nodeUI(nil))
	host := "ws-1." + domain

	r, _ := http.NewRequest("GET", "http://x/api/hello?a=1", nil)
	r.Header.Set("Cookie", "grain_session=s0; "+identity.SessionCookie+"=hub-session")
	r.Header.Set("Authorization", "Bearer hub-token")
	r.Header.Set("X-Forwarded-User", "mallory@example.com")
	r.Header.Set(identity.IAPHeader, "the-assertion")
	resp, body := h.do(t, r, host, "alice@example.com")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/hello: %d %q", resp.StatusCode, body)
	}
	want := `host=ws-1.nodes.hub.test path=/api/hello query=a=1 cookie="grain_session=s0" authz="" user="" xff=true iap="the-assertion"`
	if body != want {
		t.Fatalf("the node saw\n %s\nwant\n %s", body, want)
	}
	if m := <-asked; m.Node != "ws-1" || m.Viewer != "alice@example.com" {
		t.Fatalf("the node was asked %+v", m)
	}
	if got := resp.Header.Values("Set-Cookie"); len(got) != 1 || !strings.HasPrefix(got[0], "grain_session=s1") {
		t.Fatalf("the viewer was sent cookies %q: want the node's own, not one for its domain", got)
	}

	r, _ = http.NewRequest("POST", "http://x/api/echo", strings.NewReader("a body"))
	if resp, body := h.do(t, r, host, "alice@example.com"); resp.StatusCode != http.StatusOK || body != "a body" {
		t.Fatalf("POST /api/echo: %d %q", resp.StatusCode, body)
	}

	// The hub's own host is the hub's.
	if resp, body := h.get(t, "hub.test", "/api/hello", "alice@example.com"); resp.StatusCode != http.StatusTeapot {
		t.Fatalf("the hub's host: %d %q", resp.StatusCode, body)
	}
}

func TestAccess(t *testing.T) {
	h := newHarness(t)
	h.connect(t, "ws-1", "alice@example.com", nodeUI(nil))
	h.connect(t, "ws-refuses", "alice@example.com", nil)
	gone, _ := h.connect(t, "ws-gone", "alice@example.com", nodeUI(nil))
	gone.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if e, ok := h.ui.Registry.Get("ws-gone"); ok && e.Status == registry.Offline {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ws-gone did not go offline")
		}
		time.Sleep(10 * time.Millisecond)
	}

	for _, c := range []struct {
		node, user string
		code       int
		says       string
	}{
		{"ws-1", "", http.StatusUnauthorized, "unauthenticated"},
		{"ws-1", "bob@example.com", http.StatusNotFound, "no such node"},
		{"ws-1", "ALICE@example.com", http.StatusOK, "host="},
		{"ws-none", "alice@example.com", http.StatusNotFound, "no such node"},
		{"ws-gone", "alice@example.com", http.StatusServiceUnavailable, "offline"},
		{"ws-refuses", "alice@example.com", http.StatusForbidden, "has not turned it on"},
	} {
		resp, body := h.get(t, c.node+"."+domain, "/api/hello", c.user)
		if resp.StatusCode != c.code || !strings.Contains(body, c.says) {
			t.Errorf("%s as %q: %d %q; want %d %q", c.node, c.user, resp.StatusCode, body, c.code, c.says)
		}
	}
}

func TestStreamed(t *testing.T) {
	h := newHarness(t)
	sse := make(chan struct{})
	h.connect(t, "ws-1", "alice@example.com", nodeUI(sse))
	u, _ := url.Parse(h.url)
	r, _ := http.NewRequest("GET", h.url+"/api/events", nil)
	r.URL.Host = u.Host
	r.Host = "ws-1." + domain
	r.Header.Set("X-User", "alice@example.com")
	resp, err := http.DefaultClient.Do(r)
	check(t, err)
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	// The first event arrives while the node still holds the second:
	// the hub passes each write on as it comes.
	line, err := br.ReadString('\n')
	check(t, err)
	if line != "data: one\n" {
		t.Fatalf("first line %q", line)
	}
	close(sse)
	rest, err := io.ReadAll(br)
	check(t, err)
	if string(rest) != "\ndata: two\n\n" {
		t.Fatalf("the rest: %q", rest)
	}
}

func TestWebSocket(t *testing.T) {
	h := newHarness(t)
	h.connect(t, "ws-1", "alice@example.com", nodeUI(nil))
	u, _ := url.Parse(h.url)
	dial := func(origin string) (*websocket.Conn, error) {
		cfg, err := websocket.NewConfig("ws://ws-1."+domain+"/ws", origin)
		check(t, err)
		cfg.Header.Set("X-User", "alice@example.com")
		tc, err := net.Dial("tcp", u.Host)
		check(t, err)
		ws, err := websocket.NewClient(cfg, tc)
		if err != nil {
			tc.Close()
		}
		return ws, err
	}
	ws, err := dial("http://ws-1." + domain)
	check(t, err)
	defer ws.Close()
	for _, msg := range []string{"hello", "again"} {
		check(t, websocket.Message.Send(ws, msg))
		var got string
		ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		check(t, websocket.Message.Receive(ws, &got))
		if got != msg {
			t.Fatalf("echoed %q, want %q", got, msg)
		}
	}

	// Another node's UI, or another site, may not open one.
	if ws, err := dial("http://ws-2." + domain); err == nil {
		ws.Close()
		t.Fatal("a WebSocket from another origin was let through")
	}
}
