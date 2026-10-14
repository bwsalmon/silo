// Package docker holds the conformance suite's Docker facade cases
// (local-backend.md §13, call L24). This file has the registry's, the
// facade list of local-registry.md §12: push, pull and run from the
// same silo and from an importer, a push elsewhere, a local pull
// without egress, save and load and load's refusals, commit, login and
// build. They run against every target whose registry has a Docker
// facade: local/fake in the go job, and local/sandbox and local/vm
// where they are installed (§13 step 36).
package docker_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/conformance"
	"github.com/bwsalmon/silo/pkg/conformance/targets"
)

type obj = map[string]any

const v = "/v1.47"

func each(t *testing.T, fn func(t *testing.T, r conformance.R)) {
	conformance.ForEach(t, targets.All(), func(t *testing.T, reg conformance.Registry) {
		fn(t, conformance.R{Registry: reg, T: t})
	})
}

// ok makes a facade call that must answer 2xx.
func ok(r conformance.R, caller, method, path string, body any) conformance.Resp {
	r.T.Helper()
	res := r.Dock(caller, method, path, body, nil)
	if res.Code/100 != 2 {
		r.T.Fatalf("%s %s as %s: %d %s", method, path, caller, res.Code, res.Body)
	}
	return res
}

// fails checks a facade call answers code with want in its message.
func fails(r conformance.R, code int, want, caller, method, path string, body any, hdr map[string]string) {
	r.T.Helper()
	res := r.Dock(caller, method, path, body, hdr)
	var out struct{ Message string }
	json.Unmarshal(res.Body, &out)
	if res.Code != code || !strings.Contains(out.Message+string(res.Body), want) {
		r.T.Errorf("%s %s as %s: %d %s, want %d with %q", method, path, caller, res.Code, res.Body, code, want)
	}
}

// pulled is a pull's progress stream, which must not end in an error.
func pulled(r conformance.R, caller, ref string, hdr map[string]string) {
	r.T.Helper()
	res := r.Dock(caller, "POST", v+"/images/create?fromImage="+ref, nil, hdr)
	if res.Code != 200 || strings.Contains(string(res.Body), `"error"`) {
		r.T.Fatalf("pull %s as %s: %d %s", ref, caller, res.Code, res.Body)
	}
}

// run creates and starts a container of image as caller.
func run(r conformance.R, caller, name, image string) {
	r.T.Helper()
	var out struct{ Id string }
	json.Unmarshal(ok(r, caller, "POST", v+"/containers/create?name="+name, obj{"Image": image, "Cmd": []string{"/bin/app"}}).Body, &out)
	ok(r, caller, "POST", v+"/containers/"+out.Id+"/start", nil)
}

// push tags caller's alpine:3 as registry.silo.internal/<repo>:tag and
// pushes it, returning the digest the progress stream's aux reports.
func push(r conformance.R, caller, repo, tag string) string {
	r.T.Helper()
	pulled(r, caller, "alpine:3", nil)
	ok(r, caller, "POST", v+"/images/alpine:3/tag?repo=registry.silo.internal/"+repo+"&tag="+tag, nil)
	res := r.Dock(caller, "POST", v+"/images/registry.silo.internal/"+repo+"/push?tag="+tag, nil, nil)
	if res.Code != 200 {
		r.T.Fatalf("push %s:%s: %d %s", repo, tag, res.Code, res.Body)
	}
	var last struct {
		Aux struct {
			Tag, Digest string
			Size        int64
		}
	}
	ls := strings.Split(strings.TrimSpace(string(res.Body)), "\n")
	json.Unmarshal([]byte(ls[len(ls)-1]), &last)
	if last.Aux.Tag != tag || !strings.HasPrefix(last.Aux.Digest, "sha256:") || last.Aux.Size == 0 {
		r.T.Fatalf("push %s:%s: the stream ends %s", repo, tag, ls[len(ls)-1])
	}
	return last.Aux.Digest
}

func silos(r conformance.R) (a, b, c string) {
	all := map[string]any{"allow": conformance.Hosts}
	return r.Mk("", "team-a", obj{"egress": all}), r.Mk("", "team-b", obj{"egress": all}), r.Mk("", "team-c", obj{"egress": all})
}

