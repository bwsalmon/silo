//go:build linux

package gvisor

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/bwsalmon/silo/pkg/backend/local/nsd"
)

// RemapArg is the subcommand that runs RemapMain: `silod runtime-remap`.
const RemapArg = "runtime-remap"

// RemapMain is `silod runtime-remap`, run inside the holder: it
// prepares a container's snapshot, key, from image, with every file's
// owner moved into the container's id range, as containerd's
// WithUserNSRemappedSnapshot does. The remapped parent is committed
// once per image and range, so a silo's second container of an image
// costs no copy. It runs in the holder because remapping mounts the
// snapshot, which needs the holder's mount namespace and its root;
// silod on the host has neither. runsc runs as the range's root, and
// --overlay2=root:self writes its filestore into the root it is given,
// which a snapshot still owned by the holder's root would refuse.
//
// With --hosts, --hostname or --resolv-conf, it then writes that /etc
// file into the container's own snapshot, owned by the range's root
// (EtcFiles), as Docker's bind mounts give them. Under
// --overlay2=root:self the snapshot is the sandbox's lower layer, so
// the container's own changes to them never reach it, a restart sees
// silod's again, and a commit's rootfs-upper doesn't carry them.
func RemapMain(args []string) error {
	fs := flag.NewFlagSet(RemapArg, flag.ContinueOnError)
	address := fs.String("address", "", "silo-containerd's socket")
	ns := fs.String("namespace", "", "the containerd namespace")
	lease := fs.String("lease", "", "the lease that holds the snapshot")
	image := fs.String("image", "", "the image's name in containerd")
	snapshotter := fs.String("snapshotter", DefaultSnapshotter, "the snapshotter")
	key := fs.String("key", "", "the container's snapshot key")
	outside := fs.Uint("outside", 0, "the range's first id in the holder")
	size := fs.Uint("size", 0, "the range's size")
	var etc EtcFiles
	fs.StringVar(&etc.Hosts, "hosts", "", "/etc/hosts's contents, if it is written")
	fs.StringVar(&etc.Hostname, "hostname", "", "/etc/hostname's contents, if it is written")
	fs.StringVar(&etc.ResolvConf, "resolv-conf", "", "/etc/resolv.conf's contents, if it is written")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *address == "" || *ns == "" || *image == "" || *key == "" || *outside == 0 || *size == 0 {
		return errors.New("runtime-remap: --address, --namespace, --image, --key, --outside and --size are required")
	}
	c, err := containerd.New(*address)
	if err != nil {
		return err
	}
	defer c.Close()
	ctx := namespaces.WithNamespace(context.Background(), *ns)
	if *lease != "" {
		ctx = leases.WithLease(ctx, *lease)
	}
	img, err := c.GetImage(ctx, *image)
	if err != nil {
		return err
	}
	m := []specs.LinuxIDMapping{{ContainerID: 0, HostID: uint32(*outside), Size: uint32(*size)}}
	if err := containerd.WithUserNSRemappedSnapshot(*key, img, m, m)(ctx, c, &containers.Container{ID: *key, Snapshotter: *snapshotter}); err != nil {
		return err
	}
	if etc == (EtcFiles{}) {
		return nil
	}
	mounts, err := c.SnapshotService(*snapshotter).Mounts(ctx, *key)
	if err != nil {
		return err
	}
	return mount.WithTempMount(ctx, mounts, func(root string) error {
		return etc.Write(root, int(*outside))
	})
}

// EtcFiles are the files silod writes in a container's /etc, each only
// if it is set: what Docker bind-mounts there from the container's
// directory.
type EtcFiles struct {
	Hosts, Hostname, ResolvConf string
}

// Write writes the files into /etc in the root filesystem at dir,
// owned by owner, the container's root as the holder sees it. An image
// without /etc gets one. Paths resolve inside dir, so an image's
// symlink can't send a write out of it; an image's own file at one of
// the paths, or a symlink there, is replaced rather than written
// through.
func (e EtcFiles) Write(dir string, owner int) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if _, err := root.Stat("etc"); errors.Is(err, fs.ErrNotExist) {
		if err := root.Mkdir("etc", 0o755); err != nil {
			return err
		}
		if err := root.Lchown("etc", owner, owner); err != nil {
			return err
		}
	} else if err != nil {
		return fmt.Errorf("the image's /etc: %w", err)
	}
	for _, f := range []struct{ name, content string }{
		{"hosts", e.Hosts}, {"hostname", e.Hostname}, {"resolv.conf", e.ResolvConf},
	} {
		if f.content == "" {
			continue
		}
		p := path.Join("etc", f.name)
		if err := root.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("replacing the image's /%s: %w", p, err)
		}
		w, err := root.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		_, err = w.WriteString(f.content)
		if err == nil {
			err = w.Chown(owner, owner)
		}
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("/%s: %w", p, err)
		}
	}
	return nil
}

// InHolder is Config.InHolder for silod: exe, with args, run inside the
// holder whose pid is holderPid through exe's nsexec constructor, with
// env added to its environment.
func InHolder(holderPid int, exe string, env ...string) func(ctx context.Context, args ...string) ([]byte, error) {
	command := HolderCommand(holderPid, exe, env...)
	return func(ctx context.Context, args ...string) ([]byte, error) {
		cmd := command(args...)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		stop := context.AfterFunc(ctx, func() { cmd.Process.Kill() })
		defer stop()
		err := cmd.Wait()
		return out.Bytes(), err
	}
}

// HolderCommand is Config.HolderCmd for silod: InHolder's command, not
// yet started, for a caller that streams its output.
func HolderCommand(holderPid int, exe string, env ...string) func(args ...string) *exec.Cmd {
	return func(args ...string) *exec.Cmd {
		cmd := nsd.JoinExec(holderPid, exe, append([]string{exe}, args...)...)
		cmd.Env = append(cmd.Env, env...)
		return cmd
	}
}
