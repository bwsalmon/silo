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
	"github.com/bwsalmon/silo/internal/testlog"
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
	// The hub's servers can log after the test returns: httptest does
	// not wait for hijacked connections.
	logf := testlog.Logf(t)
	rv := &rendezvous.Server{
		Registry: registry.New(),
		Authenticate: func(r *http.Request) (rendezvous.Identity, error) {
			tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if tok == "" {
				return rendezvous.Identity{}, errors.New("no token")
			}
			return rendezvous.Identity{Owner: tok}, nil
		},
		ValidateUI: dir.ValidateNodeUI,
		Heartbeat:  time.Second,
		Logf:       logf,
	}
	ui := &Server{
		Domain:    domain,
		Registry:  rv.Registry,
		Directory: &directory.API{Store: dir, Auth: headerAuth{}, HubAdmins: []string{"user:hubadmin@example.com"}},
		Open:      rv.Open,
		Logf:      logf,
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
	return h.connectNode(t, registry.Node{ID: id, Slots: 1, UI: &registry.NodeUI{Offered: true}}, owner, ui)
}

func (h *harness) connectNode(t *testing.T, node registry.Node, owner string, ui http.Handler) (*rendezvous.Conn, <-chan Message) {
	t.Helper()
	if node.Slots == 0 {
		node.Slots = 1
	}
	cl := &rendezvous.Client{
		Hub:   h.rvURL,
		Token: func(context.Context) (string, error) { return owner, nil },
		Node:  node,
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
		"ws-1.nodes.example.com":         "ws-1",
		"WS-1.nodes.example.com:443":     "ws-1",
		"ws-1.nodes.example.com.":        "ws-1",
		"nodes.example.com":              "",
		"a.b.nodes.example.com":          "",
		"ws_1.nodes.example.com":         "",
		"ws-1.nodes.example.com.evil":    "",
		"ws-1.example.com":               "",
		"-x.nodes.example.com":           "",
		"xn--ws-1.nodes.example.com":     "",
		"XN--nxasmq6b.nodes.example.com": "",
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
	r.Header.Set("Cookie", "grain_session=s0; "+identity.SessionCookie+"=hub-session; GCP_IAP_UID=123; __Host-GCP_IAP_LS_TOKEN=tok")
	r.Header.Set("Authorization", "Bearer hub-token")
	r.Header.Set("X-Forwarded-User", "mallory@example.com")
	r.Header.Set(identity.IAPHeader, "the-assertion")
	r.Header.Set("X-Goog-Authenticated-User-Email", "accounts.google.com:alice@example.com")
	r.Header.Set("X-Goog-Authenticated-User-ID", "accounts.google.com:123")
	resp, body := h.do(t, r, host, "alice@example.com")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/hello: %d %q", resp.StatusCode, body)
	}
	want := `host=ws-1.nodes.hub.test path=/api/hello query=a=1 cookie="grain_session=s0" authz="" user="" xff=true iap=""`
	if body != want {
		t.Fatalf("the node saw\n %s\nwant\n %s", body, want)
	}
	if m := <-asked; m.Node != "ws-1" || m.Viewer != "alice@example.com" || m.Role != RoleOwner {
		t.Fatalf("the node was asked %+v", m)
	}
	if got := resp.Header.Values("Set-Cookie"); len(got) != 1 || !strings.HasPrefix(got[0], "grain_session=s1") || !strings.Contains(got[0], "SameSite=Lax") {
		t.Fatalf("the viewer was sent cookies %q: want the node's own with SameSite=Lax, not one for its domain", got)
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q, want no-referrer", got)
	}
	if got := resp.Header.Get("Cross-Origin-Opener-Policy"); got != "same-origin" {
		t.Fatalf("Cross-Origin-Opener-Policy = %q, want same-origin", got)
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

func TestTeamAdminAndRegistrationValidation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	dir := h.ui.Directory.Store
	check(t, dir.CreateTeam(ctx, "infra", "Infra"))
	check(t, dir.SetMember(ctx, "infra", "user:alice@example.com", directory.Member))
	check(t, dir.SetMember(ctx, "infra", "user:tadmin@example.com", directory.Admin))
	check(t, dir.SetMember(ctx, "infra", "user:tmember@example.com", directory.Member))
	check(t, dir.AssignRepo(ctx, "infra", "org/infra-repo"))

	check(t, dir.CreateTeam(ctx, "other", "Other"))
	check(t, dir.SetMember(ctx, "other", "user:alice@example.com", directory.Member))
	check(t, dir.SetMember(ctx, "other", "user:oadmin@example.com", directory.Admin))
	check(t, dir.AssignRepo(ctx, "other", "org/other-repo"))

	// 1. Valid ui.team: owner is in "infra" and "infra" owns every repo.
	_, asked := h.connectNode(t, registry.Node{
		ID:    "ws-team",
		Repos: []string{"org/infra-repo"},
		UI:    &registry.NodeUI{Offered: true, Team: "infra", Access: "read-only"},
	}, "alice@example.com", nodeUI(nil))

	if e, ok := h.ui.Registry.Get("ws-team"); !ok || e.UI == nil || !e.UI.Offered || e.UI.Team != "infra" || e.UI.Reason != "" {
		t.Fatalf("ws-team entry = %+v, UI = %+v", e, e.UI)
	}

	// Owner gets 200 with role: "owner", team: "infra".
	if resp, _ := h.get(t, "ws-team."+domain, "/api/hello", "alice@example.com"); resp.StatusCode != http.StatusOK {
		t.Fatalf("owner: %d", resp.StatusCode)
	}
	if m := <-asked; m.Role != RoleOwner || m.Team != "infra" || m.Viewer != "alice@example.com" {
		t.Fatalf("owner handshake = %+v", m)
	}

	// Team admin of "infra" gets 200 with role: "team-admin", team: "infra".
	if resp, _ := h.get(t, "ws-team."+domain, "/api/hello", "tadmin@example.com"); resp.StatusCode != http.StatusOK {
		t.Fatalf("team-admin: %d", resp.StatusCode)
	}
	if m := <-asked; m.Role != RoleTeamAdmin || m.Team != "infra" || m.Viewer != "tadmin@example.com" {
		t.Fatalf("team-admin handshake = %+v", m)
	}

	// Regular team member, other team's admin, and hub admin all get 404.
	for _, viewer := range []string{"tmember@example.com", "oadmin@example.com", "hubadmin@example.com", "stranger@example.com"} {
		if resp, _ := h.get(t, "ws-team."+domain, "/api/hello", viewer); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("viewer %s on ws-team: got %d, want 404", viewer, resp.StatusCode)
		}
	}

	// 2. Mixed repos: node serves a repo owned by "other" while claiming ui.team="infra".
	// Registration succeeds, but ui.team is dropped with ui.reason, falling back to owner-only.
	_, askedMixed := h.connectNode(t, registry.Node{
		ID:    "ws-mixed",
		Repos: []string{"org/infra-repo", "org/other-repo"},
		UI:    &registry.NodeUI{Offered: true, Team: "infra", Access: "read-only"},
	}, "alice@example.com", nodeUI(nil))

	if e, ok := h.ui.Registry.Get("ws-mixed"); !ok || e.UI == nil || !e.UI.Offered || e.UI.Team != "" || !strings.Contains(e.UI.Reason, "org/other-repo") {
		t.Fatalf("ws-mixed UI = %+v; want Offered=true, Team=\"\", Reason mentioning org/other-repo", e.UI)
	}
	if resp, _ := h.get(t, "ws-mixed."+domain, "/api/hello", "tadmin@example.com"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("team-admin on ws-mixed: got %d, want 404", resp.StatusCode)
	}
	if resp, _ := h.get(t, "ws-mixed."+domain, "/api/hello", "alice@example.com"); resp.StatusCode != http.StatusOK {
		t.Fatalf("owner on ws-mixed: got %d, want 200", resp.StatusCode)
	}
	if m := <-askedMixed; m.Role != RoleOwner || m.Team != "" {
		t.Fatalf("owner handshake on ws-mixed = %+v", m)
	}

	// 3. Owner not a member of ui.team: ui.team is dropped with ui.reason.
	check(t, dir.CreateTeam(ctx, "secret", "Secret"))
	check(t, dir.SetMember(ctx, "secret", "user:sadmin@example.com", directory.Admin))
	h.connectNode(t, registry.Node{
		ID: "ws-notmember",
		UI: &registry.NodeUI{Offered: true, Team: "secret"},
	}, "alice@example.com", nodeUI(nil))
	if e, ok := h.ui.Registry.Get("ws-notmember"); !ok || e.UI == nil || !e.UI.Offered || e.UI.Team != "" || !strings.Contains(e.UI.Reason, "not a member") {
		t.Fatalf("ws-notmember UI = %+v; want Team=\"\" with not-a-member reason", e.UI)
	}
	if resp, _ := h.get(t, "ws-notmember."+domain, "/api/hello", "sadmin@example.com"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("sadmin on ws-notmember: got %d, want 404", resp.StatusCode)
	}

	// 4. Punycode and non-DNS-label node IDs: registration succeeds, ui.offered is dropped with ui.reason.
	h.connectNode(t, registry.Node{
		ID: "xn--ws-1",
		UI: &registry.NodeUI{Offered: true, Team: "infra"},
	}, "alice@example.com", nodeUI(nil))
	if e, ok := h.ui.Registry.Get("xn--ws-1"); !ok || e.UI == nil || e.UI.Offered || !strings.Contains(e.UI.Reason, "punycode") {
		t.Fatalf("xn--ws-1 UI = %+v; want Offered=false with punycode reason", e.UI)
	}
	if resp, _ := h.get(t, "xn--ws-1."+domain, "/api/hello", "alice@example.com"); resp.StatusCode != http.StatusTeapot {
		// Node(host) rejects xn--ws-1, so Hosts passes it to the hub handler (418 in harness), never proxying to the node.
		t.Fatalf("xn--ws-1 host: got %d, want 418 (not a node UI host)", resp.StatusCode)
	}
	r, _ := http.NewRequest("GET", "http://xn--ws-1."+domain+"/api/hello", nil)
	r.Host = "xn--ws-1." + domain
	r.Header.Set("X-User", "alice@example.com")
	w := httptest.NewRecorder()
	h.ui.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("direct ServeHTTP on xn--ws-1: got %d, want 404", w.Code)
	}

	h.connectNode(t, registry.Node{
		ID: "WS_bad",
		UI: &registry.NodeUI{Offered: true},
	}, "alice@example.com", nodeUI(nil))
	if e, ok := h.ui.Registry.Get("WS_bad"); !ok || e.UI == nil || e.UI.Offered || e.UI.Reason == "" {
		t.Fatalf("WS_bad UI = %+v; want Offered=false with DNS label reason", e.UI)
	}
}

