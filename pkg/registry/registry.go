// Package registry is silod's registry service for local image repos
// (docs/silo/local-registry.md, §2, §5 and §6): the OCI Distribution
// API v1.1 at registry.silo.internal, which the gateway forwards from
// port 80 by Host to silod's socket, with the caller's silo as
// Silo-Origin.
//
// It is silod's own Go over the content store (Content), not the
// distribution project's registry. The store is the truth for repos,
// manifests and tags (package imagerepos); the content store holds the
// bytes, each once by digest. Who may do what is the fence API's
// RegistryAccess (call L63), and everything a caller may not read is
// the same 404 NAME_UNKNOWN.
//
// One function, Service.blob, decides whether a repo may answer for a
// blob, and every path that names a blob (GET and HEAD, a mount's
// from, a manifest's references) goes through it. It answers from the
// repo's own manifests and its uploads still in their grace hour, never
// from whether the bytes are on disk, so a client proves it has a blob
// by uploading it (call L62).
//
// A repo charges its silo's disk_gb for the distinct blobs its
// manifests reference, its uploads in their grace hour and the bytes of
// its uploads in progress (call L61); a chunk that would take the silo
// over its grant is taken back and refused with 413 DENIED.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/imagerepos"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// The limits of local-registry.md, "Uploads".
const (
	DefaultMaxChunk    = 64 << 20
	DefaultMaxManifest = 4 << 20
	DefaultMaxSessions = 16
	DefaultGrace       = time.Hour
	DefaultIdle        = 15 * time.Minute
)

// Namespace is the containerd namespace of the registry's image
// records, and SeqLabel the label on each that restore reads (§4).
const (
	Namespace = "silo-registry"
	SeqLabel  = "dev.silo.seq"
)

// ManifestChangeKind is the change-log kind of a manifest a push
// records; its sequence number is the silo-registry record's SeqLabel.
const ManifestChangeKind = "image_manifest"

// DiskClass is the class a repo's bytes are charged to: the local
// backend's disk_gb.
var DiskClass = ledger.Key{Kind: "local", Class: "disk_gb", Region: ledger.Global}

// API is what the service needs of the fence API; *fenceapi.Server is
// it.
type API interface {
	RegistryAccess(ctx context.Context, tx store.Tx, c fenceapi.Caller, n imagerepos.Name, write bool) (*fenceapi.RegistryTarget, error)
	RegistryCatalog(ctx context.Context, tx store.Tx, c fenceapi.Caller) ([]string, error)
}

// Options configure a Service.
type Options struct {
	Store   store.Store
	API     API
	Content Content
	// Now is the clock; time.Now if nil.
	Now func() time.Time
	// The limits; the defaults above if zero.
	MaxChunk    int64
	MaxManifest int64
	MaxSessions int
	Grace       time.Duration
	Idle        time.Duration
}

// Service is the registry.
type Service struct {
	opts Options

	mu      sync.Mutex
	uploads map[string]*upload
	// grace is each repo's completed uploads no manifest need yet
	// reference, by repo id and digest (local-registry.md, "Uploads").
	grace map[string]map[string]graceBlob
}

type graceBlob struct {
	size  int64
	until time.Time
}

// New returns a Service.
func New(opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MaxChunk == 0 {
		opts.MaxChunk = DefaultMaxChunk
	}
	if opts.MaxManifest == 0 {
		opts.MaxManifest = DefaultMaxManifest
	}
	if opts.MaxSessions == 0 {
		opts.MaxSessions = DefaultMaxSessions
	}
	if opts.Grace == 0 {
		opts.Grace = DefaultGrace
	}
	if opts.Idle == 0 {
		opts.Idle = DefaultIdle
	}
	return &Service{opts: opts, uploads: map[string]*upload{}, grace: map[string]map[string]graceBlob{}}
}

// SetAPI sets Options.API, for a service made before the fence API.
func (s *Service) SetAPI(api API) { s.opts.API = api }

// Error codes of the distribution spec.
const (
	codeBlobUnknown       = "BLOB_UNKNOWN"
	codeUploadInvalid     = "BLOB_UPLOAD_INVALID"
	codeUploadUnknown     = "BLOB_UPLOAD_UNKNOWN"
	codeDigestInvalid     = "DIGEST_INVALID"
	codeManifestUnknownBl = "MANIFEST_BLOB_UNKNOWN"
	codeManifestInvalid   = "MANIFEST_INVALID"
	codeManifestUnknown   = "MANIFEST_UNKNOWN"
	codeNameInvalid       = "NAME_INVALID"
	codeNameUnknown       = "NAME_UNKNOWN"
	codeSizeInvalid       = "SIZE_INVALID"
	codeUnsupported       = "UNSUPPORTED"
	codeDenied            = "DENIED"
	codeTooMany           = "TOOMANYREQUESTS"
)

