package adapter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func spec(t *testing.T) Spec {
	return Spec{
		Binary:       "/usr/local/bin/agent",
		Args:         []string{"--model", "m1"},
		Env:          map[string]string{"B": "2", "A": "1"},
		Checkout:     "/repos/team/ctx",
		SessionDir:   t.TempDir(),
		MCPCommand:   "/usr/local/bin/silo",
		MCPArgs:      []string{"mcp-bridge", "/tmp/s.sock"},
		SystemPrompt: "Be \"helpful\".\nSecond line.",
	}
}

// flagValue returns the argument after flag.
func flagValue(t *testing.T, args []string, flag string) string {
	t.Helper()
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("no %s in %q", flag, args)
	return ""
}

func TestClaude(t *testing.T) {
	s := spec(t)
	l, err := Claude{}.Prepare(s)
	if err != nil {
		t.Fatal(err)
	}
	if l.Path != s.Binary || l.Dir != s.Checkout {
		t.Errorf("launch = %+v", l)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Type, Command string
			Args          []string
		} `json:"mcpServers"`
	}
	data, err := os.ReadFile(flagValue(t, l.Args, "--mcp-config"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	srv, ok := cfg.MCPServers["silo"]
	if len(cfg.MCPServers) != 1 || !ok || srv.Type != "stdio" || srv.Command != s.MCPCommand || strings.Join(srv.Args, " ") != "mcp-bridge /tmp/s.sock" {
		t.Errorf("mcp config = %s", data)
	}
	if flagValue(t, l.Args, "--append-system-prompt") != s.SystemPrompt {
		t.Error("system prompt not passed")
	}
	disallowed := flagValue(t, l.Args, "--disallowedTools")
	for _, tool := range []string{"Bash", "Edit", "Write", "MultiEdit", "NotebookEdit", "Read"} {
		if !strings.Contains(","+disallowed+",", ","+tool+",") {
			t.Errorf("%s not disallowed: %s", tool, disallowed)
		}
	}
	if flagValue(t, l.Args, "--allowedTools") != "mcp__silo" || flagValue(t, l.Args, "--setting-sources") != "user" {
		t.Errorf("args = %q", l.Args)
	}
	if !contains(l.Args, "--strict-mcp-config") {
		t.Errorf("no --strict-mcp-config: %q", l.Args)
	}
	if got := strings.Join(l.Args[len(l.Args)-2:], " "); got != "--model m1" {
		t.Errorf("extra args not last: %q", l.Args)
	}
	if strings.Join(l.Env, " ") != "A=1 B=2" {
		t.Errorf("env = %q", l.Env)
	}
}

func TestCodex(t *testing.T) {
	s := spec(t)
	l, err := Codex{}.Prepare(s)
	if err != nil {
		t.Fatal(err)
	}
	var overrides []string
	for i, a := range l.Args {
		if a == "-c" {
			overrides = append(overrides, l.Args[i+1])
		}
	}
	all := strings.Join(overrides, "\n")
	for _, want := range []string{
		`mcp_servers.silo.command="/usr/local/bin/silo"`,
		`mcp_servers.silo.args=["mcp-bridge","/tmp/s.sock"]`,
		`developer_instructions="Be \"helpful\".\nSecond line."`,
		`features.shell_tool=false`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("no override %s in:\n%s", want, all)
		}
	}
	if flagValue(t, l.Args, "--sandbox") != "read-only" || flagValue(t, l.Args, "--ask-for-approval") != "untrusted" || flagValue(t, l.Args, "--cd") != s.Checkout {
		t.Errorf("args = %q", l.Args)
	}
	if (Codex{}).ListChanged() {
		t.Error("codex should use the fallback tools")
	}
}

func TestGemini(t *testing.T) {
	s := spec(t)
	l, err := Gemini{}.Prepare(s)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, kv := range l.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	settings := env["GEMINI_CLI_SYSTEM_SETTINGS_PATH"]
	if filepath.Dir(filepath.Dir(settings)) != s.SessionDir {
		t.Errorf("settings outside the session dir: %q", settings)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string
			Args    []string
			Trust   bool
		} `json:"mcpServers"`
		MCP   struct{ Allowed []string }
		Tools struct{ Exclude []string }
	}
	data, _ := os.ReadFile(settings)
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	srv := cfg.MCPServers["silo"]
	if len(cfg.MCPServers) != 1 || srv.Command != s.MCPCommand || !srv.Trust || strings.Join(cfg.MCP.Allowed, ",") != "silo" {
		t.Errorf("settings = %s", data)
	}
	excluded := strings.Join(cfg.Tools.Exclude, ",")
	for _, tool := range []string{"run_shell_command", "write_file", "replace"} {
		if !strings.Contains(excluded, tool) {
			t.Errorf("%s not excluded", tool)
		}
	}
	if prompt, _ := os.ReadFile(env["GEMINI_SYSTEM_MD"]); string(prompt) != s.SystemPrompt {
		t.Errorf("system prompt = %q", prompt)
	}
	if flagValue(t, l.Args, "--allowed-mcp-server-names") != "silo" || env["A"] != "1" {
		t.Errorf("launch = %+v", l)
	}
}

func TestGet(t *testing.T) {
	for _, n := range []string{"claude", "codex", "gemini"} {
		a, err := Get(n)
		if err != nil || a.Name() != n {
			t.Errorf("Get(%s) = %v, %v", n, a, err)
		}
	}
	if _, err := Get("vim"); err == nil {
		t.Error("Get(vim) succeeded")
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
