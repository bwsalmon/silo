package gateway_test

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/old/credentials"
	"github.com/bwsalmon/silo/old/gateway"
	"github.com/bwsalmon/silo/old/gateway/audit"
	"github.com/bwsalmon/silo/old/mcp"
	"github.com/bwsalmon/silo/old/sandbox"
	"github.com/bwsalmon/silo/old/sandbox/container"
	"github.com/bwsalmon/silo/old/silofile"
)

// testImage is small, and has sh, ls, head, cat, stat and sleep.
const testImage = "busybox:1.37"

// backend returns the container backend, skipping the test where there
// is no working runtime unless SILO_V2_REQUIRE_DOCKER is set, as it is
// in CI.
func backend(t *testing.T) *container.Backend {
	t.Helper()
	b, err := container.New()
	if err == nil {
		err = exec.Command(b.Runtime, "info").Run()
	}
	if err != nil {
		if os.Getenv("SILO_V2_REQUIRE_DOCKER") != "" {
			t.Fatalf("SILO_V2_REQUIRE_DOCKER is set, but there is no container runtime: %v", err)
		}
		t.Skipf("no container runtime: %v", err)
	}
	return b
}

func sessionID(t *testing.T) string {
	var b [8]byte
	rand.Read(b[:])
	return "s-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))[:12]
}

const silofileText = `silo: v2
name: itest
agent: {cli: claude}
sandboxes:
  - name: build
    kind: KIND
    image: busybox:1.37
    workdir: /work
    cpus: 1
    memory: 256Mi
    mounts:
      - {from: data, to: /data}
      - {from: out, to: /out, rw: true}
    env: {FOO: bar}
credentials:
  - name: token
    from: {env: SILO_ITEST_TOKEN}
    to:
      - {sandbox: build, env: TOKEN}
      - {sandbox: build, file: /run/secrets/token}
`

func TestContainerSandboxThroughGateway(t *testing.T) { testSandbox(t, "container") }

func TestGVisorSandboxThroughGateway(t *testing.T) { testSandbox(t, "gvisor") }

