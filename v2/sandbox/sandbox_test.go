package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/v2/silofile"
)

func TestSpecFor(t *testing.T) {
	folder := t.TempDir()
	os.Mkdir(filepath.Join(folder, "data"), 0o755)
	os.WriteFile(filepath.Join(folder, "Dockerfile"), []byte("FROM busybox\n"), 0o644)
	os.WriteFile(filepath.Join(folder, "Silofile"), []byte(`silo: v2
name: t
agent: {cli: claude}
sandboxes:
  - name: build
    kind: container
    build: Dockerfile
    memory: 1Gi
    network: open
    mounts: [{from: data, to: /data, rw: true}]
    env: {A: b}
`), 0o644)
	f, err := silofile.Load(folder)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := SpecFor(f, "build", "s-1")
	if err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(folder)
	if spec.Session != "s-1" || spec.Build != filepath.Join(real, "Dockerfile") || spec.Memory != 1<<30 || spec.Network != "open" ||
		len(spec.Mounts) != 1 || spec.Mounts[0] != (Mount{From: filepath.Join(real, "data"), To: "/data", RW: true}) || spec.Env["A"] != "b" {
		t.Fatalf("spec %+v", spec)
	}
	if _, err := SpecFor(f, "nope", "s-1"); err == nil {
		t.Fatal("SpecFor of an undeclared sandbox")
	}
}

func TestRecordImagesKeepsOtherKeys(t *testing.T) {
	folder := t.TempDir()
	os.Mkdir(filepath.Join(folder, ".silo"), 0o755)
	os.WriteFile(filepath.Join(folder, LockFile), []byte(`{"grants": {"x": 1}}`), 0o644)
	img := Image{Ref: "busybox", Digest: "busybox@sha256:00"}
	if err := RecordImages(folder, map[string]Image{"build": img}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(folder, LockFile))
	if !strings.Contains(string(data), `"grants"`) {
		t.Fatalf("lost other keys:\n%s", data)
	}
	got, err := ReadImages(folder)
	if err != nil || got["build"] != img {
		t.Fatalf("ReadImages = %v, %v", got, err)
	}
}
