package granule_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/v2/internal/granuletest"
	"github.com/bwsalmon/silo/v2/sandbox/granule"
	"github.com/bwsalmon/silo/v2/silofile"
)

// TestCheckGranulefileLive asks the real granule what each kind of
// Granulefile builds, which is what silo validate refuses a mismatch
// on: a file with no VM instruction, or one whose VM stage is not the
// target, builds a container image; VM WRAP and VM PACK build a VM.
// It needs only the granule binary: a check builds nothing.
func TestCheckGranulefileLive(t *testing.T) {
	if _, err := exec.LookPath("granule"); err != nil && os.Getenv(granule.BinaryEnv) == "" {
		if os.Getenv(granuletest.RequireEnv) != "" {
			t.Fatalf("%s is set, but granule is not on PATH", granuletest.RequireEnv)
		}
		t.Skip("granule is not on PATH")
	}
	dir := t.TempDir()
	for _, tc := range []struct {
		name, file, target string
		vm                 bool
		wantErr            string
	}{
		{name: "dockerfile", file: "FROM busybox\nRUN true\n"},
		{name: "wrap", file: "FROM busybox\nCMD [\"sleep\", \"infinity\"]\nVM WRAP app AS guest\n", vm: true},
		{name: "pack", file: "FROM ghcr.io/gke-labs/granule/debian.rootfs:latest\nVM PACK\n", vm: true},
		{name: "vm stage not the target", file: "FROM busybox AS w\nCMD [\"true\"]\nVM WRAP AS m\nFROM busybox\nCOPY --from=w /bin/true /t\n"},
		{name: "vm stage targeted", file: "FROM busybox AS w\nCMD [\"true\"]\nVM WRAP AS m\nFROM busybox\n", target: "m", vm: true},
		{name: "invalid", file: "FROM busybox\nVM FLY\n", wantErr: "FLY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".Granulefile")
			if err := os.WriteFile(p, []byte(tc.file), 0o644); err != nil {
				t.Fatal(err)
			}
			vm, err := granule.CheckGranulefile(p, tc.target)
			if errors.Is(err, silofile.ErrUnchecked) {
				t.Fatal(err)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("CheckGranulefile = %v, %v; want an error naming %q", vm, err, tc.wantErr)
				}
				return
			}
			if err != nil || vm != tc.vm {
				t.Fatalf("CheckGranulefile = %v, %v; want %v", vm, err, tc.vm)
			}
		})
	}
}
