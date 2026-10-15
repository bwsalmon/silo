package k8s_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/bwsalmon/silo/old/location"
	k8sloc "github.com/bwsalmon/silo/old/location/k8s"
	"github.com/bwsalmon/silo/old/session"
	"github.com/bwsalmon/silo/old/silofile"
)

// The tests run against a kind (Kubernetes in Docker) cluster: one they
// create, or the one SILO_V2_KIND_CLUSTER names. They build two images
// and load them into it: the agent image (busybox, with testdata/fakecli
// as /usr/local/bin/claude) and the tunnel sidecar (cmd/silo-tunnel on
// scratch). Without Docker, kind or kubectl they skip, unless
// SILO_V2_REQUIRE_DOCKER is set, as it is in CI.

const (
	agentImage  = "silo-v2-k8s-agent:test"
	tunnelImage = "silo-v2-tunnel:test"
	namespace   = "silo-test"
)

type cluster struct {
	name, kubeconfig, dir string
	created               bool
}

var (
	fixOnce sync.Once
	fix     *cluster
	fixErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if fix != nil {
		if fix.created {
			exec.Command("kind", "delete", "cluster", "--name", fix.name).Run()
		}
		os.RemoveAll(fix.dir)
	}
	os.Exit(code)
}

func kind(t *testing.T) *cluster {
	t.Helper()
	var missing error
	if err := exec.Command("docker", "info").Run(); err != nil {
		missing = fmt.Errorf("docker is not working: %v", err)
	}
	for _, b := range []string{"kind", "kubectl"} {
		if _, err := exec.LookPath(b); err != nil && missing == nil {
			missing = err
		}
	}
	if missing != nil {
		if os.Getenv("SILO_V2_REQUIRE_DOCKER") != "" {
			t.Fatalf("SILO_V2_REQUIRE_DOCKER is set, but the kind tests cannot run: %v", missing)
		}
		t.Skipf("no kind cluster: %v", missing)
	}
	fixOnce.Do(func() { fix, fixErr = startCluster() })
	if fixErr != nil {
		t.Fatal(fixErr)
	}
	return fix
}

func run(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, errb.String())
	}
	return out.String(), nil
}

func startCluster() (*cluster, error) {
	dir, err := os.MkdirTemp("", "silo-kind-")
	if err != nil {
		return nil, err
	}
	c := &cluster{dir: dir, kubeconfig: filepath.Join(dir, "kubeconfig"), name: os.Getenv("SILO_V2_KIND_CLUSTER")}
	if c.name != "" {
		if _, err := run("", "kind", "export", "kubeconfig", "--name", c.name, "--kubeconfig", c.kubeconfig); err != nil {
			return c, err
		}
	} else {
		c.name = fmt.Sprintf("silo-v2-test-%d", os.Getpid())
		c.created = true
		if _, err := run("", "kind", "create", "cluster", "--name", c.name, "--kubeconfig", c.kubeconfig, "--wait", "180s"); err != nil {
			return c, err
		}
	}
	// Everything, silo's own kubectl calls included, uses this cluster.
	os.Setenv("KUBECONFIG", c.kubeconfig)
	_, file, _, _ := runtime.Caller(0)
	pkg := filepath.Dir(file)
	for name, build := range map[string]struct{ out, pkg, dockerfile string }{
		agentImage:  {"claude", "./testdata/fakecli", "FROM busybox:1.37\nCOPY claude /usr/local/bin/claude\n"},
		tunnelImage: {"silo-tunnel", "../../cmd/silo-tunnel", "FROM scratch\nCOPY silo-tunnel /silo-tunnel\nUSER 65532:65532\nENTRYPOINT [\"/silo-tunnel\"]\n"},
	} {
		ctx := filepath.Join(dir, build.out+"-image")
		os.MkdirAll(ctx, 0o755)
		cmd := exec.Command("go", "build", "-o", filepath.Join(ctx, build.out), build.pkg)
		cmd.Dir = pkg
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
		if out, err := cmd.CombinedOutput(); err != nil {
			return c, fmt.Errorf("building %s: %v: %s", build.pkg, err, out)
		}
		os.WriteFile(filepath.Join(ctx, "Dockerfile"), []byte(build.dockerfile), 0o644)
		if _, err := run(ctx, "docker", "build", "-q", "-t", name, "."); err != nil {
			return c, err
		}
		if _, err := run("", "kind", "load", "docker-image", "--name", c.name, name); err != nil {
			return c, err
		}
	}
	if out, err := run("", "kubectl", "--context", "kind-"+c.name, "create", "namespace", namespace); err != nil && !strings.Contains(err.Error(), "AlreadyExists") {
		return c, fmt.Errorf("%v: %s", err, out)
	}
	return c, nil
}

