package session_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/bwsalmon/silo/v2/session"
)

// mcpSilofile is headless plus extra, with SELF replaced by this test
// binary, which serves MCP over stdio when SILO_MCPTEST=stdio.
func mcpSilofile(t *testing.T, extra string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return headless + strings.ReplaceAll(extra, "SELF", strconv.Quote(self))
}

const launched = `  - name: probe
    from: {env: SILO_TEST_DELIVERED}
    to: [{mcp: t, env: PROBE_DELIVERED}]
mcp:
  - name: t
    command: [SELF]
    env: {SILO_MCPTEST: stdio}
    inherit_env: [SILO_TEST_API_KEY]
    tools: [echo, env, cwd, pid]
`

func TestMCPServerThroughTheGateway(t *testing.T) {
	e := newEnv(t)
	f := e.silofile(mcpSilofile(t, launched))
	e.fake(fakeConfig{Call: "t__echo", Args: map[string]any{"text": "through the gateway"}})
	res := e.run(f, nil)
	if res.code != 0 {
		t.Fatalf("exit %d (%v)\n%s", res.code, res.err, res.stderr)
	}
	r := e.report()
	if r.Error != "" || r.Result != "through the gateway" {
		t.Fatalf("report: %+v", r)
	}
	got := strings.Join(r.Tools, ",")
	for _, want := range []string{"t__echo", "t__env", "t__cwd", "t__pid"} {
		if !strings.Contains(got, want) {
			t.Errorf("tools %s lack %s", got, want)
		}
	}
	if strings.Contains(got, "t__spawn") || strings.Contains(got, "t__set_tools") {
		t.Errorf("tools %s include ones outside the allowlist", got)
	}
	rec := e.sessionGone()
	found := false
	for _, res := range rec.Resources {
		if res.Kind == session.KindProcessGroup && res.PGID > 0 {
			found = true
			if err := syscall.Kill(-res.PGID, 0); err == nil {
				t.Errorf("the server's process group %d outlived the session", res.PGID)
			}
		}
	}
	if !found {
		t.Errorf("session.json records no process group: %+v", rec.Resources)
	}
	var lock map[string]session.MCPResolved
	if err := session.ReadLock(f.Folder, "mcp", &lock); err != nil {
		t.Fatal(err)
	}
	if l := lock["t"]; len(l.Command) != 1 || strings.Join(l.Env, ",") != "PROBE_DELIVERED,SILO_MCPTEST" || l.Restart != "never" || !l.Required {
		t.Errorf("lock.json: %+v", l)
	}
}

func TestMCPServerEnvironment(t *testing.T) {
	e := newEnv(t)
	f := e.silofile(mcpSilofile(t, launched))
	for name, want := range map[string]string{
		"AWS_SECRET_ACCESS_KEY": "<unset>",         // stray in silo's environment
		"SILO_TEST_DELIVERED":   "<unset>",         // only as the credential
		"SILO_TEST_API_KEY":     "sk-from-env",     // inherit_env
		"PROBE_DELIVERED":       "delivered-value", // to: {mcp: t, env: ...}
		"LANG":                  "C.UTF-8",         // the minimal environment
		"LC_ALL":                "<unset>",
		"SILO_MCPTEST":          "stdio", // env:
	} {
		e.fake(fakeConfig{Call: "t__env", Args: map[string]any{"name": name}})
		if res := e.run(f, nil); res.code != 0 {
			t.Fatalf("exit %d (%v)\n%s", res.code, res.err, res.stderr)
		}
		if r := e.report(); r.Result != want {
			t.Errorf("%s = %q (%s), want %q", name, r.Result, r.Error, want)
		}
	}
	e.fake(fakeConfig{Call: "t__cwd"})
	e.run(f, nil)
	if r := e.report(); r.Result != f.Folder {
		t.Errorf("cwd = %q, want the silo folder %s", r.Result, f.Folder)
	}
}

