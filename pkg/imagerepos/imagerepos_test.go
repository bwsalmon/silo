package imagerepos_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/imagerepos"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
)

var ctx = context.Background()

func TestNames(t *testing.T) {
	for _, n := range []string{"app", "a", "0", "my-app", "my_app", "my__app", "a--b", strings.Repeat("a", 63)} {
		if !imagerepos.ValidName(n) {
			t.Errorf("ValidName(%q) = false", n)
		}
	}
	for _, n := range []string{"", "App", "-app", "_app", "app-", "app.v2", "a/b", "a b", "a___b", "id_app", strings.Repeat("a", 64)} {
		if imagerepos.ValidName(n) {
			t.Errorf("ValidName(%q) = true", n)
		}
	}
	for _, tag := range []string{"1", "latest", "v1.2.3", "_x", "A-b_c.d", strings.Repeat("a", 128)} {
		if !imagerepos.ValidTag(tag) {
			t.Errorf("ValidTag(%q) = false", tag)
		}
	}
	for _, tag := range []string{"", ".x", "-x", "a:b", "a/b", strings.Repeat("a", 129)} {
		if imagerepos.ValidTag(tag) {
			t.Errorf("ValidTag(%q) = true", tag)
		}
	}
	if !imagerepos.ValidDigest("sha256:" + strings.Repeat("ab", 32)) {
		t.Error("a sha256 digest is invalid")
	}
	for _, d := range []string{"", "sha256:", "sha256:" + strings.Repeat("AB", 32), "md5:" + strings.Repeat("a", 32), "sha256:" + strings.Repeat("a", 63)} {
		if imagerepos.ValidDigest(d) {
			t.Errorf("ValidDigest(%q) = true", d)
		}
	}
	if id := imagerepos.NewID(); !strings.HasPrefix(id, "ir-") || len(id) != 9 {
		t.Errorf("NewID = %q", id)
	}
	if got := imagerepos.Ref("/team-a/x", "app"); got != "registry.silo.internal/team-a/x/app" {
		t.Errorf("Ref = %q", got)
	}
	if got := imagerepos.Ref("/", "app"); got != "registry.silo.internal/app" {
		t.Errorf("Ref of the root = %q", got)
	}
	if got := imagerepos.RefByID("k7f3qa", "app"); got != "registry.silo.internal/id_k7f3qa/app" {
		t.Errorf("RefByID = %q", got)
	}
}

func TestParseName(t *testing.T) {
	for _, c := range []struct {
		name, ns string
		want     imagerepos.Name
	}{
		{"team-a/x/app", "", imagerepos.Name{SiloPath: "/team-a/x", Repo: "app"}},
		{"app", "", imagerepos.Name{SiloPath: "/", Repo: "app"}},
		{"app", "registry.silo.internal", imagerepos.Name{SiloPath: "/", Repo: "app"}},
		{"id_k7f3qa/app", "", imagerepos.Name{SiloID: "k7f3qa", Repo: "app"}},
		{"docker.io/library/alpine", "", imagerepos.Name{Upstream: "docker.io", UpstreamRepo: "library/alpine"}},
		{"docker.io/alpine", "", imagerepos.Name{Upstream: "docker.io", UpstreamRepo: "library/alpine"}},
		{"index.docker.io/alpine", "", imagerepos.Name{Upstream: "docker.io", UpstreamRepo: "library/alpine"}},
		{"registry-1.docker.io/library/alpine", "", imagerepos.Name{Upstream: "docker.io", UpstreamRepo: "library/alpine"}},
		{"gcr.io/distroless/static", "", imagerepos.Name{Upstream: "gcr.io", UpstreamRepo: "distroless/static"}},
		{"ghcr.io/o/r.v2/x", "", imagerepos.Name{Upstream: "ghcr.io", UpstreamRepo: "o/r.v2/x"}},
		{"localhost.test:5000/a", "", imagerepos.Name{Upstream: "localhost.test:5000", UpstreamRepo: "a"}},
		// A mirror request's ns selects the cache for the whole name.
		{"library/alpine", "docker.io", imagerepos.Name{Upstream: "docker.io", UpstreamRepo: "library/alpine"}},
		{"alpine", "docker.io", imagerepos.Name{Upstream: "docker.io", UpstreamRepo: "library/alpine"}},
	} {
		got, ok := imagerepos.ParseName(c.name, c.ns)
		if !ok || got != c.want {
			t.Errorf("ParseName(%q, %q) = %+v, %v; want %+v", c.name, c.ns, got, ok, c.want)
		}
	}
	for _, c := range []struct{ name, ns string }{
		{"", ""}, {"/app", ""}, {"app/", ""}, {"team-a//app", ""},
		{"Team-a/app", ""}, {"team_a/app", ""}, {"team-a/app.v2", ""}, {"team-a/App", ""},
		{"id_k7f3qa", ""}, {"id_K7F3QA/app", ""}, {"id_k7f3qa/x/app", ""}, {"id_/app", ""}, {"team-a/id_x", ""},
		{"docker.io", ""}, {"docker.io/", ""}, {"docker.io/Library/alpine", ""}, {"-x.io/a", ""},
		{"alpine", "not a host"}, {"alpine", "localhost"},
		{strings.Repeat("a/", 128) + "a", ""},
	} {
		if got, ok := imagerepos.ParseName(c.name, c.ns); ok {
			t.Errorf("ParseName(%q, %q) = %+v; want no name", c.name, c.ns, got)
		}
	}
}

