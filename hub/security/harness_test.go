package security_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/hub/grainfiles"
	"github.com/bwsalmon/silo/hub/identity"
	"github.com/bwsalmon/silo/hub/live"
	"github.com/bwsalmon/silo/hub/nodeui"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
	"github.com/bwsalmon/silo/hub/server"
	"github.com/bwsalmon/silo/hub/tasks"
	"github.com/bwsalmon/silo/internal/pgtest"
)

// The people the suite signs in as. Team A is "team-a", owning the
// repositories acme/app (which node ws-a serves) and acme/quiet and
// acme/spare (which no node serves, so their tasks stay queued); team
// B is "team-b", owning beta/app, which node ws-b serves.
const (
	root  = "root@example.com"  // hub admin
	alice = "alice@example.com" // team A's admin; owns node ws-a
	dave  = "dave@example.com"  // team A's member
	zed   = "zed@example.com"   // team A's member, for removal
	carol = "carol@example.com" // team B's admin
	bob   = "bob@example.com"   // team B's member; owns node ws-b
	nina  = "nina@example.com"  // in no team
	pat   = "pat@example.com"   // in no team: files proposals

	teamA, teamB = "team-a", "team-b"
	repoA        = "acme/app"
	quietA       = "acme/quiet"
	spareA       = "acme/spare"
	repoB        = "beta/app"

	audience      = "/projects/1/global/backendServices/2"
	nodeDomain    = "nodes.hub.test"
	webhookSecret = "security-suite-webhook-secret"
)

var b64 = base64.RawURLEncoding

// iapKey is a fake IAP's signing key, published as a JWKS.
type iapKey struct {
	kid string
	k   *ecdsa.PrivateKey
}

func newIAPKey(t *testing.T, kid string) *iapKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &iapKey{kid, k}
}

func (s *iapKey) jwks() *httptest.Server {
	jwk := map[string]string{"kid": s.kid, "kty": "EC", "crv": "P-256",
		"x": b64.EncodeToString(s.k.X.FillBytes(make([]byte, 32))),
		"y": b64.EncodeToString(s.k.Y.FillBytes(make([]byte, 32)))}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{jwk}})
	}))
}

// sign signs an IAP assertion for email, with mod applied to its
// claims.
func (s *iapKey) sign(t *testing.T, email string, mod func(map[string]any)) string {
	t.Helper()
	now := time.Now()
	c := map[string]any{
		"iss": identity.IAPIssuer, "aud": audience, "sub": "accounts.google.com:" + email,
		"email": email, "iat": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(),
	}
	if mod != nil {
		mod(c)
	}
	h, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": s.kid, "typ": "JWT"})
	cb, _ := json.Marshal(c)
	in := b64.EncodeToString(h) + "." + b64.EncodeToString(cb)
	d := sha256.Sum256([]byte(in))
	r, ss, err := ecdsa.Sign(rand.Reader, s.k, d[:])
	if err != nil {
		t.Fatal(err)
	}
	return in + "." + b64.EncodeToString(append(r.FillBytes(make([]byte, 32)), ss.FillBytes(make([]byte, 32))...))
}

func claimsKey(t *testing.T) []byte {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

// logs collects the hub's log lines, for the suite to scan.
type logs struct {
	mu    sync.Mutex
	lines []string
	t     *testing.T
	done  bool
}

func (l *logs) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	line := fmt.Sprintf(format, args...)
	l.lines = append(l.lines, line)
	if !l.done && testing.Verbose() {
		l.t.Log(line)
	}
}

func (l *logs) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// hub is a hub serving on a listener of its own, behind a fake IAP.
type hub struct {
	t    *testing.T
	h    *server.Hub
	url  string
	key  *iapKey
	logs *logs
	db   string
}

// options vary the hub a test starts.
type options struct {
	database         string // sqlite:<temp> when empty
	grainfileFetcher grainfiles.RepoFetcher
	noWebhookSecret  bool
}

