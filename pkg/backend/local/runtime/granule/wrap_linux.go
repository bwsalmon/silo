//go:build linux

package granule

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/opencontainers/image-spec/identity"
)

// WrapArg is the subcommand that runs WrapMain: `silod runtime-wrap`.
const WrapArg = "runtime-wrap"

// WrapMain is `silod runtime-wrap`, run inside the holder: it wraps an
// image into its guest half in the wraps directory. Each of the three
// filesystems a wrap reads (the image, granule's guest rootfs and the
// granule image) is a read-only view of its containerd snapshot,
// mounted here because mounting needs the holder's mount namespace and
// its root. The guest half is written beside its final name and renamed
// into place, so a wrap that is there is whole (Wrapped).
func WrapMain(args []string) error {
	flags := flag.NewFlagSet(WrapArg, flag.ContinueOnError)
	address := flags.String("address", "", "silo-containerd's socket")
	ns := flags.String("namespace", "", "the containerd namespace")
	snapshotter := flags.String("snapshotter", "overlayfs", "the snapshotter")
	image := flags.String("image", "", "the image to wrap, by its name in containerd")
	digest := flags.String("digest", "", "the image's digest, which names its wrap")
	guest := flags.String("guest-image", "", "granule's guest rootfs image, by its name in containerd")
	runtimeImage := flags.String("granule-image", "", "the granule image, by its name in containerd")
	granule := flags.String("granule", "", "the granule binary")
	wraps := flags.String("wraps", "", "the wraps directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *address == "" || *ns == "" || *image == "" || *digest == "" || *guest == "" || *runtimeImage == "" || *granule == "" || *wraps == "" {
		return errors.New("runtime-wrap: every flag is required")
	}
	if ok, err := Wrapped(*wraps, *digest); err != nil || ok {
		return err
	}
	c, err := containerd.New(*address)
	if err != nil {
		return err
	}
	defer c.Close()
	ctx := namespaces.WithNamespace(context.Background(), *ns)
	ctx, done, err := c.WithLease(ctx)
	if err != nil {
		return err
	}
	defer done(context.WithoutCancel(ctx))

	work, err := os.MkdirTemp(*wraps, ".wrap-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	view := func(name, dir string) (func(), error) {
		img, err := c.GetImage(ctx, name)
		if err != nil {
			return nil, err
		}
		if ok, _ := img.IsUnpacked(ctx, *snapshotter); !ok {
			if err := img.Unpack(ctx, *snapshotter); err != nil {
				return nil, fmt.Errorf("unpacking %s: %w", name, err)
			}
		}
		diffs, err := img.RootFS(ctx)
		if err != nil {
			return nil, err
		}
		key := filepath.Base(work) + "-" + filepath.Base(dir)
		sn := c.SnapshotService(*snapshotter)
		ms, err := sn.View(ctx, key, identity.ChainID(diffs).String())
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		if err := mount.All(ms, dir); err != nil {
			sn.Remove(ctx, key)
			return nil, fmt.Errorf("mounting %s: %w", name, err)
		}
		return func() {
			mount.UnmountAll(dir, 0)
			sn.Remove(context.WithoutCancel(ctx), key)
		}, nil
	}
	o := WrapOptions{
		Granule: *granule,
		Rootfs:  filepath.Join(work, "rootfs"),
		Config:  filepath.Join(work, "config.json"),
		Guest:   filepath.Join(work, "guest"),
		Runtime: filepath.Join(work, "granule"),
		Out:     filepath.Join(work, "out"),
		Work:    filepath.Join(work, "stage"),
		Image:   *digest,
	}
	for _, v := range []struct{ name, dir string }{{*image, o.Rootfs}, {*guest, o.Guest}, {*runtimeImage, o.Runtime}} {
		undo, err := view(v.name, v.dir)
		if err != nil {
			return err
		}
		defer undo()
	}
	img, err := c.GetImage(ctx, *image)
	if err != nil {
		return err
	}
	desc, err := img.Config(ctx)
	if err != nil {
		return err
	}
	config, err := content.ReadBlob(ctx, c.ContentStore(), desc)
	if err != nil {
		return err
	}
	if err := os.WriteFile(o.Config, config, 0o600); err != nil {
		return err
	}
	if err := os.MkdirAll(o.Work, 0o700); err != nil {
		return err
	}
	a := WrapArgs(o)
	cmd := exec.Command(a[0], a[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("granule wrap: %w", err)
	}
	// The VM containers that mount it run as their silos' subuids, so
	// the guest half is readable by every user; the wraps directory,
	// 0700, keeps the host's other users out.
	if err := filepath.WalkDir(o.Out, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.Type()&os.ModeSymlink != 0 {
			return err
		}
		fi, err := e.Info()
		if err != nil {
			return err
		}
		m := fi.Mode().Perm() | 0o044
		if e.IsDir() || fi.Mode()&0o100 != 0 {
			m |= 0o011
		}
		return os.Chmod(p, m)
	}); err != nil {
		return err
	}
	if err := os.Rename(o.Out, WrapDir(*wraps, *digest)); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return nil
}
