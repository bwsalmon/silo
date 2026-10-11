// Package tools is silo-tools' MCP server: the tools an agent's
// harness uses on its tool VM (agents.md, "Tools", SV-105) -- exec,
// read_file, write_file, edit_file and list_dir -- modelled on grain's
// sandbox tools (pkg/mcp/sandbox_tools.go), whose behaviour this copies
// without importing it.
//
// The server speaks MCP's JSON-RPC over HTTP POST at /mcp. It never
// listens: cmd/silo-tools serves it down the tool channel it dials
// (package toolchan).
package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Limits on what a tool returns and how long exec runs.
const (
	// MaxOutput caps each of exec's stdout and stderr, and a read_file
	// or list_dir result. What is cut is said, with how much.
	MaxOutput = 64 << 10
	// DefaultTimeout and MaxTimeout bound exec.
	DefaultTimeout = 2 * time.Minute
	MaxTimeout     = 30 * time.Minute
	// ResultTTL is how long a finished exec's result is kept, so that a
	// call the channel interrupted can be fetched again by its call_id.
	ResultTTL = 10 * time.Minute
	// MaxListEntries caps list_dir.
	MaxListEntries = 2000
)

// ProtocolVersion is the MCP revision the server answers initialize
// with.
const ProtocolVersion = "2025-06-18"

// Options configure a Server.
type Options struct {
	// User is the user exec runs commands as, and that owns the files
	// write_file makes, when the server runs as root: "agent" in a tool
	// VM. Empty, or not root, runs as the server's own user.
	User string
	// Proxy is the egress proxy's URL, set as HTTPS_PROXY and HTTP_PROXY
	// in exec's environment (call 41): http://silo.internal:3128.
	Proxy string
	// NoProxy is NO_PROXY: what exec's commands reach directly.
	NoProxy string
	// Dir is the working directory exec uses when a call names none;
	// the user's home if empty.
	Dir string
	// Now is the clock; time.Now if nil. A test moves it.
	Now func() time.Time
}

// Server is the MCP server.
type Server struct {
	opts Options
	cred *syscall.Credential
	home string

	mu    sync.Mutex
	calls map[string]*call
}

// call is one exec by its call_id: running until done is closed.
type call struct {
	done     chan struct{}
	result   Result
	finished time.Time
}

// New returns a Server. It fails if opts.User is named, the server is
// root, and there is no such user.
func New(opts Options) (*Server, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Server{opts: opts, calls: map[string]*call{}}
	s.home = os.Getenv("HOME")
	if opts.User != "" && os.Geteuid() == 0 {
		cred, home, err := lookupUser(opts.User)
		if err != nil {
			return nil, err
		}
		s.cred, s.home = cred, home
	}
	if s.home == "" {
		s.home = "/"
	}
	return s, nil
}

// Result is a tool call's result.
type Result struct {
	Text    string
	IsError bool
}

// Tool is one tool: its name, description, input schema and handler.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	handler     func(ctx context.Context, args map[string]any) Result
}

// Tools are the server's tools, in the order tools/list gives them.
func (s *Server) Tools() []Tool {
	str := map[string]any{"type": "string"}
	in := func(props map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "additionalProperties": false, "properties": props, "required": required}
	}
	return []Tool{
		{Name: "exec", Description: "Run a command with bash -lc on the tool VM, as the user agent (passwordless sudo). " +
			"Returns the exit status, stdout and stderr, each capped. Give call_id to fetch the result of a call that was interrupted; it is kept for ten minutes.",
			InputSchema: in(map[string]any{
				"command":         str,
				"workdir":         map[string]any{"type": "string", "description": "Working directory; the agent's home if omitted"},
				"timeout_seconds": map[string]any{"type": "integer", "description": "Seconds before the command is killed; default 120, at most 1800"},
				"call_id":         map[string]any{"type": "string", "description": "An id of the caller's choosing: a repeated call_id returns the first call's result rather than running again"},
			}, "command"),
			handler: s.exec},
		{Name: "read_file", Description: "Read a file, or a range of its lines, numbered.",
			InputSchema: in(map[string]any{
				"file_path": str,
				"offset":    map[string]any{"type": "integer", "description": "Line number to start from"},
				"limit":     map[string]any{"type": "integer", "description": "Number of lines to read"},
			}, "file_path"),
			handler: s.readFile},
		{Name: "write_file", Description: "Create or overwrite a file.",
			InputSchema: in(map[string]any{"file_path": str, "content": str}, "file_path", "content"),
			handler:     s.writeFile},
		{Name: "edit_file", Description: "Replace an exact string in a file, once or everywhere.",
			InputSchema: in(map[string]any{"file_path": str, "old_string": str, "new_string": str,
				"replace_all": map[string]any{"type": "boolean", "default": false}}, "file_path", "old_string", "new_string"),
			handler: s.editFile},
		{Name: "list_dir", Description: "List a directory, optionally recursively, to a depth.",
			InputSchema: in(map[string]any{"path": str,
				"recursive": map[string]any{"type": "boolean", "default": false},
				"depth":     map[string]any{"type": "integer", "description": "How many levels below path a recursive listing goes; default 3"}}, "path"),
			handler: s.listDir},
	}
}