// docker tag and docker push to registry.silo.internal/<own path>/app,
// audited as image.push and registry.push, then docker pull and docker
// run of it from another container of the same silo and from an
// importer. A sibling gets manifest unknown, as for a repo that doesn't
// exist, and neither it nor the importer may push to it.
func TestDockerPushPullRun(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		a, b, c := silos(r)
		digest := push(r, a, "team-a/app", "1")
		if p := r.Last("image.push"); p["_silo"] != a || p["digest"] != digest {
			t.Errorf("image.push: %v", p)
		}
		if p := r.Last("registry.push"); p["digest"] != digest {
			t.Errorf("registry.push: %v", p)
		}
		var inspect struct{ RepoDigests []string }
		json.Unmarshal(ok(r, a, "GET", v+"/images/alpine:3/json", nil).Body, &inspect)
		if !slices.Contains(inspect.RepoDigests, "registry.silo.internal/team-a/app@"+digest) {
			t.Errorf("RepoDigests: %v", inspect.RepoDigests)
		}
		ok(r, a, "DELETE", v+"/images/registry.silo.internal/team-a/app:1", nil)
		pulled(r, a, "registry.silo.internal/team-a/app:1", nil)
		run(r, a, "from-repo", "registry.silo.internal/team-a/app:1")

		fails(r, 404, "manifest unknown", c, "POST", v+"/images/create?fromImage=registry.silo.internal/team-a/app&tag=1", nil, nil)
		fails(r, 404, "manifest unknown", c, "POST", v+"/images/create?fromImage=registry.silo.internal/team-a/nothing&tag=1", nil, nil)
		pushTo := func(caller string) {
			t.Helper()
			pulled(r, caller, "alpine:3", nil)
			ok(r, caller, "POST", v+"/images/alpine:3/tag?repo=registry.silo.internal/team-a/app&tag=2", nil)
			fails(r, 404, "name unknown", caller, "POST", v+"/images/registry.silo.internal/team-a/app/push?tag=2", nil, nil)
		}
		pushTo(c)

		r.OK(a, "POST", "/silo/v1/self/exports", obj{"name": "imgs", "protocol": "registry", "repos": []string{"app"}, "to": []string{"/team-b"}})
		r.OK(b, "POST", "/silo/v1/self/imports", obj{"export": "/team-a/imgs"})
		pulled(r, b, "registry.silo.internal/team-a/app:1", nil)
		run(r, b, "imported", "registry.silo.internal/team-a/app:1")
		pushTo(b)
	})
}

// A push to any other registry is 501 with the message.
func TestDockerPushElsewhere(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		a, _, _ := silos(r)
		pulled(r, a, "alpine:3", nil)
		for _, name := range []string{"ghcr.io/o/r", "alpine", "docker.io/library/alpine", "registry.example.com/x"} {
			fails(r, 501, "silo: pushes go to registry.silo.internal only; this host copies nothing to external registries", a, "POST", v+"/images/"+name+"/push?tag=latest", nil, nil)
		}
	})
}

// A pull from a local repo in a silo with an empty egress policy works,
// by path and by id, and a pull from the upstream doesn't.
func TestDockerLocalPullWithoutEgress(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		a, _, _ := silos(r)
		push(r, a, "team-a/app", "1")
		r.OK(r.Root(), "PATCH", "/silo/v1/silos/team-a", obj{"egress": obj{"allow": []string{}}})
		ok(r, a, "DELETE", v+"/images/alpine:3", nil)
		res := r.Dock(a, "POST", v+"/images/create?fromImage=alpine:3", nil, nil)
		if res.Code == 200 && !strings.Contains(string(res.Body), "egress") {
			t.Errorf("an upstream pull with empty egress: %d %s", res.Code, res.Body)
		}
		pulled(r, a, "registry.silo.internal/team-a/app:1", nil)
		pulled(r, a, "registry.silo.internal/id_"+a+"/app:1", nil)
	})
}

