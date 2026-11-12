package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/internal/audit"
	"github.com/bwsalmon/silo/internal/config"
	"github.com/bwsalmon/silo/internal/mcp"
)

// Reserved tool-set names belong to the built-ins.
var Reserved = map[string]bool{"repo-read": true, "repo-write": true, "silo": true, "scratch": true}

// ScratchPrefix namespaces one-off tool sets: scratch.<name>.
const ScratchPrefix = "scratch" + Sep

// BuildTimeout bounds a tool set's build step.
var BuildTimeout = 5 * time.Minute

// StartTimeout bounds a child server's initialize handshake.
var StartTimeout = 30 * time.Second

// Host starts, reloads and stops the child MCP servers of repo and
// scratch tool sets, and puts them behind the router.
type Host struct {
	Router     *Router
	Checkout   string
	SessionDir string
	// Disabled names repo tool sets config keeps out.
	Disabled map[string]bool
	Log      audit.Logger

	mu sync.Mutex // one load at a time
}

// LoadAll loads every tool set in the checkout's .silo/tools, returning a
// report line per set.
func (h *Host) LoadAll(ctx context.Context) (string, bool) {
	names, err := config.RepoToolsets(h.Checkout)
	if err != nil {
		return "list " + config.ToolsDir + ": " + err.Error(), false
	}
	return h.ReloadRepoSets(ctx, names)
}

