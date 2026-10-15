// Adapted from github.com/bwsalmon/grain pkg/agent/codex/isolation_live_test.go at d6cd9d71f9b46df18d47e5305f84b5471ce93047, Apache License 2.0.

package codex_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/v2/agentcli"
	"github.com/bwsalmon/silo/v2/agentcli/codex"
	"github.com/bwsalmon/silo/v2/location"
	"github.com/bwsalmon/silo/v2/silofile"
)

// nativeTools are the environment-backed tools grain's 0.153.0 review
// found the no-environment manifest removes (grain
// docs/codex-isolation.md).
var nativeTools = []string{"exec_command", "shell", "shell_command", "apply_patch", "view_image", "write_stdin", "request_permissions"}

// TestLiveCodexNoneIsolation runs the real codex binary with silo's
// remote_permissions: none configuration against a stub Responses API
// and a stub MCP server, so no model or credential is involved. Like
// grain's probe, the stub model calls every native tool whether or not
// it was advertised: leaving a tool out of the prompt is not a boundary,
// only the dispatcher refusing it is. It logs the tool surface codex
// advertised, which is what a reviewer compares across versions
// (cli-mapping.md §4, assumption 5).
//
// It builds the plan as for a reviewed version whatever the binary is,
// since reviewing an unreviewed binary is its point. SILO_V2_CODEX
// names the binary; codex on PATH otherwise.
//
//	SILO_V2_LIVE_CLI=1 go test ./agentcli/codex -run Live -v
func TestLiveCodexNoneIsolation(t *testing.T) {
	if os.Getenv("SILO_V2_LIVE_CLI") == "" {
		t.Skip("set SILO_V2_LIVE_CLI=1 to run the real codex binary")
	}
	binary := os.Getenv("SILO_V2_CODEX")
	if binary == "" {
		var err error
		if binary, err = exec.LookPath("codex"); err != nil {
			t.Skip("no codex on PATH")
		}
	}
	out, _ := exec.Command(binary, "--version").Output()
	t.Logf("binary: %s", strings.TrimSpace(string(out)))

	mcpCalls := 0
	var mu sync.Mutex
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil || msg.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "silo", "version": "test"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "probe", "description": "The one gateway tool.", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			mu.Lock()
			mcpCalls++
			mu.Unlock()
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "MCP_OK"}}}
		default:
			result = map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
	}))
	defer mcp.Close()

	canary := filepath.Join(t.TempDir(), "native-must-not-write")
	var requests []map[string]any
	needDiscovery := false
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			http.NotFound(w, r)
			return
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, request)
		round := len(requests)
		mu.Unlock()
		mcpCall := func() map[string]any {
			name, namespace := findTool(request, "probe", "")
			if name == "" {
				t.Error("MCP probe tool not found")
			}
			return map[string]any{"type": "function_call", "name": name, "namespace": namespace, "call_id": "mcp_probe", "arguments": `{}`}
		}
		var items []map[string]any
		switch {
		case round == 1:
			for _, name := range nativeTools {
				if name == "apply_patch" {
					continue
				}
				args := map[string]any{"cmd": "touch " + canary, "command": "touch " + canary, "path": canary, "session_id": 1}
				if name == "shell" {
					args["command"] = []string{"sh", "-c", "touch " + canary}
				}
				encoded, _ := json.Marshal(args)
				items = append(items, map[string]any{"type": "function_call", "name": name, "namespace": "functions", "call_id": "native_" + name, "arguments": string(encoded)})
			}
			items = append(items, map[string]any{"type": "custom_tool_call", "name": "apply_patch", "namespace": "functions", "call_id": "native_apply_patch", "input": "*** Begin Patch\n*** Add File: " + canary + "\n+bad\n*** End Patch"})
			serialized, _ := json.Marshal(request["tools"])
			needDiscovery = strings.Contains(string(serialized), `"type":"tool_search"`)
			if needDiscovery {
				items = append(items, map[string]any{"type": "tool_search_call", "call_id": "find_mcp", "execution": "client", "arguments": map[string]any{"query": "silo probe", "limit": 1}})
			} else {
				items = append(items, mcpCall())
			}
		case round == 2 && needDiscovery:
			items = append(items, mcpCall())
		default:
			items = append(items, map[string]any{"type": "message", "role": "assistant", "id": "msg_done", "content": []any{map[string]any{"type": "output_text", "text": "done"}}})
		}
		w.Header().Set("Content-Type", "text/event-stream")
		write := func(event map[string]any) {
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
		}
		id := fmt.Sprint("response", round)
		write(map[string]any{"type": "response.created", "response": map[string]any{"id": id}})
		for _, item := range items {
			write(map[string]any{"type": "response.output_item.done", "item": item})
		}
		write(map[string]any{"type": "response.completed", "response": map[string]any{"id": id}})
	}))
	defer stub.Close()

	plan, err := (codex.Mapping{}).Build(agentcli.Request{
		Agent:       silofile.Agent{CLI: "codex", Model: "gpt-5.5", Auth: silofile.Auth{Credential: "stub"}},
		Mode:        agentcli.Headless,
		Permissions: silofile.RemotePermissions{Mode: silofile.PermNone},
		Prompt:      "Exercise the tools.",
		GatewayURL:  mcp.URL + "/mcp", GatewayToken: "TOKEN", GatewayTools: []string{"probe"},
		Host: location.HostInfo{Binary: binary, Version: codex.ReviewedVersions[0],
			Env: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}},
		Env: map[string]string{codex.APIKeyVar: "stub-key-not-a-real-credential"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Outside /tmp, where codex refuses to put its helper binaries.
	dir, err := os.MkdirTemp(".", "live-codex-")
	if err != nil {
		t.Fatal(err)
	}
	dir, _ = filepath.Abs(dir)
	defer os.RemoveAll(dir)
	for _, f := range plan.Files {
		p := filepath.Join(dir, f.Name)
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, f.Data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The stub is a provider of its own, as in grain's probe: the
	// built-in one speaks WebSocket. It answers at once, so codex's own
	// retry ladder would only slow a failure down.
	cfg := filepath.Join(dir, codex.Home, "config.toml")
	data, _ := os.ReadFile(cfg)
	data = append([]byte("model_provider = \"stub\"\n"), data...)
	data = append(data, fmt.Sprintf("[model_providers.stub]\nname = \"stub\"\nbase_url = %q\nwire_api = \"responses\"\nenv_key = %q\nrequest_max_retries = 0\nstream_max_retries = 0\n", stub.URL+"/v1", codex.APIKeyVar)...)
	os.WriteFile(cfg, data, 0o600)

	c := plan.Command(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Env, cmd.Dir = c.Env, t.TempDir()
	cmd.Stdin = strings.NewReader(string(c.Stdin))
	transcript, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("codex: %v\n%s", err, transcript)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) == 0 {
		t.Fatalf("codex never reached the stub model\n%s", transcript)
	}
	advertised := toolNames(requests[0]["tools"])
	t.Logf("advertised tools: %s", strings.Join(advertised, ", "))
	for _, name := range nativeTools {
		for _, a := range advertised {
			if a == name {
				t.Errorf("native tool advertised: %s", name)
			}
		}
	}
	outputs := map[string]string{}
	for _, req := range requests {
		input, _ := req["input"].([]any)
		for _, entry := range input {
			item, _ := entry.(map[string]any)
			id, _ := item["call_id"].(string)
			if output, ok := item["output"]; ok && strings.HasPrefix(id, "native_") {
				encoded, _ := json.Marshal(output)
				outputs[id] = string(encoded)
			}
		}
	}
	for _, name := range nativeTools {
		out := outputs["native_"+name]
		t.Logf("native %s -> %s", name, out)
		if !strings.Contains(out, "unsupported") {
			t.Errorf("native %s was not refused: %s", name, out)
		}
	}
	if _, err := os.Stat(canary); !os.IsNotExist(err) {
		t.Errorf("a native tool wrote on the host: %v", err)
	}
	if mcpCalls == 0 {
		t.Errorf("the gateway's tool was never called\n%s", transcript)
	}
}

// toolNames lists every tool name in a Responses request's tools,
// namespaced ones as namespace/name, sorted.
func toolNames(v any) []string {
	var out []string
	var walk func(v any, ns string)
	walk = func(v any, ns string) {
		switch v := v.(type) {
		case []any:
			for _, c := range v {
				walk(c, ns)
			}
		case map[string]any:
			name, _ := v["name"].(string)
			typ, _ := v["type"].(string)
			switch {
			case typ == "namespace":
				walk(v["tools"], name)
				return
			case name != "" && ns != "":
				out = append(out, ns+"/"+name)
			case name != "":
				out = append(out, name)
			case typ != "":
				out = append(out, typ)
			}
			walk(v["tools"], ns)
		}
	}
	walk(v, "")
	sort.Strings(out)
	return out
}

func findTool(value any, needle, namespace string) (string, string) {
	switch v := value.(type) {
	case []any:
		for _, child := range v {
			if name, ns := findTool(child, needle, namespace); name != "" {
				return name, ns
			}
		}
	case map[string]any:
		name, _ := v["name"].(string)
		if v["type"] == "namespace" {
			namespace = name
		} else if name == needle || strings.HasSuffix(name, "__"+needle) {
			return name, namespace
		}
		for _, key := range []string{"tools", "input"} {
			if name, ns := findTool(v[key], needle, namespace); name != "" {
				return name, ns
			}
		}
	}
	return "", ""
}
