package gchat_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/events"
	"github.com/bwsalmon/silo/hub/chats"
	"github.com/bwsalmon/silo/hub/directory"
	"github.com/bwsalmon/silo/hub/gchat"
	"github.com/bwsalmon/silo/hub/identity"
	"github.com/bwsalmon/silo/hub/live"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/server"
	"github.com/bwsalmon/silo/hub/tasks"
)

type testJWKSigner struct {
	kid    string
	rsaKey *rsa.PrivateKey
	ecKey  *ecdsa.PrivateKey
}

func newTestJWKSigner(t *testing.T) (*testJWKSigner, *httptest.Server) {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s := &testJWKSigner{
		kid:    "gchat-key-1",
		rsaKey: rsaKey,
		ecKey:  ecKey,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		eBytes := big.NewInt(int64(rsaKey.PublicKey.E)).Bytes()
		xBytes := make([]byte, 32)
		yBytes := make([]byte, 32)
		ecKey.PublicKey.X.FillBytes(xBytes)
		ecKey.PublicKey.Y.FillBytes(yBytes)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{
				{
					"kty": "RSA",
					"alg": "RS256",
					"use": "sig",
					"kid": s.kid,
					"n":   base64.RawURLEncoding.EncodeToString(rsaKey.PublicKey.N.Bytes()),
					"e":   base64.RawURLEncoding.EncodeToString(eBytes),
				},
				{
					"kty": "EC",
					"alg": "ES256",
					"crv": "P-256",
					"use": "sig",
					"kid": "gchat-ec-1",
					"x":   base64.RawURLEncoding.EncodeToString(xBytes),
					"y":   base64.RawURLEncoding.EncodeToString(yBytes),
				},
			},
		})
	}))
	t.Cleanup(srv.Close)
	return s, srv
}

func (s *testJWKSigner) signRS256(t *testing.T, aud, iss string, exp time.Time) string {
	t.Helper()
	hdr, _ := json.Marshal(map[string]string{
		"alg": "RS256",
		"kid": s.kid,
		"typ": "JWT",
	})
	claims, _ := json.Marshal(map[string]any{
		"iss": iss,
		"aud": aud,
		"iat": exp.Add(-15 * time.Minute).Unix(),
		"exp": exp.Unix(),
	})
	signingInput := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.rsaKey, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (s *testJWKSigner) signES256(t *testing.T, aud, iss string, exp time.Time) string {
	t.Helper()
	hdr, _ := json.Marshal(map[string]string{
		"alg": "ES256",
		"kid": "gchat-ec-1",
		"typ": "JWT",
	})
	claims, _ := json.Marshal(map[string]any{
		"iss": iss,
		"aud": aud,
		"iat": exp.Add(-15 * time.Minute).Unix(),
		"exp": exp.Unix(),
	})
	signingInput := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signingInput))
	r, sigS, err := ecdsa.Sign(rand.Reader, s.ecKey, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	rawSig := make([]byte, 64)
	r.FillBytes(rawSig[:32])
	sigS.FillBytes(rawSig[32:])
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(rawSig)
}

type gchatTestEnv struct {
	t      *testing.T
	hub    *server.Hub
	srv    *httptest.Server
	sender *gchat.MemorySender
	signer *testJWKSigner
	aud    string
}

