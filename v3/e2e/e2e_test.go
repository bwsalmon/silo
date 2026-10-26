//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/v3/bridge"
)

const (
	rootAgent = "root"
	userAgent = "main"
	childName = "e2e-child"
)

// stack is a running silo: where its UI is and how to log in.
type stack struct {
	url     string // http://host:port
	token   string // raw UI login token
	runtime string // the standing agents' CLI preset
	// marker is a path the root agent's batch writes; it must not exist yet.
	marker string
}

func realCLI() string { return os.Getenv("SILO_E2E_REAL_CLI") }

func TestMilestone1(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("the gateway runs as root: sudo -E go test -tags e2e ./e2e")
	}
	if cli := realCLI(); cli != "" && cli != "gemini" {
		t.Fatalf("SILO_E2E_REAL_CLI=%q: only gemini is supported", cli)
	}
	var s *stack
	if os.Getenv("SILO_E2E_INSTALL") == "1" {
		s = installStack(t)
	} else {
		s = isolatedStack(t)
	}
	c := login(t, s)

	t.Run("standing agents connected", func(t *testing.T) {
		root := c.waitAgent(t, rootAgent, "connected")
		user := c.waitAgent(t, userAgent, "connected")
		if root.User != "silo-root" || !root.Root || !root.Standing {
			t.Errorf("root agent %+v: want standing root agent as silo-root", root)
		}
		if user.User != "silo-user" || user.Parent != rootAgent || !user.Standing || user.Root {
			t.Errorf("user agent %+v: want standing child of root as silo-user", user)
		}
		for _, a := range []agentInfo{root, user} {
			if a.Permissions != "ask" || a.Runtime != s.runtime {
				t.Errorf("agent %s: permissions %q runtime %q, want ask and %s", a.ID, a.Permissions, a.Runtime, s.runtime)
			}
		}
	})

	t.Run("chat with the user agent", func(t *testing.T) {
		msg := "hello from the e2e test"
		if realCLI() != "" {
			msg = "Reply with exactly the words: hello from the e2e test"
		}
		stream := c.subscribe(t, userAgent)
		res := c.prompt(t, userAgent, msg)
		if res.StopReason != "end_turn" {
			t.Fatalf("stopReason %q", res.StopReason)
		}
		got := stream.waitText(t, "hello from the e2e test")
		t.Logf("streamed reply: %q", got)
	})

	t.Run("root agent batch approved, run and audited", func(t *testing.T) {
		content := "written by the silo e2e test\n"
		batch := map[string]any{"batch": map[string]any{
			"title":  "e2e: write a marker file",
			"reason": "The milestone-1 end-to-end test checks that an approved batch runs as root.",
			"files": []map[string]any{{
				"path": s.marker, "mode": "0644",
				"content_b64": b64(content),
			}},
			"steps": []map[string]any{{"argv": []string{"/usr/bin/test", "-f", s.marker}}},
		}}
		args, _ := json.Marshal(batch)
		text := "mcp run_as_root " + string(args)
		if realCLI() != "" {
			text = "Call the silo MCP tool run_as_root once, with exactly these arguments, and then report the batch id: " + string(args)
			go c.autoApprovePermissions(t, rootAgent, 3*time.Minute)
		}
		done := c.promptAsync(t, rootAgent, text)

		b := c.waitPendingBatch(t, "e2e: write a marker file")
		if _, err := os.Stat(s.marker); err == nil {
			t.Fatalf("%s exists before approval", s.marker)
		}
		var detail batchRecord
		c.do(t, "GET", "/api/v1/approvals/"+b.BatchID, nil, &detail)
		if detail.Status != "pending" || detail.Hash == "" {
			t.Fatalf("batch before approval: %+v", detail)
		}
		c.do(t, "POST", "/api/v1/approvals/"+b.BatchID+"/decision",
			map[string]any{"decision": "approve", "comment": "e2e", "wait_s": 60}, &detail)
		detail = c.waitBatch(t, b.BatchID)
		if detail.Status != "succeeded" {
			t.Fatalf("batch %s: %s (%s)", b.BatchID, detail.Status, detail.Error)
		}
		got, err := os.ReadFile(s.marker)
		if err != nil || string(got) != content {
			t.Fatalf("%s after the batch: %q, %v", s.marker, got, err)
		}
		if r := <-done; r.err != nil {
			t.Fatalf("root prompt: %v", r.err)
		}

		// The audit log has the batch from submission to its end, and its
		// hash chain verifies.
		var entries []auditEntry
		c.do(t, "GET", "/api/v1/audit?limit=1000", nil, &entries)
		seen := map[string]bool{}
		for _, e := range entries {
			if bytes.Contains(e.Payload, []byte(b.BatchID)) {
				seen[e.Event] = true
			}
		}
		for _, ev := range []string{"batch.submit", "batch.decide", "batch.exec.start", "batch.exec.file", "batch.exec.step", "batch.exec.end"} {
			if !seen[ev] {
				t.Errorf("audit log has no %s for %s (saw %v)", ev, b.BatchID, seen)
			}
		}
		var v map[string]any
		c.do(t, "GET", "/api/v1/audit/verify", nil, &v)
		if ok, _ := v["ok"].(bool); !ok {
			t.Errorf("audit verify: %v", v)
		}
	})

	t.Run("child agent registered, connected, chatted with and stopped", func(t *testing.T) {
		reg := map[string]any{
			"name":     childName,
			"location": map[string]any{"kind": "subprocess", "detail": "started by main with silo connect"},
			"brief":    "",
		}
		args, _ := json.Marshal(reg)
		text := "spawn " + string(args)
		if realCLI() != "" {
			text = "First call the silo MCP tool register_agent with exactly these arguments: " + string(args) +
				". It returns a token. Then run this shell command, with TOKEN replaced by that token: " +
				"nohup silo connect --token TOKEN -- gemini --experimental-acp >/dev/null 2>&1 &"
			go c.autoApprovePermissions(t, userAgent, 3*time.Minute)
		}
		done := c.promptAsync(t, userAgent, text)

		// Under the default permissions "ask", the registration waits for
		// the human.
		p := c.waitPermission(t, "register_agent", childName)
		if p.Agent != userAgent {
			t.Errorf("registration requested by %q, want %s", p.Agent, userAgent)
		}
		c.do(t, "POST", "/api/v1/permissions/"+p.ID+"/decision", map[string]any{"decision": "approve"}, nil)
		if r := <-done; r.err != nil {
			t.Fatalf("spawn prompt: %v", r.err)
		}

		child := c.waitAgent(t, childName, "connected")
		if child.Parent != userAgent || child.Location.Kind != "subprocess" {
			t.Errorf("child %+v: want a subprocess child of %s", child, userAgent)
		}
		msg := "hello child"
		if realCLI() != "" {
			msg = "Reply with exactly the words: hello child"
		}
		stream := c.subscribe(t, childName)
		if res := c.prompt(t, childName, msg); res.StopReason != "end_turn" {
			t.Fatalf("child stopReason %q", res.StopReason)
		}
		stream.waitText(t, "hello child")

		c.do(t, "POST", "/api/v1/agents/"+childName+"/stop", map[string]any{"mode": "graceful"}, nil)
		c.waitAgent(t, childName, "stopped")
		// Its silo connect hangs up, so the bridge's session ends.
		deadline := time.Now().Add(45 * time.Second)
		for {
			var sessions []struct {
				Agent string `json:"agent"`
				State string `json:"state"`
			}
			c.do(t, "GET", "/api/v1/sessions?agent="+childName, nil, &sessions)
			live := false
			for _, ss := range sessions {
				if ss.State == "ready" || ss.State == "busy" || ss.State == "connecting" {
					live = true
				}
			}
			if !live {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("child session still live after stop: %+v", sessions)
			}
			time.Sleep(500 * time.Millisecond)
		}
		// The standing agents are untouched.
		c.waitAgent(t, userAgent, "connected")
		c.waitAgent(t, rootAgent, "connected")
	})
}

