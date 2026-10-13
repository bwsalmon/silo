package granule

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestWrapArgs(t *testing.T) {
	got := WrapArgs(WrapOptions{Granule: "/opt/granule", Rootfs: "/w/rootfs", Config: "/w/config.json",
		Guest: "/w/guest", Runtime: "/w/granule", Out: "/w/out", Work: "/w/stage", Image: "sha256:ab"})
	want := []string{"/opt/granule", "wrap",
		"-rootfs", "/w/rootfs", "-config", "/w/config.json", "-guest-rootfs", "/w/guest",
		"-parts", "/w/granule/usr/local/share/granule/guest-parts",
		"-runtime-kernel", "/w/granule/var/lib/granule/guest/vmlinux",
		"-out", "/w/out", "-work-dir", "/w/stage", "sha256:ab"}
	if !slices.Equal(got, want) {
		t.Errorf("WrapArgs\n got %q\nwant %q", got, want)
	}
}

func TestWrapped(t *testing.T) {
	dir := t.TempDir()
	if w := WrapDir(dir, "sha256:ab"); w != filepath.Join(dir, "sha256-ab") {
		t.Errorf("WrapDir %s", w)
	}
	if ok, err := Wrapped(dir, "sha256:ab"); ok || err != nil {
		t.Errorf("before: %v %v", ok, err)
	}
	if err := os.MkdirAll(WrapDir(dir, "sha256:ab"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(WrapDir(dir, "sha256:ab"), "disk.img"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := Wrapped(dir, "sha256:ab"); !ok || err != nil {
		t.Errorf("after: %v %v", ok, err)
	}
}