func TestMCPRequiredAndOptional(t *testing.T) {
	e := newEnv(t)
	f := e.silofile(mcpSilofile(t, `mcp:
  - name: t
    command: [SELF]
    env: {SILO_MCPTEST: stdio}
    tools: [echo, ecoh]
`))
	e.fake(fakeConfig{})
	res := e.run(f, nil)
	if res.code != session.ExitPrerequisite || res.err == nil || !strings.Contains(res.err.Error(), "ecoh") {
		t.Fatalf("exit %d (%v), want 69 naming ecoh", res.code, res.err)
	}

	f = e.silofile(mcpSilofile(t, `mcp:
  - name: t
    command: [SELF]
    env: {SILO_MCPTEST: stdio}
    tools: [echo]
  - name: gone
    command: [/nonexistent/mcp-server]
    required: false
`))
	e.fake(fakeConfig{Call: "t__echo", Args: map[string]any{"text": "still here"}})
	res = e.run(f, nil)
	if res.code != 0 || !strings.Contains(res.stderr, "MCP server gone did not start") {
		t.Fatalf("exit %d (%v)\n%s", res.code, res.err, res.stderr)
	}
	if r := e.report(); r.Result != "still here" || strings.Contains(strings.Join(r.Tools, ","), "gone__") {
		t.Fatalf("report: %+v", r)
	}
}

func TestMCPFromConfigRefusesInlineSecrets(t *testing.T) {
	e := newEnv(t)
	self, _ := os.Executable()
	dir := filepath.Join(e.root, "silo", "mcp")
	os.MkdirAll(dir, 0o755)
	conf := fmt.Sprintf(`{"mcpServers": {"t": {"command": %q, "env": {"SILO_MCPTEST": "stdio", "GITHUB_TOKEN": "ghp_inline"}}}}`, self)
	if err := os.WriteFile(filepath.Join(dir, "servers.json"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	f := e.silofile(mcpSilofile(t, `mcp:
  - name: t
    from: mcp-json:t
    tools: [env]
`))
	e.fake(fakeConfig{})
	res := e.run(f, nil)
	if res.code != session.ExitPrerequisite || res.err == nil || !strings.Contains(res.err.Error(), "GITHUB_TOKEN inline") {
		t.Fatalf("exit %d (%v), want the inline secret refused", res.code, res.err)
	}
	checks := session.DryRun(context.Background(), f, e.environ)
	if len(checks) != 1 || checks[0].OK || !strings.Contains(checks[0].Detail, "GITHUB_TOKEN") {
		t.Errorf("dry run: %+v", checks)
	}

	// Re-declared as a credential, the config's value is replaced.
	f = e.silofile(mcpSilofile(t, `  - name: gh
    from: {env: SILO_TEST_DELIVERED}
    to: [{mcp: t, env: GITHUB_TOKEN}]
mcp:
  - name: t
    from: mcp-json:t
    tools: [env]
`))
	e.fake(fakeConfig{Call: "t__env", Args: map[string]any{"name": "GITHUB_TOKEN"}})
	if res := e.run(f, nil); res.code != 0 {
		t.Fatalf("exit %d (%v)\n%s", res.code, res.err, res.stderr)
	}
	if r := e.report(); r.Result != "delivered-value" {
		t.Errorf("GITHUB_TOKEN = %q (%s), want the credential", r.Result, r.Error)
	}
	checks = session.DryRun(context.Background(), f, e.environ)
	if len(checks) != 1 || !checks[0].OK || !strings.Contains(checks[0].Detail, "mcp-json:t in ") ||
		!strings.Contains(checks[0].Detail, "with access to their files") {
		t.Errorf("dry run: %+v", checks)
	}
}

func TestMCPDryRunFailures(t *testing.T) {
	e := newEnv(t)
	f := e.silofile(mcpSilofile(t, `mcp:
  - name: a
    from: claude-user:nope
  - name: b
    command: [no-such-mcp-server]
  - name: c
    where: remote
    command: [x]
`))
	if err := os.WriteFile(filepath.Join(e.home, ".claude.json"), []byte(`{"mcpServers": {"github": {"command": "gh-mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	checks := session.DryRun(context.Background(), f, e.environ)
	want := map[string]string{"mcp a": "not defined", "mcp b": "not found on the server's PATH", "mcp c": "stage 5b"}
	if len(checks) != 3 {
		t.Fatalf("checks: %+v", checks)
	}
	for _, c := range checks {
		if c.OK || !strings.Contains(c.Detail, want[c.Name]) {
			t.Errorf("%s: ok=%v %q, want a failure saying %q", c.Name, c.OK, c.Detail, want[c.Name])
		}
	}
}
