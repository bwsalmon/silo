package gce

import (
	"context"
	"strings"

	"github.com/bwsalmon/silo/pkg/store"
)

// setReplica records replica as the one watching rec. An empty replica
// records nothing: the column is from store.LeaderSchema on.
func setReplica(ctx context.Context, tx store.Tx, rec Operation, replica string) error {
	if replica == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE facade_operations SET replica = ? WHERE scope = ? AND name = ?`, replica, rec.Scope, rec.Name)
	return err
}

// Adopt takes over, as this replica, every operation not yet seen to
// finish whose replica's heartbeat is older than store.ReplicaTimeout
// or gone, and watches it, so that its refund and bookkeeping still
// happen (state.md "More than one silod, and the leader", SV-52). It is
// the leader's to call: guard is called in the transaction that takes
// them over, and refuses it by returning an error (leader.Guard). It
// returns how many it adopted.
func (s *Server) Adopt(ctx context.Context, guard func(context.Context, store.Tx) error) (int, error) {
	if s.opts.Replica == "" {
		return 0, nil
	}
	var recs []Operation
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		recs = nil
		if guard != nil {
			if err := guard(ctx, tx); err != nil {
				return err
			}
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		live := store.Millis(now.Add(-store.ReplicaTimeout))
		recs, err = readOps(ctx, tx, `done = ? AND replica <> ? AND replica NOT IN (SELECT id FROM replicas WHERE heartbeat_at >= ?)`,
			false, s.opts.Replica, live)
		if err != nil {
			return err
		}
		for _, r := range recs {
			if err := setReplica(ctx, tx, r, s.opts.Replica); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, r := range recs {
		s.watch(r)
	}
	return len(recs), nil
}

// PendingObjects are the ledger objects of every operation not yet seen
// to finish: its Object and Also. The reconcile leaves their charges
// alone, whether or not the object exists yet.
func PendingObjects(ctx context.Context, tx store.Tx) (map[string]bool, error) {
	recs, err := readOps(ctx, tx, `done = ?`, false)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, r := range recs {
		if r.Object != "" {
			out[r.Object] = true
		}
		for _, o := range r.Also {
			out[o] = true
		}
	}
	return out, nil
}

// LedgerObject is the ledger's object for a real object in the host
// project, as the facade charges it: collection is compute/v1's
// (instances, disks, images, snapshots, firewalls, subnetworks,
// instanceTemplates), scope the zone, the region ("" or "global" for a
// global template), or "" for a global collection. It is "" for a
// collection the facade doesn't charge.
func LedgerObject(collection, scope, real string) string {
	switch collection {
	case "instances":
		return instanceObject(scope, real)
	case "disks":
		return diskObject(scope, real)
	case "images":
		return imageObject(real)
	case "snapshots":
		return snapshotObject(real)
	case "firewalls":
		return firewallObject(real)
	case "subnetworks":
		return subnetObject(scope, real)
	case "instanceTemplates":
		return templateObject(strings.TrimPrefix(scope, "global"), real)
	}
	return ""
}
