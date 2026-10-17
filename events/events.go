// Package events is the schema of the events a grain node streams to the
// hub for each task it runs (docs/hub.md §7.1): one JSON line per event,
// each an Event envelope whose Type names the one payload field it
// carries. It came from the retired single-run runner's package of the
// same name (old/events, described in old/docs/events.md); this module
// keeps its own copy so the hub does not import old/.
package events

import (
	"encoding/json"
	"fmt"
	"time"
)

// Version is the schema version every event carries in "v". It changes
// only when a field changes meaning or goes away; new fields and new
// event types do not change it, so a reader must ignore what it does
// not know.
const Version = 1

// Event types.
const (
	TypeStatus          = "status"
	TypeTranscript      = "transcript"
	TypeToolCall        = "tool_call"
	TypeUsage           = "usage"
	TypeApprovalRequest = "approval_request"
	TypeResumable       = "resumable"
	TypeResult          = "result"
	// TypeAwaitingReply is an interactive session waiting for a human's
	// message, and then going on again.
	TypeAwaitingReply = "awaiting_reply"
	// TypeMessage is a human's message, sent through the hub, as the
	// session took it in.
	TypeMessage = "message"
)

// Event is one thing that happened in a session.
type Event struct {
	V int `json:"v"`
	// Seq numbers a session's events from 1, in the order they were
	// emitted.
	Seq     int64     `json:"seq"`
	Time    time.Time `json:"time"`
	Session string    `json:"session,omitempty"`
	Type    string    `json:"type"`

	// Exactly one of these is set, the one Type names.
	Status          *Status          `json:"status,omitempty"`
	Transcript      *Item            `json:"transcript,omitempty"`
	ToolCall        *ToolCall        `json:"tool_call,omitempty"`
	Usage           *Usage           `json:"usage,omitempty"`
	ApprovalRequest *ApprovalRequest `json:"approval_request,omitempty"`
	Resumable       *Resumable       `json:"resumable,omitempty"`
	Result          *Result          `json:"result,omitempty"`
	AwaitingReply   *AwaitingReply   `json:"awaiting_reply,omitempty"`
	Message         *Message         `json:"message,omitempty"`
}

// Phases of a session, for Status.
const (
	PhaseSetup    = "setup"
	PhaseRunning  = "running"
	PhaseTeardown = "teardown"
)

// States of a step, for Status.
const (
	StateStarted = "started"
	StateDone    = "done"
	StateFailed  = "failed"
	// StateInfo is a line of progress that is not a step's start or end;
	// StateWarning one the user should read.
	StateInfo    = "info"
	StateWarning = "warning"
)

// Status is a setup or teardown step starting or ending, or a line of
// progress.
type Status struct {
	Phase   string `json:"phase"`
	Step    string `json:"step,omitempty"`
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}

// Kinds of transcript item.
const (
	ItemText       = "text"     // the assistant's text
	ItemThinking   = "thinking" // reasoning, where the CLI exposes it
	ItemToolCall   = "tool_call"
	ItemToolResult = "tool_result"
	ItemError      = "error" // an error the CLI reported mid-run
)

// Item is one CLI transcript item, normalised across CLIs. Tool calls
// are the CLI's own -- built-in tools and the gateway's alike; the
// gateway's calls are also reported, as audited, by tool_call events.
type Item struct {
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
	// ID pairs a tool_call with its tool_result.
	ID string `json:"id,omitempty"`
	// Tool is the tool's bare name: a gateway tool without the CLI's
	// mcp__silo__ prefix.
	Tool    string         `json:"tool,omitempty"`
	Args    map[string]any `json:"args,omitempty"`
	IsError bool           `json:"is_error,omitempty"`
}

// Phases of a gateway call, for ToolCall.
const (
	CallStart = "start"
	CallEnd   = "end"
)

