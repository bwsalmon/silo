package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/netfence"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime/fake"
	"github.com/bwsalmon/silo/pkg/backend/local/storage"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/registry"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
)

var ctx = context.Background()

type obj = map[string]any

// images is a fake puller: every reference is a 300 MB image whose
// config runs /bin/app, stored in content (a config, one small layer
// and a manifest) so that push, save and commit can read it.
type images struct {
	pulls   []string
	content *registry.Memory
}

func (f *images) Pull(_ context.Context, silo, ref, _ string) (*Pulled, error) {
	if strings.HasPrefix(ref, "missing") {
		return nil, notFound("pull access denied for %s", ref)
	}
	f.pulls = append(f.pulls, silo+" "+ref)
	name, _, _ := strings.Cut(ref, ":")
	cfg := ImageConfig{Cmd: []string{"/bin/app"}, Env: []string{"PATH=/bin", "A=1"},
		ExposedPorts: map[string]struct{}{"80/tcp": {}}, Volumes: map[string]struct{}{"/data": {}}}
	layer := testLayer(map[string]string{"bin/app": "app of " + name, "etc/conf": "base"})
	cb, _ := json.Marshal(map[string]any{"created": time.Unix(1700000000, 0).UTC(), "architecture": goarch(), "os": "linux",
		"config": cfg, "rootfs": map[string]any{"type": "layers", "diff_ids": []string{digestOf(layer)}}})
	mb, _ := json.Marshal(imageManifest{SchemaVersion: 2, MediaType: mediaOCIManifest,
		Config: &descriptor{MediaType: mediaOCIConfig, Digest: digestOf(cb), Size: int64(len(cb))},
		Layers: []descriptor{{MediaType: mediaOCILayer, Digest: digestOf(layer), Size: int64(len(layer))}}})
	for _, b := range [][]byte{cb, layer, mb} {
		f.content.Put(b)
	}
	id := digestOf(cb)
	return &Pulled{ID: id, RepoDigest: name + "@" + digestOf(mb), Size: 300 << 20, Created: time.Unix(1700000000, 0),
		Config: cfg, Manifest: digestOf(mb), MediaType: mediaOCIManifest}, nil
}

// testLayer is a layer tar of files, by path.
func testLayer(files map[string]string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	var names []string
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(files[n])), ModTime: time.Unix(0, 0)})
		tw.Write([]byte(files[n]))
	}
	tw.Close()
	return buf.Bytes()
}

// env is silod on SQLite with a local tree, serving the fence API and
// the facade over fake drivers, with the caller from a Silo-Caller
// header. The tree: root, team-a (a child with quota), its child ci,
// team-b, ro (control=read) and sbx (sandbox only).
type env struct {
	t      *testing.T
	db     store.Database
	vm     *fake.Driver
	sbx    *fake.Driver
	images *images
	// content is silo-containerd's content store, and reg the local
	// registry over it.
	content *registry.Memory
	reg     *registry.Service
	facade  *Server
	srv    *httptest.Server
	vols   string

	root, a, ci, b, ro, sandboxOnly string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}
	caller := func(r *http.Request) (fenceapi.Caller, error) {
		return fenceapi.Caller{Silo: r.Header.Get("Silo-Caller"), Via: fenceapi.ViaInternal}, nil
	}
	content := registry.NewMemory()
	e := &env{t: t, db: db, vm: fake.New(runtime.VM), sbx: fake.New(runtime.Sandbox), images: &images{content: content},
		content: content, vols: t.TempDir()}
	e.vm.Volumes, e.sbx.Volumes = e.vols, e.vols
	api := fenceapi.New(db, fenceapi.Options{Caller: caller})
	e.reg = registry.New(registry.Options{Store: db, API: api, Content: content})
	e.facade = New(db, Options{Drivers: []runtime.Driver{e.vm, e.sbx}, Storage: &storage.Dir{Root: e.vols},
		Images: e.images, Content: content, Registry: e.reg, Login: login, Caller: caller, Version: "test"})
	mux := http.NewServeMux()
	mux.Handle("/silo/", api)
	mux.Handle("/silod/", api)
	mux.Handle("/", e.facade)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(func() { e.srv.Close(); e.facade.Close(); api.Close() })

	code, root := e.do("", "POST", "/silod/bootstrap", obj{
		"backing":  obj{"kind": "local"},
		"capacity":     obj{"global": obj{"vcpu_m": 32000, "memory_mb": 32768, "disk_gb": 1000, "containers": 50, "networks": 20, "volumes": 20, "image_repos": 100}},
		"grant_policy": obj{"exports": "allow"},
		"admins":       []string{"user:admin@example.com"},
	}, nil)
	if code != 200 {
		t.Fatalf("bootstrap: %d %v", code, root)
	}
	e.root = root["id"].(string)
	mk := func(parent, name string, extra obj) string {
		body := obj{"name": name, "address": obj{"prefix_length": 20}}
		for k, v := range extra {
			body[k] = v
		}
		code, out := e.do(e.root, "POST", "/silo/v1/silos/"+parent+"/children", body, nil)
		if code != 200 {
			t.Fatalf("create %s/%s: %d %v", parent, name, code, out)
		}
		return out["id"].(string)
	}
	quota := obj{"global": obj{"vcpu_m": 4000, "memory_mb": 2048, "disk_gb": 100, "containers": 5, "networks": 3, "volumes": 3, "image_repos": 5}}
	e.a = mk("-"+e.root, "team-a", obj{"quota": quota})
	e.ci = mk("team-a", "ci", obj{"address": obj{"prefix_length": 24}, "quota": obj{"global": obj{"containers": 1, "disk_gb": 30, "vcpu_m": 1000, "memory_mb": 512, "networks": 1, "volumes": 1}}})
	e.b = mk("-"+e.root, "team-b", obj{"quota": quota})
	e.ro = mk("-"+e.root, "ro", obj{"control": "read", "quota": quota})
	e.sandboxOnly = mk("-"+e.root, "sbx", obj{"quota": quota, "isolation": obj{"allowed": []string{"sandbox"}}})
	return e
}

