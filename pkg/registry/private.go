package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/imagerepos"
	"github.com/bwsalmon/silo/pkg/store"
)

// Private upstream content (local-registry.md, §8 "Private upstream
// images", call L72).
//
// A cached object is public or private as the upstream last said when
// asked anonymously: a fetch that needed credentials is private, and
// an object classified longer ago than public_ttl is asked again, with
// an anonymous HEAD (registries such as Docker Hub don't count a HEAD
// as a pull). A private manifest is served only after the caller's own
// credentials revalidate it by digest on that request, through the
// egress proxy as the caller. Each revalidation that succeeds is an
// authorisation, (silo, upstream, repo, manifest digest), held in this
// process's memory for auth_ttl; a private blob is served only through
// an authorisation for a manifest of the same repo that references it,
// and an index's authorisation covers the manifests it lists.
//
// Credentials arrive per request (Basic Authorization on an upstream
// path, X-Registry-Auth through the Docker facade) and are dropped with
// it. Neither they nor any token the upstream gives for them is
// written to the store, the audit, a log or a cache entry, and private
// fetches are never shared in flight.

// Credentials are a caller's own for an upstream, for one request. They
// format as "[credentials]" whatever the verb, so that no error or log
// line can carry them by accident.
type Credentials struct {
	Username string
	Password string
}

// Format prints no secret.
func (c Credentials) Format(f fmt.State, _ rune) { io.WriteString(f, "[credentials]") }

// BasicCredentials are the Basic credentials of r's Authorization
// header, or nil.
func BasicCredentials(r *http.Request) *Credentials {
	u, p, ok := r.BasicAuth()
	if !ok || (u == "" && p == "") {
		return nil
	}
	return &Credentials{Username: u, Password: p}
}

// RegistryAuth are the credentials of a Docker X-Registry-Auth header
// (base64url JSON with username and password), or nil for none, for
// "{}" and for anything that doesn't decode. An identity token alone
// isn't used.
func RegistryAuth(h string) *Credentials {
	h = strings.TrimSpace(h)
	if h == "" {
		return nil
	}
	var b []byte
	for _, enc := range []*base64.Encoding{base64.URLEncoding, base64.RawURLEncoding, base64.StdEncoding, base64.RawStdEncoding} {
		if d, err := enc.DecodeString(h); err == nil {
			b = d
			break
		}
	}
	var a struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if b == nil || json.Unmarshal(b, &a) != nil || (a.Username == "" && a.Password == "") {
		return nil
	}
	return &Credentials{Username: a.Username, Password: a.Password}
}

// The classes revalidate reports for a tag: public when the anonymous
// HEAD answered, private when only the caller's credentials did.
const (
	classPublic  = "public"
	classPrivate = "private"
)

// challenge is the answer to a caller that presents no credentials for
// private content: 401, with a Basic challenge on an upstream path
// (writeErr), and Docker's "unauthorized" through the facade.
func (cr *cacheReq) challenge() error {
	return rerr(http.StatusUnauthorized, codeUnauthorized, "authentication required")
}

// needsCredentials is whether an anonymous fetch failed because the
// upstream wants credentials.
func needsCredentials(err error) bool {
	var ue *upstreamErr
	return errors.As(err, &ue) && (ue.status == http.StatusUnauthorized || ue.status == http.StatusForbidden)
}

