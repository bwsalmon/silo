package adapter

import (
	"os"
	"path/filepath"
)

// GeminiExcluded are Gemini CLI's built-in tools a session turns off.
var GeminiExcluded = []string{
	"run_shell_command",
	"write_file", "replace",
	"read_file", "read_many_files", "list_directory", "glob", "search_file_content",
	"web_fetch", "google_web_search", "save_memory",
}

// Gemini drives Gemini CLI (`gemini`).
//
//   - MCP: a settings file named by GEMINI_CLI_SYSTEM_SETTINGS_PATH (the
//     highest-precedence settings layer, so nothing has to be written
//     into the checkout), declaring only the silo server as trusted (silo
//     does its own approvals), plus --allowed-mcp-server-names silo.
//   - Prompt: GEMINI_SYSTEM_MD names a file holding the composed prompt.
//     It replaces Gemini CLI's own system prompt.
//   - Tools: tools.exclude in that settings file lists GeminiExcluded.
//   - list_changed: treated as not refreshed; the router adds
//     silo.list_tools and silo.call.
type Gemini struct{}

func (Gemini) Name() string      { return "gemini" }
func (Gemini) ListChanged() bool { return false }

func (Gemini) Prepare(s Spec) (*Launch, error) {
	dir := filepath.Join(s.SessionDir, "gemini")
	settings := filepath.Join(dir, "settings.json")
	err := writeJSON(settings, map[string]any{
		"mcpServers": map[string]any{
			ServerName: map[string]any{"command": s.MCPCommand, "args": s.MCPArgs, "trust": true},
		},
		"mcp":   map[string]any{"allowed": []string{ServerName}},
		"tools": map[string]any{"exclude": GeminiExcluded},
	})
	if err != nil {
		return nil, err
	}
	system := filepath.Join(dir, "system.md")
	if err := os.WriteFile(system, []byte(s.SystemPrompt), 0o600); err != nil {
		return nil, err
	}
	env := append([]string{
		"GEMINI_CLI_SYSTEM_SETTINGS_PATH=" + settings,
		"GEMINI_SYSTEM_MD=" + system,
	}, extraEnv(s.Env)...)
	args := []string{"--allowed-mcp-server-names", ServerName}
	return &Launch{Path: s.Binary, Args: append(args, s.Args...), Env: env, Dir: s.Checkout}, nil
}
