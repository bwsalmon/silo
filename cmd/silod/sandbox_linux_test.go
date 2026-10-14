package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/nsd"
	"github.com/bwsalmon/silo/pkg/backend/local/rootless"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/backend/local/storage"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/registry/containerdstore"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
)

// needNSD skips, or fails where SILO_REQUIRE_NSD says the nsd job's
// tools are there.
func needNSD(t *testing.T, what string, ok bool) {
	t.Helper()
	if ok {
		return
	}
	if os.Getenv("SILO_REQUIRE_NSD") != "" {
		t.Fatalf("SILO_REQUIRE_NSD is set, but %s", what)
	}
	t.Skip(what)
}

// TestSandboxContainer runs a sandbox-class container through the
// Docker facade as silod serve --backend=local builds it: silo-nsd and
// silo-containerd run as `silod nsd` and `silod containerd`, the driver
// is sandboxDriver's, the wiring is the holder's, and the image comes
// from a docker load into silo-containerd's content store. Only the
// shim's cgroups are changed, to none, which a CI runner doesn't
// delegate.
func TestSandboxContainer(t *testing.T) {
	runsc := os.Getenv("SILO_NSD_RUNSC")
	if runsc == "" {
		runsc, _ = exec.LookPath("runsc")
	}
	needNSD(t, "no runsc (set SILO_NSD_RUNSC)", runsc != "")
	for _, bin := range []string{"containerd", "newuidmap", "busybox"} {
		_, err := exec.LookPath(bin)
		needNSD(t, bin+" is not on PATH", err == nil)
	}
	shimDir := filepath.Dir(runsc)
	if _, err := os.Stat(filepath.Join(shimDir, "containerd-shim-runsc-v1")); err != nil {
		p, err := exec.LookPath("containerd-shim-runsc-v1")
		needNSD(t, "no containerd-shim-runsc-v1 beside runsc or on PATH", err == nil)
		shimDir = filepath.Dir(p)
	}
	busybox, _ := exec.LookPath("busybox")

	// The holder sees /tmp as the host does; its own /run is private.
	base, err := os.MkdirTemp("", "silod-sandbox-")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(base, 0o755)
	silod := filepath.Join(base, "silod")
	if out, err := exec.Command("go", "build", "-o", silod, ".").CombinedOutput(); err != nil {
		t.Fatalf("building silod: %v\n%s", err, out)
	}
	rt, data := filepath.Join(base, "rt"), filepath.Join(base, "data")
	for _, d := range []string{rt, data, filepath.Join(data, "silos"), filepath.Join(base, "spool")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	env := append(os.Environ(), "PATH="+shimDir+":"+filepath.Dir(runsc)+":"+os.Getenv("PATH"))
	var logs bytes.Buffer
	start := func(args ...string) *exec.Cmd {
		cmd := exec.Command(silod, args...)
		cmd.Env, cmd.Stdout, cmd.Stderr = env, &logs, &logs
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cmd.Process.Signal(syscall.SIGTERM)
			cmd.Wait()
		})
		return cmd
	}
	state := filepath.Join(rt, "nsd.state")
	start("nsd", "--socket", filepath.Join(rt, "nsd.sock"), "--state", state, "--silos", filepath.Join(data, "silos"),
		"--addr", "10.255.255.1/32,10.255.255.2/32")
	t.Cleanup(func() {
		// The containers' files belong to subuids, which only the
		// holder can remove; the cleanups run in reverse, so it is
		// still there.
		if st, err := nsd.ReadState(state); err == nil {
			nsd.JoinExec(st.Pid, silod, "rm", "-rf", base).Run()
		}
		os.RemoveAll(base)
		if t.Failed() {
			t.Logf("silod's processes:\n%s", logs.String())
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if _, err := nsd.WaitState(ctx, state, time.Minute); err != nil {
		t.Fatalf("the holder: %v\n%s", err, logs.String())
	}
	start("containerd", "--data-dir", data, "--runtime-dir", rt)

	// `silod containerd` wrote the shim's options with the systemd
	// cgroup driver; this runner has no delegated cgroups.
	p := rootless.Paths{DataDir: data, RuntimeDir: rt}
	wait := func(what string, ok func() bool) {
		t.Helper()
		for !ok() {
			if ctx.Err() != nil {
				t.Fatalf("%s never happened\n%s", what, logs.String())
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	wait("the runsc shim's options", func() bool {
		b, err := os.ReadFile(p.RunscOptions())
		return err == nil && strings.Contains(string(b), "systemd-cgroup")
	})
	b, _ := os.ReadFile(p.RunscOptions())
	if err := os.WriteFile(p.RunscOptions(), []byte(strings.Replace(string(b), `systemd-cgroup = "true"`, `ignore-cgroups = "true"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}

	db, err := sqlite.Open(filepath.Join(base, "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}
	sbx, err := sandboxDriver(sandboxConfig{Exe: silod, State: state, Containerd: p.ContainerdSocket(),
		Dir: filepath.Join(data, "cache", "containers"), Volumes: filepath.Join(data, "silos"), Runsc: runsc, Toolbox: busybox})
	if err != nil {
		t.Fatal(err)
	}
	defer sbx.(io.Closer).Close()
	wait("silo-containerd answering", func() bool { return sbx.Probe(ctx).OK })
	cs, err := containerdstore.New(containerdstore.Options{Address: p.ContainerdSocket(), Spool: filepath.Join(base, "spool")})
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	sn, err := openSiloNet(filepath.Join(rt, "nsd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer sn.Close()
	names, _ := parseLocalGateway("10.255.255.1,10.255.255.2")
	dk := newLocalDocker(db, &storage.Dir{Root: filepath.Join(data, "silos")}, names, sn, false, []runtime.Driver{sbx},
		filepath.Join(data, "silos"), "test", nil, cs)
	defer dk.f.Close()

	// A root and a child that may run only sandbox containers.
	var caller fenceapi.Caller
	api := fenceapi.New(db, fenceapi.Options{Caller: func(*http.Request) (fenceapi.Caller, error) { return caller, nil }})
	defer api.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/silo/") || strings.HasPrefix(r.URL.Path, "/silod/") {
			api.ServeHTTP(w, r)
			return
		}
		dk.f.ServeHTTP(w, r.WithContext(fenceapi.ContextWithCaller(r.Context(), caller)))
	}))
	defer srv.Close()
	call := func(method, path string, body io.Reader, out any) int {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, method, srv.URL+path, body)
		req.Header.Set("Content-Type", "application/json")
		if strings.HasSuffix(path, "/load") {
			req.Header.Set("Content-Type", "application/x-tar")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 300 {
			t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
		}
		if out != nil {
			if s, ok := out.(*string); ok {
				*s = string(b)
			} else if err := json.Unmarshal(b, out); err != nil {
				t.Fatalf("%s %s: %q: %v", method, path, b, err)
			}
		}
		return resp.StatusCode
	}
	js := func(v any) io.Reader { b, _ := json.Marshal(v); return bytes.NewReader(b) }
	caller = fenceapi.Caller{Via: fenceapi.ViaInternal}
	var root struct{ ID string }
	call("POST", "/silod/bootstrap", js(map[string]any{
		"backing":  map[string]any{"kind": "local"},
		"capacity": map[string]any{"global": map[string]any{"vcpu_m": 8000, "memory_mb": 8192, "disk_gb": 100, "containers": 5, "networks": 5, "volumes": 5, "image_repos": 5}},
		"admins":   []string{"user:admin@example.com"},
	}), &root)
	caller = fenceapi.Caller{Silo: root.ID, Via: fenceapi.ViaInternal}
	var child struct{ ID string }
	call("POST", "/silo/v1/silos/-"+root.ID+"/children", js(map[string]any{"name": "ci", "address": map[string]any{"prefix_length": 20},
		"isolation": map[string]any{"allowed": []string{"sandbox"}},
		"quota":     map[string]any{"global": map[string]any{"vcpu_m": 4000, "memory_mb": 2048, "disk_gb": 50, "containers": 2, "networks": 2, "volumes": 2}}}), &child)
	caller = fenceapi.Caller{Silo: child.ID, Via: fenceapi.ViaFrontDoor}

	call("POST", "/v1.47/images/load?quiet=1", bytes.NewReader(busyboxArchive(t, busybox, "bb:1")), nil)
	var made struct{ Id string }
	call("POST", "/v1.47/containers/create?name=hello", js(map[string]any{"Image": "bb:1",
		"Cmd": []string{"sh", "-c", "echo hello from $(id -u); ip -4 -o addr show eth0"}}), &made)
	call("POST", "/v1.47/containers/"+made.Id+"/start", nil, nil)
	var waited struct{ StatusCode int }
	call("POST", "/v1.47/containers/"+made.Id+"/wait", nil, &waited)
	var out string
	call("GET", "/v1.47/containers/"+made.Id+"/logs?stdout=1&stderr=1", nil, &out)
	t.Logf("exit %d, logs %q", waited.StatusCode, out)
	if waited.StatusCode != 0 || !strings.Contains(out, "hello from 0") || !strings.Contains(out, "eth0") {
		t.Errorf("exit %d, logs %q", waited.StatusCode, out)
	}
	var inspect struct {
		NetworkSettings struct {
			Networks map[string]struct{ IPAddress string }
		}
	}
	call("GET", "/v1.47/containers/"+made.Id+"/json", nil, &inspect)
	for _, n := range inspect.NetworkSettings.Networks {
		if n.IPAddress == "" || !strings.Contains(out, n.IPAddress+"/") {
			t.Errorf("the container's eth0 isn't its endpoint's %s: %q", n.IPAddress, out)
		}
	}
	call("DELETE", "/v1.47/containers/"+made.Id+"?force=1", nil, nil)
}

// busyboxArchive is a docker save archive of one image, tagged tag,
// whose one layer is a static busybox and its links.
func busyboxArchive(t *testing.T, busybox, tag string) []byte {
	t.Helper()
	bb, err := os.ReadFile(busybox)
	if err != nil {
		t.Fatal(err)
	}
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	for _, dir := range []string{"bin", "dev", "etc", "proc", "root", "sys", "tmp"} {
		tw.WriteHeader(&tar.Header{Name: dir + "/", Typeflag: tar.TypeDir, Mode: 0o755})
	}
	tw.WriteHeader(&tar.Header{Name: "bin/busybox", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(bb))})
	tw.Write(bb)
	for _, a := range []string{"sh", "echo", "id", "ip", "cat", "sleep", "tar"} {
		tw.WriteHeader(&tar.Header{Name: "bin/" + a, Typeflag: tar.TypeSymlink, Linkname: "busybox"})
	}
	tw.Close()
	digest := func(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }
	ld := digest(layer.Bytes())
	cfg, _ := json.Marshal(map[string]any{"architecture": goruntime.GOARCH, "os": "linux",
		"config": map[string]any{"Env": []string{"PATH=/bin"}, "Cmd": []string{"sh"}},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{"sha256:" + ld}}})
	cd := digest(cfg)
	manifest, _ := json.Marshal([]map[string]any{{"Config": "blobs/sha256/" + cd, "RepoTags": []string{tag}, "Layers": []string{"blobs/sha256/" + ld}}})
	var out bytes.Buffer
	aw := tar.NewWriter(&out)
	for _, f := range []struct {
		name string
		b    []byte
	}{{"blobs/sha256/" + ld, layer.Bytes()}, {"blobs/sha256/" + cd, cfg}, {"manifest.json", manifest}} {
		aw.WriteHeader(&tar.Header{Name: f.name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(f.b))})
		aw.Write(f.b)
	}
	aw.Close()
	return out.Bytes()
}
