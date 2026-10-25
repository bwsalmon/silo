package bridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/v3/gateway"
	"github.com/bwsalmon/silo/v3/gateway/approval"
)

func TestUITokenAuthCookieScopeAndCSRFIsolation(t *testing.T) {
	env := newTestEnv(t)

	// Generate a login token file (0600) using GenerateTokenFile.
	tokFile := filepath.Join(env.dir, "ui-token.sha256")
	rawToken, err := GenerateTokenFile(tokFile)
	if err != nil {
		t.Fatalf("GenerateTokenFile: %v", err)
	}
	if !strings.HasPrefix(rawToken, "silo_ui_") {
		t.Fatalf("unexpected token format: %q", rawToken)
	}
	st, err := os.Stat(tokFile)
	if err != nil {
		t.Fatalf("Stat token file: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %04o, want 0600", st.Mode().Perm())
	}
	fileBytes, _ := os.ReadFile(tokFile)
	if strings.Contains(string(fileBytes), rawToken) {
		t.Fatal("token file must store SHA-256 hash, not raw token")
	}

	auth, err := LoadTokenAuthenticator(tokFile)
	if err != nil {
		t.Fatalf("LoadTokenAuthenticator: %v", err)
	}
	env.br.cfg.Auth = auth

	// Start a backend server for an internal page link to verify cookie isolation.
	var proxiedCookieHeader string
	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backendLn.Close()
	backendSrv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			proxiedCookieHeader = r.Header.Get("Cookie")
			_, _ = w.Write([]byte("proxied-app-ok"))
		}),
	}
	go func() { _ = backendSrv.Serve(backendLn) }()
	defer backendSrv.Close()
	backendPort := backendLn.Addr().(*net.TCPAddr).Port

	// 1. Static UI assets at `/` and `/static/app.js` are served without a session cookie.
	indexResp, err := http.Get(env.httpServer.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	indexBody, _ := io.ReadAll(indexResp.Body)
	indexResp.Body.Close()
	if indexResp.StatusCode != http.StatusOK || !strings.Contains(string(indexBody), "silo v3") {
		t.Fatalf("GET / status=%d, body=%d bytes", indexResp.StatusCode, len(indexBody))
	}

	// 2. Unauthenticated API calls are rejected with 401 Unauthorized.
	if code := env.getJSON("/api/v1/agents", nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /api/v1/agents = %d, want 401", code)
	}
	if code := env.postJSON("/api/v1/agents", map[string]any{"name": "x"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated POST /api/v1/agents = %d, want 401", code)
	}

	// 3. Login with wrong token fails with 401.
	if code := env.postJSON("/api/v1/auth/login", map[string]any{"token": "wrong-token"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("login with wrong token = %d, want 401", code)
	}

	// 4. Login with valid token using a cookie-jar HTTP client.
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	loginBody, _ := json.Marshal(map[string]any{"token": rawToken})
	loginResp, err := client.Post(env.httpServer.URL+"/api/v1/auth/login", "application/json", bytes.NewReader(loginBody))
	if err != nil {
		t.Fatalf("POST /api/v1/auth/login: %v", err)
	}
	defer loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", loginResp.StatusCode)
	}
	var loginResult struct {
		Authenticated bool   `json:"authenticated"`
		CSRFToken     string `json:"csrf_token"`
	}
	if err := json.NewDecoder(loginResp.Body).Decode(&loginResult); err != nil {
		t.Fatalf("Decode login result: %v", err)
	}
	if !loginResult.Authenticated || loginResult.CSRFToken == "" {
		t.Fatalf("unexpected login result: %+v", loginResult)
	}

	// Verify Set-Cookie attributes: HttpOnly, SameSite=Strict, and Path scoped strictly to /api and /acp.
	cookies := loginResp.Cookies()
	if len(cookies) < 2 {
		t.Fatalf("expected scoped cookies for /api and /acp, got %d", len(cookies))
	}
	pathsSeen := map[string]bool{}
	for _, c := range cookies {
		if c.Name != SessionCookieName {
			continue
		}
		pathsSeen[c.Path] = true
		if !c.HttpOnly {
			t.Errorf("cookie on path %q must be HttpOnly", c.Path)
		}
		if c.SameSite != http.SameSiteStrictMode {
			t.Errorf("cookie on path %q SameSite = %v, want Strict", c.Path, c.SameSite)
		}
		if c.Path != "/api" && c.Path != "/acp" {
			t.Errorf("cookie scoped to unexpected Path %q (must only be /api or /acp)", c.Path)
		}
	}
	if !pathsSeen["/api"] || !pathsSeen["/acp"] {
		t.Fatalf("expected cookies for both /api and /acp, got %v", pathsSeen)
	}

	// 5. Authenticated GET /api/v1/agents succeeds.
	agResp, err := client.Get(env.httpServer.URL + "/api/v1/agents")
	if err != nil {
		t.Fatalf("authenticated GET /api/v1/agents: %v", err)
	}
	agResp.Body.Close()
	if agResp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated GET /api/v1/agents = %d, want 200", agResp.StatusCode)
	}

	// 6. Mutating POST without X-Silo-CSRF header is rejected with 403 Forbidden.
	pagePayload, _ := json.Marshal(map[string]any{
		"slug":  "app",
		"title": "Internal App",
		"host":  "127.0.0.1",
		"port":  backendPort,
	})
	noCSRFResp, err := client.Post(env.httpServer.URL+"/api/v1/pages", "application/json", bytes.NewReader(pagePayload))
	if err != nil {
		t.Fatal(err)
	}
	noCSRFResp.Body.Close()
	if noCSRFResp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST without X-Silo-CSRF = %d, want 403", noCSRFResp.StatusCode)
	}

	// Mutating POST with wrong X-Silo-CSRF header is rejected with 403 Forbidden.
	badCSRFReq, _ := http.NewRequest("POST", env.httpServer.URL+"/api/v1/pages", bytes.NewReader(pagePayload))
	badCSRFReq.Header.Set("Content-Type", "application/json")
	badCSRFReq.Header.Set(CSRFHeaderName, "wrong-csrf-token")
	badCSRFResp, err := client.Do(badCSRFReq)
	if err != nil {
		t.Fatal(err)
	}
	badCSRFResp.Body.Close()
	if badCSRFResp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST with wrong X-Silo-CSRF = %d, want 403", badCSRFResp.StatusCode)
	}

	// Mutating POST with valid X-Silo-CSRF header succeeds.
	goodReq, _ := http.NewRequest("POST", env.httpServer.URL+"/api/v1/pages", bytes.NewReader(pagePayload))
	goodReq.Header.Set("Content-Type", "application/json")
	goodReq.Header.Set(CSRFHeaderName, loginResult.CSRFToken)
	goodResp, err := client.Do(goodReq)
	if err != nil {
		t.Fatal(err)
	}
	goodResp.Body.Close()
	if goodResp.StatusCode != http.StatusOK {
		t.Fatalf("POST with valid X-Silo-CSRF = %d, want 200", goodResp.StatusCode)
	}

	// 7. Cookie path isolation: fetching /p/app/ with the same cookiejar client does NOT send the session cookie.
	pResp, err := client.Get(env.httpServer.URL + "/p/app/")
	if err != nil {
		t.Fatalf("GET /p/app/: %v", err)
	}
	pResp.Body.Close()
	if pResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /p/app/ = %d, want 200", pResp.StatusCode)
	}
	if proxiedCookieHeader != "" {
		t.Fatalf("expected /p/app/ to receive no cookies, got %q", proxiedCookieHeader)
	}

	// 8. Referer isolation: any /api/v1/* request with a Referer under /p/<slug>/ is blocked with 403 Forbidden,
	// even when carrying valid session cookies and CSRF token.
	refReq, _ := http.NewRequest("GET", env.httpServer.URL+"/api/v1/agents", nil)
	refReq.Header.Set("Referer", env.httpServer.URL+"/p/app/index.html")
	refReq.Header.Set(CSRFHeaderName, loginResult.CSRFToken)
	refResp, err := client.Do(refReq)
	if err != nil {
		t.Fatal(err)
	}
	refResp.Body.Close()
	if refResp.StatusCode != http.StatusForbidden {
		t.Fatalf("request with /p/ Referer = %d, want 403", refResp.StatusCode)
	}

	// 9. Logout revokes the session.
	logoutReq, _ := http.NewRequest("POST", env.httpServer.URL+"/api/v1/auth/logout", nil)
	logoutReq.Header.Set(CSRFHeaderName, loginResult.CSRFToken)
	logoutResp, err := client.Do(logoutReq)
	if err != nil {
		t.Fatal(err)
	}
	logoutResp.Body.Close()
	if logoutResp.StatusCode != http.StatusOK {
		t.Fatalf("logout status = %d, want 200", logoutResp.StatusCode)
	}
	afterLogoutResp, err := client.Get(env.httpServer.URL + "/api/v1/agents")
	if err != nil {
		t.Fatal(err)
	}
	afterLogoutResp.Body.Close()
	if afterLogoutResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/agents after logout = %d, want 401", afterLogoutResp.StatusCode)
	}
}

