package granule

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Where a wrap reads granule's half from, in the granule image: the
// guest parts pack-guest lays down (guestimage.PartsDir) and the
// kernel a guest rootfs without one boots (guestimage.RuntimeKernel).
const (
	PartsDir      = "/usr/local/share/granule/guest-parts"
	RuntimeKernel = GuestDir + "/vmlinux"
)

// WrapOptions is one wrap: the directories `granule wrap -rootfs`
// reads and the one it writes.
type WrapOptions struct {
	// Granule is the granule binary.
	Granule string
	// Rootfs is the wrapped image's filesystem and Config its OCI image
	// config, as a file.
	Rootfs, Config string
	// Guest is granule's guest rootfs, and Runtime the granule image's
	// filesystem, which has the parts and the kernel.
	Guest, Runtime string
	// Out is where the guest half is written, and Work where both
	// filesystems are staged while the disk is assembled.
	Out, Work string
	// Image is what the guest records the wrapped image as: its digest.
	Image string
}

// WrapArgs is `granule wrap` without docker (granule efc6acf): it
// copies the image's filesystem and config into a guest rootfs in Go
// and packs a guest disk with the host's mke2fs. Nothing from the
// wrapped image or the guest rootfs runs, and nothing chroots, so a
// wrap is not tenant code on the host. It needs root for ownership,
// which the holder's root is.
//
// local-backend.md §6 planned the wrap as a docker build through a
// private buildkitd; step 9's daemonless wrap made the builder
// unnecessary.
func WrapArgs(o WrapOptions) []string {
	return []string{o.Granule, "wrap",
		"-rootfs", o.Rootfs,
		"-config", o.Config,
		"-guest-rootfs", o.Guest,
		"-parts", filepath.Join(o.Runtime, PartsDir),
		"-runtime-kernel", filepath.Join(o.Runtime, RuntimeKernel),
		"-out", o.Out,
		"-work-dir", o.Work,
		o.Image,
	}
}

// WrapDir is image's wrap in the wraps directory dir: the guest half
// its VM containers mount at GuestDir, shared by all of them.
func WrapDir(dir, image string) string {
	return filepath.Join(dir, strings.ReplaceAll(image, ":", "-"))
}

// Wrapped is whether image has been wrapped in dir. A wrap is written
// beside its final name and renamed into place, so one that is there is
// whole.
func Wrapped(dir, image string) (bool, error) {
	_, err := os.Stat(filepath.Join(WrapDir(dir, image), "disk.img"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
