package docker

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/store"
)

// The facade's half of the local registry (local-registry.md §7, call
// L68), and the facade-specific cases of its §12.

// login is the test's upstream: good/creds is the one credential that
// works.
func login(_ context.Context, _, server, user, pass string) error {
	if user == "good" && pass == "creds" {
		return nil
	}
	return errors.New("unauthorized")
}

// send makes a call with a raw body.
func (e *env) send(caller, method, path string, body []byte) (int, []byte) {
	e.t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, bytes.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Silo-Caller", caller)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// lines are a progress stream's JSON lines.
func lines(t *testing.T, b []byte) []obj {
	t.Helper()
	var out []obj
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		o := obj{}
		if err := json.Unmarshal(sc.Bytes(), &o); err != nil {
			t.Fatalf("%q is not a JSON line", sc.Text())
		}
		out = append(out, o)
	}
	return out
}

// auditOf is every audit record of kind, with its silo as _silo.
func (e *env) auditOf(kind string) []obj {
	e.t.Helper()
	var out []obj
	err := e.db.Tx(ctx, func(tx store.Tx) error {
		rows, err := tx.Query(ctx, `SELECT silo, fields FROM audit WHERE kind = ? ORDER BY n`, kind)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = nil
		for rows.Next() {
			var silo, fields string
			if err := rows.Scan(&silo, &fields); err != nil {
				return err
			}
			f := obj{}
			json.Unmarshal([]byte(fields), &f)
			f["_silo"] = silo
			out = append(out, f)
		}
		return rows.Err()
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

// push tags caller's alpine as ref and pushes it, returning the
// digest the stream's aux reports.
func (e *env) push(caller, repo, tag string) string {
	e.t.Helper()
	e.pull(caller, "alpine")
	e.ok(caller, "POST", "/v1.47/images/alpine/tag?repo=registry.silo.internal/"+repo+"&tag="+tag, nil)
	code, b := e.send(caller, "POST", "/v1.47/images/registry.silo.internal/"+repo+"/push?tag="+tag, nil)
	if code != 200 {
		e.t.Fatalf("push %s:%s: %d %s", repo, tag, code, b)
	}
	ls := lines(e.t, b)
	aux, _ := ls[len(ls)-1]["aux"].(map[string]any)
	if aux == nil || aux["Tag"] != tag || !strings.HasPrefix(aux["Digest"].(string), "sha256:") || aux["Size"] == nil {
		e.t.Fatalf("push %s:%s: the stream ends %v", repo, tag, ls[len(ls)-1])
	}
	return aux["Digest"].(string)
}

// Tag, push, pull and run of a local image, from the same silo and from
// an importer; a sibling gets manifest unknown.
func TestPushPullRun(t *testing.T) {
	e := newEnv(t)
	digest := e.push(e.a, "team-a/app", "1")
	if p := e.auditOf(KindImagePush); len(p) != 1 || p[0]["_silo"] != e.a || p[0]["digest"] != digest {
		t.Fatalf("image.push: %v", p)
	}
	if p := e.auditOf("registry.push"); len(p) != 1 || p[0]["digest"] != digest {
		t.Fatalf("registry.push: %v", p)
	}
	// The repo is charged for its bytes, and nothing was copied: the
	// content store holds each blob once.
	if blobs := len(e.content.Images()); blobs != 1 {
		t.Fatalf("silo-registry records: %d", blobs)
	}
	inspect := e.ok(e.a, "GET", "/v1.47/images/alpine/json", nil)
	if !slices.Contains(inspect["RepoDigests"].([]any), any("registry.silo.internal/team-a/app@"+digest)) {
		t.Fatalf("RepoDigests: %v", inspect["RepoDigests"])
	}

	// Run it from another container of the same silo, after dropping the
	// tag: the pull makes the record again.
	e.ok(e.a, "DELETE", "/v1.47/images/registry.silo.internal/team-a/app:1", nil)
	pulls := len(e.images.pulls)
	e.pull(e.a, "registry.silo.internal/team-a/app:1")
	if len(e.images.pulls) != pulls {
		t.Fatal("a local pull went to the upstream puller")
	}
	id := e.ok(e.a, "POST", "/v1.47/containers/create?name=from-repo", obj{"Image": "registry.silo.internal/team-a/app:1"})["Id"].(string)
	e.ok(e.a, "POST", "/v1.47/containers/"+id+"/start", nil)

	// A sibling can't read it: Docker's manifest unknown, the same as a
	// repo that doesn't exist.
	e.fail(404, "manifest unknown", e.b, "POST", "/v1.47/images/create?fromImage=registry.silo.internal/team-a/app&tag=1", nil)
	e.fail(404, "manifest unknown", e.b, "POST", "/v1.47/images/create?fromImage=registry.silo.internal/team-a/nothing&tag=1", nil)
	// Nor push to it.
	e.pull(e.b, "alpine")
	e.ok(e.b, "POST", "/v1.47/images/alpine/tag?repo=registry.silo.internal/team-a/app&tag=2", nil)
	e.fail(404, "name unknown", e.b, "POST", "/v1.47/images/registry.silo.internal/team-a/app/push?tag=2", nil)

	// An importer pulls and runs it.
	e.ok(e.a, "POST", "/silo/v1/self/exports", obj{"name": "imgs", "protocol": "registry", "repos": []string{"app"}, "to": []string{"/team-b"}})
	e.ok(e.b, "POST", "/silo/v1/self/imports", obj{"export": "/team-a/imgs"})
	e.pull(e.b, "registry.silo.internal/team-a/app:1")
	id = e.ok(e.b, "POST", "/v1.47/containers/create?name=imported", obj{"Image": "registry.silo.internal/team-a/app:1"})["Id"].(string)
	e.ok(e.b, "POST", "/v1.47/containers/"+id+"/start", nil)
	// And still can't push.
	e.fail(404, "name unknown", e.b, "POST", "/v1.47/images/registry.silo.internal/team-a/app/push?tag=2", nil)
}

// A push to any other registry is 501, and copies nothing.
func TestPushElsewhere(t *testing.T) {
	e := newEnv(t)
	e.pull(e.a, "alpine")
	for _, name := range []string{"ghcr.io/o/r", "alpine", "docker.io/library/alpine", "registry.example.com/x"} {
		e.fail(501, "silo: pushes go to registry.silo.internal only; this host copies nothing to external registries",
			e.a, "POST", "/v1.47/images/"+name+"/push?tag=latest", nil)
	}
	if len(e.content.Images()) != 0 {
		t.Fatal("a refused push made a record")
	}
}

// refusing is a puller for a silo whose egress policy is empty.
type refusing struct{}

func (refusing) Pull(context.Context, string, string, string) (*Pulled, error) {
	return nil, forbidden("silo: egress to the upstream is denied")
}

// A silo with an empty egress policy still pulls its own local repo:
// the content is on this host.
func TestLocalPullWithoutEgress(t *testing.T) {
	e := newEnv(t)
	e.push(e.a, "team-a/app", "1")
	e.facade.opts.Images = refusing{}
	e.fail(403, "egress", e.a, "POST", "/v1.47/images/create?fromImage=busybox", nil)
	e.pull(e.a, "registry.silo.internal/team-a/app:1")
	e.pull(e.a, "registry.silo.internal/id_"+e.a+"/app:1")
}

// save then load round-trips an image with the same digests, in
// another silo.
func TestSaveLoad(t *testing.T) {
	e := newEnv(t)
	e.pull(e.a, "alpine")
	e.ok(e.a, "POST", "/v1.47/images/alpine/tag?repo=mine&tag=v1", nil)
	before := e.ok(e.a, "GET", "/v1.47/images/mine:v1/json", nil)
	code, archive := e.send(e.a, "GET", "/v1.47/images/get?names=mine:v1", nil)
	if code != 200 {
		t.Fatalf("save: %d %s", code, archive)
	}
	names := tarNames(t, archive)
	for _, n := range []string{"index.json", "manifest.json", "oci-layout"} {
		if !slices.Contains(names, n) {
			t.Fatalf("save has no %s: %v", n, names)
		}
	}
	code, b := e.send(e.b, "POST", "/v1.47/images/load", archive)
	if code != 200 || !strings.Contains(string(b), "Loaded image: mine:v1") {
		t.Fatalf("load: %d %s", code, b)
	}
	after := e.ok(e.b, "GET", "/v1.47/images/mine:v1/json", nil)
	if after["Id"] != before["Id"] {
		t.Fatalf("load gave %v, saved %v", after["Id"], before["Id"])
	}
	// Saved again from b, the archive is the same bytes.
	_, again := e.send(e.b, "GET", "/v1.47/images/mine:v1/get", nil)
	if !bytes.Equal(again, archive) {
		t.Fatal("save after load differs from the first save")
	}
	if e.used(e.b, ClassDiskGB) != 1 {
		t.Fatalf("load charged %d GiB", e.used(e.b, ClassDiskGB))
	}

	// The legacy form alone, manifest.json and its paths, loads too.
	legacy := rewrite(t, archive, func(name string, b []byte) (string, []byte, bool) {
		return name, b, name != "index.json" && name != "oci-layout"
	})
	if code, b := e.send(e.ci, "POST", "/v1.47/images/load", legacy); code != 200 || !strings.Contains(string(b), "Loaded image: mine:v1") {
		t.Fatalf("legacy load: %d %s", code, b)
	}
	if got := e.ok(e.ci, "GET", "/v1.47/images/mine:v1/json", nil); got["Id"] != before["Id"] {
		t.Fatalf("legacy load gave %v", got["Id"])
	}
}

// load of an archive with a mismatched digest, a path traversal entry
// or a size past the silo's free disk is refused and writes nothing.
func TestLoadRefusals(t *testing.T) {
	e := newEnv(t)
	cfg := []byte(`{"created":"2026-01-01T00:00:00Z","os":"linux","config":{"Cmd":["/x"]},"rootfs":{"type":"layers","diff_ids":[]}}`)
	layer := testLayer(map[string]string{"x": "new"})
	m, _ := json.Marshal(imageManifest{SchemaVersion: 2, MediaType: mediaOCIManifest,
		Config: &descriptor{MediaType: mediaOCIConfig, Digest: digestOf(cfg), Size: int64(len(cfg))},
		Layers: []descriptor{{MediaType: mediaOCILayer, Digest: digestOf(layer), Size: int64(len(layer))}}})
	idx, _ := json.Marshal(imageManifest{SchemaVersion: 2, MediaType: mediaOCIIndex, Manifests: []descriptor{{
		MediaType: mediaOCIManifest, Digest: digestOf(m), Size: int64(len(m)),
		Annotations: map[string]string{"io.containerd.image.name": "evil:1"}}}})
	blob := func(b []byte) string { return "blobs/sha256/" + strings.TrimPrefix(digestOf(b), "sha256:") }
	good := map[string][]byte{"oci-layout": []byte(`{"imageLayoutVersion":"1.0.0"}`), "index.json": idx,
		blob(cfg): cfg, blob(layer): layer, blob(m): m}
	order := []string{"oci-layout", blob(cfg), blob(layer), blob(m), "index.json"}

	nothing := func(what string) {
		t.Helper()
		for _, b := range [][]byte{cfg, layer, m} {
			if _, err := e.content.Size(ctx, digestOf(b)); err == nil {
				t.Fatalf("%s: %s was stored", what, digestOf(b))
			}
		}
		if is := e.list(e.a, "GET", "/v1.47/images/json"); len(is) != 0 {
			t.Fatalf("%s: images %v", what, is)
		}
	}
	cases := []struct {
		name, want string
		entry      func(string, []byte) (string, []byte)
	}{
		{"mismatched digest", "does not match its digest", func(n string, b []byte) (string, []byte) {
			if n == blob(layer) {
				return n, testLayer(map[string]string{"x": "evil"})
			}
			return n, b
		}},
		{"path traversal", "outside the archive", func(n string, b []byte) (string, []byte) {
			if n == blob(cfg) {
				return "blobs/sha256/../../../etc/passwd", b
			}
			return n, b
		}},
		{"absolute path", "outside the archive", func(n string, b []byte) (string, []byte) {
			if n == "oci-layout" {
				return "/etc/oci-layout", b
			}
			return n, b
		}},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, n := range order {
			name, b := c.entry(n, good[n])
			tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(b))})
			tw.Write(b)
		}
		tw.Close()
		code, out := e.send(e.a, "POST", "/v1.47/images/load", buf.Bytes())
		if code != 400 || !strings.Contains(string(out), c.want) {
			t.Errorf("%s: %d %s", c.name, code, out)
		}
		nothing(c.name)
	}

	// Past the silo's free disk: ci's disk_gb, all of it charged.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, n := range order {
		tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(good[n]))})
		tw.Write(good[n])
	}
	tw.Close()
	free, _ := e.facade.freeDisk(ctx, e.ci)
	err := e.db.Tx(ctx, func(tx store.Tx) error {
		return ledger.Charge(ctx, tx, ledger.ChargeRequest{Object: "test/fill", Silo: e.ci, Via: ledger.ViaFacade,
			Amounts: []ledger.Amount{{Key: Key(ClassDiskGB), Amount: free * ledger.Micro >> 30}}})
	})
	if err != nil {
		t.Fatal(err)
	}
	if free, _ := e.facade.freeDisk(ctx, e.ci); free != 0 {
		t.Fatalf("ci still has %d bytes free", free)
	}
	code, out := e.send(e.ci, "POST", "/v1.47/images/load", buf.Bytes())
	if code != 403 || !strings.Contains(string(out), "free disk_gb") {
		t.Errorf("oversize: %d %s", code, out)
	}
	nothing("oversize")
}