// ---- bringing the stack up -------------------------------------------------

func build(t *testing.T, dir string) (silo, fake string) {
	t.Helper()
	silo, fake = filepath.Join(dir, "silo"), filepath.Join(dir, "fakeagent")
	for out, pkg := range map[string]string{silo: "../cmd/silo", fake: "./fakeagent"} {
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build %s: %v\n%s", pkg, err, b)
		}
	}
	return silo, fake
}

// installStack runs deploy/install.sh on this host.
func installStack(t *testing.T) *stack {
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		t.Fatal("SILO_E2E_INSTALL=1 needs a host running systemd")
	}
	bin := t.TempDir()
	_, fake := build(t, bin)
	runtime := "fake"
	args := []string{"../deploy/install.sh", "--source", ".."}
	if cli := realCLI(); cli != "" {
		runtime = cli
		key := os.Getenv("GEMINI_API_KEY")
		if key == "" {
			t.Fatal("SILO_E2E_REAL_CLI=gemini needs GEMINI_API_KEY")
		}
		// Until the credential proxy lands, hand the key to the agents'
		// units from a root-only file.
		mustWrite(t, "/etc/silo/secrets/e2e-gemini.env", "GEMINI_API_KEY="+key+"\n", 0o600)
		for _, u := range []string{"silo-root", "silo-user"} {
			mustWrite(t, "/etc/systemd/system/"+u+".service.d/e2e.conf",
				"[Service]\nEnvironmentFile=/etc/silo/secrets/e2e-gemini.env\n", 0o644)
		}
	} else {
		args = append(args, "--no-cli")
		if err := os.MkdirAll("/usr/local/lib/silo-e2e", 0o755); err != nil {
			t.Fatal(err)
		}
		copyFile(t, fake, "/usr/local/lib/silo-e2e/fakeagent", 0o755)
		mustWrite(t, "/etc/silo/clis.d/fake.toml",
			"name = \"fake\"\ncommand = [\"/usr/local/lib/silo-e2e/fakeagent\"]\n", 0o644)
	}
	args = append(args, "--runtime", runtime)
	cmd := exec.Command("bash", args...)
	if g, err := exec.LookPath("go"); err == nil {
		cmd.Env = append(os.Environ(), "GO="+g)
	}
	out, err := cmd.CombinedOutput()
	t.Logf("install.sh:\n%s", out)
	if err != nil {
		t.Fatalf("install.sh: %v", err)
	}
	if os.Getenv("SILO_E2E_UNINSTALL") == "1" {
		t.Cleanup(func() {
			out, err := exec.Command("bash", "../deploy/uninstall.sh", "--purge").CombinedOutput()
			t.Logf("uninstall.sh: %v\n%s", err, out)
		})
	}
	tok, err := os.ReadFile("/etc/silo/ui-token")
	if err != nil {
		t.Fatal(err)
	}
	marker := "/var/lib/silo-e2e-marker"
	os.Remove(marker)
	t.Cleanup(func() { os.Remove(marker) })
	return &stack{url: "http://127.0.0.1:7070", token: strings.TrimSpace(string(tok)), runtime: runtime, marker: marker}
}

