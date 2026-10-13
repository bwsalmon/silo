package registry_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// asSilo is a transport that calls the registry as silo, by origin, as
// the gateway does for a container in it.
type asSilo struct{ silo string }

func (a asSilo) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Silo-Caller", a.silo)
	return http.DefaultTransport.RoundTrip(r)
}

// crane, go-containerregistry's client, against the registry in
// process: a push of an image and an index by tag, a pull that gets
// the same digests, a copy between repos (a cross-repo mount), the tag
// list, the catalog, and the referrers API.
func TestCrane(t *testing.T) {
	e := newEnv(t, nil)
	a := e.mk(e.root, "-"+e.root, "team-a", nil)
	host := strings.TrimPrefix(e.srv.URL, "http://")
	opts := []crane.Option{crane.Insecure, crane.WithTransport(asSilo{a})}

	img, err := random.Image(4096, 3)
	if err != nil {
		t.Fatal(err)
	}
	ref := host + "/team-a/app:1"
	if err := crane.Push(img, ref, opts...); err != nil {
		t.Fatalf("crane push: %v", err)
	}
	want, _ := img.Digest()
	got, err := crane.Pull(ref, opts...)
	if err != nil {
		t.Fatalf("crane pull: %v", err)
	}
	if d, _ := got.Digest(); d != want {
		t.Fatalf("pulled %s, pushed %s", d, want)
	}
	ls, err := got.Layers()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range ls {
		rc, err := l.Compressed()
		if err != nil {
			t.Fatalf("pulling a layer: %v", err)
		}
		rc.Close()
	}
	if err := crane.Copy(ref, host+"/team-a/copy:latest", opts...); err != nil {
		t.Fatalf("crane copy: %v", err)
	}
	if d, err := crane.Digest(host+"/team-a/copy:latest", opts...); err != nil || d != want.String() {
		t.Fatalf("the copy: %s %v", d, err)
	}
	// An index of two images.
	idx, err := random.Index(1024, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := name.ParseReference(host+"/team-a/app:multi", name.Insecure)
	if err := remote.WriteIndex(r, idx, remote.WithTransport(asSilo{a})); err != nil {
		t.Fatalf("pushing an index: %v", err)
	}
	if tags, err := crane.ListTags(host+"/team-a/app", opts...); err != nil || strings.Join(tags, ",") != "1,multi" {
		t.Fatalf("tags: %v %v", tags, err)
	}
	if cat, err := crane.Catalog(host, opts...); err != nil || strings.Join(cat, ",") != "team-a/app,team-a/copy" {
		t.Fatalf("catalog: %v %v", cat, err)
	}
	// A signature-like artifact with the image as its subject.
	art := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	art = mutate.ConfigMediaType(art, "application/vnd.example.sig")
	desc, err := remote.Head(mustRef(t, ref), remote.WithTransport(asSilo{a}))
	if err != nil {
		t.Fatal(err)
	}
	art = mutate.Subject(art, *desc).(v1.Image)
	ad, _ := art.Digest()
	if err := remote.Write(mustRef(t, host+"/team-a/app@"+ad.String()), art, remote.WithTransport(asSilo{a})); err != nil {
		t.Fatalf("pushing an artifact: %v", err)
	}
	refs, err := remote.Referrers(mustDigest(t, host+"/team-a/app@"+want.String()), remote.WithTransport(asSilo{a}))
	if err != nil {
		t.Fatalf("referrers: %v", err)
	}
	im, _ := refs.IndexManifest()
	if len(im.Manifests) != 1 || im.Manifests[0].Digest != ad || im.Manifests[0].ArtifactType != "application/vnd.example.sig" {
		t.Fatalf("referrers: %+v", im.Manifests)
	}

	// A sibling's crane sees nothing.
	b := e.mk(e.root, "-"+e.root, "team-b", nil)
	if _, err := crane.Pull(ref, crane.Insecure, crane.WithTransport(asSilo{b})); err == nil || !strings.Contains(err.Error(), "NAME_UNKNOWN") {
		t.Fatalf("a sibling's pull: %v", err)
	}
	if cat, err := crane.Catalog(host, crane.Insecure, crane.WithTransport(asSilo{b})); err != nil || len(cat) != 0 {
		t.Fatalf("a sibling's catalog: %v %v", cat, err)
	}
}

func mustRef(t *testing.T, s string) name.Reference {
	t.Helper()
	r, err := name.ParseReference(s, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mustDigest(t *testing.T, s string) name.Digest {
	t.Helper()
	d, err := name.NewDigest(s, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