// setClass records o's class, as of now.
func (s *Service) setClass(ctx context.Context, cr *cacheReq, o *cacheObject, private bool) error {
	o.private, o.classifiedAt = private, s.opts.Now()
	return s.opts.Store.Tx(ctx, func(tx store.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE registry_cache_objects SET private = ?, classified_at = ?
			WHERE upstream = ? AND repo = ? AND digest = ?`,
			boolInt(private), store.Millis(o.classifiedAt), cr.name.Upstream, cr.name.UpstreamRepo, o.digest)
		return err
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// classify is whether o is private: as class says if this request
// already asked the upstream about it, as recorded if that is less than
// public_ttl old, and otherwise as an anonymous HEAD answers now,
// without following a blob's redirect (an answer from the API host
// sending the client on is public). An upstream that can't be reached
// or gives another answer leaves the class as it was.
func (s *Service) classify(ctx context.Context, cr *cacheReq, o *cacheObject, kind, class string) (bool, error) {
	fresh := s.opts.Now().Sub(o.classifiedAt) < cr.cfg.PublicTTLDuration()
	switch class {
	case classPublic, classPrivate:
		if p := class == classPrivate; p != o.private || !fresh {
			return p, s.setClass(ctx, cr, o, p)
		}
		return o.private, nil
	}
	if fresh {
		return o.private, nil
	}
	f, err := s.fetcherFor(cr, nil)
	if err != nil {
		return o.private, nil
	}
	f.stopAtRedirect = true
	what := "manifests"
	if kind == kindBlob {
		what = "blobs"
	}
	resp, err := f.get(ctx, http.MethodHead, "/v2/"+cr.name.UpstreamRepo+"/"+what+"/"+o.digest, manifestAccept)
	private := o.private
	switch {
	case err != nil:
		var r *EgressRefusal
		if errors.As(err, &r) {
			return false, cr.refuse(r.Host)
		}
		if !needsCredentials(err) {
			return o.private, nil
		}
		private = true
	default:
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusOK || (resp.StatusCode >= 300 && resp.StatusCode < 400):
			private = false
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			private = true
		case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
			return o.private, nil
		}
	}
	return private, s.setClass(ctx, cr, o, private)
}

// revalidateDigest asks the upstream for the manifest digest with the
// caller's credentials (a HEAD, through the egress proxy as the
// caller). Anything but 200 is the upstream's answer, passed back.
func (s *Service) revalidateDigest(ctx context.Context, cr *cacheReq, digest string) error {
	f, err := s.fetcherFor(cr, cr.creds)
	if err != nil {
		return err
	}
	resp, err := f.get(ctx, http.MethodHead, "/v2/"+cr.name.UpstreamRepo+"/manifests/"+digest, manifestAccept)
	cr.auth = f.auth
	if err != nil {
		return cr.upstreamFailed(err)
	}
	if resp.StatusCode != http.StatusOK {
		return cr.upstreamFailed(statusErr(resp, "manifest "+digest))
	}
	resp.Body.Close()
	return nil
}

// authorisation is a revalidation that succeeded: what the manifest
// references (blobs, or an index's manifests), until when. It holds no
// credential.
type authorisation struct {
	until time.Time
	refs  map[string]bool
}

func (cr *cacheReq) authKey() string {
	return cr.caller.Silo + "\x00" + cr.name.Upstream + "\x00" + cr.name.UpstreamRepo
}

// grant records an authorisation for the caller's silo to manifest o
// of its repo, until until.
func (s *Service) grant(ctx context.Context, cr *cacheReq, o *cacheObject, until time.Time) {
	refs := map[string]bool{}
	if b, err := s.readContent(ctx, o.digest, s.opts.MaxManifest); err == nil {
		var m manifest
		if json.Unmarshal(b, &m) == nil {
			for _, d := range m.descriptors() {
				if imagerepos.ValidDigest(d.Digest) {
					refs[d.Digest] = true
				}
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := cr.authKey()
	if s.authz[k] == nil {
		s.authz[k] = map[string]authorisation{}
	}
	s.authz[k][o.digest] = authorisation{until: until, refs: refs}
}

// authorised is when the caller's silo's authorisation for a manifest
// of its repo that references digest ends, and whether it holds one.
// Expired authorisations are forgotten.
func (s *Service) authorised(cr *cacheReq, digest string) (time.Time, bool) {
	now := s.opts.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	k := cr.authKey()
	var until time.Time
	ok := false
	for d, a := range s.authz[k] {
		if !now.Before(a.until) {
			delete(s.authz[k], d)
			continue
		}
		if a.refs[digest] && a.until.After(until) {
			until, ok = a.until, true
		}
	}
	if len(s.authz[k]) == 0 {
		delete(s.authz, k)
	}
	return until, ok
}

// allowManifest decides whether the caller may have cached manifest o,
// which the upstream said was of class on this request ("" if it
// wasn't asked). Public content may be had by anyone its egress lets
// reach it. Private content needs a revalidation with the caller's own
// credentials, or, for a manifest an authorised index lists, that
// index's authorisation; either grants an authorisation for o.
func (s *Service) allowManifest(ctx context.Context, cr *cacheReq, o *cacheObject, class, validated string) error {
	private, err := s.classify(ctx, cr, o, kindManifest, class)
	if err != nil || !private {
		return err
	}
	cr.private = true
	if cr.creds == nil {
		if until, ok := s.authorised(cr, o.digest); ok {
			s.grant(ctx, cr, o, until)
			return nil
		}
		return cr.challenge()
	}
	if validated != o.digest {
		if err := s.revalidateDigest(ctx, cr, o.digest); err != nil {
			return err
		}
	}
	s.grant(ctx, cr, o, s.opts.Now().Add(cr.cfg.AuthTTLDuration()))
	return nil
}

// allowBlob decides whether the caller may have cached blob o: public
// content as for a manifest, private content only through an
// authorisation for a manifest of the same repo that references it. A
// blob the caller can't have is unknown, as a blob no manifest of the
// repo references is (call L62).
func (s *Service) allowBlob(ctx context.Context, cr *cacheReq, o *cacheObject) error {
	private, err := s.classify(ctx, cr, o, kindBlob, "")
	if err != nil || !private {
		return err
	}
	cr.private = true
	if _, ok := s.authorised(cr, o.digest); !ok {
		return blobUnknown()
	}
	return nil
}

func blobUnknown() *regError {
	return rerr(http.StatusNotFound, codeBlobUnknown, "blob unknown to registry")
}