// isolatedStack starts the gateway, the bridge and UI, and both standing
// agents as processes, with every path under one temporary directory.
func isolatedStack(t *testing.T) *stack {
	dir, err := os.MkdirTemp("/var/tmp", "silo-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !t.Failed() {
			os.RemoveAll(dir)
		} else {
			t.Logf("left %s for inspection", dir)
		}
	})
	for _, d := range []string{"bin", "etc/clis.d", "run", "state", "log", "sessions", "ui", "home/root", "home/main", "share"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	silo, fake := build(t, filepath.Join(dir, "bin"))
	runtime := "fake"
	if cli := realCLI(); cli != "" {
		runtime = cli
		if os.Getenv("GEMINI_API_KEY") == "" {
			t.Fatal("SILO_E2E_REAL_CLI=gemini needs GEMINI_API_KEY")
		}
	}
	mustWrite(t, filepath.Join(dir, "etc/clis.d/fake.toml"), fmt.Sprintf("name = \"fake\"\ncommand = [%q]\n", fake), 0o644)
	gwSock, brSock := filepath.Join(dir, "run/gateway.sock"), filepath.Join(dir, "run/bridge.sock")
	cfg := filepath.Join(dir, "etc/gateway.toml")
	mustWrite(t, cfg, fmt.Sprintf(`socket = %q
socket_group = "root"
state_dir = %q
audit_log = %q
hub_url = "unix://%s"
bridge_user = "root"
reconcile_interval_s = 2

[root_agent]
runtime = %q

[user_agent]
runtime = %q

[run_as_root]
shipped_dir = %q
`, gwSock, filepath.Join(dir, "state"), filepath.Join(dir, "log/audit.jsonl"), brSock, runtime, runtime,
		filepath.Join(dir, "share")), 0o644)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	start := func(name string, env []string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, silo, args...)
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
		cmd.WaitDelay = 20 * time.Second
		cmd.Env = append(os.Environ(), env...)
		f, err := os.OpenFile(filepath.Join(dir, "log", name+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdout, cmd.Stderr = f, f
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	gw := start("gateway", nil, "gateway", "-config", cfg)
	wg.Add(1)
	go func() { defer wg.Done(); gw.Wait() }()
	waitFor(t, "the gateway socket", func() bool { return dial(gwSock) })

	out, err := exec.Command(silo, "ui", "token", "-file", filepath.Join(dir, "ui/token.sha256")).Output()
	if err != nil {
		t.Fatal(err)
	}
	addr := freeAddr(t)
	ui := start("ui", nil, "ui", "-http", addr, "-gateway-socket", gwSock, "-socket", brSock,
		"-sessions-dir", filepath.Join(dir, "sessions"), "-token-file", filepath.Join(dir, "ui/token.sha256"))
	wg.Add(1)
	go func() { defer wg.Done(); ui.Wait() }()
	waitFor(t, "the bridge socket", func() bool { return dial(brSock) })

	// What systemd does for silo-root.service and silo-user.service:
	// silo connect with the token the gateway wrote, restarted whenever it
	// exits, for as long as the stack runs.
	for _, id := range []string{rootAgent, userAgent} {
		id := id
		wg.Add(1)
		go func() {
			defer wg.Done()
			tokFile := filepath.Join(dir, "state/tokens", id)
			for ctx.Err() == nil {
				tok, err := os.ReadFile(tokFile)
				if err != nil {
					select {
					case <-ctx.Done():
					case <-time.After(300 * time.Millisecond):
					}
					continue
				}
				cmd := start("agent-"+id, []string{"HOME=" + filepath.Join(dir, "home", id), "SILO_HUB_URL=unix://" + brSock},
					"connect", "--socket", brSock, "--clis-dir", filepath.Join(dir, "etc/clis.d"), "--token", strings.TrimSpace(string(tok)))
				cmd.Wait()
				select {
				case <-ctx.Done():
				case <-time.After(time.Second):
				}
			}
		}()
	}
	return &stack{url: "http://" + addr, token: strings.TrimSpace(string(out)), runtime: runtime,
		marker: filepath.Join(dir, "marker.txt")}
}

// ---- the UI API client -------------------------------------------------------

type client struct {
	base, csrf string
	hc         *http.Client
	cookie     string
}

func login(t *testing.T, s *stack) *client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &client{base: s.url, hc: &http.Client{Jar: jar, Timeout: 5 * time.Minute}}
	waitFor(t, "the UI", func() bool {
		r, err := c.hc.Get(s.url + "/api/v1/auth/session")
		if err == nil {
			r.Body.Close()
		}
		return err == nil
	})
	var res struct {
		CSRF string `json:"csrf_token"`
	}
	c.do(t, "POST", "/api/v1/auth/login", map[string]any{"token": s.token}, &res)
	c.csrf = res.CSRF
	u, _ := http.NewRequest("GET", s.url+"/acp/", nil)
	for _, ck := range jar.Cookies(u.URL) {
		c.cookie = ck.Name + "=" + ck.Value
	}
	if c.cookie == "" {
		t.Fatal("login set no session cookie")
	}
	return c
}

func (c *client) call(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.csrf != "" {
		req.Header.Set("X-Silo-CSRF", c.csrf)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s %s: %v: %s", method, path, err, data)
		}
	}
	return nil
}

