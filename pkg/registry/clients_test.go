package registry_test

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// Real registry clients against the registry in process, on a TCP
// port, as one silo (local-registry.md §13, step 27): the
// distribution-spec conformance suite, and the crane, skopeo and oras
// command-line tools. Each runs when its binary is given or on $PATH,
// and is skipped otherwise; the registry job of .grain/workflows
// installs them all and sets SILO_REQUIRE_REGISTRY_CLIENTS, which makes
// a missing one a failure.

// listen serves the registry on a loopback port as silo, and returns
// the port's host:port.
func (e *env) listen(silo string) string {
	e.t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		e.t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.reg.ServeHTTP(w, r.WithContext(fenceapi.ContextWithCaller(r.Context(), fenceapi.Caller{Silo: silo, Via: fenceapi.ViaOrigin})))
	})}
	go srv.Serve(l)
	e.t.Cleanup(func() { srv.Close() })
	return l.Addr().String()
}

// client finds a client's binary: the environment variable's path, or
// name on $PATH. It skips the test if there is none, unless clients
// are required.
func client(t *testing.T, env, name string) string {
	t.Helper()
	if p := os.Getenv(env); p != "" {
		return p
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	if os.Getenv("SILO_REQUIRE_REGISTRY_CLIENTS") != "" {
		t.Fatalf("no %s: set %s or put it on $PATH", name, env)
	}
	t.Skipf("no %s: set %s or put it on $PATH", name, env)
	return ""
}

func run(t *testing.T, env []string, bin string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), bin, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", filepath.Base(bin), strings.Join(args, " "), err, out)
	}
	return string(out)
}

// The OCI distribution-spec conformance suite: SILO_OCI_CONFORMANCE is
// its test binary, built from
// github.com/opencontainers/distribution-spec/conformance.
func TestDistributionSpec(t *testing.T) {
	bin := client(t, "SILO_OCI_CONFORMANCE", "oci-conformance.test")
	e := newEnv(t, nil)
	s := e.mk(e.root, "-"+e.root, "conf", nil)
	addr := e.listen(s)
	out := run(t, []string{
		"OCI_ROOT_URL=http://" + addr,
		"OCI_NAMESPACE=conf/repo-a",
		"OCI_CROSSMOUNT_NAMESPACE=conf/repo-b",
		"OCI_TEST_PULL=1", "OCI_TEST_PUSH=1", "OCI_TEST_CONTENT_DISCOVERY=1", "OCI_TEST_CONTENT_MANAGEMENT=1",
		"OCI_AUTOMATIC_CROSSMOUNT=1",
		"OCI_REPORT_DIR=" + t.TempDir(),
	}, bin)
	if i := strings.Index(out, "OCI Conformance Result"); i >= 0 {
		t.Log(out[i:])
	}
}

func TestCraneCLI(t *testing.T) {
	bin := client(t, "SILO_CRANE", "crane")
	e := newEnv(t, nil)
	s := e.mk(e.root, "-"+e.root, "team-a", nil)
	addr := e.listen(s)
	dir := t.TempDir()
	img, err := random.Image(2048, 2)
	if err != nil {
		t.Fatal(err)
	}
	tarball := filepath.Join(dir, "img.tar")
	p, err := layout.Write(filepath.Join(dir, "layout"), empty.Index)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := img.Digest()
	if err := p.AppendImage(img); err != nil {
		t.Fatal(err)
	}
	ref := addr + "/team-a/app:1"
	run(t, nil, bin, "push", "--insecure", "--index=false", filepath.Join(dir, "layout"), ref)
	if got := strings.TrimSpace(run(t, nil, bin, "digest", "--insecure", ref)); got != want.String() {
		t.Fatalf("crane digest: %s, pushed %s", got, want)
	}
	run(t, nil, bin, "copy", "--insecure", ref, addr+"/team-a/copy:1")
	run(t, nil, bin, "pull", "--insecure", addr+"/team-a/copy:1", tarball)
	if got := run(t, nil, bin, "ls", "--insecure", addr+"/team-a/app"); strings.TrimSpace(got) != "1" {
		t.Fatalf("crane ls: %q", got)
	}
	if got := run(t, nil, bin, "catalog", "--insecure", addr); strings.Fields(got)[0] != "team-a/app" || strings.Fields(got)[1] != "team-a/copy" {
		t.Fatalf("crane catalog: %q", got)
	}
}