func TestIAPAuthenticator(t *testing.T) {
	iap := &IAPAuthenticator{
		AllowedOwners: []string{"alice@example.com"},
		CSRFSecret:    "test-secret",
		Verify: func(ctx context.Context, assertion string) (string, error) {
			if assertion == "valid-jwt-alice" {
				return "alice@example.com", nil
			}
			if assertion == "valid-jwt-mallory" {
				return "mallory@example.com", nil
			}
			return "", ErrUnauthenticated
		},
	}

	// Missing header -> unauthenticated.
	r1 := httptest.NewRequest("GET", "/api/v1/agents", nil)
	if _, err := iap.Authenticate(r1); err == nil {
		t.Fatal("expected unauthenticated when IAP header is missing")
	}

	// Unauthorized owner -> rejected.
	r2 := httptest.NewRequest("GET", "/api/v1/agents", nil)
	r2.Header.Set(IAPJWTHeaderName, "valid-jwt-mallory")
	if _, err := iap.Authenticate(r2); err == nil {
		t.Fatal("expected mallory@example.com to be rejected by AllowedOwners")
	}

	// Authorized owner -> Principal with deterministic CSRF token.
	r3 := httptest.NewRequest("GET", "/api/v1/agents", nil)
	r3.Header.Set(IAPJWTHeaderName, "valid-jwt-alice")
	p, err := iap.Authenticate(r3)
	if err != nil {
		t.Fatalf("Authenticate alice: %v", err)
	}
	if p.Subject != "alice@example.com" || p.Method != "iap" || p.CSRFToken == "" {
		t.Fatalf("unexpected IAP principal: %+v", p)
	}
	if !iap.RequireCSRF(p) {
		t.Fatal("expected RequireCSRF to be true for IAP principal")
	}
}