func (c *client) do(t *testing.T, method, path string, body, out any) {
	t.Helper()
	if err := c.call(method, path, body, out); err != nil {
		t.Fatal(err)
	}
}

type agentInfo struct {
	ID          string `json:"id"`
	Root        bool   `json:"root"`
	Standing    bool   `json:"standing"`
	User        string `json:"user"`
	Parent      string `json:"parent"`
	Runtime     string `json:"runtime"`
	Permissions string `json:"permissions"`
	State       string `json:"state"`
	Location    struct {
		Kind string `json:"kind"`
	} `json:"location"`
}

func (c *client) waitAgent(t *testing.T, id, state string) agentInfo {
	t.Helper()
	var a agentInfo
	waitFor(t, "agent "+id+" "+state, func() bool {
		a = agentInfo{}
		return c.call("GET", "/api/v1/agents/"+id, nil, &a) == nil && a.State == state
	})
	return a
}

type promptResult struct {
	StopReason string `json:"stopReason"`
}

func (c *client) prompt(t *testing.T, agent, text string) promptResult {
	t.Helper()
	var r promptResult
	c.do(t, "POST", "/api/v1/sessions/"+agent+"/prompt", map[string]any{"text": text}, &r)
	return r
}

type asyncResult struct {
	res promptResult
	err error
}

func (c *client) promptAsync(t *testing.T, agent, text string) <-chan asyncResult {
	ch := make(chan asyncResult, 1)
	go func() {
		var r asyncResult
		r.err = c.call("POST", "/api/v1/sessions/"+agent+"/prompt", map[string]any{"text": text}, &r.res)
		ch <- r
	}()
	return ch
}

// stream collects the agent message text a session streams over the ACP
// WebSocket.
type stream struct {
	mu   sync.Mutex
	text strings.Builder
}