func TestSkopeo(t *testing.T) {
	bin := client(t, "SILO_SKOPEO", "skopeo")
	e := newEnv(t, nil)
	s := e.mk(e.root, "-"+e.root, "team-a", nil)
	addr := e.listen(s)
	dir := t.TempDir()
	// skopeo finds only OCI manifests in a layout.
	img := mutate.ConfigMediaType(mutate.MediaType(empty.Image, types.OCIManifestSchema1), types.OCIConfigJSON)
	for range 2 {
		l, err := random.Layer(2048, types.OCILayer)
		if err != nil {
			t.Fatal(err)
		}
		if img, err = mutate.AppendLayers(img, l); err != nil {
			t.Fatal(err)
		}
	}
	p, err := layout.Write(filepath.Join(dir, "layout"), empty.Index)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.AppendImage(img, layout.WithAnnotations(map[string]string{"org.opencontainers.image.ref.name": "1"})); err != nil {
		t.Fatal(err)
	}
	want, _ := img.Digest()
	pol := filepath.Join(dir, "policy.json")
	os.WriteFile(pol, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o644)
	common := []string{"--policy", pol}
	run(t, nil, bin, append(common, "copy", "--dest-tls-verify=false", "--preserve-digests",
		"oci:"+filepath.Join(dir, "layout")+":1", "docker://"+addr+"/team-a/app:1")...)
	run(t, nil, bin, append(common, "copy", "--src-tls-verify=false", "--dest-tls-verify=false",
		"docker://"+addr+"/team-a/app:1", "docker://"+addr+"/team-a/copy:2")...)
	run(t, nil, bin, append(common, "copy", "--src-tls-verify=false",
		"docker://"+addr+"/team-a/copy:2", "oci:"+filepath.Join(dir, "out")+":2")...)
	if got := run(t, nil, bin, "inspect", "--tls-verify=false", "--format", "{{.Digest}}", "docker://"+addr+"/team-a/app:1"); strings.TrimSpace(got) != want.String() {
		t.Fatalf("skopeo inspect: %q, pushed %s", got, want)
	}
	if got := run(t, nil, bin, "list-tags", "--tls-verify=false", "docker://"+addr+"/team-a/copy"); !strings.Contains(got, `"2"`) {
		t.Fatalf("skopeo list-tags: %s", got)
	}
}

func TestOras(t *testing.T) {
	bin := client(t, "SILO_ORAS", "oras")
	e := newEnv(t, nil)
	s := e.mk(e.root, "-"+e.root, "team-a", nil)
	addr := e.listen(s)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("an artifact\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + dir}
	ref := addr + "/team-a/notes:v1"
	cmd := exec.Command(bin, "push", "--plain-http", "--artifact-type", "application/vnd.example.notes", ref, "notes.txt")
	cmd.Dir, cmd.Env = dir, append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("oras push: %v\n%s", err, out)
	}
	sig := filepath.Join(dir, "sig.txt")
	os.WriteFile(sig, []byte("a signature\n"), 0o644)
	cmd = exec.Command(bin, "attach", "--plain-http", "--artifact-type", "application/vnd.example.sig", ref, "sig.txt")
	cmd.Dir, cmd.Env = dir, append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("oras attach: %v\n%s", err, out)
	}
	if got := run(t, env, bin, "discover", "--plain-http", "--format", "json", ref); !strings.Contains(got, "application/vnd.example.sig") {
		t.Fatalf("oras discover: %s", got)
	}
	out := filepath.Join(dir, "out")
	run(t, env, bin, "pull", "--plain-http", "-o", out, ref)
	if b, err := os.ReadFile(filepath.Join(out, "notes.txt")); err != nil || string(b) != "an artifact\n" {
		t.Fatalf("oras pull: %q %v", b, err)
	}
	if got := run(t, env, bin, "repo", "tags", "--plain-http", addr+"/team-a/notes"); strings.TrimSpace(got) != "v1" {
		t.Fatalf("oras repo tags: %q", got)
	}
}
