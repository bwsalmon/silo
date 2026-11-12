package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/internal/audit"
	"github.com/bwsalmon/silo/internal/config"
	"github.com/bwsalmon/silo/internal/mcp"
)

// Reserved tool-set names belong to the built-ins, and vendor to the
// directory of vendored sets.
var Reserved = map[string]bool{"repo-read": true, "repo-write": true, "silo": true, "scratch": true, config.VendorName: true}

// ScratchPrefix namespaces one-off tool sets: scratch.<name>.
const ScratchPrefix = "scratch" + Sep

// MCPPath is the endpoint a tool set's server answers MCP on.
const MCPPath = "/mcp"

// StartTimeout bounds how long a tool set's server has, from launch, to
// listen on its port and answer initialize. It is generous because an
// entry point may install or build things first.
var StartTimeout = 2 * time.Minute

// StopGrace is how long a tool set's processes have between SIGTERM and
// SIGKILL.
var StopGrace = 5 * time.Second

// Host starts, reloads and stops the servers of repo, vendored and
// scratch tool sets, and puts them behind the router.
type Host struct {
	Router     *Router
	Checkout   string
	SessionDir string
	// Disabled names repo tool sets config keeps out: a set's name, or a
	// vendor alias for all of that alias's sets.
	Disabled map[string]bool
	Log      audit.Logger

	mu       sync.Mutex // one load at a time
	children sync.Map   // tool-set name -> *Child currently loaded
}

// LoadAll loads every tool set in the checkout's .silo/tools, returning a
// report line per set.
func (h *Host) LoadAll(ctx context.Context) (string, bool) {
	keys, err := config.RepoToolsets(h.Checkout)
	if err != nil {
		return "list " + config.ToolsDir + ": " + err.Error(), false
	}
	return h.ReloadRepoSets(ctx, keys)
}

