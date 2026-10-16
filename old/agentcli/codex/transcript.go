// Adapted from github.com/bwsalmon/grain pkg/agent/codex/transcript.go, live_transcript.go, usagelimit.go and capacity.go at c950ca0858bc4b381e5ce7e2bd716997f63bd24d, Apache License 2.0.

package codex

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/old/agentcli"
	"github.com/bwsalmon/silo/old/events"
)

// NewParser reads codex exec --json: thread.started, turn.started,
// item.started and item.completed, turn.completed and turn.failed, and
// error.
func (Mapping) NewParser(req agentcli.Request) agentcli.Parser {
	return &parser{model: req.Agent.Model, started: map[string]bool{}}
}

type rawEvent struct {
	Type     string    `json:"type"`
	ThreadID string    `json:"thread_id"`
	Item     *rawItem  `json:"item"`
	Usage    *rawUsage `json:"usage"`
	Error    *struct {
		Message string `json:"message"`
	} `json:"error"`
	Message string `json:"message"`
}

type rawItem struct {
	ID string `json:"id"`
	// ItemType is older releases' name for Type.
	ItemType         string          `json:"item_type"`
	Type             string          `json:"type"`
	Text             string          `json:"text"`
	Message          string          `json:"message"`
	Server           string          `json:"server"`
	Tool             string          `json:"tool"`
	Arguments        json.RawMessage `json:"arguments"`
	Result           json.RawMessage `json:"result"`
	Status           string          `json:"status"`
	Error            json.RawMessage `json:"error"`
	Command          string          `json:"command"`
	AggregatedOutput string          `json:"aggregated_output"`
	ExitCode         *int            `json:"exit_code"`
}

