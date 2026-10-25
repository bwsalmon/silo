package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/v3/gateway"
)

var (
	fakeACPOnce sync.Once
	fakeACPBin  string
	fakeACPErr  error
)

func buildFakeACP(t *testing.T) string {
	t.Helper()
	fakeACPOnce.Do(func() {
		dir, err := os.MkdirTemp("", "silo-fakeacp-*")
		if err != nil {
			fakeACPErr = err
			return
		}
		bin := filepath.Join(dir, "fakeacp")
		cmd := exec.Command("go", "build", "-o", bin, "./testdata/fakeacp")
		out, err := cmd.CombinedOutput()
		if err != nil {
			fakeACPErr = fmt.Errorf("go build fakeacp: %w: %s", err, out)
			return
		}
		fakeACPBin = bin
	})
	if fakeACPErr != nil {
		t.Fatal(fakeACPErr)
	}
	return fakeACPBin
}

type testEnv struct {
	t          *testing.T
	dir        string
	gwCfg      *gateway.Config
	gwSock     string
	brSock     string
	sessDir    string
	gw         *gateway.Gateway
	br         *Bridge
	httpServer *httptest.Server
	ctx        context.Context
	cancel     context.CancelFunc
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	gwSock := filepath.Join(dir, "gw.sock")
	brSock := filepath.Join(dir, "br.sock")
	sessDir := filepath.Join(dir, "sessions")

	gwCfg := &gateway.Config{
		StateDir:   filepath.Join(dir, "state"),
		AuditLog:   filepath.Join(dir, "audit.jsonl"),
		SocketPath: gwSock,
		AdminUIDs:  []int{1000},
	}

	audit, err := gateway.OpenAuditLog(gwCfg.AuditLog, realClock{}, false)
	if err != nil {
		t.Fatalf("OpenAuditLog: %v", err)
	}

	const testBridgeUID = 999
	gw, err := gateway.New(gwCfg, gateway.Deps{
		Audit: audit,
		Store: gateway.Store{Dir: gwCfg.StateDir},
		Logf:  t.Logf,
		LookupUID: func(name string) (int, error) {
			if name == gwCfg.BridgeUser {
				return testBridgeUID, nil
			}
			return -1, nil
		},
	})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	if err := gw.Start(ctx); err != nil {
		t.Fatalf("gw.Start: %v", err)
	}

	gwListener, err := gateway.Listen(gwSock, -1)
	if err != nil {
		t.Fatalf("gateway.Listen: %v", err)
	}

	gwSrv := &gateway.Server{
		G: gw,
		Cred: func(net.Conn) (gateway.PeerCred, error) {
			return gateway.PeerCred{UID: testBridgeUID, PID: os.Getpid()}, nil
		},
	}
	gwDone := make(chan struct{})
	go func() {
		defer close(gwDone)
		_ = gwSrv.Serve(ctx, gwListener)
	}()

	br, err := New(Config{
		GatewaySocket:     gwSock,
		BridgeSocket:      brSock,
		SessionsDir:       sessDir,
		HeartbeatInterval: 100 * time.Millisecond,
		Logf:              t.Logf,
	})
	if err != nil {
		t.Fatalf("bridge.New: %v", err)
	}

	brListener, err := ListenSocket(brSock)
	if err != nil {
		t.Fatalf("ListenSocket: %v", err)
	}

	brSrv := &SocketServer{B: br}
	brDone := make(chan struct{})
	go func() {
		defer close(brDone)
		_ = brSrv.Serve(ctx, brListener)
	}()

	hs := httptest.NewServer(br.Handler())
	t.Cleanup(func() {
		cancel()
		hs.Close()
		_ = br.Close()
		_ = brListener.Close()
		<-brDone
		_ = gwListener.Close()
		<-gwDone
		_ = audit.Close()
	})

	return &testEnv{
		t:          t,
		dir:        dir,
		gwCfg:      gwCfg,
		gwSock:     gwSock,
		brSock:     brSock,
		sessDir:    sessDir,
		gw:         gw,
		br:         br,
		httpServer: hs,
		ctx:        ctx,
		cancel:     cancel,
	}
}

