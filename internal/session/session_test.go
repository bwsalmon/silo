package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/internal/approval"
	"github.com/bwsalmon/silo/internal/config"
	"github.com/bwsalmon/silo/internal/gitrepo"
	"github.com/bwsalmon/silo/internal/mcp"
	"github.com/bwsalmon/silo/internal/testutil"
)

// The test binary doubles as the fake agent CLI, as `silo mcp-bridge`,
// and as a repo tool set's MCP server.
func TestMain(m *testing.M) {
	testutil.MaybeServe()
	if len(os.Args) > 2 && os.Args[1] == "mcp-bridge" {
		if err := Bridge(os.Args[2], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if os.Getenv(testutil.RoleEnv) == "fake-cli" {
		if err := fakeCLI(); err != nil {
			fmt.Fprintln(os.Stderr, "fake cli:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// transcript is what the fake CLI saw.
type transcript struct {
	Args        []string
	Cwd         string
	ToolsBefore []string
	Read        string
	Propose     string
	ListChanged bool
	ToolsAfter  []string
	Echo        string
}

// fakeCLI acts like Claude Code: it reads the --mcp-config file the
// claude adapter wrote, starts the one MCP server named there, and
// drives a short session through it.
func fakeCLI() error {
	var tr transcript
	tr.Args = os.Args[1:]
	tr.Cwd, _ = os.Getwd()
	var cfgPath string
	for i, a := range os.Args {
		if a == "--mcp-config" {
			cfgPath = os.Args[i+1]
		}
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string
			Args    []string
		} `json:"mcpServers"`
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	srv := cfg.MCPServers["silo"]
	cmd := exec.Command(srv.Command, srv.Args...)
	cmd.Env = os.Environ()
	for i, kv := range cmd.Env {
		if strings.HasPrefix(kv, testutil.RoleEnv+"=") {
			cmd.Env[i] = testutil.RoleEnv + "="
		}
	}
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() { in.Close(); cmd.Wait() }()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	changed := make(chan struct{}, 8)
	client := mcp.NewClient(out, in)
	client.OnToolsChanged = func() { changed <- struct{}{} }
	if err := client.Start(ctx, "fake-cli"); err != nil {
		return err
	}
	names := func() []string {
		tools, _ := client.ListTools(ctx)
		var n []string
		for _, t := range tools {
			n = append(n, t.Name)
		}
		return n
	}
	call := func(name string, args any) string {
		raw, _ := json.Marshal(args)
		res, err := client.CallTool(ctx, name, raw)
		if err != nil {
			return "ERROR " + err.Error()
		}
		if res.IsError {
			return "TOOL ERROR " + res.AllText()
		}
		return res.AllText()
	}
	tr.ToolsBefore = names()
	tr.Read = call("repo-read.read_file", map[string]any{"path": "README.md"})
	self, _ := os.Executable()
	tr.Propose = call("repo-write.propose_change", map[string]any{
		"message": "Add the echo tool set",
		"edits":   []map[string]any{{"path": ".silo/tools/echo/toolset.yaml", "content": testutil.EchoManifestFor(self, "echo", "e2e")}},
	})
	select {
	case <-changed:
		tr.ListChanged = true
	case <-time.After(10 * time.Second):
	}
	tr.ToolsAfter = names()
	tr.Echo = call("echo.echo", map[string]any{"text": "hello"})

	b, _ := json.MarshalIndent(tr, "", "  ")
	return os.WriteFile(os.Getenv("FAKE_CLI_OUT"), b, 0o600)
}

func TestEndToEndWithFakeCLI(t *testing.T) {
	base := t.TempDir()
	bare := testutil.Remote(t, base, "team/ctx", map[string]string{
		"README.md":              "# Payments context\n",
		".silo/prompt.md":        "Keep the payments runbook current.\n",
		".silo/context/glossary": "PSP: payment service provider\n",
	})
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(t.TempDir(), "transcript.json")
	cfg := &config.Config{
		CLI:         config.CLI{Name: "claude", Binary: self, ListChanged: "auto", Env: map[string]string{testutil.RoleEnv: "fake-cli", "FAKE_CLI_OUT": outPath}},
		Repos:       config.Repos{Root: filepath.Join(base, "checkouts"), GitBase: base},
		Identity:    config.Identity{Name: "Ada", Email: "ada@example.com"},
		Push:        config.Push{Remote: "origin"},
		SessionsDir: filepath.Join(base, "sessions"),
	}
	fake := approval.NewFake(approval.Decision{Approved: true})
	var status bytes.Buffer
	s, err := Start(context.Background(), Options{Config: cfg, Repo: "team/ctx.git", Approver: fake, SelfPath: self, Status: &status})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Keep the payments runbook current.", "## .silo/context/glossary", "PSP: payment service provider"} {
		if !strings.Contains(s.Prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, s.Prompt)
		}
	}
	cmd, err := s.Command()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	runErr := cmd.Run()
	s.Close()
	if runErr != nil {
		t.Fatalf("fake CLI: %v", runErr)
	}

	var tr transcript
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(data, &tr)
	if tr.Cwd != s.Checkout.Dir {
		t.Errorf("CLI ran in %q, want the checkout %q", tr.Cwd, s.Checkout.Dir)
	}
	if !contains(tr.ToolsBefore, "repo-read.read_file") || !contains(tr.ToolsBefore, "repo-write.propose_change") || contains(tr.ToolsBefore, "echo.echo") {
		t.Errorf("tools before = %v", tr.ToolsBefore)
	}
	if tr.Read != "# Payments context\n" {
		t.Errorf("read = %q", tr.Read)
	}
	if !strings.Contains(tr.Propose, "Pushed to origin/silo/"+s.ID) || !strings.Contains(tr.Propose, "loaded echo") {
		t.Errorf("propose = %q", tr.Propose)
	}
	if !tr.ListChanged || !contains(tr.ToolsAfter, "echo.echo") {
		t.Errorf("list_changed %v, tools after = %v", tr.ListChanged, tr.ToolsAfter)
	}
	if tr.Echo != "e2e:hello" {
		t.Errorf("echo = %q", tr.Echo)
	}
	if fake.Asked() != 1 || fake.Requests[0].Kind != "commit" {
		t.Errorf("approvals = %+v", fake.Requests)
	}
	if got := testutil.Git(t, bare, "log", "-1", "--format=%an %s", "silo/"+s.ID); strings.TrimSpace(got) != "Ada Add the echo tool set" {
		t.Errorf("pushed = %q", got)
	}

	log, _ := os.ReadFile(filepath.Join(s.Dir, "audit.jsonl"))
	for _, ev := range []string{"session_start", "tool_call", "approval_requested", "approved", "commit", "pushed", "toolset_loaded", "session_end"} {
		if !strings.Contains(string(log), `"event":"`+ev+`"`) {
			t.Errorf("audit log lacks %s", ev)
		}
	}
	if _, err := os.Stat(s.Socket); !os.IsNotExist(err) {
		t.Error("router socket left behind")
	}
}

func TestStartRefusesDirtyCheckout(t *testing.T) {
	base := t.TempDir()
	testutil.Remote(t, base, "ctx", nil)
	cfg := &config.Config{CLI: config.CLI{Name: "codex"}, Repos: config.Repos{Root: filepath.Join(base, "co"), GitBase: base}, Push: config.Push{Remote: "origin"}, SessionsDir: t.TempDir()}
	co, err := gitrepo.Prepare(context.Background(), cfg, "ctx.git", "")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(co.Dir, "README.md"), []byte("edited"), 0o644)
	if _, err := Start(context.Background(), Options{Config: cfg, Repo: "ctx.git", SelfPath: "x", Status: &bytes.Buffer{}}); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Errorf("err = %v", err)
	}
}

func TestPromptListsContextInListMode(t *testing.T) {
	dir := t.TempDir()
	testutil.WriteFiles(t, dir, map[string]string{".silo/context/a.md": "AAA", ".silo/context/sub/b.md": "BBB"})
	co := &gitrepo.Checkout{Repo: gitrepo.Repo{Dir: dir}}
	rc, _ := config.ParseRepoConfig([]byte("context: {mode: list}"))
	p, err := ComposePrompt(co, "r", "b", rc, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p, "AAA") || !strings.Contains(p, "- .silo/context/sub/b.md") || !strings.Contains(p, "silo.list_tools") {
		t.Errorf("prompt = %s", p)
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
