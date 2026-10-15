package local_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/conformance/targets"
)

// TestInstalledSandbox runs a sandbox-class container through the
// Docker facade of the local backend installed on this host, as
// local-conformance installs it (`silo install --backend local`): the
// root's /info offers the class, a docker load of a busybox image
// survives until the create, and the container runs under runsc as its
// namespace's root with eth0 at its endpoint's address, and a
// container's memory limit is enforced by its cgroup. It skips with
// no install, unless targets.RequireLocalEnv is set.
func TestInstalledSandbox(t *testing.T) {
	r := installedRoot(t)
	var info struct{ Runtimes map[string]any }
	r.call("GET", "/info", "", nil, &info)
	if _, ok := info.Runtimes["silo-sandbox"]; !ok {
		t.Fatalf("the root's /info offers no silo-sandbox: %v", info.Runtimes)
	}

	id, code, logs := r.run("echo hello from $(id -u); ip -4 -o addr show eth0")
	if code != 0 || !strings.Contains(logs, "hello from 0") {
		t.Fatalf("exit %d, logs %q", code, logs)
	}
	var inspect struct {
		HostConfig      struct{ Runtime string }
		NetworkSettings struct {
			Networks map[string]struct{ IPAddress string }
		}
	}
	r.call("GET", "/containers/"+id+"/json", "", nil, &inspect)
	if inspect.HostConfig.Runtime != "silo-sandbox" {
		t.Errorf("HostConfig.Runtime %q, want silo-sandbox", inspect.HostConfig.Runtime)
	}
	var addrs []string
	for _, n := range inspect.NetworkSettings.Networks {
		addrs = append(addrs, n.IPAddress)
	}
	if !slices.ContainsFunc(addrs, func(a string) bool { return a != "" && strings.Contains(logs, " "+a+"/") }) {
		t.Errorf("the container's eth0 is none of its endpoints' %v: %q", addrs, logs)
	}

	// The kernel holds a container to its memory: under a 128 MiB limit,
	// tail holding 16 MiB of a line runs, and holding 400 MiB is killed.
	// local-conformance's runner delegates memory to the user's manager.
	limit := map[string]any{"Memory": 128 << 20}
	if _, code, logs := r.runWith(limit, "head -c 16m /dev/zero | tail | wc -c"); code != 0 || !strings.Contains(logs, "16777216") {
		t.Errorf("16 MiB under a 128 MiB limit: exit %d, logs %q", code, logs)
	}
	if _, code, logs := r.runWith(limit, "head -c 400m /dev/zero | tail | wc -c"); code == 0 {
		t.Errorf("400 MiB under a 128 MiB limit wasn't stopped: exit %d, logs %q", code, logs)
	} else {
		t.Logf("400 MiB under a 128 MiB limit: exit %d, logs %q", code, logs)
	}
}

// TestInstalledSandboxDNS runs a sandbox-class container in the
// installed root's default network and resolves the root's
// <id>.docker.silo.internal from it, through the /etc/resolv.conf silod
// writes, then reaches the root's Docker facade at that name. Its
// /etc/hosts names the container itself.
func TestInstalledSandboxDNS(t *testing.T) {
	r := installedRoot(t)
	host := r.in.Root + ".docker.silo.internal"
	_, code, logs := r.run("cat /etc/resolv.conf; echo ---; cat /etc/hosts; echo ---; " +
		"nslookup " + host + " && wget -q -O - http://" + host + ":2375/v1.47/version")
	if code != 0 {
		t.Fatalf("exit %d, logs %q", code, logs)
	}
	if !strings.Contains(logs, "nameserver ") {
		t.Errorf("no nameserver in the container's /etc/resolv.conf: %q", logs)
	}
	if !strings.Contains(logs, "localhost") {
		t.Errorf("no localhost in the container's /etc/hosts: %q", logs)
	}
	if !strings.Contains(logs, `"ApiVersion"`) {
		t.Errorf("no facade version from %s: %q", host, logs)
	}
}

