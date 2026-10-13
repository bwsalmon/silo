// Package containerdstore is registry.Content over silo-containerd's
// content, leases and images APIs (docs/silo/local-registry.md, §4 "One
// content store, records per repo", call L59, and §10), the containerd
// that silo-nsd runs with the data directory's containerd/ as its root
// (local-backend.md §15, step 16).
//
// containerd's content is per namespace: a blob is on disk once, but
// each namespace has its own record of it, and GC keeps a blob while
// any namespace's roots reach it. So:
//
//   - Uploads and fetches are ingested in BlobNamespace (silo-registry),
//     under their leases, which live there too.
//   - An image record of another namespace (silo-cache's entries) has
//     its blobs linked into that namespace before the record is made:
//     an ingest that names the digest and size, which containerd's
//     shared content policy commits without the bytes, since they are
//     on disk.
//   - Size and Open answer for a blob in any namespace, BlobNamespace
//     and silo-cache first: the registry's own access checks decide who
//     may have it (call L62), never containerd's.
package containerdstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/bwsalmon/silo/pkg/registry"
)

// BlobNamespace is the namespace uploads, fetches and their leases are
// in: the registry's own.
const BlobNamespace = registry.Namespace

// The labels containerd's GC reads (containerd docs/garbage-collection.md).
const (
	// LabelExpire ends a lease at an RFC 3339 time.
	LabelExpire = "containerd.io/gc.expire"
	// LabelRefContent, with a suffix, is a reference from a blob to
	// another, by digest: a manifest's to its config, layers and child
	// manifests.
	LabelRefContent = "containerd.io/gc.ref.content"
)

// NoMediaType is the target media type of a record whose Image has
// none, such as a cached blob's: containerd refuses an empty one.
// Listing gives it back as "".
const NoMediaType = "application/vnd.dev.silo.none"

// DefaultSpoolLimit is Options.SpoolLimit's default: twice the
// registry's largest chunk.
const DefaultSpoolLimit = 2 * registry.DefaultMaxChunk

// Options configure a Store.
type Options struct {
	// Address is silo-containerd's socket (rootless.Paths.ContainerdSocket).
	Address string
	// Spool is the directory an ingest keeps the bytes it may still
	// have to take back (Ingest.Truncate), os.TempDir() if empty. Its
	// files are unlinked as they are made.
	Spool string
	// SpoolLimit is how many bytes an ingest keeps back before it
	// writes them to containerd anyway; DefaultSpoolLimit if zero. It
	// must be more than the registry's MaxChunk, or a refused chunk
	// can't be taken back.
	SpoolLimit int64
	// Now is the clock leases' expiry is written by; time.Now if nil.
	Now func() time.Time
}

// Store is registry.Content over silo-containerd.
type Store struct {
	opts   Options
	client *containerd.Client
	cs     content.Store
	ls     leases.Manager
	is     images.Store
	// leaseMu is held while a lease is replaced to extend it.
	leaseMu sync.Mutex
}

var _ registry.Content = (*Store)(nil)

// New dials silo-containerd. It doesn't wait for it to answer.
func New(opts Options) (*Store, error) {
	if opts.Address == "" {
		return nil, errors.New("containerdstore: no containerd address")
	}
	if opts.SpoolLimit == 0 {
		opts.SpoolLimit = DefaultSpoolLimit
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	c, err := containerd.New(opts.Address, containerd.WithDefaultNamespace(BlobNamespace), containerd.WithTimeout(10*time.Second))
	if err != nil {
		return nil, fmt.Errorf("containerdstore: silo-containerd: %w", err)
	}
	return &Store{opts: opts, client: c, cs: c.ContentStore(), ls: c.LeasesService(), is: c.ImageService()}, nil
}

// Close closes the connection.
func (s *Store) Close() error { return s.client.Close() }

// Ping says whether silo-containerd answers.
func (s *Store) Ping(ctx context.Context) error {
	_, err := s.client.Version(ns(ctx, BlobNamespace))
	return err
}

func ns(ctx context.Context, name string) context.Context {
	return namespaces.WithNamespace(ctx, name)
}

// leaseID is id as a containerd identifier: letters, digits, '_', '.'
// and '-', at most 76 characters. The registry's and the facade's ids
// are already; anything else is hashed.
func leaseID(id string) string {
	ok := id != "" && len(id) <= 76
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-') {
			ok = false
			break
		}
	}
	if ok {
		return id
	}
	h := sha256.Sum256([]byte(id))
	return "silo-lease-" + hex.EncodeToString(h[:16])
}

