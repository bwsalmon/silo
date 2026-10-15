// Adapted from github.com/bwsalmon/grain pkg/agent/claude/transcript.go, live_transcript.go, usagelimit.go and capacity.go at c950ca0858bc4b381e5ce7e2bd716997f63bd24d, Apache License 2.0.

package claude

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/bwsalmon/silo/v2/agentcli"
	"github.com/bwsalmon/silo/v2/events"
)

// NewParser reads --output-format stream-json.
func (Mapping) NewParser(req agentcli.Request) agentcli.Parser { return NewParser() }

// NewParser returns a stream-json parser; agent.custom's
// events: claude-stream-json uses it too.
func NewParser() agentcli.Parser { return &parser{} }

// rawEvent is one stream-json line, narrowed to what silo reads.
type rawEvent struct {
	Type      string      `json:"type"`
	Subtype   string      `json:"subtype"`
	IsError   bool        `json:"is_error"`
	Message   *rawMessage `json:"message"`
	Result    string      `json:"result"`
	SessionID string      `json:"session_id"`
	Model     string      `json:"model"`
	Error     *rawError   `json:"error"`
}

type rawMessage struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
	Usage   *rawUsage       `json:"usage"`
}

type rawUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

type rawError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type rawBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`
	Input     map[string]any  `json:"input"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// maxTurnsSubtype is the result subtype claude reports when its
// --max-turns budget ran out.
const maxTurnsSubtype = "error_max_turns"

// capacityMarkers are claude's own words for a provider with no
// capacity.
var capacityMarkers = []string{"overloaded_error", "api error: 529"}

type parser struct {
	session  string
	model    string
	finished bool
	turns    int
	isError  bool
	subtype  string
	result   string
	final    string
	// errorText is the last error event's message, for a run that died
	// before its result.
	errorText string
	// Claude writes one assistant event per content block, each
	// repeating its message's usage; pending is the current message's,
	// flushed as one usage event when the next message starts.
	pendingID    string
	pendingUsage *events.Usage
}

func (p *parser) Line(line []byte) []events.Event {
	var ev rawEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil
	}
	var out []events.Event
	if ev.SessionID != "" && ev.SessionID != p.session {
		p.session = ev.SessionID
		out = append(out, events.Event{Type: events.TypeResumable, Resumable: &events.Resumable{CLI: "claude", CLISession: ev.SessionID}})
	}
	switch ev.Type {
	case "system":
		if ev.Subtype == "init" && ev.Model != "" {
			p.model = ev.Model
		}
	case "assistant", "user":
		if ev.Message == nil {
			break
		}
		if ev.Type == "assistant" {
			if ev.Message.ID != p.pendingID {
				out = append(out, p.flush()...)
				p.pendingID = ev.Message.ID
			}
			if u := ev.Message.Usage; u != nil {
				model := ev.Message.Model
				if model == "" {
					model = p.model
				}
				p.pendingUsage = &events.Usage{
					Source: events.SourceAgent, Model: model,
					InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
					CacheReadTokens: u.CacheReadInputTokens, CacheWriteTokens: u.CacheCreationInputTokens,
				}
			}
		} else {
			out = append(out, p.flush()...)
		}
		for _, b := range parseBlocks(ev.Message.Content) {
			if it := item(b, ev.Type); it != nil {
				out = append(out, events.Event{Type: events.TypeTranscript, Transcript: it})
			}
		}
	case "error":
		if ev.Error != nil {
			p.errorText = strings.TrimSpace(ev.Error.Type + ": " + ev.Error.Message)
			out = append(out, events.Event{Type: events.TypeTranscript, Transcript: &events.Item{Kind: events.ItemError, Text: p.errorText}})
		}
	case "result":
		out = append(out, p.flush()...)
		// The last result is what the run ended as.
		p.turns++
		p.finished, p.isError, p.subtype, p.result = true, ev.IsError, ev.Subtype, ev.Result
		if !ev.IsError {
			p.final = ev.Result
		}
	}
	return out
}