// FuzzParseName checks the parser's invariants on any input: a name it
// takes is exactly one form, and writes back as itself (Docker Hub's
// normalised names aside), so no name reaches a repo other than the
// one it names.
func FuzzParseName(f *testing.F) {
	for _, s := range []string{"team-a/x/app", "app", "id_k7f3qa/app", "docker.io/alpine", "gcr.io/a/b", "a/b/c/d", "id_/x", "a.b"} {
		f.Add(s, "")
	}
	f.Add("alpine", "docker.io")
	f.Fuzz(func(t *testing.T, name, ns string) {
		n, ok := imagerepos.ParseName(name, ns)
		if !ok {
			return
		}
		forms := 0
		if n.Upstream != "" {
			forms++
			if n.UpstreamRepo == "" || n.Repo != "" || n.SiloPath != "" || n.SiloID != "" {
				t.Fatalf("%q, %q: upstream with other fields: %+v", name, ns, n)
			}
		}
		if n.SiloID != "" {
			forms++
			if n.SiloPath != "" {
				t.Fatalf("%q: by id and by path: %+v", name, n)
			}
		}
		if n.SiloPath != "" {
			forms++
			if !strings.HasPrefix(n.SiloPath, "/") || strings.Contains(n.SiloPath, ".") || strings.Contains(n.SiloPath, "_") {
				t.Fatalf("%q: silo path %q", name, n.SiloPath)
			}
		}
		if forms != 1 {
			t.Fatalf("%q, %q: %d forms: %+v", name, ns, forms, n)
		}
		if n.Local() {
			if !imagerepos.ValidName(n.Repo) {
				t.Fatalf("%q: repo %q is not a valid name", name, n.Repo)
			}
			if ns == "" || ns == imagerepos.Host {
				if back := n.String(); back != name {
					t.Fatalf("%q parsed as %+v, which writes back as %q", name, n, back)
				}
			}
		}
		again, ok := imagerepos.ParseName(n.String(), "")
		if !ok || !reflect.DeepEqual(again, n) {
			t.Fatalf("%q, %q: %+v writes as %q, which parses as %+v, %v", name, ns, n, n.String(), again, ok)
		}
	})
}

func digest(c byte) string { return "sha256:" + strings.Repeat(string([]byte{c}), 64) }

