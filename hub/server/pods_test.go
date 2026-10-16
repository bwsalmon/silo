package server

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
	"github.com/bwsalmon/silo/hub/tasks"
	"github.com/bwsalmon/silo/internal/pgtest"
)

// sseEvent is one Server-Sent Event as a subscriber reads it.
type sseEvent struct{ typ, data string }

// subscribeSSE subscribes to a task's events as alice and passes on
// each event it reads; the channel closes when the stream ends.
func subscribeSSE(t *testing.T, url string) <-chan sseEvent {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set(userHeader, "alice@example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("subscribing to %s: %s", url, resp.Status)
	}
	ch := make(chan sseEvent, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.typ != "" {
					ch <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, "event: "):
				ev.typ = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return ch
}

// next reads the subscription's next event, failing after a while.
func next(t *testing.T, ch <-chan sseEvent, what string) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatalf("the subscription ended before %s", what)
		}
		return ev
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
	return sseEvent{}
}

// TestAcrossPods runs two hubs on one PostgreSQL database, as two pods
// would (docs/hub.md §2.2.4), with a node connected to the pod that
// does not dispatch. With the dispatcher's Retry and the mirrors'
// refills too slow to matter, the dispatching pod sees the node at
// once, from its registration's notice; a task submitted on the other
// pod is placed at once, from its wake-up; and a cancel made there
// reaches the task's stream, from its notice. A subscriber on the
// dispatching pod is forwarded to the holding pod and gets the token
// event its ring holds, the result and the end; with the node gone, it
// gets the stored events and the end from the database.
func TestAcrossPods(t *testing.T) {
	pg := pgtest.Start(t)
	key := []byte("0123456789abcdef0123456789abcdef")
	type pod struct {
		h   *Hub
		url string
	}
	run := func(name string) *pod {
		h, err := New(context.Background(), Config{
			Database:      pg.URL,
			DevUserHeader: userHeader,
			HubAdmins:     []string{"admin@example.com"},
			ShutdownGrace: 10 * time.Second,
			DispatchRetry: time.Hour,
			Holder:        name,
			Pod:           name,
			// A renewal, and so a refill of the mirror, every 20s.
			PodLease:     time.Minute,
			InternalAddr: "127.0.0.1:0",
			InternalKey:  key,
			Logf:         logf(t),
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
		})
		p := &pod{h: h, url: "http://" + l.Addr().String()}
		eventually(t, name+" listening", func() bool {
			return call(t, "GET", p.url+ReadyzPath, "", nil, nil) == http.StatusOK && h.Bus.Listening()
		})
		return p
	}
	// One after the other: concurrent CREATE TABLE IF NOT EXISTS can
	// fail on PostgreSQL.
	pods := map[string]*pod{"pod-a": run("pod-a")}
	pods["pod-b"] = run("pod-b")
	db := sqlDB(t, pg.URL)
	var leader string
	eventually(t, "a dispatcher", func() bool {
		return db.QueryRow(`SELECT holder FROM hub_leases WHERE name = 'dispatcher'`).Scan(&leader) == nil && leader != ""
	})
	other := map[string]string{"pod-a": "pod-b", "pod-b": "pod-a"}[leader]
	disp, held := pods[leader], pods[other]
	eventually(t, leader+" dispatching", func() bool {
		return strings.Contains(scrape(t, disp.h), "\nsilo_hub_dispatcher_leader 1\n")
	})
	soon := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("%s took longer than a notice should", what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	call(t, "POST", held.url+"/v1/teams", "admin@example.com", map[string]string{"id": "infra", "name": "Infra"}, &struct{}{})
	call(t, "PUT", held.url+"/v1/teams/infra/members/user:alice@example.com", "admin@example.com", map[string]string{"role": "member"}, &struct{}{})
	call(t, "PUT", held.url+"/v1/teams/infra/repos/bwsalmon/silo", "admin@example.com", nil, &struct{}{})
	var minted struct {
		Token string `json:"token"`
	}
	call(t, "POST", held.url+"/v1/node-tokens", "alice@example.com", map[string]string{"node": "ws-1"}, &minted)

	// The node connects to the pod that does not dispatch, and the
	// dispatching pod's mirror has it from the registration's notice.
	n := &streamNode{}
	cl := &rendezvous.Client{
		Hub:   held.url,
		Token: func(context.Context) (string, error) { return minted.Token, nil },
		Node:  registry.Node{ID: "ws-1", Repos: []string{"bwsalmon/silo"}, Slots: 1},
	}
	nctx, ncancel := context.WithCancel(context.Background())
	defer ncancel()
	conn, err := cl.Connect(nctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	go func() {
		for {
			c, err := conn.Accept()
			if err != nil {
				return
			}
			go n.handle(c)
		}
	}()
	soon("ws-1 online on the dispatching pod", func() bool { return online(t, disp.url, "ws-1") })

	// Submitted on the pod that does not dispatch: its wake-up places it.
	var task tasks.Task
	call(t, "POST", held.url+tasks.TasksPath, "alice@example.com", map[string]string{"repo": "bwsalmon/silo", "prompt": "do the thing"}, &task)
	soon("the task offered to ws-1", func() bool { return n.saw(tasks.TypeOffer) })
	eventually(t, "the task running", func() bool { return taskState(t, held.url, task.ID) == tasks.Running })

	// The node streams a stored event and a token event up to the pod
	// holding it.
	es, err := conn.Open(nctx)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(es, `{"v":1,"seq":1,"task":%q,"type":"status","status":{"phase":"running","state":"info","message":"working"}}`+"\n", task.ID)
	fmt.Fprintf(es, `{"v":1,"seq":2,"task":%q,"type":"transcript","transcript":{"kind":"text","text":"thinking aloud"}}`+"\n", task.ID)

	// A subscriber on the dispatching pod is forwarded to the holding
	// one, whose ring has the token event.
	events := tasks.TasksPath + "/" + task.ID + "/events"
	eventually(t, "the token event in the holding pod's ring", func() bool {
		sub := subscribeSSE(t, held.url+events+"?from=1")
		ev := next(t, sub, "the token event on the holding pod")
		return ev.typ == "transcript"
	})
	sub := subscribeSSE(t, disp.url+events)
	if ev := next(t, sub, "the status event"); ev.typ != "status" || !strings.Contains(ev.data, "working") {
		t.Fatalf("first event through the dispatching pod: %+v", ev)
	}
	if ev := next(t, sub, "the token event"); ev.typ != "transcript" || !strings.Contains(ev.data, "thinking aloud") {
		t.Fatalf("second event through the dispatching pod: %+v", ev)
	}

	// Cancelled on the pod that does not dispatch: its notice reaches
	// the task's stream on the dispatching pod, and through the holding
	// pod the node.
	call(t, "POST", held.url+tasks.TasksPath+"/"+task.ID+"/cancel", "alice@example.com", map[string]string{"reason": "enough"}, &struct{}{})
	soon("the cancel reaching ws-1", func() bool { return n.saw(tasks.TypeCancel) })

	// The node's result, and the end, reach the forwarded subscriber.
	fmt.Fprintf(es, `{"v":1,"seq":3,"task":%q,"type":"result","result":{"exit":{"code":1,"reason":"cancelled"}}}`+"\n", task.ID)
	es.Close()
	if ev := next(t, sub, "the result"); ev.typ != "result" {
		t.Fatalf("third event through the dispatching pod: %+v", ev)
	}
	if ev := next(t, sub, "the end"); ev.typ != "end" {
		t.Fatalf("after the result through the dispatching pod: %+v", ev)
	}

	// The node goes: no pod holds it, and the dispatching pod serves the
	// stored events from the database, without the token event.
	conn.Close()
	soon("ws-1 offline on the dispatching pod", func() bool { return !online(t, disp.url, "ws-1") })
	sub = subscribeSSE(t, disp.url+events)
	var got []string
	for ev := range sub {
		got = append(got, ev.typ)
	}
	if want := []string{"status", "result", "end"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("from the database: %v, want %v", got, want)
	}
}