// commit of a sandbox container that created, changed and deleted
// files gives an image whose new layer has those three changes; vm is
// 501.
func TestCommit(t *testing.T) {
	e := newEnv(t)
	id := e.create(e.a, "work", obj{"HostConfig": obj{"Runtime": "runsc"}})
	e.ok(e.a, "POST", "/v1.47/containers/"+id+"/start", nil)
	var in bytes.Buffer
	tw := tar.NewWriter(&in)
	for n, b := range map[string]string{"new": "created", "conf": "changed"} {
		tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(b))})
		tw.Write([]byte(b))
	}
	tw.Close()
	if err := e.sbx.CopyIn(ctx, id, "/etc", &in); err != nil {
		t.Fatal(err)
	}
	if err := e.sbx.Remove(id, "/bin/app"); err != nil {
		t.Fatal(err)
	}
	out := e.ok(e.a, "POST", "/v1.47/commit?container=work&repo=committed&tag=1&changes="+
		"CMD%20%5B%22%2Fetc%2Fnew%22%5D&changes=ENV%20B%3D2", nil)
	img := e.ok(e.a, "GET", "/v1.47/images/committed:1/json", nil)
	if img["Id"] != out["Id"] {
		t.Fatalf("commit %v, inspect %v", out, img["Id"])
	}
	cfg := img["Config"].(map[string]any)
	if c := cfg["Cmd"].([]any); len(c) != 1 || c[0] != "/etc/new" || !slices.Contains(cfg["Env"].([]any), any("B=2")) {
		t.Fatalf("config: %v", cfg)
	}
	if cs, _ := e.sbx.List(ctx); len(cs) != 1 || cs[0].State != runtime.Running {
		t.Fatalf("after commit: %v", cs)
	}
	// The new layer: the created and changed files, and the deletion as
	// OCI's whiteout.
	var rec *ImageRecord
	e.db.Tx(ctx, func(tx store.Tx) error {
		var err error
		rec, err = row[ImageRecord](ctx, tx, e.a, kindImage, imageKey(out["Id"].(string)))
		return err
	})
	m, _, err := e.facade.manifestOf(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Layers) != 2 {
		t.Fatalf("layers: %v", m.Layers)
	}
	b, err := e.facade.readBlob(ctx, m.Layers[1].Digest, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		c, _ := io.ReadAll(tr)
		got[h.Name] = string(c)
		if h.Typeflag != tar.TypeReg {
			got[h.Name] = "type " + string(h.Typeflag)
		}
	}
	want := map[string]string{"etc/new": "created", "etc/conf": "changed", "bin/.wh.app": ""}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("layer: %s is %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("layer: %v, want %v", got, want)
	}

	// The commit's image runs, and pushes.
	e.ok(e.a, "POST", "/v1.47/containers/create?name=again", obj{"Image": "committed:1", "HostConfig": obj{"Runtime": "runsc"}})
	e.ok(e.a, "POST", "/v1.47/images/committed:1/tag?repo=registry.silo.internal/team-a/committed&tag=1", nil)
	if code, b := e.send(e.a, "POST", "/v1.47/images/registry.silo.internal/team-a/committed/push?tag=1", nil); code != 200 {
		t.Fatalf("push the commit: %d %s", code, b)
	}

	vm := e.create(e.b, "vm", obj{"HostConfig": obj{"Runtime": "vm"}})
	e.fail(501, "silo: commit of a vm container is not served: its writable layer is a guest disk", e.b, "POST", "/v1.47/commit?container="+vm, nil)
}