func TestTables(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}
	err = db.Tx(ctx, func(tx store.Tx) error {
		r := &imagerepos.Repo{ID: "ir-aaaaaa", Silo: "k7f3qa", Name: "app", Seq: 3}
		if err := imagerepos.Create(ctx, tx, r); err != nil {
			return err
		}
		if r.State != imagerepos.StateReady {
			t.Errorf("state %q", r.State)
		}
		if err := imagerepos.Create(ctx, tx, &imagerepos.Repo{ID: "ir-bbbbbb", Silo: "k7f3qa", Name: "app"}); !errors.Is(err, imagerepos.ErrExists) {
			t.Errorf("a second app: %v", err)
		}
		// Names are per silo.
		if err := imagerepos.Create(ctx, tx, &imagerepos.Repo{ID: "ir-cccccc", Silo: "m2p9xd", Name: "app"}); err != nil {
			return err
		}
		if err := imagerepos.Create(ctx, tx, &imagerepos.Repo{ID: "ir-dddddd", Silo: "k7f3qa", Name: "a.b"}); err == nil {
			t.Error("a name with a dot was taken")
		}

		// Two images sharing layer L: config C1/C2, layers L and M.
		l, m1, m2, c1, c2 := digest('a'), digest('b'), digest('c'), digest('d'), digest('e')
		man1 := &imagerepos.Manifest{Repo: r.ID, Digest: digest('1'), MediaType: "application/vnd.oci.image.manifest.v1+json", Size: 10,
			Refs: []imagerepos.Blob{{Digest: c1, Size: 100}, {Digest: l, Size: 1000}, {Digest: m1, Size: 2000}}}
		man2 := &imagerepos.Manifest{Repo: r.ID, Digest: digest('2'), MediaType: "application/vnd.oci.image.manifest.v1+json", Size: 20,
			Refs: []imagerepos.Blob{{Digest: c2, Size: 200}, {Digest: l, Size: 1000}, {Digest: m2, Size: 4000}}}
		index := &imagerepos.Manifest{Repo: r.ID, Digest: digest('3'), MediaType: "application/vnd.oci.image.index.v1+json", Size: 5,
			Refs: []imagerepos.Blob{{Digest: man1.Digest, Size: 10}, {Digest: man2.Digest, Size: 20}}}
		for _, m := range []*imagerepos.Manifest{man1, man2, index} {
			if err := imagerepos.PutManifest(ctx, tx, m); err != nil {
				return err
			}
		}
		ms, err := imagerepos.Manifests(ctx, tx, r.ID)
		if err != nil {
			return err
		}
		// L once: 10+100+1000+2000 + 20+200+4000 + 5.
		if got := imagerepos.Charged(ms); got != 7335 {
			t.Errorf("Charged = %d, want 7335", got)
		}
		if got := imagerepos.SizeOf(man1); got != 3110 {
			t.Errorf("SizeOf = %d", got)
		}

		if err := imagerepos.SetTag(ctx, tx, r.ID, "1", man1.Digest); err != nil {
			return err
		}
		if err := imagerepos.SetTag(ctx, tx, r.ID, "multi", index.Digest); err != nil {
			return err
		}
		if err := imagerepos.SetTag(ctx, tx, r.ID, "x", digest('9')); !errors.Is(err, imagerepos.ErrManifestNotFound) {
			t.Errorf("tagging a manifest the repo doesn't hold: %v", err)
		}
		if err := imagerepos.SetTag(ctx, tx, r.ID, ".bad", man1.Digest); err == nil {
			t.Error("an invalid tag was taken")
		}
		if u, err := imagerepos.Untagged(ctx, tx, r.ID); err != nil || len(u) != 0 {
			t.Errorf("Untagged = %v, %v; want none: man2 is the index's", u, err)
		}
		// Moving a tag leaves the old target, untagged.
		if err := imagerepos.SetTag(ctx, tx, r.ID, "1", man2.Digest); err != nil {
			return err
		}
		tags, err := imagerepos.Tags(ctx, tx, r.ID)
		if err != nil {
			return err
		}
		if len(tags) != 2 || tags[0].Tag != "1" || tags[0].Digest != man2.Digest {
			t.Errorf("tags %+v", tags)
		}
		if err := imagerepos.Untag(ctx, tx, r.ID, "nope"); !errors.Is(err, imagerepos.ErrTagNotFound) {
			t.Errorf("untag of no tag: %v", err)
		}

		// Deleting the index takes man1, which nothing else references
		// or tags, and keeps man2, which tag 1 points at.
		gone, err := imagerepos.DeleteManifest(ctx, tx, r.ID, index.Digest)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(gone, []string{index.Digest, man1.Digest}) {
			t.Errorf("DeleteManifest removed %v", gone)
		}
		if tags, _ := imagerepos.Tags(ctx, tx, r.ID); len(tags) != 1 || tags[0].Tag != "1" {
			t.Errorf("tags after the delete: %+v", tags)
		}
		if _, err := imagerepos.DeleteManifest(ctx, tx, r.ID, index.Digest); !errors.Is(err, imagerepos.ErrManifestNotFound) {
			t.Errorf("a second delete: %v", err)
		}
		if err := imagerepos.Untag(ctx, tx, r.ID, "1"); err != nil {
			return err
		}
		if u, _ := imagerepos.Untagged(ctx, tx, r.ID); !reflect.DeepEqual(u, []string{man2.Digest}) {
			t.Errorf("Untagged = %v", u)
		}

		if n, err := imagerepos.Count(ctx, tx, "k7f3qa"); err != nil || n != 1 {
			t.Errorf("Count = %d, %v", n, err)
		}
		if err := imagerepos.SetState(ctx, tx, r.ID, imagerepos.StateDeleting); err != nil {
			return err
		}
		if got, _ := imagerepos.Get(ctx, tx, r.ID); got.State != imagerepos.StateDeleting {
			t.Errorf("state %q", got.State)
		}
		if err := imagerepos.Delete(ctx, tx, r.ID); err != nil {
			return err
		}
		if ms, _ := imagerepos.Manifests(ctx, tx, r.ID); len(ms) != 0 {
			t.Errorf("manifests outlived their repo: %d", len(ms))
		}
		if err := imagerepos.Delete(ctx, tx, r.ID); !errors.Is(err, imagerepos.ErrNotFound) {
			t.Errorf("a second delete: %v", err)
		}
		rs, err := imagerepos.Of(ctx, tx, "m2p9xd", "k7f3qa")
		if err != nil || len(rs) != 1 || rs[0].ID != "ir-cccccc" {
			t.Errorf("Of = %v, %v", rs, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
