// Package testutil holds test fixtures: git repositories with a bare
// remote, and a fake tool-set MCP server (Streamable HTTP) that the test
// binary itself serves when a start.sh re-executes it with
// SILO_TEST_ROLE=echo-server.
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
	"time"

	"github.com/bwsalmon/silo/internal/mcp"
)

// RoleEnv selects what a re-executed test binary does.
const RoleEnv = "SILO_TEST_ROLE"

// ModeEnv makes the fake server misbehave: "never-listen", "exit",
// "bind-all" (listen on every interface) or "" (behave).
const ModeEnv = "ECHO_MODE"

// MaybeServe runs the fake tool-set server and exits, when the test
// binary was started as one. Call it first in TestMain. The server
// takes its port from its first argument, as a start.sh passes it, and
// checks it against $SILO_PORT.
func MaybeServe() {
	if os.Getenv(RoleEnv) != "echo-server" {
		return
	}
	if len(os.Args) < 2 || os.Args[1] != os.Getenv("SILO_PORT") {
		fmt.Fprintf(os.Stderr, "echo-server: want the port as the only argument, matching $SILO_PORT; got %q, SILO_PORT=%q\n", os.Args[1:], os.Getenv("SILO_PORT"))
		os.Exit(2)
	}
	port := os.Args[1]
	switch os.Getenv(ModeEnv) {
	case "never-listen":
		fmt.Fprintln(os.Stderr, "echo-server: taking my time")
		time.Sleep(time.Hour)
	case "exit":
		fmt.Fprintln(os.Stderr, "echo-server: giving up at once")
		os.Exit(3)
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
	s.Add(mcp.Tool{Name: "grow", Description: "Add a tool named extra and say the list changed.", InputSchema: mcp.Schema(map[string]any{})},
		func(ctx context.Context, args json.RawMessage) *mcp.ToolResult {
			s.Add(mcp.Tool{Name: "extra", InputSchema: mcp.Schema(map[string]any{})},
				func(context.Context, json.RawMessage) *mcp.ToolResult { return mcp.Text("extra") })
			s.NotifyToolsChanged()
			return mcp.Text("grown")
		})
	s.Add(mcp.Tool{Name: "crash", Description: "Exit at once.", InputSchema: mcp.Schema(map[string]any{})},
		func(ctx context.Context, args json.RawMessage) *mcp.ToolResult {
			fmt.Fprintln(os.Stderr, "echo-server: crashing as asked")
			os.Exit(4)
			return nil
		})
	host := "127.0.0.1"
	if os.Getenv(ModeEnv) == "bind-all" {
		host = "0.0.0.0"
	}
	fmt.Fprintf(os.Stderr, "echo-server: listening on %s:%s\n", host, port)
	err := s.ListenAndServe(context.Background(), host+":"+port)
	fmt.Fprintln(os.Stderr, "echo-server:", err)
	os.Exit(1)
}

// EchoToolset is the files of a tool set serving the fake server with
// ECHO_PREFIX=prefix: a toolset.yaml carrying its environment and a
// start.sh that runs the test binary. Extra lines are added to the
// manifest as is.
func EchoToolset(t testing.TB, prefix string, extra ...string) map[string]string {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return EchoToolsetFor(self, prefix, extra...)
}

// EchoToolsetFor is EchoToolset for the binary self.
func EchoToolsetFor(self, prefix string, extra ...string) map[string]string {
	manifest := fmt.Sprintf("description: test echo server\nenv:\n  %s: echo-server\n  ECHO_PREFIX: %s\n", RoleEnv, prefix)
	for _, e := range extra {
		manifest += e + "\n"
	}
	return map[string]string{
		"toolset.yaml": manifest,
		"start.sh":     "exec " + ShellQuote(self) + " \"$1\"\n",
	}
}

// ShellQuote quotes s for a POSIX shell.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Prefixed returns files with every path under dir/.
func Prefixed(dir string, files map[string]string) map[string]string {
	out := map[string]string{}
	for p, c := range files {
		out[dir+"/"+p] = c
	}
	return out
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
