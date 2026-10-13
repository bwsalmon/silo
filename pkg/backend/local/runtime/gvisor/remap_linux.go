//go:build linux

package gvisor

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os/exec"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/leases"
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
	return containerd.WithUserNSRemappedSnapshot(*key, img, m, m)(ctx, c, &containers.Container{ID: *key, Snapshotter: *snapshotter})
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
