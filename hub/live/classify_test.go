package live

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		name  string
		line  string
		class Class
		// kind and ref are a node-only event's reference.
		kind, ref string
		rejected  bool
	}{
		{name: "status", line: `{"v":1,"seq":1,"type":"status","task":"t-1","status":{"phase":"setup","step":"tools","state":"done"}}`, class: Lifecycle},
		{name: "usage", line: `{"v":1,"seq":1,"type":"usage","task":"t-1","usage":{"source":"agent","input_tokens":3,"output_tokens":4}}`, class: Lifecycle},
		{name: "resumable", line: `{"v":1,"seq":1,"type":"resumable","task":"t-1","resumable":{"cli":"claude","cli_session":"x"}}`, class: Lifecycle},
		{name: "result", line: `{"v":1,"seq":1,"type":"result","task":"t-1","result":{"exit":{"code":0,"reason":"completed"},"final_text":"done"}}`, class: Lifecycle},
		{name: "approval", line: `{"v":1,"seq":1,"type":"approval_request","task":"t-1","approval_request":{"id":"a","tool":"push","args":{"branch":"main"}}}`, class: Conversation},
		{name: "awaiting reply", line: `{"v":1,"seq":1,"type":"awaiting_reply","task":"t-1","awaiting_reply":{"state":"waiting","question":"Which branch?"}}`, class: Lifecycle},
		{name: "resumed", line: `{"v":1,"seq":1,"type":"awaiting_reply","task":"t-1","awaiting_reply":{"state":"resumed","message_id":"m-1"}}`, class: Lifecycle},
		{name: "message", line: `{"v":1,"seq":1,"type":"message","task":"t-1","message":{"id":"m-1","text":"node's copy","sender":"x@example.com"}}`, class: Conversation},
		{name: "text", line: `{"v":1,"seq":1,"type":"transcript","task":"t-1","transcript":{"kind":"text","text":"hello"}}`, class: Token},
		{name: "thinking", line: `{"v":1,"seq":1,"type":"transcript","task":"t-1","transcript":{"kind":"thinking","text":"hmm"}}`, class: Token},

		{name: "gateway call", line: `{"v":1,"seq":1,"type":"tool_call","task":"t-1","tool_call":{"phase":"end","call":1,"tool":"read","head":"package main"},"artifact":{"ref":"call-1","size":12}}`,
			class: NodeOnly, kind: "tool_call", ref: "call-1"},
		{name: "transcript tool result", line: `{"v":1,"seq":1,"type":"transcript","task":"t-1","transcript":{"kind":"tool_result","id":"x","text":"package main"}}`,
			class: NodeOnly, kind: "tool_result"},
		{name: "diff", line: `{"v":1,"seq":1,"type":"artifact","task":"t-1","artifact":{"kind":"diff","ref":"d/1","size":99,"content":"package main"}}`,
			class: NodeOnly, kind: "diff", ref: "d/1"},
		{name: "unknown type", line: `{"v":1,"seq":1,"type":"file","task":"t-1","file":{"path":"main.go","content":"package main"}}`,
			class: NodeOnly, kind: "file"},
		{name: "unknown version", line: `{"v":2,"seq":1,"type":"status","task":"t-1","status":{"phase":"setup","state":"info","message":"package main"}}`,
			class: NodeOnly, kind: "status"},
		{name: "unknown transcript kind", line: `{"v":1,"seq":1,"type":"transcript","task":"t-1","transcript":{"kind":"patch","text":"package main"}}`,
			class: NodeOnly, kind: "patch"},
		{name: "a bad kind is unknown", line: `{"v":1,"seq":1,"type":"Weird Type","task":"t-1","artifact":{"ref":"x\n"}}`,
			class: NodeOnly, kind: "unknown"},

		{name: "top-level field outside the schema", line: `{"v":1,"seq":1,"type":"status","task":"t-1","status":{"phase":"setup","state":"done"},"diff":"package main"}`, rejected: true},
		{name: "payload field outside the schema", line: `{"v":1,"seq":1,"type":"status","task":"t-1","status":{"phase":"setup","state":"done","diff":"package main"}}`, rejected: true},
		{name: "nested field outside the schema", line: `{"v":1,"seq":1,"type":"result","task":"t-1","result":{"exit":{"code":0,"reason":"completed","stdout":"package main"}}}`, rejected: true},
		{name: "a stored event's reference", line: `{"v":1,"seq":1,"type":"status","task":"t-1","status":{"phase":"setup","state":"done"},"artifact":{"ref":"x"}}`, rejected: true},
		{name: "text with a tool's fields", line: `{"v":1,"seq":1,"type":"transcript","task":"t-1","transcript":{"kind":"text","text":"hi","args":{"content":"package main"}}}`, rejected: true},
		{name: "awaiting reply in no state", line: `{"v":1,"seq":1,"type":"awaiting_reply","task":"t-1","awaiting_reply":{"state":"asleep"}}`, rejected: true},
		{name: "message without an id", line: `{"v":1,"seq":1,"type":"message","task":"t-1","message":{"text":"hi"}}`, rejected: true},
		{name: "message field outside the schema", line: `{"v":1,"seq":1,"type":"message","task":"t-1","message":{"id":"m-1","diff":"package main"}}`, rejected: true},
		{name: "no payload", line: `{"v":1,"seq":1,"type":"status","task":"t-1"}`, rejected: true},
		{name: "no task", line: `{"v":1,"seq":1,"type":"status","status":{"phase":"setup","state":"done"}}`, rejected: true},
		{name: "no seq", line: `{"v":1,"type":"status","task":"t-1","status":{"phase":"setup","state":"done"}}`, rejected: true},
		{name: "not an object", line: `["package main"]`, rejected: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, class, err := Classify([]byte(c.line))
			if c.rejected {
				if !errors.Is(err, ErrRejected) {
					t.Fatalf("got %v, %v; want it rejected", class, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if class != c.class {
				t.Fatalf("class %v, want %v", class, c.class)
			}
			b, _ := json.Marshal(e)
			if class == NodeOnly {
				if e.Type != TypeArtifact || e.Artifact == nil || e.Artifact.Kind != c.kind || e.Artifact.Ref != c.ref {
					t.Fatalf("got %s, want an artifact of kind %q, ref %q", b, c.kind, c.ref)
				}
				if strings.Contains(string(b), "package main") {
					t.Fatalf("a node-only event kept its payload: %s", b)
				}
			}
		})
	}
}

func TestClassifyDropsNodePaths(t *testing.T) {
	e, _, err := Classify([]byte(`{"v":1,"seq":9,"type":"result","task":"t-1","result":{"exit":{"code":0,"reason":"completed"},"audit_path":"/home/a/audit","transcript_path":"/home/a/t","normalized_transcript_path":"/home/a/n"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if r := e.Result; r.AuditPath != "" || r.TranscriptPath != "" || r.NormalizedTranscriptPath != "" {
		t.Fatalf("kept the node's paths: %+v", r)
	}
}
