// Package testutil holds test fixtures: git repositories with a bare
// remote, and a fake tool-set MCP server that the test binary itself
// serves when re-executed with SILO_TEST_ROLE=echo-server.
package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/internal/mcp"
)

// RoleEnv selects what a re-executed test binary does.
const RoleEnv = "SILO_TEST_ROLE"

// MaybeServe runs the fake tool-set server and exits, when the test
// binary was started as one. Call it first in TestMain.
func MaybeServe() {
	if os.Getenv(RoleEnv) != "echo-server" {
		return
	}
	s := mcp.NewServer("echo", "1")
	s.Add(mcp.Tool{Name: "echo", Description: "Echo text back, prefixed with $ECHO_PREFIX.", InputSchema: mcp.Schema(map[string]any{"text": map[string]any{"type": "string"}})},
		func(ctx context.Context, args json.RawMessage) *mcp.ToolResult {
			var a struct{ Text string }
			json.Unmarshal(args, &a)
			return mcp.Text(os.Getenv("ECHO_PREFIX") + ":" + a.Text)
		})
	s.Add(mcp.Tool{Name: "env", Description: "List environment variable names.", InputSchema: mcp.Schema(map[string]any{})},
		func(ctx context.Context, args json.RawMessage) *mcp.ToolResult {
			var names []string
			for _, kv := range os.Environ() {
				k, _, _ := strings.Cut(kv, "=")
				names = append(names, k)
			}
			sort.Strings(names)
			return mcp.Text(strings.Join(names, "\n"))
		})
	s.Add(mcp.Tool{Name: "pwd", Description: "Working directory.", InputSchema: mcp.Schema(map[string]any{})},
		func(ctx context.Context, args json.RawMessage) *mcp.ToolResult {
			wd, _ := os.Getwd()
			return mcp.Text(wd)
		})
	s.Serve(context.Background(), os.Stdin, os.Stdout)
	os.Exit(0)
}

// EchoManifest is a toolset.yaml serving the fake server as set name,
// with ECHO_PREFIX=prefix.
func EchoManifest(t testing.TB, name, prefix string) string {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return EchoManifestFor(self, name, prefix)
}

// EchoManifestFor is EchoManifest for the binary self.
func EchoManifestFor(self, name, prefix string) string {
	return fmt.Sprintf("name: %s\ndescription: test echo server\nrun: [%q]\nenv:\n  %s: echo-server\n  ECHO_PREFIX: %s\n", name, self, RoleEnv, prefix)
}

// Git runs git in dir, failing the test on error.
func Git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.com",
		"GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// WriteFiles writes files (relative path -> contents) under dir.
func WriteFiles(t testing.TB, dir string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Remote creates a bare repository at <base>/<name>.git whose main
// branch holds files, and returns its path.
func Remote(t testing.TB, base, name string, files map[string]string) string {
	t.Helper()
	bare := filepath.Join(base, name+".git")
	os.MkdirAll(filepath.Dir(bare), 0o755)
	Git(t, base, "init", "-q", "--bare", "-b", "main", bare)
	seed := t.TempDir()
	Git(t, seed, "clone", "-q", bare, "w")
	w := filepath.Join(seed, "w")
	Git(t, w, "checkout", "-q", "-b", "main")
	if len(files) == 0 {
		files = map[string]string{"README.md": "# context\n"}
	}
	WriteFiles(t, w, files)
	Git(t, w, "add", "-A")
	Git(t, w, "commit", "-q", "-m", "initial")
	Git(t, w, "push", "-q", "origin", "main")
	return bare
}

// PushFiles commits files to branch of the bare remote from a fresh
// clone, as a person working outside the session would.
func PushFiles(t testing.TB, bare, branch string, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	Git(t, dir, "clone", "-q", "-b", branch, bare, "w")
	w := filepath.Join(dir, "w")
	WriteFiles(t, w, files)
	Git(t, w, "add", "-A")
	Git(t, w, "commit", "-q", "-m", "outside change")
	Git(t, w, "push", "-q", "origin", branch)
}
