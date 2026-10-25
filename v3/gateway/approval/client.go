package approval

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CallFunc makes one gateway API call; (*gateway.Client).Call is one.
type CallFunc func(method string, params, result any) error

// withAuth merges auth (agent_token or on_behalf_of) into params.
func withAuth(auth map[string]any, params map[string]any) map[string]any {
	for k, v := range auth {
		params[k] = v
	}
	return params
}

// Propose submits b and waits until it is decided and finished, or until
// timeout passes, whichever is first; then it returns the batch's status,
// which is still "pending" (or "approved"/"running") if time ran out.
// auth carries the root agent's credentials, e.g. {"agent_token": tok};
// nil means the caller is identified by its uid.
func Propose(ctx context.Context, call CallFunc, auth map[string]any, b Batch, timeout time.Duration) (*SubmitResult, *Status, error) {
	var sub SubmitResult
	if err := call("run_as_root.submit", withAuth(auth, map[string]any{"batch": b}), &sub); err != nil {
		return nil, nil, err
	}
	st, err := WaitFor(ctx, call, auth, sub.BatchID, timeout)
	return &sub, st, err
}

// WaitFor waits for a batch with run_as_root.wait, as many times as the
// gateway's per-call limit needs, until it finishes or timeout passes.
func WaitFor(ctx context.Context, call CallFunc, auth map[string]any, id string, timeout time.Duration) (*Status, error) {
	deadline := time.Now().Add(timeout)
	for {
		left := time.Until(deadline)
		secs := int(left.Round(time.Second) / time.Second)
		if secs < 1 {
			secs = -1 // just read the status
		}
		var st Status
		method := "run_as_root.wait"
		params := map[string]any{"batch_id": id}
		if secs > 0 {
			params["timeout_s"] = secs
		} else {
			method = "run_as_root.status"
		}
		if err := call(method, withAuth(auth, params), &st); err != nil {
			return nil, err
		}
		if Terminal(st.Status) || secs < 0 || ctx.Err() != nil {
			return &st, nil
		}
	}
}

