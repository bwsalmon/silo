package session

import (
	"context"
	"errors"
	"fmt"
	k8sloc "github.com/bwsalmon/silo/v2/location/k8s"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bwsalmon/silo/v2/sandbox"
	"github.com/bwsalmon/silo/v2/sandbox/container"
	"github.com/bwsalmon/silo/v2/sandbox/granule"
)

// GCOptions configure GC.
type GCOptions struct {
	// Getenv reads XDG_RUNTIME_DIR and friends; os.Getenv when nil.
	Getenv func(string) string
	// Backend finds labelled containers; when nil, the container
	// backend if one is installed, and the granule backend's VMs if
	// Docker is.
	Backend sandbox.Backend
	// MinAge is how old a session directory without a session.json must
	// be before it counts as stale; a minute when zero, so a session
	// still writing its first record is not taken.
	MinAge time.Duration
}

// GCReport says what GC removed.
type GCReport struct {
	Removed []string
}

// GC is the cheap local part of silo gc (design §3.1, call 2): it
// removes stale session directories, whose silo run process is gone,
// with what their session.json lists, and containers labelled with a
// session that is gone, and granule VMs labelled with one.
func GC(ctx context.Context, opts GCOptions) (GCReport, error) {
	var rep GCReport
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if opts.MinAge == 0 {
		opts.MinAge = time.Minute
	}
	b := opts.Backend
	if b == nil {
		b = gcBackend()
	}
	root := RuntimeRoot(getenv)
	entries, err := os.ReadDir(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return rep, err
	}
	var errs []error
	live := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() || !ValidID(e.Name()) {
			continue
		}
		dir := filepath.Join(root, e.Name())
		rec, err := ReadRecord(dir)
		if err != nil {
			fi, serr := os.Stat(dir)
			if serr != nil || time.Since(fi.ModTime()) < opts.MinAge {
				live[e.Name()] = true
				continue
			}
		} else if alive(rec.PID) {
			live[e.Name()] = true
			continue
		}
		for i := len(rec.Resources) - 1; i >= 0; i-- {
			if err := undoResource(ctx, b, rec.ID, rec.Resources[i]); err != nil {
				errs = append(errs, fmt.Errorf("session %s: %w", e.Name(), err))
			}
		}
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, err)
			continue
		}
		rep.Removed = append(rep.Removed, "removed stale session directory "+dir)
	}
	if lister, ok := b.(sandbox.SessionLister); ok {
		sessions, err := lister.Sessions(ctx)
		if err != nil {
			errs = append(errs, err)
		}
		for _, id := range sessions {
			if live[id] {
				continue
			}
			n, err := destroySession(ctx, b, id)
			if err != nil {
				errs = append(errs, err)
			}
			if n > 0 {
				rep.Removed = append(rep.Removed, fmt.Sprintf("removed %d sandbox(es) of ended session %s", n, id))
			}
		}
	}
	return rep, errors.Join(errs...)
}

func undoResource(ctx context.Context, b sandbox.Backend, id string, r Resource) error {
	switch r.Kind {
	case KindProcessGroup:
		if r.PGID > 0 {
			if err := syscall.Kill(-r.PGID, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
				return err
			}
		}
	case KindContainers:
		if b != nil {
			_, err := destroySession(ctx, b, id)
			return err
		}
	case KindNetwork:
		if nw, ok := b.(sandbox.Networker); ok && r.Network != "" {
			return nw.RemoveNetwork(ctx, r.Network)
		}
	case KindDir:
		return os.RemoveAll(r.Path)
	case KindPod:
		return k8sloc.DeleteSession(ctx, &k8sloc.Kubectl{Context: r.Context, Namespace: r.Namespace}, id)
	}
	return nil
}

func destroySession(ctx context.Context, b sandbox.Backend, id string) (int, error) {
	if b == nil {
		return 0, nil
	}
	found, err := b.Find(ctx, id)
	if err != nil {
		return 0, err
	}
	var errs []error
	for _, sb := range found {
		errs = append(errs, sb.Destroy(ctx))
	}
	return len(found), errors.Join(errs...)
}

// gcBackend is what GC finds labelled sandboxes with when the caller
// names no backend: containers, and granule VMs on the local Docker.
// granule itself may be gone; Destroy then removes a VM's container
// directly.
func gcBackend() sandbox.Backend {
	k := &sandbox.Kinds{ByKind: map[string]sandbox.Backend{}}
	if cb, err := container.New(); err == nil {
		k.Default = cb
	}
	if _, err := exec.LookPath("docker"); err == nil {
		gb := &granule.Backend{Granule: os.Getenv(granule.BinaryEnv)}
		k.ByKind["granule"] = gb
	}
	if len(k.ByKind) == 0 {
		if k.Default == nil {
			return nil
		}
		return k.Default
	}
	return k
}
