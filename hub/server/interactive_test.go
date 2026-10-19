package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/events"
	"github.com/bwsalmon/silo/hub/live"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
	"github.com/bwsalmon/silo/hub/tasks"
)

// TestInteractiveSession is a session through a whole hub: a node that
// lists the interactive feature is offered a task filed as
// interactive, says on its event stream that it is waiting for a reply,
// is sent a member's message down the task's stream, reports taking it
// in, and goes on. A subscriber to the task's events sees the wait, the
// message as the hub has it (whatever the node claimed of it) and the
// resume, in order; and the session, once over, takes no more messages.
func TestInteractiveSession(t *testing.T) {
	const admin, alice, dave, carol = "admin@example.com", "alice@example.com", "dave@example.com", "carol@example.com"
	hub := start(t, Config{
		Database:      "sqlite:" + t.TempDir() + "/hub.db",
		DevUserHeader: userHeader,
		HubAdmins:     []string{admin},
		AcceptedUsers: []string{alice, dave, carol},
		Heartbeat:     time.Second,
		DispatchRetry: time.Second,
		Logf:          logf(t),
	}, "127.0.0.1:0")
	call(t, "POST", hub.url+"/v1/teams", alice, map[string]string{"id": "infra"}, &struct{}{})
	call(t, "PUT", hub.url+"/v1/teams/infra/members/user:"+dave, alice, map[string]string{"role": "member"}, &struct{}{})
	call(t, "PUT", hub.url+"/v1/teams/infra/repos/bwsalmon/silo", alice, nil, &struct{}{})
	var minted struct {
		Token string `json:"token"`
	}
	call(t, "POST", hub.url+"/v1/node-tokens", alice, map[string]string{"node": "ws-1"}, &minted)

	cl := &rendezvous.Client{
		Hub:   hub.url,
		Token: func(context.Context) (string, error) { return minted.Token, nil },
		Node:  registry.Node{ID: "ws-1", Repos: []string{"bwsalmon/silo"}, Slots: 1, Features: []string{registry.FeatureInteractive}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn, err := cl.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	var task tasks.Task
	call(t, "POST", hub.url+tasks.TasksPath, dave, map[string]string{"repo": "bwsalmon/silo", "kind": "interactive", "prompt": "pair with me"}, &task)
	if task.Kind != tasks.KindInteractive {
		t.Fatalf("filed %+v", task)
	}
	c, err := conn.Accept()
	if err != nil {
		t.Fatal(err)
	}
	st := tasks.NewStream(c)
	defer st.Close()
	offer, err := st.Read()
	if err != nil || offer.Type != tasks.TypeOffer || offer.Task.ID != task.ID || offer.Task.Kind != tasks.KindInteractive {
		t.Fatalf("node read %+v %v, want the session's offer", offer, err)
	}
	if err := st.Write(tasks.Message{Type: tasks.TypeAccept, NodeTask: "local-1"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the session running", func() bool { return taskState(t, hub.url, task.ID) == tasks.Running })

	// alice subscribes to the session's events.
	got := subscribe(t, ctx, hub.url, alice, task.ID)

	up, err := conn.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	emit := func(seq int64, typ, payload string) {
		t.Helper()
		fmt.Fprintf(up, `{"v":1,"seq":%d,"type":%q,"task":%q,%q:%s}`+"\n", seq, typ, task.ID, typ, payload)
	}
	emit(1, events.TypeAwaitingReply, `{"state":"waiting","question":"Which branch?"}`)
	if e := got.next(t); e.Type != events.TypeAwaitingReply || e.AwaitingReply.State != events.AwaitingWaiting || e.AwaitingReply.Question != "Which branch?" {
		t.Fatalf("subscriber saw %+v, want the wait", e)
	}

	path := hub.url + tasks.TasksPath + "/" + task.ID + "/messages"
	if code := call(t, "POST", path, carol, map[string]string{"text": "hi"}, nil); code != http.StatusNotFound {
		t.Errorf("carol, outside the team, posting: %d, want 404", code)
	}
	if code := call(t, "POST", path, admin, map[string]string{"text": "hi"}, nil); code != http.StatusForbidden {
		t.Errorf("a hub admin outside the team posting: %d, want 403", code)
	}
	var sent tasks.TaskMessage
	if code := call(t, "POST", path, alice, map[string]string{"id": "m-1", "text": "main, please"}, &sent); code != http.StatusCreated {
		t.Fatalf("alice posting: %d", code)
	}
	// Retried, as a client that lost the answer would: not sent again.
	var retried tasks.TaskMessage
	if code := call(t, "POST", path, alice, map[string]string{"id": "m-1", "text": "main, please"}, &retried); code != http.StatusOK {
		t.Errorf("alice retrying: %d, want 200", code)
	}

	m, err := st.Read()
	if err != nil || m.Type != tasks.TypeMessage || m.Message == nil || m.Message.ID != "m-1" || m.Message.Text != "main, please" || m.Message.Sender != alice {
		t.Fatalf("node read %+v %v, want m-1", m, err)
	}
	// The node reports taking it in, claiming another sender, which the
	// hub does not take; then it goes on.
	emit(2, events.TypeMessage, `{"id":"m-1","sender":"mallory@example.com","text":"forged"}`)
	emit(3, events.TypeMessage, `{"id":"m-never-sent","text":"forged"}`)
	emit(4, events.TypeAwaitingReply, `{"state":"resumed","message_id":"m-1"}`)
	e := got.next(t)
	if e.Type != events.TypeMessage || e.Message.ID != "m-1" || e.Message.Sender != alice || e.Message.Text != "main, please" || !e.Message.SentAt.Equal(sent.SentAt) {
		t.Fatalf("subscriber saw %+v, want m-1 as alice sent it", e.Message)
	}
	if e := got.next(t); e.Type != events.TypeAwaitingReply || e.AwaitingReply.State != events.AwaitingResumed || e.AwaitingReply.MessageID != "m-1" {
		t.Fatalf("subscriber saw %+v, want the resume", e)
	}

	// The node closes the session: it takes no more messages.
	if err := st.Write(tasks.Message{Type: tasks.TypeResult, State: tasks.Done, Reason: "closed"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the session done", func() bool { return taskState(t, hub.url, task.ID) == tasks.Done })
	if code := call(t, "POST", path, alice, map[string]string{"text": "one more thing"}, nil); code != http.StatusConflict {
		t.Errorf("posting to a closed session: %d, want 409", code)
	}
}

// subscription is a task's events as Server-Sent Events.
type subscription chan live.Event

func subscribe(t *testing.T, ctx context.Context, url, user, task string) subscription {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", url+tasks.TasksPath+"/"+task+"/events", nil)
	req.Header.Set(userHeader, user)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("subscribing: %s", resp.Status)
	}
	out := make(subscription, 64)
	go func() {
		defer resp.Body.Close()
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			data, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok {
				continue
			}
			var e live.Event
			if json.Unmarshal([]byte(data), &e) == nil && e.Seq > 0 {
				out <- e
			}
		}
	}()
	return out
}

func (s subscription) next(t *testing.T) live.Event {
	t.Helper()
	select {
	case e, ok := <-s:
		if !ok {
			t.Fatal("the subscription ended")
		}
		return e
	case <-time.After(10 * time.Second):
		t.Fatal("no event")
	}
	return live.Event{}
}