// The whiteout check: overlayfs's whiteouts and opaque directories
// become OCI's, and a device or a path outside the root is refused.
func TestWhiteoutLayer(t *testing.T) {
	mk := func(hs ...*tar.Header) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, h := range hs {
			tw.WriteHeader(h)
		}
		tw.Close()
		return buf.Bytes()
	}
	var out bytes.Buffer
	err := whiteoutLayer(&out, bytes.NewReader(mk(
		&tar.Header{Name: "./a/gone", Typeflag: tar.TypeChar},
		&tar.Header{Name: "./opq", Typeflag: tar.TypeDir, Mode: 0o755, PAXRecords: map[string]string{"SCHILY.xattr.trusted.overlay.opaque": "y"}},
	)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	tr := tar.NewReader(&out)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
		if len(h.PAXRecords) > 0 {
			t.Errorf("%s keeps %v", h.Name, h.PAXRecords)
		}
	}
	if want := []string{"a/.wh.gone", "opq/", "opq/.wh..wh..opq"}; !slices.Equal(names, want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for _, h := range []*tar.Header{
		{Name: "dev/sda", Typeflag: tar.TypeBlock, Devmajor: 8},
		{Name: "dev/null", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3},
		{Name: "../escape", Typeflag: tar.TypeReg},
		{Name: "x", Typeflag: tar.TypeLink, Linkname: "../../etc/shadow"},
	} {
		if err := whiteoutLayer(io.Discard, bytes.NewReader(mk(h))); err == nil {
			t.Errorf("%s was not refused", h.Name)
		}
	}
}

// docker login validates and stores nothing; build is still 501 and
// names the way that works.
func TestAuthAndBuild(t *testing.T) {
	e := newEnv(t)
	got := e.ok(e.a, "POST", "/v1.47/auth", obj{"username": "x", "password": "y", "serveraddress": "registry.silo.internal"})
	if got["Status"] != "Login Succeeded" {
		t.Fatalf("local login: %v", got)
	}
	if got := e.ok(e.a, "POST", "/v1.47/auth", obj{"username": "good", "password": "creds", "serveraddress": "https://index.docker.io/v1/"}); got["Status"] != "Login Succeeded" {
		t.Fatalf("good login: %v", got)
	}
	e.fail(401, "unauthorized", e.a, "POST", "/v1.47/auth", obj{"username": "good", "password": "wrong", "serveraddress": "ghcr.io"})
	// It stores nothing: no row and no audit record names the
	// credential.
	e.db.Tx(ctx, func(tx store.Tx) error {
		for _, table := range []string{"audit", "docker_objects"} {
			rows, err := tx.Query(ctx, `SELECT * FROM `+table)
			if err != nil {
				return err
			}
			cols, _ := rows.Columns()
			for rows.Next() {
				vals := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				rows.Scan(ptrs...)
				for _, v := range vals {
					s, _ := v.(string)
					if b, ok := v.([]byte); ok {
						s = string(b)
					}
					if strings.Contains(s, "creds") || strings.Contains(s, "wrong") {
						t.Errorf("%s holds the credential: %s", table, s)
					}
				}
			}
			rows.Close()
		}
		return nil
	})
	e.fail(501, "silo: image builds are not served by the facade; build in a container with kaniko or BuildKit and push to registry.silo.internal",
		e.a, "POST", "/v1.47/build", nil)
	e.fail(501, "kaniko or BuildKit", e.a, "POST", "/v1.47/session", nil)
}

// tarNames are an archive's entry names.
func tarNames(t *testing.T, b []byte) []string {
	t.Helper()
	var out []string
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, h.Name)
	}
}

// rewrite copies an archive's regular files through fn, which may
// rename, change or (keep false) drop each.
func rewrite(t *testing.T, b []byte, fn func(string, []byte) (string, []byte, bool)) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		c, _ := io.ReadAll(tr)
		name, c, keep := fn(h.Name, c)
		if !keep {
			continue
		}
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(c)), ModTime: time.Unix(0, 0)})
		tw.Write(c)
	}
	tw.Close()
	return buf.Bytes()
}
