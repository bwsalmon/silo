package refgrain

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/events"
	"github.com/bwsalmon/silo/hub/conformance/workload"
	"github.com/bwsalmon/silo/hub/directory"
	"github.com/bwsalmon/silo/hub/identity"
	"github.com/bwsalmon/silo/hub/live"
	"github.com/bwsalmon/silo/hub/nodeui"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
	"github.com/bwsalmon/silo/hub/tasks"
)

type headerAuth struct{}

func (headerAuth) Principal(r *http.Request) (identity.Principal, error) {
	u := r.Header.Get("X-User")
	if u == "" {
		return identity.Principal{}, identity.ErrUnauthenticated
	}
	return identity.Principal{Email: u}, nil
}

func TestReferenceNodeSupportU3U5U7G2G4(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir, err := directory.Open(ctx, "sqlite::memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	if err := dir.CreateTeam(ctx, "infra", "Infra"); err != nil {
		t.Fatal(err)
	}
	if err := dir.SetMember(ctx, "infra", "user:alice@example.com", directory.Admin); err != nil {
		t.Fatal(err)
	}
	if err := dir.SetMember(ctx, "infra", "user:bob@example.com", directory.Member); err != nil {
		t.Fatal(err)
	}
	if err := dir.AssignRepo(ctx, "infra", "bwsalmon/silo"); err != nil {
		t.Fatal(err)
	}

	tok, _, err := dir.MintNodeToken(ctx, "bob@example.com", "ws-ref")
	if err != nil {
		t.Fatal(err)
	}

	backlog, err := tasks.Open(ctx, "sqlite::memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer backlog.Close()

	evStore, err := live.Open(ctx, "sqlite::memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer evStore.Close()

	reg := registry.New()
	dirAPI := &directory.API{Store: dir, Auth: headerAuth{}}
	rv := &rendezvous.Server{
		Registry:     reg,
		Authenticate: dir.Authenticate,
		Admit:        dir.Admit,
		ValidateUI:   dir.ValidateNodeUI,
		Heartbeat:    time.Second,
	}
	liveHub := &live.Hub{
		Log:       evStore,
		Tasks:     backlog,
		Messages:  backlog.MessageEvent,
		Directory: dirAPI,
		Open:      rv.Open,
		NodeOwner: func(id string) (string, bool) {
			e, ok := reg.Get(id)
			return e.Owner, ok
		},
	}
	rv.NodeStream = liveHub.NodeStream

	disp := &tasks.Dispatcher{
		Store:     backlog,
		Registry:  reg,
		Open:      rv.Open,
		Directory: dir,
		Finished:  func(tk tasks.Task) { liveHub.Finished(tk.ID) },
	}
	tasksAPI := &tasks.API{
		Store:      backlog,
		Dispatcher: disp,
		Directory:  dirAPI,
		Requests:   evStore,
	}
	uiSrv := &nodeui.Server{
		Domain:    "nodes.hub.test",
		Registry:  reg,
		Directory: dirAPI,
		Open:      rv.Open,
	}

	mux := http.NewServeMux()
	mux.Handle(rendezvous.ConnectPath, rv.Handler())
	mux.Handle("/v1/tasks", tasksAPI.Handler())
	mux.Handle("/v1/tasks/", tasksAPI.Handler())
	srv := httptest.NewServer(uiSrv.Hosts(mux))
	defer srv.Close()

	go disp.Run(ctx)

	g := &Grain{
		Hub:       srv.URL,
		Token:     tok,
		ID:        "ws-ref",
		Repos:     []string{"bwsalmon/silo"},
		Allow:     []string{"bwsalmon/silo"},
		Slots:     2,
		Features:  []string{FeatureApprovals, FeatureInteractive, FeatureFiling},
		UIOffered: true,
		UITeam:    "infra",
		UIAccess:  "read-only",
		Tick:      50 * time.Millisecond,
	}
	grainCtx, stopGrain := context.WithCancel(ctx)
	defer stopGrain()
	go g.Run(grainCtx)

	waitOnline := func() {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if e, ok := reg.Get("ws-ref"); ok && e.Status == registry.Online {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("timed out waiting for refgrain to come online")
	}
	waitOnline()

	doJSON := func(user, method, path string, body any) (int, []byte) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = strings.NewReader(string(b))
		}
		req, err := http.NewRequestWithContext(ctx, method, srv.URL+path, rd)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-User", user)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}

	// 1. U3 (In-run approvals) + U5 (hub_task_id and hub_url on node task status/UI).
	code, b := doJSON("alice@example.com", "POST", "/v1/tasks", map[string]any{
		"repo":   "bwsalmon/silo",
		"title":  "U3 approval",
		"prompt": workload.Prompt(workload.Approve),
	})
	if code != http.StatusCreated {
		t.Fatalf("submit approve task: %d %s", code, b)
	}
	var appTask tasks.Task
	if err := json.Unmarshal(b, &appTask); err != nil {
		t.Fatal(err)
	}

	// Wait for approval_request event to be ingested.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, ok, err := evStore.ApprovalRequest(ctx, appTask.ID, workload.ApprovalID)
		if err == nil && ok {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	// Verify U5: TaskStatus exposes hub_task_id and hub_url (<hub>/tasks/<id>).
	st, ok := g.TaskStatus(appTask.ID)
	if !ok {
		t.Fatalf("expected TaskStatus for %s on refgrain", appTask.ID)
	}
	wantHubURL := srv.URL + "/tasks/" + appTask.ID
	if st.HubTaskID != appTask.ID || st.HubURL != wantHubURL {
		t.Fatalf("U5 TaskStatus = %+v, want hub_task_id=%q hub_url=%q", st, appTask.ID, wantHubURL)
	}

	// Deliver in-run approval decision and wait for task completion.
	code, b = doJSON("alice@example.com", "POST", "/v1/tasks/"+appTask.ID+"/approvals/"+workload.ApprovalID, map[string]string{
		"decision": "allow",
	})
	if code != http.StatusOK {
		t.Fatalf("answer approval: %d %s", code, b)
	}
	for time.Now().Before(deadline) {
		cur, err := backlog.Get(ctx, appTask.ID)
		if err == nil && cur.State == tasks.Done {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if cur, _ := backlog.Get(ctx, appTask.ID); cur.State != tasks.Done || !strings.Contains(cur.Reason, "allowed by alice@example.com") {
		t.Fatalf("U3 task after approval: %+v", cur)
	}

	// 2. U7 (Interactive sessions): accept kind="interactive", emit awaiting_reply, receive message.
	code, b = doJSON("alice@example.com", "POST", "/v1/tasks", map[string]any{
		"repo":   "bwsalmon/silo",
		"kind":   "interactive",
		"title":  "U7 session",
		"prompt": workload.Prompt(workload.Interactive),
	})
	if code != http.StatusCreated {
		t.Fatalf("submit interactive task: %d %s", code, b)
	}
	var sessTask tasks.Task
	if err := json.Unmarshal(b, &sessTask); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		evs, err := evStore.Since(ctx, sessTask.ID, 0, 20)
		hasWaiting := false
		for _, ev := range evs {
			if ev.Type == events.TypeAwaitingReply && ev.AwaitingReply != nil && ev.AwaitingReply.State == events.AwaitingWaiting {
				hasWaiting = true
			}
		}
		if err == nil && hasWaiting {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	code, b = doJSON("alice@example.com", "POST", "/v1/tasks/"+sessTask.ID+"/messages", map[string]string{
		"id":   "m-u7",
		"text": "continue",
	})
	if code != http.StatusCreated {
		t.Fatalf("post message: %d %s", code, b)
	}
	for time.Now().Before(deadline) {
		cur, err := backlog.Get(ctx, sessTask.ID)
		if err == nil && cur.State == tasks.Done {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if cur, _ := backlog.Get(ctx, sessTask.ID); cur.State != tasks.Done || !strings.Contains(cur.Reason, "took message m-u7") {
		t.Fatalf("U7 session after message: %+v", cur)
	}

	// 3. G2-G4 (Team-admin Node UI proxy, read-only enforcement, and credential stripping verification).
	uiReq := func(user, method, path string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "ws-ref.nodes.hub.test"
		req.Header.Set("X-User", user)
		// Include sensitive headers and cookies that hub/nodeui must strip before reaching refgrain.
		req.Header.Set("X-Goog-Authenticated-User-Email", "accounts.google.com:"+user)
		req.Header.Set("X-Goog-Authenticated-User-ID", "accounts.google.com:12345")
		req.Header.Set("X-Goog-IAP-JWT-Assertion", "secret-jwt")
		req.AddCookie(&http.Cookie{Name: identity.SessionCookie, Value: "secret-hub-session"})
		req.AddCookie(&http.Cookie{Name: "__Host-" + identity.SessionCookie, Value: "secret-host-session"})
		req.AddCookie(&http.Cookie{Name: "GCP_IAP_UID", Value: "secret-iap"})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, body
	}

	// Team admin (alice) is allowed GET and HEAD when ui.access == "read-only",
	// and sees U5 hub_task_id and hub_url in the node UI response.
	code, b = uiReq("alice@example.com", http.MethodGet, "/api/tasks/"+appTask.ID)
	if code != http.StatusOK {
		t.Fatalf("team-admin GET /api/tasks/%s: %d %s", appTask.ID, code, b)
	}
	var uiTask TaskStatus
	if err := json.Unmarshal(b, &uiTask); err != nil {
		t.Fatal(err)
	}
	if uiTask.HubTaskID != appTask.ID || uiTask.HubURL != wantHubURL {
		t.Errorf("node UI U5 task response = %+v, want hub_task_id=%q hub_url=%q", uiTask, appTask.ID, wantHubURL)
	}
	if code, _ := uiReq("alice@example.com", http.MethodHead, "/api/tasks"); code != http.StatusOK {
		t.Errorf("team-admin HEAD /api/tasks: %d, want 200", code)
	}

	// Team admin (alice) is rejected with 403 Forbidden on POST, PUT, DELETE, PATCH when ui.access == "read-only".
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		if code, body := uiReq("alice@example.com", method, "/api/action"); code != http.StatusForbidden {
			t.Errorf("team-admin %s with read-only access: %d %s, want 403", method, code, body)
		}
	}

	// Node owner (bob) has role "owner" and is allowed state-mutating methods.
	if code, body := uiReq("bob@example.com", http.MethodPost, "/api/action"); code != http.StatusOK {
		t.Errorf("owner POST /api/action: %d %s, want 200", code, body)
	}

	// Direct unit check on LocalUIHandler: if X-Goog-Authenticated-User-* or Hub session cookie
	// ever bypassed the hub proxy and reached the node handler, it rejects the request.
	h := g.LocalUIHandler(nodeui.Message{Role: nodeui.RoleTeamAdmin, Viewer: "alice@example.com"})
	rec := httptest.NewRecorder()
	leakedReq := httptest.NewRequest(http.MethodGet, "/", nil)
	leakedReq.Header.Set("X-Goog-Authenticated-User-Email", "alice@example.com")
	h.ServeHTTP(rec, leakedReq)
	if rec.Code == http.StatusOK {
		t.Errorf("LocalUIHandler accepted request with leaked X-Goog-Authenticated-User-Email header")
	}
}