// regError is a refusal, written as the spec's error body.
type regError struct {
	status  int
	code    string
	message string
}

func (e *regError) Error() string { return e.code + ": " + e.message }

func rerr(status int, code, format string, args ...any) *regError {
	return &regError{status: status, code: code, message: fmt.Sprintf(format, args...)}
}

func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	var e *regError
	if !errors.As(err, &e) {
		log.Printf("registry: %s %s: %v", r.Method, r.URL.Path, err)
		e = rerr(http.StatusInternalServerError, "UNKNOWN", "silo: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.status)
	if r.Method == http.MethodHead {
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]any{{"code": e.code, "message": e.message}}})
}

// nameUnknown is the one answer for everything the caller may not
// read, whether it exists or not (call L63).
func nameUnknown() *regError {
	return rerr(http.StatusNotFound, codeNameUnknown, "repository name not known to registry")
}

var routeRE = regexp.MustCompile(`^/v2/(.+)/(manifests/[^/]+|blobs/uploads/?[^/]*|blobs/[^/]+|tags/list|referrers/[^/]+)$`)

// request is one request's caller and name.
type request struct {
	w      http.ResponseWriter
	r      *http.Request
	caller fenceapi.Caller
	name   imagerepos.Name
	// held is a push by the Docker facade (PushHeld), whose caller's
	// image record is its proof that it has the blobs.
	held bool
}

func (q *request) ctx() context.Context { return q.r.Context() }

// ServeHTTP serves /v2/ for the caller in the request's context.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	c, ok := fenceapi.CallerFromContext(r.Context())
	if !ok {
		writeErr(w, r, rerr(http.StatusForbidden, codeDenied, "silo: the call carries no caller"))
		return
	}
	// The caller is its origin; Authorization, if a client sends one,
	// means nothing for a local repo (call L66).
	r.Header.Del("Authorization")
	c.Manage = ""
	s.sweep(r.Context())
	path := r.URL.Path
	switch {
	case path == "/v2" || path == "/v2/":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeErr(w, r, rerr(http.StatusMethodNotAllowed, codeUnsupported, "silo: %s /v2/", r.Method))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{}"))
		return
	case path == "/v2/_catalog":
		s.catalog(w, r, c)
		return
	}
	m := routeRE.FindStringSubmatch(path)
	if m == nil {
		writeErr(w, r, rerr(http.StatusNotFound, codeNameUnknown, "silo: no such registry path %s", path))
		return
	}
	n, ok := imagerepos.ParseName(m[1], r.URL.Query().Get("ns"))
	if !ok {
		writeErr(w, r, rerr(http.StatusNotFound, codeNameInvalid, "silo: invalid repository name %q", m[1]))
		return
	}
	q := &request{w: w, r: r, caller: c, name: n}
	if !n.Local() {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeErr(w, r, rerr(http.StatusMethodNotAllowed, codeUnsupported,
				"silo: the cache is read-only; push to %s/<silo path>/<repo>", imagerepos.Host))
			return
		}
		writeErr(w, r, rerr(http.StatusNotFound, codeNameUnknown, "silo: this host's pull-through cache is not served yet"))
		return
	}
	rest := m[2]
	switch {
	case strings.HasPrefix(rest, "manifests/"):
		ref := strings.TrimPrefix(rest, "manifests/")
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			s.getManifest(q, ref)
		case http.MethodPut:
			s.putManifest(q, ref)
		case http.MethodDelete:
			s.deleteManifest(q, ref)
		default:
			s.notAllowed(q)
		}
	case strings.HasPrefix(rest, "blobs/uploads"):
		id := strings.Trim(strings.TrimPrefix(rest, "blobs/uploads"), "/")
		switch {
		case id == "" && r.Method == http.MethodPost:
			s.startUpload(q)
		case id != "" && r.Method == http.MethodPatch:
			s.patchUpload(q, id)
		case id != "" && r.Method == http.MethodPut:
			s.putUpload(q, id)
		case id != "" && r.Method == http.MethodGet:
			s.uploadStatus(q, id)
		case id != "" && r.Method == http.MethodDelete:
			s.cancelUpload(q, id)
		default:
			s.notAllowed(q)
		}
	case strings.HasPrefix(rest, "blobs/"):
		d := strings.TrimPrefix(rest, "blobs/")
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			s.getBlob(q, d)
		case http.MethodDelete:
			if _, err := s.read(q); err != nil {
				writeErr(w, r, err)
				return
			}
			writeErr(w, r, rerr(http.StatusMethodNotAllowed, codeUnsupported,
				"silo: blobs are removed by garbage collection once no manifest references them; delete the manifest"))
		default:
			s.notAllowed(q)
		}
	case rest == "tags/list":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			s.notAllowed(q)
			return
		}
		s.tags(q)
	default:
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			s.notAllowed(q)
			return
		}
		s.referrers(q, strings.TrimPrefix(rest, "referrers/"))
	}
}