// ReloadRepoSets (re)loads the repo tool sets with the given keys (see
// config.RepoToolsets) from the checkout: a new set starts, a changed
// one restarts on a new port, a deleted one stops. It returns a
// human-readable report and whether every set succeeded.
func (h *Host) ReloadRepoSets(ctx context.Context, keys []string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sort.Strings(keys)
	var lines []string
	ok := true
	for _, key := range keys {
		name := config.ToolsetName(key)
		line, err := h.loadRepoSet(ctx, key)
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

// ExitedRepoSets lists the keys of loaded repo tool sets whose server
// has exited, for silo.refresh to restart.
func (h *Host) ExitedRepoSets() []string {
	var keys []string
	h.children.Range(func(_, v any) bool {
		if c := v.(*Child); c.key != "" && c.Exited() {
			keys = append(keys, c.key)
		}
		return true
	})
	sort.Strings(keys)
	return keys
}

func (h *Host) loadRepoSet(ctx context.Context, key string) (string, error) {
	name := config.ToolsetName(key)
	dir := filepath.Join(h.Checkout, config.ToolsDir, filepath.FromSlash(key))
	// A directory a deletion left empty is as good as gone: git does
	// not keep empty directories.
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(ents) == 0) || isFile(dir) {
		if h.Router.Remove(name) {
			return "removed " + name, nil
		}
		return "nothing to load for " + name, nil
	} else if err != nil {
		return "", err
	}
	if err := config.ValidToolsetKey(key); err != nil {
		return "", err
	}
	alias, _, vendored := config.VendoredKey(key)
	if !vendored && Reserved[key] {
		return "", fmt.Errorf("%q is a built-in tool set's name; rename the directory", key)
	}
	// Tool-set names stay unique: a vendor alias may not also be one of
	// the repository's own sets, or <alias>.<set>.<tool> and
	// <set>.<tool> could name the same thing.
	clash := alias
	if !vendored {
		clash = key
	}
	if vendored && (Reserved[alias] || h.dirExists(config.ToolsDir, alias)) || !vendored && h.dirExists(config.VendorDir, clash) {
		return "", fmt.Errorf("%q is both a tool set in %s/ and a vendor alias in %s/; rename one", clash, config.ToolsDir, config.VendorDir)
	}
	if h.Disabled[name] || (vendored && h.Disabled[alias]) {
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
	child.key = key
	if err := h.add(ctx, name, child, Options{Origin: Repo, RequiresApproval: m.RequiresApproval}); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s %s (tools: %s)", verb, name, h.toolNames(name)), nil
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func (h *Host) dirExists(parts ...string) bool {
	fi, err := os.Stat(filepath.Join(append([]string{h.Checkout}, parts...)...))
	return err == nil && fi.IsDir()
}

func (h *Host) add(ctx context.Context, name string, child *Child, opts Options) error {
	if err := h.Router.Add(ctx, name, child, opts); err != nil {
		child.Close()
		return err
	}
	h.children.Store(name, child)
	return nil
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
	if err := h.add(ctx, full, child, Options{Origin: Scratch, RequiresApproval: m.RequiresApproval}); err != nil {
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

// start runs a tool set's entry point with a free loopback port, waits
// for its server to answer MCP there, and checks it bound to loopback
// only.
func (h *Host) start(ctx context.Context, name, dir string, m *config.ToolsetManifest) (*Child, error) {
	entry, err := config.DirEntrypoint(dir)
	if err != nil {
		return nil, err
	}
	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("pick a port: %w", err)
	}
	stateDir := filepath.Join(h.SessionDir, "state", name)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	vars := map[string]string{
		"PORT":        strconv.Itoa(port),
		"TOOLSET":     name,
		"TOOLSET_DIR": dir,
		"STATE_DIR":   stateDir,
		"SESSION_DIR": h.SessionDir,
		"CHECKOUT":    h.Checkout,
	}
	logPath := filepath.Join(h.SessionDir, "logs", name+".log")
	os.MkdirAll(filepath.Dir(logPath), 0o700)
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	offset, _ := logf.Seek(0, io.SeekEnd)
	argv := []string{config.Interpreter(entry), entry, strconv.Itoa(port)}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = childEnv(m, vars)
	cmd.Stdout = logf
	cmd.Stderr = logf
	ownGroup(cmd)
	if err := cmd.Start(); err != nil {
		logf.Close()
		return nil, fmt.Errorf("start %s: %w", strings.Join(argv, " "), err)
	}
	c := &Child{
		name: name, manifest: m, cmd: cmd, port: port,
		log: logf, logPath: logPath, logOffset: offset,
		exited: make(chan struct{}),
	}
	c.onToolsChanged = func() { h.Router.Refresh(context.Background(), name) }
	c.onClose = func() { h.children.CompareAndDelete(name, c) }
	go func() {
		err := cmd.Wait()
		c.waitErr = err
		close(c.exited)
		status := "exited"
		if err != nil {
			status = err.Error()
		}
		h.Log.Log("toolset_exited", map[string]any{"toolset": name, "port": port, "status": status})
	}()
	h.Log.Log("toolset_started", map[string]any{"toolset": name, "entrypoint": entry, "port": port, "pid": cmd.Process.Pid})

	sctx, cancel := context.WithTimeout(ctx, StartTimeout)
	defer cancel()
	if err := c.waitReady(sctx); err != nil {
		c.Close()
		return nil, fmt.Errorf("%v\noutput (%s):\n%s", err, logPath, c.tail())
	}
	if err := checkLoopback(cmd.Process.Pid, port); err != nil {
		c.Close()
		return nil, fmt.Errorf("refused: %v; a tool set's server must bind to 127.0.0.1 only", err)
	}
	return c, nil
}

// freePort asks the kernel for a free loopback port. The server binds
// it a moment later; nothing else is likely to take it in between.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// Child is a tool set served by a child process over MCP's Streamable
// HTTP transport on a loopback port.
type Child struct {
	name      string
	key       string // repo tool sets' key; "" for scratch
	manifest  *config.ToolsetManifest
	cmd       *exec.Cmd
	port      int
	client    *mcp.HTTPClient
	log       *os.File
	logPath   string
	logOffset int64
	exited    chan struct{}
	waitErr   error
	once      sync.Once
	// onToolsChanged runs when the server says its tools changed;
	// onClose when the child is closed.
	onToolsChanged, onClose func()
}

// Port is the loopback port the server listens on.
func (c *Child) Port() int { return c.port }

// Pid is the entry point's process id, which is also its process group.
func (c *Child) Pid() int { return c.cmd.Process.Pid }

// Exited reports whether the server's process has exited.
func (c *Child) Exited() bool {
	select {
	case <-c.exited:
		return true
	default:
		return false
	}
}

// waitReady polls the port until it accepts a connection, then performs
// the MCP initialize handshake.
func (c *Child) waitReady(ctx context.Context) error {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(c.port))
	for {
		select {
		case <-c.exited:
			return fmt.Errorf("%s exited (%v) before it served MCP on %s", c.name, c.exitStatus(), addr)
		case <-ctx.Done():
			return fmt.Errorf("%s did not listen on %s within %s", c.name, addr, StartTimeout)
		default:
		}
		conn, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		select {
		case <-c.exited:
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
	c.client = mcp.NewHTTPClient("http://" + addr + MCPPath)
	c.client.OnToolsChanged = c.onToolsChanged
	if err := c.client.Start(ctx, "silo"); err != nil {
		if c.Exited() {
			return fmt.Errorf("%s exited (%v) during the MCP handshake", c.name, c.exitStatus())
		}
		return fmt.Errorf("MCP initialize with %s at http://%s%s failed: %v", c.name, addr, MCPPath, err)
	}
	return nil
}

func (c *Child) exitStatus() string {
	if c.waitErr != nil {
		return c.waitErr.Error()
	}
	return "status 0"
}

// tail is the end of what the server wrote to stdout and stderr since
// this start.
func (c *Child) tail() string {
	f, err := os.Open(c.logPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	const max = 8 * 1024
	fi, _ := f.Stat()
	from := c.logOffset
	if fi != nil && fi.Size()-from > max {
		from = fi.Size() - max
	}
	f.Seek(from, io.SeekStart)
	data, _ := io.ReadAll(io.LimitReader(f, max))
	if len(data) == 0 {
		return "(nothing)"
	}
	return string(data)
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
	if c.Exited() {
		return nil, fmt.Errorf("%s's server has exited (%v); silo.refresh restarts it. Its output:\n%s", c.name, c.exitStatus(), c.tail())
	}
	return c.client.CallTool(ctx, tool, args)
}

// Close stops the server: its MCP session ends, then its whole process
// group gets SIGTERM and, after StopGrace, SIGKILL.
func (c *Child) Close() error {
	c.once.Do(func() {
		c.onClose()
		if c.client != nil {
			c.client.Close()
		}
		stopGroup(c.cmd.Process.Pid, c.exited, StopGrace)
		c.log.Close()
	})
	return nil
}

// childEnv is all a child server gets: PATH, HOME, TMPDIR and silo's Go
// toolchain variables (so the entry point can find interpreters,
// toolchains and their caches), silo's variables, and what its manifest
// declares.
func childEnv(m *config.ToolsetManifest, vars map[string]string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "PATH" || k == "HOME" || k == "TMPDIR" || strings.HasPrefix(k, "GO") {
			env = append(env, kv)
		}
	}
	for _, k := range []string{"PORT", "TOOLSET", "TOOLSET_DIR", "STATE_DIR", "SESSION_DIR", "CHECKOUT"} {
		env = append(env, "SILO_"+k+"="+vars[k])
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

// expand substitutes ${TOOLSET_DIR}, ${STATE_DIR}, ${CHECKOUT} and the
// other silo variables in a manifest's env values.
func expand(s string, vars map[string]string) string {
	for k, v := range vars {
		s = strings.ReplaceAll(s, "${"+k+"}", v)
	}
	return s
}

// ChangedToolsets names the keys of the tool sets touched by a list of
// checkout-relative paths: "<set>" for .silo/tools/<set>/..., and
// "vendor/<alias>/<set>" for .silo/tools/vendor/<alias>/<set>/....
func ChangedToolsets(paths []string) []string {
	seen := map[string]bool{}
	var keys []string
	prefix := config.ToolsDir + "/"
	for _, p := range paths {
		p = filepath.ToSlash(p)
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(p, prefix), "/")
		key := ""
		switch {
		case len(parts) < 2:
			continue // a file directly in .silo/tools
		case parts[0] == config.VendorName:
			if len(parts) < 4 {
				continue // VENDOR.lock, or a file directly in vendor/
			}
			key = strings.Join(parts[:3], "/")
		default:
			key = parts[0]
		}
		if key != "" && !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}