func (c *cluster) kubectl() *k8sloc.Kubectl {
	return &k8sloc.Kubectl{Context: "kind-" + c.name, Namespace: namespace}
}

func (c *cluster) get(t *testing.T, args ...string) string {
	t.Helper()
	k := c.kubectl()
	argv := k.Argv(args...)
	out, err := run("", argv[0], argv[1:]...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// gone waits until nothing labelled with session id is left.
func (c *cluster) gone(t *testing.T, id string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		out := c.get(t, "get", "pods,secrets", "-l", k8sloc.SessionLabel+"="+id, "-o", "name")
		if strings.TrimSpace(out) == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("left behind for session %s:\n%s", id, out)
		}
		time.Sleep(time.Second)
	}
}

type harness struct {
	t       *testing.T
	c       *cluster
	root    string
	environ []string
}

func newHarness(t *testing.T, c *cluster, fake string) *harness {
	root, err := os.MkdirTemp("/tmp", "silo-k-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	for _, s := range []string{"run", "state", "home"} {
		os.MkdirAll(filepath.Join(root, s), 0o700)
	}
	return &harness{t: t, c: c, root: root, environ: []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Join(root, "home"), "KUBECONFIG=" + c.kubeconfig,
		"XDG_RUNTIME_DIR=" + filepath.Join(root, "run"), "XDG_STATE_HOME=" + filepath.Join(root, "state"),
		"SILO_TEST_SECRET=hunter2-very-secret", "SILO_FAKE_CONFIG=" + fake,
	}}
}

const silofileText = `silo: v2
name: k8s
agent:
  cli: claude
  mode: %s
%slocation:
  kind: k8s
  k8s:
    context: kind-%s
    namespace: silo-test
    image: silo-v2-k8s-agent:test
    tunnel_image: silo-v2-tunnel:test
    resources: {cpu: 100m, memory: 128Mi}
remote_permissions: all
sandboxes:
  - name: build
    kind: container
    image: busybox:1.37
credentials:
  - name: secret
    from: {env: SILO_TEST_SECRET}
    to: [{agent: env, name: SECRET_TOKEN}]
  - name: fake
    from: {env: SILO_FAKE_CONFIG}
    to: [{agent: env, name: FAKE_CONFIG}]
`

func (h *harness) silofile(mode string) *silofile.Silofile {
	prompt := ""
	if mode == "headless" {
		prompt = "  prompt: \"hello from silo\"\n"
	}
	dir := filepath.Join(h.root, "silo")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "Silofile"), []byte(fmt.Sprintf(silofileText, mode, prompt, h.c.name)), 0o644)
	f, err := silofile.Load(dir)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := f.Validate(silofile.Options{}).Err(); err != nil {
		h.t.Fatal(err)
	}
	return f
}

type outcome struct {
	code   int
	err    error
	stderr string
}

