package gce

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	gceapi "github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/ledger"
	"google.golang.org/api/googleapi"
)

// The reasons the facade's own refusals carry (gcp-backend.md,
// "Errors", SV-75). GCE's own errors keep theirs.
const (
	ReasonForbidden        = gceapi.ReasonForbidden
	ReasonRegionNotAllowed = "regionNotAllowed"
	ReasonInvalid          = gceapi.ReasonInvalid
	ReasonNotFound         = gceapi.ReasonNotFound
	ReasonAlreadyExists    = gceapi.ReasonAlreadyExists
	ReasonQuotaExceeded    = gceapi.ReasonQuotaExceeded
	ReasonBackendError     = gceapi.ReasonBackendError
)

// Error is a GCE-shaped error:
//
//	{"error": {"code": 403, "message": "...",
//	  "errors": [{"domain": "global", "reason": "forbidden", "message": "..."}]}}
type Error struct {
	Code    int
	Reason  string
	Domain  string
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("%d %s: %s", e.Code, e.Reason, e.Message) }

func errorf(code int, reason, format string, args ...any) *Error {
	return &Error{Code: code, Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// forbidden is a 403 forbidden: a refused method or field, a project
// outside the caller's reach, or the caller's control policy.
func forbidden(format string, args ...any) *Error {
	return errorf(http.StatusForbidden, ReasonForbidden, format, args...)
}

// invalid is a 400 invalid: a name too long, a range outside the
// silo's space, a filter the facade doesn't support.
func invalid(format string, args ...any) *Error {
	return errorf(http.StatusBadRequest, ReasonInvalid, format, args...)
}

// notFound is GCE's 404 for the resource at path, a path as the tenant
// names it.
func notFound(path string) *Error {
	return errorf(http.StatusNotFound, ReasonNotFound, "The resource '%s' was not found", path)
}

// regionNotAllowed is the 403 for a region, or a zone in one, outside
// the silo's allowed regions (allocators.md, "Allowed regions").
func regionNotAllowed(silo, what string) *Error {
	return errorf(http.StatusForbidden, ReasonRegionNotAllowed, "silo: %s is not in the allowed regions of silo %s", what, silo)
}

// unavailable is the 503 for silod's own failures: the store, or a
// call to GCE that didn't get an answer.
func unavailable(format string, args ...any) *Error {
	return errorf(http.StatusServiceUnavailable, ReasonBackendError, format, args...)
}

// methodNotAllowed is the 403 for a method the facade doesn't allow.
func methodNotAllowed(method string) *Error {
	return forbidden("silo: method not allowed: compute.%s", method)
}

// projectForbidden is GCE's 403 for a project the caller can't reach,
// the same whether or not the silo exists.
func projectForbidden(method, project string) *Error {
	return forbidden("Required 'compute.%s' permission for 'projects/%s'", method, project)
}

// asError turns err into the GCE shape. A GCE error keeps its code and
// reason, with its message rewritten by rw (nil leaves it). A ledger
// refusal is GCE's quotaExceeded or invalid. Anything else, the store
// or a context ending among them, is silod being unavailable.
func asError(err error, rw func(string) string) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) && ge.Code != 0 {
		out := &Error{Code: ge.Code, Message: ge.Message, Reason: gceapi.Reason(ge)}
		if out.Reason == "" {
			out.Reason = ReasonBackendError
		}
		if out.Reason == ReasonQuotaExceeded {
			out.Domain = "usageLimits"
		}
		if rw != nil {
			out.Message = rw(out.Message)
		}
		return out
	}
	var le *ledger.Error
	if errors.As(err, &le) {
		switch le.Code {
		case ledger.QuotaExceeded:
			return errorf(http.StatusForbidden, ReasonQuotaExceeded, "%s", le.Message).domain("usageLimits")
		case ledger.InvalidArgument:
			return invalid("%s", le.Message)
		case ledger.NotFound:
			return errorf(http.StatusNotFound, ReasonNotFound, "%s", le.Message)
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return unavailable("silo: the call did not finish: %v", err)
	}
	return unavailable("silo: silod is unavailable: %v", err)
}

func (e *Error) domain(d string) *Error { e.Domain = d; return e }

// MarshalJSON writes e in GCE's shape.
func (e *Error) MarshalJSON() ([]byte, error) {
	domain := e.Domain
	if domain == "" {
		domain = "global"
	}
	type item struct {
		Domain  string `json:"domain"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
	return json.Marshal(map[string]any{"error": map[string]any{
		"code":    e.Code,
		"message": e.Message,
		"errors":  []item{{Domain: domain, Reason: e.Reason, Message: e.Message}},
	}})
}

func writeError(w http.ResponseWriter, err error, rw func(string) string) {
	e := asError(err, rw)
	b, _ := json.Marshal(e)
	writeRaw(w, e.Code, b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writeError(w, unavailable("silo: encoding the response: %v", err), nil)
		return
	}
	writeRaw(w, status, b)
}

func writeRaw(w http.ResponseWriter, status int, b []byte) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}
