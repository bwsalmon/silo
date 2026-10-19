package tasks

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/events"
)

func TestSharedChatsEndToEnd(t *testing.T) {
	h := newAPIHub(t)
	ctx := context.Background()

	// Accept users in directory so they can sign in and/or be invited (stranger@example.com remains unaccepted).
	for _, u := range []string{"alice@example.com", "dave@example.com", "bob@example.com", "viewer@example.com", "outsider@example.com"} {
		check(t, h.dir.SetUser(ctx, u, false, "root@example.com"))
	}

	// Reject inviting an unaccepted user.
	code, _ := h.do(t, "alice@example.com", "POST", ChatsPath, map[string]any{
		"title":      "Payments Ops Room",
		"deployment": "infra",
		"participants": []map[string]any{
			{"email": "stranger@example.com", "role": "participant"},
		},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 when inviting unaccepted user, got %d", code)
	}

	// Non-member of deployment cannot create a chat session for it.
	code, _ = h.do(t, "bob@example.com", "POST", ChatsPath, map[string]any{
		"title":      "Payments Ops Room",
		"deployment": "infra",
	})
	if code != http.StatusForbidden {
		t.Fatalf("expected 403 when non-member creates chat, got %d", code)
	}

	// Create shared chat session with linked_deployments, participants, repos, references, external_bindings.
	code, b := h.do(t, "alice@example.com", "POST", ChatsPath, map[string]any{
		"title":              "Payments Ops Room",
		"deployment":         "infra",
		"linked_deployments": []string{"web"},
		"reply_mode":         ReplyModeMention,
		"repos":              []string{"bwsalmon/silo"},
		"references":         []string{"https://docs.example.com/payments-v2"},
		"participants": []map[string]any{
			{"email": "bob@example.com", "role": "participant"},
			{"email": "viewer@example.com", "role": "viewer"},
		},
		"external_bindings": []map[string]any{
			{
				"provider":    "google_chat",
				"external_id": "spaces/AAA123",
				"thread_id":   "threads/BBB456",
				"metadata":    map[string]string{"space_name": "Payments Incident"},
			},
		},
	})
	if code != http.StatusCreated {
		t.Fatalf("create chat failed: %d %s", code, b)
	}
	var created ChatSession
	check(t, json.Unmarshal(b, &created))
	if created.ID == "" || created.TaskID == "" || created.Title != "Payments Ops Room" || created.Deployment != "infra" {
		t.Fatalf("unexpected created chat: %+v", created)
	}
	if created.ReplyMode != ReplyModeMention {
		t.Fatalf("expected mention reply mode, got %q", created.ReplyMode)
	}
	if len(created.LinkedDeployments) != 1 || created.LinkedDeployments[0] != "web" {
		t.Fatalf("unexpected linked_deployments: %+v", created.LinkedDeployments)
	}
	if len(created.ExternalBindings) != 1 || created.ExternalBindings[0].Provider != "google_chat" || created.ExternalBindings[0].ExternalID != "spaces/AAA123" {
		t.Fatalf("unexpected external_bindings: %+v", created.ExternalBindings)
	}

	// Backing task is interactive and has context header in prompt.
	backingTask, err := h.store.Get(ctx, created.TaskID)
	check(t, err)
	if backingTask.Kind != KindInteractive || backingTask.Team != "infra" {
		t.Fatalf("unexpected backing task: %+v", backingTask)
	}
	if !strings.Contains(backingTask.Prompt, "Payments Ops Room") || !strings.Contains(backingTask.Prompt, "bwsalmon/silo") {
		t.Fatalf("backing task prompt missing room context: %q", backingTask.Prompt)
	}

	// Check participants list includes deployment members (alice, dave) and invited users (bob, viewer).
	code, b = h.do(t, "bob@example.com", "GET", ChatsPath+"/"+created.ID, nil)
	if code != http.StatusOK {
		t.Fatalf("invited participant GET chat failed: %d %s", code, b)
	}
	var detail ChatSession
	check(t, json.Unmarshal(b, &detail))
	foundEmails := map[string]ChatParticipant{}
	for _, p := range detail.Participants {
		foundEmails[p.Email] = p
	}
	for _, wantEmail := range []string{"alice@example.com", "dave@example.com", "bob@example.com", "viewer@example.com"} {
		if _, ok := foundEmails[wantEmail]; !ok {
			t.Fatalf("missing participant %s in %+v", wantEmail, detail.Participants)
		}
	}
	if foundEmails["alice@example.com"].Source != "deployment" || foundEmails["bob@example.com"].Source != "invited" {
		t.Fatalf("unexpected participant sources: %+v", foundEmails)
	}

	// Non-participant gets 404.
	code, _ = h.do(t, "outsider@example.com", "GET", ChatsPath+"/"+created.ID, nil)
	if code != http.StatusNotFound {
		t.Fatalf("expected 404 for outsider, got %d", code)
	}

	// Viewer can read, but cannot post messages or PATCH settings.
	code, _ = h.do(t, "viewer@example.com", "GET", ChatsPath+"/"+created.ID, nil)
	if code != http.StatusOK {
		t.Fatalf("expected 200 for viewer GET, got %d", code)
	}
	code, _ = h.do(t, "viewer@example.com", "POST", ChatsPath+"/"+created.ID+"/messages", map[string]any{"text": "hi"})
	if code != http.StatusForbidden {
		t.Fatalf("expected 403 for viewer POST message, got %d", code)
	}
	code, _ = h.do(t, "viewer@example.com", "PATCH", ChatsPath+"/"+created.ID, map[string]any{"title": "hacked"})
	if code != http.StatusForbidden {
		t.Fatalf("expected 403 for viewer PATCH, got %d", code)
	}

	// Connect SSE listener to /v1/chats/{id}/events.
	req, err := http.NewRequest("GET", h.url+ChatsPath+"/"+created.ID+"/events", nil)
	check(t, err)
	req.Header.Set("X-User", "dave@example.com")
	resp, err := http.DefaultClient.Do(req)
	check(t, err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events stream status %d", resp.StatusCode)
	}
	sseCh := make(chan ChatEvent, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "data: ") {
				var ev ChatEvent
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err == nil {
					sseCh <- ev
				}
			}
		}
	}()

	// In "mention" mode, human messages without @agent / @infra / trigger_agent do NOT wake the backing task.
	code, b = h.do(t, "alice@example.com", "POST", ChatsPath+"/"+created.ID+"/messages", map[string]any{
		"id":   "m-1",
		"text": "Hey @bob@example.com, what do you think of the retry backoff?",
	})
	if code != http.StatusCreated {
		t.Fatalf("alice post message: %d %s", code, b)
	}
	var postRes1 ChatMessage
	check(t, json.Unmarshal(b, &postRes1))
	if postRes1.TriggeredAgent {
		t.Fatalf("expected TriggeredAgent=false for human-to-human message in mention mode")
	}

	// Verify SSE delivered the message event.
	select {
	case ev := <-sseCh:
		if ev.Type != "message" || ev.Message == nil || ev.Message.Text != "Hey @bob@example.com, what do you think of the retry backoff?" {
			t.Fatalf("unexpected SSE event: %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for SSE message event")
	}

	// Backing task should have 0 messages posted to its task_messages queue so far.
	tMsgs, err := h.store.Messages(ctx, created.TaskID, true)
	check(t, err)
	if len(tMsgs) != 0 {
		t.Fatalf("expected 0 backing task messages before mention, got %d", len(tMsgs))
	}

	// Bob is @mentioned in Alice's message -> Bob's Unified Inbox (/v1/inbox) surfaces a chat_mention item!
	code, b = h.do(t, "bob@example.com", "GET", InboxPath, nil)
	if code != http.StatusOK {
		t.Fatalf("bob inbox failed: %d %s", code, b)
	}
	var bobInbox struct {
		Items []InboxItem `json:"items"`
	}
	check(t, json.Unmarshal(b, &bobInbox))
	foundMention := false
	for _, it := range bobInbox.Items {
		if it.Kind == InboxChatMention && it.ChatID == created.ID {
			foundMention = true
		}
	}
	if !foundMention {
		t.Fatalf("expected chat_mention in bob's inbox, got %+v", bobInbox.Items)
	}

	// Now Bob replies and mentions @infra -> wakes the backing agent and delivers BOTH Alice's and Bob's messages with [sender] attribution!
	code, b = h.do(t, "bob@example.com", "POST", ChatsPath+"/"+created.ID+"/messages", map[string]any{
		"id":   "m-2",
		"text": "I think 500ms jitter is good. @infra can you draft the retry policy and file a task for @web?",
	})
	if code != http.StatusCreated {
		t.Fatalf("bob post message: %d %s", code, b)
	}
	var postRes2 ChatMessage
	check(t, json.Unmarshal(b, &postRes2))
	if !postRes2.TriggeredAgent {
		t.Fatalf("expected TriggeredAgent=true when @infra is mentioned")
	}

	tMsgs, err = h.store.Messages(ctx, created.TaskID, true)
	check(t, err)
	if len(tMsgs) != 1 {
		t.Fatalf("expected 1 batched task message delivered to backing agent, got %d", len(tMsgs))
	}
	if !strings.Contains(tMsgs[0].Text, "[alice@example.com]: Hey @bob@example.com") ||
		!strings.Contains(tMsgs[0].Text, "[bob@example.com]: I think 500ms jitter is good.") {
		t.Fatalf("batched transcript slice missing sender attribution: %q", tMsgs[0].Text)
	}

	// Simulate the agent replying via TaskEvent (TypeTranscript).
	h.api.OnTaskEvent(created.TaskID, events.Event{
		Seq:  10,
		Time: time.Now().UTC(),
		Type: events.TypeTranscript,
		Transcript: &events.Item{
			Kind: events.ItemText,
			Text: "Here is the retry policy. Should I proceed?",
		},
	})
	select {
	case ev := <-sseCh:
		if ev.Type == "message" && ev.Message != nil && ev.Message.Role == "user" {
			ev = <-sseCh
		}
		if ev.Type != events.TypeTranscript || ev.Message == nil || ev.Message.Role != "agent" || ev.Message.Sender != "infra" {
			t.Fatalf("expected agent transcript SSE event, got %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for assistant SSE event")
	}

	// Simulate a task proposed from the shared chat's backing task targeting linked deployment "web".
	filedTask, err := h.store.Create(ctx, Spec{
		Team:       "web",
		Title:      "Update checkout client retry policy",
		Prompt:     "Use 500ms jitter backoff",
		Submitter:  "alice@example.com",
		OriginTask: created.TaskID,
	})
	check(t, err)
	h.api.OnTaskFiled(filedTask)

	// Verify SSE broadcasts task_proposed event.
	select {
	case ev := <-sseCh:
		if ev.Type != "task_proposed" || ev.ProposedTask == nil || ev.ProposedTask.ID != filedTask.ID {
			t.Fatalf("expected task_proposed SSE event, got %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for task_proposed SSE event")
	}

	// Switch reply_mode to "all" via PATCH /v1/chats/{id} and verify every message triggers the agent.
	code, b = h.do(t, "alice@example.com", "PATCH", ChatsPath+"/"+created.ID, map[string]any{
		"reply_mode": ReplyModeAll,
		"title":      "Payments Ops Room (Active)",
	})
	if code != http.StatusOK {
		t.Fatalf("PATCH chat failed: %d %s", code, b)
	}
	var updated ChatSession
	check(t, json.Unmarshal(b, &updated))
	if updated.ReplyMode != ReplyModeAll || updated.Title != "Payments Ops Room (Active)" {
		t.Fatalf("unexpected updated chat: %+v", updated)
	}
	// Drain chat_updated SSE event.
	select {
	case ev := <-sseCh:
		if ev.Type != "chat_updated" {
			t.Fatalf("expected chat_updated SSE event, got %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for chat_updated SSE event")
	}

	code, b = h.do(t, "dave@example.com", "POST", ChatsPath+"/"+created.ID+"/messages", map[string]any{
		"id":   "m-3",
		"text": "Yes, please proceed with the rollout.",
	})
	if code != http.StatusCreated {
		t.Fatalf("dave post message in all mode: %d %s", code, b)
	}
	var postRes3 ChatMessage
	check(t, json.Unmarshal(b, &postRes3))
	if !postRes3.TriggeredAgent {
		t.Fatalf("expected TriggeredAgent=true in 'all' reply mode")
	}
}
