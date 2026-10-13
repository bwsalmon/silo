// Package contenttest is the contract every registry.Content keeps,
// as the registry relies on it: run against registry.Memory in the `go`
// job and against silo-containerd (package containerdstore) in the
// `nsd` job, so that the fake the handler's tests run on behaves as
// containerd does.
package contenttest

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/registry"
)

// Run runs the contract against c. Each case uses blobs and names of
// its own, so c may be shared, and may hold others' content. Leases are
// held to the real clock: the cases that wait for one to expire take a
// few seconds.
func Run(t *testing.T, c registry.Content) {
	cases := []struct {
		name string
		fn   func(*testing.T, registry.Content)
	}{
		{"IngestAndCommit", ingestAndCommit},
		{"CommitDuplicate", commitDuplicate},
		{"CommitWrongDigest", commitWrongDigest},
		{"Truncate", truncate},
		{"Resume", resume},
		{"LeaseExpires", leaseExpires},
		{"LeaseExtended", leaseExtended},
		{"ImageKeepsRefs", imageKeepsRefs},
		{"CacheNamespace", cacheNamespace},
		{"ReadLease", readLease},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t, c) })
	}
}

var ctx = context.Background()

func blob(n int) ([]byte, string) {
	b := make([]byte, n)
	rand.Read(b)
	return b, registry.Digest("sha256", b)
}

func id() string {
	b := make([]byte, 6)
	rand.Read(b)
	return registry.Digest("sha256", b)[7:19]
}

// put commits b under lease and returns its digest.
func put(t *testing.T, c registry.Content, lease string, b []byte) string {
	t.Helper()
	d := registry.Digest("sha256", b)
	in, err := c.Ingest(ctx, "contenttest-"+id(), lease)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if _, err := in.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := in.Commit(ctx, int64(len(b)), d); err != nil {
		t.Fatalf("commit %s: %v", d, err)
	}
	return d
}

func lease(t *testing.T, c registry.Content, d time.Duration) string {
	t.Helper()
	l := "contenttest-" + id()
	if err := c.Lease(ctx, l, time.Now().Add(d)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.DeleteLease(ctx, l) })
	return l
}