// do makes a call as caller, with headers, and decodes a JSON answer.
func (e *env) do(caller, method, path string, body any, hdr map[string]string) (int, obj) {
	e.t.Helper()
	code, b, _ := e.raw(caller, method, path, body, hdr)
	out := obj{}
	if len(bytes.TrimSpace(b)) > 0 && b[0] == '{' {
		if err := json.Unmarshal(b, &out); err != nil {
			e.t.Fatalf("%s %s: %d, body %q is not JSON", method, path, code, b)
		}
	} else if len(b) > 0 {
		out["raw"] = string(b)
	}
	return code, out
}

func (e *env) raw(caller, method, path string, body any, hdr map[string]string) (int, []byte, http.Header) {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, r)
	if err != nil {
		e.t.Fatal(err)
	}
	if caller != "" {
		req.Header.Set("Silo-Caller", caller)
	}
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

// ok makes a call that must answer 2xx.
func (e *env) ok(caller, method, path string, body any) obj {
	e.t.Helper()
	code, out := e.do(caller, method, path, body, nil)
	if code/100 != 2 {
		e.t.Fatalf("%s %s: %d %v", method, path, code, out)
	}
	return out
}

// list makes a call that must answer a JSON list.
func (e *env) list(caller, method, path string) []any {
	e.t.Helper()
	code, b, _ := e.raw(caller, method, path, nil, nil)
	if code != 200 {
		e.t.Fatalf("%s %s: %d %s", method, path, code, b)
	}
	var out []any
	if err := json.Unmarshal(b, &out); err != nil {
		e.t.Fatalf("%s %s: %s is not a list", method, path, b)
	}
	return out
}

// fail makes a call that must fail with code, its message containing
// want.
func (e *env) fail(code int, want, caller, method, path string, body any) {
	e.t.Helper()
	got, out := e.do(caller, method, path, body, nil)
	msg, _ := out["message"].(string)
	if got != code || !strings.Contains(msg, want) {
		e.t.Errorf("%s %s: %d %q, want %d containing %q", method, path, got, msg, code, want)
	}
}

