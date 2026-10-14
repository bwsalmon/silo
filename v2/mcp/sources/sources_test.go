package sources

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fixture is a home directory and a silo folder holding one config per
// source, read from testdata.
func fixture(t *testing.T) Options {
	t.Helper()
	root := t.TempDir()
	home, folder := filepath.Join(root, "home"), filepath.Join(root, "silo")
	project := filepath.Join(root, "work", "widgets")
	claude, err := os.ReadFile("testdata/claude.json")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(home, ".claude.json"):                         strings.ReplaceAll(string(claude), "PROJECT", project),
		filepath.Join(home, ".codex", "config.toml"):                "testdata/codex.toml",
		filepath.Join(home, ".gemini", "config", "mcp_config.json"): "testdata/agy.json",
		filepath.Join(folder, "mcp", "servers.json"):                "testdata/servers.json",
		filepath.Join(folder, "other.mcp.json"):                     "testdata/other.mcp.json",
	}
	for dst, src := range files {
		data := src
		if strings.HasPrefix(src, "testdata/") {
			b, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}
			data = string(b)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := map[string]string{"HOME": home}
	return Options{Folder: folder, Getenv: func(k string) string { return env[k] }}
}

func resolve(t *testing.T, o Options, from string) (*Entry, error) {
	t.Helper()
	ref, err := Parse(from)
	if err != nil {
		t.Fatalf("Parse(%q): %v", from, err)
	}
	return Resolve(ref, o)
}

func TestSources(t *testing.T) {
	o := fixture(t)
	project := filepath.Join(filepath.Dir(o.Folder), "work", "widgets")
	for _, tc := range []struct {
		from    string
		command []string
		url     string
		env     []string
		headers []string
		file    string
	}{
		{from: "claude-user:github", command: []string{"github-mcp-server", "stdio"}, env: []string{"GITHUB_PERSONAL_ACCESS_TOKEN"}, file: ".claude.json"},
		{from: "claude-user:tickets", url: "http://127.0.0.1:7400/mcp", headers: []string{"Authorization"}, file: ".claude.json"},
		{from: "claude-project:lint@" + project, command: []string{"lint-mcp"}, file: ".claude.json"},
		{from: "mcp-json:docs", command: []string{"docs-search-mcp", "--index", "/idx"}, env: []string{"LOG_LEVEL"}, file: "servers.json"},
		{from: "mcp-json:other@other.mcp.json", url: "https://example.com/mcp", file: "other.mcp.json"},
		{from: "codex:postgres", command: []string{"pg-mcp", "--readonly"}, env: []string{"DATABASE_URL", "PGAPPNAME"}, file: "config.toml"},
		{from: "codex:remote", url: "https://mcp.example.com/mcp", headers: []string{"Authorization", "X-Team"}, file: "config.toml"},
		{from: "agy:search", command: []string{"search-mcp"}, file: "mcp_config.json"},
		{from: "agy:web", url: "https://web.example.com/mcp", file: "mcp_config.json"},
	} {
		t.Run(tc.from, func(t *testing.T) {
			e, err := resolve(t, o, tc.from)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(e.Command, tc.command) || e.URL != tc.url {
				t.Errorf("command %q url %q, want %q %q", e.Command, e.URL, tc.command, tc.url)
			}
			if got := e.EnvNames(); len(got)+len(tc.env) > 0 && !reflect.DeepEqual(got, tc.env) {
				t.Errorf("env %v, want %v", got, tc.env)
			}
			if got := e.HeaderNames(); len(got)+len(tc.headers) > 0 && !reflect.DeepEqual(got, tc.headers) {
				t.Errorf("headers %v, want %v", got, tc.headers)
			}
			if filepath.Base(e.File) != tc.file || e.From != tc.from {
				t.Errorf("file %s from %s", e.File, e.From)
			}
		})
	}
}

