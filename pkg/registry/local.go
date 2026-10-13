package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/imagerepos"
)

// Error is a refusal of PushHeld or Pull: the status, code and message
// the registry would have answered over HTTP.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// HTTPStatus is the status the registry answered with.
func (e *Error) HTTPStatus() int { return e.Status }

// PushHeld is the Docker facade's push (local-registry.md, §7
// "Pushing"): it writes manifest, whose bytes and blobs are already in
// the content store, into the local repo name names (its path below
// registry.silo.internal), as c, at ref, a tag or the manifest's
// digest. It is a manifest PUT in every other way: write access as c,
// first-push creation, the manifest's checks, the repo's charge and
// the registry.push record. The one difference is the proof of
// possession: the caller's image record is its proof that it has the
// blobs, so each is checked in the content store and none is uploaded
// or copied. It returns the manifest's digest.
func (s *Service) PushHeld(ctx context.Context, c fenceapi.Caller, name, ref, mediaType string, manifest []byte) (string, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodPut, "/v2/"+name+"/manifests/"+ref, bytes.NewReader(manifest))
	if err != nil {
		return "", err
	}
	r.Header.Set("Content-Type", mediaType)
	rec, err := s.local(r, c, name, func(q *request) { s.putManifest(q, ref) })
	if err != nil {
		return "", err
	}
	if rec.code != http.StatusCreated {
		return "", rec.err()
	}
	return rec.header.Get("Docker-Content-Digest"), nil
}

// Pull is the Docker facade's pull of a local repo (§7, "Pulling from
// the local registry"): name's manifest at ref, as c, with read access
// and the registry.pull record a pull over HTTP makes. Everything c may
// not read is a 404, as it is over HTTP.
func (s *Service) Pull(ctx context.Context, c fenceapi.Caller, name, ref string) (mediaType string, manifest []byte, digest string, err error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, "/v2/"+name+"/manifests/"+ref, nil)
	if err != nil {
		return "", nil, "", err
	}
	rec, err := s.local(r, c, name, func(q *request) { s.getManifest(q, ref) })
	if err != nil {
		return "", nil, "", err
	}
	if rec.code != http.StatusOK {
		return "", nil, "", rec.err()
	}
	return rec.header.Get("Content-Type"), rec.body.Bytes(), rec.header.Get("Docker-Content-Digest"), nil
}

// local serves one request of the facade's in process, as the
// registry serves a client's, and returns what it answered.
func (s *Service) local(r *http.Request, c fenceapi.Caller, name string, serve func(*request)) (*recorder, error) {
	n, ok := imagerepos.ParseName(name, "")
	if !ok || !n.Local() {
		return nil, &Error{Status: http.StatusNotFound, Code: codeNameInvalid, Message: fmt.Sprintf("silo: invalid repository name %q", name)}
	}
	c.Manage = ""
	rec := &recorder{header: http.Header{}}
	s.sweep(r.Context())
	serve(&request{w: rec, r: r, caller: c, name: n, held: r.Method == http.MethodPut})
	return rec, nil
}

// recorder is the response of a request local serves.
type recorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (w *recorder) Header() http.Header { return w.header }

func (w *recorder) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}

func (w *recorder) Write(b []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.body.Write(b)
}

var _ io.Writer = (*recorder)(nil)

// err is a refusal the recorder holds, as an *Error.
func (w *recorder) err() error {
	var out struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	e := &Error{Status: w.code, Code: "UNKNOWN", Message: http.StatusText(w.code)}
	if json.Unmarshal(w.body.Bytes(), &out) == nil && len(out.Errors) > 0 {
		e.Code, e.Message = out.Errors[0].Code, out.Errors[0].Message
	}
	return e
}