func read(t *testing.T, c registry.Content, d string) []byte {
	t.Helper()
	ra, n, closer, err := c.Open(ctx, d)
	if err != nil {
		t.Fatalf("open %s: %v", d, err)
	}
	defer closer.Close()
	b, err := io.ReadAll(io.NewSectionReader(ra, 0, n))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func has(t *testing.T, c registry.Content, d string) bool {
	t.Helper()
	_, err := c.Size(ctx, d)
	if errors.Is(err, registry.ErrUnknown) {
		return false
	} else if err != nil {
		t.Fatal(err)
	}
	return true
}

func collect(t *testing.T, c registry.Content) {
	t.Helper()
	if err := c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
}

func ingestAndCommit(t *testing.T, c registry.Content) {
	b, _ := blob(70000)
	d := put(t, c, lease(t, c, time.Hour), b)
	if n, err := c.Size(ctx, d); err != nil || n != int64(len(b)) {
		t.Fatalf("size: %d %v", n, err)
	}
	if got := read(t, c, d); string(got) != string(b) {
		t.Fatal("read back other bytes")
	}
	_, missing := blob(10)
	if _, err := c.Size(ctx, missing); !errors.Is(err, registry.ErrUnknown) {
		t.Fatalf("size of a blob that isn't there: %v", err)
	}
	if _, _, _, err := c.Open(ctx, missing); !errors.Is(err, registry.ErrUnknown) {
		t.Fatalf("open of a blob that isn't there: %v", err)
	}
}

// commitDuplicate is proof of possession: a second upload of a blob is
// hashed in full and dropped, and its commit succeeds.
func commitDuplicate(t *testing.T, c registry.Content) {
	b, _ := blob(5000)
	l := lease(t, c, time.Hour)
	d := put(t, c, l, b)
	if put(t, c, l, b) != d {
		t.Fatal("digest changed")
	}
}

func commitWrongDigest(t *testing.T, c registry.Content) {
	b, _ := blob(5000)
	_, other := blob(5000)
	ref := "contenttest-" + id()
	in, err := c.Ingest(ctx, ref, lease(t, c, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Abort(ctx, ref)
	defer in.Close()
	in.Write(b)
	if err := in.Commit(ctx, int64(len(b)), other); !errors.Is(err, registry.ErrDigest) {
		t.Fatalf("commit as another digest: %v, want ErrDigest", err)
	}
}

// truncate takes back a chunk after Offset, as the registry does when
// the silo has no room for it, and takes everything back.
func truncate(t *testing.T, c registry.Content) {
	a, _ := blob(3000)
	b, _ := blob(4000)
	cc, _ := blob(5000)
	ref := "contenttest-" + id()
	in, err := c.Ingest(ctx, ref, lease(t, c, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	in.Write(a)
	start := in.Offset()
	if start != int64(len(a)) {
		t.Fatalf("offset %d", start)
	}
	in.Write(b)
	if err := in.Truncate(start); err != nil {
		t.Fatalf("taking back a chunk: %v", err)
	}
	if n := in.Offset(); n != start {
		t.Fatalf("offset after truncate: %d", n)
	}
	in.Write(cc)
	want := append(append([]byte{}, a...), cc...)
	if got := in.Digest("sha256"); got != registry.Digest("sha256", want) {
		t.Fatalf("digest %s after truncate", got)
	}
	d := registry.Digest("sha256", want)
	if err := in.Commit(ctx, in.Offset(), d); err != nil {
		t.Fatal(err)
	}
	if string(read(t, c, d)) != string(want) {
		t.Fatal("read back other bytes")
	}

	// To nothing, after the bytes are in.
	in2, err := c.Ingest(ctx, ref+"-0", lease(t, c, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	defer in2.Close()
	in2.Write(a)
	in2.Offset()
	if err := in2.Truncate(0); err != nil {
		t.Fatalf("truncate to 0: %v", err)
	}
	in2.Write(b)
	if err := in2.Commit(ctx, int64(len(b)), registry.Digest("sha256", b)); err != nil {
		t.Fatalf("commit after truncate to 0: %v", err)
	}
}

// resume opens an ingest again by its ref, where it was left.
func resume(t *testing.T, c registry.Content) {
	a, _ := blob(3000)
	b, _ := blob(4000)
	ref := "contenttest-" + id()
	l := lease(t, c, time.Hour)
	in, err := c.Ingest(ctx, ref, l)
	if err != nil {
		t.Fatal(err)
	}
	in.Write(a)
	if in.Offset() != int64(len(a)) {
		t.Fatal("offset")
	}
	in.Close()
	in, err = c.Ingest(ctx, ref, l)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if n := in.Offset(); n != int64(len(a)) {
		t.Fatalf("resumed at %d, want %d", n, len(a))
	}
	in.Write(b)
	all := append(append([]byte{}, a...), b...)
	if err := in.Commit(ctx, int64(len(all)), registry.Digest("sha256", all)); err != nil {
		t.Fatal(err)
	}
}

// expire waits past an expiry written in whole seconds.
func expire(d time.Duration) { time.Sleep(d + 1100*time.Millisecond) }

func leaseExpires(t *testing.T, c registry.Content) {
	b, _ := blob(1000)
	d := put(t, c, lease(t, c, 2*time.Second), b)
	collect(t, c)
	if !has(t, c, d) {
		t.Fatal("collected under its lease")
	}
	expire(2 * time.Second)
	collect(t, c)
	if has(t, c, d) {
		t.Fatal("kept after its lease expired")
	}
}

// leaseExtended is the grace hour: an upload's lease, extended at
// commit, keeps the blob after the expiry it was made with.
func leaseExtended(t *testing.T, c registry.Content) {
	b, _ := blob(1000)
	l := lease(t, c, 2*time.Second)
	d := put(t, c, l, b)
	if err := c.Lease(ctx, l, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("extending: %v", err)
	}
	expire(2 * time.Second)
	collect(t, c)
	if !has(t, c, d) {
		t.Fatal("collected under its extended lease")
	}
	if err := c.DeleteLease(ctx, l); err != nil {
		t.Fatal(err)
	}
	collect(t, c)
	if has(t, c, d) {
		t.Fatal("kept after its lease ended")
	}
	if err := c.DeleteLease(ctx, l); err != nil {
		t.Fatalf("ending an ended lease: %v", err)
	}
}

// imageKeepsRefs is a repo's manifest record: it keeps the manifest and
// what the manifest references, and nothing once it is gone.
func imageKeepsRefs(t *testing.T, c registry.Content) {
	l := lease(t, c, time.Hour)
	cfg, _ := blob(300)
	layer, _ := blob(9000)
	man, _ := blob(500)
	refs := []string{put(t, c, l, cfg), put(t, c, l, layer)}
	m := put(t, c, l, man)
	name := "repo" + id() + "@" + m
	img := registry.Image{Name: name, Digest: m, MediaType: "application/vnd.oci.image.manifest.v1+json", Size: 500,
		Refs: refs, Labels: map[string]string{registry.SeqLabel: "7"}}
	if err := c.PutImage(ctx, registry.Namespace, img); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteLease(ctx, l); err != nil {
		t.Fatal(err)
	}
	collect(t, c)
	for _, d := range append(refs, m) {
		if !has(t, c, d) {
			t.Fatalf("%s collected while a record references it", d)
		}
	}
	imgs, err := c.ListImages(ctx, registry.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(imgs, func(i registry.Image) bool { return i.Name == name })
	if i < 0 {
		t.Fatalf("%s not listed", name)
	}
	got := imgs[i]
	if got.Digest != m || got.MediaType != img.MediaType || got.Size != 500 || !slices.Equal(got.Refs, refs) || got.Labels[registry.SeqLabel] != "7" {
		t.Fatalf("listed %+v, put %+v", got, img)
	}
	// Replaced, with a label more.
	img.Labels = map[string]string{registry.SeqLabel: "7", registry.OrphanLabel: "now"}
	if err := c.PutImage(ctx, registry.Namespace, img); err != nil {
		t.Fatalf("replacing: %v", err)
	}
	imgs, _ = c.ListImages(ctx, registry.Namespace)
	if i := slices.IndexFunc(imgs, func(i registry.Image) bool { return i.Name == name }); i < 0 || imgs[i].Labels[registry.OrphanLabel] != "now" {
		t.Fatal("the replaced record's labels")
	}
	nss, err := c.Namespaces(ctx)
	if err != nil || !slices.Contains(nss, registry.Namespace) {
		t.Fatalf("namespaces %v %v", nss, err)
	}
	if err := c.DeleteImage(ctx, registry.Namespace, name); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteImage(ctx, registry.Namespace, name); err != nil {
		t.Fatalf("deleting a record that is gone: %v", err)
	}
	collect(t, c)
	for _, d := range append(refs, m) {
		if has(t, c, d) {
			t.Fatalf("%s kept after its record went", d)
		}
	}
}

// cacheNamespace is a cache entry: a blob fetched into the store is
// kept by its silo-cache record, and still found and read.
func cacheNamespace(t *testing.T, c registry.Content) {
	l := lease(t, c, time.Hour)
	b, _ := blob(2000)
	d := put(t, c, l, b)
	name := "upstream.example/lib/" + id() + "@" + d
	if err := c.PutImage(ctx, registry.CacheNamespace, registry.Image{Name: name, Digest: d, Size: 2000}); err != nil {
		t.Fatal(err)
	}
	c.DeleteLease(ctx, l)
	collect(t, c)
	if !has(t, c, d) || string(read(t, c, d)) != string(b) {
		t.Fatal("the cache entry's blob")
	}
	imgs, err := c.ListImages(ctx, registry.CacheNamespace)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(imgs, func(i registry.Image) bool { return i.Name == name })
	if i < 0 || imgs[i].MediaType != "" || imgs[i].Digest != d {
		t.Fatalf("listed %v", imgs)
	}
	if err := c.DeleteImage(ctx, registry.CacheNamespace, name); err != nil {
		t.Fatal(err)
	}
	collect(t, c)
	if has(t, c, d) {
		t.Fatal("kept after its entry went")
	}
}

// readLease is a request's: what it opened stays until it ends.
func readLease(t *testing.T, c registry.Content) {
	l := lease(t, c, time.Hour)
	b, _ := blob(2000)
	d := put(t, c, l, b)
	rctx, done, err := c.ReadLease(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, _, closer, err := c.Open(rctx, d)
	if err != nil {
		t.Fatal(err)
	}
	closer.Close()
	c.DeleteLease(ctx, l)
	collect(t, c)
	if !has(t, c, d) {
		t.Fatal("collected mid-request")
	}
	done()
	collect(t, c)
	if has(t, c, d) {
		t.Fatal("kept after the request ended")
	}
}
