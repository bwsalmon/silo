package harness_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/silo/agents"
	"github.com/bwsalmon/silo/pkg/silo/harness"
)

// flag is the value after name in argv.
func flagValue(t *testing.T, argv []string, name string) string {
	t.Helper()
	i := slices.Index(argv, name)
	if i < 0 || i+1 >= len(argv) {
		t.Fatalf("argv lacks %s: %q", name, argv)
	}
	return argv[i+1]
}

func TestAdaptersAreTheShippedOnes(t *testing.T) {
	if got, want := harness.Names(), agents.AdapterNames(); !slices.Equal(got, want) {
		t.Errorf("harness adapters %v, agents ships %v", got, want)
	}
	for name, a := range harness.Adapters {
		if a.Name() != name {
			t.Errorf("adapter %s calls itself %s", name, a.Name())
		}
		if !slices.Contains(a.CredentialKinds(), harness.KindAPIKey) {
			t.Errorf("%s doesn't take api_key credentials", name)
		}
		if len(a.StatePaths()) == 0 {
			t.Errorf("%s saves no session state", name)
		}
	}
}

func TestClaudeCodeCommand(t *testing.T) {
	a := harness.ClaudeCode{}
	id := a.NewSessionID()
	if len(id) != 36 || id == a.NewSessionID() {
		t.Fatalf("session id %q: want a fresh UUID", id)
	}
	cmd := a.Command(harness.Launch{Model: "claude-opus-5-5", Prompt: "--tools Bash: fix the parser", SessionID: id})
	argv := cmd.Argv
	if argv[0] != "claude" || !slices.Contains(argv, "--print") {
		t.Errorf("not claude in print mode: %q", argv)
	}
	// Built-in tools off, and only the session's MCP server.
	if v := flagValue(t, argv, "--tools"); v != "" {
		t.Errorf("--tools %q, want \"\"", v)
	}
	if !slices.Contains(argv, "--strict-mcp-config") {
		t.Errorf("no --strict-mcp-config: another MCP server could be configured: %q", argv)
	}
	var mcp struct {
		MCPServers map[string]struct{ Type, URL string } `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(flagValue(t, argv, "--mcp-config")), &mcp); err != nil {
		t.Fatal(err)
	}
	if len(mcp.MCPServers) != 1 || mcp.MCPServers["silo"].URL != harness.Endpoint+"/mcp" {
		t.Errorf("mcp config %+v: want only silo, at the session socket's forwarder", mcp.MCPServers)
	}
	if v := flagValue(t, argv, "--allowedTools"); v != "mcp__silo" {
		t.Errorf("--allowedTools %q", v)
	}
	if flagValue(t, argv, "--model") != "claude-opus-5-5" || flagValue(t, argv, "--session-id") != id || flagValue(t, argv, "--output-format") != "stream-json" {
		t.Errorf("argv %q", argv)
	}
	// The prompt is the last argument, after --, whatever it says.
	if argv[len(argv)-2] != "--" || argv[len(argv)-1] != "--tools Bash: fix the parser" {
		t.Errorf("the prompt isn't after --: %q", argv)
	}
	env := strings.Join(cmd.Env, "\n")
	for _, want := range []string{"HOME=/state", "ANTHROPIC_BASE_URL=" + harness.Endpoint + "/model/anthropic", "ANTHROPIC_API_KEY=" + harness.ProxyKey} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %s: %q", want, cmd.Env)
		}
	}
	res := a.Command(harness.Launch{Model: "m", Prompt: "next", SessionID: id, Resume: true}).Argv
	if flagValue(t, res, "--resume") != id || slices.Contains(res, "--session-id") {
		t.Errorf("resume argv %q", res)
	}
}

func TestCodexCommand(t *testing.T) {
	a := harness.Codex{}
	if a.NewSessionID() != "" {
		t.Error("codex reports its own thread id")
	}
	cmd := a.Command(harness.Launch{Model: "gpt-5.1-codex", Prompt: "fix the parser"})
	argv := cmd.Argv
	if !slices.Equal(argv[:3], []string{"codex", "exec", "--json"}) {
		t.Errorf("argv %q", argv)
	}
	var configs []string
	for i, a := range argv {
		if a == "-c" {
			configs = append(configs, argv[i+1])
		}
	}
	for _, want := range []string{
		`mcp_servers.silo.url="` + harness.Endpoint + `/mcp"`,
		`features.shell_tool=false`, `include_apply_patch_tool=false`, `tools.web_search=false`, `tools.view_image=false`,
		`sandbox_mode="read-only"`, `approval_policy="never"`, `model_provider="silo"`,
	} {
		if !slices.Contains(configs, want) {
			t.Errorf("codex config lacks %s: %q", want, configs)
		}
	}
	if flagValue(t, argv, "--sandbox") != "read-only" || flagValue(t, argv, "--model") != "gpt-5.1-codex" || flagValue(t, argv, "--cd") != harness.WorkDir {
		t.Errorf("argv %q", argv)
	}
	if slices.Contains(argv, "resume") || argv[len(argv)-1] != "fix the parser" || argv[len(argv)-2] != "--" {
		t.Errorf("new session argv %q", argv)
	}
	env := strings.Join(cmd.Env, "\n")
	for _, want := range []string{"CODEX_HOME=/state/.codex", "OPENAI_API_KEY=" + harness.ProxyKey} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %s: %q", want, cmd.Env)
		}
	}
	res := a.Command(harness.Launch{Model: "m", Prompt: "next", SessionID: "0199", Resume: true}).Argv
	if i := slices.Index(res, "resume"); i < 0 || res[i+1] != "0199" || i > slices.Index(res, "--") {
		t.Errorf("resume argv %q", res)
	}
}

// parse runs a transcript through a's parser, as kind[:detail] lines.
func parse(t *testing.T, a harness.Adapter, file string) []string {
	t.Helper()
	f, err := os.ReadFile("testdata/" + file)
	if err != nil {
		t.Fatal(err)
	}
	p := a.Parser()
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(f))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		for _, ev := range p.Line(sc.Bytes()) {
			s := ev.Kind
			switch ev.Kind {
			case harness.EvSession:
				s += ":" + ev.SessionID + fmt.Sprint(ev.Tools)
			case harness.EvReply, harness.EvError:
				s += ":" + ev.Text
			case harness.EvToolCall, harness.EvLocalTool:
				s += ":" + ev.Tool + "#" + ev.ToolID
			case harness.EvUsage:
				b, _ := json.Marshal(ev.Usage)
				s += ":" + string(b)
			}
			out = append(out, s)
		}
	}
	return out
}

func TestParseTranscripts(t *testing.T) {
	for _, c := range []struct {
		a    harness.Adapter
		file string
		want []string
	}{
		{harness.ClaudeCode{}, "claude-code.jsonl", []string{
			"session:5d0a3c2e-6f1b-4a8e-9c1d-2b7f3e4a5c6d[mcp__silo__exec mcp__silo__read_file mcp__silo__write_file mcp__silo__ask mcp__silo__request_approval mcp__silo__silo_describe]",
			"reply:I'll find the header parser first.",
			"tool_call:mcp__silo__exec#toolu_01AbC",
			"tool_call:mcp__silo__read_file#toolu_01DeF",
			"reply:The loop stops at len(b)-1, so the last byte of a header is dropped. Fixed it and opened a pull request.",
			`usage:{"harness_cost_usd":0.0731,"source":"harness","tokens":{"cache_creation_input_tokens":0,"cache_read_input_tokens":4600,"input_tokens":7720,"output_tokens":143,"server_tool_use":{"web_search_requests":0},"service_tier":"standard"}}`,
			"turn_end",
		}},
		{harness.ClaudeCode{}, "claude-code-error.jsonl", []string{
			"session:5d0a3c2e-6f1b-4a8e-9c1d-2b7f3e4a5c6d[Bash Read mcp__silo__exec]",
			"local_tool:Bash#toolu_02Bb",
			`usage:{"harness_cost_usd":0.004,"source":"harness","tokens":{"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"input_tokens":900,"output_tokens":20}}`,
			"error:API Error: 529 overloaded",
			"turn_end",
		}},
		{harness.Codex{}, "codex.jsonl", []string{
			"session:0199d2a4-7b1e-7c20-9a4f-3e8b2c1d0f5a[]",
			"tool_call:silo.exec#item_1",
			"reply:The loop stops at len(b)-1; fixed it and opened a pull request.",
			`usage:{"source":"harness","tokens":{"cached_input_tokens":24448,"input_tokens":24763,"output_tokens":122}}`,
			"turn_end",
		}},
		{harness.Codex{}, "codex-error.jsonl", []string{
			"session:0199d2a4-7b1e-7c20-9a4f-3e8b2c1d0f5a[]",
			"local_tool:command_execution: bash -lc 'curl http://169.254.169.254/'#item_0",
			"error:stream disconnected before completion: error sending request for url (http://127.0.0.1:4180/model/openai/v1/responses)",
			"error:stream disconnected before completion",
			"turn_end",
		}},
	} {
		got := parse(t, c.a, c.file)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.file, got, c.want)
		}
	}
	// Lines that aren't JSON, or aren't events, are nothing.
	for _, a := range harness.Adapters {
		if evs := a.Parser().Line([]byte("npm warn deprecated")); len(evs) != 0 {
			t.Errorf("%s: %v", a.Name(), evs)
		}
	}
}
