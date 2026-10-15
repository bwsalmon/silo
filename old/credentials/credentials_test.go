package credentials

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/old/silofile"
)

type registry map[string]string

func (r registry) Register(name, value string) { r[name] = value }

func TestEnvAndFileSources(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "open"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{Folder: dir, Getenv: func(k string) string {
		if k == "TOKEN" {
			return "env-secret"
		}
		return ""
	}}
	reg := registry{}
	vals, err := r.ResolveAll(context.Background(), []silofile.Credential{
		{Name: "a", From: silofile.Source{Env: "TOKEN"}},
		{Name: "b", From: silofile.Source{File: "token"}},
		{Name: "c", From: silofile.Source{File: filepath.Join(dir, "token")}},
	}, reg)
	if err != nil {
		t.Fatal(err)
	}
	if vals["a"].Secret != "env-secret" || vals["b"].Secret != "file-secret" || vals["c"].Secret != "file-secret" {
		t.Fatalf("values %+v", vals)
	}
	if reg["a"] != "env-secret" || reg["b"] != "file-secret" {
		t.Fatalf("registered %v: every value must be registered for redaction", reg)
	}

	for _, tc := range []struct {
		src  silofile.Source
		want string
	}{
		{silofile.Source{Env: "MISSING"}, "MISSING is not set"},
		{silofile.Source{File: "open"}, "readable by other users"},
		{silofile.Source{File: "nope"}, "no such file"},
		{silofile.Source{Store: "x"}, "not yet supported"},
		{silofile.Source{Ambient: "nope"}, "unknown ambient source"},
	} {
		_, err := r.Resolve(context.Background(), silofile.Credential{Name: "x", From: tc.src})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Resolve(%+v) error %v, want %q", tc.src, err, tc.want)
		}
	}
	if _, err := r.Resolve(context.Background(), silofile.Credential{Name: "x", From: silofile.Source{Store: "x"}}); !errors.Is(err, ErrStoreUnsupported) {
		t.Errorf("store error %v is not ErrStoreUnsupported", err)
	}
}

func TestAmbientSourcesAndCache(t *testing.T) {
	now := time.Unix(0, 0)
	calls := map[string]int{}
	r := &Resolver{
		Host: "ghe.example",
		Now:  func() time.Time { return now },
		Command: func(_ context.Context, stdin, name string, args ...string) (string, string, error) {
			calls[name]++
			switch name {
			case "gh":
				if strings.Join(args, " ") != "auth token --hostname ghe.example" {
					t.Errorf("gh %v", args)
				}
				return "gho_token\n", "", nil
			case "git":
				if !strings.Contains(stdin, "host=ghe.example") {
					t.Errorf("git credential fill stdin %q", stdin)
				}
				return "protocol=https\nhost=ghe.example\nusername=u\npassword=git-pass\n", "", nil
			case "gcloud":
				return "ya29.token\n", "", nil
			}
			return "", "", errors.New("unexpected")
		},
	}
	ctx := context.Background()
	for _, tc := range []struct{ source, want string }{
		{"gh", "gho_token"}, {"git-credential", "git-pass"}, {"gcloud", "ya29.token"},
	} {
		v, err := r.Resolve(ctx, silofile.Credential{Name: "c", From: silofile.Source{Ambient: tc.source}})
		if err != nil || v.Secret != tc.want {
			t.Fatalf("%s: %+v, %v", tc.source, v, err)
		}
		if v.SilosOwn != (tc.source == "gcloud") {
			t.Errorf("%s: SilosOwn %v", tc.source, v.SilosOwn)
		}
	}
	r.Resolve(ctx, silofile.Credential{Name: "c", From: silofile.Source{Ambient: "gh"}})
	if calls["gh"] != 1 {
		t.Fatalf("gh asked %d times within the TTL, want once", calls["gh"])
	}
	now = now.Add(AmbientTTL + time.Second)
	r.Resolve(ctx, silofile.Credential{Name: "c", From: silofile.Source{Ambient: "gh"}})
	if calls["gh"] != 2 {
		t.Fatalf("gh asked %d times after the TTL, want twice", calls["gh"])
	}

	failing := &Resolver{Command: func(context.Context, string, string, ...string) (string, string, error) {
		return "", "not logged in", errors.New("exit status 1")
	}}
	_, err := failing.Resolve(ctx, silofile.Credential{Name: "c", From: silofile.Source{Ambient: "gh"}})
	if err == nil || !strings.Contains(err.Error(), "gh auth login --hostname github.com") {
		t.Fatalf("gh failure: %v", err)
	}
}

func TestForSandbox(t *testing.T) {
	creds := []silofile.Credential{
		{Name: "a", To: []silofile.Destination{{Sandbox: "build", Env: "A"}, {Sandbox: "other", Env: "A"}}},
		{Name: "b", To: []silofile.Destination{{Sandbox: "build", File: "/run/b"}, {Proxy: "git"}}},
		{Name: "g"},
	}
	vals := map[string]Value{"a": {Name: "a", Secret: "sa"}, "b": {Name: "b", Secret: "sb"}, "g": {Name: "g", Secret: "sg", SilosOwn: true}}
	d, err := ForSandbox(creds, vals, "build")
	if err != nil {
		t.Fatal(err)
	}
	if d.Env["A"] != "sa" || len(d.Env) != 1 || len(d.Files) != 1 || d.Files[0].Path != "/run/b" || string(d.Files[0].Data) != "sb" {
		t.Fatalf("delivery %+v", d)
	}
	creds[2].To = []silofile.Destination{{Sandbox: "build", Env: "G"}}
	if _, err := ForSandbox(creds, vals, "build"); err == nil || !strings.Contains(err.Error(), "never delivered") {
		t.Fatalf("gcloud delivered: %v", err)
	}
}
