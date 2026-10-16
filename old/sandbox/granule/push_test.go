package granule

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestPush pushes a small image to a registry in a container on this
// machine and checks it comes back by digest.
func TestPush(t *testing.T) {
	if err := exec.Command("docker", "info").Run(); err != nil {
		if os.Getenv("SILO_V2_REQUIRE_DOCKER") != "" {
			t.Fatalf("SILO_V2_REQUIRE_DOCKER is set, but docker info: %v", err)
		}
		t.Skipf("no Docker: %v", err)
	}
	docker := func(args ...string) string {
		t.Helper()
		var stderr strings.Builder
		cmd := exec.Command("docker", args...)
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("docker %v: %v: %s", args, err, stderr.String())
		}
		return strings.TrimSpace(string(out))
	}
	docker("pull", "-q", "registry:2")
	id := docker("run", "-d", "--rm", "-p", "127.0.0.1::5000", "registry:2")
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", id).Run() })
	port := docker("port", id, "5000/tcp")
	port = port[strings.LastIndex(port, ":")+1:]
	docker("pull", "-q", "busybox:1.37")
	local := "silo-granulefile:push-test"
	docker("tag", "busybox:1.37", local)
	t.Cleanup(func() { exec.Command("docker", "rmi", local).Run() })
	repo := "localhost:" + port + "/silo/dev"
	b := &Backend{Docker: "docker"}
	ctx := context.Background()
	var ref string
	var err error
	for i := 0; i < 20; i++ { // the registry may still be starting
		if ref, err = b.Push(ctx, local, repo); err == nil {
			break
		}
		exec.Command("sleep", "0.5").Run()
	}
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ref, repo+"@sha256:") {
		t.Fatalf("Push = %q, want %s@sha256:...", ref, repo)
	}
	docker("rmi", repo+":push-test")
	docker("pull", "-q", ref)
	docker("rmi", ref)
}
