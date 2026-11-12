package adapter

import (
	"path/filepath"
	"strings"
)

// ClaudeDisallowed are Claude Code's built-in tools a session turns off:
// the shell, every file write, and file reads and web access too, so the
// agent works only through silo's tools.
var ClaudeDisallowed = []string{
	"Bash", "BashOutput", "KillShell",
	"Edit", "MultiEdit", "Write", "NotebookEdit",
	"Read", "Glob", "Grep", "LS",
	"WebFetch", "WebSearch", "Task",
}

// Claude drives Claude Code (`claude`).
//
//   - MCP: a JSON file passed with --mcp-config, and --strict-mcp-config
//     so no other configured server is loaded.
//   - Prompt: --append-system-prompt, keeping Claude Code's own.
//   - Tools: --disallowedTools ClaudeDisallowed; --allowedTools
//     mcp__silo so Claude Code does not ask again for what silo already
//     gates; --setting-sources user so the context repository's own
//     .claude/settings*.json (permissions, hooks) is not applied.
//   - list_changed: refreshed by Claude Code.
type Claude struct{}

func (Claude) Name() string      { return "claude" }
func (Claude) ListChanged() bool { return true }

func (Claude) Prepare(s Spec) (*Launch, error) {
	cfgPath := filepath.Join(s.SessionDir, "claude-mcp.json")
	err := writeJSON(cfgPath, map[string]any{
		"mcpServers": map[string]any{
			ServerName: map[string]any{"type": "stdio", "command": s.MCPCommand, "args": s.MCPArgs},
		},
	})
	if err != nil {
		return nil, err
	}
	args := []string{
		"--mcp-config", cfgPath,
		"--strict-mcp-config",
		"--append-system-prompt", s.SystemPrompt,
		"--disallowedTools", strings.Join(ClaudeDisallowed, ","),
		"--allowedTools", "mcp__" + ServerName,
		"--setting-sources", "user",
	}
	return &Launch{Path: s.Binary, Args: append(args, s.Args...), Env: extraEnv(s.Env), Dir: s.Checkout}, nil
}
