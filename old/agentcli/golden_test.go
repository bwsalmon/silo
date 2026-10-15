package agentcli_test

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/old/agentcli"
	_ "github.com/bwsalmon/silo/old/agentcli/claude"
	"github.com/bwsalmon/silo/old/agentcli/codex"
	_ "github.com/bwsalmon/silo/old/agentcli/custom"
	"github.com/bwsalmon/silo/old/location"
	"github.com/bwsalmon/silo/old/silofile"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

type cell struct {
	name    string
	cli     string
	version string
	mode    string
	perm    silofile.RemotePermissions
	custom  *silofile.CustomCLI
	// code is the exit code a refusal maps to: 76 for a *Refusal, 69
	// for a *PrerequisiteError; 0 for a plan.
	code int
}

var (
	none   = silofile.RemotePermissions{Mode: silofile.PermNone}
	all    = silofile.RemotePermissions{Mode: silofile.PermAll}
	subset = silofile.RemotePermissions{Mode: silofile.PermSubset, Tools: []string{"Read", "Grep", "Edit"}}
)

func args(a ...string) *[]string { return &a }

var customCLI = &silofile.CustomCLI{
	Argv: silofile.CustomArgv{
		Interactive: []string{"--tui"},
		Headless:    []string{"--prompt-file", "{prompt}", "--jsonl"},
	},
	MCPConfig:   silofile.CustomMCPConfig{Format: "claude-json", Arg: []string{"--mcp", "{file}"}},
	Permissions: silofile.CustomPermissions{None: args("--no-builtin-tools"), All: args()},
}

var customTOML = &silofile.CustomCLI{
	Argv:        silofile.CustomArgv{Headless: []string{"--run"}},
	MCPConfig:   silofile.CustomMCPConfig{Format: "codex-toml", Env: "MY_AGENT_MCP_CONFIG"},
	Permissions: silofile.CustomPermissions{None: args("--safe")},
}

func cells() []cell {
	var out []cell
	for _, mode := range []string{agentcli.Interactive, agentcli.Headless} {
		for _, p := range []struct {
			name string
			perm silofile.RemotePermissions
		}{{"none", none}, {"all", all}, {"subset", subset}} {
			out = append(out, cell{name: fmt.Sprintf("claude-%s-%s", mode, p.name), cli: "claude", version: "2.1.197", mode: mode, perm: p.perm})
			c := cell{name: fmt.Sprintf("codex-%s-%s", mode, p.name), cli: "codex", version: "0.153.0", mode: mode, perm: p.perm}
			if p.name == "subset" {
				c.code = 76
			}
			out = append(out, c)
			c = cell{name: fmt.Sprintf("custom-%s-%s", mode, p.name), cli: "custom", mode: mode, perm: p.perm, custom: customCLI}
			if p.name == "subset" {
				c.code = 76
			}
			out = append(out, c)
		}
	}
	return append(out,
		cell{name: "claude-unknown-tool", cli: "claude", version: "2.1.197", mode: agentcli.Headless,
			perm: silofile.RemotePermissions{Mode: silofile.PermSubset, Tools: []string{"Read", "Teleport"}}, code: 76},
		// Named by earlier rosters, but 2.1.197 drops it in print mode.
		cell{name: "claude-unavailable-tool", cli: "claude", version: "2.1.197", mode: agentcli.Headless,
			perm: silofile.RemotePermissions{Mode: silofile.PermSubset, Tools: []string{"Read", "TodoWrite"}}, code: 76},
		cell{name: "claude-attached", cli: "claude", version: "2.1.197", mode: agentcli.Attached, perm: none, code: 76},
		cell{name: "claude-unreviewed-version-subset", cli: "claude", version: "9.9.9", mode: agentcli.Headless, perm: subset},
		cell{name: "codex-unreviewed-none", cli: "codex", version: "0.170.0", mode: agentcli.Headless, perm: none, code: 76},
		cell{name: "codex-unreviewed-all", cli: "codex", version: "0.170.0", mode: agentcli.Headless, perm: all},
		cell{name: "codex-0.162.1-headless-none", cli: "codex", version: "0.162.1", mode: agentcli.Headless, perm: none},
		cell{name: "codex-attached", cli: "codex", version: "0.153.0", mode: agentcli.Attached, perm: all, code: 76},
		cell{name: "custom-undeclared-all", cli: "custom", mode: agentcli.Headless, perm: all, custom: customTOML, code: 76},
		cell{name: "custom-toml-env-none", cli: "custom", mode: agentcli.Headless, perm: none, custom: customTOML},
	)
}

