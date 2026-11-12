// Package adapter turns a session into the command line of the agent CLI
// silo drives. Each CLI takes its MCP servers, its system prompt and its
// tool restrictions differently; an adapter knows one CLI's way.
//
// Every adapter hands the CLI exactly one MCP server, "silo" (silo's
// router, reached through `silo mcp-bridge`), and turns off the CLI's own
// shell, file-editing and file-reading tools as far as that CLI allows.
// docs/config.md lists what each adapter passes and its limits.
package adapter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// ServerName is the name the CLI knows silo's router by.
const ServerName = "silo"

// Spec is what an adapter needs to know about the session.
type Spec struct {
	// Binary, Args and Env come from silo's config: the CLI's executable,
	// extra arguments appended after the adapter's, and extra environment.
	Binary string
	Args   []string
	Env    map[string]string
	// Checkout is the context repository's working tree: the CLI's
	// working directory.
	Checkout string
	// SessionDir is where the adapter may write config files.
	SessionDir string
	// MCPCommand and MCPArgs start the stdio MCP server the CLI talks to.
	MCPCommand string
	MCPArgs    []string
	// SystemPrompt is the composed system prompt: silo's preamble, the
	// repository's .silo/prompt.md and its context.
	SystemPrompt string
}

// Launch is the command to run.
type Launch struct {
	Path string
	Args []string
	// Env is added to silo's environment.
	Env []string
	Dir string
}

// Adapter prepares one CLI.
type Adapter interface {
	Name() string
	// ListChanged reports whether the CLI refreshes its tools on
	// notifications/tools/list_changed.
	ListChanged() bool
	Prepare(spec Spec) (*Launch, error)
}

// Get returns the adapter for a CLI name.
func Get(name string) (Adapter, error) {
	switch name {
	case "claude":
		return Claude{}, nil
	case "codex":
		return Codex{}, nil
	case "gemini":
		return Gemini{}, nil
	}
	return nil, fmt.Errorf("no adapter for CLI %q", name)
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func extraEnv(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var env []string
	for _, k := range keys {
		env = append(env, k+"="+m[k])
	}
	return env
}