// notAllowed answers a method the path doesn't take, after the access
// check, so that it says nothing of a repo the caller can't read.
func (s *Service) notAllowed(q *request) {
	if _, err := s.read(q); err != nil {
		writeErr(q.w, q.r, err)
		return
	}
	writeErr(q.w, q.r, rerr(http.StatusMethodNotAllowed, codeUnsupported, "silo: %s is not served on %s", q.r.Method, q.r.URL.Path))
}

// access is RegistryAccess inside tx, with a refusal as the registry
// answers it: everything not found is NAME_UNKNOWN.
func (s *Service) access(ctx context.Context, tx store.Tx, q *request, write bool) (*fenceapi.RegistryTarget, error) {
	t, err := s.opts.API.RegistryAccess(ctx, tx, q.caller, q.name, write)
	var fe *fenceapi.Error
	if errors.As(err, &fe) && fe.Code == fenceapi.NotFound {
		return nil, &denied{reason: "access", regError: nameUnknown()}
	}
	return t, err
}

// denied is a refusal registry.denied records, with its reason.
type denied struct {
	reason string
	*regError
}

func (d *denied) Unwrap() error { return d.regError }

// read resolves the request's repo for a pull, in a transaction of its
// own, auditing a refusal.
func (s *Service) read(q *request) (*fenceapi.RegistryTarget, error) {
	var t *fenceapi.RegistryTarget
	err := s.opts.Store.Tx(q.ctx(), func(tx store.Tx) error {
		var err error
		t, err = s.access(q.ctx(), tx, q, false)
		return err
	})
	return t, s.refused(q, err)
}

// refused audits err as registry.denied, in a transaction of its own,
// if it is a refusal, under the caller's own silo: a repo it can't read
// is never named under that repo's silo. It returns err.
func (s *Service) refused(q *request, err error) error {
	var d *denied
	if !errors.As(err, &d) {
		return err
	}
	if aerr := s.opts.Store.Tx(context.WithoutCancel(q.ctx()), func(tx store.Tx) error {
		t, err := tree.Get(q.ctx(), tx, q.caller.Silo)
		if err != nil {
			return err
		}
		return fenceapi.Audit(q.ctx(), tx, t, q.caller, fenceapi.KindRegistryDenied, "", d.code,
			map[string]any{"repo": q.name.String(), "method": q.r.Method, "path": q.r.URL.Path, "reason": d.reason, "message": d.message})
	}); aerr != nil {
		log.Printf("registry: auditing a refusal of %s %s: %v", q.r.Method, q.r.URL.Path, aerr)
	}
	return err
}

// audit records a pull or a push under the repo's silo.
func audit(ctx context.Context, tx store.Tx, t *fenceapi.RegistryTarget, kind string, fields map[string]any) error {
	fields["repo"] = t.Name
	if t.Repo != nil {
		fields["id"] = t.Repo.ID
	}
	if t.Import != "" {
		fields["import"] = t.Import
	}
	if t.Caller.FromToolVM != "" && t.Caller.Agent == "" {
		fields["agent"] = t.Caller.FromToolVM
	}
	return fenceapi.Audit(ctx, tx, t.Silo(), t.Caller, kind, "", "OK", fields)
}

// blob is the one blob-access function (call L62): the stored size of
// digest if repo may answer for it, because one of its manifests is it
// or references it, or it is an upload of the repo still in its grace
// hour. Anything else is ok false, whether the bytes are on disk for
// someone else or not.
func (s *Service) blob(ctx context.Context, tx store.Tx, repo *imagerepos.Repo, digest string) (int64, bool, error) {
	if repo == nil || !imagerepos.ValidDigest(digest) {
		return 0, false, nil
	}
	s.mu.Lock()
	g, ok := s.grace[repo.ID][digest]
	s.mu.Unlock()
	if ok && s.opts.Now().Before(g.until) {
		return g.size, true, nil
	}
	ms, err := imagerepos.Manifests(ctx, tx, repo.ID)
	if err != nil {
		return 0, false, err
	}
	for _, m := range ms {
		if m.Digest == digest {
			return m.Size, true, nil
		}
		for _, r := range m.Refs {
			if r.Digest == digest {
				return r.Size, true, nil
			}
		}
	}
	return 0, false, nil
}