func (c *client) subscribe(t *testing.T, agent string) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ws, err := bridge.DialWS(ctx, strings.Replace(c.base, "http", "ws", 1)+"/acp/v1/sessions/"+agent+"?history=false",
		map[string]string{"Cookie": c.cookie})
	if err != nil {
		t.Fatalf("subscribe %s: %v", agent, err)
	}
	t.Cleanup(func() { ws.Close() })
	s := &stream{}
	go func() {
		for {
			b, err := ws.ReadFrame()
			if err != nil {
				return
			}
			var f struct {
				Method string `json:"method"`
				Params struct {
					Update struct {
						Kind    string `json:"sessionUpdate"`
						Content struct {
							Text string `json:"text"`
						} `json:"content"`
					} `json:"update"`
				} `json:"params"`
			}
			if json.Unmarshal(b, &f) == nil && f.Method == "session/update" && f.Params.Update.Kind == "agent_message_chunk" {
				s.mu.Lock()
				s.text.WriteString(f.Params.Update.Content.Text)
				s.mu.Unlock()
			}
		}
	}()
	return s
}

func (s *stream) waitText(t *testing.T, want string) string {
	t.Helper()
	var got string
	waitFor(t, fmt.Sprintf("streamed text %q", want), func() bool {
		s.mu.Lock()
		got = s.text.String()
		s.mu.Unlock()
		return strings.Contains(strings.ToLower(got), strings.ToLower(want))
	})
	return got
}

type batchRecord struct {
	ID      string `json:"id"`
	BatchID string `json:"batch_id"`
	Hash    string `json:"hash"`
	Title   string `json:"title"`
	Status  string `json:"status"`
	Error   string `json:"error"`
}

func (c *client) waitPendingBatch(t *testing.T, title string) batchRecord {
	t.Helper()
	var found batchRecord
	waitFor(t, "a pending batch "+title, func() bool {
		var list []batchRecord
		if c.call("GET", "/api/v1/approvals", nil, &list) != nil {
			return false
		}
		for _, b := range list {
			if b.Title == title && b.Status == "pending" {
				found = b
				return true
			}
		}
		return false
	})
	return found
}

func (c *client) waitBatch(t *testing.T, id string) batchRecord {
	t.Helper()
	var r batchRecord
	waitFor(t, "batch "+id+" to finish", func() bool {
		r = batchRecord{}
		if c.call("GET", "/api/v1/approvals/"+id, nil, &r) != nil {
			return false
		}
		return r.Status != "pending" && r.Status != "approved" && r.Status != "running"
	})
	return r
}

type permission struct {
	ID          string `json:"id"`
	Agent       string `json:"agent"`
	Kind        string `json:"kind"`
	TargetAgent string `json:"target_agent"`
	State       string `json:"state"`
}

func (c *client) waitPermission(t *testing.T, kind, target string) permission {
	t.Helper()
	var found permission
	waitFor(t, kind+" "+target+" awaiting approval", func() bool {
		var list []permission
		if c.call("GET", "/api/v1/permissions?pending=true", nil, &list) != nil {
			return false
		}
		for _, p := range list {
			if p.Kind == kind && p.TargetAgent == target && p.State == "pending" {
				found = p
				return true
			}
		}
		return false
	})
	return found
}

// autoApprovePermissions approves the real CLI's own tool-permission
// prompts (ACP session/request_permission) for agent, as the human would.
func (c *client) autoApprovePermissions(t *testing.T, agent string, d time.Duration) {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		var list []permission
		if c.call("GET", "/api/v1/permissions?pending=true&agent="+agent, nil, &list) == nil {
			for _, p := range list {
				if p.Kind == "acp" {
					c.call("POST", "/api/v1/permissions/"+p.ID+"/decision", map[string]any{"decision": "approve"}, nil)
				}
			}
		}
		time.Sleep(time.Second)
	}
}

type auditEntry struct {
	Seq     int64           `json:"seq"`
	Event   string          `json:"event"`
	Payload json.RawMessage `json:"payload"`
}

// ---- helpers -------------------------------------------------------------

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	timeout := 60 * time.Second
	if realCLI() != "" {
		timeout = 5 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func dial(sock string) bool {
	c, err := net.Dial("unix", sock)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func mustWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	os.Chmod(path, mode)
}

func copyFile(t *testing.T, src, dst string, mode os.FileMode) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	tmp := dst + ".new"
	mustWrite(t, tmp, string(b), mode)
	if err := os.Rename(tmp, dst); err != nil {
		t.Fatal(err)
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
