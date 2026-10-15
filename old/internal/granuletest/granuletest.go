// Package granuletest is what tests that boot real granule VMs share:
// whether this machine can, and which guest image to boot.
package granuletest

import (
	"os"
	"os/exec"
	"testing"

	"github.com/bwsalmon/silo/old/sandbox/granule"
)

// DefaultImage is grain's published guest (grain
// docs/granule-guest-images.md): a granule VM image of Debian 12 with
// git, curl and procps, which grain's own sandboxes boot.
const DefaultImage = "ghcr.io/bwsalmon/grain/guest:latest"

// ImageEnv overrides DefaultImage.
const ImageEnv = "SILO_V2_GRANULE_IMAGE"

// RequireEnv makes a machine that cannot boot a granule VM a failure
// rather than a skip.
const RequireEnv = "SILO_V2_REQUIRE_GRANULE"

// Require skips t unless this machine has granule (on PATH, or
// SILO_GRANULE), Docker and a /dev/kvm it may open -- or fails it, when
// SILO_V2_REQUIRE_GRANULE is set -- and pulls the guest image. It
// returns the image.
func Require(t *testing.T) string {
	t.Helper()
	missing := func(format string, args ...any) {
		t.Helper()
		if os.Getenv(RequireEnv) != "" {
			t.Fatalf(RequireEnv+" is set, but "+format, args...)
		}
		t.Skipf("granule VMs cannot run here ("+RequireEnv+"=1 makes this a failure): "+format, args...)
	}
	if _, err := granule.New(); err != nil {
		missing("%v", err)
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		missing("docker info: %v", err)
	}
	kvm, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		missing("KVM: %v", err)
	}
	kvm.Close()
	image := os.Getenv(ImageEnv)
	if image == "" {
		image = DefaultImage
	}
	if exec.Command("docker", "image", "inspect", image).Run() != nil {
		if out, err := exec.Command("docker", "pull", "-q", image).CombinedOutput(); err != nil {
			t.Fatalf("pulling the guest image %s: %v\n%s", image, err, out)
		}
	}
	return image
}
