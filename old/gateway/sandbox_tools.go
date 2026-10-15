// Adapted from github.com/bwsalmon/grain pkg/mcp/sandbox_tools.go and pkg/mcp/run_command_result.go at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

package gateway

import (
	"context"
	"fmt"
	"io/fs"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/old/mcp"
	"github.com/bwsalmon/silo/old/sandbox"
)

// Sandbox tool limits.
const (
	// DefaultExecTimeout is sandbox_exec's timeout when the call gives
	// none.
	DefaultExecTimeout = 5 * time.Minute
	// MaxExecTimeout caps sandbox_exec's timeout argument. The Silofile
	// has no field to raise it yet.
	MaxExecTimeout = 30 * time.Minute
	// DefaultReadLimit and MaxReadLimit bound sandbox_read_file.
	DefaultReadLimit = 256 << 10
	MaxReadLimit     = 4 << 20
	// fileOpTimeout bounds the read, write and list tools.
	fileOpTimeout = 2 * time.Minute
)

// SandboxTools returns sandbox_exec, sandbox_read_file,
// sandbox_write_file and sandbox_list_dir over the named sandboxes
// (design §4.1), or nothing when there are none. Each tool's sandbox
// argument is an enum of the names; a call naming another is refused.
func SandboxTools(sandboxes map[string]sandbox.Sandbox) []Tool {
	if len(sandboxes) == 0 {
		return nil
	}
	names := make([]string, 0, len(sandboxes))
	for n := range sandboxes {
		names = append(names, n)
	}
	sort.Strings(names)
	st := &sandboxTools{sandboxes: sandboxes, names: names}
	return []Tool{st.execTool(), st.readTool(), st.writeTool(), st.listTool()}
}

type sandboxTools struct {
	sandboxes map[string]sandbox.Sandbox
	names     []string
}

func (st *sandboxTools) schema(required []string, props map[string]any) map[string]any {
	all := map[string]any{
		"sandbox": map[string]any{
			"type":        "string",
			"enum":        st.names,
			"description": "The sandbox to act in.",
		},
	}
	for k, v := range props {
		all[k] = v
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           all,
		"required":             append([]string{"sandbox"}, required...),
	}
}

// args is a call's arguments; its readers record the first wrong type
// as a refusal in err.
type args struct {
	m   map[string]any
	err error
}

// parse checks a call's arguments against the tool's schema, refusing
// unknown and missing ones, and resolves its sandbox.
func (st *sandboxTools) parse(m map[string]any, schema map[string]any) (*args, sandbox.Sandbox, error) {
	props := schema["properties"].(map[string]any)
	for k := range m {
		if _, ok := props[k]; !ok {
			return nil, nil, Refusef("unknown argument %q", k)
		}
	}
	for _, k := range schema["required"].([]string) {
		if _, ok := m[k]; !ok {
			return nil, nil, Refusef("argument %q is required", k)
		}
	}
	a := &args{m: m}
	name := a.str("sandbox")
	if a.err != nil {
		return nil, nil, a.err
	}
	sb, ok := st.sandboxes[name]
	if !ok {
		return nil, nil, Refusef("no sandbox is named %q: this session declares %s", name, strings.Join(st.names, ", "))
	}
	return a, sb, nil
}

func (a *args) str(k string) string {
	v, ok := a.m[k]
	if !ok || a.err != nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		a.err = Refusef("argument %q must be a string", k)
	}
	return s
}

func (a *args) num(k string) (float64, bool) {
	v, ok := a.m[k]
	if !ok || a.err != nil {
		return 0, false
	}
	f, ok := v.(float64)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		a.err = Refusef("argument %q must be a number", k)
		return 0, false
	}
	return f, true
}

func (a *args) strMap(k string) map[string]string {
	v, ok := a.m[k]
	if !ok || a.err != nil {
		return nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		a.err = Refusef("argument %q must be an object of strings", k)
		return nil
	}
	out := make(map[string]string, len(obj))
	for name, val := range obj {
		s, ok := val.(string)
		if !ok {
			a.err = Refusef("argument %q: %q must be a string", k, name)
			return nil
		}
		out[name] = s
	}
	return out
}

func (st *sandboxTools) execTool() Tool {
	schema := st.schema([]string{"command"}, map[string]any{
		"command": map[string]any{"type": "string", "description": "Run by /bin/sh -c inside the sandbox."},
		"cwd":     map[string]any{"type": "string", "description": "Working directory; default the sandbox's workdir."},
		"env":     map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "Variables added to the sandbox's environment for this command."},
		"timeout": map[string]any{"type": "number", "description": fmt.Sprintf("Seconds; default %d, at most %d.", int(DefaultExecTimeout.Seconds()), int(MaxExecTimeout.Seconds()))},
		"stdin":   map[string]any{"type": "string", "description": "Given to the command on standard input."},
	})
	return Tool{
		Name:        "sandbox_exec",
		Description: "Run a shell command in a sandbox. Answers with the exit code, stdout and stderr; long output keeps its beginning and end.",
		InputSchema: schema,
		// The handler bounds itself by the call's timeout argument; this
		// is the backstop above the largest it can ask for.
		Timeout: MaxExecTimeout + time.Minute,
		Handler: func(ctx context.Context, m map[string]any) (mcp.Result, error) {
			a, sb, err := st.parse(m, schema)
			if err != nil {
				return mcp.Result{}, err
			}
			req := sandbox.ExecRequest{
				Argv: []string{"/bin/sh", "-c", a.str("command")},
				Cwd:  a.str("cwd"),
				Env:  a.strMap("env"),
			}
			if _, ok := m["stdin"]; ok {
				req.Stdin = []byte(a.str("stdin"))
			}
			timeout := DefaultExecTimeout
			if secs, ok := a.num("timeout"); ok {
				timeout = time.Duration(secs * float64(time.Second))
				timeout = min(max(timeout, time.Second), MaxExecTimeout)
			}
			if a.err != nil {
				return mcp.Result{}, a.err
			}
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			res, err := sb.Exec(ctx, req)
			text := execText(res)
			if err != nil {
				if ctx.Err() == context.DeadlineExceeded {
					return mcp.Result{Text: text}, fmt.Errorf("the command timed out after %s and was killed: %w", timeout, context.DeadlineExceeded)
				}
				return mcp.Result{Text: text}, err
			}
			return mcp.Result{Text: text, IsError: res.ExitCode != 0}, nil
		},
	}
}