// getBlob is GET and HEAD /v2/<name>/blobs/<digest>.
func (s *Service) getBlob(q *request, digest string) {
	ctx := q.ctx()
	var size int64
	err := s.opts.Store.Tx(ctx, func(tx store.Tx) error {
		t, err := s.access(ctx, tx, q, false)
		if err != nil {
			return err
		}
		n, ok, err := s.blob(ctx, tx, t.Repo, digest)
		if err != nil {
			return err
		}
		if !ok {
			return rerr(http.StatusNotFound, codeBlobUnknown, "blob unknown to registry")
		}
		size = n
		return audit(ctx, tx, t, fenceapi.KindRegistryPull, map[string]any{"reference": digest, "digest": digest,
			"bytes": n, "status": http.StatusOK, "method": q.r.Method})
	})
	if err = s.refused(q, err); err != nil {
		writeErr(q.w, q.r, err)
		return
	}
	ra, n, closer, err := s.opts.Content.Open(ctx, digest)
	if errors.Is(err, ErrUnknown) {
		writeErr(q.w, q.r, rerr(http.StatusNotFound, codeBlobUnknown, "silo: the blob's content is missing"))
		return
	}
	if err != nil {
		writeErr(q.w, q.r, err)
		return
	}
	defer closer.Close()
	if n != size {
		log.Printf("registry: blob %s is %d bytes on disk, recorded as %d", digest, n, size)
	}
	q.w.Header().Set("Docker-Content-Digest", digest)
	q.w.Header().Set("Content-Type", "application/octet-stream")
	q.w.Header().Set("Etag", `"`+digest+`"`)
	serveContent(q.w, q.r, ra, n)
}

// tags is GET /v2/<name>/tags/list, with n and last.
func (s *Service) tags(q *request) {
	ctx := q.ctx()
	var tags []string
	err := s.opts.Store.Tx(ctx, func(tx store.Tx) error {
		tags = nil
		t, err := s.access(ctx, tx, q, false)
		if err != nil {
			return err
		}
		ts, err := imagerepos.Tags(ctx, tx, t.Repo.ID)
		if err != nil {
			return err
		}
		for _, tg := range ts {
			tags = append(tags, tg.Tag)
		}
		return nil
	})
	if err = s.refused(q, err); err != nil {
		writeErr(q.w, q.r, err)
		return
	}
	page, link, err := paginate(q.r, tags)
	if err != nil {
		writeErr(q.w, q.r, err)
		return
	}
	if link != "" {
		q.w.Header().Set("Link", link)
	}
	writeJSON(q.w, q.r, map[string]any{"name": q.name.String(), "tags": page})
}

// catalog is GET /v2/_catalog: what the caller may read, by path name.
func (s *Service) catalog(w http.ResponseWriter, r *http.Request, c fenceapi.Caller) {
	var names []string
	err := s.opts.Store.Tx(r.Context(), func(tx store.Tx) error {
		var err error
		names, err = s.opts.API.RegistryCatalog(r.Context(), tx, c)
		return err
	})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	page, link, err := paginate(r, names)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if link != "" {
		w.Header().Set("Link", link)
	}
	writeJSON(w, r, map[string]any{"repositories": page})
}

// paginate cuts a sorted list at the request's last and n, and returns
// the Link to the next page, if there is one.
func paginate(r *http.Request, all []string) ([]string, string, error) {
	out := []string{}
	last := r.URL.Query().Get("last")
	for _, v := range all {
		if last == "" || v > last {
			out = append(out, v)
		}
	}
	nq := r.URL.Query().Get("n")
	if nq == "" {
		return out, "", nil
	}
	n, err := strconv.Atoi(nq)
	if err != nil || n < 0 {
		return nil, "", rerr(http.StatusBadRequest, codeUnsupported, "silo: n %q: want a count", nq)
	}
	if n >= len(out) {
		return out, "", nil
	}
	out = out[:n]
	link := ""
	if n > 0 {
		u := *r.URL
		v := u.Query()
		v.Set("last", out[n-1])
		u.RawQuery = v.Encode()
		link = fmt.Sprintf(`<%s>; rel="next"`, u.RequestURI())
	}
	return out, link, nil
}

func writeJSON(w http.ResponseWriter, r *http.Request, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	if r.Method == http.MethodHead {
		return
	}
	w.Write(b)
}
