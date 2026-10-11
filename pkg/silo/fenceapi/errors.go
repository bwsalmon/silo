package fenceapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/bwsalmon/silo/pkg/silo/ledger"
	"github.com/bwsalmon/silo/pkg/silo/manage"
	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/tree"
)

// Code is one of the fence API's error codes (fence-api.md, "Errors",
// SV-2).
type Code string

// Every code in the table, with the HTTP status it is sent with.
const (
	InvalidArgument    Code = "INVALID_ARGUMENT"
	PermissionDenied   Code = "PERMISSION_DENIED"
	ControlDenied      Code = "CONTROL_DENIED"
	LooserThanParent   Code = "LOOSER_THAN_PARENT"
	QuotaExceeded      Code = "QUOTA_EXCEEDED"
	ResourceExhausted  Code = "RESOURCE_EXHAUSTED"
	NotFound           Code = "NOT_FOUND"
	AlreadyExists      Code = "ALREADY_EXISTS"
	FailedPrecondition Code = "FAILED_PRECONDITION"
	Unimplemented      Code = "UNIMPLEMENTED"
	Unavailable        Code = "UNAVAILABLE"
	Internal           Code = "INTERNAL"
)

// Status is the HTTP status a code is sent with.
func (c Code) Status() int {
	switch c {
	case InvalidArgument:
		return http.StatusBadRequest
	case PermissionDenied, ControlDenied, LooserThanParent, QuotaExceeded:
		return http.StatusForbidden
	case ResourceExhausted:
		return http.StatusTooManyRequests
	case NotFound:
		return http.StatusNotFound
	case AlreadyExists, FailedPrecondition:
		return http.StatusConflict
	case Unimplemented:
		return http.StatusNotImplemented
	case Unavailable:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// Error is a refusal, sent as {"error": {"code", "message", "details"}}.
type Error struct {
	Code    Code           `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
	// cause is the error an INTERNAL one was made from. Unwrapping to
	// it lets the store see a serialization failure inside a
	// transaction and retry it.
	cause error
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// Unwrap is the error e was made from, if any.
func (e *Error) Unwrap() error { return e.cause }

func errorf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func (e *Error) with(k string, v any) *Error {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	e.Details[k] = v
	return e
}

// notFound is how every silo the caller may not see is refused, so
// that nothing outside its subtree is revealed.
func notFound(ref string) *Error {
	return errorf(NotFound, "no silo %s", ref).with("silo", ref)
}

// asError turns anything a handler returned into the fence API's
// shape: an *Error as it is, a ledger refusal by its code, and
// anything else as INTERNAL (or UNAVAILABLE when the store is gone).
func asError(ctx context.Context, tx store.Tx, err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	var le *ledger.Error
	if errors.As(err, &le) {
		out := &Error{Code: Code(le.Code), Message: le.Message}
		if le.Class != "" {
			out.with("class", le.Class)
		}
		if le.Region != "" {
			out.with("region", le.Region)
		}
		if le.Code == ledger.QuotaExceeded || le.Code == ledger.FailedPrecondition {
			if le.Asked != 0 || le.Free != 0 {
				out.with("asked", jsonAmount(le.Asked)).with("free", jsonAmount(le.Free))
			}
		}
		if le.Silo != "" && tx != nil {
			if s, err := tree.Get(ctx, tx, le.Silo); err == nil {
				out.with("silo", s.Path)
			}
		}
		return out
	}
	var be interface {
		Budget() (quota string, limit, want int)
	}
	if errors.As(err, &be) {
		quota, limit, want := be.Budget()
		return (&Error{Code: ResourceExhausted, Message: err.Error()}).with("quota", quota).with("limit", limit).with("want", want)
	}
	var inv *manage.Invalid
	if errors.As(err, &inv) {
		return (&Error{Code: InvalidArgument, Message: inv.Message}).with("field", inv.Field)
	}
	var pre *manage.Precondition
	if errors.As(err, &pre) {
		return &Error{Code: FailedPrecondition, Message: pre.Message}
	}
	var den *manage.Denied
	if errors.As(err, &den) {
		return grantDetails(&Error{Code: PermissionDenied, Message: den.Error()}, den.Grant)
	}
	switch {
	case errors.Is(err, manage.ErrExists):
		return &Error{Code: AlreadyExists, Message: err.Error()}
	case errors.Is(err, manage.ErrNotFound):
		return &Error{Code: NotFound, Message: err.Error()}
	case errors.Is(err, tree.ErrExists):
		return &Error{Code: AlreadyExists, Message: err.Error()}
	case errors.Is(err, tree.ErrTooDeep):
		return (&Error{Code: ResourceExhausted, Message: err.Error()}).with("depth_cap", tree.MaxDepth)
	case errors.Is(err, tree.ErrInvalid):
		return &Error{Code: InvalidArgument, Message: err.Error()}
	case errors.Is(err, store.ErrClosed), errors.Is(err, store.ErrConflict), errors.Is(err, context.Canceled):
		return &Error{Code: Unavailable, Message: err.Error()}
	}
	return &Error{Code: Internal, Message: err.Error(), cause: err}
}