func (e *testEnv) postJSON(path string, body any, out any) int {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("Marshal: %v", err)
		}
		r = bytes.NewReader(b)
	}
	resp, err := http.Post(e.httpServer.URL+path, "application/json", r)
	if err != nil {
		e.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, out)
	}
	return resp.StatusCode
}

func (e *testEnv) getJSON(path string, out any) int {
	e.t.Helper()
	resp, err := http.Get(e.httpServer.URL + path)
	if err != nil {
		e.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, out)
	}
	return resp.StatusCode
}

func waitFor(t *testing.T, desc string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

func TestBridgeConnectAndPermissionsAskDefault(t *testing.T) {
	bin := buildFakeACP(t)
	env := newTestEnv(t)

	// 1. Verify standing agents (`root` and `main`) default to permissions="ask".
	var agents []gateway.AgentInfo
	if code := env.getJSON("/api/v1/agents", &agents); code != http.StatusOK {
		t.Fatalf("GET /api/v1/agents status = %d", code)
	}
	foundRoot, foundMain := false, false
	for _, a := range agents {
		if a.ID == "root" {
			foundRoot = true
			if a.Permissions != gateway.PermAsk {
				t.Fatalf("root permissions = %q, want %q", a.Permissions, gateway.PermAsk)
			}
		}
		if a.ID == "main" {
			foundMain = true
			if a.Permissions != gateway.PermAsk {
				t.Fatalf("main permissions = %q, want %q", a.Permissions, gateway.PermAsk)
			}
		}
	}
	if !foundRoot || !foundMain {
		t.Fatalf("standing agents missing: root=%v main=%v", foundRoot, foundMain)
	}

	// 2. Register a new worker agent as human via HTTP; verify it defaults to permissions="ask".
	var reg gateway.RegisterResult
	if code := env.postJSON("/api/v1/agents", map[string]any{
		"name":  "worker-1",
		"kind":  "process",
		"brief": "no-perm initial setup",
	}, &reg); code != http.StatusOK {
		t.Fatalf("POST /api/v1/agents status = %d", code)
	}
	if reg.Token == "" {
		t.Fatalf("expected registration token for worker-1")
	}

	var workerInfo gateway.AgentInfo
	if code := env.getJSON("/api/v1/agents/worker-1", &workerInfo); code != http.StatusOK {
		t.Fatalf("GET /api/v1/agents/worker-1 status = %d", code)
	}
	if workerInfo.Permissions != gateway.PermAsk {
		t.Fatalf("worker-1 permissions = %q, want %q", workerInfo.Permissions, gateway.PermAsk)
	}

	// 3. Connect worker-1 using `silo connect` over the bridge Unix socket.
	connectCtx, connectCancel := context.WithCancel(env.ctx)
	defer connectCancel()
	connectDone := make(chan error, 1)
	go func() {
		connectDone <- RunConnect(connectCtx, ConnectOptions{
			SocketPath: env.brSock,
			Token:      reg.Token,
			HomeDir:    env.dir,
			Command:    []string{bin},
		})
	}()

	// Wait for the initial brief ("no-perm initial setup") to complete and land in the transcript.
	waitFor(t, "initial brief completion in transcript", func() bool {
		lines, err := env.br.ReadTranscript("worker-1", 0)
		if err != nil {
			return false
		}
		for _, l := range lines {
			if strings.Contains(string(l.Message), "completed: no-perm initial setup") {
				return true
			}
		}
		return false
	})

	// 4. Open a live WebSocket stream to /acp/v1/sessions/worker-1 and verify history replay.
	wsURL := "ws" + strings.TrimPrefix(env.httpServer.URL, "http") + "/acp/v1/sessions/worker-1"
	wsConn, err := DialWS(env.ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("DialWS session stream: %v", err)
	}
	defer wsConn.Close()

	seenHistory := false
	for i := 0; i < 6; i++ {
		frame, err := wsConn.ReadFrame()
		if err != nil {
			t.Fatalf("ReadFrame from session WS: %v", err)
		}
		if strings.Contains(string(frame), "no-perm initial setup") {
			seenHistory = true
			break
		}
	}
	if !seenHistory {
		t.Fatal("expected initial brief frame on WS history replay")
	}

	// 5. Send a prompt that triggers `session/request_permission`.
	// Because worker-1 defaults to `permissions: "ask"`, the prompt must block on human approval.
	type promptResult struct {
		code int
		body map[string]any
	}
	promptCh := make(chan promptResult, 1)
	go func() {
		var out map[string]any
		code := env.postJSON("/api/v1/sessions/worker-1/prompt", map[string]any{
			"text": "deploy service",
		}, &out)
		promptCh <- promptResult{code: code, body: out}
	}()

	// Wait for the permission request to appear in the pending queue.
	var permID string
	waitFor(t, "pending ACP permission request", func() bool {
		var perms []*PermissionRequest
		env.getJSON("/api/v1/permissions?pending=true", &perms)
		for _, p := range perms {
			if p.Agent == "worker-1" && p.Kind == "acp" && p.State == PermStatePending {
				permID = p.ID
				return true
			}
		}
		return false
	})

	// Verify the prompt is still blocked before human approval.
	select {
	case res := <-promptCh:
		t.Fatalf("expected prompt to block until human approves permission, but got early result: %+v", res)
	case <-time.After(100 * time.Millisecond):
	}

	// 6. Approve the pending permission as human via POST /api/v1/permissions/:id/decision.
	var decided PermissionRequest
	if code := env.postJSON("/api/v1/permissions/"+permID+"/decision", map[string]any{
		"decision": "approve",
	}, &decided); code != http.StatusOK {
		t.Fatalf("approve permission status = %d", code)
	}
	if decided.State != PermStateApproved {
		t.Fatalf("permission state = %q, want approved", decided.State)
	}

	// Now the prompt must finish with stopReason="end_turn".
	select {
	case res := <-promptCh:
		if res.code != http.StatusOK {
			t.Fatalf("prompt status = %d, body = %v", res.code, res.body)
		}
		if res.body["stopReason"] != "end_turn" {
			t.Fatalf("stopReason = %v, want end_turn", res.body["stopReason"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for prompt to complete after permission approval")
	}

	// 7. Test rejecting a permission under `permissions: "ask"`.
	rejectCh := make(chan promptResult, 1)
	go func() {
		var out map[string]any
		code := env.postJSON("/api/v1/sessions/worker-1/prompt", map[string]any{
			"text": "dangerous command",
		}, &out)
		rejectCh <- promptResult{code: code, body: out}
	}()

	var rejectPermID string
	waitFor(t, "second pending ACP permission request", func() bool {
		var perms []*PermissionRequest
		env.getJSON("/api/v1/permissions?pending=true", &perms)
		for _, p := range perms {
			if p.Agent == "worker-1" && p.State == PermStatePending {
				rejectPermID = p.ID
				return true
			}
		}
		return false
	})

	if code := env.postJSON("/api/v1/permissions/"+rejectPermID+"/decision", map[string]any{
		"decision": "reject",
	}, nil); code != http.StatusOK {
		t.Fatalf("reject permission status = %d", code)
	}

	select {
	case res := <-rejectCh:
		if res.code != http.StatusOK {
			t.Fatalf("rejected prompt status = %d", res.code)
		}
		if res.body["stopReason"] != "cancelled" {
			t.Fatalf("stopReason = %v, want cancelled", res.body["stopReason"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for rejected prompt to finish")
	}

	// 8. Switch worker-1 to `permissions: "auto"` and verify subsequent prompts auto-approve.
	var updatedInfo gateway.AgentInfo
	if code := env.postJSON("/api/v1/agents/worker-1/permissions", map[string]any{
		"permissions": "auto",
	}, &updatedInfo); code != http.StatusOK {
		t.Fatalf("set permissions status = %d", code)
	}
	if updatedInfo.Permissions != gateway.PermAuto {
		t.Fatalf("permissions = %q, want auto", updatedInfo.Permissions)
	}

	var autoOut map[string]any
	if code := env.postJSON("/api/v1/sessions/worker-1/prompt", map[string]any{
		"text": "auto-approved task",
	}, &autoOut); code != http.StatusOK {
		t.Fatalf("auto prompt status = %d", code)
	}
	if autoOut["stopReason"] != "end_turn" {
		t.Fatalf("auto prompt stopReason = %v, want end_turn", autoOut["stopReason"])
	}

	// 9. Stop worker-1 via /api/v1/agents/worker-1/stop and verify `silo connect` terminates cleanly.
	var stopRes gateway.StopResult
	if code := env.postJSON("/api/v1/agents/worker-1/stop", map[string]any{}, &stopRes); code != http.StatusOK {
		t.Fatalf("stop status = %d", code)
	}
	if len(stopRes.Stopped) == 0 || stopRes.Stopped[0] != "worker-1" {
		t.Fatalf("unexpected stop result: %+v", stopRes)
	}

	select {
	case err := <-connectDone:
		if err != nil {
			t.Fatalf("RunConnect returned unexpected error on stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for RunConnect to exit after stop")
	}
}

func TestChildRegisterRequiresHumanApprovalByDefault(t *testing.T) {
	env := newTestEnv(t)
	client := NewClient(env.brSock)

	// Register a parent agent `lead` with may_register=true (defaults to permissions="ask").
	var leadReg gateway.RegisterResult
	if code := env.postJSON("/api/v1/agents", map[string]any{
		"name":         "lead",
		"kind":         "process",
		"may_register": true,
	}, &leadReg); code != http.StatusOK {
		t.Fatalf("register lead status = %d", code)
	}

	// Connect `lead` on the gateway so its session token can register children.
	var leadConn gateway.ConnectResult
	if err := env.br.gwCall(env.ctx, "agent.connect", map[string]any{"token": leadReg.Token}, &leadConn); err != nil {
		t.Fatalf("connect lead: %v", err)
	}

	// Have `lead` call `agent.register` with wait_approval=true (as `silo-mcp` does).
	// Because `lead` has permissions="ask", this must create `child-1` in `pending` state
	// and block until the human approves it.
	regCh := make(chan struct {
		res gateway.RegisterResult
		err error
	}, 1)
	go func() {
		var res gateway.RegisterResult
		err := client.Call(env.ctx, "agent.register", map[string]any{
			"agent_token":   leadConn.SessionToken,
			"name":          "child-1",
			"kind":          "process",
			"brief":         "investigate logs",
			"wait_approval": true,
		}, &res)
		regCh <- struct {
			res gateway.RegisterResult
			err error
		}{res: res, err: err}
	}()

	// Wait for the pending `register_agent` permission request to appear.
	var permID string
	waitFor(t, "pending register_agent permission", func() bool {
		perms := env.br.ListPermissions("lead", true)
		for _, p := range perms {
			if p.Kind == "register_agent" && p.TargetAgent == "child-1" {
				permID = p.ID
				return true
			}
		}
		return false
	})

	// Verify child-1 is in state="pending" before approval.
	var childStatus gateway.AgentInfo
	if err := client.Call(env.ctx, "agent.status", map[string]any{"agent": "child-1"}, &childStatus); err != nil {
		t.Fatalf("status child-1: %v", err)
	}
	if childStatus.State != gateway.StatePending {
		t.Fatalf("child-1 state before approval = %q, want %q", childStatus.State, gateway.StatePending)
	}

	// Approve the permission via POST /api/v1/permissions/:id/decision.
	if code := env.postJSON("/api/v1/permissions/"+permID+"/decision", map[string]any{
		"decision": "approve",
	}, nil); code != http.StatusOK {
		t.Fatalf("approve register_agent status = %d", code)
	}

	// Now `lead`'s blocked `agent.register` call returns with the active token!
	select {
	case out := <-regCh:
		if out.err != nil {
			t.Fatalf("agent.register failed after approval: %v", out.err)
		}
		if out.res.Token == "" {
			t.Fatal("expected non-empty token after human approval")
		}
		if out.res.State != gateway.StateRegistered {
			t.Fatalf("agent state = %q, want %q", out.res.State, gateway.StateRegistered)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for agent.register to return after approval")
	}
}

func TestPageLinksAndReverseProxy(t *testing.T) {
	env := newTestEnv(t)

	// Start a backend HTTP + WebSocket server representing an agent's internal web service.
	backendMux := http.NewServeMux()
	backendMux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"path":       r.URL.Path,
			"query":      r.URL.RawQuery,
			"cookie":     r.Header.Get("Cookie"),
			"auth":       r.Header.Get("Authorization"),
			"fwd_prefix": r.Header.Get("X-Forwarded-Prefix"),
		})
	})
	backendMux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		fc, err := UpgradeWS(w, r)
		if err != nil {
			return
		}
		defer fc.Close()
		msg, err := fc.ReadFrame()
		if err != nil {
			return
		}
		_ = fc.WriteFrame([]byte("echo:" + string(msg)))
	})
	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen backend: %v", err)
	}
	defer backendLn.Close()
	backendSrv := &http.Server{Handler: backendMux}
	go func() { _ = backendSrv.Serve(backendLn) }()
	defer backendSrv.Close()
	backendPort := backendLn.Addr().(*net.TCPAddr).Port

	// Pick a free port for ExposePort.
	probeLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen probe: %v", err)
	}
	exposePort := probeLn.Addr().(*net.TCPAddr).Port
	_ = probeLn.Close()

	// 1. Register internal port page link with expose_port.
	var page gateway.Page
	if code := env.postJSON("/api/v1/pages", map[string]any{
		"slug":        "preview",
		"title":       "Preview App",
		"host":        "127.0.0.1",
		"port":        backendPort,
		"expose_port": exposePort,
	}, &page); code != http.StatusOK {
		t.Fatalf("register internal page status = %d", code)
	}
	if page.Slug != "preview" || page.Port != backendPort || page.ExposePort != exposePort {
		t.Fatalf("unexpected page: %+v", page)
	}

	// 2. Request /p/preview/hello?foo=bar with sensitive headers and verify they are stripped.
	req, err := http.NewRequest("GET", env.httpServer.URL+"/p/preview/hello?foo=bar", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", "silo_session=topsecret")
	req.Header.Set("Authorization", "Bearer topsecret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /p/preview/hello: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /p/preview/hello status = %d", resp.StatusCode)
	}
	var echo map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&echo); err != nil {
		t.Fatalf("Decode echo: %v", err)
	}
	if echo["path"] != "/hello" || echo["query"] != "foo=bar" {
		t.Fatalf("unexpected proxied path/query: %+v", echo)
	}
	if echo["cookie"] != "" || echo["auth"] != "" {
		t.Fatalf("expected Cookie and Authorization to be stripped, got: %+v", echo)
	}
	if echo["fwd_prefix"] != "/p/preview" {
		t.Fatalf("X-Forwarded-Prefix = %v, want /p/preview", echo["fwd_prefix"])
	}

	// 3. Test WebSocket reverse-proxying through /p/preview/ws.
	wsURL := "ws" + strings.TrimPrefix(env.httpServer.URL, "http") + "/p/preview/ws"
	wsConn, err := DialWS(env.ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("DialWS /p/preview/ws: %v", err)
	}
	defer wsConn.Close()
	if err := wsConn.WriteFrame([]byte("ping-ws")); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	reply, err := wsConn.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if string(reply) != "echo:ping-ws" {
		t.Fatalf("WS proxy reply = %q, want %q", string(reply), "echo:ping-ws")
	}

	// 4. Test exposed port listener directly.
	expResp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/hello?via=exposed", exposePort))
	if err != nil {
		t.Fatalf("GET exposed port: %v", err)
	}
	defer expResp.Body.Close()
	var expEcho map[string]any
	_ = json.NewDecoder(expResp.Body).Decode(&expEcho)
	if expEcho["query"] != "via=exposed" {
		t.Fatalf("unexpected exposed port response: %+v", expEcho)
	}

	// 5. Register external URL page link and test redirect + validation.
	var extPage gateway.Page
	if code := env.postJSON("/api/v1/pages", map[string]any{
		"slug":  "ext-docs",
		"title": "External Docs",
		"url":   "https://example.com/docs",
	}, &extPage); code != http.StatusOK {
		t.Fatalf("register external page status = %d", code)
	}
	noRedirectClient := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	extResp, err := noRedirectClient.Get(env.httpServer.URL + "/p/ext-docs/")
	if err != nil {
		t.Fatalf("GET /p/ext-docs/: %v", err)
	}
	defer extResp.Body.Close()
	if extResp.StatusCode != http.StatusFound {
		t.Fatalf("GET /p/ext-docs/ status = %d, want 302", extResp.StatusCode)
	}
	if loc := extResp.Header.Get("Location"); loc != "https://example.com/docs" {
		t.Fatalf("Location = %q, want https://example.com/docs", loc)
	}

	// Reject non-http(s) URL schemes.
	if code := env.postJSON("/api/v1/pages", map[string]any{
		"slug": "bad-url",
		"url":  "javascript:alert(1)",
	}, nil); code == http.StatusOK {
		t.Fatal("expected javascript: URL to be rejected")
	}

	// 6. Unregister preview page and verify /p/preview/ returns 404.
	delReq, _ := http.NewRequest("DELETE", env.httpServer.URL+"/api/v1/pages/preview", nil)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("DELETE /api/v1/pages/preview: %v", err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE status = %d", delResp.StatusCode)
	}

	afterResp, err := http.Get(env.httpServer.URL + "/p/preview/hello")
	if err != nil {
		t.Fatal(err)
	}
	afterResp.Body.Close()
	if afterResp.StatusCode != http.StatusNotFound {
		t.Fatalf("after unregister status = %d, want 404", afterResp.StatusCode)
	}
}