func (h *harness) run(f *silofile.Silofile, mod func(*session.Options)) outcome {
	var stdout bytes.Buffer
	stderr := &lockedBuf{}
	opts := session.Options{
		File: f, Environ: h.environ, Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: stderr,
		NoGC: true, Grace: 2 * time.Second,
	}
	if mod != nil {
		mod(&opts)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	code, err := session.Run(ctx, opts)
	return outcome{code, err, stderr.String()}
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type fakeReport struct {
	Env    []string `json:"env"`
	Cwd    string   `json:"cwd"`
	Stdin  string   `json:"stdin"`
	TTY    bool     `json:"tty"`
	URL    string   `json:"url"`
	Tools  []string `json:"tools"`
	Result string   `json:"result"`
	Error  string   `json:"error"`
}

func (r fakeReport) env(k string) string {
	for _, kv := range r.Env {
		if key, v, _ := strings.Cut(kv, "="); key == k {
			return v
		}
	}
	return ""
}

// parseReport finds the fake CLI's report in what it printed.
func parseReport(t *testing.T, out string) fakeReport {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		_, js, ok := strings.Cut(sc.Text(), "FAKE-REPORT ")
		if !ok {
			continue
		}
		var r fakeReport
		if err := json.Unmarshal([]byte(strings.TrimSpace(js)), &r); err != nil {
			t.Fatalf("report %q: %v", js, err)
		}
		return r
	}
	t.Fatalf("the fake CLI printed no report:\n%s", out)
	return fakeReport{}
}

func fakeConfig(call bool, exit, sleep int) string {
	c := map[string]any{"exit": exit, "sleep": sleep}
	if call {
		c["call"] = "sandbox_exec"
		c["args"] = map[string]any{"sandbox": "build", "command": "echo from-the-sandbox"}
	}
	b, _ := json.Marshal(c)
	return string(b)
}

// TestHeadless runs a headless session whose CLI is in a pod: it reaches
// the gateway through the tunnel and calls a sandbox tool on the
// invoking machine, its credentials arrive through the Secret and never
// in the pod's spec, and teardown deletes the pod and the Secret.
func TestHeadless(t *testing.T) {
	c := kind(t)
	h := newHarness(t, c, fakeConfig(true, 7, 4))
	type seen struct{ pod, secret string }
	got := make(chan seen, 1)
	go func() {
		// While the CLI runs: the session's pod and Secret.
		for i := 0; i < 600; i++ {
			out, _ := run("", "kubectl", "--context", "kind-"+c.name, "-n", namespace, "get", "pods", "-l", k8sloc.ManagedByLabel+"="+k8sloc.ManagedBy, "-o", "json")
			if strings.Contains(out, `"Running"`) {
				sec, _ := run("", "kubectl", "--context", "kind-"+c.name, "-n", namespace, "get", "secrets", "-l", k8sloc.ManagedByLabel+"="+k8sloc.ManagedBy, "-o", "json")
				if strings.Contains(sec, "ownerReferences") {
					got <- seen{out, sec}
					return
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		got <- seen{}
	}()
	out := h.run(h.silofile("headless"), nil)
	if out.code != 7 {
		t.Fatalf("exit %d, want the CLI's 7: %v\n%s", out.code, out.err, out.stderr)
	}
	rep := parseReport(t, out.stderr)
	if rep.Error != "" || rep.URL != fmt.Sprintf("http://127.0.0.1:%d/mcp", k8sloc.GatewayPort) {
		t.Errorf("the fake CLI did not reach the gateway through the tunnel: %+v", rep)
	}
	if !strings.Contains(rep.Result, "from-the-sandbox") {
		t.Errorf("sandbox_exec through the tunnel: %q", rep.Result)
	}
	if rep.TTY || !strings.Contains(rep.Stdin, "hello from silo") {
		t.Errorf("headless: tty %v, stdin %q", rep.TTY, rep.Stdin)
	}
	if rep.Cwd != k8sloc.PodDir+"/work" {
		t.Errorf("cwd %s, want the private work directory", rep.Cwd)
	}
	if v := rep.env("SECRET_TOKEN"); v != "hunter2-very-secret" && !strings.Contains(v, "REDACTED") {
		t.Errorf("SECRET_TOKEN = %q: the delivered credential is not in the CLI's environment", v)
	}
	pod := rep.env("HOSTNAME")
	id := strings.TrimPrefix(pod, "silo-")
	if !strings.HasPrefix(pod, "silo-s-") {
		t.Fatalf("the CLI ran in %q, not a session's pod", pod)
	}
	s := <-got
	if s.pod == "" {
		t.Fatalf("never saw the session's running pod and its Secret")
	}
	if strings.Contains(s.pod, "hunter2") {
		t.Errorf("a credential is in the pod's spec")
	}
	if !strings.Contains(s.pod, `"`+k8sloc.SessionLabel+`":"`+id+`"`) && !strings.Contains(s.pod, `"`+k8sloc.SessionLabel+`": "`+id+`"`) {
		t.Errorf("the pod is not labelled with session %s", id)
	}
	if !strings.Contains(s.secret, `"kind": "Pod"`) {
		t.Errorf("the Secret is not owned by the pod:\n%s", s.secret)
	}
	c.gone(t, id)
}

// openPTY opens a pseudo-terminal pair.
func openPTY(t *testing.T) (ptm, pts *os.File) {
	t.Helper()
	ptm, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no ptys: %v", err)
	}
	var unlock int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, ptm.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		t.Fatal(e)
	}
	var n uint32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, ptm.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); e != 0 {
		t.Fatal(e)
	}
	pts, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ptm.Close(); pts.Close() })
	return ptm, pts
}

