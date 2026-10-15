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
	dk := targets.UnixClient(in.Docker)

	call := func(method, path, ctype string, body []byte, out any) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, method, "http://docker/v1.47"+path, bytes.NewReader(body))
		req.Header.Set("Content-Type", ctype)
		resp, err := dk.Do(req)
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
	js := func(v any) []byte { b, _ := json.Marshal(v); return b }

	var info struct{ Runtimes map[string]any }
	call("GET", "/info", "", nil, &info)
	if _, ok := info.Runtimes["silo-sandbox"]; !ok {
		t.Fatalf("the root's /info offers no silo-sandbox: %v", info.Runtimes)
	}

	tag := fmt.Sprintf("silo-conformance-bb:%d", time.Now().UnixNano())
	call("POST", "/images/load?quiet=1", "application/x-tar", busyboxArchive(t, busybox, tag), nil)
	t.Cleanup(func() { call("DELETE", "/images/"+tag+"?force=1", "", nil, nil) })
	// A load's blobs once lasted only as long as its request; the GC
	// that took them runs soon after, not at once.
	time.Sleep(2 * time.Second)

	var made struct{ Id string }
	call("POST", "/containers/create", "application/json", js(map[string]any{"Image": tag,
		"HostConfig": map[string]any{"Runtime": "silo-sandbox"},
		"Cmd":        []string{"sh", "-c", "echo hello from $(id -u); ip -4 -o addr show eth0"}}), &made)
	t.Cleanup(func() { call("DELETE", "/containers/"+made.Id+"?force=1", "", nil, nil) })
	call("POST", "/containers/"+made.Id+"/start", "", nil, nil)
	var waited struct{ StatusCode int }
	call("POST", "/containers/"+made.Id+"/wait", "", nil, &waited)
	var logs string
	call("GET", "/containers/"+made.Id+"/logs?stdout=1&stderr=1", "", nil, &logs)
	if waited.StatusCode != 0 || !strings.Contains(logs, "hello from 0") {
		t.Fatalf("exit %d, logs %q", waited.StatusCode, logs)
	}
	var inspect struct {
		HostConfig      struct{ Runtime string }
		NetworkSettings struct {
			Networks map[string]struct{ IPAddress string }
		}
	}
	call("GET", "/containers/"+made.Id+"/json", "", nil, &inspect)
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
	run := func(memory int64, script string) (int, string) {
		t.Helper()
		var made struct{ Id string }
		call("POST", "/containers/create", "application/json", js(map[string]any{"Image": tag,
			"HostConfig": map[string]any{"Runtime": "silo-sandbox", "Memory": memory},
			"Cmd":        []string{"sh", "-c", script}}), &made)
		t.Cleanup(func() { call("DELETE", "/containers/"+made.Id+"?force=1", "", nil, nil) })
		call("POST", "/containers/"+made.Id+"/start", "", nil, nil)
		var waited struct{ StatusCode int }
		call("POST", "/containers/"+made.Id+"/wait", "", nil, &waited)
		var logs string
		call("GET", "/containers/"+made.Id+"/logs?stdout=1&stderr=1", "", nil, &logs)
		return waited.StatusCode, logs
	}
	const limit = 128 << 20
	if code, logs := run(limit, "head -c 16m /dev/zero | tail | wc -c"); code != 0 || !strings.Contains(logs, "16777216") {
		t.Errorf("16 MiB under a 128 MiB limit: exit %d, logs %q", code, logs)
	}
	if code, logs := run(limit, "head -c 400m /dev/zero | tail | wc -c"); code == 0 {
		t.Errorf("400 MiB under a 128 MiB limit wasn't stopped: exit %d, logs %q", code, logs)
	} else {
		t.Logf("400 MiB under a 128 MiB limit: exit %d, logs %q", code, logs)
	}
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