func testSandbox(t *testing.T, kind string) {
	b := backend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if kind == "gvisor" {
		ok, err := b.HasRunsc(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			// Refused clearly, before anything is created.
			_, err := b.Create(ctx, sandbox.SandboxSpec{Session: sessionID(t), Name: "x", Kind: "gvisor", Image: testImage})
			if !errors.Is(err, container.ErrNoRunsc) || !strings.Contains(err.Error(), "runsc") {
				t.Fatalf("gvisor without runsc: %v", err)
			}
			if os.Getenv("SILO_V2_REQUIRE_RUNSC") != "" {
				t.Fatal("SILO_V2_REQUIRE_RUNSC is set, but the runtime has no runsc")
			}
			t.Skip("no runsc")
		}
	}

	folder := t.TempDir()
	for _, d := range []string{"data", "out"} {
		if err := os.Mkdir(filepath.Join(folder, d), 0o777); err != nil {
			t.Fatal(err)
		}
	}
	// World-writable, so that a rootless runtime's mapped root can write.
	os.Chmod(filepath.Join(folder, "out"), 0o777)
	if err := os.WriteFile(filepath.Join(folder, "data", "hello.txt"), []byte("hello from the host\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "Silofile"), []byte(strings.Replace(silofileText, "KIND", kind, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := silofile.Load(folder)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Validate(silofile.Options{}).Err(); err != nil {
		t.Fatalf("the test's Silofile is invalid: %v", err)
	}

	session := sessionID(t)
	auditPath := filepath.Join(t.TempDir(), audit.FileName)
	log, err := audit.Open(auditPath, session, audit.Caller{CLI: "test", Host: "local"})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	const secret = "itest-secret-value-1234"
	res := &credentials.Resolver{Folder: folder, Getenv: func(k string) string {
		if k == "SILO_ITEST_TOKEN" {
			return secret
		}
		return ""
	}}
	vals, err := res.ResolveAll(ctx, f.Credentials, log)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := sandbox.SpecFor(f, "build", session)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := credentials.ForSandbox(f.Credentials, vals, "build")
	if err != nil {
		t.Fatal(err)
	}
	delivery.Apply(&spec)

	sb, err := b.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	destroyed := false
	defer func() {
		if !destroyed {
			sb.Destroy(context.Background())
		}
	}()
	if err := delivery.DeliverFiles(ctx, sb); err != nil {
		t.Fatal(err)
	}
	c := sb.(*container.Container)
	if !strings.Contains(c.Image.Digest, "sha256:") || c.Image.Ref != testImage {
		t.Fatalf("image resolved to %+v", c.Image)
	}
	if err := sandbox.RecordImages(folder, map[string]sandbox.Image{"build": c.Image}); err != nil {
		t.Fatal(err)
	}
	if imgs, err := sandbox.ReadImages(folder); err != nil || imgs["build"] != c.Image {
		t.Fatalf("lock file images %v, %v", imgs, err)
	}

	gw := gateway.New(log)
	gw.Register(gateway.SandboxTools(map[string]sandbox.Sandbox{"build": sb})...)
	srv, err := gw.Serve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	client := mcp.NewClient(srv.URL, srv.Token)
	if _, err := client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	call := func(tool string, args map[string]any) *mcp.CallResult {
		t.Helper()
		args["sandbox"] = "build"
		r, err := client.CallTool(ctx, tool, args)
		if err != nil {
			t.Fatalf("%s %v: %v", tool, args, err)
		}
		return r
	}
	exec := func(command string) string {
		t.Helper()
		r := call("sandbox_exec", map[string]any{"command": command})
		if r.IsError {
			t.Fatalf("sandbox_exec %q: %s", command, r.Text())
		}
		out, _, _ := strings.Cut(strings.TrimPrefix(r.Text(), "exit=0\nstdout:\n"), "stderr:\n")
		return strings.TrimSpace(out)
	}

	if got := exec(`echo "$FOO $TOKEN"; pwd`); got != "bar "+secret+"\n/work" {
		t.Errorf("env and workdir: %q", got)
	}
	if got := exec(`cat /run/secrets/token; echo; stat -c %a /run/secrets/token`); got != secret+"\n400" {
		t.Errorf("credential file: %q", got)
	}
	// A refresh replaces it.
	if err := sb.DeliverFile(ctx, "/run/secrets/token", []byte("rotated")); err != nil {
		t.Fatal(err)
	}
	if got := exec(`cat /run/secrets/token; ls -a /run/secrets | wc -l`); got != "rotated3" {
		t.Errorf("after refresh: %q (want the new value and no temporary file left)", got)
	}

	// Nothing of the host but the declared mounts.
	home, _ := os.UserHomeDir()
	wd, _ := os.Getwd()
	for _, p := range []string{"/var/run/docker.sock", "/run/docker.sock", "/run/podman/podman.sock", home, wd, folder, filepath.Join(folder, "Silofile")} {
		if got := exec(`if [ -e "` + p + `" ]; then echo present; else echo absent; fi`); got != "absent" {
			t.Errorf("host path %s is visible in the sandbox", p)
		}
	}
	if got := exec(`grep -c docker.sock /proc/mounts || true`); got != "0" {
		t.Errorf("a Docker socket is mounted: %q", got)
	}
	if got := exec(`ls /sys/class/net 2>/dev/null | grep -v '^lo$' | wc -l`); got != "0" {
		t.Errorf("network: none has interfaces besides lo: %q", got)
	}

	// The mounts: read-only unless rw.
	if r := call("sandbox_read_file", map[string]any{"path": "/data/hello.txt"}); r.IsError || r.Text() != "hello from the host\n" {
		t.Errorf("read of a mount: %+v %q", r, r.Text())
	}
	if r := call("sandbox_write_file", map[string]any{"path": "/data/x", "content": "no"}); !r.IsError {
		t.Errorf("wrote into a read-only mount: %q", r.Text())
	}
	if r := call("sandbox_write_file", map[string]any{"path": "/out/result.txt", "content": "made inside"}); r.IsError {
		t.Errorf("write into an rw mount: %q", r.Text())
	}
	if data, err := os.ReadFile(filepath.Join(folder, "out", "result.txt")); err != nil || string(data) != "made inside" {
		t.Errorf("the rw mount's file on the host: %q, %v", data, err)
	}

	// Write, read back and list, relative to the workdir.
	if r := call("sandbox_write_file", map[string]any{"path": "sub/a.txt", "content": "line\n", "mode": "0600"}); r.IsError {
		t.Fatalf("write: %q", r.Text())
	}
	if r := call("sandbox_read_file", map[string]any{"path": "/work/sub/a.txt"}); r.IsError || r.Text() != "line\n" {
		t.Errorf("read back: %q", r.Text())
	}
	if got := exec(`stat -c %a sub/a.txt`); got != "600" {
		t.Errorf("mode %q", got)
	}
	if r := call("sandbox_list_dir", map[string]any{"path": "sub"}); r.IsError || !strings.Contains(r.Text(), "a.txt") {
		t.Errorf("list: %q", r.Text())
	}
	if r := call("sandbox_list_dir", map[string]any{"path": "/nope"}); !r.IsError {
		t.Errorf("list of a missing directory: %q", r.Text())
	}
	if r := call("sandbox_read_file", map[string]any{"path": "/work"}); !r.IsError || !strings.Contains(r.Text(), "directory") {
		t.Errorf("read of a directory: %q", r.Text())
	}

	// Exit codes, stdin, per-call env and cwd.
	r := call("sandbox_exec", map[string]any{"command": "cat; echo $X; pwd; exit 7", "stdin": "in\n", "env": map[string]any{"X": "y"}, "cwd": "/tmp"})
	if !r.IsError || r.Text() != "exit=7\nstdout:\nin\ny\n/tmp\nstderr:\n" {
		t.Errorf("exec: %q", r.Text())
	}

	// A timeout kills the command inside the sandbox.
	start := time.Now()
	r = call("sandbox_exec", map[string]any{"command": "echo started; sleep 60", "timeout": 1})
	if !r.IsError || !strings.Contains(r.Text(), "timed out after 1s") || !strings.Contains(r.Text(), "started") {
		t.Errorf("timeout: %q", r.Text())
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("a 1s timeout took %s", d)
	}
	if got := exec(`ps 2>/dev/null | grep -c '[s]leep 60' || true`); got != "0" {
		t.Errorf("the timed-out command is still running: %q", got)
	}

	// The audit log never holds the credential.
	if data, err := os.ReadFile(auditPath); err != nil || strings.Contains(string(data), secret) || !strings.Contains(string(data), "[credential:token]") {
		t.Errorf("the credential reached the audit log")
	}

	// Find by label, and teardown.
	found, err := b.Find(ctx, session)
	if err != nil || len(found) != 1 || found[0].(*container.Container).Name != "build" {
		t.Fatalf("Find = %v, %v", found, err)
	}
	if err := found[0].Destroy(ctx); err != nil {
		t.Fatal(err)
	}
	destroyed = true
	if found, err := b.Find(ctx, session); err != nil || len(found) != 0 {
		t.Fatalf("after Destroy, Find = %v, %v", found, err)
	}
	if err := sb.Destroy(ctx); err != nil {
		t.Errorf("a second Destroy: %v", err)
	}
}