func TestCookieAndHeaderHardening(t *testing.T) {
	h := newHarness(t)
	customUI := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, v := range []string{
			"grain_ok=1; Path=/; Secure",
			"grain_nosecure=1; Path=/",
			"silo_session=forged; Path=/; Secure",
			"silo_oidc_flow=forged; Path=/; Secure",
			"__Host-evil=1; Path=/; Secure",
			"GCP_IAP_UID=forged; Path=/; Secure",
			"domain_cookie=1; Domain=" + domain + "; Path=/; Secure",
		} {
			w.Header().Add("Set-Cookie", v)
		}
		w.WriteHeader(http.StatusOK)
	})
	h.connect(t, "ws-sec", "alice@example.com", customUI)

	r, _ := http.NewRequest("GET", "http://x/test", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	resp, _ := h.do(t, r, "ws-sec."+domain, "alice@example.com")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got := resp.Header.Values("Set-Cookie")
	if len(got) != 1 || !strings.HasPrefix(got[0], "grain_ok=1") || !strings.Contains(got[0], "Secure") || !strings.Contains(got[0], "SameSite=Lax") {
		t.Fatalf("HTTPS Set-Cookie = %q; want only grain_ok=1 with Secure and SameSite=Lax", got)
	}
	if resp.Header.Get("Referrer-Policy") != "no-referrer" || resp.Header.Get("Cross-Origin-Opener-Policy") != "same-origin" {
		t.Fatalf("security headers = %v", resp.Header)
	}
}