type rawUsage struct {
	InputTokens       int64 `json:"input_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
}

// ShellTool is the name a codex command execution is reported under.
const ShellTool = "shell"

// usageLimitMarkers mean "out of budget" in a failed run's text.
var usageLimitMarkers = []string{
	"usage limit", "rate limit exceeded", "rate_limit_exceeded",
	"quota exceeded", "insufficient_quota", "exceeded your current quota",
}

// capacityMarkers are codex's words for a provider with no capacity.
var capacityMarkers = []string{
	"server is overloaded",
	"engine is currently overloaded",
	"server had an error while processing your request",
	"exceeded retry limit, last status: 429",
	"429 too many requests",
}

var retryAfterPattern = regexp.MustCompile(`(?i)try again in\s+(?:(\d+)\s*h)?\s*(?:(\d+)\s*m(?:in)?)?\s*(?:(\d+)\s*s)?`)

type parser struct {
	model    string
	thread   string
	started  map[string]bool
	last     string
	final    string
	finished bool
	turns    int
	failure  string
	failed   bool
	// errorText is the last error event's message: codex reports its
	// retries as errors, so one counts only if no turn ended after it.
	errorText string
}

func (p *parser) Line(line []byte) []events.Event {
	var ev rawEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil
	}
	var out []events.Event
	switch ev.Type {
	case "thread.started":
		if ev.ThreadID != "" && ev.ThreadID != p.thread {
			p.thread = ev.ThreadID
			out = append(out, events.Event{Type: events.TypeResumable, Resumable: &events.Resumable{CLI: "codex", CLISession: ev.ThreadID}})
		}
	case "item.started", "item.completed":
		if ev.Item != nil {
			out = append(out, p.item(ev.Item, ev.Type == "item.completed")...)
		}
	case "turn.started":
		// A resumed session's next turn (an injected message) is judged
		// on its own.
		p.finished = false
	case "turn.completed":
		p.turns++
		p.finished, p.failed, p.failure, p.errorText = true, false, "", ""
		p.final = p.last
		if u := ev.Usage; u != nil {
			out = append(out, events.Event{Type: events.TypeUsage, Usage: &events.Usage{
				Source: events.SourceAgent, Model: p.model,
				InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheReadTokens: u.CachedInputTokens,
			}})
		}
	case "turn.failed":
		p.turns++
		p.finished, p.failed = true, true
		p.failure = failureText(&ev)
		out = append(out, events.Event{Type: events.TypeTranscript, Transcript: &events.Item{Kind: events.ItemError, Text: p.failure}})
	case "error":
		if t := failureText(&ev); t != "" {
			p.errorText = t
			out = append(out, events.Event{Type: events.TypeTranscript, Transcript: &events.Item{Kind: events.ItemError, Text: t}})
		}
	}
	return out
}

func failureText(ev *rawEvent) string {
	if ev.Error != nil && ev.Error.Message != "" {
		return ev.Error.Message
	}
	return ev.Message
}

func (p *parser) item(it *rawItem, completed bool) []events.Event {
	typ := it.Type
	if it.ItemType != "" {
		typ = it.ItemType
	}
	one := func(i *events.Item) []events.Event {
		return []events.Event{{Type: events.TypeTranscript, Transcript: i}}
	}
	switch typ {
	case "agent_message":
		if completed && it.Text != "" {
			p.last = it.Text
			return one(&events.Item{Kind: events.ItemText, Text: it.Text})
		}
	case "reasoning":
		if completed && it.Text != "" {
			return one(&events.Item{Kind: events.ItemThinking, Text: it.Text})
		}
	case "error":
		if completed {
			text := it.Message
			if text == "" {
				text = it.Text
			}
			if text != "" {
				return one(&events.Item{Kind: events.ItemError, Text: text})
			}
		}
	case "mcp_tool_call", "command_execution":
		var out []events.Event
		if !p.started[it.ID] {
			p.started[it.ID] = true
			call := &events.Item{Kind: events.ItemToolCall, ID: it.ID}
			if typ == "command_execution" {
				call.Tool, call.Args = ShellTool, map[string]any{"command": it.Command}
			} else {
				call.Tool, call.Args = it.Tool, decodeArguments(it.Arguments)
			}
			out = append(out, events.Event{Type: events.TypeTranscript, Transcript: call})
		}
		if completed {
			res := &events.Item{Kind: events.ItemToolResult, ID: it.ID}
			if typ == "command_execution" {
				res.Text = it.AggregatedOutput
				res.IsError = strings.EqualFold(it.Status, "failed") || (it.ExitCode != nil && *it.ExitCode != 0)
			} else {
				res.Text, res.IsError = toolResult(it)
			}
			out = append(out, events.Event{Type: events.TypeTranscript, Transcript: res})
		}
		return out
	}
	return nil
}

func (p *parser) Outcome() agentcli.Outcome {
	o := agentcli.Outcome{CLISession: p.thread, Finished: p.finished, Turns: p.turns}
	switch {
	case p.failed:
		o.Failure = classify(p.failure)
	case !p.finished && p.errorText != "":
		o.Failure = classify(p.errorText)
	default:
		o.FinalText = p.final
	}
	return o
}

// classify is grain's run-failure classification of codex's text.
func classify(text string) *events.CLIError {
	msg := agentcli.TrimMessage(text)
	switch {
	case agentcli.Mentions(text, usageLimitMarkers):
		e := &events.CLIError{Kind: events.CLIErrorUsageLimit, Message: msg}
		if d, ok := retryAfter(text); ok {
			e.RetryAfterSeconds = int64(d / time.Second)
		}
		return e
	case agentcli.MentionsCapacity(text, capacityMarkers):
		return &events.CLIError{Kind: events.CLIErrorOverloaded, Message: msg}
	case agentcli.ResemblesRefusal(text):
		return &events.CLIError{Kind: events.CLIErrorRefused, Message: msg}
	}
	return &events.CLIError{Kind: events.CLIErrorOther, Message: msg}
}

func retryAfter(text string) (time.Duration, bool) {
	m := retryAfterPattern.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	total := time.Duration(atoi(m[1]))*time.Hour + time.Duration(atoi(m[2]))*time.Minute + time.Duration(atoi(m[3]))*time.Second
	return total, total > 0
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// decodeArguments reads a tool call's arguments: an object, or a string
// holding one.
func decodeArguments(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err == nil {
		return args
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || s == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(s), &args); err != nil {
		return nil
	}
	return args
}

func toolResult(it *rawItem) (string, bool) {
	failed := strings.EqualFold(it.Status, "failed")
	if len(it.Error) > 0 && string(it.Error) != "null" {
		var s string
		if json.Unmarshal(it.Error, &s) == nil {
			return s, true
		}
		var obj struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(it.Error, &obj) == nil && obj.Message != "" {
			return obj.Message, true
		}
		return string(it.Error), true
	}
	text, isError := resultText(it.Result)
	return text, isError || failed
}

// resultText reads an MCP result: a string, an {Ok|Err} wrapper, or
// {content: [{text}], isError}.
func resultText(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, false
	}
	var wrapped struct {
		Ok  json.RawMessage `json:"Ok"`
		Err json.RawMessage `json:"Err"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil {
		switch {
		case len(wrapped.Err) > 0:
			text, _ := resultText(wrapped.Err)
			return text, true
		case len(wrapped.Ok) > 0:
			return resultText(wrapped.Ok)
		}
	}
	var content struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &content); err == nil && len(content.Content) > 0 {
		parts := make([]string, 0, len(content.Content))
		for _, c := range content.Content {
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
		return strings.Join(parts, " "), content.IsError
	}
	return string(raw), false
}
