package docker

import (
	"context"
	"slices"
	"testing"
)

// A container's image is recorded in the drivers' namespace before
// their Prepare, by the manifest its record names, with that
// manifest's config and layers as its references.
func TestStageImage(t *testing.T) {
	e := newEnv(t)
	f := newFetcher(e.content)
	e.facade.opts.Fetcher = f
	e.facade.opts.RunNamespace = "silo"
	e.pull(e.a, "alpine:3")
	if r := e.content.Records("silo"); len(r) != 0 {
		t.Fatalf("a pull staged %v", r)
	}
	idx, err := parseManifest(f.objects[f.index], mediaOCIIndex)
	if err != nil {
		t.Fatal(err)
	}
	md := idx.Manifests[0].Digest
	m, err := parseManifest(f.objects[md], mediaOCIManifest)
	if err != nil {
		t.Fatal(err)
	}
	var blobs []string
	for _, d := range m.blobs() {
		blobs = append(blobs, d.Digest)
	}
	e.ok(e.a, "POST", "/v1.47/containers/create?name=web", obj{"Image": "alpine:3"})
	recs := e.content.Records("silo")
	if len(recs) != 1 {
		t.Fatalf("staged: %+v", recs)
	}
	r := recs[0]
	if r.Name != StagedName(md) || r.Digest != md || r.MediaType != mediaOCIManifest || r.Size != int64(len(f.objects[md])) {
		t.Errorf("the record: %+v", r)
	}
	if !slices.Equal(r.Refs, blobs) {
		t.Errorf("refs %v, want %v", r.Refs, blobs)
	}
	// A second container of the image records it again, in place.
	e.ok(e.a, "POST", "/v1.47/containers/create?name=web2", obj{"Image": "alpine:3"})
	if n := len(e.content.Records("silo")); n != 1 {
		t.Errorf("%d records after a second create", n)
	}
	// An index's references are its platform's child and the child's.
	refs, size, err := e.facade.imageRefs(context.Background(), f.index, mediaOCIIndex)
	if err != nil || size != int64(len(f.objects[f.index])) || !slices.Equal(refs, append([]string{md}, blobs...)) {
		t.Errorf("an index's refs: %v, %d, %v", refs, size, err)
	}
}
