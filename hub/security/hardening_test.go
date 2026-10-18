package security_test

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/events"
	"github.com/bwsalmon/silo/hub/grainfiles"
	"github.com/bwsalmon/silo/hub/rendezvous"
	"github.com/bwsalmon/silo/hub/tasks"
)

// up checks the hub still answers.
func (hb *hub) up(t *testing.T) {
	t.Helper()
	if code, _, _ := hb.do(nobody, "GET", "/healthz", nil); code != http.StatusOK {
		t.Fatalf("/healthz: %d", code)
	}
	if code, _, _ := hb.do(hb.user(alice), "GET", "/v1/me", nil); code != http.StatusOK {
		t.Fatalf("/v1/me: %d", code)
	}
}

// TestOversizedPayloads: bodies past each API's bound are refused, not
// read whole, and the hub goes on.
func TestOversizedPayloads(t *testing.T) {
	hb := startHub(t, options{})
	hb.teams()
	big := strings.Repeat("x", 8<<20)
	for _, r := range []struct {
		who          string
		method, path string
		body         any
	}{
		{root, "POST", "/v1/teams", map[string]string{"id": "big", "name": big}},
		{alice, "PUT", "/v1/teams/team-a/settings", map[string]string{"x": big}},
		{alice, "POST", "/v1/node-tokens", map[string]string{"node": big}},
		{alice, "POST", tasks.TasksPath, map[string]string{"repo": quietA, "prompt": big}},
		{alice, "POST", tasks.TasksPath, map[string]string{"repo": quietA, "prompt": "p", "title": big}},
		{alice, "POST", tasks.TasksPath + "/t-1/cancel", map[string]string{"reason": big}},
	} {
		code, b, _ := hb.do(hb.user(r.who), r.method, r.path, r.body)
		if code < 400 || code >= 500 {
			t.Errorf("%s %s with an 8 MiB body: %d %.200s, want a 4xx", r.method, r.path, code, b)
		}
	}
	// A header block past the server's bound.
	req, _ := http.NewRequest("GET", hb.url+"/v1/me", nil)
	hb.user(alice)(req)
	req.Header.Set("X-Pad", strings.Repeat("y", 2<<20))
	if resp, err := http.DefaultClient.Do(req); err == nil {
		if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
			t.Errorf("a 2 MiB header: %d, want 431", resp.StatusCode)
		}
		resp.Body.Close()
	}
	// An oversized webhook payload (> 5 MiB) signed with a valid HMAC is refused.
	bigWebhook := []byte(`{"ref":"refs/heads/main","pad":"` + big + `"}`)
	whReq, _ := http.NewRequest("POST", hb.url+grainfiles.GitHubWebhookPath, strings.NewReader(string(bigWebhook)))
	whReq.Header.Set("Content-Type", "application/json")
	whReq.Header.Set("X-GitHub-Event", "push")
	whReq.Header.Set("X-Hub-Signature-256", signWebhook(webhookSecret, bigWebhook))
	if resp, err := http.DefaultClient.Do(whReq); err == nil {
		if resp.StatusCode < 400 || resp.StatusCode >= 500 {
			t.Errorf("POST %s with 8 MiB webhook body: %d, want 4xx", grainfiles.GitHubWebhookPath, resp.StatusCode)
		}
		resp.Body.Close()
	}
	hb.up(t)
}

// handshake opens a raw connection to the rendezvous and completes its
// WebSocket handshake with token, answering the connection and what is
// left of the answer.
func handshake(t *testing.T, hb *hub, token string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(hb.url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 16)
	rand.Read(key)
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: hub\r\nAuthorization: Bearer %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Protocol: %s\r\nOrigin: http://hub\r\n\r\n",
		rendezvous.ConnectPath, token, base64.StdEncoding.EncodeToString(key), rendezvous.Protocol)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake: %v %v", resp, err)
	}
	return c, br
}

