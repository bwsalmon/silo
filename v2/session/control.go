package session

import (
	"context"
	"path/filepath"

	"github.com/bwsalmon/silo/v2/control"
)

func init() { RegisterStep(Step{Name: "control", Order: 10, Run: stepControl}) }

// stepControl opens the session's control socket, control.sock in the
// session directory (design §3.2, grain.md §2.1), before anything that
// registers a method on it. Stage 4a registers approve; stage 6b adds
// inject, cancel and extend_deadline.
func stepControl(ctx context.Context, s *Session) (Undo, error) {
	srv, err := control.Listen(filepath.Join(s.Dir, control.FileName))
	if err != nil {
		return nil, err
	}
	s.Control = srv
	return func(context.Context) error { return srv.Close() }, nil
}

// ControlSocket is the control socket of session id, which need not be
// running.
func ControlSocket(getenv func(string) string, id string) string {
	return filepath.Join(RuntimeRoot(getenv), id, control.FileName)
}