func newTestEnv(t *testing.T) *gchatTestEnv {
	t.Helper()
	signer, jwkSrv := newTestJWKSigner(t)
	sender := gchat.NewMemorySender()
	aud := "https://hub.example.com/v1/integrations/google-chat/events"

	ctx := context.Background()
	h, err := server.New(ctx, server.Config{
		Database:           "sqlite::memory:",
		DevUserHeader:      "X-User",
		HubAdmins:          []string{"user:root@example.com"},
		AcceptedUsers:      []string{"alice@example.com", "dave@example.com", "bob@example.com", "outsider@example.com"},
		HubURL:             "https://hub.example.com",
		GoogleChatAudience: aud,
		GoogleChatCertsURL: jwkSrv.URL,
		GoogleChatKeys:     &identity.KeySet{URL: jwkSrv.URL},
		GoogleChatSender:   sender,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })

	must(t, h.Directory.CreateTeam(ctx, "infra", "Infra Team"))
	must(t, h.Directory.SetMember(ctx, "infra", "user:alice@example.com", directory.Admin))
	must(t, h.Directory.SetMember(ctx, "infra", "user:dave@example.com", directory.Member))
	must(t, h.Directory.AssignRepo(ctx, "infra", "bwsalmon/silo"))

	// Register an online node for the infra deployment so status shows "connected" and wakeups can be verified.
	_, err = h.Registry.Register(registry.Node{
		ID:       "infra",
		Repos:    []string{"bwsalmon/silo"},
		Features: []string{registry.FeatureInteractive},
		Slots:    4,
	}, "alice@example.com")
	must(t, err)

	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)

	return &gchatTestEnv{
		t:      t,
		hub:    h,
		srv:    srv,
		sender: sender,
		signer: signer,
		aud:    aud,
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (e *gchatTestEnv) postWebhook(token string, ev gchat.Event) (int, gchat.OutboundMessage, []byte) {
	e.t.Helper()
	body, err := json.Marshal(ev)
	must(e.t, err)
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+gchat.EventsPath, bytes.NewReader(body))
	must(e.t, err)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	must(e.t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	must(e.t, err)
	var out gchat.OutboundMessage
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, raw
}

func (e *gchatTestEnv) doAs(user, method, path string, reqBody any) (int, []byte) {
	e.t.Helper()
	var rd io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		must(e.t, err)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rd)
	must(e.t, err)
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != "" {
		req.Header.Set("X-User", user)
	}
	resp, err := http.DefaultClient.Do(req)
	must(e.t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	must(e.t, err)
	return resp.StatusCode, raw
}

func (e *gchatTestEnv) validToken() string {
	return e.signer.signRS256(e.t, e.aud, gchat.DefaultIssuer, time.Now().Add(10*time.Minute))
}

func cardContainsText(msg gchat.OutboundMessage, sub string) bool {
	if strings.Contains(msg.Text, sub) {
		return true
	}
	raw, _ := json.Marshal(msg.CardsV2)
	return strings.Contains(string(raw), sub)
}

func TestGoogleChatJWTAndAcceptedUserAuth(t *testing.T) {
	env := newTestEnv(t)

	// 1. Missing token -> 401
	code, _, _ := env.postWebhook("", gchat.Event{
		Type:  "ADDED_TO_SPACE",
		User:  gchat.User{Email: "alice@example.com"},
		Space: gchat.Space{Name: "spaces/AAA"},
	})
	if code != http.StatusUnauthorized {
		t.Fatalf("missing token: got %d, want 401", code)
	}

	// 2. Wrong audience -> 401
	wrongAudTok := env.signer.signRS256(t, "https://wrong.example.com", gchat.DefaultIssuer, time.Now().Add(10*time.Minute))
	code, _, _ = env.postWebhook(wrongAudTok, gchat.Event{
		Type:  "ADDED_TO_SPACE",
		User:  gchat.User{Email: "alice@example.com"},
		Space: gchat.Space{Name: "spaces/AAA"},
	})
	if code != http.StatusUnauthorized {
		t.Fatalf("wrong audience: got %d, want 401", code)
	}

	// 3. Expired token -> 401
	expiredTok := env.signer.signRS256(t, env.aud, gchat.DefaultIssuer, time.Now().Add(-10*time.Minute))
	code, _, _ = env.postWebhook(expiredTok, gchat.Event{
		Type:  "ADDED_TO_SPACE",
		User:  gchat.User{Email: "alice@example.com"},
		Space: gchat.Space{Name: "spaces/AAA"},
	})
	if code != http.StatusUnauthorized {
		t.Fatalf("expired token: got %d, want 401", code)
	}

	// 4. Valid ES256 and RS256 tokens for accepted user -> 200
	esTok := env.signer.signES256(t, env.aud, gchat.DefaultIssuer, time.Now().Add(10*time.Minute))
	code, card, _ := env.postWebhook(esTok, gchat.Event{
		Type:  "ADDED_TO_SPACE",
		User:  gchat.User{Email: "alice@example.com"},
		Space: gchat.Space{Name: "spaces/AAA", DisplayName: "Infra Ops"},
	})
	if code != http.StatusOK {
		t.Fatalf("ADDED_TO_SPACE with ES256 token: got %d, want 200", code)
	}
	if !cardContainsText(card, "/grain new") || !cardContainsText(card, "/grain link") {
		t.Fatalf("expected onboarding card with /grain commands, got %+v", card)
	}

	// 5. Unaccepted user (stranger@example.com not in accepted_users) -> 403 refusal card
	code, refusal, _ := env.postWebhook(env.validToken(), gchat.Event{
		Type:  "ADDED_TO_SPACE",
		User:  gchat.User{Email: "stranger@example.com"},
		Space: gchat.Space{Name: "spaces/AAA"},
	})
	if code != http.StatusForbidden {
		t.Fatalf("unaccepted user: got %d, want 403", code)
	}
	if !cardContainsText(refusal, gchat.AccessDeniedMessage) {
		t.Fatalf("expected refusal card containing %q, got %+v", gchat.AccessDeniedMessage, refusal)
	}
}

func TestGoogleChatSlashCommandsAndBindingsAPI(t *testing.T) {
	env := newTestEnv(t)
	tok := env.validToken()

	// 1. /grain new infra Incident Triage -> creates chat session and binds space/thread
	code, respCard, _ := env.postWebhook(tok, gchat.Event{
		Type:  "MESSAGE",
		User:  gchat.User{Email: "alice@example.com", DisplayName: "Alice"},
		Space: gchat.Space{Name: "spaces/INC1", DisplayName: "Incident Space"},
		Message: &gchat.Message{
			Name:   "spaces/INC1/messages/m1",
			Text:   "/grain new infra Incident Triage",
			Thread: &gchat.Thread{Name: "spaces/INC1/threads/t1"},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("/grain new failed: %d %+v", code, respCard)
	}
	if !cardContainsText(respCard, "Incident Triage") || !cardContainsText(respCard, "infra") {
		t.Fatalf("unexpected /grain new card: %+v", respCard)
	}

	// Verify chat was created and bound in store
	code, listRaw := env.doAs("alice@example.com", http.MethodGet, tasks.ChatsPath, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /v1/chats failed: %d %s", code, listRaw)
	}
	var listWrap struct {
		Chats []chats.ChatSession `json:"chats"`
	}
	must(t, json.Unmarshal(listRaw, &listWrap))
	chatList := listWrap.Chats
	if len(chatList) != 1 {
		t.Fatalf("expected 1 chat session, got %d", len(chatList))
	}
	created := chatList[0]
	if created.Title != "Incident Triage" || created.Deployment != "infra" {
		t.Fatalf("unexpected chat: %+v", created)
	}
	if len(created.ExternalBindings) != 1 ||
		created.ExternalBindings[0].Provider != chats.ProviderGoogleChat ||
		created.ExternalBindings[0].ExternalID != "spaces/INC1" ||
		created.ExternalBindings[0].ThreadID != "spaces/INC1/threads/t1" {
		t.Fatalf("unexpected external_bindings: %+v", created.ExternalBindings)
	}

	// 2. /grain status -> returns bound chat link, deployment status ("connected"), and reply mode ("mention")
	code, statusCard, _ := env.postWebhook(tok, gchat.Event{
		Type:  "MESSAGE",
		User:  gchat.User{Email: "dave@example.com"},
		Space: gchat.Space{Name: "spaces/INC1"},
		Message: &gchat.Message{
			Text:         "/grain status",
			SlashCommand: &gchat.SlashCommand{CommandName: "/grain"},
			ArgumentText: "status",
			Thread:       &gchat.Thread{Name: "spaces/INC1/threads/t1"},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("/grain status failed: %d", code)
	}
	if !cardContainsText(statusCard, "/chats/"+created.ID) ||
		!cardContainsText(statusCard, "online") ||
		!cardContainsText(statusCard, "mention") {
		t.Fatalf("/grain status card missing expected fields: %+v", statusCard)
	}

	// 3. /grain mode all -> updates reply_mode to "all"
	code, modeCard, _ := env.postWebhook(tok, gchat.Event{
		Type:  "MESSAGE",
		User:  gchat.User{Email: "dave@example.com"},
		Space: gchat.Space{Name: "spaces/INC1"},
		Message: &gchat.Message{
			Text:   "/grain mode all",
			Thread: &gchat.Thread{Name: "spaces/INC1/threads/t1"},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("/grain mode all failed: %d", code)
	}
	if !cardContainsText(modeCard, "all") {
		t.Fatalf("expected mode card to confirm all, got %+v", modeCard)
	}

	// Switch back to mention mode via /grain mode mention
	code, modeCard, _ = env.postWebhook(tok, gchat.Event{
		Type:  "MESSAGE",
		User:  gchat.User{Email: "dave@example.com"},
		Space: gchat.Space{Name: "spaces/INC1"},
		Message: &gchat.Message{
			Text:   "/grain mode @mention",
			Thread: &gchat.Thread{Name: "spaces/INC1/threads/t1"},
		},
	})
	if code != http.StatusOK || !cardContainsText(modeCard, "mention") {
		t.Fatalf("/grain mode mention failed: %d %+v", code, modeCard)
	}

	// 4. /grain link <chat-id> from another space:
	// First verify outsider (accepted user, but not in infra team) gets 403 refusal card!
	code, refusal, _ := env.postWebhook(tok, gchat.Event{
		Type:  "MESSAGE",
		User:  gchat.User{Email: "outsider@example.com"},
		Space: gchat.Space{Name: "spaces/OTHER"},
		Message: &gchat.Message{
			Text: "/grain link " + created.ID,
		},
	})
	if code != http.StatusForbidden || !cardContainsText(refusal, gchat.AccessDeniedMessage) {
		t.Fatalf("expected 403 refusal when outsider links chat, got %d %+v", code, refusal)
	}

	// Authorized team member links spaces/ROOM2 to created.ID
	code, linkCard, _ := env.postWebhook(tok, gchat.Event{
		Type:  "MESSAGE",
		User:  gchat.User{Email: "alice@example.com"},
		Space: gchat.Space{Name: "spaces/ROOM2", DisplayName: "Room 2"},
		Message: &gchat.Message{
			Text: "/grain link " + created.ID,
		},
	})
	if code != http.StatusOK || !cardContainsText(linkCard, created.ID) {
		t.Fatalf("/grain link failed: %d %+v", code, linkCard)
	}

	// 5. Verify PUT /v1/chats/{id}/bindings/google-chat and DELETE /v1/chats/{id}/bindings/google-chat
	code, putRaw := env.doAs("alice@example.com", http.MethodPut, tasks.ChatsPath+"/"+created.ID+"/bindings/google-chat", map[string]string{
		"space_name":  "spaces/API_SPACE",
		"thread_name": "spaces/API_SPACE/threads/th9",
	})
	if code != http.StatusOK {
		t.Fatalf("PUT bindings/google-chat failed: %d %s", code, putRaw)
	}
	var updated chats.ChatSession
	must(t, json.Unmarshal(putRaw, &updated))
	if len(updated.ExternalBindings) != 1 ||
		updated.ExternalBindings[0].ExternalID != "spaces/API_SPACE" ||
		updated.ExternalBindings[0].ThreadID != "spaces/API_SPACE/threads/th9" {
		t.Fatalf("unexpected bindings after PUT: %+v", updated.ExternalBindings)
	}

	code, delRaw := env.doAs("alice@example.com", http.MethodDelete, tasks.ChatsPath+"/"+created.ID+"/bindings/google-chat", nil)
	if code != http.StatusOK {
		t.Fatalf("DELETE bindings/google-chat failed: %d %s", code, delRaw)
	}
	must(t, json.Unmarshal(delRaw, &updated))
	if len(updated.ExternalBindings) != 0 {
		t.Fatalf("expected 0 bindings after DELETE, got %+v", updated.ExternalBindings)
	}
}

func TestGoogleChatTurnTakingWebMirroringAndApprovals(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	tok := env.validToken()

	// Create a shared chat session bound to spaces/OPS1 / thread spaces/OPS1/threads/main
	code, createRaw := env.doAs("alice@example.com", http.MethodPost, tasks.ChatsPath, map[string]any{
		"title":      "Ops War Room",
		"deployment": "infra",
		"reply_mode": "mention",
	})
	if code != http.StatusCreated {
		t.Fatalf("create chat failed: %d %s", code, createRaw)
	}
	var chat chats.ChatSession
	must(t, json.Unmarshal(createRaw, &chat))

	code, _ = env.doAs("alice@example.com", http.MethodPut, tasks.ChatsPath+"/"+chat.ID+"/bindings/google-chat", map[string]string{
		"space_name":  "spaces/OPS1",
		"thread_name": "spaces/OPS1/threads/main",
	})
	if code != http.StatusOK {
		t.Fatalf("bind google-chat failed: %d", code)
	}

	// Open an SSE stream on GET /v1/chats/{id}/events to verify live broadcast to Web UI viewers
	sseCtx, cancelSSE := context.WithCancel(ctx)
	defer cancelSSE()
	sseReq, err := http.NewRequestWithContext(sseCtx, http.MethodGet, env.srv.URL+tasks.ChatsPath+"/"+chat.ID+"/events", nil)
	must(t, err)
	sseReq.Header.Set("X-User", "alice@example.com")
	sseResp, err := http.DefaultClient.Do(sseReq)
	must(t, err)
	defer sseResp.Body.Close()
	if sseResp.StatusCode != http.StatusOK {
		t.Fatalf("GET chat events SSE failed: %d", sseResp.StatusCode)
	}
	sseLines := make(chan string, 32)
	go func() {
		sc := bufio.NewScanner(sseResp.Body)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "data: ") {
				sseLines <- strings.TrimPrefix(line, "data: ")
			}
		}
	}()

	// 1. Unauthorized user in bound room -> 403 refusal card
	code, refusal, _ := env.postWebhook(tok, gchat.Event{
		Type:  "MESSAGE",
		User:  gchat.User{Email: "outsider@example.com"},
		Space: gchat.Space{Name: "spaces/OPS1"},
		Message: &gchat.Message{
			Text:   "hello from outsider",
			Thread: &gchat.Thread{Name: "spaces/OPS1/threads/main"},
		},
	})
	if code != http.StatusForbidden || !cardContainsText(refusal, gchat.AccessDeniedMessage) {
		t.Fatalf("expected 403 refusal for outsider in bound room, got %d %+v", code, refusal)
	}

	// 2. Authorized user sends message WITHOUT @mention while reply_mode == "mention"
	// -> Appended to chat and broadcast via SSE, but does NOT wake agent (0 messages in task_messages queue).
	code, _, _ = env.postWebhook(tok, gchat.Event{
		Type:  "MESSAGE",
		User:  gchat.User{Email: "dave@example.com", DisplayName: "Dave"},
		Space: gchat.Space{Name: "spaces/OPS1"},
		Message: &gchat.Message{
			Name:   "spaces/OPS1/messages/msg-1",
			Text:   "heads up, latency is spiking",
			Thread: &gchat.Thread{Name: "spaces/OPS1/threads/main"},
			Attachment: []gchat.Attachment{
				{
					ContentName: "latency.png",
					ContentType: "image/png",
					DownloadURI: "https://chat.googleapis.com/download/latency.png",
				},
			},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("MESSAGE without mention failed: %d", code)
	}

	select {
	case data := <-sseLines:
		if !strings.Contains(data, "heads up, latency is spiking") || !strings.Contains(data, "dave@example.com") {
			t.Fatalf("unexpected SSE payload for non-mention message: %s", data)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for SSE broadcast of Google Chat message")
	}

	tMsgs, err := env.hub.Backlog.Messages(ctx, chat.TaskID, true)
	must(t, err)
	if len(tMsgs) != 0 {
		t.Fatalf("expected 0 backing task messages before @mention in mention mode, got %d", len(tMsgs))
	}

	// 3. Authorized user sends message WITH @Grain bot mention
	// -> Leading @Grain mention is stripped, message + attachments stored, AND agent wakes (flushing unread messages to task_messages).
	code, _, _ = env.postWebhook(tok, gchat.Event{
		Type:  "MESSAGE",
		User:  gchat.User{Email: "dave@example.com", DisplayName: "Dave"},
		Space: gchat.Space{Name: "spaces/OPS1"},
		Message: &gchat.Message{
			Name:   "spaces/OPS1/messages/msg-2",
			Text:   "@Grain please check the database connection pool",
			Thread: &gchat.Thread{Name: "spaces/OPS1/threads/main"},
			Annotations: []gchat.Annotation{
				{
					Type: "USER_MENTION",
					UserMention: &gchat.UserMentionMetadata{
						User: gchat.User{DisplayName: "Grain", Type: "BOT"},
					},
				},
			},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("MESSAGE with @Grain mention failed: %d", code)
	}

	tMsgs, err = env.hub.Backlog.Messages(ctx, chat.TaskID, true)
	must(t, err)
	if len(tMsgs) != 1 {
		t.Fatalf("expected 1 batched task message delivered to backing agent after @Grain mention, got %d", len(tMsgs))
	}
	if !strings.Contains(tMsgs[0].Text, "heads up, latency is spiking") ||
		!strings.Contains(tMsgs[0].Text, "please check the database connection pool") {
		t.Fatalf("unexpected flushed task message text: %q", tMsgs[0].Text)
	}

	chatMsgs, err := env.hub.Backlog.ChatMessages(ctx, chat.ID, 0)
	must(t, err)
	if len(chatMsgs) != 2 {
		t.Fatalf("expected 2 chat messages, got %d", len(chatMsgs))
	}
	if len(chatMsgs[0].Attachments) != 1 || chatMsgs[0].Attachments[0].Name != "latency.png" {
		t.Fatalf("expected attachment on first chat message, got %+v", chatMsgs[0].Attachments)
	}
	if chatMsgs[1].Text != "please check the database connection pool" || chatMsgs[1].Sender != "dave@example.com" || !chatMsgs[1].TriggeredAgent {
		t.Fatalf("expected stripped text, sender dave@example.com, and TriggeredAgent=true, got %+v", chatMsgs[1])
	}

	// 4. Switch to reply_mode = "all" and verify a message without @mention wakes the agent immediately
	code, _, _ = env.postWebhook(tok, gchat.Event{
		Type:  "MESSAGE",
		User:  gchat.User{Email: "alice@example.com"},
		Space: gchat.Space{Name: "spaces/OPS1"},
		Message: &gchat.Message{
			Text:   "/grain mode all",
			Thread: &gchat.Thread{Name: "spaces/OPS1/threads/main"},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("/grain mode all failed: %d", code)
	}
	code, _, _ = env.postWebhook(tok, gchat.Event{
		Type:  "MESSAGE",
		User:  gchat.User{Email: "alice@example.com"},
		Space: gchat.Space{Name: "spaces/OPS1"},
		Message: &gchat.Message{
			Text:   "now check redis too",
			Thread: &gchat.Thread{Name: "spaces/OPS1/threads/main"},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("MESSAGE in all mode failed: %d", code)
	}
	tMsgs, err = env.hub.Backlog.Messages(ctx, chat.TaskID, true)
	must(t, err)
	if len(tMsgs) != 2 {
		t.Fatalf("expected 2 task messages after all-mode message without @mention, got %d", len(tMsgs))
	}

	// 5. Outbound mirroring: Human posts a message in Hub Web UI (POST /v1/chats/{id}/messages)
	// -> Mirrored to bound Google Chat space/thread attributed to [alice@example.com]
	env.sender.Reset()
	code, _ = env.doAs("alice@example.com", http.MethodPost, tasks.ChatsPath+"/"+chat.ID+"/messages", map[string]any{
		"text": "I am looking at the dashboard from the Hub UI",
	})
	if code != http.StatusCreated {
		t.Fatalf("POST /v1/chats/{id}/messages failed: %d", code)
	}
	sent := env.sender.Messages()
	if len(sent) != 1 {
		t.Fatalf("expected 1 outbound Google Chat message from Web UI post, got %d", len(sent))
	}
	if sent[0].SpaceName != "spaces/OPS1" || sent[0].ThreadName != "spaces/OPS1/threads/main" {
		t.Fatalf("unexpected space/thread on mirrored Web UI message: %+v", sent[0])
	}
	if !strings.Contains(sent[0].Text, "[alice@example.com]") || !strings.Contains(sent[0].Text, "I am looking at the dashboard from the Hub UI") {
		t.Fatalf("unexpected mirrored Web UI message text: %q", sent[0].Text)
	}

	// 6. Outbound agent replies, clarifying questions, approval_request cards, and proposed task cards:
	// Transition the backing task to Running so in-run approvals can be resolved.
	_, err = env.hub.Backlog.Offer(ctx, chat.TaskID, "infra", "alice@example.com")
	must(t, err)
	_, err = env.hub.Backlog.Accept(ctx, chat.TaskID, "node-task-1")
	must(t, err)

	env.sender.Reset()

	// 6a. Agent posts transcript reply
	env.hub.Live.OnEvent(live.Event{
		Task: chat.TaskID,
		Event: events.Event{
			Seq:  1,
			Time: time.Now().UTC(),
			Type: events.TypeTranscript,
			Transcript: &events.Item{
				Kind: events.ItemText,
				Text: "Connection pool saturation is at 95%.",
			},
		},
	})
	// 6b. Agent asks a clarifying question (awaiting_reply)
	env.hub.Live.OnEvent(live.Event{
		Task: chat.TaskID,
		Event: events.Event{
			Seq:  2,
			Time: time.Now().UTC(),
			Type: events.TypeAwaitingReply,
			AwaitingReply: &events.AwaitingReply{
				State:    events.AwaitingWaiting,
				Question: "Should I drain replica-2 first?",
			},
		},
	})
	// 6c. Agent requests an in-run approval (approval_request)
	reqEv := events.Event{
		Seq:  3,
		Time: time.Now().UTC(),
		Type: events.TypeApprovalRequest,
		ApprovalRequest: &events.ApprovalRequest{
			ID:      "req-drain-1",
			Tool:    "kubectl",
			Message: "drain replica-2",
		},
	}
	must(t, env.hub.Events.Append(ctx, live.Event{Task: chat.TaskID, Event: reqEv}, live.Lifecycle))
	env.hub.Live.OnEvent(live.Event{Task: chat.TaskID, Event: reqEv})

	// 6d. Agent proposes a follow-up task in the chat session
	propTask, err := env.hub.Backlog.Propose(ctx, tasks.Spec{
		Repo:       "bwsalmon/silo",
		Team:       "infra",
		Title:      "Bump max_connections to 500",
		Prompt:     "Increase max_connections in postgres config",
		Submitter:  "alice@example.com",
		OriginTask: chat.TaskID,
	})
	must(t, err)
	env.hub.GoogleChat.OnChatTaskEvent(chat, tasks.ChatEvent{
		Type:         "task_proposed",
		Time:         propTask.Created,
		ChatID:       chat.ID,
		TaskID:       chat.TaskID,
		ProposedTask: &propTask,
	})

	sent = env.sender.Messages()
	if len(sent) != 4 {
		t.Fatalf("expected 4 outbound messages (transcript, awaiting_reply, approval_request, task_proposed), got %d", len(sent))
	}
	if !strings.Contains(sent[0].Text, "Connection pool saturation is at 95%.") {
		t.Fatalf("unexpected transcript outbound message: %+v", sent[0])
	}
	if !cardContainsText(sent[1], "Should I drain replica-2 first?") {
		t.Fatalf("unexpected awaiting_reply outbound message: %+v", sent[1])
	}
	if len(sent[2].CardsV2) == 0 || !cardContainsText(sent[2], "req-drain-1") || !cardContainsText(sent[2], "Open in Grain Hub") {
		t.Fatalf("expected interactive approval_request card, got %+v", sent[2])
	}
	if len(sent[3].CardsV2) == 0 || !cardContainsText(sent[3], propTask.ID) || !cardContainsText(sent[3], "Bump max_connections to 500") {
		t.Fatalf("expected interactive task_proposed card, got %+v", sent[3])
	}

	// 7. Interactive CARD_CLICKED in Google Chat:
	// 7a. Click "Allow" on the in-run approval_request card as dave@example.com
	approvalMsgName := sent[2].Name
	code, clickedCard, _ := env.postWebhook(tok, gchat.Event{
		Type:  "CARD_CLICKED",
		User:  gchat.User{Email: "dave@example.com", DisplayName: "Dave"},
		Space: gchat.Space{Name: "spaces/OPS1"},
		Message: &gchat.Message{
			Name:   approvalMsgName,
			Thread: &gchat.Thread{Name: "spaces/OPS1/threads/main"},
		},
		Action: &gchat.FormAction{
			ActionMethodName: "resolve_approval",
			Parameters: []gchat.ActionParameter{
				{Key: "chat_id", Value: chat.ID},
				{Key: "task_id", Value: chat.TaskID},
				{Key: "request_id", Value: "req-drain-1"},
				{Key: "decision", Value: "allow"},
			},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("CARD_CLICKED resolve_approval failed: %d %+v", code, clickedCard)
	}
	if clickedCard.ActionResponse == nil || clickedCard.ActionResponse.Type != "UPDATE_MESSAGE" {
		t.Fatalf("expected UPDATE_MESSAGE actionResponse on CARD_CLICKED, got %+v", clickedCard.ActionResponse)
	}
	if !cardContainsText(clickedCard, "Allowed") || !cardContainsText(clickedCard, "dave@example.com") {
		t.Fatalf("expected updated card to record Allowed by dave@example.com, got %+v", clickedCard)
	}
	// Verify approval was recorded in Hub store with AnsweredBy == dave@example.com
	code, appRaw := env.doAs("alice@example.com", http.MethodGet, "/v1/tasks/"+chat.TaskID+"/approvals", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /v1/tasks/%s/approvals failed: %d %s", chat.TaskID, code, appRaw)
	}
	var appWrap struct {
		Approvals []tasks.Answer `json:"approvals"`
	}
	must(t, json.Unmarshal(appRaw, &appWrap))
	approvals := appWrap.Approvals
	if len(approvals) != 1 || approvals[0].Request != "req-drain-1" || approvals[0].Decision != "allow" || approvals[0].AnsweredBy != "dave@example.com" {
		t.Fatalf("unexpected stored approval after CARD_CLICKED: %+v", approvals)
	}
	// Verify Sender.UpdateMessage was also invoked for the card message
	if updatedMsg, ok := env.sender.Message(approvalMsgName); !ok || !cardContainsText(updatedMsg, "Allowed") {
		t.Fatalf("expected Sender.UpdateMessage to update %s in-place, got %+v", approvalMsgName, updatedMsg)
	}

	// 7b. Click "Approve" on the proposed task card as dave@example.com
	proposalMsgName := sent[3].Name
	code, propClickedCard, _ := env.postWebhook(tok, gchat.Event{
		Type:  "CARD_CLICKED",
		User:  gchat.User{Email: "dave@example.com", DisplayName: "Dave"},
		Space: gchat.Space{Name: "spaces/OPS1"},
		Message: &gchat.Message{
			Name:   proposalMsgName,
			Thread: &gchat.Thread{Name: "spaces/OPS1/threads/main"},
		},
		Action: &gchat.FormAction{
			ActionMethodName: "resolve_proposal",
			Parameters: []gchat.ActionParameter{
				{Key: "chat_id", Value: chat.ID},
				{Key: "task_id", Value: propTask.ID},
				{Key: "decision", Value: "approve"},
			},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("CARD_CLICKED resolve_proposal approve failed: %d %+v", code, propClickedCard)
	}
	if propClickedCard.ActionResponse == nil || propClickedCard.ActionResponse.Type != "UPDATE_MESSAGE" {
		t.Fatalf("expected UPDATE_MESSAGE actionResponse on proposal CARD_CLICKED, got %+v", propClickedCard.ActionResponse)
	}
	if !cardContainsText(propClickedCard, "Approved") || !cardContainsText(propClickedCard, "dave@example.com") {
		t.Fatalf("expected updated proposal card to show Approved by dave@example.com, got %+v", propClickedCard)
	}
	approvedTask, err := env.hub.Backlog.Get(ctx, propTask.ID)
	must(t, err)
	if approvedTask.State != tasks.Queued || approvedTask.ApprovedBy != "dave@example.com" {
		t.Fatalf("expected proposed task to be queued and ApprovedBy dave@example.com, got %+v", approvedTask)
	}

	// 7c. Also test Reject on a second proposed task via CARD_CLICKED
	propTask2, err := env.hub.Backlog.Propose(ctx, tasks.Spec{
		Repo:       "bwsalmon/silo",
		Team:       "infra",
		Title:      "Drop old table",
		Prompt:     "Drop legacy table",
		Submitter:  "alice@example.com",
		OriginTask: chat.TaskID,
	})
	must(t, err)
	code, rejClickedCard, _ := env.postWebhook(tok, gchat.Event{
		Type:  "CARD_CLICKED",
		User:  gchat.User{Email: "alice@example.com", DisplayName: "Alice"},
		Space: gchat.Space{Name: "spaces/OPS1"},
		Message: &gchat.Message{
			Name:   "spaces/OPS1/messages/prop-2",
			Thread: &gchat.Thread{Name: "spaces/OPS1/threads/main"},
		},
		Action: &gchat.FormAction{
			ActionMethodName: "resolve_proposal",
			Parameters: []gchat.ActionParameter{
				{Key: "chat_id", Value: chat.ID},
				{Key: "task_id", Value: propTask2.ID},
				{Key: "decision", Value: "reject"},
			},
		},
	})
	if code != http.StatusOK || !cardContainsText(rejClickedCard, "Rejected") {
		t.Fatalf("CARD_CLICKED resolve_proposal reject failed: %d %+v", code, rejClickedCard)
	}
	rejectedTask, err := env.hub.Backlog.Get(ctx, propTask2.ID)
	must(t, err)
	if rejectedTask.State != tasks.Rejected {
		t.Fatalf("expected rejected task to be rejected, got %s", rejectedTask.State)
	}

	// 7d. Also test Deny on a second in-run approval_request card via CARD_CLICKED
	reqEv2 := events.Event{
		Seq:  4,
		Time: time.Now().UTC(),
		Type: events.TypeApprovalRequest,
		ApprovalRequest: &events.ApprovalRequest{
			ID:      "req-drop-2",
			Tool:    "psql",
			Message: "DROP DATABASE prod",
		},
	}
	must(t, env.hub.Events.Append(ctx, live.Event{Task: chat.TaskID, Event: reqEv2}, live.Lifecycle))
	code, denyCard, _ := env.postWebhook(tok, gchat.Event{
		Type:  "CARD_CLICKED",
		User:  gchat.User{Email: "alice@example.com", DisplayName: "Alice"},
		Space: gchat.Space{Name: "spaces/OPS1"},
		Message: &gchat.Message{
			Name:   "spaces/OPS1/messages/app-2",
			Thread: &gchat.Thread{Name: "spaces/OPS1/threads/main"},
		},
		Action: &gchat.FormAction{
			ActionMethodName: "resolve_approval",
			Parameters: []gchat.ActionParameter{
				{Key: "chat_id", Value: chat.ID},
				{Key: "task_id", Value: chat.TaskID},
				{Key: "request_id", Value: "req-drop-2"},
				{Key: "decision", Value: "deny"},
			},
		},
	})
	if code != http.StatusOK || !cardContainsText(denyCard, "Denied") || !cardContainsText(denyCard, "alice@example.com") {
		t.Fatalf("CARD_CLICKED resolve_approval deny failed: %d %+v", code, denyCard)
	}
}

func TestHTTPSenderOAuth2AndREST(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	must(t, err)
	pkcs8Bytes, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	must(t, err)
	pemBlock := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8Bytes})

	var gotTokenReq bool
	var gotPostPath, gotPostQuery, gotPostAuth string
	var gotPatchPath, gotPatchQuery, gotPatchAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/token":
			gotTokenReq = true
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "ya29.mock-gchat-bot-token",
				"expires_in":   3600,
			})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/spaces/"):
			gotPostPath = r.URL.Path
			gotPostQuery = r.URL.RawQuery
			gotPostAuth = r.Header.Get("Authorization")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "spaces/ABC/messages/msg-99",
			})
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/spaces/"):
			gotPatchPath = r.URL.Path
			gotPatchQuery = r.URL.RawQuery
			gotPatchAuth = r.Header.Get("Authorization")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "spaces/ABC/messages/msg-99",
			})
		default:
			http.Error(w, "unexpected route "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	saJSON, err := json.Marshal(map[string]string{
		"type":           "service_account",
		"client_email":   "grain-bot@my-project.iam.gserviceaccount.com",
		"private_key":    string(pemBlock),
		"private_key_id": "key-1",
		"token_uri":      srv.URL + "/token",
	})
	must(t, err)

	sender, err := gchat.NewHTTPSender(string(saJSON))
	must(t, err)
	sender.BaseURL = srv.URL

	msgName, err := sender.SendMessage(context.Background(), "spaces/ABC", "spaces/ABC/threads/t1", gchat.OutboundMessage{
		Text: "Hello from Grain Hub",
	})
	must(t, err)
	if !gotTokenReq {
		t.Fatal("expected OAuth2 token exchange request")
	}
	if msgName != "spaces/ABC/messages/msg-99" {
		t.Fatalf("SendMessage name = %q, want spaces/ABC/messages/msg-99", msgName)
	}
	if gotPostPath != "/v1/spaces/ABC/messages" || !strings.Contains(gotPostQuery, "messageReplyOption=REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD") {
		t.Fatalf("unexpected POST path/query: %s ? %s", gotPostPath, gotPostQuery)
	}
	if gotPostAuth != "Bearer ya29.mock-gchat-bot-token" {
		t.Fatalf("unexpected POST Authorization: %q", gotPostAuth)
	}

	err = sender.UpdateMessage(context.Background(), msgName, gchat.OutboundMessage{
		Text: "Updated message",
	})
	must(t, err)
	if gotPatchPath != "/v1/spaces/ABC/messages/msg-99" || gotPatchQuery != "updateMask=text,cardsV2" {
		t.Fatalf("unexpected PATCH path/query: %s ? %s", gotPatchPath, gotPatchQuery)
	}
	if gotPatchAuth != "Bearer ya29.mock-gchat-bot-token" {
		t.Fatalf("unexpected PATCH Authorization: %q", gotPatchAuth)
	}
}

func TestGoogleChatBotAndOutboundEchoLoopPrevention(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	tok := env.validToken()

	code, createRaw := env.doAs("alice@example.com", http.MethodPost, tasks.ChatsPath, map[string]any{
		"title":      "Loop Prevention Room",
		"deployment": "infra",
		"reply_mode": "all",
	})
	if code != http.StatusCreated {
		t.Fatalf("create chat failed: %d %s", code, createRaw)
	}
	var chat chats.ChatSession
	must(t, json.Unmarshal(createRaw, &chat))

	code, _ = env.doAs("alice@example.com", http.MethodPut, tasks.ChatsPath+"/"+chat.ID+"/bindings/google-chat", map[string]string{
		"space_name":  "spaces/LOOP1",
		"thread_name": "spaces/LOOP1/threads/t1",
	})
	if code != http.StatusOK {
		t.Fatalf("bind google-chat failed: %d", code)
	}

	// 1. Inbound event from a BOT user must be ignored (200 OK, 0 chat messages).
	code, _, _ = env.postWebhook(tok, gchat.Event{
		Type:  "MESSAGE",
		User:  gchat.User{Email: "alice@example.com", Type: "BOT"},
		Space: gchat.Space{Name: "spaces/LOOP1"},
		Message: &gchat.Message{
			Name:   "spaces/LOOP1/messages/bot-1",
			Text:   "[@infra]: automated bot reply",
			Thread: &gchat.Thread{Name: "spaces/LOOP1/threads/t1"},
			Sender: &gchat.User{Email: "alice@example.com", Type: "BOT"},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("BOT message webhook status = %d, want 200", code)
	}

	// 2. Outbound Web UI message and agent transcript reply echoed back by Google Chat must be ignored.
	env.sender.Reset()
	code, _ = env.doAs("alice@example.com", http.MethodPost, tasks.ChatsPath+"/"+chat.ID+"/messages", map[string]any{
		"text": "initial human message from web",
	})
	if code != http.StatusCreated {
		t.Fatalf("POST web message failed: %d", code)
	}
	env.hub.Live.OnEvent(live.Event{
		Task: chat.TaskID,
		Event: events.Event{
			Seq:  1,
			Time: time.Now().UTC(),
			Type: events.TypeTranscript,
			Transcript: &events.Item{
				Kind: events.ItemText,
				Text: "Agent response from infra",
			},
		},
	})
	sent := env.sender.Messages()
	if len(sent) != 2 {
		t.Fatalf("expected 2 outbound messages, got %d", len(sent))
	}

	// Echo both outbound messages back into the Google Chat webhook (by message Name and by [@infra]: prefix).
	for _, s := range sent {
		code, _, _ = env.postWebhook(tok, gchat.Event{
			Type:  "MESSAGE",
			User:  gchat.User{Email: "alice@example.com"},
			Space: gchat.Space{Name: "spaces/LOOP1"},
			Message: &gchat.Message{
				Name:   s.Name,
				Text:   s.Text,
				Thread: &gchat.Thread{Name: "spaces/LOOP1/threads/t1"},
			},
		})
		if code != http.StatusOK {
			t.Fatalf("echoed outbound message status = %d, want 200", code)
		}
	}
	code, _, _ = env.postWebhook(tok, gchat.Event{
		Type:  "MESSAGE",
		User:  gchat.User{Email: "alice@example.com"},
		Space: gchat.Space{Name: "spaces/LOOP1"},
		Message: &gchat.Message{
			Name:   "spaces/LOOP1/messages/brand-new-echo-id",
			Text:   "[@infra]: echoed agent output with new message ID",
			Thread: &gchat.Thread{Name: "spaces/LOOP1/threads/t1"},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("echoed [@infra]: message status = %d, want 200", code)
	}

	// Only the 1 original human web message should exist in chat_messages!
	storedMsgs, err := env.hub.Backlog.ChatMessages(ctx, chat.ID, 0)
	must(t, err)
	if len(storedMsgs) != 1 {
		t.Fatalf("expected only 1 stored human message (no bot/echo loops), got %d: %+v", len(storedMsgs), storedMsgs)
	}
}
