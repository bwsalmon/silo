package session_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/v2/internal/granuletest"
	"github.com/bwsalmon/silo/v2/sandbox"
	"github.com/bwsalmon/silo/v2/sandbox/container"
	"github.com/bwsalmon/silo/v2/sandbox/granule"
	"github.com/bwsalmon/silo/v2/session"
)

// granuleBackends is what silo run makes for a Silofile with granule
// sandboxes (session's newBackend), with granule's state kept in the
// test's own directory.
func granuleBackends(t *testing.T) (*sandbox.Kinds, *granule.Backend) {
	t.Helper()
	gb, err := granule.New()
	if err != nil {
		t.Fatal(err)
	}
	gb.StateDir = t.TempDir()
	return &sandbox.Kinds{
		Default: &container.Backend{Runtime: gb.Docker},
		ByKind:  map[string]sandbox.Backend{"granule": gb},
	}, gb
}

// TestGranuleGitProxyEndToEnd is journey 7: a granule microVM has its
// repo cloned through the session's git proxy, from inside the guest,
// and pushes a branch its Silofile allows; the credential never reaches
// the guest, and the VM goes with the session.
func TestGranuleGitProxyEndToEnd(t *testing.T) {
	image := granuletest.Require(t)
	kinds, gb := granuleBackends(t)
	up := newGitUpstream(t)
	e := newEnv(t)
	writeToken(t, e, upstreamToken)
	e.fake(fakeConfig{Call: "sandbox_exec", Args: map[string]any{"sandbox": "vm", "command": sandboxScript}})
	f := e.silofile(gitSilofile + `sandboxes:
  - name: vm
    kind: granule
    image: ` + image + `
    workdir: /work
    repos: [widgets]
    env:
      GIT_AUTHOR_NAME: Silo Agent
      GIT_AUTHOR_EMAIL: agent@example.invalid
      GIT_COMMITTER_NAME: Silo Agent
      GIT_COMMITTER_EMAIL: agent@example.invalid
`)
	res := e.run(f, func(o *session.Options) { o.Backend = kinds; o.GitForwarderFor = up.forwards })
	if res.code != 0 {
		t.Fatalf("exit %d (%v)\n%s", res.code, res.err, res.stderr)
	}
	r := e.report()
	out := r.Result
	t.Logf("in the guest:\n%s", out)
	if r.Error != "" || !strings.Contains(out, "head: Initial widgets") {
		t.Fatalf("the checkout is not there: %q (error %q)\n%s", out, r.Error, res.stderr)
	}
	if !strings.Contains(out, "push-silo=0") {
		t.Errorf("the push to silo/feature failed:\n%s", out)
	}
	if strings.Contains(out, "push-main=0") || !strings.Contains(out, "matches no push pattern") {
		t.Errorf("the push to main was not refused by the proxy:\n%s", out)
	}
	if strings.Contains(out, upstreamToken) {
		t.Errorf("the credential reached the guest:\n%s", out)
	}
	if !strings.Contains(out, "url = http://silo:") {
		t.Errorf("origin is not the proxy URL with the sandbox's token:\n%s", out)
	}
	if up.ref(t, "refs/heads/silo/feature") == "" {
		t.Error("silo/feature did not reach the upstream")
	}
	rec := e.sessionGone()
	if found, err := gb.Find(context.Background(), rec.ID); err != nil || len(found) != 0 {
		t.Errorf("the session's VM is left after teardown: %v, %v", found, err)
	}
}

// TestGranuleGCFindsLeakedVM is silo gc for granule (design §3.1): a VM
// labelled with a session that has ended -- here one that never had a
// session directory -- is found and deleted.
func TestGranuleGCFindsLeakedVM(t *testing.T) {
	image := granuletest.Require(t)
	kinds, gb := granuleBackends(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	leaked := session.NewID()
	sb, err := gb.Create(ctx, sandbox.SandboxSpec{Session: leaked, Name: "vm", Kind: "granule", Image: image})
	if err != nil {
		t.Fatal(err)
	}
	vm := sb.(*granule.VM).VM
	t.Cleanup(func() { _ = sb.Destroy(context.Background()) })

	e := newEnv(t)
	getenv := func(k string) string {
		for _, kv := range e.environ {
			if strings.HasPrefix(kv, k+"=") {
				return kv[len(k)+1:]
			}
		}
		return ""
	}
	rep, err := session.GC(ctx, session.GCOptions{Getenv: getenv, Backend: kinds})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(rep.Removed, "\n"), "ended session "+leaked) {
		t.Errorf("gc did not report the leaked VM: %q", rep.Removed)
	}
	if err := exec.Command("docker", "inspect", granule.ContainerName(vm)).Run(); err == nil {
		t.Errorf("the leaked VM's container %s is still there", granule.ContainerName(vm))
	}
	if _, err := os.Stat(gb.StateDir + "/" + vm); err == nil {
		t.Logf("granule's saved state for %s is still in its state directory", vm)
	}
}