// ReloadRepoSets (re)loads the named repo tool sets from the checkout:
// a new set starts, a changed one restarts in place, a deleted one stops.
// It returns a human-readable report and whether every set succeeded.
func (h *Host) ReloadRepoSets(ctx context.Context, names []string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sort.Strings(names)
	var lines []string
	ok := true
	for _, name := range names {
		line, err := h.loadRepoSet(ctx, name)
		if err != nil {
			ok = false
			line = fmt.Sprintf("FAILED %s: %v", name, err)
			if h.Router.Has(name) {
				line += "\n  (the previous version of " + name + " is still running)"
			}
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"), ok
}

func (h *Host) loadRepoSet(ctx context.Context, name string) (string, error) {
	dir := filepath.Join(h.Checkout, config.ToolsDir, name)
	if _, err := os.Stat(filepath.Join(dir, config.Manifest)); errors.Is(err, os.ErrNotExist) {
		if h.Router.Remove(name) {
			return "removed " + name, nil
		}
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return "", fmt.Errorf("%s has no %s", filepath.Join(config.ToolsDir, name), config.Manifest)
		}
		return "nothing to load for " + name, nil
	}
	if Reserved[name] {
		return "", fmt.Errorf("%q is a built-in tool set's name; rename the directory", name)
	}
	if h.Disabled[name] {
		return "skipped " + name + ": disabled by config", nil
	}
	m, err := config.LoadManifest(dir)
	if err != nil {
		return "", err
	}
	verb := "loaded"
	if h.Router.Has(name) {
		verb = "reloaded"
	}
	child, err := h.start(ctx, name, dir, m)
	if err != nil {
		return "", err
	}
	if err := h.Router.Add(ctx, name, child, Options{Origin: Repo, RequiresApproval: m.RequiresApproval}); err != nil {
		child.Close()
		return "", err
	}
	return fmt.Sprintf("%s %s (tools: %s)", verb, name, h.toolNames(name)), nil
}

// LoadScratch (re)loads the one-off tool set in dir as scratch.<name>.
func (h *Host) LoadScratch(ctx context.Context, name, dir string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m, err := config.LoadManifest(dir)
	if err != nil {
		return "", err
	}
	full := ScratchPrefix + name
	child, err := h.start(ctx, full, dir, m)
	if err != nil {
		return "", err
	}
	if err := h.Router.Add(ctx, full, child, Options{Origin: Scratch, RequiresApproval: m.RequiresApproval}); err != nil {
		child.Close()
		return "", err
	}
	return fmt.Sprintf("loaded %s (tools: %s)", full, h.toolNames(full)), nil
}

// UnloadScratch stops one-off tool set scratch.<name>.
func (h *Host) UnloadScratch(name string) bool {
	return h.Router.Remove(ScratchPrefix + name)
}

func (h *Host) toolNames(set string) string {
	var names []string
	for _, t := range h.Router.Tools() {
		if strings.HasPrefix(t.Name, set+Sep) && !strings.Contains(strings.TrimPrefix(t.Name, set+Sep), Sep) {
			names = append(names, t.Name)
		}
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// start builds (if the manifest says how) and starts a child server.
func (h *Host) start(ctx context.Context, name, dir string, m *config.ToolsetManifest) (*Child, error) {
	buildDir := filepath.Join(h.SessionDir, "build", name)
	os.RemoveAll(buildDir)
	if err := os.MkdirAll(buildDir, 0o700); err != nil {
		return nil, err
	}
	vars := map[string]string{"TOOLSET_DIR": dir, "BUILD_DIR": buildDir, "CHECKOUT": h.Checkout}
	env := childEnv(m, vars)
	if len(m.Build) > 0 {
		bctx, cancel := context.WithTimeout(ctx, BuildTimeout)
		defer cancel()
		argv := expandAll(m.Build, vars)
		cmd := exec.CommandContext(bctx, resolveArgv0(argv[0], dir), argv[1:]...)
		cmd.Dir = dir
		cmd.Env = append(env, buildExtraEnv()...)
		out, err := cmd.CombinedOutput()
		h.Log.Log("toolset_build", map[string]any{"toolset": name, "ok": err == nil, "output": clip(string(out), 16*1024)})
		if err != nil {
			return nil, fmt.Errorf("build failed (%v):\n%s", err, clip(string(out), 16*1024))
		}
	}
	argv := expandAll(m.Run, vars)
	cmd := exec.Command(resolveArgv0(argv[0], dir), argv[1:]...)
	cmd.Dir = h.Checkout
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	logPath := filepath.Join(h.SessionDir, "logs", name+".stderr")
	os.MkdirAll(filepath.Dir(logPath), 0o700)
	stderr, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		stderr.Close()
		return nil, fmt.Errorf("start %s: %w", strings.Join(argv, " "), err)
	}
	c := &Child{name: name, manifest: m, cmd: cmd, stdin: stdin, stderr: stderr, exited: make(chan struct{})}
	go func() { cmd.Wait(); close(c.exited) }()
	c.client = mcp.NewClient(stdout, stdin)
	c.client.OnToolsChanged = func() { h.Router.Refresh(context.Background(), name) }
	sctx, cancel := context.WithTimeout(ctx, StartTimeout)
	defer cancel()
	if err := c.client.Start(sctx, "silo"); err != nil {
		c.Close()
		tail, _ := os.ReadFile(logPath)
		return nil, fmt.Errorf("MCP handshake with %s failed: %v\nstderr:\n%s", name, err, clip(string(tail), 8*1024))
	}
	return c, nil
}

// Child is a tool set served by a child process.
type Child struct {
	name     string
	manifest *config.ToolsetManifest
	cmd      *exec.Cmd
	stdin    io.Closer
	stderr   io.Closer
	client   *mcp.Client
	exited   chan struct{}
	once     sync.Once
}

// Tools lists the server's tools, filtered by the manifest's allowlist.
func (c *Child) Tools(ctx context.Context) ([]mcp.Tool, error) {
	tools, err := c.client.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	if len(c.manifest.Tools) == 0 {
		return tools, nil
	}
	allow := map[string]config.ToolDecl{}
	for _, d := range c.manifest.Tools {
		allow[d.Name] = d
	}
	var out []mcp.Tool
	for _, t := range tools {
		if d, ok := allow[t.Name]; ok {
			if d.Description != "" {
				t.Description = d.Description
			}
			out = append(out, t)
		}
	}
	return out, nil
}

// Call calls one of the server's tools.
func (c *Child) Call(ctx context.Context, tool string, args json.RawMessage) (*mcp.ToolResult, error) {
	if len(c.manifest.Tools) > 0 {
		found := false
		for _, d := range c.manifest.Tools {
			found = found || d.Name == tool
		}
		if !found {
			return nil, fmt.Errorf("tool %q is not in %s's manifest", tool, c.name)
		}
	}
	return c.client.CallTool(ctx, tool, args)
}

// Close stops the server: its stdin closes, and it is killed if it has
// not exited two seconds later.
func (c *Child) Close() error {
	c.once.Do(func() {
		c.stdin.Close()
		select {
		case <-c.exited:
		case <-time.After(2 * time.Second):
			c.cmd.Process.Kill()
			<-c.exited
		}
		c.stderr.Close()
	})
	return nil
}

// childEnv is all a child server gets: PATH (so its interpreter can be
// found), silo's variables, and what its manifest declares.
func childEnv(m *config.ToolsetManifest, vars map[string]string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"SILO_TOOLSET_DIR=" + vars["TOOLSET_DIR"],
		"SILO_BUILD_DIR=" + vars["BUILD_DIR"],
		"SILO_CHECKOUT=" + vars["CHECKOUT"],
	}
	for _, k := range m.PassEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	keys := make([]string, 0, len(m.Env))
	for k := range m.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+expand(m.Env[k], vars))
	}
	return env
}

// buildExtraEnv is what a build also needs from silo's environment to
// find its toolchain's caches: HOME, TMPDIR and Go's variables.
func buildExtraEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "HOME" || k == "TMPDIR" || strings.HasPrefix(k, "GO") {
			env = append(env, kv)
		}
	}
	return env
}

func expand(s string, vars map[string]string) string {
	for k, v := range vars {
		s = strings.ReplaceAll(s, "${"+k+"}", v)
	}
	return s
}

func expandAll(argv []string, vars map[string]string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = expand(a, vars)
	}
	return out
}

// resolveArgv0 makes a ./relative command relative to the tool set's
// directory rather than the checkout the server runs in.
func resolveArgv0(cmd, dir string) string {
	if strings.HasPrefix(cmd, "./") || strings.HasPrefix(cmd, "../") {
		return filepath.Join(dir, cmd)
	}
	return cmd
}

// ChangedToolsets names the tool sets touched by a list of
// checkout-relative paths.
func ChangedToolsets(paths []string) []string {
	seen := map[string]bool{}
	var names []string
	prefix := config.ToolsDir + "/"
	for _, p := range paths {
		p = filepath.ToSlash(p)
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimPrefix(p, prefix), "/")
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