// TestInstalledSandboxExec execs into a running sandbox-class container
// through the installed root's Docker facade: runsc exec, run as the
// container's root inside silo-nsd's namespaces, must reach the process
// file the shim wrote, which a rootless install's 0700 $XDG_RUNTIME_DIR
// once kept from it.
func TestInstalledSandboxExec(t *testing.T) {
	r := installedRoot(t)
	js := func(v any) []byte { b, _ := json.Marshal(v); return b }
	var made struct{ Id string }
	r.call("POST", "/containers/create", "application/json", js(map[string]any{"Image": r.image,
		"HostConfig": map[string]any{"Runtime": "silo-sandbox"},
		"Cmd":        []string{"sleep", "300"}}), &made)
	t.Cleanup(func() { r.call("DELETE", "/containers/"+made.Id+"?force=1", "", nil, nil) })
	r.call("POST", "/containers/"+made.Id+"/start", "", nil, nil)

	exec := func(script string) (code int, out string) {
		t.Helper()
		var ex struct{ Id string }
		r.call("POST", "/containers/"+made.Id+"/exec", "application/json", js(map[string]any{
			"Cmd": []string{"sh", "-c", script}, "AttachStdout": true, "AttachStderr": true}), &ex)
		// Not a TTY: out is multiplexed, its frames' headers included.
		r.call("POST", "/exec/"+ex.Id+"/start", "application/json", js(map[string]any{"Detach": false, "Tty": false}), &out)
		var in struct {
			Running  bool
			ExitCode int
		}
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
			r.call("GET", "/exec/"+ex.Id+"/json", "", nil, &in)
			if !in.Running || time.Now().After(deadline) {
				break
			}
		}
		if in.Running {
			t.Fatalf("exec %q still running", script)
		}
		return in.ExitCode, out
	}
	if code, out := exec("echo exec as $(id -u)"); code != 0 || !strings.Contains(out, "exec as 0") {
		t.Errorf("exec: exit %d, output %q", code, out)
	}
	if code, out := exec("exit 3"); code != 3 {
		t.Errorf("exec of exit 3: exit %d, output %q", code, out)
	}
}

// installed is the local install's root, through its Docker facade,
// with a busybox image loaded for its containers.
type installed struct {
	t     *testing.T
	ctx   context.Context
	in    *targets.Install
	dk    *http.Client
	image string
}

// installedRoot finds the local install, or skips (fails, with
// targets.RequireLocalEnv set) without one, and loads a busybox image
// into its root, deleted when the test ends.
func installedRoot(t *testing.T) *installed {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel) // after the cleanups that delete what was made
	need := func(what string, err error) {
		t.Helper()
		if err == nil {
			return
		}
		if os.Getenv(targets.RequireLocalEnv) != "" {
			t.Fatalf("%s is set, but %s: %v", targets.RequireLocalEnv, what, err)
		}
		t.Skipf("%s: %v", what, err)
	}
	in, err := targets.FindInstall(ctx)
	need("no local install", err)
	busybox, err := exec.LookPath("busybox")
	need("no busybox to make an image of", err)
	r := &installed{t: t, ctx: ctx, in: in, dk: targets.UnixClient(in.Docker)}

	r.image = fmt.Sprintf("silo-conformance-bb:%d", time.Now().UnixNano())
	r.call("POST", "/images/load?quiet=1", "application/x-tar", busyboxArchive(t, busybox, r.image), nil)
	t.Cleanup(func() { r.call("DELETE", "/images/"+r.image+"?force=1", "", nil, nil) })
	// A load's blobs once lasted only as long as its request; the GC
	// that took them runs soon after, not at once.
	time.Sleep(2 * time.Second)
	return r
}

// call makes one Engine API call, failing the test on an error status,
// and decodes its body into out: a *string as it is, else as JSON.
func (r *installed) call(method, path, ctype string, body []byte, out any) {
	t := r.t
	t.Helper()
	req, _ := http.NewRequestWithContext(r.ctx, method, "http://docker/v1.47"+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", ctype)
	resp, err := r.dk.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
	}
	switch o := out.(type) {
	case nil:
	case *string:
		*o = string(b)
	default:
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s %s: %q: %v", method, path, b, err)
		}
	}
}

// run runs script with sh -c in a sandbox-class container of the
// image, deleted when the test ends, and returns its id, exit code
// and logs.
func (r *installed) run(script string) (id string, code int, logs string) {
	r.t.Helper()
	return r.runWith(nil, script)
}

// runWith is run with hostConfig's fields added to the container's
// HostConfig.
func (r *installed) runWith(hostConfig map[string]any, script string) (id string, code int, logs string) {
	r.t.Helper()
	js := func(v any) []byte { b, _ := json.Marshal(v); return b }
	hc := map[string]any{"Runtime": "silo-sandbox"}
	for k, v := range hostConfig {
		hc[k] = v
	}
	var made struct{ Id string }
	r.call("POST", "/containers/create", "application/json", js(map[string]any{"Image": r.image,
		"HostConfig": hc,
		"Cmd":        []string{"sh", "-c", script}}), &made)
	r.t.Cleanup(func() { r.call("DELETE", "/containers/"+made.Id+"?force=1", "", nil, nil) })
	r.call("POST", "/containers/"+made.Id+"/start", "", nil, nil)
	var waited struct{ StatusCode int }
	r.call("POST", "/containers/"+made.Id+"/wait", "", nil, &waited)
	r.call("GET", "/containers/"+made.Id+"/logs?stdout=1&stderr=1", "", nil, &logs)
	return made.Id, waited.StatusCode, logs
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
	for _, a := range []string{"sh", "echo", "id", "ip", "cat", "sleep", "nc", "ping", "wget", "nslookup", "head", "tail", "wc"} {
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
