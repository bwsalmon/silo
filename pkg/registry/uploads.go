package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/ids"
	"github.com/bwsalmon/silo/pkg/imagerepos"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// upload is one upload session: per repo and per caller silo, with a
// containerd lease of its own (local-registry.md, "Uploads").
type upload struct {
	id     string
	repo   string // the repo's id
	silo   string // the caller's silo
	lease  string
	ingest Ingest

	mu sync.Mutex // one request at a time
	// size is the bytes it holds, charged to the repo; last is when it
	// was last used. Both under Service.mu.
	size int64
	last int64
}

func leaseOf(id string) string { return "silo-registry-upload-" + id }

// microGiB is n bytes in millionths of a GiB, the ledger's unit of
// disk_gb, rounded up.
func microGiB(n int64) int64 {
	return n/(1<<30)*ledger.Micro + (n%(1<<30)*ledger.Micro+(1<<30)-1)/(1<<30)
}

// charge brings the charge t's repo holds on its silo's disk_gb to what
// it holds now (call L61): the distinct blobs its manifests reference,
// its grace-hour uploads, and its uploads in progress. A rise its silo
// has no room for is 413 DENIED. It returns the change, in bytes as
// the ledger counts them.
func (s *Service) charge(ctx context.Context, tx store.Tx, t *fenceapi.RegistryTarget) (int64, error) {
	ms, err := imagerepos.Manifests(ctx, tx, t.Repo.ID)
	if err != nil {
		return 0, err
	}
	want := microGiB(s.held(t.Repo.ID, ms))
	object := imageRepoChargeObject(t.Repo.ID)
	cs, err := ledger.Charges(ctx, tx, object)
	if err != nil {
		return 0, err
	}
	var have int64
	for _, c := range cs {
		if c.Key == DiskClass {
			have += c.Amount
		}
	}
	switch {
	case want > have:
		err := ledger.Charge(ctx, tx, ledger.ChargeRequest{Object: object, Silo: t.Silo().ID, Via: t.ChargeVia(),
			Amounts: []ledger.Amount{{Key: DiskClass, Amount: want - have}}})
		if errors.Is(err, ledger.ErrQuotaExceeded) {
			row, _ := ledger.Get(ctx, tx, t.Silo().ID, DiskClass)
			return 0, &denied{reason: "quota", regError: rerr(http.StatusRequestEntityTooLarge, codeDenied,
				"silo: %s is over its disk_gb grant (%.1f of %.0f GiB)", t.Silo().Path,
				float64(row.Used+row.Delegated+want-have)/ledger.Micro, float64(row.Granted)/ledger.Micro)}
		}
		if err != nil {
			return 0, err
		}
	case want < have:
		if _, err := ledger.Refund(ctx, tx, object, []ledger.Amount{{Key: DiskClass, Amount: have - want}}); err != nil {
			return 0, err
		}
	}
	return want - have, nil
}

// imageRepoChargeObject is the ledger object of a repo, the one its
// image_repos count is charged to, so that deleting the repo refunds
// its bytes with it.
func imageRepoChargeObject(id string) string { return "image_repo:" + id }

// held is the bytes repo holds: Charged over its manifests, its
// grace-hour blobs and its open uploads.
func (s *Service) held(repo string, ms []*imagerepos.Manifest) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	extra := &imagerepos.Manifest{}
	now := s.opts.Now()
	for d, g := range s.grace[repo] {
		if now.Before(g.until) {
			extra.Refs = append(extra.Refs, imagerepos.Blob{Digest: d, Size: g.size})
		}
	}
	n := imagerepos.Charged(append(ms, extra))
	for _, u := range s.uploads {
		if u.repo == repo {
			n += u.size
		}
	}
	return n
}

