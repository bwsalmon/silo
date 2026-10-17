package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/events"
	"github.com/bwsalmon/silo/hub/registry"
)

func TestPostMessages(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		task, err := s.Create(ctx, Spec{Team: "infra", Repo: "bwsalmon/silo", Kind: KindInteractive, Prompt: "let's talk", Submitter: "alice@example.com"})
		check(t, err)
		read, err := s.Get(ctx, task.ID)
		check(t, err)
		if read.Kind != KindInteractive {
			t.Errorf("kind read back %q", read.Kind)
		}
		if _, err := s.Create(ctx, Spec{Team: "infra", Kind: "chatty", Prompt: "x", Submitter: "alice@example.com"}); !errors.Is(err, ErrInvalid) {
			t.Errorf("an unknown kind: %v, want ErrInvalid", err)
		}

		m, dup, err := s.Post(ctx, TaskMessage{Task: task.ID, ID: "m-1", Text: "go on", Sender: "Alice@Example.com",
			Attachments: []events.Attachment{{Name: "spec.md", Ref: "https://example.com/spec.md"}}})
		check(t, err)
		if dup || m.Sender != "alice@example.com" || m.SentAt.IsZero() || m.Delivered {
			t.Fatalf("posted %+v dup %v", m, dup)
		}
		// Retried: the message kept, nothing new.
		again, dup, err := s.Post(ctx, TaskMessage{Task: task.ID, ID: "m-1", Text: "go on", Sender: "alice@example.com"})
		check(t, err)
		if !dup || !again.SentAt.Equal(m.SentAt) || len(again.Attachments) != 1 {
			t.Errorf("retried %+v dup %v", again, dup)
		}
		// The same ID with other words is not a retry.
		if _, _, err := s.Post(ctx, TaskMessage{Task: task.ID, ID: "m-1", Text: "stop", Sender: "alice@example.com"}); !errors.Is(err, ErrState) {
			t.Errorf("a different m-1: %v, want ErrState", err)
		}
		gen, _, err := s.Post(ctx, TaskMessage{Task: task.ID, Text: "and then", Sender: "dave@example.com"})
		check(t, err)
		if !strings.HasPrefix(gen.ID, "m-") || gen.ID == "m-1" {
			t.Errorf("hub's ID %q", gen.ID)
		}
		for _, bad := range []TaskMessage{
			{Task: task.ID, Text: "  ", Sender: "alice@example.com"},
			{Task: task.ID, Text: "x"},
			{Task: task.ID, Text: strings.Repeat("x", maxMessageText+1), Sender: "alice@example.com"},
			{Task: task.ID, ID: "a\nb", Text: "x", Sender: "alice@example.com"},
			{Task: task.ID, Text: "x", Sender: "alice@example.com", Attachments: []events.Attachment{{Name: "x", Ref: "http://example.com/x"}}},
			{Task: task.ID, Text: "x", Sender: "alice@example.com", Attachments: []events.Attachment{{Name: "x"}}},
		} {
			if _, _, err := s.Post(ctx, bad); !errors.Is(err, ErrInvalid) {
				t.Errorf("posting %+v: %v, want ErrInvalid", bad, err)
			}
		}

		check(t, s.MessageDelivered(ctx, task.ID, "m-1"))
		un, err := s.Messages(ctx, task.ID, true)
		check(t, err)
		if len(un) != 1 || un[0].ID != gen.ID {
			t.Errorf("undelivered %+v", un)
		}
		all, err := s.Messages(ctx, task.ID, false)
		check(t, err)
		if len(all) != 2 || all[0].ID != "m-1" || !all[0].Delivered || all[0].Attachments[0].Ref != "https://example.com/spec.md" {
			t.Errorf("all %+v", all)
		}
		e, ok, err := s.MessageEvent(ctx, task.ID, "m-1")
		check(t, err)
		if !ok || e.Sender != "alice@example.com" || e.Text != "go on" || len(e.Attachments) != 1 {
			t.Errorf("m-1's event %+v %v", e, ok)
		}
		if _, ok, err := s.MessageEvent(ctx, "t-other", "m-1"); ok || err != nil {
			t.Errorf("another task's m-1: %v %v", ok, err)
		}
	})
}