// docker save then docker load round-trips an image with the same
// digests, in both archive forms (OCI's index.json and the legacy
// manifest.json alone), and a save after the load is the same bytes.
func TestDockerSaveLoad(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		a, b, c := silos(r)
		pulled(r, a, "alpine:3", nil)
		ok(r, a, "POST", v+"/images/alpine:3/tag?repo=mine&tag=v1", nil)
		var before, after struct{ Id string }
		json.Unmarshal(ok(r, a, "GET", v+"/images/mine:v1/json", nil).Body, &before)
		archive := ok(r, a, "GET", v+"/images/get?names=mine:v1", nil).Body
		names := tarFiles(t, archive)
		for _, n := range []string{"index.json", "manifest.json", "oci-layout"} {
			if _, ok := names[n]; !ok {
				t.Fatalf("save has no %s", n)
			}
		}
		if res := load(r, b, archive); res.Code != 200 || !strings.Contains(string(res.Body), "Loaded image: mine:v1") {
			t.Fatalf("load: %d %s", res.Code, res.Body)
		}
		json.Unmarshal(ok(r, b, "GET", v+"/images/mine:v1/json", nil).Body, &after)
		if after.Id != before.Id {
			t.Errorf("load gave %s, saved %s", after.Id, before.Id)
		}
		if again := ok(r, b, "GET", v+"/images/mine:v1/get", nil).Body; !bytes.Equal(again, archive) {
			t.Error("a save after the load differs from the first save")
		}

		// The legacy form alone, manifest.json and its paths.
		var legacy []entry
		for _, e := range tarEntries(t, archive) {
			if e.name != "index.json" && e.name != "oci-layout" {
				legacy = append(legacy, e)
			}
		}
		if res := load(r, c, mkTar(legacy)); res.Code != 200 || !strings.Contains(string(res.Body), "Loaded image: mine:v1") {
			t.Fatalf("legacy load: %d %s", res.Code, res.Body)
		}
		json.Unmarshal(ok(r, c, "GET", v+"/images/mine:v1/json", nil).Body, &after)
		if after.Id != before.Id {
			t.Errorf("legacy load gave %s, saved %s", after.Id, before.Id)
		}
	})
}

