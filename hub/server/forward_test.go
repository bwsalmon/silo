package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/hub/live"
	"github.com/bwsalmon/silo/hub/nodeui"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
	"github.com/bwsalmon/silo/hub/tasks"
	"github.com/bwsalmon/silo/internal/pgtest"
)

// streamNode is a node answering every kind of stream the hub opens:
// it accepts offers and resumes, serves views of one diff, and serves
// its UI.
type streamNode struct{ node }

const forwardedDiff = "--- a/main.go\n+++ b/main.go\n"

func (n *streamNode) handle(c net.Conn) {
	br := bufio.NewReader(c)
	line, err := br.ReadBytes('\n')
	if err != nil {
		c.Close()
		return
	}
	var m struct {
		Type string `json:"type"`
	}
	json.Unmarshal(line, &m)
	n.mu.Lock()
	n.seen = append(n.seen, m.Type)
	n.mu.Unlock()
	switch m.Type {
	case live.TypeView:
		var v live.ViewMessage
		json.Unmarshal(line, &v)
		live.ServeView(c, v, func(v live.ViewMessage) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(forwardedDiff)), nil
		})
	case nodeui.TypeHTTP:
		var u nodeui.Message
		json.Unmarshal(line, &u)
		nodeui.Serve(c, u, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "ws-1's UI for %s", r.Header.Get("X-Forwarded-Host"))
		}))
	default:
		defer c.Close()
		if m.Type == tasks.TypeOffer || m.Type == tasks.TypeResume {
			b, _ := json.Marshal(tasks.Message{Type: tasks.TypeAccept, NodeTask: "local-1"})
			c.Write(append(b, '\n'))
		}
		// The rest of the task's stream: a cancel, for one.
		for {
			line, err := br.ReadBytes('\n')
			if err != nil {
				return
			}
			json.Unmarshal(line, &m)
			n.mu.Lock()
			n.seen = append(n.seen, m.Type)
			n.mu.Unlock()
		}
	}
}