func TestAPIMessages(t *testing.T) {
	h := newAPIHub(t)
	ctx := context.Background()
	// dave, in infra, starts a session; it is filed interactive.
	task := h.submit(t, "dave@example.com", map[string]any{"repo": "bwsalmon/silo", "kind": "interactive", "prompt": "pair with me"})
	if task.Kind != KindInteractive || task.State != Queued {
		t.Fatalf("filed %+v", task)
	}
	if code, _ := h.do(t, "dave@example.com", "POST", TasksPath, map[string]any{"repo": "bwsalmon/silo", "kind": "chat", "prompt": "x"}); code != http.StatusBadRequest {
		t.Errorf("filing an unknown kind: %d, want 400", code)
	}
	path := TasksPath + "/" + task.ID + "/messages"
	hi := map[string]any{"id": "m-1", "text": "hello"}
	for _, c := range []struct {
		user string
		body any
		want int
	}{
		{"", hi, http.StatusUnauthorized},
		// Not in the team: carol cannot see it, the hub admin may see
		// it but not talk to it.
		{"carol@example.com", hi, http.StatusNotFound},
		{"bob@example.com", hi, http.StatusNotFound},
		{"root@example.com", hi, http.StatusForbidden},
		{"dave@example.com", map[string]any{"text": ""}, http.StatusBadRequest},
		{"dave@example.com", map[string]any{"text": "x", "from": "root"}, http.StatusBadRequest},
	} {
		if code, b := h.do(t, c.user, "POST", path, c.body); code != c.want {
			t.Errorf("%q posting %v: %d %s, want %d", c.user, c.body, code, b, c.want)
		}
	}
	// Queued, not yet running: kept for the node's accept.
	code, b := h.do(t, "dave@example.com", "POST", path, hi)
	var m TaskMessage
	if code != http.StatusCreated || json.Unmarshal(b, &m) != nil || m.Sender != "dave@example.com" || m.ID != "m-1" {
		t.Fatalf("dave posting: %d %s", code, b)
	}
	// A retry is the same message, not a second one.
	code, b = h.do(t, "dave@example.com", "POST", path, hi)
	var again TaskMessage
	if code != http.StatusOK || json.Unmarshal(b, &again) != nil || !again.SentAt.Equal(m.SentAt) {
		t.Errorf("dave retrying: %d %s", code, b)
	}
	// Another member of the team, in another role, may post too.
	if code, b := h.do(t, "alice@example.com", "POST", path, map[string]any{"text": "me too"}); code != http.StatusCreated {
		t.Errorf("alice posting: %d %s", code, b)
	}
	code, b = h.do(t, "alice@example.com", "GET", path, nil)
	var list struct{ Messages []TaskMessage }
	if code != http.StatusOK || json.Unmarshal(b, &list) != nil || len(list.Messages) != 2 || list.Messages[0].Sender != "dave@example.com" {
		t.Errorf("listing: %d %s", code, b)
	}

	// Its node away: still kept.
	_, err := h.store.Offer(ctx, task.ID, "ws-1", "alice@example.com")
	check(t, err)
	_, err = h.store.Accept(ctx, task.ID, "local-1")
	check(t, err)
	_, err = h.store.Lose(ctx, task.ID, "gone")
	check(t, err)
	if code, b := h.do(t, "dave@example.com", "POST", path, map[string]any{"text": "are you there?"}); code != http.StatusCreated {
		t.Errorf("posting to a lost task: %d %s", code, b)
	}
	// Over: refused.
	_, err = h.store.Cancel(ctx, task.ID, "closed")
	check(t, err)
	if code, b := h.do(t, "dave@example.com", "POST", path, map[string]any{"text": "one more"}); code != http.StatusConflict {
		t.Errorf("posting to a cancelled task: %d %s, want 409", code, b)
	}

	// The submitter outside the team may post to their proposal's
	// session once it runs; an ordinary task takes no messages.
	p := h.submit(t, "bob@example.com", map[string]any{"repo": "bwsalmon/silo", "kind": "interactive", "prompt": "help me"})
	ppath := TasksPath + "/" + p.ID + "/messages"
	if code, _ := h.do(t, "bob@example.com", "POST", ppath, hi); code != http.StatusConflict {
		t.Errorf("posting to a proposal: %d, want 409", code)
	}
	_, err = h.store.Approve(ctx, p.ID, "dave@example.com")
	check(t, err)
	if code, b := h.do(t, "bob@example.com", "POST", ppath, hi); code != http.StatusCreated {
		t.Errorf("bob posting to his approved session: %d %s", code, b)
	}
	plain := h.submit(t, "dave@example.com", map[string]any{"repo": "bwsalmon/silo", "prompt": "just do it"})
	if code, _ := h.do(t, "dave@example.com", "POST", TasksPath+"/"+plain.ID+"/messages", hi); code != http.StatusConflict {
		t.Errorf("posting to an ordinary task: %d, want 409", code)
	}
}

