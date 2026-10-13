package fake_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"syscall"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime/fake"
)

const image = "sha256:0123456789abcdef"

func spec(id string) runtime.Spec {
	return runtime.Spec{ID: id, Silo: "s1", Image: image, NetNS: "/run/silo/ns/" + id + "/net",
		Process: runtime.Process{Args: []string{"sleep", "inf"}, Stdin: true},
		Labels:  map[string]string{runtime.LabelSilo: "s1", runtime.LabelName: "web"},
		Mounts: []runtime.Mount{{Kind: runtime.MountVolume, Source: "/data/silos/s1/volumes/v1", Target: "/data"},
			{Kind: runtime.MountTmpfs, Target: "/tmp"}}}
}

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	d := fake.New(runtime.Sandbox)
	d.Volumes = "/data/silos"
	if err := d.Create(ctx, spec("c1")); err == nil {
		t.Fatal("created from an image that was not prepared")
	}
	if err := d.Prepare(ctx, image); err != nil {
		t.Fatal(err)
	}
	if err := d.Create(ctx, spec("c1")); err != nil {
		t.Fatal(err)
	}
	if err := d.Pause(ctx, "c1"); !errors.Is(err, runtime.ErrState) {
		t.Errorf("pause of a created container: %v", err)
	}
	if err := d.Start(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, "c1"); !errors.Is(err, runtime.ErrState) {
		t.Errorf("delete of a running container: %v", err)
	}
	if code, err := d.Exec(ctx, "c1", runtime.Process{Args: []string{"true"}}, runtime.ExecIO{}); err != nil || code != 0 {
		t.Errorf("exec: %d %v", code, err)
	}
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	tw.WriteHeader(&tar.Header{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "etc/motd", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2})
	tw.Write([]byte("hi"))
	tw.Close()
	if err := d.CopyIn(ctx, "c1", "/", &tb); err != nil {
		t.Fatal(err)
	}
	rc, err := d.CopyOut(ctx, "c1", "/etc/motd")
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(rc)
	if h, err := tr.Next(); err != nil || h.Name != "motd" {
		t.Fatalf("copy out: %v %v", h, err)
	}
	if b, _ := io.ReadAll(tr); string(b) != "hi" {
		t.Errorf("copy out read %q", b)
	}
	s, _ := d.Streams(ctx, "c1")
	io.WriteString(s.Stdin, "input")
	if d.Stdin("c1") != "input" {
		t.Errorf("stdin %q", d.Stdin("c1"))
	}
	wait := make(chan runtime.ExitStatus)
	go func() { st, _ := d.Wait(ctx, "c1"); wait <- st }()
	if err := d.Stop(ctx, "c1", time.Second); err != nil {
		t.Fatal(err)
	}
	if st := <-wait; st.Code != 128+int(syscall.SIGTERM) || st.Signal != syscall.SIGTERM {
		t.Errorf("exit after stop: %+v", st)
	}
	cs, _ := d.List(ctx)
	if len(cs) != 1 || cs[0].State != runtime.Stopped || cs[0].Labels[runtime.LabelName] != "web" {
		t.Errorf("list: %+v", cs)
	}
	if err := d.Delete(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Wait(ctx, "c1"); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("wait on a deleted container: %v", err)
	}
}

func TestValidateRefuses(t *testing.T) {
	for name, edit := range map[string]func(*runtime.Spec){
		"no netns":               func(s *runtime.Spec) { s.NetNS = "" },
		"host path volume":       func(s *runtime.Spec) { s.Mounts[0].Source = "/etc" },
		"escaping volume":        func(s *runtime.Spec) { s.Mounts[0].Source = "/data/silos/../../etc" },
		"bind mount":             func(s *runtime.Spec) { s.Mounts[0].Kind = "bind" },
		"tmpfs with a source":    func(s *runtime.Spec) { s.Mounts[1].Source = "/data/silos/x" },
		"image by tag":           func(s *runtime.Spec) { s.Image = "alpine:latest" },
		"label for another silo": func(s *runtime.Spec) { s.Labels[runtime.LabelSilo] = "s2" },
		"relative target":        func(s *runtime.Spec) { s.Mounts[1].Target = "tmp" },
	} {
		s := spec("c1")
		edit(&s)
		if err := s.Validate("/data/silos"); err == nil {
			t.Errorf("%s: valid", name)
		}
	}
	s := spec("c1")
	if err := s.Validate("/data/silos"); err != nil {
		t.Errorf("the good spec: %v", err)
	}
}