// TestForwardedOpen runs two hubs on one PostgreSQL database, as two
// pods would (docs/hub.md §2.2.3): a node connected to the pod that
// does not dispatch is offered a task and cancelled by the pod that
// does, and its artifacts are viewed and its UI served through it, each
// stream forwarded over the holding pod's internal listener. That
// listener refuses tokens that are missing, wrong, expired or minted
// for another pod.
func TestForwardedOpen(t *testing.T) {
	pg := pgtest.Start(t)
	key := []byte("0123456789abcdef0123456789abcdef")
	cfg := func(pod string) Config {
		return Config{
			Database:      pg.URL,
			DevUserHeader: userHeader,
			HubAdmins:     []string{"admin@example.com"},
			NodeUIDomain:  "nodes.hub.test",
			Heartbeat:     200 * time.Millisecond,
			ShutdownGrace: 10 * time.Second,
			DispatchRetry: time.Second,
			Holder:        pod,
			Pod:           pod,
			PodLease:      600 * time.Millisecond,
			InternalAddr:  "127.0.0.1:0",
			InternalKey:   key,
			Logf:          logf(t),
		}
	}
	hubs := map[string]*running{}
	for _, pod := range []string{"pod-a", "pod-b"} {
		hubs[pod] = start(t, cfg(pod), "127.0.0.1:0")
		eventually(t, pod+" ready", func() bool { return call(t, "GET", hubs[pod].url+ReadyzPath, "", nil, nil) == http.StatusOK })
	}
	db := sqlDB(t, pg.URL)
	var leader string
	eventually(t, "a dispatcher", func() bool {
		return db.QueryRow(`SELECT holder FROM hub_leases WHERE name = 'dispatcher'`).Scan(&leader) == nil && leader != ""
	})
	// The node connects to the pod that does not dispatch.
	other := map[string]string{"pod-a": "pod-b", "pod-b": "pod-a"}[leader]
	disp, held := hubs[leader], hubs[other]

	call(t, "POST", disp.url+"/v1/teams", "admin@example.com", map[string]string{"id": "infra", "name": "Infra"}, &struct{}{})
	call(t, "PUT", disp.url+"/v1/teams/infra/members/user:alice@example.com", "admin@example.com", map[string]string{"role": "member"}, &struct{}{})
	call(t, "PUT", disp.url+"/v1/teams/infra/repos/bwsalmon/silo", "admin@example.com", nil, &struct{}{})
	var minted struct {
		Token string `json:"token"`
	}
	call(t, "POST", disp.url+"/v1/node-tokens", "alice@example.com", map[string]string{"node": "ws-1"}, &minted)
	n := &streamNode{}
	cl := &rendezvous.Client{
		Hub:   held.url,
		Token: func(context.Context) (string, error) { return minted.Token, nil },
		Node:  registry.Node{ID: "ws-1", Repos: []string{"bwsalmon/silo"}, Slots: 1},
	}
	nctx, ncancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() { ran <- cl.Run(nctx, n.handle) }()
	t.Cleanup(func() {
		ncancel()
		<-ran
	})
	eventually(t, "ws-1 online on the dispatching pod", func() bool { return online(t, disp.url, "ws-1") })

	// The offer, from the dispatching pod through the holding one.
	var task tasks.Task
	call(t, "POST", disp.url+tasks.TasksPath, "alice@example.com", map[string]string{"repo": "bwsalmon/silo", "prompt": "do the thing"}, &task)
	eventually(t, "the task offered to ws-1", func() bool { return n.saw(tasks.TypeOffer) })
	eventually(t, "the task running", func() bool { return taskState(t, disp.url, task.ID) == tasks.Running })

	// A view, and the node's UI, through the pod that does not hold it.
	get := func(host, url string) (int, string) {
		req, _ := http.NewRequest("GET", url, nil)
		if host != "" {
			req.Host = host
		}
		req.Header.Set(userHeader, "alice@example.com")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// diff-1 as the node lists it, through whichever pod holds its
	// event stream: the refs are shared in the database.
	if _, err := db.Exec(`INSERT INTO task_refs (task, ref, seq) VALUES ($1, 'diff-1', 1)`, task.ID); err != nil {
		t.Fatal(err)
	}
	if code, body := get("", disp.url+tasks.TasksPath+"/"+task.ID+"/artifacts?ref=diff-1"); code != http.StatusOK || body != forwardedDiff {
		t.Fatalf("viewing diff-1 through the other pod: %d %q", code, body)
	}
	if code, body := get("ws-1.nodes.hub.test", disp.url+"/"); code != http.StatusOK || body != "ws-1's UI for ws-1.nodes.hub.test" {
		t.Fatalf("ws-1's UI through the other pod: %d %q", code, body)
	}

	// The cancel rides the forwarded offer stream.
	call(t, "POST", disp.url+tasks.TasksPath+"/"+task.ID+"/cancel", "alice@example.com", map[string]string{"reason": "enough"}, &struct{}{})
	eventually(t, "the cancel reaching ws-1", func() bool { return n.saw(tasks.TypeCancel) })

	// The holding pod's internal listener, asked directly.
	var addr string
	if err := db.QueryRow(`SELECT addr FROM hub_pods WHERE pod = $1`, other).Scan(&addr); err != nil {
		t.Fatal(err)
	}
	open := func(node, token string) int {
		req, _ := http.NewRequest("GET", "http://"+addr+"/internal/v1/nodes/"+node+"/open", nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", rendezvous.OpenUpgrade)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	now := time.Now()
	for _, c := range []struct {
		what, node, token string
		want              int
	}{
		{"no token", "ws-1", "", http.StatusUnauthorized},
		{"a bad token", "ws-1", strings.Repeat("0", 64), http.StatusUnauthorized},
		{"another key's token", "ws-1", rendezvous.InternalToken([]byte("another key, another key, another"), other, now), http.StatusUnauthorized},
		{"an expired token", "ws-1", rendezvous.InternalToken(key, other, now.Add(-5*time.Minute)), http.StatusUnauthorized},
		{"another pod's token", "ws-1", rendezvous.InternalToken(key, leader, now), http.StatusUnauthorized},
		{"a node it does not hold", "ws-2", rendezvous.InternalToken(key, other, now), http.StatusNotFound},
		{"a good token", "ws-1", rendezvous.InternalToken(key, other, now), http.StatusSwitchingProtocols},
	} {
		if got := open(c.node, c.token); got != c.want {
			t.Errorf("internal open with %s: %d, want %d", c.what, got, c.want)
		}
	}

	// The node goes: the dispatching pod finds it offline.
	held.shutdown(t)
	if code, _ := get("", disp.url+tasks.TasksPath+"/"+task.ID+"/artifacts?ref=diff-1"); code != http.StatusServiceUnavailable {
		t.Errorf("viewing on a node no pod holds: %d, want 503", code)
	}
}
