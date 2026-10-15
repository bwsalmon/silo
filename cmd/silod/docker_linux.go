package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/netip"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/nsd"
	"github.com/bwsalmon/silo/pkg/backend/local/rootless"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime/gvisor"
	"github.com/bwsalmon/silo/pkg/facade/local/dockergw"
)

// holderNet is silo-net through silo-nsd's RPC on socket: listeners
// it opens there, its dial pool, and its wiring of containers.
type holderNet struct {
	*dockergw.Wiring
	c    *nsd.Client
	pool *nsd.Pool
}

func openSiloNet(socket string) (siloNet, error) {
	c, err := nsd.Dial(socket)
	if err != nil {
		return nil, err
	}
	return &holderNet{Wiring: &dockergw.Wiring{Holder: c}, c: c, pool: nsd.NewPool(c, "tcp4", 8)}, nil
}

func (h *holderNet) Listen(network, addr string) (net.Listener, error) {
	return h.c.Listen(network, addr)
}

func (h *holderNet) ListenPacket(network, addr string) (net.PacketConn, error) {
	return h.c.ListenPacket(network, addr)
}

func (h *holderNet) Dial(ctx context.Context, local, target netip.AddrPort) (net.Conn, error) {
	return h.pool.Dial(ctx, local, target)
}

func (h *holderNet) Close() error {
	h.pool.Close()
	return h.c.Close()
}

// sandboxDriver is the sandbox class's driver (local-backend.md §15
// step 7) over what silo-nsd and silo-containerd.service run:
// silo-containerd in the holder, and the runsc shim's options that
// `silod containerd` writes beside the holder's state when runsc is on
// its PATH. silod itself is the log copier, `silod runtime-remap` and
// `silod runsc`, joined to whichever holder the state file names when
// it runs, so that a restarted holder is followed.
func sandboxDriver(c sandboxConfig) (runtime.Driver, error) {
	return gvisor.New(c.gvisor())
}

func (c sandboxConfig) gvisor() gvisor.Config {
	holder := func() int {
		st, err := nsd.ReadState(c.State)
		if err != nil {
			// Pid 0 joins nothing: the command fails and says so.
			log.Printf("the sandbox driver: %v", err)
			return 0
		}
		return st.Pid
	}
	p := rootless.Paths{RuntimeDir: filepath.Dir(c.State)}
	cfg := gvisor.Config{
		Address: c.Containerd,
		Options: p.RunscOptions(),
		Dir:     c.Dir,
		Volumes: c.Volumes,
		Logger:  []string{c.Exe, gvisor.LogArg},
		InHolder: func(ctx context.Context, args ...string) ([]byte, error) {
			return gvisor.InHolder(holder(), c.Exe)(ctx, args...)
		},
		HolderCmd: func(args ...string) *exec.Cmd {
			return gvisor.HolderCommand(holder(), c.Exe)(args...)
		},
		RunscRoot: p.RunscRoot(),
		Toolbox:   c.Toolbox,
	}
	if c.Runsc != "" {
		cfg.Runsc = []string{"runsc", "--runsc", c.Runsc}
	}
	// Each container is a cgroup in its silo's, in the scope silod
	// keeps in the user's manager. Where that can't be made (no user
	// manager, or no delegation), containers get none, and CPU and
	// memory are the ledger's alone.
	if c.SiloPaths != nil {
		tree := c.Cgroups
		if tree == nil {
			tree = &rootless.SandboxTree{}
		}
		cfg.CgroupsPath = func(spec runtime.Spec) string {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			paths, err := c.SiloPaths(ctx)
			path := paths[spec.Silo]
			if err != nil || len(path) == 0 {
				if err == nil {
					err = errors.New("no such silo")
				}
				log.Printf("the sandbox driver: the silo path of %s: %v; using a cgroup of its own", spec.Silo, err)
				path = []string{spec.Silo}
			}
			cg, err := tree.Path(ctx, spec.ID, path...)
			if err != nil {
				log.Printf("the sandbox driver: %s's cgroup: %v; its limits are the ledger's alone", spec.ID, err)
				return ""
			}
			return cg
		}
	}
	return cfg
}
