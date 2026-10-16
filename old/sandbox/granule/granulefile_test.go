package granule

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/old/sandbox"
)

// contextDir writes a silo folder with a Granulefile in sandboxes/ and
// returns the Granulefile's path.
func contextDir(t *testing.T) string {
	t.Helper()
	folder := t.TempDir()
	for name, data := range map[string]string{
		"Silofile":                  "silo: v2\n",
		"README.md":                 "outside the context\n",
		"sandboxes/dev.Granulefile": "FROM alpine\nCOPY files/ /files/\nVM WRAP\n",
		"sandboxes/files/a.txt":     "a\n",
		"sandboxes/files/b.sh":      "#!/bin/sh\n",
	} {
		p := filepath.Join(folder, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(folder, "sandboxes", "dev.Granulefile")
}

func TestCacheKey(t *testing.T) {
	key := func(t *testing.T, gf, target string, args map[string]string) string {
		t.Helper()
		k, err := CacheKey(gf, target, args)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(k, "sha256:") || len(k) != len("sha256:")+64 {
			t.Fatalf("key = %q", k)
		}
		return k
	}
	write := func(t *testing.T, path, data string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	args := map[string]string{"A": "1", "B": "2"}
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, gf string) (target string, args map[string]string)
		same   bool
	}{
		{name: "nothing", same: true, change: func(t *testing.T, gf string) (string, map[string]string) {
			return "", map[string]string{"B": "2", "A": "1"}
		}},
		{name: "the Granulefile", change: func(t *testing.T, gf string) (string, map[string]string) {
			write(t, gf, "FROM alpine\nCOPY files/ /files/\nVM PACK\n", 0o644)
			return "", args
		}},
		{name: "a context file", change: func(t *testing.T, gf string) (string, map[string]string) {
			write(t, filepath.Join(filepath.Dir(gf), "files", "a.txt"), "A\n", 0o644)
			return "", args
		}},
		{name: "a new context file", change: func(t *testing.T, gf string) (string, map[string]string) {
			write(t, filepath.Join(filepath.Dir(gf), "files", "c.txt"), "", 0o644)
			return "", args
		}},
		{name: "a renamed context file", change: func(t *testing.T, gf string) (string, map[string]string) {
			d := filepath.Join(filepath.Dir(gf), "files")
			if err := os.Rename(filepath.Join(d, "a.txt"), filepath.Join(d, "z.txt")); err != nil {
				t.Fatal(err)
			}
			return "", args
		}},
		{name: "an executable bit", change: func(t *testing.T, gf string) (string, map[string]string) {
			if err := os.Chmod(filepath.Join(filepath.Dir(gf), "files", "b.sh"), 0o755); err != nil {
				t.Fatal(err)
			}
			return "", args
		}},
		{name: "a symlink", change: func(t *testing.T, gf string) (string, map[string]string) {
			if err := os.Symlink("a.txt", filepath.Join(filepath.Dir(gf), "files", "l")); err != nil {
				t.Fatal(err)
			}
			return "", args
		}},
		{name: "an arg value", change: func(t *testing.T, gf string) (string, map[string]string) {
			return "", map[string]string{"A": "1", "B": "3"}
		}},
		{name: "an arg name", change: func(t *testing.T, gf string) (string, map[string]string) {
			return "", map[string]string{"A": "1", "C": "2"}
		}},
		{name: "args that run together", change: func(t *testing.T, gf string) (string, map[string]string) {
			return "", map[string]string{"A": "1B", "": "2"}
		}},
		{name: "the target", change: func(t *testing.T, gf string) (string, map[string]string) {
			return "guest", args
		}},
		{name: "a file outside the context", same: true, change: func(t *testing.T, gf string) (string, map[string]string) {
			folder := filepath.Dir(filepath.Dir(gf))
			write(t, filepath.Join(folder, "README.md"), "changed\n", 0o644)
			write(t, filepath.Join(folder, "prompts", "task.md"), "new\n", 0o644)
			write(t, filepath.Join(folder, ".silo", "lock.json"), "{}\n", 0o644)
			return "", args
		}},
		{name: "silo's and granule's own files in the context", same: true, change: func(t *testing.T, gf string) (string, map[string]string) {
			write(t, filepath.Join(filepath.Dir(gf), ".silo", "lock.json"), "{}\n", 0o644)
			write(t, filepath.Join(filepath.Dir(gf), ".granule-build-123", "Dockerfile"), "FROM x\n", 0o644)
			return "", args
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gf := contextDir(t)
			before := key(t, gf, "", args)
			target, a := tc.change(t, gf)
			after := key(t, gf, target, a)
			if (before == after) != tc.same {
				t.Errorf("key before %s, after %s: same = %v, want %v", before, after, before == after, tc.same)
			}
		})
	}
}

// fakeGranulefileBackend is a backend whose granule answers check with
// plan, records build's argv and "builds" by making docker answer
// image inspect for the tag it was given.
func fakeGranulefileBackend(t *testing.T, plan string) (*Backend, string, string) {
	t.Helper()
	dir := t.TempDir()
	images := filepath.Join(dir, "images")
	glog, dlog := filepath.Join(dir, "granule.log"), filepath.Join(dir, "docker.log")
	granule := fakeCLI(t, dir, "granule", glog, `case "$1 $2" in
"granulefile check") printf '%s\n' "x: ok" "`+plan+`" "nothing was built: this checks the file.";;
"granulefile build") while [ $# -gt 0 ]; do [ "$1" = -t ] && echo "$2" >> `+images+`; shift; done; echo building;;
esac`)
	docker := fakeCLI(t, dir, "docker", dlog, `case "$1" in
image) for last; do :; done; grep -qxF "$last" `+images+` 2>/dev/null || { echo "No such image: $last" >&2; exit 1; }; echo sha256:built;;
esac`)
	return &Backend{Granule: granule, Docker: docker}, glog, dlog
}

func TestBuildImageCaches(t *testing.T) {
	b, glog, _ := fakeGranulefileBackend(t, "target: guest, which produces a granule VM image")
	gf := contextDir(t)
	spec := sandbox.SandboxSpec{Name: "dev", Kind: "granule", Build: gf, Target: "guest", BuildArgs: map[string]string{"V": "1", "A": "x y"}}
	key, err := CacheKey(gf, "guest", spec.BuildArgs)
	if err != nil {
		t.Fatal(err)
	}
	for run := 0; run < 2; run++ {
		img, tag, err := b.BuildImage(context.Background(), spec)
		if err != nil {
			t.Fatal(err)
		}
		if want := (sandbox.Image{Ref: gf, Digest: "sha256:built", Key: key}); img != want || tag != BuiltTag(key) {
			t.Errorf("run %d: BuildImage = %+v, %s; want %+v, %s", run, img, tag, want, BuiltTag(key))
		}
	}
	var builds [][]string
	for _, c := range calls(t, glog) {
		if c[0] == "granulefile" && c[1] == "build" {
			builds = append(builds, c)
		}
	}
	want := []string{"granulefile", "build", "-f", gf, "-t", BuiltTag(key), "-target", "guest", "-build-arg", "A=x y", "-build-arg", "V=1", filepath.Dir(gf)}
	if len(builds) != 1 || strings.Join(builds[0], "\x00") != strings.Join(want, "\x00") {
		t.Errorf("builds = %q, want one: %q", builds, want)
	}
	// The context changes: a new key, and a build.
	if err := os.WriteFile(filepath.Join(filepath.Dir(gf), "files", "a.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	img, _, err := b.BuildImage(context.Background(), spec)
	if err != nil || img.Key == key {
		t.Fatalf("after a context change: %+v, %v", img, err)
	}
}

// TestBuildImageRefusesContainer: a Granulefile whose target builds a
// container image is refused for kind granule before anything is built.
func TestBuildImageRefusesContainer(t *testing.T) {
	b, glog, _ := fakeGranulefileBackend(t, "target: stage 0, which produces an OCI container image")
	gf := contextDir(t)
	_, err := b.Create(context.Background(), sandbox.SandboxSpec{Session: "s", Name: "dev", Kind: "granule", Build: gf})
	if err == nil || !strings.Contains(err.Error(), "builds a container image, not a granule VM image") {
		t.Fatalf("Create = %v", err)
	}
	for _, c := range calls(t, glog) {
		if c[0] != "granulefile" || c[1] != "check" {
			t.Errorf("granule ran %q, want only granulefile check", c)
		}
	}
}

func TestCheck(t *testing.T) {
	for _, tc := range []struct {
		plan    string
		vm      bool
		wantErr string
	}{
		{plan: "target: guest, which produces a granule VM image", vm: true},
		{plan: "target: stage 1, which produces an OCI container image"},
		{plan: "something else", wantErr: "did not say what"},
	} {
		b, glog, _ := fakeGranulefileBackend(t, tc.plan)
		vm, err := b.Check(context.Background(), "/f/sandboxes/dev.Granulefile", "guest")
		if vm != tc.vm || (err == nil) != (tc.wantErr == "") || (err != nil && !strings.Contains(err.Error(), tc.wantErr)) {
			t.Errorf("Check with %q = %v, %v", tc.plan, vm, err)
		}
		got := calls(t, glog)
		if want := "granulefile check -f /f/sandboxes/dev.Granulefile -target guest /f/sandboxes"; len(got) != 1 || strings.Join(got[0], " ") != want {
			t.Errorf("granule calls = %q, want %q", got, want)
		}
	}
}