// TestInteractive runs the CLI on a TTY in the pod with kubectl exec -t.
func TestInteractive(t *testing.T) {
	c := kind(t)
	h := newHarness(t, c, fakeConfig(true, 3, 0))
	ptm, pts := openPTY(t)
	var term lockedBuf
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := ptm.Read(b)
			term.Write(b[:n])
			if err != nil {
				return
			}
		}
	}()
	out := h.run(h.silofile("interactive"), func(o *session.Options) { o.Stdin, o.Stdout, o.Stderr = pts, pts, pts })
	if out.code != 3 {
		t.Fatalf("exit %d, want the CLI's 3: %v\nterminal: %s", out.code, out.err, term.String())
	}
	time.Sleep(300 * time.Millisecond)
	text := term.String()
	if !strings.Contains(text, "fake claude on a tty") {
		t.Fatalf("the terminal saw %q", text)
	}
	rep := parseReport(t, strings.ReplaceAll(text, "\r", ""))
	if !rep.TTY || rep.Error != "" || !strings.Contains(rep.Result, "from-the-sandbox") {
		t.Errorf("interactive: %+v", rep)
	}
	c.gone(t, strings.TrimPrefix(rep.env("HOSTNAME"), "silo-"))
}

func newID() string {
	const a = "abcdefghijklmnopqrstuvwxyz234567"
	b := make([]byte, 12)
	rand.Read(b)
	for i := range b {
		b[i] = a[int(b[i])%len(a)]
	}
	return "s-" + string(b)
}

// TestGCRemovesLeakedPod leaves a session's pod behind, as a silo that
// was killed would, and silo gc's k8s part deletes it; a live session's
// pod is kept.
func TestGCRemovesLeakedPod(t *testing.T) {
	c := kind(t)
	k := *c.kubectl()
	leaked, live := newID(), newID()
	for _, id := range []string{leaked, live} {
		l := k8sloc.New(k8sloc.Config{Kubectl: k, Image: agentImage, TunnelImage: tunnelImage})
		s := &location.Session{ID: id, CLI: "claude", VersionArgs: []string{"--version"}, Secrets: map[string]string{"TOKEN": "x"}}
		if _, err := l.Prepare(context.Background(), s); err != nil {
			t.Fatal(err)
		}
		// No Teardown: silo died.
	}
	t.Cleanup(func() { k8sloc.DeleteSession(context.Background(), &k, live) })
	removed, err := k8sloc.GC(context.Background(), &k, k8sloc.GCOptions{Live: func(id string) bool { return id == live }})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"pod/silo-" + leaked: true, "secret/silo-" + leaked: true}
	for _, r := range removed {
		if !want[r] {
			t.Errorf("gc removed %s", r)
		}
		delete(want, r)
	}
	if len(want) != 0 {
		t.Errorf("gc did not remove %v (removed %v)", want, removed)
	}
	c.gone(t, leaked)
	if out := c.get(t, "get", "pods", "-l", k8sloc.SessionLabel+"="+live, "-o", "name"); !strings.Contains(out, "silo-"+live) {
		t.Errorf("gc removed the live session's pod")
	}
}

// TestDryRunChecks checks a good location and an image that does not
// exist.
func TestDryRunChecks(t *testing.T) {
	c := kind(t)
	s := &location.Session{CLI: "claude", VersionArgs: []string{"--version"}}
	cfg := k8sloc.Config{Kubectl: *c.kubectl(), Image: agentImage, TunnelImage: tunnelImage}
	for _, r := range k8sloc.Check(context.Background(), cfg, s) {
		if !r.OK {
			t.Errorf("%s: %s", r.Name, r.Detail)
		}
	}
	cfg.Image = "silo-v2-no-such-image:never"
	res := k8sloc.Check(context.Background(), cfg, s)
	last := res[len(res)-1]
	if last.OK || !strings.Contains(last.Name, "images can be pulled") || !strings.Contains(last.Detail, "silo-v2-no-such-image") {
		t.Errorf("a missing image: %+v", res)
	}
	cfg = k8sloc.Config{Kubectl: k8sloc.Kubectl{Context: "kind-" + c.name, Namespace: "silo-no-such-namespace"}, Image: agentImage}
	res = k8sloc.Check(context.Background(), cfg, s)
	if len(res) != 2 || res[1].OK {
		t.Errorf("a missing namespace: %+v", res)
	}
}