func TestSecretRequestWritesFileWithoutContextLeak(t *testing.T) {
	bin := buildFakeACP(t)
	env := newTestEnv(t)

	// Register and connect an agent via WebSocket `/connect` (`silo connect --url ws://...`).
	var reg gateway.RegisterResult
	if code := env.postJSON("/api/v1/agents", map[string]any{
		"name": "sec-agent",
		"kind": "container",
	}, &reg); code != http.StatusOK {
		t.Fatalf("register sec-agent status = %d", code)
	}

	agentHome := filepath.Join(env.dir, "agent-home")
	connectCtx, connectCancel := context.WithCancel(env.ctx)
	defer connectCancel()
	wsConnectURL := "ws" + strings.TrimPrefix(env.httpServer.URL, "http") + "/connect"
	go func() {
		_ = RunConnect(connectCtx, ConnectOptions{
			URL:     wsConnectURL,
			Token:   reg.Token,
			HomeDir: agentHome,
			Command: []string{bin},
		})
	}()

	waitFor(t, "sec-agent connected", func() bool {
		var info gateway.AgentInfo
		if err := NewClient(env.brSock).Call(env.ctx, "agent.status", map[string]any{"agent": "sec-agent"}, &info); err != nil {
			return false
		}
		return info.State == gateway.StateConnected
	})

	// Request a secret via `silo-mcp` (`request_secret` tool).
	const secretValue = "sk_live_ULTRA_SECRET_DO_NOT_LEAK_777"
	secretResCh := make(chan struct {
		out map[string]any
		err error
	}, 1)
	go func() {
		raw, err := callMCPTool(env.ctx, NewClient(env.brSock), reg.Token, "request_secret", map[string]any{
			"name":   "stripe-api-key",
			"reason": "Need Stripe API key to run billing integration tests",
		})
		var out map[string]any
		if rawBytes, ok := raw.(json.RawMessage); ok {
			_ = json.Unmarshal(rawBytes, &out)
		}
		secretResCh <- struct {
			out map[string]any
			err error
		}{out: out, err: err}
	}()

	// Wait for the pending secret request to appear in the bridge.
	var secID string
	waitFor(t, "pending secret request", func() bool {
		var list []gateway.SecretRequest
		env.getJSON("/api/v1/secrets?pending=true", &list)
		for _, sr := range list {
			if sr.Agent == "sec-agent" && sr.Name == "stripe-api-key" && sr.State == gateway.SecretPending {
				secID = sr.ID
				return true
			}
		}
		return false
	})

	// Human provides the secret via POST /api/v1/secrets/:id/provide.
	var provideResp map[string]any
	if code := env.postJSON("/api/v1/secrets/"+secID+"/provide", map[string]any{
		"value": secretValue,
	}, &provideResp); code != http.StatusOK {
		t.Fatalf("provide secret status = %d", code)
	}

	// Verify the MCP tool call returns `{status: "stored", name, path}` and NEVER the secret value.
	var toolOut map[string]any
	select {
	case res := <-secretResCh:
		if res.err != nil {
			t.Fatalf("request_secret failed: %v", res.err)
		}
		toolOut = res.out
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for request_secret to complete")
	}

	rawToolOut, _ := json.Marshal(toolOut)
	if strings.Contains(string(rawToolOut), secretValue) {
		t.Fatalf("LEAK: secret value appeared in MCP tool output: %s", rawToolOut)
	}
	if toolOut["status"] != "stored" || toolOut["name"] != "stripe-api-key" {
		t.Fatalf("unexpected request_secret output: %+v", toolOut)
	}

	wantPath := filepath.Join(agentHome, ".config", "silo", "secrets", "stripe-api-key")
	if toolOut["path"] != wantPath {
		t.Fatalf("secret path = %v, want %q", toolOut["path"], wantPath)
	}

	// Verify the file exists on disk with 0600 permissions and exact bytes.
	st, err := os.Stat(wantPath)
	if err != nil {
		t.Fatalf("Stat secret file: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("secret file permissions = %04o, want 0600", perm)
	}
	fileBytes, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("ReadFile secret: %v", err)
	}
	if string(fileBytes) != secretValue {
		t.Fatalf("secret file content = %q, want %q", string(fileBytes), secretValue)
	}

	// Verify the secret value is NOT present in state.json, audit.jsonl, or session transcripts.
	for _, checkFile := range []string{
		filepath.Join(env.gwCfg.StateDir, "state.json"),
		env.gwCfg.AuditLog,
		filepath.Join(env.sessDir, "sec-agent", "transcript.jsonl"),
	} {
		data, err := os.ReadFile(checkFile)
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("ReadFile %s: %v", checkFile, err)
		}
		if strings.Contains(string(data), secretValue) {
			t.Fatalf("LEAK: secret value found in %s", checkFile)
		}
	}
}

