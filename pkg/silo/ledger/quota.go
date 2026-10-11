package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/tree"
)

// ViaFacade is the via of a charge the tenant's own facade call made.
// A call made on the silo's behalf is "ancestor:<id>" or
// "manage:<grant>".
const ViaFacade = "facade"

// capacitySource is the from of the edge that carries the root's
// capacity: the host project, or install's setting.
const capacitySource = ""

// The columns of a quota row that edges and charges move. Fixed
// names, not classes.
const (
	colGranted   = "granted"
	colDelegated = "delegated"
	colUsed      = "used"
)

// Get reads one quota row. A row never written reads as zeros.
func Get(ctx context.Context, tx store.Tx, silo string, k Key) (Row, error) {
	r := Row{Silo: silo, Key: k}
	err := tx.QueryRow(ctx, `SELECT granted, delegated, used, consumed, over_committed FROM quota
		WHERE silo = ? AND kind = ? AND class = ? AND region = ?`, silo, k.Kind, k.Class, k.Region).
		Scan(&r.Granted, &r.Delegated, &r.Used, &r.Consumed, &r.OverCommitted)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	return r, err
}

// Quota lists a silo's quota rows, by kind, region and class. Rows that
// are all zero are left out.
func Quota(ctx context.Context, tx store.Tx, silo string) ([]Row, error) {
	return queryRows(ctx, tx, `SELECT silo, kind, class, region, granted, delegated, used, consumed, over_committed
		FROM quota WHERE silo = ? AND (granted <> 0 OR delegated <> 0 OR used <> 0 OR consumed <> 0)
		ORDER BY kind, region, class`, silo)
}

// OverCommitted lists the rows of the root that are over-committed.
func OverCommitted(ctx context.Context, tx store.Tx) ([]Row, error) {
	return queryRows(ctx, tx, `SELECT silo, kind, class, region, granted, delegated, used, consumed, over_committed
		FROM quota WHERE over_committed = ? ORDER BY silo, kind, region, class`, true)
}

func queryRows(ctx context.Context, tx store.Tx, q string, args ...any) ([]Row, error) {
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.Silo, &r.Kind, &r.Class, &r.Region, &r.Granted, &r.Delegated, &r.Used, &r.Consumed, &r.OverCommitted); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// add moves one column of a quota row by delta, creating the row.
func add(ctx context.Context, tx store.Tx, silo string, k Key, col string, delta int64) error {
	if delta == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO quota (silo, kind, class, region) VALUES (?, ?, ?, ?)
		ON CONFLICT (silo, kind, class, region) DO NOTHING`, silo, k.Kind, k.Class, k.Region); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE quota SET `+col+` = `+col+` + ?
		WHERE silo = ? AND kind = ? AND class = ? AND region = ?`, delta, silo, k.Kind, k.Class, k.Region)
	return err
}

