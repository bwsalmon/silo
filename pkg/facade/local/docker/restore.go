package docker

import (
	"context"
	"errors"
	"fmt"

	"github.com/bwsalmon/silo/pkg/backend/local/storage"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// The audit kinds of Recover (local-rootless-storage.md call L40).
const (
	// KindContainerRecreated is a container row whose containerd
	// record was gone, made again from the row: stopped, with a fresh
	// writable layer. Its result is FAILED, with a reason, when it
	// couldn't be.
	KindContainerRecreated = "container.recreated"
	// KindVolumeMissing is a volume row whose directory is gone. It is
	// not made again empty: an empty volume that looks like the old one
	// is worse than an error.
	KindVolumeMissing = "volume_missing"
)

// Records reads the ids of every silo's containers, and its volumes by
// storage.Ref's String: the local backend's RecordsFunc, by which its
// inventory tells an object made after a restored store was copied
// from one the store knows.
func Records(ctx context.Context, tx store.Tx) (containers, volumes map[string]bool, err error) {
	cs, err := allRows[Container](ctx, tx, kindContainer)
	if err != nil {
		return nil, nil, err
	}
	vs, err := allRows[Volume](ctx, tx, kindVolume)
	if err != nil {
		return nil, nil, err
	}
	containers, volumes = map[string]bool{}, map[string]bool{}
	for _, l := range cs {
		for _, c := range l {
			containers[c.ID] = true
		}
	}
	for silo, l := range vs {
		for _, v := range l {
			volumes[storage.Ref{Silo: silo, Volume: v.Name}.String()] = true
		}
	}
	return containers, volumes, nil
}

// Recovery is what Recover found and did.
type Recovery struct {
	// Recreated are the containers made again from their rows, by id;
	// NotRecreated those that couldn't be, with why.
	Recreated    []string
	NotRecreated map[string]string
	// Missing are the volumes whose directories are gone, as
	// silo/volume.
	Missing []string
}

// Recover settles the rows that have no records at silod's start
// (local-rootless-storage.md call L40), after a restore, or after the
// cache was wiped:
//
//   - a volume row whose directory is gone is audited volume_missing,
//     and left so;
//   - a container row whose containerd record is gone is made again
//     from the row, stopped and with a fresh writable layer, and
//     audited container.recreated. One that mounts a missing volume, or
//     whose class this host doesn't run, is not: making it would make
//     the volume again, empty. Its row keeps why as its Error.
//
// Nothing that exists is changed or deleted, and objects without rows
// are the reconcile's to report.
func (s *Server) Recover(ctx context.Context) (*Recovery, error) {
	if s.opts.Storage == nil {
		return nil, errors.New("docker: recover: this silod keeps no volumes")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &Recovery{NotRecreated: map[string]string{}}
	es, err := s.opts.Storage.List(ctx)
	if err != nil {
		return nil, err
	}
	onDisk := map[string]bool{}
	for _, e := range es {
		onDisk[e.Ref.String()] = true
	}
	have := map[string]bool{}
	for _, d := range s.opts.Drivers {
		cs, err := d.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("docker: recover: listing %s containers: %w", d.Class(), err)
		}
		for _, c := range cs {
			have[c.ID] = true
		}
	}
	var gone map[string][]*Container
	missing := map[string]bool{}
	err = s.st.Tx(ctx, func(tx store.Tx) error {
		out.Missing, gone = nil, map[string][]*Container{}
		clear(missing)
		vs, err := allRows[Volume](ctx, tx, kindVolume)
		if err != nil {
			return err
		}
		for _, silo := range sortedKeys(vs) {
			for _, v := range vs[silo] {
				ref := storage.Ref{Silo: silo, Volume: v.Name}
				if onDisk[ref.String()] {
					continue
				}
				missing[ref.String()] = true
				out.Missing = append(out.Missing, ref.String())
				if err := auditSilo(ctx, tx, silo, KindVolumeMissing, "OK", map[string]any{"name": v.Name}); err != nil {
					return err
				}
			}
		}
		cs, err := allRows[Container](ctx, tx, kindContainer)
		if err != nil {
			return err
		}
		for _, silo := range sortedKeys(cs) {
			for _, c := range cs[silo] {
				if !have[c.ID] {
					gone[silo] = append(gone[silo], c)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, silo := range sortedKeys(gone) {
		for _, c := range gone[silo] {
			why := ""
			if s.drivers[c.Class] == nil {
				why = fmt.Sprintf("this host does not run isolation class %q", c.Class)
			}
			for _, m := range c.Mounts {
				if m.Type == "volume" && missing[storage.Ref{Silo: silo, Volume: m.Name}.String()] && why == "" {
					why = fmt.Sprintf("volume %s is missing", m.Name)
				}
			}
			if why == "" {
				stop, _ := c.Config["StopSignal"].(string)
				// The create checked it; a bad one is the default.
				sig, _ := parseSignal(stop)
				if err := s.makeContainer(ctx, silo, c, nil, sig); err != nil {
					why = err.Error()
				}
			}
			result, fields := "OK", map[string]any{"name": c.Name, "id": c.ID}
			if why != "" {
				result, fields["reason"] = "FAILED", why
				out.NotRecreated[c.ID] = why
			} else {
				out.Recreated = append(out.Recreated, c.ID)
			}
			err := s.st.Tx(ctx, func(tx store.Tx) error {
				if why != "" {
					c.Error = "silo: not re-created after a restore: " + why
					if err := put(ctx, tx, silo, kindContainer, c.ID, c.Name, c); err != nil {
						return err
					}
				}
				return auditSilo(ctx, tx, silo, KindContainerRecreated, result, fields)
			})
			if err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// auditSilo writes a record of silod's own under silo, made by the
// root. A silo that is gone is not an error.
func auditSilo(ctx context.Context, tx store.Tx, silo, kind, result string, fields map[string]any) error {
	t, err := tree.Get(ctx, tx, silo)
	if errors.Is(err, tree.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	root, err := tree.Root(ctx, tx)
	if err != nil {
		return err
	}
	return fenceapi.Audit(ctx, tx, t, fenceapi.Caller{Silo: root.ID, Via: fenceapi.ViaInternal}, kind, "", result, fields)
}