func TestRunAsRootApprovalQueueAndAuditViaBridgeHTTP(t *testing.T) {
	env := newTestEnv(t)

	// Connect the standing `root` agent with its seeded token so it can propose `run_as_root` batches.
	rootTokBytes, err := os.ReadFile(filepath.Join(env.gwCfg.TokenDir(), gateway.RootAgentID))
	if err != nil {
		t.Fatalf("read root token file: %v", err)
	}
	var rootConn gateway.ConnectResult
	if err := env.br.gwCall(env.ctx, "agent.connect", map[string]any{
		"token": strings.TrimSpace(string(rootTokBytes)),
	}, &rootConn); err != nil {
		t.Fatalf("connect root: %v", err)
	}

	// 1. Root submits a batch that modifies `/etc/silo/root.md` (triggering the
	// "changes the root agent's own instructions" risk flag and hash confirmation)
	// plus an exec step so we can inspect facts, flags, unified diff, and argv.
	var submit1 approval.SubmitResult
	if err := NewClient(env.brSock).Call(env.ctx, "run_as_root.submit", map[string]any{
		"agent_token": rootConn.SessionToken,
		"batch": map[string]any{
			"title":  "Update root instructions and check echo",
			"reason": "Need to document new Postgres service in root.md",
			"files": []map[string]any{
				{
					"path":        "/etc/silo/root.md",
					"content_b64": base64.StdEncoding.EncodeToString([]byte("# Updated Root Instructions\nManage postgres.\n")),
					"mode":        "0644",
				},
			},
			"steps": []map[string]any{
				{
					"argv": []string{"/bin/echo", "checking-system"},
				},
			},
		},
	}, &submit1); err != nil {
		t.Fatalf("run_as_root.submit (batch 1): %v", err)
	}

	// 2. List pending approvals via GET /api/v1/approvals and inspect full record via GET /api/v1/approvals/:id.
	var pending []approval.Summary
	if code := env.getJSON("/api/v1/approvals", &pending); code != http.StatusOK {
		t.Fatalf("GET /api/v1/approvals status = %d", code)
	}
	if len(pending) != 1 || pending[0].BatchID != submit1.BatchID {
		t.Fatalf("expected 1 pending batch %s, got %+v", submit1.BatchID, pending)
	}

	var b1Rec approval.Record
	if code := env.getJSON("/api/v1/approvals/"+submit1.BatchID, &b1Rec); code != http.StatusOK {
		t.Fatalf("GET /api/v1/approvals/%s status = %d", submit1.BatchID, code)
	}
	b1 := b1Rec.Presentation
	if b1Rec.ID != submit1.BatchID || b1.ShortHash == "" || len(b1.ShortHash) != 16 {
		t.Fatalf("unexpected batch presentation header: %+v", b1)
	}
	if b1.Snapshot != approval.NoSnapshotMessage {
		t.Fatalf("Snapshot = %q, want %q", b1.Snapshot, approval.NoSnapshotMessage)
	}
	if !b1.NeedsHashConfirmation {
		t.Fatal("expected NeedsHashConfirmation=true when modifying /etc/silo/root.md")
	}
	hasRootInstrFlag := false
	for _, fl := range b1.Flags {
		if fl.Code == approval.FlagRootInstructions && fl.Message == approval.RootInstructionsMessage {
			hasRootInstrFlag = true
		}
	}
	if !hasRootInstrFlag {
		t.Fatalf("expected 'changes the root agent's own instructions' flag, got %+v", b1.Flags)
	}
	if len(b1.Files) != 1 || !strings.Contains(b1.Files[0].Diff, "+# Updated Root Instructions") {
		t.Fatalf("expected unified diff on file 0, got %+v", b1.Files)
	}
	if len(b1.Steps) != 1 || len(b1.Steps[0].Argv) != 2 || b1.Steps[0].Argv[0] != "/bin/echo" {
		t.Fatalf("expected resolved argv on step 0, got %+v", b1.Steps)
	}

	// 3. Approving batch 1 without confirm hash must fail because NeedsHashConfirmation is true.
	if code := env.postJSON("/api/v1/approvals/"+b1Rec.ID+"/decision", map[string]any{
		"decision": "approve",
		"comment":  "trying without hash",
	}, nil); code == http.StatusOK {
		t.Fatal("expected approval without confirm hash to be rejected for root instructions edit")
	}

	// Reject batch 1 with a comment via POST /api/v1/approvals/:id/decision.
	var rejectedRec approval.Record
	if code := env.postJSON("/api/v1/approvals/"+b1Rec.ID+"/decision", map[string]any{
		"decision": "reject",
		"comment":  "do not edit root.md right now",
	}, &rejectedRec); code != http.StatusOK {
		t.Fatalf("reject batch 1 status = %d", code)
	}
	if rejectedRec.Status != approval.StatusRejected || rejectedRec.Decision == nil || rejectedRec.Decision.Comment != "do not edit root.md right now" {
		t.Fatalf("unexpected rejected batch state: %+v", rejectedRec)
	}

	// 4. Root submits a second batch that writes a file in env.dir and runs `/bin/echo`.
	targetFile := filepath.Join(env.dir, "approved-output.txt")
	var submit2 approval.SubmitResult
	if err := NewClient(env.brSock).Call(env.ctx, "run_as_root.submit", map[string]any{
		"agent_token": rootConn.SessionToken,
		"batch": map[string]any{
			"title":  "Write config file and echo status",
			"reason": "Requested by operator",
			"files": []map[string]any{
				{
					"path":        targetFile,
					"content_b64": base64.StdEncoding.EncodeToString([]byte("hello-approved-world\n")),
					"mode":        "0644",
				},
			},
			"steps": []map[string]any{
				{
					"argv": []string{"/bin/echo", "step-two-ok"},
				},
			},
		},
	}, &submit2); err != nil {
		t.Fatalf("run_as_root.submit (batch 2): %v", err)
	}

	// Approve batch 2 via POST /api/v1/approvals/:id/decision.
	var approvedRec approval.Record
	if code := env.postJSON("/api/v1/approvals/"+submit2.BatchID+"/decision", map[string]any{
		"decision": "approve",
		"comment":  "looks safe",
		"wait_s":   3,
	}, &approvedRec); code != http.StatusOK {
		t.Fatalf("approve batch 2 status = %d", code)
	}
	if approvedRec.Status != approval.StatusSucceeded {
		t.Fatalf("batch 2 status = %q (err=%q), want succeeded", approvedRec.Status, approvedRec.Error)
	}
	if len(approvedRec.Steps) != 1 || !strings.Contains(approvedRec.Steps[0].Stdout, "step-two-ok") {
		t.Fatalf("expected step results with stdout 'step-two-ok', got %+v", approvedRec.Steps)
	}
	written, err := os.ReadFile(targetFile)
	if err != nil || string(written) != "hello-approved-world\n" {
		t.Fatalf("targetFile content = %q, err = %v", string(written), err)
	}

	// 5. Verify GET /api/v1/approvals?all=true returns both decided batches in history.
	var allBatches []approval.Summary
	if code := env.getJSON("/api/v1/approvals?all=true", &allBatches); code != http.StatusOK {
		t.Fatalf("GET /api/v1/approvals?all=true status = %d", code)
	}
	if len(allBatches) != 2 {
		t.Fatalf("expected 2 total batches in history, got %d", len(allBatches))
	}

	// 6. Verify audit log and chain verification via GET /api/v1/audit and GET /api/v1/audit/verify.
	var entries []gateway.AuditEntry
	if code := env.getJSON("/api/v1/audit", &entries); code != http.StatusOK {
		t.Fatalf("GET /api/v1/audit status = %d", code)
	}
	if len(entries) == 0 {
		t.Fatal("expected non-empty audit log")
	}
	var verifyRes gateway.VerifyResult
	if code := env.getJSON("/api/v1/audit/verify", &verifyRes); code != http.StatusOK {
		t.Fatalf("GET /api/v1/audit/verify status = %d", code)
	}
	if !verifyRes.OK || verifyRes.Entries == 0 {
		t.Fatalf("unexpected audit verify result: %+v", verifyRes)
	}
}