// settle records whether a row is over-committed, and logs a change
// when that flips. Only the root's capacity can put a row there.
func settle(ctx context.Context, tx store.Tx, silo string, k Key) error {
	r, err := Get(ctx, tx, silo, k)
	if err != nil {
		return err
	}
	over := r.Delegated+r.Used > r.Granted
	if over == r.OverCommitted {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE quota SET over_committed = ?
		WHERE silo = ? AND kind = ? AND class = ? AND region = ?`, over, silo, k.Kind, k.Class, k.Region); err != nil {
		return err
	}
	data, _ := json.Marshal(map[string]any{"kind": k.Kind, "class": k.Class, "region": k.Region,
		"over_committed": over, "granted": r.Granted, "held": r.Delegated + r.Used})
	_, err = tx.Append(ctx, "quota.over_committed", silo, string(data))
	return err
}

// refuseOverCommitted is the over-commitment rule: while the root is
// over-committed in k, nothing in the tree may charge or be granted
// more of k.
func refuseOverCommitted(ctx context.Context, tx store.Tx, root string, k Key) error {
	r, err := Get(ctx, tx, root, k)
	if err != nil {
		return err
	}
	if r.OverCommitted {
		return &Error{Code: QuotaExceeded, Silo: root, Key: k, Free: r.Free(),
			Message: fmt.Sprintf("%s: the root is over-committed (granted %s, held %s); nothing more can be charged or granted until it is back under",
				k, FormatAmount(r.Granted), FormatAmount(r.Delegated+r.Used))}
	}
	return nil
}

// regionAllowed checks that a regional key's region is one of silo's.
func regionAllowed(ctx context.Context, tx store.Tx, s *tree.Silo, c *Class, k Key) error {
	if c.Scope == ScopeGlobal {
		return nil
	}
	ok, err := hasRegion(ctx, tx, s.ID, k.Region)
	if err != nil || ok {
		return err
	}
	return &Error{Code: FailedPrecondition, Silo: s.ID, Key: k,
		Message: fmt.Sprintf("%s: region %s is not allowed in %s", k, k.Region, s.Path)}
}

// sum adds up amounts by key, keeping the order keys first appear in,
// and refuses negative amounts.
func sum(as []Amount) ([]Amount, error) {
	idx := map[Key]int{}
	var out []Amount
	for _, a := range as {
		if a.Amount < 0 {
			return nil, &Error{Code: InvalidArgument, Key: a.Key, Message: fmt.Sprintf("%s: negative amount", a.Key)}
		}
		if i, ok := idx[a.Key]; ok {
			out[i].Amount += a.Amount
			continue
		}
		idx[a.Key] = len(out)
		out = append(out, a)
	}
	return out, nil
}

// ---- Grants ----

// Edge is one allocation edge: an amount of one class in one region,
// from one silo to another.
type Edge struct {
	From, To string
	Key
	Amount int64
}

func edge(ctx context.Context, tx store.Tx, from, to string, k Key) (int64, error) {
	var n int64
	err := tx.QueryRow(ctx, `SELECT amount FROM allocations
		WHERE from_silo = ? AND to_silo = ? AND kind = ? AND class = ? AND region = ?`,
		from, to, k.Kind, k.Class, k.Region).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

func setEdge(ctx context.Context, tx store.Tx, from, to string, k Key, amount int64) error {
	if amount == 0 {
		_, err := tx.Exec(ctx, `DELETE FROM allocations
			WHERE from_silo = ? AND to_silo = ? AND kind = ? AND class = ? AND region = ?`,
			from, to, k.Kind, k.Class, k.Region)
		return err
	}
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO allocations (from_silo, to_silo, kind, class, region, amount, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (from_silo, to_silo, kind, class, region) DO UPDATE SET amount = excluded.amount, updated_at = excluded.updated_at`,
		from, to, k.Kind, k.Class, k.Region, amount, store.Millis(now))
	return err
}