// ToolCall is a gateway call's start or end, as its audit line has it
// (credentials already redacted).
type ToolCall struct {
	Phase string `json:"phase"`
	Call  int64  `json:"call"`
	Tool  string `json:"tool"`
	// As is the name the agent called Tool by, when the session's
	// rename map gives it another.
	As   string         `json:"as,omitempty"`
	Args map[string]any `json:"args,omitempty"`
	// End only.
	Outcome    string `json:"outcome,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Size       *int   `json:"size,omitempty"`
	Head       string `json:"head,omitempty"`
	DurationMS *int64 `json:"duration_ms,omitempty"`
}

// SourceAgent tags usage the agent CLI reported.
const SourceAgent = "agent"

// Usage is tokens one model spent, as the CLI reports them: per message
// or per turn in usage events, summed per model in a result's totals.
type Usage struct {
	Source           string `json:"source"`
	Model            string `json:"model,omitempty"`
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64  `json:"cache_write_tokens,omitempty"`
}

// Add adds u's tokens to t.
func (t *Usage) Add(u Usage) {
	t.InputTokens += u.InputTokens
	t.OutputTokens += u.OutputTokens
	t.CacheReadTokens += u.CacheReadTokens
	t.CacheWriteTokens += u.CacheWriteTokens
}

// ApprovalRequest is a gateway call waiting for the user's approval
// (a tool with approve: true, design §12.3). Under --json it is
// answered over the control socket's approve method by ID.
type ApprovalRequest struct {
	ID   string         `json:"id"`
	Tool string         `json:"tool"`
	Args map[string]any `json:"args,omitempty"`
	// Message is the request in a sentence, with the silo approve
	// command that answers it.
	Message string `json:"message,omitempty"`
	// Deadline is when the call gives up waiting.
	Deadline *time.Time `json:"deadline,omitempty"`
}

// States of an AwaitingReply.
const (
	// AwaitingWaiting is the session stopped until a human replies.
	AwaitingWaiting = "waiting"
	// AwaitingResumed is the session going on again, after a reply or
	// for any other reason.
	AwaitingResumed = "resumed"
)

// AwaitingReply is an interactive session's agent waiting for a human
// (State AwaitingWaiting), with the question it asks if it asks one,
// and the matching resume when it goes on (AwaitingResumed), with the
// ID of the message it goes on with if a message woke it.
type AwaitingReply struct {
	State    string `json:"state"`
	Question string `json:"question,omitempty"`
	// MessageID is the Message that resumed the session.
	MessageID string `json:"message_id,omitempty"`
}

// Attachment is a file a message refers to, by reference only: the hub
// keeps no file a human uploads (docs/hub-interface.md).
type Attachment struct {
	Name string `json:"name"`
	// Ref is where the file is: an https URL, or the ref of one of the
	// task's own artifacts.
	Ref       string `json:"ref"`
	MediaType string `json:"media_type,omitempty"`
	Size      int64  `json:"size,omitempty"`
}

// Message is a human's message to an interactive session, sent through
// the hub. The node reports one it has taken in by its ID; the hub
// stores and shows its own record of the message, not the node's.
type Message struct {
	ID          string       `json:"id"`
	Text        string       `json:"text,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
	// Sender is who sent it, the hub's claim from their sign-on.
	Sender string    `json:"sender,omitempty"`
	SentAt time.Time `json:"sent_at,omitzero"`
}

// Resumable is the CLI's own session id, which resumes the
// conversation (claude --resume, codex exec resume).
type Resumable struct {
	CLI        string `json:"cli"`
	CLISession string `json:"cli_session"`
}

// Exit reasons.
const (
	ReasonCompleted      = "completed"
	ReasonCancelled      = "cancelled"
	ReasonDeadline       = "deadline"
	ReasonCLIError       = "cli_error"
	ReasonConnectionLost = "connection_lost"
	ReasonSetupFailed    = "setup_failed"
)

// Kinds of CLI error.
const (
	CLIErrorUsageLimit = "usage_limit" // the account's usage or rate limit
	CLIErrorOverloaded = "overloaded"  // the provider had no capacity
	CLIErrorRefused    = "refused"     // the provider refused the request (auth, quota, an API error)
	CLIErrorMaxTurns   = "max_turns"   // the CLI's own turn budget ran out
	CLIErrorOther      = "error"       // any other failure the CLI reported, or a non-zero exit
)

// CLIError is what the CLI's own output said went wrong.
type CLIError struct {
	Kind    string `json:"kind"`
	Message string `json:"message,omitempty"`
	// ResetAt is when a usage limit lifts, when the CLI said.
	ResetAt *time.Time `json:"reset_at,omitempty"`
	// RetryAfterSeconds is how long to wait before trying again, when
	// the CLI said that instead.
	RetryAfterSeconds int64 `json:"retry_after_seconds,omitempty"`
}