func randomID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func unknown(err error) error {
	if errdefs.IsNotFound(err) {
		return fmt.Errorf("%w: %v", registry.ErrUnknown, err)
	}
	return err
}

// find is digest's record in the first namespace that has one:
// BlobNamespace, silo-cache, then any other.
func (s *Store) find(ctx context.Context, dgst string) (string, content.Info, error) {
	d, err := digest.Parse(dgst)
	if err != nil {
		return "", content.Info{}, fmt.Errorf("%w: %v", registry.ErrUnknown, err)
	}
	tried := map[string]bool{}
	try := func(n string) (content.Info, bool, error) {
		tried[n] = true
		info, err := s.cs.Info(ns(ctx, n), d)
		if errdefs.IsNotFound(err) {
			return info, false, nil
		}
		return info, err == nil, err
	}
	for _, n := range []string{BlobNamespace, registry.CacheNamespace} {
		if info, ok, err := try(n); ok || err != nil {
			return n, info, err
		}
	}
	all, err := s.client.NamespaceService().List(ctx)
	if err != nil {
		return "", content.Info{}, err
	}
	for _, n := range all {
		if tried[n] {
			continue
		}
		if info, ok, err := try(n); ok || err != nil {
			return n, info, err
		}
	}
	return "", content.Info{}, registry.ErrUnknown
}

func (s *Store) Size(ctx context.Context, dgst string) (int64, error) {
	_, info, err := s.find(ctx, dgst)
	if err != nil {
		return 0, err
	}
	return info.Size, nil
}

func (s *Store) Open(ctx context.Context, dgst string) (io.ReaderAt, int64, io.Closer, error) {
	n, info, err := s.find(ctx, dgst)
	if err != nil {
		return nil, 0, nil, err
	}
	if rl, ok := registry.ReadLeaseOf(ctx).(*readLease); ok {
		if err := rl.hold(ctx, n, info.Digest); err != nil {
			return nil, 0, nil, err
		}
	}
	ra, err := s.cs.ReaderAt(ns(ctx, n), ocispec.Descriptor{Digest: info.Digest, Size: info.Size})
	if err != nil {
		return nil, 0, nil, unknown(err)
	}
	return ra, ra.Size(), ra, nil
}

// readLease is a request's leases, one in each namespace it opened a
// blob in, made at the first.
type readLease struct {
	s  *Store
	mu sync.Mutex
	id string
	// made are the namespaces the lease has been made in.
	made []string
}

func (s *Store) ReadLease(ctx context.Context) (context.Context, func(), error) {
	rl := &readLease{s: s, id: "silo-registry-read-" + randomID()}
	return registry.WithReadLease(ctx, rl), rl.done, nil
}

// hold adds dgst, in namespace n, to the request's lease there.
func (rl *readLease) hold(ctx context.Context, n string, dgst digest.Digest) error {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	nctx := ns(ctx, n)
	l := leases.Lease{ID: rl.id}
	if !slices.Contains(rl.made, n) {
		expire := rl.s.opts.Now().Add(registry.ReadLeaseTTL).UTC().Format(time.RFC3339)
		if _, err := rl.s.ls.Create(nctx, leases.WithID(rl.id), leases.WithLabel(LabelExpire, expire)); err != nil && !errdefs.IsAlreadyExists(err) {
			return err
		}
		rl.made = append(rl.made, n)
	}
	return rl.s.ls.AddResource(nctx, l, leases.Resource{ID: dgst.String(), Type: "content"})
}

func (rl *readLease) done() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	for _, n := range rl.made {
		// One that can't be deleted expires on its own.
		rl.s.ls.Delete(ns(context.Background(), n), leases.Lease{ID: rl.id})
	}
	rl.made = nil
}

