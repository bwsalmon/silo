package main

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	facade "github.com/bwsalmon/silo/pkg/facade/gcp/gce"
	"github.com/bwsalmon/silo/pkg/fence"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/gwtls"
	"github.com/bwsalmon/silo/pkg/leader"
	"github.com/bwsalmon/silo/pkg/ledger/rootcap"
	"github.com/bwsalmon/silo/pkg/reconcile"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// leaderConfig is what the leader's duties work on. Any of gce, facade
// and fence may be nil, for a silod serving neither the facade nor the
// fence; the duties that need them are left out.
type leaderConfig struct {
	gce        gce.Client
	project    string
	facade     *facade.Server
	fence      *fence.Applier
	reportOnly bool
	// root is the root capacity refresh's config; refresh is whether
	// to run it (not over the fake GCE, whose quotas a test sets).
	root    rootcap.Config
	refresh bool
}

// How often the leader does each duty (state.md "More than one silod,
// and the leader", SV-52).
const (
	adoptEvery  = 10 * time.Second
	hourlyEvery = time.Hour
)

// leaderDuties are the leader's periodic duties, besides applying the
// fence: the reconcile, adopting the operations of replicas that died,
// the root's capacity, and audit pruning. Each write they make checks
// the leader epoch (leader.Guard).
func leaderDuties(db store.Store, c leaderConfig) []leader.Duty {
	var out []leader.Duty
	if (c.gce != nil && c.project != "") || c.fence != nil {
		r := reconcile.New(reconcile.Options{Store: db, GCE: c.gce, Project: c.project, Fence: c.fence, ReportOnly: c.reportOnly})
		out = append(out, leader.Duty{Name: "reconcile", Every: reconcile.Every, Run: func(ctx context.Context) error {
			rep, err := r.Run(ctx)
			if rep != nil {
				log.Printf("reconcile: %d objects, %d orphans (%d new, %d deleted), %d foreign, %d charges dropped, %d silos recounted, %d fence drift steps%s",
					rep.Objects, len(rep.Orphans), len(rep.Found), len(rep.Deleted), len(rep.Foreign), len(rep.Dropped), len(rep.Recounted), len(rep.Drift),
					map[bool]string{true: " (report-only)"}[rep.ReportOnly])
			}
			return err
		}})
	}
	if c.facade != nil {
		out = append(out, leader.Duty{Name: "adopt operations", Every: adoptEvery, Run: func(ctx context.Context) error {
			n, err := c.facade.Adopt(ctx, leader.Guard)
			if n > 0 {
				log.Printf("leader: adopted %d operations of replicas whose heartbeat stopped", n)
			}
			return err
		}})
	}
	if c.gce != nil && c.project != "" && c.refresh {
		cfg := c.root
		cfg.Guard = leader.Guard
		rc := rootcap.New(db, c.gce, cfg)
		out = append(out, leader.Duty{Name: "root capacity", Every: hourlyEvery, Run: func(ctx context.Context) error {
			_, err := rc.Refresh(ctx)
			if errors.Is(err, tree.ErrNotFound) {
				return nil // before bootstrap: no root yet
			}
			return err
		}})
	}
	out = append(out, leader.Duty{Name: "audit pruning", Every: hourlyEvery, Run: func(ctx context.Context) error {
		n, err := fenceapi.PruneAuditGuarded(ctx, db, leader.Guard)
		if n > 0 {
			log.Printf("leader: pruned %d audit records older than %v", n, fenceapi.AuditRetention)
		}
		return err
	}})
	return out
}

// gatewayCertDuty issues and renews gateway replicas' certificates
// (package gwtls): only the leader writes the gateway VMs' metadata.
func gatewayCertDuty(iss *gwtls.Issuer) leader.Duty {
	return leader.Duty{Name: "gateway certificates", Every: gwtls.Every, Run: func(ctx context.Context) error {
		n, err := iss.Run(ctx)
		if n > 0 {
			log.Printf("leader: issued %d gateway certificates", n)
		}
		return err
	}}
}
