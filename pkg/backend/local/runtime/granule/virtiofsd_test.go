package granule

import (
	"context"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
)

func TestVirtiofsdPerVolume(t *testing.T) {
	dir := t.TempDir()
	spec := runtime.Spec{ID: "c", Process: runtime.Process{Args: []string{"true"}}, Mounts: []runtime.Mount{
		{Kind: runtime.MountVolume, Source: "/vol/a", Target: "/data"},
		{Kind: runtime.MountVolume, Source: "/vol/b", Target: "/etc/app", ReadOnly: true},
	}}
	v, err := NewVM(spec, "/w", dir, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var started [][]string
	var ls []net.Listener
	defer func() {
		for _, l := range ls {
			l.Close()
		}
	}()
	f := Virtiofsd{Binary: "/usr/libexec/virtiofsd", Start: func(_ context.Context, userns string, argv []string) error {
		if userns != "/ns/user" {
			t.Errorf("started in %q", userns)
		}
		started = append(started, argv)
		sock := strings.TrimPrefix(argv[1], "--socket-path=")
		l, err := net.Listen("unix", sock)
		if err == nil {
			ls = append(ls, l)
		}
		return err
	}}
	if err := f.StartAll(context.Background(), v, "/ns/user"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"/usr/libexec/virtiofsd", "--socket-path=" + filepath.Join(dir, "fs", "v0.sock"), "--shared-dir=/vol/a", "--sandbox=chroot", "--modcaps=-mknod"},
		{"/usr/libexec/virtiofsd", "--socket-path=" + filepath.Join(dir, "fs", "v1.sock"), "--shared-dir=/vol/b", "--sandbox=chroot", "--modcaps=-mknod", "--readonly"},
	}
	if !slices.EqualFunc(started, want, slices.Equal[[]string]) {
		t.Errorf("started %q, want %q", started, want)
	}

	// The sockets are the ones the VM container is told about, through
	// the directory it mounts at IODir.
	env, _ := v.Env()
	if !slices.Contains(env, "CHV_VOLUMES=tag=v0,socket=/run/silo/fs/v0.sock,destination=/data;tag=v1,socket=/run/silo/fs/v1.sock,destination=/etc/app,readonly=on") {
		t.Errorf("env %q", env)
	}
}

func TestVirtiofsdNoSocket(t *testing.T) {
	spec := runtime.Spec{ID: "c", Process: runtime.Process{Args: []string{"true"}},
		Mounts: []runtime.Mount{{Kind: runtime.MountVolume, Source: "/vol/a", Target: "/data"}}}
	v, err := NewVM(spec, "/w", t.TempDir(), "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	f := Virtiofsd{Binary: "virtiofsd", Wait: 100 * time.Millisecond, Start: func(context.Context, string, []string) error { return nil }}
	if err := f.StartAll(context.Background(), v, "/ns/user"); err == nil || !strings.Contains(err.Error(), "made no socket") {
		t.Errorf("a virtiofsd that never listens: %v", err)
	}
	if err := (Virtiofsd{}).StartAll(context.Background(), v, "/ns/user"); err == nil {
		t.Error("volumes without virtiofsd")
	}
}