// Exit is how a session ended.
type Exit struct {
	// Code is the agent session's exit code.
	Code   int       `json:"code"`
	Reason string    `json:"reason"`
	Detail string    `json:"detail,omitempty"`
	CLI    *CLIError `json:"cli_error,omitempty"`
}

// Result is a session's outcome: the last event of every session.
type Result struct {
	Exit       Exit   `json:"exit"`
	FinalText  string `json:"final_text,omitempty"`
	CLISession string `json:"cli_session,omitempty"`
	// UsageTotals is the tokens spent, one entry per model.
	UsageTotals []Usage `json:"usage_totals,omitempty"`
	AuditPath   string  `json:"audit_path,omitempty"`
	// TranscriptPath is the CLI's raw headless output;
	// NormalizedTranscriptPath the transcript events, as JSON lines.
	TranscriptPath           string `json:"transcript_path,omitempty"`
	NormalizedTranscriptPath string `json:"normalized_transcript_path,omitempty"`
	// Errors are what went wrong in setup and teardown.
	Errors []string `json:"errors,omitempty"`
}

// Marshal encodes e as one JSON line, newline included.
func Marshal(e Event) ([]byte, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Unmarshal decodes one event and checks it: a version this package
// reads, and the payload its type names.
func Unmarshal(b []byte, e *Event) error {
	*e = Event{}
	if err := json.Unmarshal(b, e); err != nil {
		return err
	}
	if e.V != Version {
		return fmt.Errorf("events: schema version %d, want %d", e.V, Version)
	}
	if e.payload() == nil && known(e.Type) {
		return fmt.Errorf("events: %s event without its %s field", e.Type, e.Type)
	}
	return nil
}

func known(t string) bool {
	switch t {
	case TypeStatus, TypeTranscript, TypeToolCall, TypeUsage, TypeApprovalRequest, TypeResumable, TypeResult,
		TypeAwaitingReply, TypeMessage:
		return true
	}
	return false
}

func (e *Event) payload() any {
	switch e.Type {
	case TypeStatus:
		return nilIf(e.Status)
	case TypeTranscript:
		return nilIf(e.Transcript)
	case TypeToolCall:
		return nilIf(e.ToolCall)
	case TypeUsage:
		return nilIf(e.Usage)
	case TypeApprovalRequest:
		return nilIf(e.ApprovalRequest)
	case TypeResumable:
		return nilIf(e.Resumable)
	case TypeResult:
		return nilIf(e.Result)
	case TypeAwaitingReply:
		return nilIf(e.AwaitingReply)
	case TypeMessage:
		return nilIf(e.Message)
	}
	return nil
}

func nilIf[T any](p *T) any {
	if p == nil {
		return nil
	}
	return p
}

// Redact passes every string e carries -- messages, transcript text,
// tool arguments, results -- through redact, which replaces credential
// values (design §8.4). Args maps are copied, not changed in place.
func (e *Event) Redact(redact func(string) string) {
	if s := e.Status; s != nil {
		s.Message = redact(s.Message)
	}
	if t := e.Transcript; t != nil {
		t.Text = redact(t.Text)
		t.Args = redactMap(t.Args, redact)
	}
	if c := e.ToolCall; c != nil {
		c.Args = redactMap(c.Args, redact)
		c.Reason, c.Head = redact(c.Reason), redact(c.Head)
	}
	if a := e.ApprovalRequest; a != nil {
		a.Args = redactMap(a.Args, redact)
		a.Message = redact(a.Message)
	}
	if a := e.AwaitingReply; a != nil {
		a.Question = redact(a.Question)
	}
	if r := e.Result; r != nil {
		r.FinalText = redact(r.FinalText)
		r.Exit.Detail = redact(r.Exit.Detail)
		if r.Exit.CLI != nil {
			r.Exit.CLI.Message = redact(r.Exit.CLI.Message)
		}
		for i := range r.Errors {
			r.Errors[i] = redact(r.Errors[i])
		}
	}
}

func redactMap(m map[string]any, redact func(string) string) map[string]any {
	if m == nil {
		return nil
	}
	return redactValue(m, redact).(map[string]any)
}

func redactValue(v any, redact func(string) string) any {
	switch v := v.(type) {
	case string:
		return redact(v)
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			out[redact(k)] = redactValue(e, redact)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = redactValue(e, redact)
		}
		return out
	}
	return v
}
