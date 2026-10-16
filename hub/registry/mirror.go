package registry

import (
	"context"
	"reflect"
	"time"
)

// Run keeps this pod in the shared registry until ctx ends: every
// third of PodLease it renews the pod's lease in hub_pods, takes
// offline the rows naming this pod whose connections it does not hold
// (an earlier process's under the same name, or a close that failed to
// reach the database), and refills the mirror of every other pod's
// nodes (Refill). When ctx ends it deletes the pod's lease, which takes
// any of its nodes still named in the database offline at once. Without
// a Store it only waits for ctx. It returns ctx's error.
func (r *Registry) Run(ctx context.Context) error {
	if r.Store == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	t := time.NewTicker(r.podLease() / 3)
	defer t.Stop()
	for {
		if err := r.renew(ctx); err != nil && ctx.Err() == nil {
			r.logf("registry: renewing pod %s's lease: %v", r.Pod, err)
		}
		if err := r.Refill(ctx); err != nil && ctx.Err() == nil {
			r.logf("registry: refilling from the database: %v", err)
		}
		select {
		case <-ctx.Done():
			dctx, cancel := storeContext()
			if err := r.Store.deletePod(dctx, r.Pod); err != nil {
				r.logf("registry: deleting pod %s's lease: %v", r.Pod, err)
			}
			cancel()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// renew renews the pod's lease and sweeps its stale rows.
func (r *Registry) renew(ctx context.Context) error {
	if err := r.Store.renewPod(ctx, r.Pod, r.PodAddr, r.now().Add(r.podLease())); err != nil {
		return err
	}
	held := map[string]string{}
	r.mu.Lock()
	for id, n := range r.nodes {
		if n.lease != nil {
			held[id] = n.conn
		}
	}
	r.mu.Unlock()
	return r.Store.clearPod(ctx, r.Pod, held)
}

// Refill reads the shared registry into this pod's mirror of it. The
// nodes this pod holds are left as they are, since this pod is
// authoritative for them, and so is a node this pod changed after the
// read. Every other node takes the database's row and its pod's lease.
// A node still on the connection it was on keeps its claims, unless it
// has heartbeated since the last refill, which settles them, as a
// heartbeat on this pod does.
func (r *Registry) Refill(ctx context.Context) error {
	if r.Store == nil {
		return nil
	}
	r.mu.Lock()
	seq := r.seq
	r.mu.Unlock()
	rows, err := r.Store.load(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := false
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		seen[row.ID] = true
		cur := r.nodes[row.ID]
		if cur != nil && (cur.lease != nil || cur.changed > seq) {
			continue
		}
		n := &node{entry: row.Entry, conn: row.conn, pod: row.pod, podUntil: row.leaseUntil, settled: row.settled}
		if n.conn == "" {
			n.entry.Free = 0
		}
		if cur != nil && cur.conn != "" && cur.conn == n.conn {
			n.epoch = cur.epoch
			n.entry.Claimed = cur.entry.Claimed
			if cur.settled != n.settled {
				n.epoch++
				n.entry.Claimed = 0
			}
		}
		if cur == nil || cur.conn != n.conn || cur.pod != n.pod || !reflect.DeepEqual(cur.entry, n.entry) {
			changed = true
		}
		r.nodes[row.ID] = n
	}
	for id, n := range r.nodes {
		if !seen[id] && n.lease == nil && n.changed <= seq {
			delete(r.nodes, id)
			changed = true
		}
	}
	if changed {
		r.notify()
	}
	return nil
}

// Holder reads afresh, from the shared registry, which other pod holds
// the node's connection and the address it is reached at (docs/hub.md
// §2.2.3): what Server.Open forwards a stream to. ok is false when no
// live pod but this one holds it, or without a Store.
func (r *Registry) Holder(ctx context.Context, id string) (pod, addr string, ok bool, err error) {
	if r.Store == nil {
		return "", "", false, nil
	}
	pod, addr, until, err := r.Store.holder(ctx, id)
	if err != nil || pod == "" || pod == r.Pod || !r.now().Before(until) {
		return "", "", false, err
	}
	return pod, addr, true, nil
}
