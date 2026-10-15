// Package live is the Silo hub's live event path (docs/hub.md §7): the
// session events a node streams up for each task it runs, sorted by
// class, kept or not by that class, and fanned out to the task's
// subscribers over Server-Sent Events or a WebSocket; and the
// view-through that reaches a node's diffs and tool logs without the
// hub ever keeping them.
//
// The node classifies what it sends, because only the node knows what
// it holds; the hub enforces the classification by schema (Classify):
//
//	lifecycle     status, usage, resumable, result   stored, broadcast
//	conversation  approval_request                   stored, broadcast
//	token stream  transcript text, thinking, error   an in-memory ring per task, broadcast
//	node-only     everything else: tool_call,        a reference only, in the ring, broadcast;
//	              transcript tool calls and results,  the payload is dropped
//	              artifact, any type the hub does not know
//
// A stored or token event carrying a field outside its type's schema is
// rejected: neither kept nor broadcast. Nothing node-only is ever
// written to the hub's store, and nothing that passes through the
// view-through is written anywhere.
package live

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/bwsalmon/silo/events"
)

// Class is how the hub treats an event.
type Class int

const (
	// NodeOnly is an event whose payload never leaves its node: the hub
	// keeps and broadcasts a reference to it (an artifact event) and
	// nothing else. An event the hub cannot classify is node-only.
	NodeOnly Class = iota
	// Lifecycle is a task's progress and outcome: stored and broadcast.
	Lifecycle
	// Conversation is what a human reads and answers: stored and
	// broadcast.
	Conversation
	// Token is the assistant's text as it streams: kept in an in-memory
	// ring per task, for late joiners, and broadcast; never stored.
	Token
)

func (c Class) String() string {
	switch c {
	case Lifecycle:
		return "lifecycle"
	case Conversation:
		return "conversation"
	case Token:
		return "token"
	}
	return "node-only"
}

// Stored is whether the hub's store keeps events of class c.
func (c Class) Stored() bool { return c == Lifecycle || c == Conversation }

// TypeArtifact is the type of the reference the hub keeps in place of a
// node-only event. A node may also send one itself, for something it
// holds that no other event names (a diff).
const TypeArtifact = "artifact"

// Artifact is a node-only event as the hub has it: what it was and
// where to view it, never what it said.
type Artifact struct {
	// Kind is what the node holds: the artifact kind the node named
	// ("diff"), or else the type of the event it stands for
	// ("tool_call", "tool_result" for a transcript tool item).
	Kind string `json:"kind"`
	// Ref is the node's opaque name for it, which the view-through
	// asks the node for; empty when the node offers no view of it.
	Ref string `json:"ref,omitempty"`
	// Size is its size in bytes, as the node said.
	Size int64 `json:"size,omitempty"`
}

// Event is an event as the hub has it: the envelope of package events
// with the hub's task ID added, and, in place of a node-only event's
// payload, its Artifact.
type Event struct {
	events.Event
	Task     string    `json:"task"`
	Artifact *Artifact `json:"artifact,omitempty"`
}

// Limits on what a node sends.
const (
	maxRef  = 512
	maxLine = 1 << 20
)

var kindName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

// ErrRejected is an event the hub keeps no part of: one whose payload
// strays outside its type's schema.
var ErrRejected = errors.New("live: event rejected")

func rejected(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRejected, fmt.Sprintf(format, args...))
}

// Classify reads one event line as a node sent it and returns what the
// hub may keep of it, and its class. A node-only event comes back as
// an artifact event holding only its reference, whatever the line
// carried. A lifecycle, conversation or token event comes back as
// decoded against its type's schema; a field outside that schema, at
// the top or in the payload, rejects the event (ErrRejected), as does
// a line that is not an event at all.
func Classify(line []byte) (Event, Class, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return Event{}, 0, rejected("not a JSON object: %v", err)
	}
	var e Event
	for k, dst := range map[string]any{"v": &e.V, "seq": &e.Seq, "time": &e.Time, "session": &e.Session, "type": &e.Type, "task": &e.Task} {
		if b, ok := raw[k]; ok {
			if err := json.Unmarshal(b, dst); err != nil {
				return Event{}, 0, rejected("%s: %v", k, err)
			}
		}
	}
	if e.Task == "" {
		return Event{}, 0, rejected("no task")
	}
	if e.Seq <= 0 {
		return Event{}, 0, rejected("seq %d: want 1 or more", e.Seq)
	}
	class := classOf(e, raw)
	if class == NodeOnly {
		return reference(e, raw), NodeOnly, nil
	}
	for k := range raw {
		switch k {
		case "v", "seq", "time", "session", "type", "task", e.Type:
		default:
			return Event{}, 0, rejected("%s event: field %q is outside its schema", e.Type, k)
		}
	}
	if err := decodePayload(&e, raw[e.Type]); err != nil {
		return Event{}, 0, rejected("%s event: %v", e.Type, err)
	}
	switch {
	case e.Transcript != nil:
		// Only the assistant's words: a tool's name, arguments or ID
		// belong to the node-only tool items.
		t := e.Transcript
		if t.ID != "" || t.Tool != "" || t.Args != nil {
			return Event{}, 0, rejected("transcript %s item carries a tool's fields", t.Kind)
		}
	case e.Result != nil:
		// Paths on the node's disk: what is behind them is node-only.
		r := e.Result
		r.AuditPath, r.TranscriptPath, r.NormalizedTranscriptPath = "", "", ""
	}
	return e, class, nil
}