func TestCodexHomeAndNotes(t *testing.T) {
	o := fixture(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[mcp_servers.x]\ncommand = \"x\"\nenv_vars = [\"FOO\"]\ncwd = \"/tmp\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	home := o.Getenv("HOME")
	o.Getenv = func(k string) string {
		return map[string]string{"HOME": home, "CODEX_HOME": dir}[k]
	}
	e, err := resolve(t, o, "codex:x")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Join(e.Notes, "\n"); !strings.Contains(n, "env_vars FOO") || !strings.Contains(n, "cwd") {
		t.Errorf("notes = %q", n)
	}
}

func TestMCPJSONDefaultFallsBackToDotMCPJSON(t *testing.T) {
	o := fixture(t)
	if err := os.Remove(filepath.Join(o.Folder, "mcp", "servers.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(t, o, "mcp-json:docs"); err == nil || !strings.Contains(err.Error(), "neither mcp/servers.json nor .mcp.json") {
		t.Fatalf("err = %v", err)
	}
	if err := os.WriteFile(filepath.Join(o.Folder, ".mcp.json"), []byte(`{"mcpServers":{"docs":{"command":"d"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := resolve(t, o, "mcp-json:docs")
	if err != nil || filepath.Base(e.File) != ".mcp.json" {
		t.Fatalf("%+v, %v", e, err)
	}
}

func TestNotFoundNamesWhatIsDefined(t *testing.T) {
	o := fixture(t)
	_, err := resolve(t, o, "claude-user:gitlab")
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "github, tickets") {
		t.Fatalf("err = %v", err)
	}
	_, err = resolve(t, o, "claude-project:lint@/no/such/dir")
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "/no/such/dir") {
		t.Fatalf("err = %v", err)
	}
	_, err = resolve(t, o, "codex:nope")
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "postgres, remote") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseRefusesBareAndMalformedNames(t *testing.T) {
	for from, want := range map[string]string{
		"github":                 "names no source",
		"vscode:github":          "unknown source",
		"claude-user:":           "names no server",
		"claude-project:lint":    "needs the project's directory",
		"claude-user:github@dir": "takes no @",
		"codex:pg@x":             "takes no @",
		"mcp-json:docs@":         "empty @",
	} {
		if _, err := Parse(from); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q) = %v, want %q", from, err, want)
		}
	}
	r, err := Parse("mcp-json:docs@mcp/x.json")
	if err != nil || r.Arg != "mcp/x.json" || r.String() != "mcp-json:docs@mcp/x.json" {
		t.Errorf("Parse = %+v, %v", r, err)
	}
}

func TestLooksSecret(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		want        bool
	}{
		{"GITHUB_PERSONAL_ACCESS_TOKEN", "ghp_abc123", true},
		{"GITHUB_PERSONAL_ACCESS_TOKEN", "${GITHUB_TOKEN}", false},
		{"API_KEY", "anything", true},
		{"DATABASE_PASSWORD", "hunter2", true},
		{"LOG_LEVEL", "debug", false},
		{"DATABASE_URL", "postgres://ro:hunter2@db/widgets", true},
		{"DATABASE_URL", "postgres://db/widgets", false},
		{"OPENAI_BASE", "sk-proj-abcdef", true},
		{"Authorization", "Bearer ${TOKEN}", false},
		{"Authorization", "Bearer abc", true},
		{"X-Team", "widgets", false},
		{"X-Api-Key", "k", true},
		{"API_KEY", "", false},
	} {
		if got := LooksSecret(tc.name, tc.value); got != tc.want {
			t.Errorf("LooksSecret(%q, %q) = %v, want %v", tc.name, tc.value, got, tc.want)
		}
	}
}

func TestExpandAndRefs(t *testing.T) {
	look := func(k string) (string, bool) {
		v, ok := map[string]string{"A": "1"}[k]
		return v, ok
	}
	if got := Expand("x${A}y${B:-d}z${C}", look); got != "x1ydz" {
		t.Errorf("Expand = %q", got)
	}
	if got := Refs("Bearer ${TOK} ${B:-x}"); !reflect.DeepEqual(got, []string{"TOK", "B"}) {
		t.Errorf("Refs = %v", got)
	}
}
