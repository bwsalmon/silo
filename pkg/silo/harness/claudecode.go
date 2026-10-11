package harness

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
)

// ClaudeCode is the claude-code adapter: Claude Code's CLI, claude, in
// print mode with stream-json output.
//
// Its built-in tools are off with --tools "" (no Bash, Edit, Read,
// WebFetch, ...), and its only MCP server is the session's, by
// --mcp-config with --strict-mcp-config, so that no settings file can
// add another. --allowedTools mcp__silo lets those tools run without a
// permission prompt nobody would answer. Its session is a file under
// $HOME/.claude/projects, named by the --session-id silod gives it, and
// --resume continues it.
type ClaudeCode struct{}

func (ClaudeCode) Name() string              { return "claude-code" }
func (ClaudeCode) CredentialKinds() []string { return []string{KindAPIKey, KindSubscription} }
func (ClaudeCode) StatePaths() []string      { return []string{".claude/projects"} }
func (ClaudeCode) NewSessionID() string      { return newUUID() }
func (ClaudeCode) Parser() Parser            { return &claudeParser{} }

func (ClaudeCode) Command(l Launch) Command {
	argv := []string{"claude", "--print",
		"--output-format", "stream-json", "--verbose",
		"--model", l.Model,
		"--tools", "",
		"--mcp-config", claudeMCPConfig, "--strict-mcp-config",
		"--allowedTools", "mcp__" + mcpServer,
	}
	if l.Resume {
		argv = append(argv, "--resume", l.SessionID)
	} else {
		argv = append(argv, "--session-id", l.SessionID)
	}
	// The prompt after --, so that no option takes it for a value.
	argv = append(argv, "--", l.Prompt)
	return Command{Argv: argv, Env: []string{
		"HOME=" + Home,
		"ANTHROPIC_BASE_URL=" + modelBase("anthropic"),
		"ANTHROPIC_API_KEY=" + ProxyKey,
		"DISABLE_AUTOUPDATER=1",
		"DISABLE_TELEMETRY=1",
		"DISABLE_ERROR_REPORTING=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	}}
}

var claudeMCPConfig = func() string {
	b, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
		mcpServer: map[string]any{"type": "http", "url": Endpoint + "/mcp"},
	}})
	return string(b)
}()

type claudeParser struct{}

type claudeLine struct {
	Type      string   `json:"type"`
	Subtype   string   `json:"subtype"`
	SessionID string   `json:"session_id"`
	Tools     []string `json:"tools"`
	IsError   bool     `json:"is_error"`
	Result    string   `json:"result"`
	Message   struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`
	Usage        map[string]any `json:"usage"`
	TotalCostUSD *float64       `json:"total_cost_usd"`
	NumTurns     int            `json:"num_turns"`
	Errors       []string       `json:"errors"`
}

func (p *claudeParser) Line(line []byte) []Event {
	var l claudeLine
	if json.Unmarshal(line, &l) != nil {
		return nil
	}
	switch l.Type {
	case "system":
		if l.Subtype == "init" {
			return []Event{{Kind: EvSession, SessionID: l.SessionID, Tools: l.Tools}}
		}
	case "assistant":
		var out []Event
		for _, c := range l.Message.Content {
			switch c.Type {
			case "text":
				if strings.TrimSpace(c.Text) != "" {
					out = append(out, Event{Kind: EvReply, Text: c.Text})
				}
			case "tool_use":
				kind := EvToolCall
				if !isSiloTool(c.Name) {
					kind = EvLocalTool
				}
				out = append(out, Event{Kind: kind, Tool: c.Name, ToolID: c.ID})
			}
		}
		return out
	case "result":
		var out []Event
		if l.Usage != nil {
			u := map[string]any{"tokens": l.Usage, "source": "harness"}
			if l.TotalCostUSD != nil {
				u["harness_cost_usd"] = *l.TotalCostUSD
			}
			out = append(out, Event{Kind: EvUsage, Usage: u})
		}
		if l.IsError || strings.HasPrefix(l.Subtype, "error") {
			msg := l.Result
			if len(l.Errors) > 0 {
				msg = strings.Join(l.Errors, "; ")
			}
			if msg == "" {
				msg = "claude: the turn ended in " + l.Subtype
			}
			out = append(out, Event{Kind: EvError, Text: msg})
		}
		return append(out, Event{Kind: EvTurnEnd, SessionID: l.SessionID})
	}
	return nil
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