func startHub(t *testing.T, o options) *hub {
	t.Helper()
	key := newIAPKey(t, "iap-1")
	ks := key.jwks()
	t.Cleanup(ks.Close)
	lg := &logs{t: t}
	if o.database == "" {
		if os.Getenv("SILO_SECURITY_POSTGRES") != "" {
			o.database = pgtest.Start(t).URL
		} else {
			o.database = "sqlite:" + t.TempDir() + "/hub.db"
		}
	}
	whSecret := webhookSecret
	if o.noWebhookSecret {
		whSecret = ""
	}
	h, err := server.New(context.Background(), server.Config{
		Database:         o.database,
		IAPAudience:      audience,
		IAPKeys:          &identity.KeySet{URL: ks.URL},
		HubAdmins:        []string{"user:" + root},
		NodeUIDomain:     nodeDomain,
		InternalKey:      bytes.Repeat([]byte("k"), rendezvous.MinInternalKey),
		ClaimsIssuer:     "https://hub.test",
		ClaimsKeys:       [][]byte{claimsKey(t)},
		WebhookSecret:    whSecret,
		GrainfileFetcher: o.grainfileFetcher,
		Heartbeat:        time.Second,
		DispatchRetry:    time.Second,
		ShutdownGrace:    5 * time.Second,
		Logf:             lg.logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Serve(ctx, l) }()
	t.Cleanup(func() {
		cancel()
		<-done
		h.Close()
		// A failure the suite cannot reproduce locally is read from
		// what the hub said while it happened.
		if t.Failed() && !testing.Verbose() {
			t.Logf("hub log:\n%s", lg.all())
		}
		lg.mu.Lock()
		lg.done = true
		lg.mu.Unlock()
	})
	return &hub{t: t, h: h, url: "http://" + l.Addr().String(), key: key, logs: lg, db: o.database}
}

// as is a caller's credentials, applied to a request.
type as func(r *http.Request)

func (hb *hub) user(email string) as {
	tok := hb.key.sign(hb.t, email, nil)
	return func(r *http.Request) { r.Header.Set(identity.IAPHeader, tok) }
}

func bearer(tok string) as {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

func nobody(*http.Request) {}

// do makes a request to the hub's public handler and answers its
// status and body.
func (hb *hub) do(who as, method, path string, body any) (int, []byte, http.Header) {
	hb.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, hb.url+path, rd)
	if err != nil {
		hb.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	who(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		hb.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

// must makes a request that must answer want, decoding its body into
// out when out is not nil.
func (hb *hub) must(who as, method, path string, body any, want int, out any) {
	hb.t.Helper()
	code, b, _ := hb.do(who, method, path, body)
	if code != want {
		hb.t.Fatalf("%s %s: %d %s, want %d", method, path, code, b, want)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			hb.t.Fatalf("%s %s: %v: %s", method, path, err, b)
		}
	}
}

func (hb *hub) mint(owner, node string) (token, id string) {
	hb.t.Helper()
	var m struct {
		Token     string `json:"token"`
		NodeToken struct {
			ID string `json:"id"`
		} `json:"node_token"`
	}
	hb.must(hb.user(owner), "POST", "/v1/node-tokens", map[string]string{"node": node}, http.StatusCreated, &m)
	return m.Token, m.NodeToken.ID
}

func (hb *hub) task(who as, id string) (tasks.Task, int) {
	hb.t.Helper()
	code, b, _ := hb.do(who, "GET", tasks.TasksPath+"/"+id, nil)
	var tk tasks.Task
	if code == http.StatusOK {
		json.Unmarshal(b, &tk)
	}
	return tk, code
}

func (hb *hub) file(who string, body map[string]string) tasks.Task {
	hb.t.Helper()
	var tk tasks.Task
	hb.must(hb.user(who), "POST", tasks.TasksPath, body, http.StatusCreated, &tk)
	return tk
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// teams sets up team A and team B as the constants above say.
func (hb *hub) teams() {
	hb.t.Helper()
	r := hb.user(root)
	for _, tm := range []string{teamA, teamB} {
		hb.must(r, "POST", "/v1/teams", map[string]string{"id": tm, "name": tm}, http.StatusCreated, nil)
	}
	for _, m := range []struct{ team, who, role string }{
		{teamA, alice, "admin"}, {teamA, dave, "member"}, {teamA, zed, "member"},
		{teamB, carol, "admin"}, {teamB, bob, "member"},
	} {
		hb.must(r, "PUT", "/v1/teams/"+m.team+"/members/user:"+m.who, map[string]string{"role": m.role}, http.StatusNoContent, nil)
	}
	for _, rp := range []struct{ team, repo string }{{teamA, repoA}, {teamA, quietA}, {teamA, spareA}, {teamB, repoB}} {
		hb.must(r, "PUT", "/v1/teams/"+rp.team+"/repos/"+rp.repo, nil, http.StatusNoContent, nil)
	}
}

// fakeNode is a grain node's end of the hub's interfaces, as far as the
// suite needs: it accepts the offers it is told to, records what the
// hub sends down each task stream, serves the artifacts it listed and a
// UI, and writes the events it is told to.
type fakeNode struct {
	t    *testing.T
	id   string
	conn *rendezvous.Conn

	mu       sync.Mutex
	streams  map[string]*taskStream // by task ID
	offered  []string
	artifact string // what the view-through serves
}

type taskStream struct {
	c  net.Conn
	mu sync.Mutex
	in []tasks.Message
}

func (s *taskStream) write(m tasks.Message) error {
	b, _ := json.Marshal(m)
	_, err := s.c.Write(append(b, '\n'))
	return err
}

func (s *taskStream) saw(typ string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.in {
		if m.Type == typ {
			return true
		}
	}
	return false
}

// connect connects a node with token, serving repos.
func connectNode(t *testing.T, hb *hub, token, id string, repos ...string) *fakeNode {
	t.Helper()
	cl := &rendezvous.Client{
		Hub:   hb.url,
		Token: func(context.Context) (string, error) { return token, nil },
		Node:  registry.Node{ID: id, Repos: repos, Slots: 4, Features: []string{registry.FeatureInteractive}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := cl.Connect(ctx)
	if err != nil {
		t.Fatalf("node %s connecting: %v", id, err)
	}
	n := &fakeNode{t: t, id: id, conn: c, streams: map[string]*taskStream{}, artifact: "artifact bytes"}
	t.Cleanup(func() { c.Close() })
	go n.serve()
	return n
}

func (n *fakeNode) serve() {
	for {
		c, err := n.conn.Accept()
		if err != nil {
			return
		}
		go n.stream(c)
	}
}

// bufConn is a stream whose first bytes were read into r.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c bufConn) Read(b []byte) (int, error) { return c.r.Read(b) }

func (n *fakeNode) stream(c net.Conn) {
	r := bufio.NewReaderSize(c, 1<<16)
	line, err := r.ReadBytes('\n')
	if err != nil {
		c.Close()
		return
	}
	var head struct {
		Type string `json:"type"`
	}
	json.Unmarshal(line, &head)
	switch head.Type {
	case live.TypeView:
		var m live.ViewMessage
		json.Unmarshal(line, &m)
		live.ServeView(bufConn{c, r}, m, func(m live.ViewMessage) (io.ReadCloser, error) {
			n.mu.Lock()
			defer n.mu.Unlock()
			return io.NopCloser(strings.NewReader(n.artifact)), nil
		})
		return
	case nodeui.TypeHTTP:
		var m nodeui.Message
		json.Unmarshal(line, &m)
		nodeui.Serve(bufConn{c, r}, m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "node %s's UI", n.id)
		}))
		return
	}
	var m tasks.Message
	if json.Unmarshal(line, &m) != nil || m.Task == nil {
		c.Close()
		return
	}
	s := &taskStream{c: c, in: []tasks.Message{m}}
	n.mu.Lock()
	if m.Type == tasks.TypeOffer {
		n.offered = append(n.offered, m.Task.ID)
	}
	n.streams[m.Task.ID] = s
	n.mu.Unlock()
	if m.Type == tasks.TypeOffer || m.Type == tasks.TypeResume {
		s.write(tasks.Message{Type: tasks.TypeAccept, NodeTask: "local-" + m.Task.ID})
	}
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var m tasks.Message
		if json.Unmarshal(line, &m) == nil {
			s.mu.Lock()
			s.in = append(s.in, m)
			s.mu.Unlock()
		}
	}
}

func (n *fakeNode) taskStream(id string) *taskStream {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.streams[id]
}

// events opens an event stream and writes lines on it.
func (n *fakeNode) events(lines ...string) net.Conn {
	n.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	up, err := n.conn.Open(ctx)
	if err != nil {
		n.t.Fatal(err)
	}
	for _, l := range lines {
		if _, err := io.WriteString(up, l+"\n"); err != nil {
			n.t.Fatal(err)
		}
	}
	return up
}

// event is one event line for task.
func event(task string, seq int64, typ, payload string) string {
	return fmt.Sprintf(`{"v":1,"seq":%d,"type":%q,"task":%q,%q:%s}`, seq, typ, task, typ, payload)
}

// storedEvents reads what a subscriber to task as who gets from the
// replay, up to want events or 5s.
func (hb *hub) replay(who as, task string, from int64, want int) (int, []live.Event) {
	hb.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s%s/%s/events?from=%d", hb.url, tasks.TasksPath, task, from), nil)
	who(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		hb.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	var out []live.Event
	sc := bufio.NewScanner(resp.Body)
	for len(out) < want && sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var e live.Event
		if json.Unmarshal([]byte(data), &e) == nil && e.Seq > 0 {
			out = append(out, e)
		}
	}
	return resp.StatusCode, out
}