// write resolves the request's repo for a push inside tx, creating it
// if this is its first push (call L58) through fenceapi.CreateImageRepo,
// which charges image_repos and refuses a caller whose own control
// policy isn't full.
func (s *Service) write(ctx context.Context, tx store.Tx, q *request) (*fenceapi.RegistryTarget, error) {
	t, err := s.access(ctx, tx, q, true)
	if err != nil || t.Repo != nil {
		return t, err
	}
	r, err := fenceapi.CreateImageRepo(ctx, tx, t.Chain, t.Caller, t.Name)
	var fe *fenceapi.Error
	if errors.As(err, &fe) {
		switch fe.Code {
		case fenceapi.PermissionDenied:
			return nil, &denied{reason: "control", regError: rerr(http.StatusForbidden, codeDenied, "silo: %s", fe.Message)}
		case fenceapi.QuotaExceeded:
			return nil, &denied{reason: "quota", regError: rerr(http.StatusForbidden, codeDenied, "silo: %s", fe.Message)}
		}
	}
	if errors.Is(err, ledger.ErrQuotaExceeded) {
		return nil, &denied{reason: "quota", regError: rerr(http.StatusForbidden, codeDenied,
			"silo: %s has no image_repos left to create %s", t.Silo().Path, t.Name)}
	}
	if err != nil {
		return nil, err
	}
	t.Repo = r
	return t, nil
}

// push runs fn on the request's repo for a push in one transaction,
// auditing a refusal in one of its own.
func (s *Service) push(q *request, fn func(tx store.Tx, t *fenceapi.RegistryTarget) error) error {
	ctx := q.ctx()
	err := s.opts.Store.Tx(ctx, func(tx store.Tx) error {
		t, err := s.write(ctx, tx, q)
		if err != nil {
			return err
		}
		return fn(tx, t)
	})
	return s.refused(q, err)
}

func (s *Service) location(q *request, u *upload) string {
	return "/v2/" + q.name.String() + "/blobs/uploads/" + u.id
}

// startUpload is POST /v2/<name>/blobs/uploads/: a mount with
// mount and from, a monolithic upload with digest, or a new session.
func (s *Service) startUpload(q *request) {
	v := q.r.URL.Query()
	if d := v.Get("mount"); d != "" {
		if from := v.Get("from"); from != "" && s.mount(q, d, from) {
			return
		}
		// Not mountable, or no from: a session, as the spec allows,
		// never an answer from global content (call L62).
	}
	var u *upload
	err := s.push(q, func(tx store.Tx, t *fenceapi.RegistryTarget) error {
		var err error
		u, err = s.open(q, t)
		return err
	})
	if err != nil {
		writeErr(q.w, q.r, err)
		return
	}
	if d := v.Get("digest"); d != "" {
		s.finish(q, u, d)
		return
	}
	q.w.Header().Set("Location", s.location(q, u))
	q.w.Header().Set("Docker-Upload-UUID", u.id)
	q.w.Header().Set("Range", "0-0")
	q.w.Header().Set("Content-Length", "0")
	q.w.WriteHeader(http.StatusAccepted)
}

// open makes a session for the caller on t's repo, with its lease.
func (s *Service) open(q *request, t *fenceapi.RegistryTarget) (*upload, error) {
	s.mu.Lock()
	n := 0
	for _, u := range s.uploads {
		if u.silo == q.caller.Silo {
			n++
		}
	}
	s.mu.Unlock()
	if n >= s.opts.MaxSessions {
		return nil, &denied{reason: "sessions", regError: rerr(http.StatusTooManyRequests, codeTooMany,
			"silo: a silo may have %d uploads open at once", s.opts.MaxSessions)}
	}
	u := &upload{id: ids.New() + ids.New() + ids.New(), repo: t.Repo.ID, silo: q.caller.Silo}
	u.lease = leaseOf(u.id)
	ctx := context.WithoutCancel(q.ctx())
	if err := s.opts.Content.Lease(ctx, u.lease, s.opts.Now().Add(s.opts.Grace)); err != nil {
		return nil, err
	}
	in, err := s.opts.Content.Ingest(ctx, "silo-registry-"+u.id, u.lease)
	if err != nil {
		s.opts.Content.DeleteLease(ctx, u.lease)
		return nil, err
	}
	u.ingest = in
	s.mu.Lock()
	u.last = s.opts.Now().UnixNano()
	s.uploads[u.id] = u
	s.mu.Unlock()
	return u, nil
}

