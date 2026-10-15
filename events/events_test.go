package events

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func samples() []Event {
	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	size, dur := 12, int64(30)
	reset := at.Add(time.Hour)
	base := func(seq int64, typ string) Event {
		return Event{V: Version, Seq: seq, Time: at, Session: "s-abcdefghijkl", Type: typ}
	}
	var out []Event
	e := base(1, TypeStatus)
	e.Status = &Status{Phase: PhaseSetup, Step: "gateway", State: StateDone}
	out = append(out, e)
	e = base(2, TypeTranscript)
	e.Transcript = &Item{Kind: ItemToolCall, ID: "t1", Tool: "sandbox_exec", Args: map[string]any{"command": "ls", "n": 2.0, "list": []any{"a", true}}}
	out = append(out, e)
	e = base(3, TypeToolCall)
	e.ToolCall = &ToolCall{Phase: CallEnd, Call: 1, Tool: "sandbox_exec", Outcome: "ok", Size: &size, Head: "go.mod", DurationMS: &dur}
	out = append(out, e)
	e = base(4, TypeUsage)
	e.Usage = &Usage{Source: SourceAgent, Model: "claude-opus-5-5", InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheWriteTokens: 4}
	out = append(out, e)
	e = base(5, TypeApprovalRequest)
	e.ApprovalRequest = &ApprovalRequest{ID: "a1", Tool: "deploy", Args: map[string]any{"env": "prod"}}
	out = append(out, e)
	e = base(6, TypeResumable)
	e.Resumable = &Resumable{CLI: "claude", CLISession: "abc"}
	out = append(out, e)
	e = base(7, TypeResult)
	e.Result = &Result{
		Exit:       Exit{Code: 1, Reason: ReasonCLIError, Detail: "the agent CLI exited", CLI: &CLIError{Kind: CLIErrorUsageLimit, Message: "limit", ResetAt: &reset, RetryAfterSeconds: 60}},
		FinalText:  "done",
		CLISession: "abc", UsageTotals: []Usage{{Source: SourceAgent, Model: "m", InputTokens: 5}},
		AuditPath: "/a", TranscriptPath: "/t.raw", NormalizedTranscriptPath: "/t", Errors: []string{"x"},
	}
	out = append(out, e)
	return out
}

func TestRoundTrip(t *testing.T) {
	for _, e := range samples() {
		b, err := Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(string(b), "}\n") || strings.Count(string(b), "\n") != 1 {
			t.Fatalf("%s: not one JSON line: %q", e.Type, b)
		}
		var got Event
		if err := Unmarshal(b, &got); err != nil {
			t.Fatalf("%s: %v", e.Type, err)
		}
		if !reflect.DeepEqual(got, e) {
			t.Errorf("%s: round trip\n got %+v\nwant %+v", e.Type, got, e)
		}
	}
}

func TestUnmarshalChecks(t *testing.T) {
	var e Event
	if err := Unmarshal([]byte(`{"v":2,"type":"status","status":{"phase":"setup","state":"info"}}`), &e); err == nil {
		t.Error("a later schema version was accepted")
	}
	if err := Unmarshal([]byte(`{"v":1,"type":"result"}`), &e); err == nil {
		t.Error("a result event without its result was accepted")
	}
	// A type this version does not know is passed on, for the reader to
	// ignore.
	if err := Unmarshal([]byte(`{"v":1,"type":"future","future":{}}`), &e); err != nil || e.Type != "future" {
		t.Errorf("unknown type: %v %+v", err, e)
	}
}

func TestRedact(t *testing.T) {
	red := func(s string) string { return strings.ReplaceAll(s, "sekrit", "[credential:k]") }
	for _, e := range samples() {
		if e.Transcript != nil {
			e.Transcript.Text = "token sekrit"
			e.Transcript.Args = map[string]any{"sekrit": []any{"x sekrit"}}
		}
		if e.Result != nil {
			e.Result.FinalText = "sekrit"
			e.Result.Exit.CLI.Message = "sekrit"
		}
		if e.ToolCall != nil {
			e.ToolCall.Head = "sekrit"
		}
		orig := e.Transcript
		var args map[string]any
		if orig != nil {
			args = orig.Args
		}
		e.Redact(red)
		b, _ := Marshal(e)
		if strings.Contains(string(b), "sekrit") {
			t.Errorf("%s: not redacted: %s", e.Type, b)
		}
		if args != nil && args["sekrit"] == nil {
			t.Errorf("%s: the caller's args map was changed in place", e.Type)
		}
	}
}
