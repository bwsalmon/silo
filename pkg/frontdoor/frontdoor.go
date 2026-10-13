// Package frontdoor is silod's front door: port 8443 on the control
// VM, reached from outside the cloud through an IAP TCP tunnel
// (docs/silo/gateway.md, "v1: IAP TCP forwarding plus an ID token",
// SV-88).
//
// IAP decides who may open a tunnel at all. This package decides who a
// caller that got through acts as. Every request carries
// Authorization: Bearer <Google ID token> with audience
// silo:<host-project>. The token's signature, audience, issuer and
// expiry are verified and its email is taken. The email is looked up in
// the member table. An identity with no member row gets 403. A mapped
// one is handed to the API as a Caller of its silo, via front_door, the
// same Caller a VM inside that silo is given by the gateway. The facade
// and the fence API then hold it to that silo's reach and its control
// policy. For these callers silod's own check is the only layer, since
// the front door is not behind the fence.
package frontdoor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	facade "github.com/bwsalmon/silo/pkg/facade/gcp/gce"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// Port is the front door's port on the control VM, which the fence rule
// gw-admin-in opens to IAP's range.
const Port = 8443

// Audience is the audience a front-door ID token must carry for the
// Silo installed in hostProject.
func Audience(hostProject string) string { return "silo:" + hostProject }

// The paths the front door serves. /silod/bootstrap is install's, and
// only while there is no root; /silod/health is what install waits on
// before it; /silod/credentials is where uninstall lists the agent
// credentials and deletes each, as a root member.
const (
	BootstrapPath   = "/silod/bootstrap"
	HealthPath      = "/silod/health"
	CredentialsPath = "/silod/credentials"
)

// Options configure a Handler.
type Options struct {
	// Verifier checks each request's ID token. Required.
	Verifier Verifier
	// API serves /compute/v1/, /silo/v1/ and silod's own paths, reading
	// the caller from the request's context.
	API http.Handler
}

// Handler returns the front door's handler over st: it verifies the
// caller, maps it to its silo, and passes the request on to opts.API.
func Handler(st store.Store, opts Options) http.Handler {
	return &handler{st: st, opts: opts}
}

type handler struct {
	st   store.Store
	opts Options
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	api := strings.HasPrefix(path, "/compute/v1/") || strings.HasPrefix(path, "/silo/v1/") ||
		path == CredentialsPath || strings.HasPrefix(path, CredentialsPath+"/")
	if !api && path != BootstrapPath && path != HealthPath {
		refuse(w, path, http.StatusNotFound, "silo: no such path "+path)
		return
	}
	// Who the caller is comes from the token and the member table, and
	// from nothing the caller sends besides: headers meant for silod's
	// other transports are dropped before anything reads them.
	for k := range r.Header {
		if siloHeader(k) {
			r.Header.Del(k)
		}
	}
	tok, ok := bearer(r)
	if !ok {
		refuse(w, path, http.StatusForbidden, "silo: the front door needs Authorization: Bearer <Google ID token>")
		return
	}
	// The API never sees the token: nothing downstream needs it, and
	// nothing downstream should be able to log or replay it.
	r.Header.Del("Authorization")
	id, err := h.opts.Verifier.Verify(r.Context(), tok)
	if err != nil {
		refuse(w, path, http.StatusForbidden, "silo: the ID token is not valid: "+err.Error())
		return
	}
	caller := fenceapi.Caller{Via: fenceapi.ViaFrontDoor, Member: id}
	switch {
	case path == HealthPath:
		// Install waits on this before there are any members; it tells a
		// verified caller only whether silod is serving.
	case path == BootstrapPath:
		rooted, err := h.rooted(r.Context())
		if err != nil {
			refuse(w, path, http.StatusServiceUnavailable, "silo: checking for the root: "+err.Error())
			return
		}
		if rooted {
			refuse(w, path, http.StatusForbidden, "silo: the root silo exists; bootstrap is accepted only before it does")
			return
		}
	default:
		silo, err := MemberSilo(r.Context(), h.st, id)
		if err != nil {
			refuse(w, path, http.StatusServiceUnavailable, "silo: reading the member table: "+err.Error())
			return
		}
		if silo == "" {
			refuse(w, path, http.StatusForbidden, fmt.Sprintf("silo: %s is not a member of any silo", id))
			return
		}
		caller.Silo = silo
	}
	h.opts.API.ServeHTTP(w, r.WithContext(fenceapi.ContextWithCaller(r.Context(), caller)))
}

// MemberSilo is the silo identity is mapped to, or "" if none (or if
// its silo is gone): how every front door, this one and the host's
// registry port (package hostdoor), maps its caller.
func MemberSilo(ctx context.Context, st store.Store, identity string) (string, error) {
	var silo string
	err := st.Tx(ctx, func(tx store.Tx) error {
		id, err := fenceapi.MemberSilo(ctx, tx, identity)
		if err != nil || id == "" {
			return err
		}
		if _, err := tree.Get(ctx, tx, id); errors.Is(err, tree.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		silo = id
		return nil
	})
	return silo, err
}

// rooted reports whether the root silo exists.
func (h *handler) rooted(ctx context.Context) (bool, error) {
	var ok bool
	err := h.st.Tx(ctx, func(tx store.Tx) error {
		_, err := tree.Root(ctx, tx)
		if errors.Is(err, tree.ErrNotFound) {
			return nil
		}
		ok = err == nil
		return err
	})
	return ok, err
}

// bearer is the request's bearer token.
func bearer(r *http.Request) (string, bool) {
	h := r.Header.Values("Authorization")
	if len(h) != 1 {
		return "", false
	}
	scheme, tok, ok := strings.Cut(strings.TrimSpace(h[0]), " ")
	tok = strings.TrimSpace(tok)
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" {
		return "", false
	}
	return tok, true
}

// siloHeader reports whether a header name is one of silod's
// transports' own: Silo-Origin and Silo-Caller*, in any case, with - or
// _.
func siloHeader(k string) bool {
	k = strings.ToLower(strings.ReplaceAll(k, "_", "-"))
	return strings.HasPrefix(k, "silo-origin") || strings.HasPrefix(k, "silo-caller")
}

// refuse answers in place of the API: GCE's error shape on the facade's
// paths, the fence API's everywhere else.
func refuse(w http.ResponseWriter, path string, status int, msg string) {
	var b []byte
	if strings.HasPrefix(path, "/compute/v1/") {
		reason := facade.ReasonForbidden
		switch status {
		case http.StatusNotFound:
			reason = facade.ReasonNotFound
		case http.StatusServiceUnavailable:
			reason = facade.ReasonBackendError
		}
		b, _ = json.Marshal(&facade.Error{Code: status, Reason: reason, Message: msg})
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	} else {
		code := fenceapi.PermissionDenied
		switch status {
		case http.StatusNotFound:
			code = fenceapi.NotFound
		case http.StatusServiceUnavailable:
			code = fenceapi.Unavailable
		}
		b, _ = json.Marshal(map[string]any{"error": &fenceapi.Error{Code: code, Message: msg}})
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}