// docker load of an archive with a mismatched digest, a path traversal
// entry, an absolute path, or more bytes than the silo's free disk_gb
// is refused and loads nothing.
func TestDockerLoadRefusals(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		a, _, _ := silos(r)
		cfg := []byte(`{"created":"2026-01-01T00:00:00Z","os":"linux","config":{"Cmd":["/x"]},"rootfs":{"type":"layers","diff_ids":[]}}`)
		layer := mkTar([]entry{{"x", []byte("new")}})
		desc := func(mt string, b []byte) obj {
			return obj{"mediaType": mt, "digest": conformance.Digest(b), "size": len(b)}
		}
		m, _ := json.Marshal(obj{"schemaVersion": 2, "mediaType": conformance.MediaOCIManifest,
			"config": desc("application/vnd.oci.image.config.v1+json", cfg),
			"layers": []obj{desc("application/vnd.oci.image.layer.v1.tar", layer)}})
		md := desc(conformance.MediaOCIManifest, m)
		md["annotations"] = obj{"io.containerd.image.name": "evil:1"}
		idx, _ := json.Marshal(obj{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": []obj{md}})
		blob := func(b []byte) string { return "blobs/sha256/" + strings.TrimPrefix(conformance.Digest(b), "sha256:") }
		good := []entry{{"oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)}, {blob(cfg), cfg}, {blob(layer), layer}, {blob(m), m}, {"index.json", idx}}
		with := func(change func(e entry) entry) []byte {
			var out []entry
			for _, e := range good {
				out = append(out, change(e))
			}
			return mkTar(out)
		}
		// Nothing is loaded, and, where the target shows its content
		// store, nothing of the archive is stored.
		nothing := func(caller, what string) {
			t.Helper()
			if res := r.Dock(caller, "GET", v+"/images/evil:1/json", nil, nil); res.Code != 404 {
				t.Errorf("%s: a refused load wrote the image: %d", what, res.Code)
			}
			if s, ok := r.Registry.(conformance.Structural); ok {
				for _, b := range [][]byte{cfg, layer, m} {
					if s.HasBlob(conformance.Digest(b)) {
						t.Errorf("%s: %s was stored", what, conformance.Digest(b))
					}
				}
			}
		}
		for _, c := range []struct {
			name, want string
			change     func(e entry) entry
		}{
			{"mismatched digest", "does not match its digest", func(e entry) entry {
				if e.name == blob(layer) {
					e.body = mkTar([]entry{{"x", []byte("evil")}})
				}
				return e
			}},
			{"path traversal", "outside the archive", func(e entry) entry {
				if e.name == blob(cfg) {
					e.name = "blobs/sha256/../../../etc/passwd"
				}
				return e
			}},
			{"absolute path", "outside the archive", func(e entry) entry {
				if e.name == "oci-layout" {
					e.name = "/etc/oci-layout"
				}
				return e
			}},
		} {
			if res := load(r, a, with(c.change)); res.Code != 400 || !strings.Contains(string(res.Body), c.want) {
				t.Errorf("%s: %d %s", c.name, res.Code, res.Body)
			}
			nothing(a, c.name)
		}
		// A silo with no disk_gb left.
		full := r.Mk("", "team-full", obj{"quota": obj{"global": obj{"vcpu_m": 400, "memory_mb": 400, "disk_gb": 0,
			"containers": 1, "networks": 1, "volumes": 1, "image_repos": 1}}})
		if res := load(r, full, mkTar(good)); res.Code != 403 || !strings.Contains(string(res.Body), "free disk_gb") {
			t.Errorf("past free disk_gb: %d %s", res.Code, res.Body)
		}
		nothing(full, "past free disk_gb")
	})
}

// docker commit of a sandbox container gives an image whose new layer
// holds the files written to it, with the commit's changes in its
// config; it leaves the container running, and the image runs and
// pushes. (Deletions as whiteouts are the facade's own TestWhiteoutLayer
// and the gVisor driver's.) A vm container's commit is 501.
func TestDockerCommit(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		a, _, _ := silos(r)
		pulled(r, a, "alpine:3", nil)
		var c struct{ Id string }
		json.Unmarshal(ok(r, a, "POST", v+"/containers/create?name=work", obj{"Image": "alpine:3", "Cmd": []string{"/bin/app"},
			"HostConfig": obj{"Runtime": "silo-sandbox"}}).Body, &c)
		ok(r, a, "POST", v+"/containers/"+c.Id+"/start", nil)
		files := mkTar([]entry{{"new", []byte("created")}, {"conf", []byte("changed")}})
		if res := raw(r, a, "PUT", v+"/containers/"+c.Id+"/archive?path=/etc", files); res.Code != 200 {
			t.Fatalf("copying files in: %d %s", res.Code, res.Body)
		}
		var out struct{ Id string }
		json.Unmarshal(ok(r, a, "POST", v+"/commit?container=work&repo=committed&tag=1&changes="+
			"CMD%20%5B%22%2Fetc%2Fnew%22%5D&changes=ENV%20B%3D2", nil).Body, &out)
		var img struct {
			Id     string
			Config struct{ Cmd, Env []string }
		}
		json.Unmarshal(ok(r, a, "GET", v+"/images/committed:1/json", nil).Body, &img)
		if img.Id != out.Id || !slices.Equal(img.Config.Cmd, []string{"/etc/new"}) || !slices.Contains(img.Config.Env, "B=2") {
			t.Fatalf("commit %s, inspect %+v", out.Id, img)
		}
		var state struct{ State struct{ Running bool } }
		json.Unmarshal(ok(r, a, "GET", v+"/containers/work/json", nil).Body, &state)
		if !state.State.Running {
			t.Error("the commit stopped the container")
		}
		got := map[string]string{}
		for _, e := range tarEntries(t, lastLayer(t, ok(r, a, "GET", v+"/images/committed:1/get", nil).Body)) {
			got[e.name] = string(e.body)
		}
		if got["etc/new"] != "created" || got["etc/conf"] != "changed" {
			t.Errorf("the new layer: %v", got)
		}

		// Each container takes much of the silo's disk_gb: one at a time.
		ok(r, a, "DELETE", v+"/containers/work?force=1", nil)
		run(r, a, "again", "committed:1")
		ok(r, a, "DELETE", v+"/containers/again?force=1", nil)
		ok(r, a, "POST", v+"/images/committed:1/tag?repo=registry.silo.internal/team-a/committed&tag=1", nil)
		ok(r, a, "POST", v+"/images/registry.silo.internal/team-a/committed/push?tag=1", nil)

		ok(r, a, "POST", v+"/containers/create?name=vm1", obj{"Image": "alpine:3", "Cmd": []string{"/bin/app"}, "HostConfig": obj{"Runtime": "silo-vm"}})
		fails(r, 501, "silo: commit of a vm container is not served: its writable layer is a guest disk", a, "POST", v+"/commit?container=vm1&repo=x&tag=1", nil, nil)
	})
}

// docker build and its session are still 501, and the message names
// kaniko and BuildKit.
func TestDockerBuild(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		a, _, _ := silos(r)
		fails(r, 501, "silo: image builds are not served by the facade; build in a container with kaniko or BuildKit and push to registry.silo.internal",
			a, "POST", v+"/build", nil, nil)
		fails(r, 501, "kaniko or BuildKit", a, "POST", v+"/session", nil, nil)
	})
}