// LoadBatchFile reads a batch from a JSON file for `silo root propose`. A
// file write or attachment may give "source": a path (relative to the batch
// file) whose content is read now and sent inline, so the gateway stages a
// copy and never reads the agent's file again.
func LoadBatchFile(p string) (*Batch, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var in struct {
		Title  string `json:"title"`
		Reason string `json:"reason"`
		Files  []struct {
			FileWrite
			Source  string `json:"source"`
			Content string `json:"content"`
		} `json:"files"`
		Steps       []Step `json:"steps"`
		Attachments []struct {
			Attachment
			Source  string `json:"source"`
			Content string `json:"content"`
		} `json:"attachments"`
		Expects *Expects `json:"expects"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	base := filepath.Dir(p)
	inline := func(what, b64, src, text string) (string, error) {
		n := 0
		for _, s := range []string{b64, src, text} {
			if s != "" {
				n++
			}
		}
		if n > 1 {
			return "", fmt.Errorf("%s: give one of content_b64, content and source", what)
		}
		switch {
		case src != "":
			if !filepath.IsAbs(src) {
				src = filepath.Join(base, src)
			}
			c, err := os.ReadFile(src)
			if err != nil {
				return "", fmt.Errorf("%s: %w", what, err)
			}
			return base64.StdEncoding.EncodeToString(c), nil
		case text != "":
			return base64.StdEncoding.EncodeToString([]byte(text)), nil
		}
		return b64, nil
	}
	b := &Batch{Title: in.Title, Reason: in.Reason, Steps: in.Steps, Expects: in.Expects}
	for i, f := range in.Files {
		fw := f.FileWrite
		if fw.ContentB64, err = inline(fmt.Sprintf("files[%d]", i), fw.ContentB64, f.Source, f.Content); err != nil {
			return nil, err
		}
		b.Files = append(b.Files, fw)
	}
	for i, a := range in.Attachments {
		at := a.Attachment
		if at.ContentB64, err = inline(fmt.Sprintf("attachments[%d]", i), at.ContentB64, a.Source, a.Content); err != nil {
			return nil, err
		}
		b.Attachments = append(b.Attachments, at)
	}
	return b, nil
}

// Describe is the one-paragraph account of a batch's outcome the root
// agent is given (design §7.4).
func Describe(st *Status) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Batch %s (%s…) %q ", st.BatchID, short(st.Hash), st.Title)
	switch st.Status {
	case StatusPending:
		b.WriteString("is pending: the human has not decided yet. Do not resubmit it; the decision arrives as a new message.")
	case StatusApproved, StatusRunning:
		b.WriteString("was approved and is " + st.Status + ".")
	case StatusRejected:
		b.WriteString("was rejected")
		if st.Decision != nil && st.Decision.Comment != "" {
			b.WriteString(": " + st.Decision.Comment)
		}
		b.WriteString(". Read the comment and change your approach; do not resubmit the same batch.")
	case StatusCancelled:
		b.WriteString("was cancelled.")
	default:
		b.WriteString("was approved")
		if st.Decision != nil && st.Decision.Rule != "" {
			b.WriteString(" (standing approval " + st.Decision.Rule + ")")
		}
		b.WriteString(" and " + st.Status)
		if st.Error != "" {
			b.WriteString(": " + st.Error)
		}
		b.WriteString(".")
		if st.SnapshotRef != nil {
			if st.SnapshotRef.ID != "" {
				fmt.Fprintf(&b, " Snapshot %s (%s) was taken first.", st.SnapshotRef.ID, st.SnapshotRef.Kind)
			} else if st.SnapshotRef.Note != "" {
				b.WriteString(" " + st.SnapshotRef.Note + ".")
			}
		}
		if len(st.Files) > 0 {
			fmt.Fprintf(&b, " %d file write(s) applied.", len(st.Files))
		}
		for _, s := range st.Steps {
			fmt.Fprintf(&b, "\nstep %d %v exited %d", s.Index+1, s.Argv, s.Exit)
			if s.Error != "" {
				b.WriteString(" (" + s.Error + ")")
			}
			if s.Stdout != "" {
				fmt.Fprintf(&b, "\n  stdout: %s", strings.TrimSpace(s.Stdout))
			}
			if s.Stderr != "" {
				fmt.Fprintf(&b, "\n  stderr: %s", strings.TrimSpace(s.Stderr))
			}
		}
	}
	return b.String()
}

// MCPToolName is the silo-mcp tool that proposes batches (design §5.3).
const MCPToolName = "run_as_root"

// MCPToolDescription is the tool's description for the agent.
const MCPToolDescription = `Propose a batch of file writes and commands to run as root on this machine. ` +
	`A human reviews the exact files, commands, diffs and risk flags, bound to a hash of everything that will run, and approves or rejects it. ` +
	`Put every file in "files" with full content and every script in "attachments" (reference one in argv as "attachment:<name>"); never point a step at a file an agent can write. ` +
	`Prefer typed operations ({"op":"pkg.install","names":[...]}, unit.enable, unit.restart, user.add_to_group). ` +
	`The call blocks until the batch is decided and has run, or returns status "pending"; do not resubmit a pending batch.`

// MCPInputSchema is the tool's JSON Schema: the batch itself.
var MCPInputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["title", "reason"],
  "properties": {
    "title": {"type": "string", "description": "one line"},
    "reason": {"type": "string", "description": "why, and how to undo it"},
    "files": {"type": "array", "items": {"type": "object", "required": ["path"], "properties": {
      "path": {"type": "string"}, "mode": {"type": "string"}, "owner": {"type": "string"}, "group": {"type": "string"},
      "content_b64": {"type": "string"}, "delete": {"type": "boolean"}}}},
    "attachments": {"type": "array", "items": {"type": "object", "required": ["name", "content_b64"], "properties": {
      "name": {"type": "string"}, "content_b64": {"type": "string"}, "mode": {"type": "string"}}}},
    "steps": {"type": "array", "items": {"type": "object", "properties": {
      "argv": {"type": "array", "items": {"type": "string"}},
      "op": {"type": "string", "enum": ["pkg.install", "pkg.remove", "unit.enable", "unit.restart", "user.add_to_group"]},
      "names": {"type": "array", "items": {"type": "string"}}, "unit": {"type": "string"},
      "user": {"type": "string"}, "group": {"type": "string"},
      "cwd": {"type": "string"}, "env": {"type": "object", "additionalProperties": {"type": "string"}},
      "stdin_attachment": {"type": "string"}, "timeout_s": {"type": "integer"}}}},
    "expects": {"type": "object", "properties": {"reboot": {"type": "boolean"}, "network": {"type": "array", "items": {"type": "string"}}}}
  }
}`)

// MCPCall runs the run_as_root tool for silo-mcp: args is the tool's input
// (a Batch), and the result is the text to return and whether it is an
// error.
func MCPCall(ctx context.Context, call CallFunc, auth map[string]any, args json.RawMessage, timeout time.Duration) (string, bool) {
	var b Batch
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return "run_as_root: bad batch: " + err.Error(), true
	}
	sub, st, err := Propose(ctx, call, auth, b, timeout)
	if err != nil {
		return "run_as_root: " + err.Error(), true
	}
	text := Describe(st)
	if len(sub.Flags) > 0 {
		var codes []string
		for _, f := range sub.Flags {
			codes = append(codes, f.Code)
		}
		text += "\nRisk flags shown to the human: " + strings.Join(codes, ", ")
	}
	text += "\n" + sub.Snapshot
	failed := st.Status == StatusFailed || st.Status == StatusRefused || st.Status == StatusRejected
	return text, failed
}
