// Package builtin holds silo's built-in tool sets: repo-read, repo-write,
// scratch (one-off tools) and silo (refresh, and the fallback tools for
// CLIs that ignore tools/list_changed).
package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bwsalmon/silo/internal/mcp"
)

type toolFunc func(ctx context.Context, args json.RawMessage) *mcp.ToolResult

// set is a tool set implemented in-process.
type set struct {
	tools []mcp.Tool
	funcs map[string]toolFunc
}

func (s *set) add(name, desc string, schema json.RawMessage, f toolFunc) {
	if s.funcs == nil {
		s.funcs = map[string]toolFunc{}
	}
	s.tools = append(s.tools, mcp.Tool{Name: name, Description: desc, InputSchema: schema})
	s.funcs[name] = f
}

func (s *set) Tools(context.Context) ([]mcp.Tool, error) {
	out := append([]mcp.Tool(nil), s.tools...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *set) Call(ctx context.Context, tool string, args json.RawMessage) (*mcp.ToolResult, error) {
	f := s.funcs[tool]
	if f == nil {
		return nil, fmt.Errorf("unknown tool %q", tool)
	}
	return f(ctx, args), nil
}

func (s *set) Close() error { return nil }

// decode unmarshals tool arguments; an empty argument list is {}.
func decode(args json.RawMessage, v any) error {
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("bad arguments: %v", err)
	}
	return nil
}

func str(desc string) map[string]any  { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any  { return map[string]any{"type": "integer", "description": desc} }
func flag(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }

// resolvePath turns an agent-supplied path into an absolute path inside
// root, refusing anything that escapes it (by .. or a symlink) or reaches
// into .git.
func resolvePath(root, rel string) (string, error) {
	if rel == "" {
		rel = "."
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("path %q must be relative to the repository root", rel)
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q leaves the repository", rel)
	}
	for _, seg := range strings.Split(clean, string(filepath.Separator)) {
		// Case-insensitively: on macOS's and Windows' default file
		// systems .GIT is the same directory as .git.
		if strings.EqualFold(seg, ".git") {
			return "", fmt.Errorf("path %q is inside .git", rel)
		}
	}
	abs := filepath.Join(root, clean)
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	// Check the deepest existing ancestor's real location.
	probe := abs
	for {
		real, err := filepath.EvalSymlinks(probe)
		if err == nil {
			if real != realRoot && !strings.HasPrefix(real, realRoot+string(filepath.Separator)) {
				return "", fmt.Errorf("path %q leaves the repository through a symlink", rel)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	return abs, nil
}

// checkRef refuses a git revision that would be read as an option.
func checkRef(ref string) error {
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("revision %q may not start with '-'", ref)
	}
	return nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("\n... [output clipped: %d more bytes]", len(s)-n)
}