// used is silo's use of a local class, in whole units.
func (e *env) used(silo, class string) int64 {
	e.t.Helper()
	var r ledger.Row
	err := e.db.Tx(ctx, func(tx store.Tx) error {
		var err error
		r, err = ledger.Get(ctx, tx, silo, Key(class))
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return r.Used / ledger.Micro
}

func (e *env) pull(caller, ref string) {
	e.t.Helper()
	code, b, _ := e.raw(caller, "POST", "/v1.47/images/create?fromImage="+ref, nil, nil)
	if code != 200 {
		e.t.Fatalf("pull %s: %d %s", ref, code, b)
	}
}

// create pulls alpine and creates a container from body, returning its
// id.
func (e *env) create(caller, name string, body obj) string {
	e.t.Helper()
	e.pull(caller, "alpine")
	if body == nil {
		body = obj{}
	}
	if body["Image"] == nil {
		body["Image"] = "alpine"
	}
	out := e.ok(caller, "POST", "/v1.47/containers/create?name="+name, body)
	return out["Id"].(string)
}

func TestVersionsAndRoutes(t *testing.T) {
	e := newEnv(t)
	code, b, h := e.raw("", "GET", "/_ping", nil, nil)
	if code != 200 || string(b) != "OK" || h.Get("Api-Version") != "1.47" {
		t.Errorf("_ping: %d %q %v", code, b, h)
	}
	if code, _, _ := e.raw("", "HEAD", "/v1.43/_ping", nil, nil); code != 200 {
		t.Errorf("HEAD _ping: %d", code)
	}
	v := e.ok(e.a, "GET", "/version", nil)
	if v["ApiVersion"] != "1.47" || v["MinAPIVersion"] != "1.41" || v["Version"] != "test" {
		t.Errorf("version: %v", v)
	}
	e.fail(400, "client version 1.40 is too old. Minimum supported API version is 1.41", e.a, "GET", "/v1.40/info", nil)
	e.fail(400, "client version 1.48 is too new. Maximum supported API version is 1.47", e.a, "GET", "/v1.48/info", nil)
	e.ok(e.a, "GET", "/v1.41/info", nil)
	e.fail(404, "page not found", e.a, "GET", "/v1.47/nothing/here", nil)
	e.fail(404, "page not found", e.a, "PATCH", "/v1.47/containers/json", nil)
	e.fail(503, "This node is not a swarm manager", e.a, "GET", "/v1.47/swarm", nil)
	e.fail(503, "This node is not a swarm manager", e.a, "POST", "/v1.47/services/create", nil)
	e.fail(501, "build in a container with kaniko or BuildKit and push to registry.silo.internal", e.a, "POST", "/v1.47/build", nil)
	e.fail(501, "not supported", e.a, "POST", "/v1.47/containers/abc/update", nil)
	e.fail(501, "not supported", e.a, "GET", "/v1.47/containers/abc/export", nil)
	e.fail(501, "pushes go to registry.silo.internal only; this host copies nothing to external registries", e.a, "POST", "/v1.47/images/ghcr.io/o/r/push", nil)
	e.fail(501, "not supported", e.a, "GET", "/v1.47/plugins", nil)
	e.fail(501, "image import", e.a, "POST", "/v1.47/images/create?fromSrc=-", nil)
	e.fail(501, "not served by this facade yet", e.a, "POST", "/v1.47/containers/abc/exec", nil)
	// No caller is 403 on everything but /_ping.
	e.fail(403, "carries no caller", "", "GET", "/v1.47/info", nil)
}

func TestMatch(t *testing.T) {
	for _, c := range []struct {
		pattern, path string
		want          map[string]string
	}{
		{"/images/{name+}/json", "/images/ghcr.io/o/r:1/json", map[string]string{"name": "ghcr.io/o/r:1"}},
		{"/images/{name+}/json", "/images/alpine/json", map[string]string{"name": "alpine"}},
		{"/images/{name+}", "/images/a/b", map[string]string{"name": "a/b"}},
		{"/containers/{id}/start", "/containers/x/start", map[string]string{"id": "x"}},
		{"/containers/{id}/start", "/containers//start", nil},
		{"/images/{name+}/json", "/images/json", nil},
	} {
		got, ok := match(c.pattern, c.path, false)
		if ok != (c.want != nil) || (ok && fmt.Sprint(got) != fmt.Sprint(c.want)) {
			t.Errorf("match(%s, %s) = %v, %v; want %v", c.pattern, c.path, got, ok, c.want)
		}
	}
}

func TestContainerLifecycleAndCharges(t *testing.T) {
	e := newEnv(t)
	id := e.create(e.a, "web", obj{"Env": []string{"A=2"}, "HostConfig": obj{"Memory": 256 << 20, "NanoCpus": 500_000_000,
		"PortBindings": obj{"80/tcp": []obj{{"HostPort": "8080"}}}}})
	if len(id) != 64 {
		t.Fatalf("id %q", id)
	}
	// The image's 300 MB is one GiB, the container's writable layer ten,
	// and the anonymous volume for the image's /data ten.
	if got := e.used(e.a, ClassDiskGB); got != 21 {
		t.Errorf("disk_gb after create: %d, want 21 (image 1, layer 10, volume 10)", got)
	}
	// /data, the image's VOLUME, is an anonymous volume, charged too.
	if got := e.used(e.a, ClassVolumes); got != 1 {
		t.Errorf("volumes after create: %d, want 1", got)
	}
	if got := e.used(e.a, ClassContainers); got != 1 {
		t.Errorf("containers after create: %d", got)
	}
	if got := e.used(e.a, ClassVCPUm); got != 0 {
		t.Errorf("vcpu_m before start: %d", got)
	}
	spec, ok := e.vm.Spec(id)
	if !ok {
		t.Fatal("the vm driver has no container")
	}
	if strings.Join(spec.Process.Args, " ") != "/bin/app" || strings.Join(spec.Process.Env, ",") != "PATH=/bin,A=2" ||
		spec.Resources.MemoryMB != 256 || spec.Resources.VCPUm != 500 || spec.Labels[runtime.LabelSilo] != e.a ||
		spec.Labels[runtime.LabelName] != "web" || len(spec.Mounts) != 1 || !strings.HasPrefix(spec.Mounts[0].Source, e.vols) {
		t.Errorf("spec: %+v", spec)
	}

	e.ok(e.a, "POST", "/v1.47/containers/web/start", nil)
	if got := e.used(e.a, ClassMemoryMB); got != 256 {
		t.Errorf("memory_mb while running: %d", got)
	}
	if got := e.used(e.a, ClassVCPUm); got != 500 {
		t.Errorf("vcpu_m while running: %d", got)
	}
	if code, _, _ := e.raw(e.a, "POST", "/v1.47/containers/web/start", nil, nil); code != 304 {
		t.Errorf("second start: %d, want 304", code)
	}
	in := e.ok(e.a, "GET", "/v1.47/containers/"+id[:8]+"/json", nil)
	st := in["State"].(obj)
	if st["Status"] != "running" || in["Name"] != "/web" || in["HostConfig"].(obj)["Runtime"] != RuntimeVM {
		t.Errorf("inspect: %v", in)
	}
	ports := in["NetworkSettings"].(obj)["Ports"].(obj)["80/tcp"].([]any)[0].(obj)
	if ports["HostIp"] != "0.0.0.0" || ports["HostPort"] != "8080" {
		t.Errorf("ports: %v", ports)
	}
	if l := e.list(e.a, "GET", "/v1.47/containers/json"); len(l) != 1 {
		t.Errorf("list: %v", l)
	}
	// Names are the silo's own: team-b may have a web too, and doesn't
	// see team-a's.
	e.fail(404, "No such container: web", e.b, "GET", "/v1.47/containers/web/json", nil)
	e.create(e.b, "web", nil)

	e.ok(e.a, "POST", "/v1.47/containers/web/stop?t=1", nil)
	if got := e.used(e.a, ClassMemoryMB); got != 0 {
		t.Errorf("memory_mb after stop: %d", got)
	}
	code, b, _ := e.raw(e.a, "POST", "/v1.47/containers/web/wait", nil, nil)
	if code != 200 || !strings.Contains(string(b), `"StatusCode":143`) {
		t.Errorf("wait: %d %s", code, b)
	}
	e.fail(409, "is not running", e.a, "POST", "/v1.47/containers/web/kill", nil)
	if l := e.list(e.a, "GET", "/v1.47/containers/json"); len(l) != 0 {
		t.Errorf("list without all: %v", l)
	}
	if l := e.list(e.a, "GET", `/v1.47/containers/json?all=1&filters={"status":["exited"]}`); len(l) != 1 {
		t.Errorf("list exited: %v", l)
	}

	// Restart charges again; a delete of a running container needs force.
	e.ok(e.a, "POST", "/v1.47/containers/web/restart", nil)
	if got := e.used(e.a, ClassMemoryMB); got != 256 {
		t.Errorf("memory_mb after restart: %d", got)
	}
	e.fail(409, "You cannot remove a running container", e.a, "DELETE", "/v1.47/containers/web", nil)
	e.ok(e.a, "POST", "/v1.47/containers/web/rename?name=api", nil)
	e.ok(e.a, "DELETE", "/v1.47/containers/api?force=1&v=1", nil)
	for _, c := range []string{ClassMemoryMB, ClassVCPUm, ClassContainers, ClassVolumes} {
		if got := e.used(e.a, c); got != 0 {
			t.Errorf("%s after delete: %d", c, got)
		}
	}
	if got := e.used(e.a, ClassDiskGB); got != 1 {
		t.Errorf("disk_gb after delete: %d, want the image's 1", got)
	}
	if _, ok := e.vm.Spec(id); ok {
		t.Error("the driver still has the container")
	}
}

func TestStartOverQuota(t *testing.T) {
	e := newEnv(t)
	e.create(e.a, "big", obj{"HostConfig": obj{"Memory": 4096 << 20}})
	e.fail(409, "silo: quota exceeded", e.a, "POST", "/v1.47/containers/big/start", nil)
	if got := e.used(e.a, ClassMemoryMB); got != 0 {
		t.Errorf("memory_mb after a refused start: %d", got)
	}
	// ci may hold one container.
	e.create(e.ci, "one", obj{"HostConfig": obj{"StorageOpt": obj{"size": "5G"}}})
	e.fail(409, "quota exceeded", e.ci, "POST", "/v1.47/containers/create?name=two", obj{"Image": "alpine"})
}

func TestRefusals(t *testing.T) {
	e := newEnv(t)
	e.pull(e.a, "alpine")
	path := "/v1.47/containers/create"
	hc := func(h obj) obj { return obj{"Image": "alpine", "HostConfig": h} }
	for _, c := range []struct {
		code int
		want string
		body obj
	}{
		{403, "privileged container", hc(obj{"Privileged": true})},
		{403, "CapAdd", hc(obj{"CapAdd": []string{"NET_ADMIN"}})},
		{403, "security option", hc(obj{"SecurityOpt": []string{"seccomp=unconfined"}})},
		{403, "host devices", hc(obj{"Devices": []obj{{"PathOnHost": "/dev/kvm"}}})},
		{403, "host devices", hc(obj{"DeviceRequests": []obj{{"Count": -1}}})},
		{403, "NetworkMode host", hc(obj{"NetworkMode": "host"})},
		{403, "PidMode host", hc(obj{"PidMode": "host"})},
		{403, "another container's namespaces", hc(obj{"IpcMode": "container:abc"})},
		{403, "sysctls", hc(obj{"Sysctls": obj{"net.ipv4.ip_forward": "1"}})},
		{403, "OomScoreAdj", hc(obj{"OomScoreAdj": -500})},
		{403, "bind mounts of host paths are not allowed; use a volume", hc(obj{"Binds": []string{"/etc:/host-etc"}})},
		{403, "bind mounts of host paths", hc(obj{"Mounts": []obj{{"Type": "bind", "Source": "/", "Target": "/x"}}})},
		{403, "npipe", hc(obj{"Mounts": []obj{{"Type": "npipe", "Source": "x", "Target": "/x"}}})},
		{403, "volume driver", hc(obj{"VolumeDriver": "nfs"})},
		{403, "no host address", hc(obj{"PortBindings": obj{"80/tcp": []obj{{"HostIp": "127.0.0.1", "HostPort": "80"}}}})},
		{403, "logging driver", hc(obj{"LogConfig": obj{"Type": "syslog"}})},
		{403, "Silo's own namespace", obj{"Image": "alpine", "Labels": obj{"dev.silo.silo": "x"}}},
		{400, "field MacAddress is not allowed", obj{"Image": "alpine", "MacAddress": "02:42:00:00:00:01"}},
		{400, "field HostConfig.Dns is not allowed", hc(obj{"Dns": []string{"8.8.8.8"}})},
		{400, "unknown runtime", hc(obj{"Runtime": "runc-custom"})},
		{400, "can't be published", hc(obj{"PortBindings": obj{"80/tcp": []obj{{"HostPort": "2375"}}}})},
		{400, "EndpointsConfig.x.MacAddress", obj{"Image": "alpine", "NetworkingConfig": obj{"EndpointsConfig": obj{"x": obj{"MacAddress": "02:42:00:00:00:01"}}}}},
		{404, "No such image", obj{"Image": "nginx"}},
	} {
		e.fail(c.code, c.want, e.a, "POST", path, c.body)
	}
	// Defaults are not refused: what the docker CLI sends for a plain
	// run.
	e.ok(e.a, "POST", path, obj{"Image": "alpine", "MacAddress": "", "HostConfig": obj{"Privileged": false, "NetworkMode": "default",
		"Binds": []string{"cache:/cache:ro"}, "LogConfig": obj{"Type": "json-file", "Config": obj{}}, "Runtime": "runc",
		"SecurityOpt": []string{"no-new-privileges"}}})
	// The named volume was made, and charged.
	if got := e.used(e.a, ClassVolumes); got != 2 {
		t.Errorf("volumes: %d, want cache and the anonymous /data", got)
	}
	// Every refused write is audited.
	var n int
	err := e.db.Tx(ctx, func(tx store.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM audit WHERE silo = ? AND kind = 'facade.call' AND result <> 'OK'`, e.a).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 20 {
		t.Errorf("%d refusals audited", n)
	}
}

func TestTargetsAndControl(t *testing.T) {
	e := newEnv(t)
	e.create(e.ci, "job", nil)
	host := func(id string) map[string]string { return map[string]string{"Host": id + ".docker.silo.internal:2375"} }
	// team-a reaches its child ci by Host; team-b doesn't, with the same
	// 404 as for a silo that doesn't exist.
	code, b, _ := e.raw(e.a, "GET", "/v1.47/containers/json?all=1", nil, host(e.ci))
	if code != 200 || !strings.Contains(string(b), "/job") {
		t.Errorf("a on ci: %d %s", code, b)
	}
	if code, out := e.do(e.b, "GET", "/v1.47/containers/json", nil, host(e.ci)); code != 404 {
		t.Errorf("b on ci: %d %v", code, out)
	}
	if code, out := e.do(e.b, "GET", "/v1.47/containers/json", nil, host("nosuch")); code != 404 {
		t.Errorf("b on nosuch: %d %v", code, out)
	}
	// ci doesn't reach its parent.
	if code, _ := e.do(e.ci, "GET", "/v1.47/info", nil, host(e.a)); code != 404 {
		t.Errorf("ci on a: %d", code)
	}
	// A host socket's silo is the target, whatever Host says.
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(fenceapi.ContextWithCaller(ContextWithSocket(r.Context(), e.ci), fenceapi.Caller{Silo: e.root, Via: fenceapi.ViaFrontDoor}))
		e.facade.ServeHTTP(w, r)
	}))
	defer h.Close()
	resp, err := http.Get(h.URL + "/v1.47/containers/json?all=1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "/job") {
		t.Errorf("socket: %s", b)
	}
	// control=read reads, and writes nothing.
	e.ok(e.ro, "GET", "/v1.47/containers/json", nil)
	e.fail(403, "control policy is read", e.ro, "POST", "/v1.47/volumes/create", obj{"Name": "v"})
	code, b, _ = e.raw(e.ro, "POST", "/v1.47/containers/x/wait", nil, nil)
	if code != 404 {
		t.Errorf("wait, a read, as ro: %d %s", code, b)
	}
}

func TestClasses(t *testing.T) {
	e := newEnv(t)
	info := e.ok(e.a, "GET", "/v1.47/info", nil)
	if info["DefaultRuntime"] != RuntimeVM || info["OperatingSystem"] != OperatingSystem || info["NCPU"] != 4.0 ||
		info["MemTotal"] != float64(2048<<20) || info["Swarm"].(obj)["LocalNodeState"] != "inactive" || info["KernelVersion"] != "granule" {
		t.Errorf("info: %v", info)
	}
	if rs := info["Runtimes"].(obj); len(rs) != 2 {
		t.Errorf("runtimes: %v", rs)
	}
	id := e.create(e.a, "sx", obj{"HostConfig": obj{"Runtime": "runsc"}})
	if _, ok := e.sbx.Spec(id); !ok {
		t.Error("runsc didn't land in the sandbox class")
	}
	in := e.ok(e.a, "GET", "/v1.47/containers/sx/json", nil)
	if in["HostConfig"].(obj)["Runtime"] != RuntimeSandbox {
		t.Errorf("runtime shown: %v", in["HostConfig"].(obj)["Runtime"])
	}
	// sbx may run sandbox only, and that is its default.
	e.pull(e.sandboxOnly, "alpine")
	e.fail(403, `isolation class "vm" is not allowed in this silo (allowed: sandbox)`, e.sandboxOnly, "POST", "/v1.47/containers/create",
		obj{"Image": "alpine", "HostConfig": obj{"Runtime": "silo-vm"}})
	id = e.ok(e.sandboxOnly, "POST", "/v1.47/containers/create", obj{"Image": "alpine"})["Id"].(string)
	if _, ok := e.sbx.Spec(id); !ok {
		t.Error("sbx's default isn't sandbox")
	}
	if info := e.ok(e.sandboxOnly, "GET", "/v1.47/info", nil); info["DefaultRuntime"] != RuntimeSandbox || len(info["Runtimes"].(obj)) != 1 {
		t.Errorf("sbx info: %v", info)
	}
	// A class the host can't run is 409, and never a fallback.
	e.vm.SetProbe(runtime.Probe{Why: "no usable /dev/kvm"})
	e.fail(409, `this host cannot run isolation class "vm" (no usable /dev/kvm)`, e.a, "POST", "/v1.47/containers/create", obj{"Image": "alpine"})
	if info := e.ok(e.a, "GET", "/v1.47/info", nil); info["DefaultRuntime"] != "" || len(info["Runtimes"].(obj)) != 1 {
		t.Errorf("info without kvm: %v", info)
	}
}

func TestNetworks(t *testing.T) {
	e := newEnv(t)
	out := e.ok(e.a, "POST", "/v1.47/networks/create", obj{"Name": "app", "Labels": obj{"com.docker.compose.network": "app"}})
	nid := out["Id"].(string)
	n := e.ok(e.a, "GET", "/v1.47/networks/app", nil)
	cfg := n["IPAM"].(obj)["Config"].([]any)[0].(obj)
	subnet := netip.MustParsePrefix(cfg["Subnet"].(string))
	if subnet.Bits() != 24 || cfg["Gateway"] != subnet.Addr().Next().String() {
		t.Errorf("ipam: %v", cfg)
	}
	if got := e.used(e.a, ClassNetworks); got != 1 {
		t.Errorf("networks: %d", got)
	}
	e.fail(409, "already exists", e.a, "POST", "/v1.47/networks/create", obj{"Name": "app"})
	e.fail(403, "pre-defined network", e.a, "POST", "/v1.47/networks/create", obj{"Name": "host"})
	e.fail(400, "driver", e.a, "POST", "/v1.47/networks/create", obj{"Name": "o", "Driver": "overlay"})
	e.fail(400, "not inside the blocks", e.a, "POST", "/v1.47/networks/create", obj{"Name": "o", "IPAM": obj{"Config": []obj{{"Subnet": "192.168.0.0/24"}}}})
	e.fail(400, "first address", e.a, "POST", "/v1.47/networks/create", obj{"Name": "o", "IPAM": obj{"Config": []obj{{"Subnet": subnet.String(), "Gateway": "1.2.3.4"}}}})
	e.fail(400, "field Options is not allowed", e.a, "POST", "/v1.47/networks/create", obj{"Name": "o", "Options": obj{"com.docker.network.bridge.name": "br0"}})
	e.fail(400, "field EnableIPv6 is not allowed", e.a, "POST", "/v1.47/networks/create", obj{"Name": "o", "EnableIPv6": true})

	id := e.create(e.a, "db", obj{"HostConfig": obj{"NetworkMode": "app"},
		"NetworkingConfig": obj{"EndpointsConfig": obj{"app": obj{"Aliases": []string{"database"}}}}})
	web := e.create(e.a, "web", nil) // on the default bridge, made now
	if got := e.used(e.a, ClassNetworks); got != 2 {
		t.Errorf("networks with the default bridge: %d", got)
	}
	e.ok(e.a, "POST", "/v1.47/networks/app/connect", obj{"Container": "web", "EndpointConfig": obj{"IPAMConfig": obj{"IPv4Address": subnet.Addr().Next().Next().Next().Next().String()}}})
	e.fail(403, "already exists", e.a, "POST", "/v1.47/networks/app/connect", obj{"Container": "web"})
	n = e.ok(e.a, "GET", "/v1.47/networks/"+nid[:6], nil)
	if len(n["Containers"].(obj)) != 2 {
		t.Errorf("containers on app: %v", n["Containers"])
	}
	e.fail(403, "has active endpoints", e.a, "DELETE", "/v1.47/networks/app", nil)
	if l := e.list(e.a, "GET", `/v1.47/networks?filters={"label":["com.docker.compose.network=app"]}`); len(l) != 1 {
		t.Errorf("label filter: %v", l)
	}

	// The fence sees each network's bridge, and each endpoint pinned.
	var topo *topoView
	err := e.db.Tx(ctx, func(tx store.Tx) error {
		t, err := Topology(netip.MustParseAddr("10.255.255.1"), netip.MustParseAddr("10.255.255.2"))(ctx, tx)
		topo = &topoView{nets: len(t.Networks), ports: len(t.Ports)}
		for _, p := range t.Ports {
			if !subnetOf(t.Networks, p.Network).Contains(p.Addr) {
				topo.bad++
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if topo.nets != 2 || topo.ports != 3 || topo.bad != 0 {
		t.Errorf("topology: %+v", topo)
	}

	// DNS: names and aliases on shared networks only, scoped by origin.
	ans := func(silo, from, name string) string {
		var a Answer
		err := e.db.Tx(ctx, func(tx store.Tx) error {
			var err error
			a, err = e.facade.Lookup(ctx, tx, Names{API: netip.MustParseAddr("10.255.255.1"), Docker: netip.MustParseAddr("10.255.255.2")},
				silo, netip.MustParseAddr(from), name)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(a.Addrs)
	}
	dbIn := e.ok(e.a, "GET", "/v1.47/containers/"+id+"/json", nil)
	dbAddr := dbIn["NetworkSettings"].(obj)["Networks"].(obj)["app"].(obj)["IPAddress"].(string)
	webIn := e.ok(e.a, "GET", "/v1.47/containers/"+web+"/json", nil)
	webBridge := webIn["NetworkSettings"].(obj)["Networks"].(obj)["bridge"].(obj)["IPAddress"].(string)
	if got := ans(e.a, webBridge, "database"); got != "["+dbAddr+"]" {
		t.Errorf("web asks database: %s", got)
	}
	if got := ans(e.b, webBridge, "database"); got != "[]" {
		t.Errorf("team-b asks database: %s", got)
	}
	if got := ans(e.a, webBridge, "example.com"); got != "[]" {
		t.Errorf("external name: %s", got)
	}
	if got := ans(e.a, webBridge, e.ci+".docker.silo.internal"); got != "[10.255.255.2]" {
		t.Errorf("silo name: %s", got)
	}
	// The registry is silo.internal's address, for every silo
	// (local-registry.md, call L56).
	for _, silo := range []string{e.a, e.b} {
		if got := ans(silo, webBridge, "registry.silo.internal."); got != "[10.255.255.1]" {
			t.Errorf("registry.silo.internal: %s", got)
		}
	}

	e.ok(e.a, "POST", "/v1.47/networks/app/disconnect", obj{"Container": "web"})
	e.ok(e.a, "DELETE", "/v1.47/containers/db?force=1", nil)
	e.ok(e.a, "DELETE", "/v1.47/networks/app", nil)
	if got := e.used(e.a, ClassNetworks); got != 1 {
		t.Errorf("networks after delete: %d", got)
	}
	// The subnet is free again.
	e.ok(e.a, "POST", "/v1.47/networks/create", obj{"Name": "again", "IPAM": obj{"Config": []obj{{"Subnet": subnet.String()}}}})
}

type topoView struct{ nets, ports, bad int }

func subnetOf(ns []netfence.Network, id string) netip.Prefix {
	for _, n := range ns {
		if n.ID == id {
			return n.Subnet
		}
	}
	return netip.Prefix{}
}

func TestVolumes(t *testing.T) {
	e := newEnv(t)
	v := e.ok(e.a, "POST", "/v1.47/volumes/create", obj{"Name": "data", "DriverOpts": obj{"size": "2G"}, "Labels": obj{"x": "y"}})
	if v["Name"] != "data" || v["Driver"] != "local" || v["Mountpoint"] != "" {
		t.Errorf("create: %v", v)
	}
	if got := e.used(e.a, ClassDiskGB); got != 2 {
		t.Errorf("disk_gb: %d", got)
	}
	if fi, err := os.Stat(filepath.Join(e.vols, e.a, "volumes", "data")); err != nil || !fi.IsDir() {
		t.Errorf("volume directory: %v", err)
	}
	// A create of one that exists returns it, and charges nothing more.
	e.ok(e.a, "POST", "/v1.47/volumes/create", obj{"Name": "data"})
	if got := e.used(e.a, ClassVolumes); got != 1 {
		t.Errorf("volumes: %d", got)
	}
	e.fail(403, "driver option", e.a, "POST", "/v1.47/volumes/create", obj{"Name": "nfs", "DriverOpts": obj{"type": "nfs", "o": "addr=10.0.0.1"}})
	e.fail(403, "volume driver", e.a, "POST", "/v1.47/volumes/create", obj{"Name": "x", "Driver": "rexray"})
	e.fail(400, "invalid characters", e.a, "POST", "/v1.47/volumes/create", obj{"Name": "../x"})
	e.fail(404, "no such volume", e.b, "GET", "/v1.47/volumes/data", nil)

	e.create(e.a, "user", obj{"HostConfig": obj{"Mounts": []obj{{"Type": "volume", "Source": "data", "Target": "/data"}}}})
	e.fail(409, "volume is in use", e.a, "DELETE", "/v1.47/volumes/data", nil)
	l := e.ok(e.a, "GET", "/v1.47/volumes", nil)["Volumes"].([]any)
	if len(l) != 1 {
		t.Errorf("list: %v", l)
	}
	e.ok(e.a, "DELETE", "/v1.47/containers/user", nil)
	pr := e.ok(e.a, "POST", "/v1.47/volumes/prune", nil)
	if len(pr["VolumesDeleted"].([]any)) != 0 {
		t.Errorf("prune took a named volume: %v", pr)
	}
	e.ok(e.a, "DELETE", "/v1.47/volumes/data", nil)
	if _, err := os.Stat(filepath.Join(e.vols, e.a, "volumes", "data")); !os.IsNotExist(err) {
		t.Errorf("volume directory after delete: %v", err)
	}
	if got := e.used(e.a, ClassVolumes); got != 0 {
		t.Errorf("volumes after delete: %d", got)
	}
}

func TestImages(t *testing.T) {
	e := newEnv(t)
	e.pull(e.a, "alpine")
	e.pull(e.a, "alpine:latest")
	if got := e.used(e.a, ClassDiskGB); got != 1 {
		t.Errorf("disk_gb after two pulls of one image: %d", got)
	}
	l := e.list(e.a, "GET", "/v1.47/images/json")
	if len(l) != 1 || fmt.Sprint(l[0].(obj)["RepoTags"]) != "[alpine:latest]" {
		t.Errorf("images: %v", l)
	}
	e.ok(e.a, "POST", "/v1.47/images/alpine/tag?repo=registry.silo.internal/a/app&tag=v1", nil)
	in := e.ok(e.a, "GET", "/v1.47/images/registry.silo.internal/a/app:v1/json", nil)
	if len(in["RepoTags"].([]any)) != 2 {
		t.Errorf("inspect: %v", in)
	}
	if l := e.list(e.b, "GET", "/v1.47/images/json"); len(l) != 0 {
		t.Errorf("team-b sees team-a's images: %v", l)
	}
	e.fail(404, "pull access denied", e.a, "POST", "/v1.47/images/create?fromImage=missing", nil)
	e.create(e.a, "cc", nil)
	code, b, _ := e.raw(e.a, "DELETE", "/v1.47/images/alpine", nil, nil)
	if code != 200 || !strings.Contains(string(b), "Untagged") {
		t.Errorf("untag: %d %s", code, b)
	}
	e.fail(409, "image is being used", e.a, "DELETE", "/v1.47/images/registry.silo.internal/a/app:v1", nil)
	e.ok(e.a, "DELETE", "/v1.47/containers/cc?v=1", nil)
	code, b, _ = e.raw(e.a, "DELETE", "/v1.47/images/registry.silo.internal/a/app:v1", nil, nil)
	if code != 200 || !strings.Contains(string(b), "Deleted") {
		t.Errorf("delete: %d %s", code, b)
	}
	if got := e.used(e.a, ClassDiskGB); got != 0 {
		t.Errorf("disk_gb after the image's delete: %d", got)
	}
}

func TestPublishedPortsAndLogs(t *testing.T) {
	e := newEnv(t)
	id := e.create(e.root, "web", obj{"HostConfig": obj{"PublishAllPorts": true}})
	e.create(e.root, "other", obj{"HostConfig": obj{"PortBindings": obj{"80/tcp": []obj{{"HostPort": "32768"}}}}})
	e.ok(e.root, "POST", "/v1.47/containers/web/start", nil)
	e.fail(409, "port is already allocated", e.root, "POST", "/v1.47/containers/other/start", nil)
	var ps []Published
	err := e.db.Tx(ctx, func(tx store.Tx) error {
		var err error
		ps, err = e.facade.Published(ctx, tx)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].Port != 32768 || ps[0].Target.Port() != 80 || !ps[0].Host || ps[0].Silo != e.root {
		t.Errorf("published: %+v", ps)
	}

	if err := e.vm.Output(id, "hello\nworld\n", "oops\n"); err != nil {
		t.Fatal(err)
	}
	code, b, h := e.raw(e.root, "GET", "/v1.47/containers/web/logs?stdout=1&stderr=1&tail=1", nil, nil)
	want := "\x01\x00\x00\x00\x00\x00\x00\x06world\n\x02\x00\x00\x00\x00\x00\x00\x05oops\n"
	if code != 200 || string(b) != want || h.Get("Content-Type") != "application/vnd.docker.multiplexed-stream" {
		t.Errorf("logs: %d %q %v", code, b, h)
	}
	code, b, _ = e.raw(e.root, "GET", "/v1.47/containers/web/stats?stream=false", nil, nil)
	if code != 200 || !strings.Contains(string(b), `"pids_stats":{"current":1}`) {
		t.Errorf("stats: %d %s", code, b)
	}
	// A container that exits on its own gives its run charge back.
	e.ok(e.root, "POST", "/v1.47/containers/other/rename?name=o2", nil)
	if err := e.vm.Exit(id, 3); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for e.used(e.root, ClassMemoryMB) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := e.used(e.root, ClassMemoryMB); got != 0 {
		t.Errorf("memory_mb after the workload exited: %d", got)
	}
	in := e.ok(e.root, "GET", "/v1.47/containers/web/json", nil)
	if in["State"].(obj)["ExitCode"] != 3.0 || in["State"].(obj)["Status"] != "exited" {
		t.Errorf("state: %v", in["State"])
	}
}