// execText is sandbox_exec's answer: grain's run_command shape.
func execText(res sandbox.ExecResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "exit=%d\n", res.ExitCode)
	b.WriteString("stdout:\n")
	b.Write(res.Stdout)
	if len(res.Stdout) > 0 && res.Stdout[len(res.Stdout)-1] != '\n' {
		b.WriteByte('\n')
	}
	b.WriteString("stderr:\n")
	b.Write(res.Stderr)
	return b.String()
}

func (st *sandboxTools) readTool() Tool {
	schema := st.schema([]string{"path"}, map[string]any{
		"path":  map[string]any{"type": "string", "description": "Absolute, or relative to the sandbox's workdir."},
		"limit": map[string]any{"type": "integer", "description": fmt.Sprintf("Bytes to read; default %d, at most %d.", DefaultReadLimit, MaxReadLimit)},
	})
	return Tool{
		Name:        "sandbox_read_file",
		Description: "Read a file in a sandbox.",
		InputSchema: schema,
		Timeout:     fileOpTimeout,
		Handler: func(ctx context.Context, m map[string]any) (mcp.Result, error) {
			a, sb, err := st.parse(m, schema)
			if err != nil {
				return mcp.Result{}, err
			}
			path := a.str("path")
			limit := int64(DefaultReadLimit)
			if n, ok := a.num("limit"); ok {
				limit = min(max(int64(n), 1), MaxReadLimit)
			}
			if a.err != nil {
				return mcp.Result{}, a.err
			}
			data, err := sb.ReadFile(ctx, path, limit+1)
			if err != nil {
				return mcp.Result{}, err
			}
			if int64(len(data)) > limit {
				return mcp.Result{Text: string(data[:limit]) +
					fmt.Sprintf("\n... [%s is longer than %d bytes: the rest was not read; give a larger limit]", path, limit)}, nil
			}
			return mcp.Result{Text: string(data)}, nil
		},
	}
}

func (st *sandboxTools) writeTool() Tool {
	schema := st.schema([]string{"path", "content"}, map[string]any{
		"path":    map[string]any{"type": "string", "description": "Absolute, or relative to the sandbox's workdir. Parent directories are created."},
		"content": map[string]any{"type": "string"},
		"mode":    map[string]any{"type": "string", "description": "Octal permissions; default 0644."},
	})
	return Tool{
		Name:        "sandbox_write_file",
		Description: "Write a file in a sandbox, replacing it if it exists.",
		InputSchema: schema,
		Timeout:     fileOpTimeout,
		Handler: func(ctx context.Context, m map[string]any) (mcp.Result, error) {
			a, sb, err := st.parse(m, schema)
			if err != nil {
				return mcp.Result{}, err
			}
			path, content := a.str("path"), a.str("content")
			mode := fs.FileMode(0o644)
			if s := a.str("mode"); s != "" {
				n, err := strconv.ParseUint(s, 8, 32)
				if err != nil || n > 0o7777 {
					return mcp.Result{}, Refusef("mode %q is not octal permissions", s)
				}
				mode = fs.FileMode(n)
			}
			if a.err != nil {
				return mcp.Result{}, a.err
			}
			if err := sb.WriteFile(ctx, path, []byte(content), mode); err != nil {
				return mcp.Result{}, err
			}
			return mcp.Result{Text: fmt.Sprintf("wrote %d bytes to %s", len(content), path)}, nil
		},
	}
}

// listScript lists a directory with ls, which every image with a shell
// has.
const listScript = `exec ls -la -- "$1"`

func (st *sandboxTools) listTool() Tool {
	schema := st.schema(nil, map[string]any{
		"path": map[string]any{"type": "string", "description": "Default the sandbox's workdir."},
	})
	return Tool{
		Name:        "sandbox_list_dir",
		Description: "List a directory in a sandbox (ls -la).",
		InputSchema: schema,
		Timeout:     fileOpTimeout,
		Handler: func(ctx context.Context, m map[string]any) (mcp.Result, error) {
			a, sb, err := st.parse(m, schema)
			if err != nil {
				return mcp.Result{}, err
			}
			path := a.str("path")
			if a.err != nil {
				return mcp.Result{}, a.err
			}
			if path == "" {
				path = "."
			}
			res, err := sb.Exec(ctx, sandbox.ExecRequest{Argv: []string{"/bin/sh", "-c", listScript, "silo", path}})
			if err != nil {
				return mcp.Result{}, err
			}
			if res.ExitCode != 0 {
				return mcp.Result{Text: strings.TrimSpace(string(res.Stderr)), IsError: true}, nil
			}
			return mcp.Result{Text: string(res.Stdout)}, nil
		},
	}
}