func (p *parser) flush() []events.Event {
	u := p.pendingUsage
	p.pendingUsage, p.pendingID = nil, ""
	if u == nil {
		return nil
	}
	return []events.Event{{Type: events.TypeUsage, Usage: u}}
}

func item(b rawBlock, role string) *events.Item {
	switch b.Type {
	case "thinking":
		if b.Thinking != "" {
			return &events.Item{Kind: events.ItemThinking, Text: b.Thinking}
		}
	case "text":
		// A user message's text is the prompt or an injected message,
		// not the assistant's.
		if b.Text != "" && role == "assistant" {
			return &events.Item{Kind: events.ItemText, Text: b.Text}
		}
	case "tool_use":
		return &events.Item{Kind: events.ItemToolCall, ID: b.ID, Tool: BareToolName(b.Name), Args: b.Input}
	case "tool_result":
		return &events.Item{Kind: events.ItemToolResult, ID: b.ToolUseID, Text: toolResultText(b.Content), IsError: b.IsError}
	}
	return nil
}

// BareToolName strips the mcp__<server>__ prefix claude gives every MCP
// tool, so a gateway tool is named as the gateway names it.
func BareToolName(name string) string {
	rest, ok := strings.CutPrefix(name, "mcp__")
	if !ok {
		return name
	}
	if _, tool, ok := strings.Cut(rest, "__"); ok {
		return tool
	}
	return name
}

func (p *parser) Outcome() agentcli.Outcome {
	o := agentcli.Outcome{FinalText: p.final, CLISession: p.session, Finished: p.finished, Turns: p.turns}
	o.Failure = p.classify()
	if o.Failure != nil {
		o.FinalText = ""
	}
	return o
}

// classify is grain's run-failure classification, from the output
// alone: a usage limit, no capacity, the turn budget, a refusal, or the
// run's own failure.
func (p *parser) classify() *events.CLIError {
	if p.finished && !p.isError {
		// Claude reports a usage limit as an ordinary answer.
		if m := resetEpochPattern.FindStringSubmatch(p.result); m != nil {
			return usageLimit(p.result, resetFromEpoch(m[1]))
		}
		if isHitLimitAnswer(p.result) {
			return usageLimit(p.result, resetFromClock(p.result, usageLimitNow()))
		}
		return nil
	}
	text := p.errorText
	if p.finished {
		text = strings.TrimSpace(p.result + "\n" + p.errorText)
	}
	if !p.finished && text == "" {
		return nil
	}
	switch {
	case mentionsUsageLimit(text):
		at := time.Time{}
		if m := resetEpochPattern.FindStringSubmatch(text); m != nil {
			at = resetFromEpoch(m[1])
		} else {
			at = resetFromClock(text, usageLimitNow())
		}
		return usageLimit(text, at)
	case agentcli.MentionsCapacity(text, capacityMarkers):
		return &events.CLIError{Kind: events.CLIErrorOverloaded, Message: agentcli.TrimMessage(text)}
	case p.subtype == maxTurnsSubtype:
		return &events.CLIError{Kind: events.CLIErrorMaxTurns, Message: agentcli.TrimMessage(text)}
	case agentcli.ResemblesRefusal(text):
		return &events.CLIError{Kind: events.CLIErrorRefused, Message: agentcli.TrimMessage(text)}
	}
	msg := text
	if msg == "" {
		msg = p.subtype
	}
	return &events.CLIError{Kind: events.CLIErrorOther, Message: agentcli.TrimMessage(msg)}
}

func usageLimit(text string, at time.Time) *events.CLIError {
	e := &events.CLIError{Kind: events.CLIErrorUsageLimit, Message: agentcli.TrimMessage(text)}
	if !at.IsZero() {
		e.ResetAt = &at
	}
	return e
}

// parseBlocks reads a message's content: a plain string or a list of
// typed blocks.
func parseBlocks(raw json.RawMessage) []rawBlock {
	var blocks []rawBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return blocks
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		return []rawBlock{{Type: "text", Text: s}}
	}
	return nil
}

// toolResultText reads a tool_result's content, which has the same
// string-or-blocks shape, joining text blocks with a space.
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		parts := make([]string, 0, len(blocks))
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}