// closedWithin waits for the hub to close c.
func closedWithin(c net.Conn, r io.Reader, d time.Duration) bool {
	c.SetReadDeadline(time.Now().Add(d))
	_, err := io.Copy(io.Discard, r)
	return err == nil || !isTimeout(err)
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

// TestMalformedRendezvousFrames: a node, past the handshake, that
// sends what is not a WebSocket frame, a frame the multiplexer cannot
// read, or event lines that are not events or are too long, is cut off
// or has its stream ended, and the hub and its other nodes go on.
func TestMalformedRendezvousFrames(t *testing.T) {
	w := newWorld(t)
	hb := w.hb
	tok, _ := hb.mint(alice, "ws-junk")

	for name, junk := range map[string][]byte{
		"not a frame":       []byte("this is not a websocket frame\r\n\r\n"),
		"a frame too long":  {0x82, 0xff, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 1, 2, 3, 4},
		"random bytes":      randomBytes(4096),
		"an unmasked frame": append([]byte{0x82, 0x7e, 0x10, 0x00}, randomBytes(4096)...),
		"a masked garbage":  append([]byte{0x82, 0xfe, 0x10, 0x00, 1, 2, 3, 4}, randomBytes(4096)...),
	} {
		t.Run(name, func(t *testing.T) {
			c, br := handshake(t, hb, tok)
			defer c.Close()
			c.Write(junk)
			if !closedWithin(c, br, 45*time.Second) {
				t.Errorf("the hub kept a connection that sent %s", name)
			}
		})
	}

	// Event lines that are not events, too long, or binary.
	up := w.nodeA.events(
		"not json",
		`{"v":1}`,
		strings.Repeat("z", 100),
		string(randomBytes(512)),
	)
	up.Write([]byte(strings.Repeat("a", 2<<20) + "\n"))
	up.Close()
	// The node's own events still flow.
	w.nodeA.events(event(w.runA.ID, 60, events.TypeStatus, `{"phase":"running","state":"info","message":"still here"}`)).Close()
	eventually(t, "a good event after the junk", func() bool {
		_, evs := hb.replay(hb.user(alice), w.runA.ID, 59, 1)
		return len(evs) == 1 && evs[0].Seq == 60
	})
	hb.up(t)
	if got, _ := hb.task(hb.user(bob), w.runB.ID); got.State != tasks.Running {
		t.Errorf("team B's task after the junk: %s", got.State)
	}
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

// TestSlowClients holds many connections open the slowloris way: half
// a request's headers, a WebSocket handshake for an event subscription
// that is never read, and a rendezvous handshake that never says hello.
// While they are open the hub still serves people and nodes, and it
// closes the slow ones within its documented bounds: the header
// timeout and the hello timeout (30s each).
func TestSlowClients(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the hub's 30s timeouts")
	}
	w := newWorld(t)
	hb := w.hb
	addr := strings.TrimPrefix(hb.url, "http://")
	tok, _ := hb.mint(alice, "ws-slow")

	var (
		mu    sync.Mutex
		conns []net.Conn
		rs    []io.Reader
	)
	keep := func(c net.Conn, r io.Reader) {
		mu.Lock()
		conns, rs = append(conns, c), append(rs, r)
		mu.Unlock()
	}
	for range 40 {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(c, "GET /v1/me HTTP/1.1\r\nHost: hub\r\nX-Slow: ")
		keep(c, c)
	}
	for range 10 {
		c, br := handshake(t, hb, tok)
		keep(c, br)
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	// Meanwhile, everyone else is served.
	hb.up(t)
	tokB, _ := hb.mint(bob, "ws-b2")
	if _, err := dial(t, hb, tokB, "ws-b2", repoB); err != nil {
		t.Errorf("a node connecting beside the slow clients: %v", err)
	}
	if code, _ := try(t, hb.user(alice), "GET", hb.url+tasks.TasksPath+"/"+w.runA.ID+"/events", "", nil, false); code != http.StatusOK {
		t.Errorf("subscribing beside the slow clients: %d", code)
	}

	// And the slow ones are closed.
	var wg sync.WaitGroup
	var open sync.Map
	for i, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !closedWithin(c, rs[i], 45*time.Second) {
				open.Store(i, true)
			}
		}()
	}
	wg.Wait()
	n := 0
	open.Range(func(any, any) bool { n++; return true })
	if n > 0 {
		t.Errorf("%d of %d slow connections were still open after 45s", n, len(conns))
	}
	hb.up(t)
}
