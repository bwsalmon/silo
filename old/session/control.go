package session

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"time"

	"github.com/bwsalmon/silo/old/control"
)

func init() { RegisterStep(Step{Name: "control", Order: 10, Run: stepControl}) }

// InjectParams are the control socket's inject method's params.
type InjectParams struct {
	Message string `json:"message"`
}

// InjectResult is what inject answers.
type InjectResult struct {
	Delivery Delivery `json:"delivery"`
}

// CancelParams are cancel's params.
type CancelParams struct {
	Reason string `json:"reason,omitempty"`
}

// ExtendParams are extend_deadline's params.
type ExtendParams struct {
	Deadline time.Time `json:"deadline"`
}

// ExtendResult is what extend_deadline answers.
type ExtendResult struct {
	Deadline time.Time `json:"deadline"`
}

// stepControl opens the session's control socket, control.sock in the
// session directory (design §3.2, grain.md §2.1), before anything that
// registers a method on it, and registers inject, cancel and
// extend_deadline; the local executor registers approve (stage 4a).
// Only the invoking user can open the socket (mode 0600, in a 0700
// directory): that is its whole authentication.
func stepControl(ctx context.Context, s *Session) (Undo, error) {
	srv, err := control.Listen(filepath.Join(s.Dir, control.FileName))
	if err != nil {
		return nil, err
	}
	s.hookMu.Lock()
	s.Control = srv
	s.hookMu.Unlock()
	srv.Handle("inject", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p InjectParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if p.Message == "" {
			return nil, control.Errorf(`inject: want {"message": "..."}`)
		}
		d, err := s.Inject(p.Message)
		if err != nil {
			return nil, control.Errorf("inject: %v", err)
		}
		return InjectResult{Delivery: d}, nil
	})
	srv.Handle("cancel", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p CancelParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if p.Reason == "" {
			p.Reason = "the control socket's cancel"
		}
		s.requestCancel(p.Reason)
		return struct{}{}, nil
	})
	srv.Handle("extend_deadline", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p ExtendParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if p.Deadline.IsZero() {
			return nil, control.Errorf(`extend_deadline: want {"deadline": "<RFC 3339 time>"}`)
		}
		s.requestExtend(p.Deadline)
		return ExtendResult{Deadline: p.Deadline.UTC()}, nil
	})
	return func(context.Context) error { return srv.Close() }, nil
}

// decodeParams decodes a method's params, which must be an object when
// given; unknown fields are refused, so a misspelt one is not ignored.
func decodeParams(raw json.RawMessage, v any) error {
	if string(raw) == "null" {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return control.Errorf("params: %v", err)
	}
	return nil
}

// requestCancel ends the session as cancelled, with reason: Cancel and
// the control socket's cancel. A second request before the first is
// taken is dropped.
func (s *Session) requestCancel(reason string) {
	select {
	case s.cancel <- reason:
	default:
	}
}

// requestExtend moves the session's deadline to t, replacing a move not
// yet taken. It takes effect once the CLI has started.
func (s *Session) requestExtend(t time.Time) {
	for {
		select {
		case s.extend <- t:
			return
		default:
		}
		select {
		case <-s.extend:
		default:
		}
	}
}

// ControlSocket is the control socket of session id, which need not be
// running.
func ControlSocket(getenv func(string) string, id string) string {
	return filepath.Join(RuntimeRoot(getenv), id, control.FileName)
}