// session finds the caller's session id on the request's repo, after
// the access check: a session of another repo or another silo is
// unknown.
func (s *Service) session(tx store.Tx, q *request, t *fenceapi.RegistryTarget, id string) (*upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.uploads[id]
	if u == nil || u.repo != t.Repo.ID || u.silo != q.caller.Silo {
		return nil, rerr(http.StatusNotFound, codeUploadUnknown, "blob upload unknown to registry")
	}
	u.last = s.opts.Now().UnixNano()
	return u, nil
}

// lookup is session in a transaction of its own, for a write.
func (s *Service) lookup(q *request, id string) (*upload, error) {
	var u *upload
	err := s.push(q, func(tx store.Tx, t *fenceapi.RegistryTarget) error {
		var err error
		u, err = s.session(tx, q, t, id)
		return err
	})
	return u, err
}

// patchUpload is PATCH /v2/<name>/blobs/uploads/<id>: a chunk, or a
// streamed body.
func (s *Service) patchUpload(q *request, id string) {
	u, err := s.lookup(q, id)
	if err != nil {
		writeErr(q.w, q.r, err)
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := s.chunk(q, u); err != nil {
		writeErr(q.w, q.r, err)
		return
	}
	q.w.Header().Set("Location", s.location(q, u))
	q.w.Header().Set("Docker-Upload-UUID", u.id)
	q.w.Header().Set("Range", rangeOf(u.ingest.Offset()))
	q.w.Header().Set("Content-Length", "0")
	q.w.WriteHeader(http.StatusAccepted)
}

func rangeOf(n int64) string {
	if n == 0 {
		return "0-0"
	}
	return fmt.Sprintf("0-%d", n-1)
}

// chunk appends the request's body to u, at most MaxChunk bytes, and
// charges it. A chunk the silo has no disk_gb for is taken back and
// refused with 413 DENIED, so an upload never overshoots its grant
// (call L61).
func (s *Service) chunk(q *request, u *upload) error {
	start := u.ingest.Offset()
	if cr := q.r.Header.Get("Content-Range"); cr != "" {
		a, _, ok := strings.Cut(cr, "-")
		from, err := strconv.ParseInt(strings.TrimPrefix(a, "bytes="), 10, 64)
		if !ok || err != nil || from != start {
			q.w.Header().Set("Location", s.location(q, u))
			q.w.Header().Set("Range", rangeOf(start))
			return rerr(http.StatusRequestedRangeNotSatisfiable, codeUploadInvalid,
				"silo: the chunk starts at %q; the upload holds %d bytes", cr, start)
		}
	}
	n, err := io.Copy(u.ingest, io.LimitReader(q.r.Body, s.opts.MaxChunk+1))
	if err == nil && n > s.opts.MaxChunk {
		err = rerr(http.StatusRequestEntityTooLarge, codeSizeInvalid, "silo: a chunk is at most %d bytes", s.opts.MaxChunk)
	}
	if err != nil {
		if terr := u.ingest.Truncate(start); terr != nil {
			log.Printf("registry: taking back a chunk of upload %s: %v", u.id, terr)
		}
		var re *regError
		if !errors.As(err, &re) {
			err = rerr(http.StatusBadRequest, codeUploadInvalid, "silo: reading the chunk: %v", err)
		}
		return err
	}
	if n == 0 {
		return nil
	}
	s.mu.Lock()
	u.size = start + n
	s.mu.Unlock()
	err = s.push(q, func(tx store.Tx, t *fenceapi.RegistryTarget) error {
		_, err := s.charge(q.ctx(), tx, t)
		return err
	})
	if err != nil {
		s.mu.Lock()
		u.size = start
		s.mu.Unlock()
		if terr := u.ingest.Truncate(start); terr != nil {
			log.Printf("registry: taking back a chunk of upload %s: %v", u.id, terr)
		}
	}
	return err
}

// putUpload is PUT /v2/<name>/blobs/uploads/<id>?digest=: the last
// chunk, if any, and the commit.
func (s *Service) putUpload(q *request, id string) {
	u, err := s.lookup(q, id)
	if err != nil {
		writeErr(q.w, q.r, err)
		return
	}
	s.finish(q, u, q.r.URL.Query().Get("digest"))
}

// finish takes the request's body as u's last chunk and commits it as
// digest. The blob stays in the repo's grace hour until a manifest
// references it.
func (s *Service) finish(q *request, u *upload, digest string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !imagerepos.ValidDigest(digest) {
		writeErr(q.w, q.r, rerr(http.StatusBadRequest, codeDigestInvalid, "silo: digest %q: want sha256:<64 hex> or sha512:<128 hex>", digest))
		return
	}
	if err := s.chunk(q, u); err != nil {
		writeErr(q.w, q.r, err)
		return
	}
	ctx := context.WithoutCancel(q.ctx())
	size := u.ingest.Offset()
	if err := u.ingest.Commit(ctx, size, digest); errors.Is(err, ErrDigest) {
		writeErr(q.w, q.r, rerr(http.StatusBadRequest, codeDigestInvalid, "provided digest did not match uploaded content"))
		return
	} else if err != nil {
		writeErr(q.w, q.r, err)
		return
	}
	// The lease keeps the blob for the grace hour, then expires.
	if err := s.opts.Content.Lease(ctx, u.lease, s.opts.Now().Add(s.opts.Grace)); err != nil {
		log.Printf("registry: extending lease %s: %v", u.lease, err)
	}
	s.mu.Lock()
	delete(s.uploads, u.id)
	if s.grace[u.repo] == nil {
		s.grace[u.repo] = map[string]graceBlob{}
	}
	s.grace[u.repo][digest] = graceBlob{size: size, until: s.opts.Now().Add(s.opts.Grace)}
	s.mu.Unlock()
	err := s.push(q, func(tx store.Tx, t *fenceapi.RegistryTarget) error {
		delta, err := s.charge(ctx, tx, t)
		if err != nil {
			return err
		}
		return audit(ctx, tx, t, fenceapi.KindRegistryPush, map[string]any{"digest": digest, "bytes": size,
			"charged_delta": delta, "upload": u.id})
	})
	if err != nil {
		writeErr(q.w, q.r, err)
		return
	}
	q.w.Header().Set("Location", "/v2/"+q.name.String()+"/blobs/"+digest)
	q.w.Header().Set("Docker-Content-Digest", digest)
	q.w.Header().Set("Content-Length", "0")
	q.w.WriteHeader(http.StatusCreated)
}

// uploadStatus is GET /v2/<name>/blobs/uploads/<id>.
func (s *Service) uploadStatus(q *request, id string) {
	u, err := s.lookup(q, id)
	if err != nil {
		writeErr(q.w, q.r, err)
		return
	}
	q.w.Header().Set("Location", s.location(q, u))
	q.w.Header().Set("Docker-Upload-UUID", u.id)
	q.w.Header().Set("Range", rangeOf(u.ingest.Offset()))
	q.w.Header().Set("Content-Length", "0")
	q.w.WriteHeader(http.StatusNoContent)
}

// cancelUpload is DELETE /v2/<name>/blobs/uploads/<id>: its bytes are
// dropped and refunded.
func (s *Service) cancelUpload(q *request, id string) {
	u, err := s.lookup(q, id)
	if err != nil {
		writeErr(q.w, q.r, err)
		return
	}
	s.drop(context.WithoutCancel(q.ctx()), u)
	q.w.WriteHeader(http.StatusNoContent)
}

// drop ends a session: its ingest and lease go, and its repo's charge
// is brought down.
func (s *Service) drop(ctx context.Context, u *upload) {
	s.mu.Lock()
	delete(s.uploads, u.id)
	s.mu.Unlock()
	if err := s.opts.Content.Abort(ctx, "silo-registry-"+u.id); err != nil {
		log.Printf("registry: aborting upload %s: %v", u.id, err)
	}
	if err := s.opts.Content.DeleteLease(ctx, u.lease); err != nil {
		log.Printf("registry: ending lease %s: %v", u.lease, err)
	}
	s.recharge(ctx, u.repo)
}

// recharge brings repo's charge down after something it held went, as
// silod: no caller's access is checked.
func (s *Service) recharge(ctx context.Context, repo string) {
	err := s.opts.Store.Tx(ctx, func(tx store.Tx) error {
		r, err := imagerepos.Get(ctx, tx, repo)
		if errors.Is(err, imagerepos.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		chain, err := tree.Ancestry(ctx, tx, r.Silo)
		if err != nil {
			return err
		}
		t := &fenceapi.RegistryTarget{Chain: chain, Name: r.Name, Repo: r,
			Caller: fenceapi.Caller{Silo: r.Silo, Via: fenceapi.ViaInternal}}
		_, err = s.charge(ctx, tx, t)
		return err
	})
	if err != nil {
		log.Printf("registry: recharging repo %s: %v", repo, err)
	}
}

// sweep cancels sessions idle for Idle and drops grace-hour blobs past
// their hour, refunding what they held.
func (s *Service) sweep(ctx context.Context) {
	now := s.opts.Now()
	var idle []*upload
	repos := map[string]bool{}
	s.mu.Lock()
	for _, u := range s.uploads {
		if now.UnixNano()-u.last > int64(s.opts.Idle) {
			idle = append(idle, u)
		}
	}
	for repo, bs := range s.grace {
		for d, g := range bs {
			if !now.Before(g.until) {
				delete(bs, d)
				repos[repo] = true
			}
		}
		if len(bs) == 0 {
			delete(s.grace, repo)
		}
	}
	s.mu.Unlock()
	ctx = context.WithoutCancel(ctx)
	for _, u := range idle {
		if u.mu.TryLock() {
			s.drop(ctx, u)
			u.mu.Unlock()
		}
	}
	for repo := range repos {
		s.recharge(ctx, repo)
	}
}

// mount is a cross-repository mount of digest from the repo named
// from: done only if the caller may read from and from may answer for
// the blob (call L62). It reports whether it answered the request.
func (s *Service) mount(q *request, digest, from string) bool {
	fn, ok := imagerepos.ParseName(from, "")
	if !ok || !fn.Local() || !imagerepos.ValidDigest(digest) {
		return false
	}
	ctx := q.ctx()
	var size int64
	mounted := false
	err := s.push(q, func(tx store.Tx, t *fenceapi.RegistryTarget) error {
		mounted = false
		src, err := s.opts.API.RegistryAccess(ctx, tx, q.caller, fn, false)
		var fe *fenceapi.Error
		if errors.As(err, &fe) && fe.Code == fenceapi.NotFound {
			return nil
		}
		if err != nil {
			return err
		}
		n, ok, err := s.blob(ctx, tx, src.Repo, digest)
		if err != nil || !ok {
			return err
		}
		size, mounted = n, true
		s.mu.Lock()
		if s.grace[t.Repo.ID] == nil {
			s.grace[t.Repo.ID] = map[string]graceBlob{}
		}
		prev, had := s.grace[t.Repo.ID][digest]
		s.grace[t.Repo.ID][digest] = graceBlob{size: n, until: s.opts.Now().Add(s.opts.Grace)}
		s.mu.Unlock()
		delta, err := s.charge(ctx, tx, t)
		if err == nil {
			err = audit(ctx, tx, t, fenceapi.KindRegistryPush, map[string]any{"digest": digest, "bytes": n,
				"charged_delta": delta, "mounted_from": fn.String()})
		}
		if err != nil {
			s.mu.Lock()
			if had {
				s.grace[t.Repo.ID][digest] = prev
			} else {
				delete(s.grace[t.Repo.ID], digest)
			}
			s.mu.Unlock()
		}
		return err
	})
	if err != nil {
		writeErr(q.w, q.r, err)
		return true
	}
	if !mounted {
		return false
	}
	_ = size
	q.w.Header().Set("Location", "/v2/"+q.name.String()+"/blobs/"+digest)
	q.w.Header().Set("Docker-Content-Digest", digest)
	q.w.Header().Set("Content-Length", "0")
	q.w.WriteHeader(http.StatusCreated)
	return true
}
