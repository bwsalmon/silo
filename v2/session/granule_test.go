package session_test

import (
	"context"
	"os"
	"os/exec"
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