// Edges lists the allocation edges into a silo.
func Edges(ctx context.Context, tx store.Tx, to string) ([]Edge, error) {
	rows, err := tx.Query(ctx, `SELECT from_silo, to_silo, kind, class, region, amount FROM allocations
		WHERE to_silo = ? ORDER BY kind, region, class, from_silo`, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Edge
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.From, &e.To, &e.Kind, &e.Class, &e.Region, &e.Amount); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// grantPlan is one checked change to a child's grant.
type grantPlan struct {
	k          Key
	old, new   int64
	parentFree int64
	childFree  int64
}

// planGrant checks setting child's grant of each key to its amount,
// and returns what would change. chain is the child's ancestry.
func planGrant(ctx context.Context, tx store.Tx, chain []*tree.Silo, amounts []Amount) ([]grantPlan, error) {
	child := chain[len(chain)-1]
	if child.IsRoot() {
		return nil, invalid("the root's quota is its capacity, which is set rather than granted")
	}
	parent, root := chain[len(chain)-2], chain[0]
	seen := map[Key]bool{}
	var plans []grantPlan
	for _, a := range amounts {
		if a.Amount < 0 {
			return nil, &Error{Code: InvalidArgument, Key: a.Key, Message: fmt.Sprintf("%s: negative grant", a.Key)}
		}
		if seen[a.Key] {
			return nil, &Error{Code: InvalidArgument, Key: a.Key, Message: fmt.Sprintf("%s: named twice", a.Key)}
		}
		seen[a.Key] = true
		c, err := checkKey(ctx, tx, a.Key)
		if err != nil {
			return nil, err
		}
		old, err := edge(ctx, tx, parent.ID, child.ID, a.Key)
		if err != nil {
			return nil, err
		}
		pr, err := Get(ctx, tx, parent.ID, a.Key)
		if err != nil {
			return nil, err
		}
		cr, err := Get(ctx, tx, child.ID, a.Key)
		if err != nil {
			return nil, err
		}
		p := grantPlan{k: a.Key, old: old, new: a.Amount, parentFree: pr.Free(), childFree: cr.Free()}
		switch delta := a.Amount - old; {
		case delta > 0:
			if err := regionAllowed(ctx, tx, child, c, a.Key); err != nil {
				return nil, err
			}
			if err := refuseOverCommitted(ctx, tx, root.ID, a.Key); err != nil {
				return nil, err
			}
			if delta > pr.Free() {
				return nil, &Error{Code: QuotaExceeded, Silo: parent.ID, Key: a.Key, Asked: delta, Free: pr.Free(),
					Message: fmt.Sprintf("%s: asked %s more for %s, %s has %s free",
						a.Key, FormatAmount(delta), child.Path, parent.Path, FormatAmount(pr.Free()))}
			}
		case delta < 0:
			if held := cr.Delegated + cr.Used; a.Amount < held {
				return nil, &Error{Code: FailedPrecondition, Silo: child.ID, Key: a.Key, Asked: a.Amount, Free: cr.Free(),
					Message: fmt.Sprintf("%s: can't lower %s to %s, it holds %s (%s delegated, %s used)",
						a.Key, child.Path, FormatAmount(a.Amount), FormatAmount(held),
						FormatAmount(cr.Delegated), FormatAmount(cr.Used))}
			}
		}
		plans = append(plans, p)
	}
	return plans, nil
}

// Grant sets child's grant from its parent of each key to its amount,
// raising or lowering it. All of it is checked before any of it is
// written:
//
//   - raising by n needs n free in the parent, an allowed region in
//     the child, and a root that isn't over-committed there;
//   - lowering to g needs g >= child.delegated + child.used. Nothing is
//     reclaimed to make room.
func Grant(ctx context.Context, tx store.Tx, child string, amounts []Amount) error {
	chain, err := tree.Ancestry(ctx, tx, child)
	if err != nil {
		return err
	}
	plans, err := planGrant(ctx, tx, chain, amounts)
	if err != nil {
		return err
	}
	parent := chain[len(chain)-2].ID
	for _, p := range plans {
		delta := p.new - p.old
		if delta == 0 {
			continue
		}
		if err := setEdge(ctx, tx, parent, child, p.k, p.new); err != nil {
			return err
		}
		if err := add(ctx, tx, child, p.k, colGranted, delta); err != nil {
			return err
		}
		if err := add(ctx, tx, parent, p.k, colDelegated, delta); err != nil {
			return err
		}
		// Lowering a grant can bring an over-committed root back under.
		if err := settle(ctx, tx, parent, p.k); err != nil {
			return err
		}
	}
	return logQuota(ctx, tx, child, plans)
}

func logQuota(ctx context.Context, tx store.Tx, silo string, plans []grantPlan) error {
	var keys []string
	for _, p := range plans {
		if p.new != p.old {
			keys = append(keys, p.k.String())
		}
	}
	if len(keys) == 0 {
		return nil
	}
	data, _ := json.Marshal(map[string]any{"changed": keys})
	_, err := tx.Append(ctx, "quota", silo, string(data))
	return err
}

// SetCapacity sets the root's capacity of each key: what the host
// project's quota allows, less the control plane's own use, or what
// install was given. Unlike a grant it isn't checked against anything,
// so it can fall below what the tree holds; the row is then recorded
// as over-committed, and new charges and grants of that key are
// refused until it is back under. Nothing is reclaimed.
func SetCapacity(ctx context.Context, tx store.Tx, root string, amounts []Amount) error {
	r, err := tree.Get(ctx, tx, root)
	if err != nil {
		return err
	}
	if !r.IsRoot() {
		return invalid("%s is not the root; quota below the root is granted, not set", r.Path)
	}
	seen := map[Key]bool{}
	var plans []grantPlan
	for _, a := range amounts {
		if a.Amount < 0 {
			return &Error{Code: InvalidArgument, Key: a.Key, Message: fmt.Sprintf("%s: negative capacity", a.Key)}
		}
		if seen[a.Key] {
			return &Error{Code: InvalidArgument, Key: a.Key, Message: fmt.Sprintf("%s: named twice", a.Key)}
		}
		seen[a.Key] = true
		c, err := checkKey(ctx, tx, a.Key)
		if err != nil {
			return err
		}
		old, err := edge(ctx, tx, capacitySource, root, a.Key)
		if err != nil {
			return err
		}
		if a.Amount > old {
			if err := regionAllowed(ctx, tx, r, c, a.Key); err != nil {
				return err
			}
		}
		plans = append(plans, grantPlan{k: a.Key, old: old, new: a.Amount})
	}
	for _, p := range plans {
		if p.new == p.old {
			continue
		}
		if err := setEdge(ctx, tx, capacitySource, root, p.k, p.new); err != nil {
			return err
		}
		if err := add(ctx, tx, root, p.k, colGranted, p.new-p.old); err != nil {
			return err
		}
		if err := settle(ctx, tx, root, p.k); err != nil {
			return err
		}
	}
	return logQuota(ctx, tx, root, plans)
}

// ---- Charges ----

// ChargeRow is one charge: what an object holds of one class in one
// region, who pays for it and where it lives.
type ChargeRow struct {
	Object string
	Payer  string // whose quota it is charged to
	Home   string // where the object lives; the payer in v1
	Via    string // ViaFacade, "ancestor:<id>" or "manage:<grant>"
	Key
	Amount int64
}

// ChargeRequest is a request to charge an object to a silo.
type ChargeRequest struct {
	Object  string   // what it is for: a resource's self-link, an agent
	Silo    string   // the object's home, and in v1 its payer
	Via     string   // how the call reached the silo
	Amounts []Amount // what it charges; added to what the object holds
}

func checkVia(via string) error {
	if via == ViaFacade {
		return nil
	}
	for _, p := range []string{"ancestor:", "manage:"} {
		if rest, ok := strings.CutPrefix(via, p); ok && rest != "" {
			return nil
		}
	}
	return invalid("via %q: want %s, ancestor:<id> or manage:<grant>", via, ViaFacade)
}

// chargePlan is one checked charge to one key.
type chargePlan struct {
	k    Key
	n    int64
	free int64
}

func planCharge(ctx context.Context, tx store.Tx, chain []*tree.Silo, amounts []Amount) ([]chargePlan, error) {
	s, root := chain[len(chain)-1], chain[0]
	as, err := sum(amounts)
	if err != nil {
		return nil, err
	}
	var plans []chargePlan
	for _, a := range as {
		c, err := checkKey(ctx, tx, a.Key)
		if err != nil {
			return nil, err
		}
		r, err := Get(ctx, tx, s.ID, a.Key)
		if err != nil {
			return nil, err
		}
		plans = append(plans, chargePlan{k: a.Key, n: a.Amount, free: r.Free()})
		if a.Amount == 0 {
			continue
		}
		if err := regionAllowed(ctx, tx, s, c, a.Key); err != nil {
			return nil, err
		}
		if err := refuseOverCommitted(ctx, tx, root.ID, a.Key); err != nil {
			return nil, err
		}
		// Ancestors need no check: s's grant was reserved out of theirs.
		if a.Amount > r.Free() {
			return nil, &Error{Code: QuotaExceeded, Silo: s.ID, Key: a.Key, Asked: a.Amount, Free: r.Free(),
				Message: fmt.Sprintf("%s: asked %s, %s has %s free", a.Key, FormatAmount(a.Amount), s.Path, FormatAmount(r.Free()))}
		}
	}
	return plans, nil
}

// Charge charges an object's amounts to its silo: each succeeds only
// if used + n <= granted - delegated there, the region is allowed, and
// the root isn't over-committed in it. All are checked before any is
// written. Charging an object again adds to what it holds, as growing
// a disk does.
func Charge(ctx context.Context, tx store.Tx, c ChargeRequest) error {
	if c.Object == "" {
		return invalid("a charge needs the object it is for")
	}
	if err := checkVia(c.Via); err != nil {
		return err
	}
	chain, err := tree.Ancestry(ctx, tx, c.Silo)
	if err != nil {
		return err
	}
	plans, err := planCharge(ctx, tx, chain, c.Amounts)
	if err != nil {
		return err
	}
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	for _, p := range plans {
		if p.n == 0 {
			continue
		}
		// v1: the payer is the home.
		payer, home := c.Silo, c.Silo
		if _, err := tx.Exec(ctx, `INSERT INTO charges (object, payer, home, via, kind, class, region, amount, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (object, payer, kind, class, region)
			DO UPDATE SET amount = charges.amount + excluded.amount, via = excluded.via, updated_at = excluded.updated_at`,
			c.Object, payer, home, c.Via, p.k.Kind, p.k.Class, p.k.Region, p.n, store.Millis(now), store.Millis(now)); err != nil {
			return err
		}
		if err := add(ctx, tx, payer, p.k, colUsed, p.n); err != nil {
			return err
		}
	}
	return nil
}

// Charges lists what an object is charged.
func Charges(ctx context.Context, tx store.Tx, object string) ([]ChargeRow, error) {
	rows, err := tx.Query(ctx, `SELECT object, payer, home, via, kind, class, region, amount FROM charges
		WHERE object = ? ORDER BY payer, kind, region, class`, object)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChargeRow
	for rows.Next() {
		var c ChargeRow
		if err := rows.Scan(&c.Object, &c.Payer, &c.Home, &c.Via, &c.Kind, &c.Class, &c.Region, &c.Amount); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ChargedObject is an object holding charges, and its payers.
type ChargedObject struct {
	Object string
	Payers []string
}

// ChargedObjects lists every object holding charges none of which has
// been written since before: the reconcile drops those whose object
// doesn't exist (gcp-backend.md, "Reconcile", SV-77).
func ChargedObjects(ctx context.Context, tx store.Tx, before time.Time) ([]ChargedObject, error) {
	rows, err := tx.Query(ctx, `SELECT object, payer FROM charges WHERE object IN
		(SELECT object FROM charges GROUP BY object HAVING MAX(updated_at) < ?)
		GROUP BY object, payer ORDER BY object, payer`, store.Millis(before))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChargedObject
	for rows.Next() {
		var o, p string
		if err := rows.Scan(&o, &p); err != nil {
			return nil, err
		}
		if n := len(out); n > 0 && out[n-1].Object == o {
			out[n-1].Payers = append(out[n-1].Payers, p)
			continue
		}
		out = append(out, ChargedObject{Object: o, Payers: []string{p}})
	}
	return out, rows.Err()
}

// Refund gives back what an object is charged: all of it if amounts
// is empty (the object is gone, or its GCE call failed), or the amounts
// named (a disk's provisioned IOPS lowered). Each charge is refunded to
// its payer, whatever its home. Refunding more than is charged is
// INVALID_ARGUMENT. It returns what was refunded.
func Refund(ctx context.Context, tx store.Tx, object string, amounts []Amount) ([]ChargeRow, error) {
	held, err := Charges(ctx, tx, object)
	if err != nil {
		return nil, err
	}
	var out []ChargeRow
	if len(amounts) == 0 {
		out = held
	} else {
		as, err := sum(amounts)
		if err != nil {
			return nil, err
		}
		for _, a := range as {
			left := a.Amount
			for _, c := range held {
				if c.Key != a.Key || left == 0 {
					continue
				}
				n := min(left, c.Amount)
				c.Amount = n
				out = append(out, c)
				left -= n
			}
			if left > 0 {
				return nil, &Error{Code: InvalidArgument, Key: a.Key, Asked: a.Amount,
					Message: fmt.Sprintf("%s: refunds %s more than %s is charged", a.Key, FormatAmount(left), object)}
			}
		}
	}
	for _, c := range out {
		if _, err := tx.Exec(ctx, `UPDATE charges SET amount = amount - ?
			WHERE object = ? AND payer = ? AND kind = ? AND class = ? AND region = ?`,
			c.Amount, c.Object, c.Payer, c.Kind, c.Class, c.Region); err != nil {
			return nil, err
		}
		if err := add(ctx, tx, c.Payer, c.Key, colUsed, -c.Amount); err != nil {
			return nil, err
		}
		// A refund in an over-committed root can bring it back under.
		if err := settle(ctx, tx, c.Payer, c.Key); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM charges WHERE object = ? AND amount = 0`, object); err != nil {
		return nil, err
	}
	return out, nil
}

// Drift is a quota row whose used the recount changed.
type Drift struct {
	Key
	Was, Now int64
}

// Recount sets each of silo's used to the sum of the charges it pays,
// grouped by payer, never by where the objects live. The reconcile
// calls it after bringing the charges in line with the objects that
// exist. It returns the rows it changed.
func Recount(ctx context.Context, tx store.Tx, silo string) ([]Drift, error) {
	want := map[Key]int64{}
	rows, err := tx.Query(ctx, `SELECT kind, class, region, SUM(amount) FROM charges
		WHERE payer = ? GROUP BY kind, class, region`, silo)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k Key
		var n int64
		if err := rows.Scan(&k.Kind, &k.Class, &k.Region, &n); err != nil {
			rows.Close()
			return nil, err
		}
		want[k] = n
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	have, err := queryRows(ctx, tx, `SELECT silo, kind, class, region, granted, delegated, used, consumed, over_committed
		FROM quota WHERE silo = ?`, silo)
	if err != nil {
		return nil, err
	}
	was := map[Key]int64{}
	for _, r := range have {
		was[r.Key] = r.Used
		if _, ok := want[r.Key]; !ok {
			want[r.Key] = 0
		}
	}
	var drift []Drift
	for k, n := range want {
		if was[k] != n {
			drift = append(drift, Drift{Key: k, Was: was[k], Now: n})
		}
	}
	sort.Slice(drift, func(i, j int) bool { return drift[i].Key.String() < drift[j].Key.String() })
	for _, d := range drift {
		if err := add(ctx, tx, silo, d.Key, colUsed, d.Now-d.Was); err != nil {
			return nil, err
		}
		if err := settle(ctx, tx, silo, d.Key); err != nil {
			return nil, err
		}
	}
	return drift, nil
}

// ---- Preview ----

// Payment is one line of what a change would charge, and to whom
// (custom-allocators.md item 8).
type Payment struct {
	Payer string
	Key
	Amount int64
}

// Level is the free amount of one key at one silo, before and after
// the change.
type Level struct {
	Silo, Path string
	Key
	FreeBefore, FreeAfter int64
}

// Preview is what a charge or a grant would do, without doing it.
type Preview struct {
	Charges []Payment
	// Levels runs from the silo the change is made in up to the root,
	// for each key.
	Levels []Level
	// Refusal is the error the change would fail with, or nil.
	Refusal error
}

// levels fills in each key's free amount from chain's last silo up to
// the root, with delta[silo] applied after.
func levels(ctx context.Context, tx store.Tx, chain []*tree.Silo, keys []Key, delta map[string]map[Key]int64) ([]Level, error) {
	var out []Level
	for _, k := range keys {
		for i := len(chain) - 1; i >= 0; i-- {
			s := chain[i]
			r, err := Get(ctx, tx, s.ID, k)
			if err != nil {
				return nil, err
			}
			out = append(out, Level{Silo: s.ID, Path: s.Path, Key: k,
				FreeBefore: r.Free(), FreeAfter: r.Free() + delta[s.ID][k]})
		}
	}
	return out, nil
}

// PreviewCharge reports what Charge would charge to silo, and the
// free amounts from silo up to the root, without charging anything.
func PreviewCharge(ctx context.Context, tx store.Tx, silo string, amounts []Amount) (*Preview, error) {
	chain, err := tree.Ancestry(ctx, tx, silo)
	if err != nil {
		return nil, err
	}
	as, err := sum(amounts)
	if err != nil {
		return nil, err
	}
	p := &Preview{}
	if _, err := planCharge(ctx, tx, chain, as); err != nil {
		if CodeOf(err) == "" {
			return nil, err
		}
		p.Refusal = err
	}
	delta := map[string]map[Key]int64{silo: {}}
	var keys []Key
	for _, a := range as {
		if _, err := checkKey(ctx, tx, a.Key); err != nil {
			continue // already the refusal
		}
		p.Charges = append(p.Charges, Payment{Payer: silo, Key: a.Key, Amount: a.Amount})
		delta[silo][a.Key] = -a.Amount
		keys = append(keys, a.Key)
	}
	if p.Levels, err = levels(ctx, tx, chain, keys, delta); err != nil {
		return nil, err
	}
	return p, nil
}

// PreviewGrant reports what Grant would do: the change to each grant
// is charged to the parent (a negative amount gives it back), and the
// free amounts run from the child up to the root.
func PreviewGrant(ctx context.Context, tx store.Tx, child string, amounts []Amount) (*Preview, error) {
	chain, err := tree.Ancestry(ctx, tx, child)
	if err != nil {
		return nil, err
	}
	p := &Preview{}
	plans, err := planGrant(ctx, tx, chain, amounts)
	if err != nil {
		if CodeOf(err) == "" {
			return nil, err
		}
		p.Refusal = err
	}
	if plans == nil && len(chain) > 1 {
		// Refused: preview what can be priced, unchecked.
		parent := chain[len(chain)-2].ID
		for _, a := range amounts {
			if _, err := checkKey(ctx, tx, a.Key); err != nil {
				continue
			}
			old, err := edge(ctx, tx, parent, child, a.Key)
			if err != nil {
				return nil, err
			}
			plans = append(plans, grantPlan{k: a.Key, old: old, new: a.Amount})
		}
	}
	if len(chain) < 2 {
		return p, nil
	}
	parent := chain[len(chain)-2].ID
	delta := map[string]map[Key]int64{child: {}, parent: {}}
	var keys []Key
	for _, g := range plans {
		d := g.new - g.old
		p.Charges = append(p.Charges, Payment{Payer: parent, Key: g.k, Amount: d})
		delta[child][g.k] = d
		delta[parent][g.k] = -d
		keys = append(keys, g.k)
	}
	if p.Levels, err = levels(ctx, tx, chain, keys, delta); err != nil {
		return nil, err
	}
	return p, nil
}

// ---- Checking the books ----

// Check verifies the whole ledger and returns every way it is wrong:
// granted that isn't the sum of the edges into a silo, delegated that
// isn't the sum of the edges out of it, used that isn't the sum of the
// charges it pays, and delegated + used > granted anywhere but an
// over-committed root. An empty result is a ledger in order.
func Check(ctx context.Context, tx store.Tx) ([]string, error) {
	var bad []string
	report := func(q, format string) error {
		rows, err := tx.Query(ctx, q)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var silo, kind, class, region string
			var a, b int64
			if err := rows.Scan(&silo, &kind, &class, &region, &a, &b); err != nil {
				return err
			}
			bad = append(bad, fmt.Sprintf(format, silo, region+" "+kind+"/"+class, a, b))
		}
		return rows.Err()
	}
	checks := []struct{ q, format string }{
		{`SELECT q.silo, q.kind, q.class, q.region, q.granted, COALESCE(e.n, 0) FROM quota q
			LEFT JOIN (SELECT to_silo, kind, class, region, SUM(amount) AS n FROM allocations
				GROUP BY to_silo, kind, class, region) e
			ON e.to_silo = q.silo AND e.kind = q.kind AND e.class = q.class AND e.region = q.region
			WHERE q.granted <> COALESCE(e.n, 0)`,
			"%s %s: granted %d, but the edges into it sum to %d"},
		{`SELECT q.silo, q.kind, q.class, q.region, q.delegated, COALESCE(e.n, 0) FROM quota q
			LEFT JOIN (SELECT from_silo, kind, class, region, SUM(amount) AS n FROM allocations
				GROUP BY from_silo, kind, class, region) e
			ON e.from_silo = q.silo AND e.kind = q.kind AND e.class = q.class AND e.region = q.region
			WHERE q.delegated <> COALESCE(e.n, 0)`,
			"%s %s: delegated %d, but the edges out of it sum to %d"},
		{`SELECT q.silo, q.kind, q.class, q.region, q.used, COALESCE(c.n, 0) FROM quota q
			LEFT JOIN (SELECT payer, kind, class, region, SUM(amount) AS n FROM charges
				GROUP BY payer, kind, class, region) c
			ON c.payer = q.silo AND c.kind = q.kind AND c.class = q.class AND c.region = q.region
			WHERE q.used <> COALESCE(c.n, 0)`,
			"%s %s: used %d, but the charges it pays sum to %d"},
		{`SELECT silo, kind, class, region, delegated + used, granted FROM quota
			WHERE delegated + used > granted AND NOT (over_committed AND silo IN (SELECT id FROM silos WHERE parent = ''))`,
			"%s %s: holds %d, more than its granted %d"},
		{`SELECT to_silo, kind, class, region, amount, 0 FROM allocations a
			WHERE from_silo <> '' AND from_silo <> (SELECT parent FROM silos WHERE id = a.to_silo)`,
			"%s %s: an edge of %d from something other than its parent (%d)"},
		{`SELECT silo, kind, class, region, used, 0 FROM quota WHERE used < 0 OR delegated < 0 OR granted < 0`,
			"%s %s: a negative amount (used %d) (%d)"},
	}
	for _, c := range checks {
		if err := report(c.q, c.format); err != nil {
			return nil, err
		}
	}
	return bad, nil
}

// ReleaseSilo returns everything a silo being deleted holds in the
// ledger to its parent: its grants go back to zero, and its quota rows
// and allowed regions are dropped. It refuses, with
// FAILED_PRECONDITION, a silo that still delegates to a child or pays
// for an object; deletion is bottom-up and deletes those first.
func ReleaseSilo(ctx context.Context, tx store.Tx, silo string) error {
	s, err := tree.Get(ctx, tx, silo)
	if err != nil {
		return err
	}
	if s.IsRoot() {
		return invalid("the root can't be deleted")
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM allocations WHERE from_silo = ?`, silo).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return precondition("%s still delegates quota to %d children", s.Path, n)
	}
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM charges WHERE payer = ? OR home = ?`, silo, silo).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return precondition("%s still holds %d charges for its objects", s.Path, n)
	}
	edges, err := Edges(ctx, tx, silo)
	if err != nil {
		return err
	}
	var zero []Amount
	for _, e := range edges {
		zero = append(zero, Amount{Key: e.Key})
	}
	if len(zero) > 0 {
		if err := Grant(ctx, tx, silo, zero); err != nil {
			return err
		}
	}
	for _, q := range []string{`DELETE FROM quota WHERE silo = ?`, `DELETE FROM silo_regions WHERE silo = ?`} {
		if _, err := tx.Exec(ctx, q, silo); err != nil {
			return err
		}
	}
	return nil
}
