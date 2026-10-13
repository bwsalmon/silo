package docker

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/bwsalmon/silo/pkg/facade"
	"github.com/bwsalmon/silo/pkg/ledger"
)

// Error is a refusal in Docker's shape, {"message": "…"}, with Docker's
// status codes (local-backend.md, "Names, ids and errors").
type Error struct {
	Code    int
	Message string
}

func (e *Error) Error() string { return e.Message }

func errorf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func invalid(format string, args ...any) *Error {
	return errorf(http.StatusBadRequest, format, args...)
}
func forbidden(format string, args ...any) *Error {
	return errorf(http.StatusForbidden, format, args...)
}
func notFound(format string, args ...any) *Error { return errorf(http.StatusNotFound, format, args...) }
func conflict(format string, args ...any) *Error { return errorf(http.StatusConflict, format, args...) }
func unsupported(format string, args ...any) *Error {
	return errorf(http.StatusNotImplemented, format, args...)
}
func unavailable(format string, args ...any) *Error {
	return errorf(http.StatusServiceUnavailable, format, args...)
}

// pageNotFound is Docker's answer to a route it doesn't have.
var pageNotFound = &Error{Code: http.StatusNotFound, Message: "page not found"}

// notSwarm is Docker's own answer for a node outside a swarm, which
// clients handle.
var notSwarm = &Error{Code: http.StatusServiceUnavailable,
	Message: "This node is not a swarm manager. Use \"docker swarm init\" or \"docker swarm join\" to connect this node to swarm and try again."}

// asError turns err into an *Error: the core's refusals, the ledger's
// and the address allocator's, and anything else as a 500.
func asError(err error) *Error {
	var (
		e           *Error
		unreachable *facade.Unreachable
		grant       *facade.GrantDenied
		ctl         *facade.ControlDenied
		field       *facade.FieldDenied
		le          *ledger.Error
		status      interface{ HTTPStatus() int }
	)
	switch {
	case errors.As(err, &e):
		return e
	case errors.As(err, &unreachable):
		return notFound("silo: no such silo here")
	case errors.As(err, &grant):
		return forbidden("%s", grant.Error())
	case errors.As(err, &ctl):
		return forbidden("silo: this silo's control policy is %s; %s", ctl.Policy, ctl.Error())
	case errors.As(err, &field):
		return invalid("%s", field.Error())
	case errors.As(err, &le):
		switch le.Code {
		case ledger.QuotaExceeded:
			return conflict("silo: quota exceeded: %s", le.Message)
		case ledger.InvalidArgument, ledger.FailedPrecondition:
			return invalid("silo: %s", le.Message)
		case ledger.ResourceExhausted:
			return conflict("silo: %s", le.Message)
		case ledger.NotFound:
			return notFound("silo: %s", le.Message)
		}
	case errors.As(err, &status):
		return errorf(status.HTTPStatus(), "silo: %v", err)
	}
	return errorf(http.StatusInternalServerError, "silo: %v", err)
}

func writeError(w http.ResponseWriter, err error) {
	e := asError(err)
	writeJSON(w, e.Code, map[string]string{"message": e.Message})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}