// Call runs the tool name with args.
func (s *Server) Call(ctx context.Context, name string, args map[string]any) Result {
	for _, t := range s.Tools() {
		if t.Name == name {
			if args == nil {
				args = map[string]any{}
			}
			return t.handler(ctx, args)
		}
	}
	return Result{Text: "no such tool: " + name, IsError: true}
}

// Env is the environment exec gives a command: a fixed PATH, the
// user's HOME, USER and LOGNAME, and the egress proxy's variables in
// both cases.
func (s *Server) Env() []string {
	user := s.opts.User
	if s.cred == nil {
		user = os.Getenv("USER")
	}
	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=" + s.home, "USER=" + user, "LOGNAME=" + user, "SHELL=/bin/bash", "LANG=C.UTF-8", "TERM=dumb",
	}
	if s.opts.Proxy != "" {
		for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy"} {
			env = append(env, k+"="+s.opts.Proxy)
		}
	}
	if s.opts.NoProxy != "" {
		env = append(env, "NO_PROXY="+s.opts.NoProxy, "no_proxy="+s.opts.NoProxy)
	}
	return env
}

func (s *Server) exec(ctx context.Context, args map[string]any) Result {
	command, _ := args["command"].(string)
	if command == "" {
		return Result{Text: "command is required", IsError: true}
	}
	id, _ := args["call_id"].(string)
	if id == "" {
		id = newID()
	}
	s.mu.Lock()
	s.expire()
	c, seen := s.calls[id]
	if !seen {
		c = &call{done: make(chan struct{})}
		s.calls[id] = c
	}
	s.mu.Unlock()
	if !seen {
		// The command outlives the call that started it: a channel that
		// drops mid-command leaves its result to be fetched.
		go func() {
			r := s.run(command, args)
			s.mu.Lock()
			c.result, c.finished = r, s.opts.Now()
			s.mu.Unlock()
			close(c.done)
		}()
	}
	select {
	case <-c.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return c.result
	case <-ctx.Done():
		return Result{Text: fmt.Sprintf("the call ended before the command did; it goes on running, and exec with call_id %q fetches its result", id), IsError: true}
	}
}

// expire forgets results older than ResultTTL. s.mu is held.
func (s *Server) expire() {
	now := s.opts.Now()
	for id, c := range s.calls {
		if !c.finished.IsZero() && now.Sub(c.finished) > ResultTTL {
			delete(s.calls, id)
		}
	}
}

func (s *Server) run(command string, args map[string]any) Result {
	timeout := DefaultTimeout
	if v, ok := args["timeout_seconds"].(float64); ok && v > 0 {
		timeout = time.Duration(v * float64(time.Second))
	}
	if timeout > MaxTimeout {
		timeout = MaxTimeout
	}
	dir := s.opts.Dir
	if w, _ := args["workdir"].(string); w != "" {
		dir = w
	}
	if dir == "" {
		dir = s.home
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	cmd.Dir = dir
	cmd.Env = s.Env()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: s.cred}
	// Kill the whole process group, not just bash, on the deadline.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	stdout, stderr := &capped{max: MaxOutput}, &capped{max: MaxOutput}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	started := time.Now()
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			code = -1
			stderr.WriteString(err.Error())
		}
	}
	notice := ""
	if errors.Is(ctx.Err(), context.DeadlineExceeded) && time.Since(started) >= timeout {
		notice = fmt.Sprintf("\n(killed after the timeout of %s)", timeout)
	}
	text := fmt.Sprintf("exit=%d%s\nstdout:\n%s\nstderr:\n%s", code, notice, stdout.String(), stderr.String())
	return Result{Text: text, IsError: code != 0}
}

