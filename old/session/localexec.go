package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/bwsalmon/silo/old/agentcli"
	"github.com/bwsalmon/silo/old/control"
	"github.com/bwsalmon/silo/old/events"
	"github.com/bwsalmon/silo/old/gateway"
	"github.com/bwsalmon/silo/old/localexec"
)

func init() { RegisterToolProvider("local", provideLocal) }

// ApproveParams are the control socket's approve method's params:
// approve (or deny) call ID, or the one call waiting when ID is empty.
type ApproveParams struct {
	ID   string `json:"id,omitempty"`
	Deny bool   `json:"deny,omitempty"`
}

// ApproveResult is what approve answers: the call it answered.
type ApproveResult struct {
	localexec.Request
	Approved bool `json:"approved"`
}

// Approve answers a local script's call waiting for approval, as the
// control socket's approve does (design §12.3): call id, or the one call
// waiting when id is empty.
func (s *Session) Approve(ctx context.Context, id string, allow bool) (ApproveResult, error) {
	var res ApproveResult
	s.hookMu.Lock()
	ctl := s.Control
	s.hookMu.Unlock()
	if ctl == nil {
		return res, errors.New("approve: the session has no control socket yet")
	}
	err := ctl.Invoke(ctx, "approve", ApproveParams{ID: id, Deny: !allow}, &res)
	var ce *control.Error
	if errors.As(err, &ce) && ce.Code == control.CodeMethodNotFound {
		err = errors.New("approve: this session has no local script that asks for approval")
	}
	return res, err
}

// provideLocal snapshots the local executor's scripts and serves one
// tool per script (design §12). A refused script fails the session.
func provideLocal(ctx context.Context, s *Session) ([]gateway.Tool, Undo, error) {
	f := s.File
	if f.Local == nil {
		return nil, nil, nil
	}
	listed, errs := f.LocalScripts()
	if len(errs) > 0 {
		return nil, nil, &Error{Code: ExitInvalid, Err: errs}
	}
	scripts, err := localexec.Snapshot(f, listed)
	if err != nil {
		return nil, nil, &Error{Code: ExitInvalid, Err: err}
	}
	delivered, err := localexec.Delivered(f.Credentials, s.Credentials)
	if err != nil {
		return nil, nil, &Error{Code: ExitRefused, Err: err}
	}
	ex := localexec.New(localexec.Config{
		Scripts:   scripts,
		Workdir:   localexec.Workdir(f),
		Environ:   localexec.BaseEnviron(s.opts.Environ, f.Credentials, s.Credentials),
		Env:       delivered,
		OutputCap: localexec.OutputCap(f),
		Approver:  s.approver(),
	})
	for _, sc := range scripts {
		note := ""
		if sc.Approve {
			note = ", each call approved by you"
		}
		s.status("local script %s as %s (sha256 %s%s)", sc.Name, sc.Tool, sc.SHA256[:12], note)
	}
	return ex.Tools(), func(context.Context) error { return ex.Close() }, nil
}

// approver picks how approve: true calls are asked about (design
// §12.3): on silo's own terminal in headless mode when it has one, and
// otherwise through silo approve on the control socket, announced by a
// desktop notification in interactive mode and by an approval_request
// event to a sink that is not silo's own terminal lines (--json).
func (s *Session) approver() localexec.Approver {
	if s.Mode == agentcli.Headless && isTerminal(s.opts.Stdin) {
		return &localexec.Terminal{In: s.opts.Stdin, Out: s.opts.Stderr}
	}
	q := &localexec.Queue{Notify: func(r localexec.Request) {
		cmd := fmt.Sprintf("silo approve %s %s", s.ID, r.ID)
		msg := fmt.Sprintf("call %s waits for your approval: %s %s (run %q, or add --deny)", r.ID, r.Tool, r.Summary(), cmd)
		if _, text := s.em.sink.(TextSink); !text || s.Mode != agentcli.Interactive {
			// In interactive mode the terminal is the CLI's.
			args := make([]any, len(r.Args))
			for i, a := range r.Args {
				args[i] = a
			}
			deadline := r.Deadline.UTC()
			s.RequestApproval(events.ApprovalRequest{
				ID: r.ID, Tool: r.Tool, Message: msg, Deadline: &deadline,
				Args: map[string]any{"script": r.Script, "args": args, "stdin_bytes": float64(r.Stdin)},
			})
		}
		if s.Mode == agentcli.Interactive {
			go localexec.Notify(context.Background(), s.getenv("PATH"),
				"silo: "+r.Tool+" waits for approval", r.Summary()+"\n"+cmd)
		}
	}}
	if s.Control != nil {
		s.Control.Handle("approve", func(ctx context.Context, raw json.RawMessage) (any, error) {
			var p ApproveParams
			if string(raw) != "null" {
				if err := json.Unmarshal(raw, &p); err != nil {
					return nil, control.Errorf("approve: %v", err)
				}
			}
			r, err := q.Answer(p.ID, !p.Deny)
			if err != nil {
				return nil, control.Errorf("%v", err)
			}
			return ApproveResult{Request: r, Approved: !p.Deny}, nil
		})
	}
	return q
}
