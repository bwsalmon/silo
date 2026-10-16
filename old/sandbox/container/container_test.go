package container

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/old/sandbox"
)

// fakeRuntime writes a script standing in for the runtime CLI, which
// logs its arguments and answers `info` with the runtimes given.
func fakeRuntime(t *testing.T, runtimes string) (*Backend, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	script := "#!/bin/sh\necho \"$*\" >> " + logPath + "\ncase \"$1\" in info) printf '" + runtimes + "\\n';; image) echo sha256:abc;; run) echo cid;; esac\n"
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Backend{Runtime: path}, logPath
}

func TestGVisorRefusedWithoutRunsc(t *testing.T) {
	b, logPath := fakeRuntime(t, "io.containerd.runc.v2\\nrunc")
	_, err := b.Create(context.Background(), sandbox.SandboxSpec{Session: "s-x", Name: "build", Kind: "gvisor", Image: "busybox"})
	if !errors.Is(err, ErrNoRunsc) || !strings.Contains(err.Error(), `sandbox "build" is kind gvisor`) {
		t.Fatalf("error %v", err)
	}
	log, _ := os.ReadFile(logPath)
	if strings.Contains(string(log), "run ") || strings.Contains(string(log), "pull") {
		t.Fatalf("something was created or pulled:\n%s", log)
	}
}

func TestRunArguments(t *testing.T) {
	b, logPath := fakeRuntime(t, "runc\\nrunsc")
	spec := sandbox.SandboxSpec{
		Session: "s-abc", Name: "build", Kind: "gvisor", Image: "busybox", Workdir: "/w",
		CPUs: 1.5, Memory: 1 << 30, Network: "none",
		Mounts: []sandbox.Mount{{From: "/f/data", To: "/data"}, {From: "/f/out", To: "/out", RW: true}},
		Env:    map[string]string{"SECRET": "do-not-show"},
	}
	if _, err := b.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(logPath)
	var run string
	for _, l := range strings.Split(string(log), "\n") {
		if strings.HasPrefix(l, "run ") {
			run = l
		}
	}
	for _, want := range []string{
		"--name silo-s-abc-build", "--label silo.session=s-abc", "--label silo.sandbox=build",
		"--security-opt no-new-privileges", "--runtime=runsc", "--network none", "--cpus 1.5",
		"--memory 1073741824", "--workdir /w", "-e SECRET",
		"--mount type=bind,source=/f/data,target=/data,readonly",
		"--mount type=bind,source=/f/out,target=/out -e",
	} {
		if !strings.Contains(run, want) {
			t.Errorf("run lacks %q:\n%s", want, run)
		}
	}
	if strings.Contains(string(log), "do-not-show") {
		t.Errorf("a credential value is on the runtime's command line:\n%s", log)
	}
	for _, bad := range []string{"docker.sock", "--network host", "--privileged", "-v "} {
		if strings.Contains(run, bad) {
			t.Errorf("run has %q:\n%s", bad, run)
		}
	}
}

func TestBuildFromDockerfile(t *testing.T) {
	b, err := New()
	if err == nil {
		err = exec.Command(b.Runtime, "info").Run()
	}
	if err != nil {
		if os.Getenv("SILO_V2_REQUIRE_DOCKER") != "" {
			t.Fatalf("SILO_V2_REQUIRE_DOCKER is set, but there is no container runtime: %v", err)
		}
		t.Skipf("no container runtime: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dir := t.TempDir()
	dockerfile := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte("FROM busybox:1.37\nRUN echo built > /built\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	session := "s-build" + strings.ToLower(time.Now().Format("150405"))
	sb, err := b.Create(ctx, sandbox.SandboxSpec{Session: session, Name: "b", Kind: "container", Build: dockerfile})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Destroy(context.Background())
	c := sb.(*Container)
	if c.Image.Ref != dockerfile || !strings.HasPrefix(c.Image.Digest, "sha256:") {
		t.Errorf("image %+v", c.Image)
	}
	data, err := sb.ReadFile(ctx, "/built", 100)
	if err != nil || string(data) != "built\n" {
		t.Fatalf("ReadFile = %q, %v", data, err)
	}
	if data, err := sb.ReadFile(ctx, "/built", 3); err != nil || string(data) != "bui" {
		t.Fatalf("ReadFile with a limit = %q, %v", data, err)
	}
	if _, err := sb.ReadFile(ctx, "/nope", 100); err == nil {
		t.Fatal("ReadFile of a missing file succeeded")
	}
}