// TestInteractivePlacement checks the dispatcher places an interactive
// task only on a node that lists the feature, and an ordinary one on
// either.
func TestInteractivePlacement(t *testing.T) {
	h := newHarness(t)
	plain := h.node(t, "alice@example.com", silo("ws-plain", 2))
	session := h.submit(t, Spec{Team: "infra", Repo: "bwsalmon/silo", Kind: KindInteractive})
	plain.none(t, 300*time.Millisecond)
	if got, _ := h.store.Get(context.Background(), session.ID); got.State != Queued {
		t.Fatalf("with no interactive node the session is %s, want queued", got.State)
	}
	chat := silo("ws-chat", 2)
	chat.Features = []string{registry.FeatureInteractive}
	n := h.node(t, "alice@example.com", chat)
	s := n.next(t, TypeOffer)
	if s.first.Task.ID != session.ID || s.first.Task.Kind != KindInteractive {
		t.Fatalf("ws-chat was offered %+v", s.first.Task)
	}
	plain.none(t, 100*time.Millisecond)
}

// TestMessageDownTheStream posts messages to an interactive task and
// checks they reach its node over the rendezvous, on the task's stream:
// one posted before the node accepted the task at its accept, and one
// posted while it runs at once.
func TestMessageDownTheStream(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	chat := silo("ws-1", 2)
	chat.Features = []string{registry.FeatureInteractive}
	n := h.node(t, "alice@example.com", chat)

	task := h.submit(t, Spec{Team: "infra", Repo: "bwsalmon/silo", Kind: KindInteractive})
	s := n.next(t, TypeOffer)
	h.await(t, task.ID, "offered", inState(Offered))
	_, _, err := h.store.Post(ctx, TaskMessage{Task: task.ID, ID: "m-early", Text: "first", Sender: "alice@example.com"})
	check(t, err)
	h.d.Posted(ctx, task.ID)
	s.send(t, Message{Type: TypeAccept, NodeTask: "local-1"})
	read := func(want string) Message {
		t.Helper()
		m, err := s.Read()
		check(t, err)
		if m.Type != TypeMessage || m.Message == nil || m.Message.ID != want || m.Task == nil || m.Task.ID != task.ID {
			t.Fatalf("node read %+v, want message %s", m, want)
		}
		return m
	}
	if m := read("m-early"); m.Message.Text != "first" || m.Message.Sender != "alice@example.com" || m.Message.SentAt.IsZero() {
		t.Errorf("m-early arrived as %+v", m.Message)
	}
	h.await(t, task.ID, "running", inState(Running))
	_, _, err = h.store.Post(ctx, TaskMessage{Task: task.ID, ID: "m-2", Text: "second", Sender: "dave@example.com"})
	check(t, err)
	h.d.Posted(ctx, task.ID)
	if m := read("m-2"); m.Message.Sender != "dave@example.com" {
		t.Errorf("m-2 arrived as %+v", m.Message)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		ms, err := h.store.Messages(ctx, task.ID, true)
		check(t, err)
		if len(ms) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("still undelivered: %+v", ms)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The stream carries its result after.
	s.send(t, Message{Type: TypeResult, State: Done, Reason: "closed"})
	h.await(t, task.ID, "done", inState(Done))
}
