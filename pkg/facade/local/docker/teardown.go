package docker

import (
	"context"

	"github.com/bwsalmon/silo/pkg/store"
)

// DeleteSilo removes what the facade keeps for a silo being deleted
// (local-backend.md §12 "Teardown"), once its containers are gone from
// the runtime (local.Containers.DeleteContents): every container,
// network, volume and image record row, with its charges, a network's
// subnet back to the silo's blocks, and its volumes' directories. Each
// container's listeners are closed and its wiring undone. A silo with
// no objects is not an error, so a delete that failed half way can be
// made again.
func (s *Server) DeleteSilo(ctx context.Context, silo string) error {
	ctx = context.WithoutCancel(ctx)
	var cs []*Container
	var vols []string
	s.mu.Lock()
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		cs, vols = nil, nil
		var err error
		if cs, err = s.containers(ctx, tx, silo); err != nil {
			return err
		}
		for _, c := range cs {
			for _, o := range []string{runObject(silo, c.ID), Object(silo, kindContainer, c.ID)} {
				if _, err := refund(ctx, tx, o); err != nil {
					return err
				}
			}
			if err := remove(ctx, tx, silo, kindContainer, c.ID); err != nil {
				return err
			}
			if err := topologyChanged(ctx, tx, silo, c.ID); err != nil {
				return err
			}
		}
		ns, err := s.networks(ctx, tx, silo)
		if err != nil {
			return err
		}
		for _, n := range ns {
			if _, err := s.deleteNetwork(ctx, tx, silo, n); err != nil {
				return err
			}
		}
		vs, err := rows[Volume](ctx, tx, silo, kindVolume)
		if err != nil {
			return err
		}
		for _, v := range vs {
			if _, err := s.deleteVolume(ctx, tx, silo, v); err != nil {
				return err
			}
			vols = append(vols, v.Name)
		}
		is, err := s.images(ctx, tx, silo)
		if err != nil {
			return err
		}
		for _, i := range is {
			if _, err := s.deleteImage(ctx, tx, silo, i); err != nil {
				return err
			}
		}
		return nil
	})
	s.mu.Unlock()
	if err != nil {
		return err
	}
	for _, c := range cs {
		s.unlisten(c.ID)
		s.unwire(ctx, c)
		s.forgetExecs(c.ID)
	}
	for _, v := range vols {
		s.deleteVolumeDir(ctx, silo, v)
	}
	return nil
}
