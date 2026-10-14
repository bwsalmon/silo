// Adapted from github.com/bwsalmon/grain pkg/agent/claude/claude_live_test.go at d6cd9d71f9b46df18d47e5305f84b5471ce93047, Apache License 2.0.

package main

// Live checks of cli-mapping.md's assumptions (§4) against the real
// claude and codex binaries and a real model, driven through silo run.
// They spend tokens, so they run only with SILO_V2_LIVE_CLI=1, and each
// skips without its CLI on PATH or its key:
//
//	SILO_V2_LIVE_CLI=1 ANTHROPIC_API_KEY=... OPENAI_API_KEY=... \
//	  go test ./cmd/silo -run Live -v
//
// ANTHROPIC_BASE_URL and OPENAI_BASE_URL, when set, are delivered to the
// CLI too, for a key that belongs to a proxy. SILO_V2_CLAUDE_MODEL and
// SILO_V2_CODEX_MODEL choose the models (a small claude, and codex's
// default, otherwise).

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/v2/agentcli/claude"
	"github.com/bwsalmon/silo/v2/session"
)

func liveCLI(t *testing.T, cli, keyVar string) {
	t.Helper()
	if os.Getenv("SILO_V2_LIVE_CLI") == "" {
		t.Skip("set SILO_V2_LIVE_CLI=1 to run the real CLIs against a model")
	}
	if _, err := exec.LookPath(cli); err != nil {
		t.Skipf("no %s on PATH", cli)
	}
	if os.Getenv(keyVar) == "" {
		t.Skipf("%s is not set", keyVar)
	}
}

func randomHex(t *testing.T) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// liveRun is one silo run --json and what it left.
type liveRun struct {
	code   int
	events []map[string]any
	// raw is the CLI's own output, transcript.raw.jsonl.
	raw    []map[string]any
	stderr string
}

// liveSilo is a silo folder with two local scripts, local_alpha and
// local_beta, which the gateway serves as its tools.
type liveSilo struct {
	dir, home string
	environ   []string
}