func TestMCPServerToolsAndReadAgentVisibility(t *testing.T) {
	bin := buildFakeACP(t)
	env := newTestEnv(t)

	// 1. Verify `tools/list` via ServeMCP exposes the 10 tools and NO `send_message` / `message_agent`.
	var inBuf, outBuf bytes.Buffer
	enc := json.NewEncoder(&inBuf)
	_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})

	if err := ServeMCP(env.ctx, &inBuf, &outBuf, NewClient(env.brSock), ""); err != nil {
		t.Fatalf("ServeMCP: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(outBuf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 MCP responses (initialize + tools/list), got %d: %s", len(lines), outBuf.String())
	}
	var listResp struct {
		Result struct {
			Tools []MCPTool `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &listResp); err != nil {
		t.Fatalf("Unmarshal tools/list: %v", err)
	}
	toolNames := map[string]bool{}
	for _, tl := range listResp.Result.Tools {
		toolNames[tl.Name] = true
		if tl.Name == "send_message" || tl.Name == "message_agent" {
			t.Fatalf("forbidden inter-agent messaging tool %q exposed in silo-mcp", tl.Name)
		}
	}
	for _, want := range []string{
		"register_agent", "list_agents", "read_agent", "stop_agent",
		"register_page", "unregister_page", "list_pages",
		"request_secret", "file_request", "report",
	} {
		if !toolNames[want] {
			t.Fatalf("missing expected MCP tool %q", want)
		}
	}

	// 2. Test `report`, `file_request`, and `read_agent` visibility rules (non-root vs root).
	// Connect standing `root` agent with its seeded token file so we can call read_agent as root.
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

	var childReg gateway.RegisterResult
	if code := env.postJSON("/api/v1/agents", map[string]any{
		"name":   "child-worker",
		"parent": gateway.RootAgentID,
		"kind":   "process",
		"brief":  "no-perm build widget",
	}, &childReg); code != http.StatusOK {
		t.Fatalf("register child-worker status = %d", code)
	}

	connectCtx, connectCancel := context.WithCancel(env.ctx)
	defer connectCancel()
	go func() {
		_ = RunConnect(connectCtx, ConnectOptions{
			SocketPath: env.brSock,
			Token:      childReg.Token,
			HomeDir:    env.dir,
			Command:    []string{bin},
		})
	}()

	waitFor(t, "child-worker brief complete", func() bool {
		lines, err := env.br.ReadTranscript("child-worker", 0)
		if err != nil {
			return false
		}
		for _, l := range lines {
			if strings.Contains(string(l.Message), "completed: no-perm build widget") {
				return true
			}
		}
		return false
	})

	// Have child-worker call `report` and `file_request` via MCP.
	if _, err := callMCPTool(env.ctx, NewClient(env.brSock), childReg.Token, "report", map[string]any{
		"status":    "done",
		"summary":   "Widget compiled and tested",
		"exit_code": 0,
	}); err != nil {
		t.Fatalf("MCP report: %v", err)
	}
	if _, err := callMCPTool(env.ctx, NewClient(env.brSock), childReg.Token, "file_request", map[string]any{
		"summary":  "Install libpq-dev",
		"details":  "Needed for postgres driver compilation",
		"commands": []string{"apt-get install -y libpq-dev"},
	}); err != nil {
		t.Fatalf("MCP file_request: %v", err)
	}

	// Read child-worker as a non-root agent (`child-worker` itself): must include transcript!
	rawNonRoot, err := callMCPTool(env.ctx, NewClient(env.brSock), childReg.Token, "read_agent", map[string]any{
		"agent": "child-worker",
	})
	if err != nil {
		t.Fatalf("MCP read_agent (non-root): %v", err)
	}
	var nonRootView map[string]any
	_ = json.Unmarshal(rawNonRoot.(json.RawMessage), &nonRootView)
	repMap, _ := nonRootView["report"].(map[string]any)
	if repMap == nil || repMap["summary"] != "Widget compiled and tested" {
		t.Fatalf("missing report in read_agent: %+v", nonRootView)
	}
	txSlice, _ := nonRootView["transcript"].([]any)
	if len(txSlice) == 0 {
		t.Fatalf("expected non-root read_agent to include session transcript, got: %+v", nonRootView)
	}

	// Read child-worker as `root`: must receive structured status/report ONLY (no raw transcript).
	rawRoot, err := callMCPTool(env.ctx, NewClient(env.brSock), rootConn.SessionToken, "read_agent", map[string]any{
		"agent": "child-worker",
	})
	if err != nil {
		t.Fatalf("root read_agent: %v", err)
	}
	var rootView map[string]any
	_ = json.Unmarshal(rawRoot.(json.RawMessage), &rootView)
	rootRep, _ := rootView["report"].(map[string]any)
	if rootRep == nil || rootRep["summary"] != "Widget compiled and tested" {
		t.Fatalf("expected root to see structured report, got: %+v", rootView)
	}
	if _, hasTx := rootView["transcript"]; hasTx {
		t.Fatalf("expected root read_agent to omit raw transcript, got: %+v", rootView["transcript"])
	}
}