func (s *Store) Lease(ctx context.Context, id string, expire time.Time) error {
	ctx = ns(ctx, BlobNamespace)
	id = leaseID(id)
	label := leases.WithLabel(LabelExpire, expire.UTC().Format(time.RFC3339))
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	_, err := s.ls.Create(ctx, leases.WithID(id), label)
	if !errdefs.IsAlreadyExists(err) {
		return err
	}
	// containerd can't change a lease's labels, so extending one
	// replaces it: a guard lease holds what it holds while it is
	// deleted and made again.
	res, err := s.ls.ListResources(ctx, leases.Lease{ID: id})
	if err != nil {
		return err
	}
	guard, err := s.ls.Create(ctx, leases.WithRandomID(),
		leases.WithLabel(LabelExpire, s.opts.Now().Add(10*time.Minute).UTC().Format(time.RFC3339)))
	if err != nil {
		return err
	}
	defer s.ls.Delete(context.WithoutCancel(ctx), guard)
	for _, r := range res {
		if err := s.ls.AddResource(ctx, guard, r); err != nil {
			return err
		}
	}
	if err := s.ls.Delete(ctx, leases.Lease{ID: id}); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	l, err := s.ls.Create(ctx, leases.WithID(id), label)
	if err != nil {
		return err
	}
	for _, r := range res {
		if err := s.ls.AddResource(ctx, l, r); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) DeleteLease(ctx context.Context, id string) error {
	err := s.ls.Delete(ns(ctx, BlobNamespace), leases.Lease{ID: leaseID(id)})
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (s *Store) Abort(ctx context.Context, ref string) error {
	err := s.cs.Abort(ns(ctx, BlobNamespace), ref)
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// Collect has containerd collect now: deleting a lease synchronously
// runs its GC, which is global, before the delete returns.
func (s *Store) Collect(ctx context.Context) error {
	ctx = ns(ctx, BlobNamespace)
	l, err := s.ls.Create(ctx, leases.WithRandomID(),
		leases.WithLabel(LabelExpire, s.opts.Now().Add(time.Minute).UTC().Format(time.RFC3339)))
	if err != nil {
		return err
	}
	return s.ls.Delete(ctx, l, leases.SynchronousDelete)
}

func (s *Store) PutImage(ctx context.Context, n string, img registry.Image) error {
	target, err := digest.Parse(img.Digest)
	if err != nil {
		return fmt.Errorf("containerdstore: image %s: %w", img.Name, err)
	}
	nctx := ns(ctx, n)
	// A lease of the image's own holds what is linked into n until the
	// record does.
	l, err := s.ls.Create(nctx, leases.WithRandomID(),
		leases.WithLabel(LabelExpire, s.opts.Now().Add(10*time.Minute).UTC().Format(time.RFC3339)))
	if err != nil {
		return err
	}
	defer s.ls.Delete(context.WithoutCancel(nctx), l)
	lctx := leases.WithLease(nctx, l.ID)
	seen := map[string]bool{}
	for _, d := range append([]string{img.Digest}, img.Refs...) {
		if seen[d] {
			continue
		}
		seen[d] = true
		if err := s.link(lctx, n, l, d); err != nil {
			return err
		}
	}
	if len(img.Refs) > 0 {
		info := content.Info{Digest: target, Labels: map[string]string{}}
		var paths []string
		for i, r := range img.Refs {
			k := LabelRefContent + "." + strconv.Itoa(i)
			info.Labels[k] = r
			paths = append(paths, "labels."+k)
		}
		if _, err := s.cs.Update(nctx, info, paths...); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("containerdstore: labelling %s's references: %w", img.Digest, err)
		}
	}
	mt := img.MediaType
	if mt == "" {
		mt = NoMediaType
	}
	rec := images.Image{Name: img.Name, Labels: img.Labels,
		Target: ocispec.Descriptor{MediaType: mt, Digest: target, Size: img.Size}}
	if _, err := s.is.Create(nctx, rec); errdefs.IsAlreadyExists(err) {
		_, err = s.is.Update(nctx, rec)
		return err
	} else {
		return err
	}
}

// link makes dgst's record in namespace n, under lease l in ctx: a
// blob already on disk is committed there by digest and size, without
// its bytes. A blob that is nowhere is left out: GC has nothing of it to
// keep.
func (s *Store) link(ctx context.Context, n string, l leases.Lease, dgst string) error {
	d, err := digest.Parse(dgst)
	if err != nil {
		return err
	}
	if _, err := s.cs.Info(ctx, d); err == nil {
		return s.ls.AddResource(ctx, l, leases.Resource{ID: d.String(), Type: "content"})
	} else if !errdefs.IsNotFound(err) {
		return err
	}
	_, info, err := s.find(ctx, dgst)
	if errors.Is(err, registry.ErrUnknown) {
		return nil
	} else if err != nil {
		return err
	}
	w, err := content.OpenWriter(ctx, s.cs, content.WithRef("silo-link-"+n+"-"+d.Encoded()),
		content.WithDescriptor(ocispec.Descriptor{Digest: d, Size: info.Size}))
	if errdefs.IsAlreadyExists(err) {
		return nil
	} else if err != nil {
		return err
	}
	defer w.Close()
	if err := w.Commit(ctx, info.Size, d); err != nil && !errdefs.IsAlreadyExists(err) {
		return fmt.Errorf("containerdstore: linking %s into %s: %w", dgst, n, err)
	}
	return nil
}

func (s *Store) DeleteImage(ctx context.Context, n, name string) error {
	err := s.is.Delete(ns(ctx, n), name)
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (s *Store) ListImages(ctx context.Context, n string) ([]registry.Image, error) {
	nctx := ns(ctx, n)
	recs, err := s.is.List(nctx)
	if err != nil {
		return nil, err
	}
	out := make([]registry.Image, 0, len(recs))
	for _, r := range recs {
		img := registry.Image{Name: r.Name, Digest: r.Target.Digest.String(), MediaType: r.Target.MediaType,
			Size: r.Target.Size, Labels: r.Labels}
		if img.MediaType == NoMediaType {
			img.MediaType = ""
		}
		if info, err := s.cs.Info(nctx, r.Target.Digest); err == nil {
			img.Refs = refsOf(info.Labels)
		} else if !errdefs.IsNotFound(err) {
			return nil, err
		}
		out = append(out, img)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// refsOf is the digests a blob's LabelRefContent labels name, in the
// order of their suffixes.
func refsOf(labels map[string]string) []string {
	type ref struct {
		i int
		d string
	}
	var rs []ref
	for k, v := range labels {
		suffix, ok := strings.CutPrefix(k, LabelRefContent+".")
		if !ok {
			continue
		}
		i, err := strconv.Atoi(suffix)
		if err != nil {
			i = 1 << 30
		}
		rs = append(rs, ref{i, v})
	}
	sort.Slice(rs, func(a, b int) bool { return rs[a].i < rs[b].i || rs[a].i == rs[b].i && rs[a].d < rs[b].d })
	var out []string
	for _, r := range rs {
		out = append(out, r.d)
	}
	return out
}

func (s *Store) Namespaces(ctx context.Context) ([]string, error) {
	all, err := s.client.NamespaceService().List(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range all {
		recs, err := s.is.List(ns(ctx, n))
		if err != nil {
			return nil, err
		}
		if len(recs) > 0 {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (s *Store) Ingest(ctx context.Context, ref, lease string) (registry.Ingest, error) {
	ctx = ns(ctx, BlobNamespace)
	if lease != "" {
		ctx = leases.WithLease(ctx, leaseID(lease))
	}
	in := &ingest{s: s, ctx: ctx, ref: ref}
	if err := in.open(); err != nil {
		return nil, err
	}
	return in, nil
}

// ingest is a containerd writer with a spool in front of it: what was
// written since the last Offset (or Digest) is kept in the spool, so
// that Truncate can take it back, which containerd's writers can do
// only to nothing.
type ingest struct {
	s   *Store
	ctx context.Context
	ref string
	w   content.Writer
	// flushed is what containerd holds; local hashes it, from an ingest
	// opened empty, for Digest's sha512.
	flushed int64
	local   hash.Hash
	spool   *os.File
	spooled int64
	err     error
}

func (in *ingest) open() error {
	w, err := content.OpenWriter(in.ctx, in.s.cs, content.WithRef(in.ref))
	if err != nil {
		return err
	}
	st, err := w.Status()
	if err != nil {
		w.Close()
		return err
	}
	in.w, in.flushed, in.local = w, st.Offset, nil
	if st.Offset == 0 {
		in.local = sha512.New()
	}
	return nil
}

func (in *ingest) Write(p []byte) (int, error) {
	if in.err != nil {
		return 0, in.err
	}
	if in.spool == nil {
		f, err := os.CreateTemp(in.s.opts.Spool, "silo-ingest-")
		if err != nil {
			return 0, err
		}
		os.Remove(f.Name())
		in.spool = f
	}
	n, err := in.spool.WriteAt(p, in.spooled)
	in.spooled += int64(n)
	if err == nil && in.spooled > in.s.opts.SpoolLimit {
		err = in.flush()
	}
	return n, err
}

// flush writes the spool to containerd.
func (in *ingest) flush() error {
	if in.err != nil || in.spooled == 0 {
		return in.err
	}
	var dst io.Writer = in.w
	if in.local != nil {
		dst = io.MultiWriter(in.w, in.local)
	}
	if _, err := io.Copy(dst, io.NewSectionReader(in.spool, 0, in.spooled)); err != nil {
		in.err = fmt.Errorf("containerdstore: writing ingest %s: %w", in.ref, err)
		return in.err
	}
	in.flushed += in.spooled
	in.spooled = 0
	return in.spool.Truncate(0)
}

func (in *ingest) Offset() int64 {
	in.flush()
	return in.flushed + in.spooled
}

func (in *ingest) Truncate(size int64) error {
	switch {
	case size < 0 || size > in.flushed+in.spooled:
		return fmt.Errorf("containerdstore: truncate to %d of %d bytes", size, in.flushed+in.spooled)
	case size >= in.flushed:
		in.spooled = size - in.flushed
		if in.spool == nil {
			return nil
		}
		return in.spool.Truncate(in.spooled)
	case size == 0:
		// Afresh: containerd's own Truncate would do, but only through
		// a write that follows it.
		in.w.Close()
		if err := in.s.cs.Abort(in.ctx, in.ref); err != nil && !errdefs.IsNotFound(err) {
			return err
		}
		in.spooled, in.err = 0, nil
		if in.spool != nil {
			in.spool.Truncate(0)
		}
		return in.open()
	}
	return fmt.Errorf("containerdstore: can't take back bytes %d to %d of ingest %s: containerd has them", size, in.flushed, in.ref)
}

func (in *ingest) Digest(alg string) string {
	if in.flush() != nil {
		return ""
	}
	if alg == "sha512" {
		if in.local == nil {
			return ""
		}
		return "sha512:" + hex.EncodeToString(in.local.Sum(nil))
	}
	return in.w.Digest().String()
}

func (in *ingest) Commit(ctx context.Context, size int64, dgst string) error {
	if err := in.flush(); err != nil {
		return err
	}
	d, err := digest.Parse(dgst)
	if err != nil || d.Algorithm() != digest.SHA256 {
		// containerd's content store verifies sha256 only.
		return fmt.Errorf("%w: silo-containerd stores sha256 content only, not %q", registry.ErrDigest, dgst)
	}
	if in.flushed != size {
		return fmt.Errorf("%w: %d bytes, not %d", registry.ErrDigest, in.flushed, size)
	}
	// The commit is under the ingest's namespace and lease, whatever
	// ctx carries.
	cctx := ns(ctx, BlobNamespace)
	if l, ok := leases.FromContext(in.ctx); ok {
		cctx = leases.WithLease(cctx, l)
	}
	err = in.w.Commit(cctx, size, d)
	switch {
	case err == nil, errdefs.IsAlreadyExists(err):
		// Proof of possession hashed it in full; the copy on disk stays.
		return nil
	case errdefs.IsFailedPrecondition(err), errdefs.IsInvalidArgument(err):
		return fmt.Errorf("%w: %v", registry.ErrDigest, err)
	}
	return err
}

func (in *ingest) Close() error {
	if in.spool != nil {
		in.spool.Close()
		in.spool = nil
	}
	return in.w.Close()
}