// TestGranulefileBuildsOnceEndToEnd is journey 10: a granule sandbox
// defined by a Granulefile that wraps a container in a VM is built at
// the first run, booted, recorded in .silo/lock.json by digest and
// content key, and booted again at the second run without a rebuild.
func TestGranulefileBuildsOnceEndToEnd(t *testing.T) {
	granuletest.Require(t)
	kinds, gb := granuleBackends(t)
	// granule, through a wrapper that logs each granulefile build.
	real, err := exec.LookPath(gb.Granule)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	builds := filepath.Join(dir, "builds")
	gb.Granule = filepath.Join(dir, "granule")
	wrapper := "#!/bin/sh\nif [ \"$1 $2\" = \"granulefile build\" ]; then echo build >> " + builds + "; fi\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(gb.Granule, []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}

	e := newEnv(t)
	e.timeout = 10 * time.Minute
	// A nonce of the test's own, so that no earlier run's image is a
	// cache hit.
	nonce := session.NewID()
	folder := filepath.Join(e.root, "silo")
	for name, data := range map[string]string{
		"sandboxes/dev.Granulefile": "FROM busybox:1.37\nARG NONCE\nCOPY hello.txt /hello.txt\nRUN echo \"$NONCE\" > /nonce\nCMD [\"sleep\", \"infinity\"]\nVM WRAP app\n",
		"sandboxes/hello.txt":       "hello from the Granulefile's context\n",
	} {
		p := filepath.Join(folder, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f := e.silofile(headless + `sandboxes:
  - name: vm
    kind: granule
    granulefile: sandboxes/dev.Granulefile
    args: {NONCE: ` + nonce + `}
`)
	if vm, err := granule.CheckGranulefile(filepath.Join(folder, "sandboxes", "dev.Granulefile"), ""); err != nil || !vm {
		t.Fatalf("granule granulefile check: %v, %v; want a VM image", vm, err)
	}
	key, err := granule.CacheKey(filepath.Join(folder, "sandboxes", "dev.Granulefile"), "", map[string]string{"NONCE": nonce})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", granule.BuiltTag(key)).Run() })

	// The wrapped container's filesystem is in the guest (granule
	// docs/granulefile.md §5.2).
	e.fake(fakeConfig{Call: "sandbox_exec", Args: map[string]any{"sandbox": "vm", "command": "cat /var/lib/granule/container/rootfs/hello.txt /var/lib/granule/container/rootfs/nonce"}})
	var digest string
	for run := 1; run <= 2; run++ {
		res := e.run(f, func(o *session.Options) { o.Backend = kinds })
		if res.code != 0 {
			t.Fatalf("run %d: exit %d (%v)\n%s", run, res.code, res.err, res.stderr)
		}
		r := e.report()
		if r.Error != "" || !strings.Contains(r.Result, "hello from the Granulefile's context") || !strings.Contains(r.Result, nonce) {
			t.Fatalf("run %d: in the guest: %q (error %q)\n%s", run, r.Result, r.Error, res.stderr)
		}
		images, err := sandbox.ReadImages(folder)
		if err != nil {
			t.Fatal(err)
		}
		img := images["vm"]
		if img.Key != key || !strings.HasPrefix(img.Digest, "sha256:") || !strings.HasSuffix(img.Ref, "dev.Granulefile") {
			t.Fatalf("run %d: lock.json records %+v, want key %s", run, img, key)
		}
		if run == 1 {
			digest = img.Digest
		} else if img.Digest != digest {
			t.Errorf("run 2 booted %s, run 1 %s", img.Digest, digest)
		}
		data, _ := os.ReadFile(builds)
		if n := strings.Count(string(data), "build"); n != 1 {
			t.Fatalf("after run %d, granule granulefile build ran %d times, want once", run, n)
		}
	}
}
