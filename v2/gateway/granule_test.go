package gateway_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/v2/credentials"
	"github.com/bwsalmon/silo/v2/gateway"
	"github.com/bwsalmon/silo/v2/gateway/audit"
	"github.com/bwsalmon/silo/v2/internal/granuletest"
	"github.com/bwsalmon/silo/v2/mcp"
	"github.com/bwsalmon/silo/v2/sandbox"
	"github.com/bwsalmon/silo/v2/sandbox/granule"
	"github.com/bwsalmon/silo/v2/silofile"
)

const granuleSilofile = `silo: v2
name: itest
agent: {cli: claude}
sandboxes:
  - name: vm
    kind: granule
    image: IMAGE
    workdir: /work
    cpus: 1
    memory: 1Gi
    env: {FOO: "bar 'quoted'"}
credentials:
  - name: token
    from: {env: SILO_ITEST_TOKEN}
    to:
      - {sandbox: vm, env: TOKEN}
      - {sandbox: vm, file: /run/secrets/token}
`

// TestGranuleSandboxThroughGateway is journey 7's sandbox: a granule
// microVM booted from a guest image, reached through sandbox_exec and
// the file tools, with credentials delivered, then destroyed.
func TestGranuleSandboxThroughGateway(t *testing.T) {
	image := granuletest.Require(t)
	b, err := granule.New()
	if err != nil {
		t.Fatal(err)
	}
	b.StateDir = t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "Silofile"), []byte(strings.Replace(granuleSilofile, "IMAGE", image, 1)), 0o644); err != nil {
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
	log, err := audit.Open(filepath.Join(t.TempDir(), audit.FileName), session, audit.Caller{CLI: "test", Host: "local"})
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
	spec, err := sandbox.SpecFor(f, "vm", session)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := credentials.ForSandbox(f.Credentials, vals, "vm")
	if err != nil {
		t.Fatal(err)
	}
	delivery.Apply(&spec)

	sb, err := b.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	vm := sb.(*granule.VM)
	destroyed := false
	defer func() {
		if !destroyed {
			sb.Destroy(context.Background())
		}
	}()
	if err := delivery.DeliverFiles(ctx, sb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(vm.Image.Digest, "sha256:") || vm.Image.Ref != image {
		t.Errorf("image resolved to %+v", vm.Image)
	}

	gw := gateway.New(log)
	gw.Register(gateway.SandboxTools(map[string]sandbox.Sandbox{"vm": sb})...)
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
		args["sandbox"] = "vm"
		r, err := client.CallTool(ctx, tool, args)
		if err != nil {
			t.Fatalf("%s %v: %v", tool, args, err)
		}
		return r
	}
	run := func(command string) string {
		t.Helper()
		r := call("sandbox_exec", map[string]any{"command": command})
		if r.IsError {
			t.Fatalf("sandbox_exec %q: %s", command, r.Text())
		}
		out, _, _ := strings.Cut(strings.TrimPrefix(r.Text(), "exit=0\nstdout:\n"), "stderr:\n")
		return strings.TrimSpace(out)
	}

	// A VM, not a container: its own kernel, and the guest's root.
	if got := run(`echo "$FOO $TOKEN"; pwd`); got != "bar 'quoted' "+secret+"\n/work" {
		t.Errorf("env and workdir: %q", got)
	}
	// A container shares the host's kernel, and its boot id.
	if got := run(`cat /proc/sys/kernel/random/boot_id`); got == hostBootID(t) {
		t.Errorf("the sandbox runs the host's kernel: boot id %q", got)
	}
	if r := call("sandbox_exec", map[string]any{"command": "echo out; echo err >&2; exit 3"}); !strings.Contains(r.Text(), "exit=3") ||
		!strings.Contains(r.Text(), "out") || !strings.Contains(r.Text(), "err") {
		t.Errorf("a failing command: %q", r.Text())
	}
	if got := run(`cat /run/secrets/token; echo; stat -c %a /run/secrets/token`); got != secret+"\n400" {
		t.Errorf("credential file: %q", got)
	}
	if err := sb.DeliverFile(ctx, "/run/secrets/token", []byte("rotated")); err != nil {
		t.Fatal(err)
	}
	if got := run(`cat /run/secrets/token; ls -a /run/secrets | wc -l`); got != "rotated3" {
		t.Errorf("after refresh: %q (want the new value and no temporary file left)", got)
	}
	// network: none is a network that reaches only this machine.
	if got := run(`curl -sS -m 3 -o /dev/null http://1.1.1.1/ 2>/dev/null && echo reached || echo unreachable`); !strings.HasSuffix(got, "unreachable") {
		t.Errorf("network none reached the internet: %q", got)
	}

	if r := call("sandbox_write_file", map[string]any{"path": "sub/a.txt", "content": "line\n", "mode": "0600"}); r.IsError {
		t.Fatalf("write: %q", r.Text())
	}
	if r := call("sandbox_read_file", map[string]any{"path": "/work/sub/a.txt"}); r.IsError || r.Text() != "line\n" {
		t.Errorf("read back: %q", r.Text())
	}
	if got := run(`stat -c %a sub/a.txt`); got != "600" {
		t.Errorf("mode %q", got)
	}
	if r := call("sandbox_list_dir", map[string]any{"path": "sub"}); r.IsError || !strings.Contains(r.Text(), "a.txt") {
		t.Errorf("list: %q", r.Text())
	}
	if r := call("sandbox_read_file", map[string]any{"path": "/nope"}); !r.IsError {
		t.Errorf("read of a missing file: %q", r.Text())
	}

	// A cancelled command is killed in the guest.
	cctx, ccancel := context.WithTimeout(ctx, 3*time.Second)
	_, err = sb.Exec(cctx, sandbox.ExecRequest{Argv: []string{"/bin/sh", "-c", "sleep 300"}})
	ccancel()
	if err == nil {
		t.Error("a cancelled exec returned no error")
	}
	if got := run(`pgrep -x sleep >/dev/null && echo running || echo gone`); got != "gone" {
		t.Errorf("the cancelled command is %s", got)
	}

	// Found again by its session label, then destroyed with its network.
	found, err := b.Find(ctx, session)
	if err != nil || len(found) != 1 || found[0].(*granule.VM).Name != "vm" || found[0].(*granule.VM).Network == "" {
		t.Fatalf("Find = %+v, %v", found, err)
	}
	if err := found[0].Destroy(ctx); err != nil {
		t.Fatal(err)
	}
	destroyed = true
	if found, err := b.Find(ctx, session); err != nil || len(found) != 0 {
		t.Errorf("after Destroy, Find = %+v, %v", found, err)
	}
	if exec.Command("docker", "network", "inspect", vm.Network).Run() == nil {
		t.Errorf("the sandbox's network %s is left", vm.Network)
	}
}

func hostBootID(t *testing.T) string {
	out, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}