// capped keeps the first max bytes written to it, and counts the rest.
type capped struct {
	max     int
	buf     bytes.Buffer
	dropped int64
}

func (c *capped) Write(p []byte) (int, error) {
	room := c.max - c.buf.Len()
	if room > len(p) {
		room = len(p)
	}
	if room > 0 {
		c.buf.Write(p[:room])
	}
	c.dropped += int64(len(p) - max(room, 0))
	return len(p), nil
}

func (c *capped) WriteString(s string) { c.Write([]byte(s)) }

func (c *capped) String() string {
	if c.dropped == 0 {
		return c.buf.String()
	}
	return fmt.Sprintf("%s\n... (%d more bytes cut; the cap is %d)", c.buf.String(), c.dropped, c.max)
}

// capText cuts s to MaxOutput, saying so.
func capText(s, advice string) string {
	if len(s) <= MaxOutput {
		return s
	}
	return fmt.Sprintf("%s\n... (%d more bytes cut; the cap is %d; %s)", s[:MaxOutput], len(s)-MaxOutput, MaxOutput, advice)
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// path resolves a file argument: relative paths are from the user's
// home.
func (s *Server) path(p string) (string, error) {
	if p == "" {
		return "", errors.New("file_path is required")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(s.home, p)
	}
	return filepath.Clean(p), nil
}

// lines splits content as Python's splitlines does: a trailing newline
// makes no empty last line.
func lines(content string) []string {
	l := strings.Split(content, "\n")
	if len(l) > 0 && l[len(l)-1] == "" && strings.HasSuffix(content, "\n") {
		l = l[:len(l)-1]
	}
	return l
}

func intArg(args map[string]any, k string) (int, bool) {
	v, ok := args[k].(float64)
	return int(v), ok
}

func (s *Server) readFile(_ context.Context, args map[string]any) Result {
	fp, _ := args["file_path"].(string)
	full, err := s.path(fp)
	if err != nil {
		return Result{Text: err.Error(), IsError: true}
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return Result{Text: fmt.Sprintf("Error reading %s: %v", fp, err), IsError: true}
	}
	l := lines(string(data))
	start := 0
	if v, ok := intArg(args, "offset"); ok {
		start = v
	}
	start = min(max(start, 0), len(l))
	end := len(l)
	if v, ok := intArg(args, "limit"); ok {
		end = start + v
	}
	end = min(max(end, start), len(l))
	var b strings.Builder
	for i := start; i < end; i++ {
		if i > start {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%6d\t%s", i+1, l[i])
	}
	return Result{Text: capText(b.String(), "read the rest with offset and limit")}
}

func (s *Server) writeFile(_ context.Context, args map[string]any) Result {
	fp, _ := args["file_path"].(string)
	content, ok := args["content"].(string)
	if !ok {
		return Result{Text: "content is required", IsError: true}
	}
	full, err := s.path(fp)
	if err != nil {
		return Result{Text: err.Error(), IsError: true}
	}
	if err := s.mkdirAll(filepath.Dir(full)); err != nil {
		return Result{Text: fmt.Sprintf("Error creating %s: %v", filepath.Dir(fp), err), IsError: true}
	}
	_, statErr := os.Stat(full)
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return Result{Text: fmt.Sprintf("Error writing %s: %v", fp, err), IsError: true}
	}
	if errors.Is(statErr, fs.ErrNotExist) {
		s.chown(full)
	}
	return Result{Text: "Wrote " + fp}
}

// mkdirAll makes dir and its missing parents, owned by the user.
func (s *Server) mkdirAll(dir string) error {
	var made []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil || d == filepath.Dir(d) {
			break
		}
		made = append(made, d)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, d := range made {
		s.chown(d)
	}
	return nil
}

// chown gives a file the server made to the user commands run as, so
// that the agent's own commands can change it.
func (s *Server) chown(p string) {
	if s.cred != nil {
		os.Lchown(p, int(s.cred.Uid), int(s.cred.Gid))
	}
}

func (s *Server) editFile(_ context.Context, args map[string]any) Result {
	fp, _ := args["file_path"].(string)
	oldStr, hasOld := args["old_string"].(string)
	newStr, hasNew := args["new_string"].(string)
	if !hasOld || !hasNew {
		return Result{Text: "old_string and new_string are required", IsError: true}
	}
	if oldStr == "" {
		return Result{Text: "old_string must not be empty: it matches between every character in the file. " +
			"Use write_file to create or replace a file, or name the exact text this edit replaces.", IsError: true}
	}
	replaceAll, _ := args["replace_all"].(bool)
	full, err := s.path(fp)
	if err != nil {
		return Result{Text: err.Error(), IsError: true}
	}
	st, err := os.Stat(full)
	if err != nil {
		return Result{Text: fmt.Sprintf("Error reading %s: %v", fp, err), IsError: true}
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return Result{Text: fmt.Sprintf("Error reading %s: %v", fp, err), IsError: true}
	}
	content := string(data)
	count := strings.Count(content, oldStr)
	if count == 0 {
		return Result{Text: fmt.Sprintf("String not found in file: %q", oldStr), IsError: true}
	}
	if count > 1 && !replaceAll {
		return Result{Text: fmt.Sprintf("String appears %d times in the file. Use replace_all: true to replace "+
			"every occurrence, or provide more surrounding context to uniquely identify the instance you mean.", count), IsError: true}
	}
	n := 1
	if replaceAll {
		n = -1
	}
	if err := os.WriteFile(full, []byte(strings.Replace(content, oldStr, newStr, n)), st.Mode().Perm()); err != nil {
		return Result{Text: fmt.Sprintf("Error writing %s: %v", fp, err), IsError: true}
	}
	return Result{Text: fmt.Sprintf("Wrote %s (%d replaced)", fp, map[bool]int{true: count, false: 1}[replaceAll])}
}

func (s *Server) listDir(_ context.Context, args map[string]any) Result {
	p, _ := args["path"].(string)
	if p == "" {
		p = "."
	}
	full, err := s.path(p)
	if err != nil {
		return Result{Text: err.Error(), IsError: true}
	}
	recursive, _ := args["recursive"].(bool)
	depth := 1
	if recursive {
		depth = 3
		if v, ok := intArg(args, "depth"); ok && v > 0 {
			depth = v
		}
	}
	var out []string
	more := false
	var walk func(dir, rel string, level int) error
	walk = func(dir, rel string, level int) error {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
		for _, e := range ents {
			if len(out) >= MaxListEntries {
				more = true
				return nil
			}
			name := filepath.Join(rel, e.Name())
			switch {
			case e.IsDir():
				out = append(out, name+"/")
				if level+1 < depth {
					if err := walk(filepath.Join(dir, e.Name()), name, level+1); err != nil {
						out = append(out, fmt.Sprintf("%s/ (unreadable: %v)", name, err))
					}
				}
			case e.Type()&fs.ModeSymlink != 0:
				target, _ := os.Readlink(filepath.Join(dir, e.Name()))
				out = append(out, name+" -> "+target)
			default:
				size := int64(0)
				if info, err := e.Info(); err == nil {
					size = info.Size()
				}
				out = append(out, fmt.Sprintf("%s (%d bytes)", name, size))
			}
		}
		return nil
	}
	if err := walk(full, "", 0); err != nil {
		return Result{Text: fmt.Sprintf("Error listing %s: %v", p, err), IsError: true}
	}
	text := strings.Join(out, "\n")
	if more {
		text += fmt.Sprintf("\n... (more entries; the cap is %d; list a subdirectory)", MaxListEntries)
	}
	return Result{Text: capText(text, "list a subdirectory")}
}

// ServeHTTP serves MCP's JSON-RPC on POST /mcp: initialize, ping,
// tools/list and tools/call. A notification gets 202 and no body.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/mcp" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, Response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &RPCError{Code: -32700, Message: "parse error: " + err.Error()}})
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, s.handle(r.Context(), req))
}

func (s *Server) handle(ctx context.Context, req Request) Response {
	out := Response{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		out.Result = map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "silo-tools", "version": Version},
		}
	case "ping":
		out.Result = map[string]any{}
	case "tools/list":
		var list []map[string]any
		for _, t := range s.Tools() {
			list = append(list, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema})
		}
		out.Result = map[string]any{"tools": list}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			out.Error = &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
			break
		}
		res := s.Call(ctx, p.Name, p.Arguments)
		out.Result = map[string]any{"content": []map[string]any{{"type": "text", "text": res.Text}}, "isError": res.IsError}
	default:
		out.Error = &RPCError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return out
}

// Version is silo-tools' version: the one silod's startup script
// checks "silo-tools --version" against before fetching it again,
// agents.ToolsVersion (a test holds them equal; importing package
// agents would build silod's store into this binary).
var Version = "v1.0.0"

// Request is a JSON-RPC request or notification.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC error.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
