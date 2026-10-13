package docker

import (
	"context"
	"encoding/json"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/store"
)

// Kind is the class kind of the local backend's quota classes
// (local-backend.md §11, call L19). Every class is global: regions
// don't apply on one host.
const Kind = "local"

// The classes the facade charges (local-backend.md, "Quota charges").
const (
	ClassVCPUm      = "vcpu_m"
	ClassMemoryMB   = "memory_mb"
	ClassDiskGB     = "disk_gb"
	ClassContainers = "containers"
	ClassNetworks   = "networks"
	ClassVolumes    = "volumes"
)

// The class defaults, for a container that sets no limit (§11).
const (
	DefaultVCPUm    = 1000
	DefaultMemoryMB = 512
	DefaultDiskGB   = 10
)

// Key is a class's quota row key.
func Key(class string) ledger.Key { return ledger.Key{Kind: Kind, Class: class, Region: ledger.Global} }

// line is one class and amount, in whole units.
type line struct {
	class string
	n     int64
}

// Object names a charged object, as the ledger keeps it. A running
// container's CPU and memory are charged to a second object, "/run"
// after its own, so that a stop refunds them alone.
func Object(silo, kind, id string) string { return "docker/" + silo + "/" + kind + "s/" + id }

func runObject(silo, id string) string { return Object(silo, kindContainer, id) + "/run" }

// charge charges object's lines to the target, inside tx, and returns
// them as the audit records them.
func (s *Server) charge(ctx context.Context, tx store.Tx, q *request, object string, lines ...line) ([]fenceapi.ChargeLine, error) {
	var as []ledger.Amount
	var out []fenceapi.ChargeLine
	for _, l := range lines {
		if l.n == 0 {
			continue
		}
		as = append(as, ledger.Amount{Key: Key(l.class), Amount: ledger.Units(l.n)})
		out = append(out, fenceapi.ChargeLine{Payer: q.target.ID, Home: q.target.ID, Via: q.via(),
			Kind: Kind, Class: l.class, Region: ledger.Global, Amount: json.Number(ledger.FormatAmount(ledger.Units(l.n)))})
	}
	if len(as) == 0 {
		return nil, nil
	}
	if err := ledger.Charge(ctx, tx, ledger.ChargeRequest{Object: object, Silo: q.target.ID, Via: q.via(), Amounts: as}); err != nil {
		return nil, err
	}
	return out, nil
}

// refund gives back everything object is charged, inside tx, and
// returns it as the audit records a refund: negative amounts.
func refund(ctx context.Context, tx store.Tx, object string) ([]fenceapi.ChargeLine, error) {
	rs, err := ledger.Refund(ctx, tx, object, nil)
	if err != nil {
		return nil, err
	}
	out := make([]fenceapi.ChargeLine, 0, len(rs))
	for _, r := range rs {
		out = append(out, fenceapi.ChargeLine{Payer: r.Payer, Home: r.Home, Via: r.Via, Kind: r.Kind, Class: r.Class,
			Region: r.Region, Amount: json.Number(ledger.FormatAmount(-r.Amount))})
	}
	return out, nil
}

// refundLater refunds object in a transaction of its own, for a
// runtime call that failed after its charge committed.
func (s *Server) refundLater(ctx context.Context, object string) {
	_ = s.st.Tx(context.WithoutCancel(ctx), func(tx store.Tx) error {
		_, err := refund(ctx, tx, object)
		return err
	})
}

// granted is the target's grant of a class, in whole units.
func granted(ctx context.Context, tx store.Tx, silo, class string) (int64, error) {
	r, err := ledger.Get(ctx, tx, silo, Key(class))
	if err != nil {
		return 0, err
	}
	return r.Granted / ledger.Micro, nil
}

// gib is n bytes in whole GiB, rounded up.
func gib(n int64) int64 { return (n + 1<<30 - 1) >> 30 }