// classOf sorts an event by its type, and a transcript item by its
// kind. Anything it does not recognise, including a schema version it
// does not read, is node-only.
func classOf(e Event, raw map[string]json.RawMessage) Class {
	if e.V != events.Version {
		return NodeOnly
	}
	switch e.Type {
	case events.TypeStatus, events.TypeUsage, events.TypeResumable, events.TypeResult:
		return Lifecycle
	case events.TypeApprovalRequest:
		return Conversation
	case events.TypeTranscript:
		var item struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(raw[events.TypeTranscript], &item) != nil {
			return NodeOnly
		}
		switch item.Kind {
		case events.ItemText, events.ItemThinking, events.ItemError:
			return Token
		}
	}
	return NodeOnly
}

// decodePayload decodes b, strictly, into the payload field e.Type names.
func decodePayload(e *Event, b json.RawMessage) error {
	var dst any
	switch e.Type {
	case events.TypeStatus:
		e.Status = new(events.Status)
		dst = e.Status
	case events.TypeUsage:
		e.Usage = new(events.Usage)
		dst = e.Usage
	case events.TypeResumable:
		e.Resumable = new(events.Resumable)
		dst = e.Resumable
	case events.TypeResult:
		e.Result = new(events.Result)
		dst = e.Result
	case events.TypeApprovalRequest:
		e.ApprovalRequest = new(events.ApprovalRequest)
		dst = e.ApprovalRequest
	case events.TypeTranscript:
		e.Transcript = new(events.Item)
		dst = e.Transcript
	default:
		return fmt.Errorf("no schema for %q", e.Type)
	}
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		return fmt.Errorf("no %s field", e.Type)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

// reference is the artifact event that stands in for a node-only one:
// its envelope, and of the line's "artifact" object only a kind, a ref
// and a size. Every other field is dropped unread.
func reference(e Event, raw map[string]json.RawMessage) Event {
	var a struct {
		Kind string `json:"kind"`
		Ref  string `json:"ref"`
		Size int64  `json:"size"`
	}
	if b, ok := raw[TypeArtifact]; ok {
		// A malformed reference is an absent one.
		if json.Unmarshal(b, &a) != nil {
			a.Kind, a.Ref, a.Size = "", "", 0
		}
	}
	if a.Kind == "" {
		a.Kind = e.Type
		if e.Type == events.TypeTranscript {
			var item struct {
				Kind string `json:"kind"`
			}
			json.Unmarshal(raw[events.TypeTranscript], &item)
			a.Kind = item.Kind
		}
	}
	if !kindName.MatchString(a.Kind) {
		a.Kind = "unknown"
	}
	if !validRef(a.Ref) {
		a.Ref = ""
	}
	if a.Size < 0 {
		a.Size = 0
	}
	return Event{
		Event: events.Event{V: events.Version, Seq: e.Seq, Time: e.Time, Session: e.Session, Type: TypeArtifact},
		Task:  e.Task,
		Artifact: &Artifact{
			Kind: a.Kind,
			Ref:  a.Ref,
			Size: a.Size,
		},
	}
}

// validRef is whether ref is a name the hub will pass back to a node:
// short and printable.
func validRef(ref string) bool {
	if ref == "" || len(ref) > maxRef {
		return false
	}
	for _, c := range ref {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// stamp gives e the hub's time when the node sent none.
func stamp(e *Event, now time.Time) {
	if e.Time.IsZero() {
		e.Time = now.UTC()
	}
}
