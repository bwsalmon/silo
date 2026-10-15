package gvisor_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bwsalmon/silo/pkg/backend/local/runtime/gvisor"
)

// TestEtcFilesWrite: the files go into the root's /etc, made if the
// image has none; an image's symlink at one of the paths is replaced
// rather than written through, and an /etc that leads out of the root
// is refused with nothing written outside it.
func TestEtcFilesWrite(t *testing.T) {
	e := gvisor.EtcFiles{Hosts: "127.0.0.1\tlocalhost\n", Hostname: "c1\n", ResolvConf: "nameserver 10.255.255.1\n"}
	read := func(p string) string {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	bare := t.TempDir()
	if err := e.Write(bare, os.Getuid()); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"hosts": e.Hosts, "hostname": e.Hostname, "resolv.conf": e.ResolvConf} {
		if got := read(filepath.Join(bare, "etc", name)); got != want {
			t.Errorf("/etc/%s: %q, want %q", name, got, want)
		}
	}

	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	linked := t.TempDir()
	os.Mkdir(filepath.Join(linked, "etc"), 0o755)
	if err := os.Symlink(outside, filepath.Join(linked, "etc", "resolv.conf")); err != nil {
		t.Fatal(err)
	}
	if err := e.Write(linked, os.Getuid()); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(linked, "etc", "resolv.conf")); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("/etc/resolv.conf over a symlink: %v, %v", fi, err)
	}

	escaping := t.TempDir()
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(escaping, "etc")); err != nil {
		t.Fatal(err)
	}
	if err := e.Write(escaping, os.Getuid()); err == nil {
		t.Error("an /etc leading out of the root was written to")
	}
	if got := read(outside); got != "mine\n" {
		t.Errorf("the file outside the root: %q", got)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(outside), "hosts")); err == nil {
		t.Error("/etc/hosts was written outside the root")
	}
}