func TestGeneralRequestForwardToRootAndResolveViaHTTP(t *testing.T) {
	env := newTestEnv(t)

	// Register and connect a child agent, then file a general request.
	var reg gateway.RegisterResult
	if code := env.postJSON("/api/v1/agents", map[string]any{
		"name": "worker-1",
		"kind": "container",
	}, &reg); code != http.StatusOK {
		t.Fatalf("register worker-1 status = %d", code)
	}
	var workerConn gateway.ConnectResult
	if err := env.br.gwCall(env.ctx, "agent.connect", map[string]any{
		"token": reg.Token,
	}, &workerConn); err != nil {
		t.Fatalf("connect worker-1: %v", err)
	}

	reqBody, _ := json.Marshal(map[string]any{
		"summary": "Need nginx installed",
		"detail":  "Please install nginx on the host and expose port 8080",
	})
	httpReq, _ := http.NewRequest("POST", env.httpServer.URL+"/api/v1/requests", bytes.NewReader(reqBody))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Silo-Agent-Token", workerConn.SessionToken)
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	var filed gateway.AgentRequest
	_ = json.NewDecoder(resp.Body).Decode(&filed)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || filed.ID == "" {
		t.Fatalf("file request status=%d, filed=%+v", resp.StatusCode, filed)
	}

	// Forward the request to root via POST /api/v1/requests/:id/resolve with resolution="forward".
	var resolved gateway.AgentRequest
	if code := env.postJSON("/api/v1/requests/"+filed.ID+"/resolve", map[string]any{
		"resolution": "forward",
		"note":       "Approved for root to inspect",
	}, &resolved); code != http.StatusOK {
		t.Fatalf("resolve request status = %d", code)
	}
	if resolved.State != gateway.RequestForwarded {
		t.Fatalf("resolved.State = %q, want %q", resolved.State, gateway.RequestForwarded)
	}

	// Verify root's transcript contains the forwarded request marked clearly as untrusted content.
	var rootTx []TranscriptEvent
	waitFor(t, "forwarded request in root transcript", func() bool {
		if code := env.getJSON("/api/v1/sessions/root/transcript", &rootTx); code != http.StatusOK {
			return false
		}
		for _, ev := range rootTx {
			if strings.Contains(string(ev.Message), "Request from agent worker-1 (untrusted content)") &&
				strings.Contains(string(ev.Message), "Need nginx installed") {
				return true
			}
		}
		return false
	})
	_ = time.Now()
}