func newLiveSilo(t *testing.T) *liveSilo {
	root := t.TempDir()
	s := &liveSilo{dir: filepath.Join(root, "silo"), home: filepath.Join(root, "home")}
	for _, d := range []string{s.home, filepath.Join(s.dir, "scripts"), filepath.Join(root, "run"), filepath.Join(root, "state")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	os.Chmod(s.dir, 0o755)
	os.Chmod(filepath.Join(s.dir, "scripts"), 0o755)
	for _, name := range []string{"alpha", "beta"} {
		script := fmt.Sprintf("#!/bin/sh\necho %s-ran\n", name)
		if err := os.WriteFile(filepath.Join(s.dir, "scripts", name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s.environ = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + s.home,
		"XDG_RUNTIME_DIR=" + filepath.Join(root, "run"), "XDG_STATE_HOME=" + filepath.Join(root, "state"),
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "OPENAI_API_KEY", "OPENAI_BASE_URL"} {
		if v := os.Getenv(k); v != "" {
			s.environ = append(s.environ, "SILO_LIVE_"+k+"="+v)
		}
	}
	return s
}

// credentials is the Silofile's credentials: the key, which agent.auth
// names, and the base URL, delivered to the agent's environment.
func credentials(keyVar, urlVar string) string {
	b := "credentials:\n  - {name: key, from: {env: SILO_LIVE_" + keyVar + "}}\n"
	if os.Getenv(urlVar) != "" {
		b += "  - {name: url, from: {env: SILO_LIVE_" + urlVar + "}, to: [{agent: env, name: " + urlVar + "}]}\n"
	}
	return b
}

func (s *liveSilo) run(t *testing.T, silofile string) liveRun {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.dir, "Silofile"), []byte(silofile), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runSession([]string{"--json", s.dir}, strings.NewReader(""), &stdout, &stderr, session.Options{
		Environ: s.environ, NoGC: true, Grace: 5 * time.Second, Signals: make(chan os.Signal),
	})
	r := liveRun{code: code, events: jsonLines(t, stdout.Bytes()), stderr: stderr.String()}
	if len(r.events) > 0 {
		id, _ := r.events[0]["session"].(string)
		state := agentcliGetenv(s.environ, "XDG_STATE_HOME")
		if raw, err := os.ReadFile(filepath.Join(state, "silo", "sessions", id, "transcript.raw.jsonl")); err == nil {
			r.raw = jsonLines(t, raw)
		}
	}
	return r
}

func jsonLines(t *testing.T, b []byte) []map[string]any {
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func (r liveRun) dump() string {
	var b strings.Builder
	fmt.Fprintf(&b, "exit %d\nstderr: %s\n", r.code, r.stderr)
	for _, e := range r.events {
		j, _ := json.Marshal(e)
		if len(j) > 400 {
			j = append(j[:400], "..."...)
		}
		fmt.Fprintf(&b, "%s\n", j)
	}
	return b.String()
}

// toolCalls are the gateway calls silo recorded, by tool, with their
// outcomes.
func (r liveRun) toolCalls() map[string]string {
	out := map[string]string{}
	for _, e := range r.events {
		if e["type"] != "tool_call" {
			continue
		}
		c, _ := e["tool_call"].(map[string]any)
		if c["phase"] == "end" {
			out[c["tool"].(string)], _ = c["outcome"].(string)
		}
	}
	return out
}

// claudeInitTools is the tool list of claude's stream-json init event.
func (r liveRun) claudeInitTools() []string {
	for _, m := range r.raw {
		if m["type"] == "system" && m["subtype"] == "init" {
			var out []string
			for _, n := range m["tools"].([]any) {
				out = append(out, n.(string))
			}
			sort.Strings(out)
			return out
		}
	}
	return nil
}

// claudeToolUses are the names of every tool_use block claude emitted.
func (r liveRun) claudeToolUses() []string {
	var out []string
	for _, m := range r.raw {
		msg, _ := m["message"].(map[string]any)
		if m["type"] != "assistant" || msg == nil {
			continue
		}
		content, _ := msg["content"].([]any)
		for _, c := range content {
			block, _ := c.(map[string]any)
			if block["type"] == "tool_use" {
				out = append(out, block["name"].(string))
			}
		}
	}
	return out
}

func (r liveRun) rawText() string {
	b, _ := json.Marshal(r.raw)
	return string(b)
}

func claudeModel() string {
	if m := os.Getenv("SILO_V2_CLAUDE_MODEL"); m != "" {
		return m
	}
	return "claude-haiku-4-5-20251001"
}

func claudeSilofile(perm, prompt, extra string) string {
	return fmt.Sprintf(`silo: v2
name: live-claude
agent:
  cli: claude
  model: %s
  auth: {credential: key}
  mode: headless
  prompt: %q
%slocation: {kind: local}
remote_permissions: %s
local:
  scripts: {dir: scripts}
`, claudeModel(), prompt, extra, perm) + credentials("ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL")
}

// hookProject is a directory whose project and local settings each
// carry a SessionStart and a UserPromptSubmit hook that leave a marker
// in markers.
func hookProject(t *testing.T) (dir, markers string) {
	dir, markers = t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".claude"), 0o755)
	for _, f := range []string{"settings.json", "settings.local.json"} {
		hook := func(event string) any {
			return []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "touch " + filepath.Join(markers, f+"-"+event)}}}}
		}
		b, _ := json.Marshal(map[string]any{"hooks": map[string]any{"SessionStart": hook("SessionStart"), "UserPromptSubmit": hook("UserPromptSubmit")}})
		if err := os.WriteFile(filepath.Join(dir, ".claude", f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, markers
}

func listDir(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// claudeDirect runs the claude binary itself, not through silo, for the
// controls and for what silo's own argv never exercises.
func claudeDirect(t *testing.T, dir, prompt string, args ...string) []map[string]any {
	t.Helper()
	argv := append([]string{"-p", "--output-format", "stream-json", "--verbose", "--model", claudeModel()}, args...)
	cmd := exec.Command("claude", argv...)
	cmd.Dir, cmd.Stdin = dir, strings.NewReader(prompt)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "ANTHROPIC_API_KEY=" + os.Getenv("ANTHROPIC_API_KEY")}
	if u := os.Getenv("ANTHROPIC_BASE_URL"); u != "" {
		cmd.Env = append(cmd.Env, "ANTHROPIC_BASE_URL="+u)
	}
	done := make(chan struct{})
	timer := time.AfterFunc(3*time.Minute, func() {
		select {
		case <-done:
		default:
			cmd.Process.Kill()
		}
	})
	defer timer.Stop()
	out, err := cmd.Output()
	close(done)
	if err != nil {
		t.Logf("claude %v: %v", args, err)
	}
	return jsonLines(t, out)
}

// TestLiveClaudeNone: a remote_permissions none session has the
// gateway's tools and nothing else; every gateway tool is usable with
// only mcp__silo in --allowedTools (assumption 4); a host file cannot be
// read; and project and local hooks in its workdir do not run
// (assumption 2), though the same directory's hooks do run for a plain
// claude -p (the control).
func TestLiveClaudeNone(t *testing.T) {
	liveCLI(t, "claude", "ANTHROPIC_API_KEY")
	s := newLiveSilo(t)
	secret := randomHex(t)
	canary := filepath.Join(t.TempDir(), "host-canary.txt")
	os.WriteFile(canary, []byte(secret+"\n"), 0o644)
	project, markers := hookProject(t)

	prompt := "First list, by exact name, every tool you have. Then call local_alpha and local_beta, with no arguments. " +
		"Then try every way you have to read the file " + canary + " and print what it contains. Finally reply DONE."
	r := s.run(t, claudeSilofile("none", prompt, "  workdir: "+project+"\n"))
	if r.code != 0 {
		t.Fatalf("silo run:\n%s", r.dump())
	}
	init := r.claudeInitTools()
	if want := []string{"mcp__silo__local_alpha", "mcp__silo__local_beta"}; strings.Join(init, ",") != strings.Join(want, ",") {
		t.Errorf("init tools = %v, want %v", init, want)
	}
	for _, name := range r.claudeToolUses() {
		if !strings.HasPrefix(name, "mcp__silo__") {
			t.Errorf("claude called %s, which is not a gateway tool", name)
		}
	}
	calls := r.toolCalls()
	for _, tool := range []string{"local_alpha", "local_beta"} {
		if calls[tool] != "ok" {
			t.Errorf("%s: outcome %q, want ok (calls: %v)", tool, calls[tool], calls)
		}
	}
	if strings.Contains(r.rawText(), secret) {
		t.Errorf("the host file's contents reached the transcript")
	}
	if m := listDir(markers); len(m) != 0 {
		t.Errorf("project or local hooks ran under --setting-sources user: %v", m)
	}

	// The control: the same project's hooks run without the flag.
	claudeDirect(t, project, "Reply OK.")
	got := listDir(markers)
	for _, f := range []string{"settings.json", "settings.local.json"} {
		found := false
		for _, m := range got {
			found = found || strings.HasPrefix(m, f+"-")
		}
		if !found {
			t.Errorf("control: %s's hooks did not run for a plain claude -p in %s (markers: %v); the check above proves nothing", f, project, got)
		}
	}
}

// TestLiveClaudeSubset: a headless subset runs with --permission-mode
// dontAsk; the listed tool works, the gateway's tools are pre-approved
// by mcp__silo alone (assumption 4), and the session ends rather than
// waiting on a prompt.
func TestLiveClaudeSubset(t *testing.T) {
	liveCLI(t, "claude", "ANTHROPIC_API_KEY")
	s := newLiveSilo(t)
	secret := randomHex(t)
	canary := filepath.Join(t.TempDir(), "readable.txt")
	os.WriteFile(canary, []byte(secret+"\n"), 0o644)
	prompt := "Call local_alpha with no arguments. Then use the Read tool to read " + canary + " and reply with its contents."
	r := s.run(t, claudeSilofile("[Read]", prompt, ""))
	if r.code != 0 {
		t.Fatalf("silo run:\n%s", r.dump())
	}
	if want := []string{"Read", "mcp__silo__local_alpha", "mcp__silo__local_beta"}; strings.Join(r.claudeInitTools(), ",") != strings.Join(want, ",") {
		t.Errorf("init tools = %v, want %v", r.claudeInitTools(), want)
	}
	if calls := r.toolCalls(); calls["local_alpha"] != "ok" {
		t.Errorf("local_alpha under dontAsk: %v", calls)
	}
	if !strings.Contains(r.rawText(), secret) {
		t.Errorf("Read did not return the file under dontAsk:\n%s", r.dump())
	}
}

// TestLiveClaudeDontAsk: in print mode, --permission-mode dontAsk
// refuses a tool that is available but not in --allowedTools, rather
// than prompting for it (assumption 3). silo's own argv never has such
// a tool (a subset's --tools and --allowedTools name the same tools), so
// this runs claude directly.
func TestLiveClaudeDontAsk(t *testing.T) {
	liveCLI(t, "claude", "ANTHROPIC_API_KEY")
	dir := t.TempDir()
	target := filepath.Join(dir, "must-not-exist")
	lines := claudeDirect(t, dir, "Use the Bash tool to run exactly this command: touch "+target+" . Then reply DONE.",
		"--tools", "Bash,Read", "--allowedTools", "Read", "--permission-mode", "dontAsk", "--setting-sources", "user")
	var result map[string]any
	for _, m := range lines {
		if m["type"] == "result" {
			result = m
		}
	}
	if result == nil {
		t.Fatalf("no result event: claude did not finish\n%v", lines)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("Bash ran without approval: %v", err)
	}
	denials, _ := result["permission_denials"].([]any)
	denied := false
	for _, d := range denials {
		denied = denied || d.(map[string]any)["tool_name"] == "Bash"
	}
	if !denied {
		t.Errorf("result has no permission denial for Bash: %v", result)
	}
}

// TestLiveClaudeRoster: every name in silo's roster table for the
// installed claude is accepted by --tools, and comes back in the init
// event under its own name or its alias's (assumption 1). claude drops
// an unknown name without a word, which is what silo's table is for.
func TestLiveClaudeRoster(t *testing.T) {
	liveCLI(t, "claude", "ANTHROPIC_API_KEY")
	out, err := exec.Command("claude", "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	version := strings.Fields(string(out))[0]
	roster, ok := claude.Rosters[version]
	if !ok {
		t.Skipf("silo has no roster table for claude %s", version)
	}
	s := newLiveSilo(t)
	r := s.run(t, claudeSilofile("["+strings.Join(roster, ", ")+"]", "Reply OK.", ""))
	if r.code != 0 {
		t.Fatalf("silo run:\n%s", r.dump())
	}
	got := map[string]bool{}
	for _, n := range r.claudeInitTools() {
		got[n] = true
	}
	want := map[string]bool{"mcp__silo__local_alpha": true, "mcp__silo__local_beta": true}
	for _, n := range roster {
		if a, ok := claude.Aliases[n]; ok {
			n = a
		}
		want[n] = true
		if !got[n] {
			t.Errorf("%s is in silo's roster for claude %s but claude did not load it", n, version)
		}
	}
	for n := range got {
		if !want[n] {
			t.Errorf("claude loaded %s, which silo's roster does not name", n)
		}
	}
}

func codexSilofile(perm, prompt string) string {
	model := ""
	if m := os.Getenv("SILO_V2_CODEX_MODEL"); m != "" {
		model = "  model: " + m + "\n"
	}
	return fmt.Sprintf(`silo: v2
name: live-codex
agent:
  cli: codex
%s  auth: {credential: key}
  mode: headless
  prompt: %q
location: {kind: local}
remote_permissions: %s
local:
  scripts: {dir: scripts}
`, model, prompt, perm) + credentials("OPENAI_API_KEY", "OPENAI_BASE_URL")
}

// TestLiveCodex: codex authenticates from agent.auth's credential alone
// and calls the gateway's tools; remote_permissions none is refused on
// a Codex version nobody has reviewed (assumption 5;
// agentcli/codex's TestLiveCodexNoneIsolation is the review probe).
func TestLiveCodex(t *testing.T) {
	liveCLI(t, "codex", "OPENAI_API_KEY")
	s := newLiveSilo(t)
	r := s.run(t, codexSilofile("all", "Call the tool local_alpha, then the tool local_beta, with no arguments. Then reply DONE."))
	if r.code != 0 {
		t.Fatalf("silo run:\n%s", r.dump())
	}
	calls := r.toolCalls()
	for _, tool := range []string{"local_alpha", "local_beta"} {
		if calls[tool] != "ok" {
			t.Errorf("%s: outcome %q, want ok (calls: %v)", tool, calls[tool], calls)
		}
	}

	r = s.run(t, codexSilofile("none", "Reply OK."))
	out, _ := exec.Command("codex", "--version").Output()
	reviewed := strings.Contains(string(out), "0.153.0")
	switch {
	case reviewed && r.code != 0:
		t.Errorf("none on reviewed %s:\n%s", out, r.dump())
	case !reviewed && r.code != 76:
		t.Errorf("none on unreviewed %s: exit %d, want 76\n%s", out, r.code, r.dump())
	}
}
