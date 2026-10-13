package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Content is what the registry needs of silo-containerd's content API
// (local-registry.md, §4 "One content store, records per repo", call
// L59): blobs by digest, ingests that a client's upload writes into,
// leases that keep an upload's bytes while it is open and for the grace
// hour after, and the image records of namespace silo-registry that
// keep what a repo references. containerd implements it on the host
// (a later step's wiring); Memory is the fake every test runs on.
//
// Content answers for every blob on disk, whoever pushed it. The
// registry never asks it whether a blob exists on a caller's behalf:
// that goes through the one blob-access function, Service.blob, which
// answers from the repo's own records (call L62).
type Content interface {
	// Size is a stored blob's size, or ErrUnknown.
	Size(ctx context.Context, digest string) (int64, error)
	// Open opens a stored blob for reading, with its size, or
	// ErrUnknown.
	Open(ctx context.Context, digest string) (io.ReaderAt, int64, io.Closer, error)
	// Ingest opens the ingest ref under lease, creating it empty or
	// resuming it where it was left.
	Ingest(ctx context.Context, ref, lease string) (Ingest, error)
	// Abort removes the ingest ref and its bytes.
	Abort(ctx context.Context, ref string) error
	// Lease creates or extends a lease to expire, as containerd's
	// containerd.io/gc.expire label does.
	Lease(ctx context.Context, id string, expire time.Time) error
	// DeleteLease ends a lease; what it alone held is GC's.
	DeleteLease(ctx context.Context, id string) error
	// PutImage creates or replaces an image record in silo-registry.
	PutImage(ctx context.Context, img Image) error
	// DeleteImage removes an image record of silo-registry.
	DeleteImage(ctx context.Context, name string) error
}

// Ingest is an upload's bytes on their way into the store.
type Ingest interface {
	io.Writer
	// Offset is how many bytes the ingest holds.
	Offset() int64
	// Truncate drops the bytes after size, as containerd's
	// content.Writer does: a chunk refused for quota is taken back.
	Truncate(size int64) error
	// Digest is the digest of the bytes so far, in algorithm alg.
	Digest(alg string) string
	// Commit stores the bytes as digest, checking it and size. A blob
	// already stored by digest is not an error: the new bytes, hashed
	// in full, are dropped in favour of the copy on disk.
	Commit(ctx context.Context, size int64, digest string) error
	Close() error
}

// Image is one silo-registry record: <repo id>@<digest>, whose target
// is the manifest, and whose labels carry dev.silo.seq. Refs are the
// digests the manifest references, which containerd's implementation
// labels the manifest's content with (containerd.io/gc.ref.content.*),
// so that GC keeps exactly what some repo references.
type Image struct {
	Name      string
	Digest    string
	MediaType string
	Size      int64
	Refs      []string
	Labels    map[string]string
}

// ErrUnknown is a blob the content store doesn't hold.
var ErrUnknown = errors.New("registry: content not found")

// ErrDigest is a commit whose bytes don't match the digest it named.
var ErrDigest = errors.New("registry: digest mismatch")

// Memory is an in-memory Content, for tests and the fake driver.
type Memory struct {
	mu      sync.Mutex
	blobs   map[string][]byte
	ingests map[string]*memIngest
	leases  map[string]time.Time
	images  map[string]Image
}

// NewMemory returns an empty Memory.
func NewMemory() *Memory {
	return &Memory{blobs: map[string][]byte{}, ingests: map[string]*memIngest{}, leases: map[string]time.Time{},
		images: map[string]Image{}}
}

func (m *Memory) Size(_ context.Context, digest string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.blobs[digest]
	if !ok {
		return 0, ErrUnknown
	}
	return int64(len(b)), nil
}

func (m *Memory) Open(_ context.Context, digest string) (io.ReaderAt, int64, io.Closer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.blobs[digest]
	if !ok {
		return nil, 0, nil, ErrUnknown
	}
	return bytes.NewReader(b), int64(len(b)), io.NopCloser(nil), nil
}

func (m *Memory) Ingest(_ context.Context, ref, lease string) (Ingest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	in := m.ingests[ref]
	if in == nil {
		in = &memIngest{m: m, ref: ref}
		m.ingests[ref] = in
	}
	return in, nil
}

func (m *Memory) Abort(_ context.Context, ref string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.ingests, ref)
	return nil
}

func (m *Memory) Lease(_ context.Context, id string, expire time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.leases[id] = expire
	return nil
}

func (m *Memory) DeleteLease(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.leases, id)
	return nil
}

func (m *Memory) PutImage(_ context.Context, img Image) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.images[img.Name] = img
	return nil
}

func (m *Memory) DeleteImage(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.images, name)
	return nil
}

// Images lists the silo-registry records, by name.
func (m *Memory) Images() []Image {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Image
	for _, img := range m.images {
		out = append(out, img)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Leases lists the open leases and when each expires.
func (m *Memory) Leases() map[string]time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]time.Time{}
	for k, v := range m.leases {
		out[k] = v
	}
	return out
}

// Put stores b, as another silo's push or a facade pull would, and
// returns its sha256 digest.
func (m *Memory) Put(b []byte) string {
	d := Digest("sha256", b)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blobs[d] = append([]byte(nil), b...)
	return d
}

type memIngest struct {
	m   *Memory
	ref string
	buf []byte
}

func (in *memIngest) Write(p []byte) (int, error) {
	in.m.mu.Lock()
	defer in.m.mu.Unlock()
	in.buf = append(in.buf, p...)
	return len(p), nil
}

func (in *memIngest) Offset() int64 {
	in.m.mu.Lock()
	defer in.m.mu.Unlock()
	return int64(len(in.buf))
}

func (in *memIngest) Truncate(size int64) error {
	in.m.mu.Lock()
	defer in.m.mu.Unlock()
	if size > int64(len(in.buf)) || size < 0 {
		return fmt.Errorf("registry: truncate to %d of %d bytes", size, len(in.buf))
	}
	in.buf = in.buf[:size]
	return nil
}

func (in *memIngest) Digest(alg string) string {
	in.m.mu.Lock()
	defer in.m.mu.Unlock()
	return Digest(alg, in.buf)
}

func (in *memIngest) Commit(_ context.Context, size int64, digest string) error {
	in.m.mu.Lock()
	defer in.m.mu.Unlock()
	alg, _, _ := strings.Cut(digest, ":")
	if int64(len(in.buf)) != size || Digest(alg, in.buf) != digest {
		return ErrDigest
	}
	if _, ok := in.m.blobs[digest]; !ok {
		in.m.blobs[digest] = in.buf
	}
	delete(in.m.ingests, in.ref)
	return nil
}

func (in *memIngest) Close() error { return nil }

// newHash is the hash of a digest algorithm the registry takes.
func newHash(alg string) hash.Hash {
	if alg == "sha512" {
		return sha512.New()
	}
	return sha256.New()
}

// Digest is b's digest in algorithm alg, sha256 or sha512.
func Digest(alg string, b []byte) string {
	h := newHash(alg)
	h.Write(b)
	if alg != "sha512" {
		alg = "sha256"
	}
	return alg + ":" + hex.EncodeToString(h.Sum(nil))
}
