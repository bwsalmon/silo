package gateway

import (
	"encoding/json"
	"net/http"
	"strings"

	facade "github.com/bwsalmon/silo/pkg/silo/facade/gcp/gce"
	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
)

// refusal is an answer the gateway, or silod's origin check, gives in
// place of the API's own: shaped as GCE's error on the facade's paths,
// and as the fence API's everywhere else.
type refusal struct {
	status  int
	code    fenceapi.Code // for the fence API
	reason  string        // GCE's reason, for the facade
	message string
	details map[string]any
}

func (e *refusal) write(w http.ResponseWriter, path string) {
	var b []byte
	if strings.HasPrefix(path, "/compute/v1/") {
		b, _ = json.Marshal(&facade.Error{Code: e.status, Reason: e.reason, Message: e.message})
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	} else {
		b, _ = json.Marshal(map[string]any{"error": &fenceapi.Error{Code: e.code, Message: e.message, Details: e.details}})
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(e.status)
	w.Write(append(b, '\n'))
}

func forbidden(msg string) *refusal {
	return &refusal{status: http.StatusForbidden, code: fenceapi.PermissionDenied, reason: facade.ReasonForbidden, message: msg}
}

func unavailable(msg string) *refusal {
	return &refusal{status: http.StatusServiceUnavailable, code: fenceapi.Unavailable, reason: facade.ReasonBackendError, message: msg}
}

func notFound(msg string) *refusal {
	return &refusal{status: http.StatusNotFound, code: fenceapi.NotFound, reason: facade.ReasonNotFound, message: msg}
}

// controlDenied is Layer 1's refusal: on the facade, GCE's 403
// forbidden; on the fence API, CONTROL_DENIED.
func controlDenied(path, policy string) *refusal {
	return &refusal{status: http.StatusForbidden, code: fenceapi.ControlDenied, reason: facade.ReasonForbidden,
		message: "silo: control-plane calls from " + path + " are refused (control=" + policy + ")",
		details: map[string]any{"policy": policy}}
}
