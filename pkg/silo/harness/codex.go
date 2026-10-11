package harness

import (
	"encoding/json"
	"strconv"
)

// Codex is the codex adapter: OpenAI's Codex CLI, codex exec, with
// --json output.
//
// Its built-in tools are off by configuration: no shell tool, no
// apply_patch, no web search and no image viewer, and the read-only
// sandbox with approvals never, so that if a shell is offered anyway
// it can change nothing and asks nobody. Its one MCP server is the
// session's, over HTTP to the forwarder (rmcp's streamable HTTP
// client), and its model provider is the model proxy on the session
// socket. Its session is a rollout file under $CODEX_HOME/sessions,
// named by the thread id it reports first, and codex exec resume
// continues it.
type Codex struct{}

func (Codex) Name() string              { return "codex" }
func (Codex) CredentialKinds() []string { return []string{KindAPIKey, KindSubscription} }
func (Codex) StatePaths() []string      { return []string{".codex/sessions"} }
func (Codex) NewSessionID() string      { return "" }
func (Codex) Parser() Parser            { return &codexParser{} }

// codexConfig are the -c overrides every turn runs with.
var codexConfig = []string{
	`mcp_servers.` + mcpServer + `.url=` + strconv.Quote(Endpoint+"/mcp"),
	`experimental_use_rmcp_client=true`,
	`model_provider="silo"`,
	`model_providers.silo={name="silo",base_url=` + strconv.Quote(modelBase("openai")+"/v1") + `,env_key="OPENAI_API_KEY",wire_api="responses"}`,
	`approval_policy="never"`,
	`sandbox_mode="read-only"`,
	`features.shell_tool=false`,
	`include_apply_patch_tool=false`,
	`tools.web_search=false`,
	`tools.view_image=false`,
	`shell_environment_policy.inherit="none"`,
	`check_for_update_on_startup=false`,
	`analytics.enabled=false`,
}

func (Codex) Command(l Launch) Command {
	argv := []string{"codex", "exec", "--json", "--skip-git-repo-check",
		"--cd", WorkDir, "--model", l.Model, "--sandbox", "read-only"}
	for _, c := range codexConfig {
		argv = append(argv, "-c", c)
	}
	if l.Resume {
		argv = append(argv, "resume", l.SessionID)
	}
	argv = append(argv, "--", l.Prompt)
	return Command{Argv: argv, Env: []string{
		"HOME=" + Home,
		"CODEX_HOME=" + Home + "/.codex",
		"OPENAI_API_KEY=" + ProxyKey,
		"NO_COLOR=1",
	}}
}

type codexParser struct{}

type codexLine struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"`
	Error    *struct {
		Message string `json:"message"`
	} `json:"error"`
	Usage map[string]any `json:"usage"`
	Item  *struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Text    string `json:"text"`
		Server  string `json:"server"`
		Tool    string `json:"tool"`
		Command string `json:"command"`
		Message string `json:"message"`
	} `json:"item"`
}

func (p *codexParser) Line(line []byte) []Event {
	var l codexLine
	if json.Unmarshal(line, &l) != nil {
		return nil
	}
	switch l.Type {
	case "thread.started":
		return []Event{{Kind: EvSession, SessionID: l.ThreadID}}
	case "turn.completed":
		var out []Event
		if l.Usage != nil {
			out = append(out, Event{Kind: EvUsage, Usage: map[string]any{"tokens": l.Usage, "source": "harness"}})
		}
		return append(out, Event{Kind: EvTurnEnd})
	case "turn.failed":
		msg := "codex: the turn failed"
		if l.Error != nil && l.Error.Message != "" {
			msg = l.Error.Message
		}
		return []Event{{Kind: EvError, Text: msg}, {Kind: EvTurnEnd}}
	case "error":
		return []Event{{Kind: EvError, Text: l.Message}}
	case "item.started", "item.completed":
		it := l.Item
		if it == nil {
			return nil
		}
		switch it.Type {
		case "agent_message":
			if l.Type == "item.completed" && it.Text != "" {
				return []Event{{Kind: EvReply, Text: it.Text}}
			}
		case "mcp_tool_call":
			if l.Type == "item.started" {
				kind := EvToolCall
				if it.Server != mcpServer {
					kind = EvLocalTool
				}
				return []Event{{Kind: kind, Tool: it.Server + "." + it.Tool, ToolID: it.ID}}
			}
		case "command_execution", "file_change", "web_search":
			if l.Type == "item.started" {
				tool := it.Type
				if it.Command != "" {
					tool += ": " + it.Command
				}
				return []Event{{Kind: EvLocalTool, Tool: tool, ToolID: it.ID}}
			}
		case "error":
			if l.Type == "item.completed" {
				return []Event{{Kind: EvError, Text: it.Message}}
			}
		}
	}
	return nil
}
