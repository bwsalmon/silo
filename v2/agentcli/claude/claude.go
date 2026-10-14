// Adapted from github.com/bwsalmon/grain pkg/agent/claude/claude.go at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

// Package claude maps a Silofile onto Claude Code's command line
// (cli-mapping.md §2.1, §3).
package claude

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/bwsalmon/silo/v2/agentcli"
	"github.com/bwsalmon/silo/v2/location"
	"github.com/bwsalmon/silo/v2/silofile"
)

func init() { agentcli.Register("claude", Mapping{}) }

// Staged file names.
const (
	MCPConfigFile    = "mcp.json"
	SystemPromptFile = "system-prompt.md"
)

// APIKeyVar is set from agent.auth: {credential: x} (design §8.2).
const APIKeyVar = "ANTHROPIC_API_KEY"

// Rosters is Claude Code's built-in tool roster per reviewed version,
// which remote_permissions subsets are checked against (cli-mapping.md
// §2.1). Claude Code does not print its roster, so each table is an
// assumption until stage 1d checks it against the real binary
// (cli-mapping.md §4, assumption 1).
var Rosters = map[string][]string{
	"2.1.197": {
		"Agent", "AskUserQuestion", "Bash", "BashOutput", "Edit",
		"EnterPlanMode", "ExitPlanMode", "Glob", "Grep", "KillShell",
		"LSP", "MultiEdit", "NotebookEdit", "Read", "Skill",
		"SlashCommand", "Task", "TaskOutput", "TaskStop", "TodoWrite",
		"WebFetch", "WebSearch", "Write",
	},
}

// Roster returns the roster for version and the version it belongs to:
// the version's own table, or else the newest one, which a caller should
// say it used.
func Roster(version string) (names []string, of string) {
	if r, ok := Rosters[version]; ok {
		return r, version
	}
	versions := make([]string, 0, len(Rosters))
	for v := range Rosters {
		versions = append(versions, v)
	}
	sort.Slice(versions, func(i, j int) bool { return versionLess(versions[i], versions[j]) })
	newest := versions[len(versions)-1]
	return Rosters[newest], newest
}

func versionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, _ := strconv.Atoi(as[i])
		y, _ := strconv.Atoi(bs[i])
		if x != y {
			return x < y
		}
	}
	return len(as) < len(bs)
}

// Mapping is Claude Code.
type Mapping struct{}

func (Mapping) VersionArgs() []string { return []string{"--version"} }

// ParseVersion reads "2.1.197 (Claude Code)".
func (Mapping) ParseVersion(out string) (string, error) { return agentcli.SemVer(out) }

// MCPConfig is the --mcp-config file: the gateway and nothing else.
func MCPConfig(url, token string) []byte {
	b, _ := json.MarshalIndent(map[string]any{
		"mcpServers": map[string]any{
			agentcli.ServerName: map[string]any{
				"type":    "http",
				"url":     url,
				"headers": map[string]string{"Authorization": "Bearer " + token},
			},
		},
	}, "", "  ")
	return append(b, '\n')
}

func (Mapping) Build(req agentcli.Request) (agentcli.Plan, error) {
	if err := agentcli.CheckMode(req.Mode); err != nil {
		return agentcli.Plan{}, err
	}
	perm := req.Permissions.Effective()
	var plan agentcli.Plan
	if perm == silofile.PermSubset {
		roster, of := Roster(req.Host.Version)
		if of != req.Host.Version {
			plan.Notes = append(plan.Notes, fmt.Sprintf("claude %s is not in silo's reviewed roster tables; checking remote_permissions against %s's", req.Host.Version, of))
		}
		known := map[string]bool{}
		for _, n := range roster {
			known[n] = true
		}
		for _, t := range req.Permissions.Tools {
			if !known[t] {
				return agentcli.Plan{}, agentcli.Refusef("remote_permissions names %q, which is not a built-in tool of claude %s (its roster: %s)", t, of, strings.Join(roster, ", "))
			}
		}
	}

	plan.Files = []location.StagedFile{{Name: MCPConfigFile, Data: MCPConfig(req.GatewayURL, req.GatewayToken)}}
	if req.SystemPrompt != "" {
		plan.Files = append(plan.Files, location.StagedFile{Name: SystemPromptFile, Data: []byte(req.SystemPrompt)})
	}

	env := map[string]string{
		"MCP_TOOL_TIMEOUT": strconv.FormatInt(agentcli.ToolTimeout.Milliseconds(), 10),
	}
	if perm != silofile.PermAll {
		env["CLAUDE_CODE_DISABLE_AUTO_MEMORY"] = "1"
	}
	for k, v := range req.Env {
		env[k] = v
	}

	plan.Command = func(dir string) location.Command {
		argv := []string{req.Host.Binary}
		if req.Mode == agentcli.Headless {
			argv = append(argv, "-p", "--output-format", "stream-json", "--verbose", "--input-format", "stream-json")
		}
		if req.Agent.Model != "" {
			argv = append(argv, "--model", req.Agent.Model)
		}
		allowed := []string{"mcp__" + agentcli.ServerName}
		argv = append(argv, "--mcp-config", filepath.Join(dir, MCPConfigFile), "--strict-mcp-config")
		switch perm {
		case silofile.PermNone:
			argv = append(argv, "--tools", "")
		case silofile.PermSubset:
			argv = append(argv, "--tools", strings.Join(req.Permissions.Tools, ","))
		}
		switch {
		case perm == silofile.PermNone:
		case req.Mode == agentcli.Interactive:
			argv = append(argv, "--permission-mode", "default")
		case perm == silofile.PermAll:
			argv = append(argv, "--dangerously-skip-permissions")
		default:
			argv = append(argv, "--permission-mode", "dontAsk")
			allowed = append(allowed, req.Permissions.Tools...)
		}
		argv = append(argv, "--allowedTools", strings.Join(allowed, ","), "--disable-slash-commands")
		if perm != silofile.PermAll {
			argv = append(argv, "--setting-sources", "user")
		}
		if req.SystemPrompt != "" {
			argv = append(argv, "--append-system-prompt-file", filepath.Join(dir, SystemPromptFile))
		}
		cmd := location.Command{Argv: argv, Env: agentcli.MergeEnv(req.Host.Env, env), TTY: req.Mode == agentcli.Interactive}
		switch {
		case req.Mode == agentcli.Headless:
			// The prompt is one stream-json user message on stdin, never
			// argv, where ps would show it.
			cmd.Stdin = UserMessage(req.Prompt)
		case req.Prompt != "":
			argv = append(argv, "--", req.Prompt)
			cmd.Argv = argv
		}
		return cmd
	}
	return plan, nil
}

// UserMessage is one stream-json input line carrying a user message.
func UserMessage(text string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": []map[string]string{{"type": "text", "text": text}}},
	})
	return append(b, '\n')
}
