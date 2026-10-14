package session

import (
	"context"
	"errors"
	"fmt"
	k8sloc "github.com/bwsalmon/silo/v2/location/k8s"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bwsalmon/silo/v2/sandbox"
	"github.com/bwsalmon/silo/v2/sandbox/container"
)

// GCOptions configure GC.
type GCOptions struct {
	// Getenv reads XDG_RUNTIME_DIR and friends; os.Getenv when nil.
	Getenv func(string) string
	// Backend finds labelled containers; the container backend when nil
	// and one is installed.
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
// session that is gone.
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
		if cb, err := container.New(); err == nil {
			b = cb
		}
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
				rep.Removed = append(rep.Removed, fmt.Sprintf("removed %d container(s) of ended session %s", n, id))
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
