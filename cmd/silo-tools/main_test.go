package main

import (
	"debug/elf"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// silo-tools builds static, with no cgo, for both the architectures a
// tool VM may be (agents.md, "Creation": "static, amd64 or arm64"):
// the gateway serves these files to any image the facade allows.
func TestStaticBuilds(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-builds")
	}
	for arch, machine := range map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64} {
		out := filepath.Join(t.TempDir(), "silo-tools-linux-"+arch)
		cmd := exec.Command("go", "build", "-o", out, ".")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("building for %s: %v\n%s", arch, err, b)
		}
		f, err := elf.Open(out)
		if err != nil {
			t.Fatal(err)
		}
		if f.Machine != machine {
			t.Errorf("%s: machine %v", arch, f.Machine)
		}
		for _, p := range f.Progs {
			if p.Type == elf.PT_INTERP {
				t.Errorf("%s: dynamically linked", arch)
			}
		}
		f.Close()
	}
}

// Without -name-prefix, silo-tools reads the facade's silo-name-prefix
// from the metadata server.
func TestMetadataNamePrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "no flavor", http.StatusForbidden)
			return
		}
		w.Write([]byte("k7f3qa-\n"))
	}))
	defer srv.Close()
	old := metadataURL
	defer func() { metadataURL = old }()
	metadataURL = srv.URL
	if got := metadataNamePrefix(); got != "k7f3qa-" {
		t.Errorf("prefix %q, want k7f3qa-", got)
	}
	metadataURL = srv.URL + "/nowhere\x00"
	if got := metadataNamePrefix(); got != "" {
		t.Errorf("a bad URL: %q", got)
	}
}
