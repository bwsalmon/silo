package adapter

import (
	"encoding/json"
)

// Codex drives the OpenAI Codex CLI (`codex`).
//
//   - MCP: -c mcp_servers.silo.command=... and .args=... overrides, so the
//     person's ~/.codex (and its login) is used unchanged.
//   - Prompt: -c developer_instructions=..., added to Codex's own.
//   - Tools: Codex's shell cannot be removed in every version. Silo asks
//     for features.shell_tool=false and turns off web search, and also
//     runs Codex with --sandbox read-only and --ask-for-approval untrusted,
//     so that where a version still offers its shell, the shell cannot
//     write and Codex asks the person before running anything.
//   - list_changed: not refreshed; the router adds silo.list_tools and
//     silo.call.
type Codex struct{}

func (Codex) Name() string      { return "codex" }
func (Codex) ListChanged() bool { return false }

func (Codex) Prepare(s Spec) (*Launch, error) {
	args := []string{
		"-c", "mcp_servers." + ServerName + ".command=" + tomlString(s.MCPCommand),
		"-c", "mcp_servers." + ServerName + ".args=" + tomlStrings(s.MCPArgs),
		"-c", "developer_instructions=" + tomlString(s.SystemPrompt),
		"-c", "features.shell_tool=false",
		"-c", "tools.web_search=false",
		"--sandbox", "read-only",
		"--ask-for-approval", "untrusted",
		"--cd", s.Checkout,
	}
	return &Launch{Path: s.Binary, Args: append(args, s.Args...), Env: extraEnv(s.Env), Dir: s.Checkout}, nil
}

// tomlString quotes s as a TOML basic string; JSON's string escapes are
// a subset of TOML's.
func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func tomlStrings(ss []string) string {
	if ss == nil {
		ss = []string{}
	}
	b, _ := json.Marshal(ss)
	return string(b)
}