// docker login to registry.silo.internal succeeds without looking; to
// an upstream, good and bad credentials give Login Succeeded and 401,
// through the token service and as the caller, so a registry its
// egress doesn't allow is 403. Nothing is stored: no record holds the
// password.
func TestDockerLogin(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		a, _, _ := silos(r)
		up := r.Upstream()
		login := func(user, pass, server string) string {
			t.Helper()
			var got struct{ Status string }
			json.Unmarshal(ok(r, a, "POST", v+"/auth", obj{"username": user, "password": pass, "serveraddress": server}).Body, &got)
			return got.Status
		}
		if got := login("x", up.Pass+"-local", "registry.silo.internal"); got != "Login Succeeded" {
			t.Errorf("local login: %q", got)
		}
		if got := login(up.User, up.Pass, "https://index.docker.io/v1/"); got != "Login Succeeded" {
			t.Errorf("good login: %q", got)
		}
		fails(r, 401, "unauthorized", a, "POST", v+"/auth", obj{"username": up.User, "password": up.Pass + "-wrong", "serveraddress": "docker.io"}, nil)
		fails(r, 403, "egress", a, "POST", v+"/auth", obj{"username": up.User, "password": up.Pass, "serveraddress": "ghcr.io"}, nil)
		for _, kind := range []string{"facade.call", "registry.cache", "registry.pull", "registry.denied", "docker.auth"} {
			b, _ := json.Marshal(r.Audit(kind))
			if strings.Contains(string(b), up.Pass) {
				t.Errorf("a %s record holds the password", kind)
			}
		}
	})
}

// docker pull of a private upstream image with X-Registry-Auth works,
// and without it fails with Docker's unauthorized.
func TestDockerPrivatePull(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		a, b, _ := silos(r)
		up := r.Upstream()
		auth, _ := json.Marshal(obj{"username": up.User, "password": up.Pass, "serveraddress": "docker.io"})
		pulled(r, a, "private/app:1", map[string]string{"X-Registry-Auth": base64.URLEncoding.EncodeToString(auth)})
		fails(r, 401, "unauthorized", b, "POST", v+"/images/create?fromImage=private/app:1", nil, nil)
	})
}

// load posts archive to docker load as caller.
func load(r conformance.R, caller string, archive []byte) conformance.Resp {
	r.T.Helper()
	return raw(r, caller, "POST", v+"/images/load", archive)
}

// raw makes a facade call with a tar body.
func raw(r conformance.R, caller, method, path string, body []byte) conformance.Resp {
	r.T.Helper()
	req, _ := http.NewRequest(method, "http://docker.silo.internal"+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-tar")
	res, err := r.Docker(caller, req)
	return readResp(r.T, res, err)
}

// entry is one regular file of a tar archive.
type entry struct {
	name string
	body []byte
}

// mkTar is an archive of entries, in order.
func mkTar(entries []entry) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		tw.WriteHeader(&tar.Header{Name: e.name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(e.body)), ModTime: time.Unix(0, 0)})
		tw.Write(e.body)
	}
	tw.Close()
	return buf.Bytes()
}

// tarEntries are an archive's regular files, in order.
func tarEntries(t testing.TB, b []byte) []entry {
	t.Helper()
	var out []entry
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			c, _ := io.ReadAll(tr)
			out = append(out, entry{h.Name, c})
		}
	}
}

// tarFiles are an archive's regular files by name.
func tarFiles(t testing.TB, b []byte) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, e := range tarEntries(t, b) {
		out[e.name] = e.body
	}
	return out
}

// lastLayer is the top layer, uncompressed, of the one image of a docker
// save archive, found through its index.json.
func lastLayer(t testing.TB, archive []byte) []byte {
	t.Helper()
	files := tarFiles(t, archive)
	blob := func(d string) []byte {
		b, ok := files["blobs/sha256/"+strings.TrimPrefix(d, "sha256:")]
		if !ok {
			t.Fatalf("the archive has no blob %s", d)
		}
		return b
	}
	var idx struct{ Manifests []struct{ Digest string } }
	var m struct {
		Layers []struct{ MediaType, Digest string }
	}
	json.Unmarshal(files["index.json"], &idx)
	if len(idx.Manifests) != 1 {
		t.Fatalf("index.json: %s", files["index.json"])
	}
	json.Unmarshal(blob(idx.Manifests[0].Digest), &m)
	if len(m.Layers) == 0 {
		t.Fatal("the image has no layers")
	}
	l := m.Layers[len(m.Layers)-1]
	b := blob(l.Digest)
	if strings.HasSuffix(l.MediaType, "gzip") {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		if b, err = io.ReadAll(zr); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

func readResp(t *testing.T, res *http.Response, err error) conformance.Resp {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return conformance.Resp{Code: res.StatusCode, Header: res.Header, Body: b}
}