func request(c cell) agentcli.Request {
	a := silofile.Agent{CLI: c.cli, Model: "test-model", Custom: c.custom}
	env := map[string]string{"DELIVERED": "to-agent"}
	switch c.cli {
	case "claude":
		a.Auth.Credential = "key"
		env["ANTHROPIC_API_KEY"] = "sk-test"
	case "codex":
		a.Auth.Credential = "key"
		a.Effort = "high"
		env["OPENAI_API_KEY"] = "sk-test"
		env["OPENAI_BASE_URL"] = "http://127.0.0.1:9745/v1"
	}
	return agentcli.Request{
		Agent: a, Mode: c.mode, Permissions: c.perm,
		Prompt: "do the thing", SystemPrompt: "be careful",
		GatewayURL: "http://127.0.0.1:41234/mcp", GatewayToken: "TOKEN",
		GatewayTools: []string{"sandbox_exec", "sandbox_read_file"},
		Host:         location.HostInfo{Binary: "/bin/" + c.cli, Version: c.version, Home: "/home/u", Env: []string{"PATH=/bin", "HOME=/home/u"}},
		Env:          env,
	}
}

// render writes a plan as text: argv, env, stdin and every staged file.
func render(plan agentcli.Plan) string {
	cmd := plan.Command("/STAGE")
	var b strings.Builder
	fmt.Fprintf(&b, "tty: %v\nargv:\n", cmd.TTY)
	for _, a := range cmd.Argv {
		fmt.Fprintf(&b, "  %q\n", a)
	}
	b.WriteString("env:\n")
	for _, e := range cmd.Env {
		fmt.Fprintf(&b, "  %s\n", e)
	}
	if cmd.Stdin != nil {
		fmt.Fprintf(&b, "stdin: %q\n", cmd.Stdin)
	}
	for _, n := range plan.Notes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	for _, f := range plan.Files {
		if f.Link != "" {
			fmt.Fprintf(&b, "--- %s -> %s\n", f.Name, f.Link)
			continue
		}
		fmt.Fprintf(&b, "--- %s\n%s", f.Name, f.Data)
		if !strings.HasSuffix(string(f.Data), "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func TestGolden(t *testing.T) {
	for _, c := range cells() {
		t.Run(c.name, func(t *testing.T) {
			m, err := agentcli.Lookup(c.cli)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := m.Build(request(c))
			var got string
			switch {
			case err != nil:
				code := 1
				var pre *location.PrerequisiteError
				if agentcli.IsRefusal(err) {
					code = 76
				} else if errors.As(err, &pre) {
					code = 69
				}
				if code != c.code {
					t.Fatalf("got %v (exit %d), want exit %d", err, code, c.code)
				}
				got = fmt.Sprintf("refused (exit %d): %v\n", code, err)
			case c.code != 0:
				t.Fatalf("got a plan, want exit %d", c.code)
			default:
				got = render(plan)
			}
			golden := filepath.Join("testdata", c.name+".golden")
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Fatalf("differs from %s (run go test ./agentcli -update if intended):\n%s", golden, got)
			}
		})
	}
}

func TestCodexAmbientAuthLinksTheUsersLogin(t *testing.T) {
	home := t.TempDir()
	c := cell{cli: "codex", version: "0.153.0", mode: agentcli.Headless, perm: none}
	req := request(c)
	req.Agent.Auth.Credential = ""
	req.Host.Home = home
	req.Host.Env = []string{"HOME=" + home}
	var pre *location.PrerequisiteError
	if _, err := (codex.Mapping{}).Build(req); !errors.As(err, &pre) {
		t.Fatalf("no auth.json: got %v, want a prerequisite error", err)
	}
	os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
	os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte("{}"), 0o600)
	plan, err := (codex.Mapping{}).Build(req)
	if err != nil {
		t.Fatal(err)
	}
	var link string
	for _, f := range plan.Files {
		if f.Name == filepath.Join(codex.Home, "auth.json") {
			link = f.Link
		}
	}
	if link != filepath.Join(home, ".codex", "auth.json") {
		t.Fatalf("auth.json link = %q", link)
	}
	if !strings.Contains(render(plan), `cli_auth_credentials_store = "file"`) {
		t.Fatal("config does not keep the login in the file")
	}
}
