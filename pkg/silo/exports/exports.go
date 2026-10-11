// Package exports is proxied exports' shared half (docs/silo/fence-api.md,
// "Exports (proxied)", SV-14; gateway.md, "Proxied exports", SV-83): the
// exports, export_offers and imports tables. The fence API offers,
// accepts and withdraws them and decides when an import is suspended;
// the gateway carries their traffic and the fence opens
// export-<id>-in for them. This package only stores them. Every time is
// database time (Tx.Now).
package exports

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/store"
)

// Protocols.
const (
	HTTP = "http"
	TCP  = "tcp"
)

// Import states. An offer that hasn't been accepted has no import, and
// is PENDING.
const (
	StatePending   = "PENDING"
	StateActive    = "ACTIVE"
	StateSuspended = "SUSPENDED"
)

// OwnerTenant is the one owner v1 has: the exporting silo itself
// (custom-allocators.md item 10). A later owner, a handle, is kept apart
// so a tenant can't withdraw what the handle owns.
const OwnerTenant = "tenant"

// The gateway's TCP export ports, one per accepted tcp import.
const FirstPort, LastPort = 20000, 29999

// ChangeKind is the change-log kind of an export's change: an offer,
// an accept, a withdrawal, a suspension or a resumption. Its key is the
// export's id. The gateway's tables and the fence follow it.
const ChangeKind = "export"

// ErrNotFound is a lookup that found nothing.
var ErrNotFound = errors.New("exports: not found")

// ErrPortsExhausted is an accept with every TCP export port taken.
var ErrPortsExhausted = errors.New("exports: every TCP export port is taken")

// Export is one export.
type Export struct {
	ID            string
	Exporter      string
	Name          string
	Owner         string
	Protocol      string
	TargetIP      string // "" while an instance target has no address
	TargetPort    int
	Instance      string // "" for an ip:port target
	Zone          string
	ProxyProtocol bool
	Created       time.Time
}

// Target is the export's ip:port, invalid while it has no address.
func (e *Export) Target() netip.AddrPort {
	a, err := netip.ParseAddr(e.TargetIP)
	if err != nil {
		return netip.AddrPort{}
	}
	return netip.AddrPortFrom(a, uint16(e.TargetPort))
}

// Offer is an export offered to one silo.
type Offer struct {
	Export string
	ToPath string
	ToSilo string // "" if no silo held ToPath when it was offered
}

// Import is an accepted offer.
type Import struct {
	ID       string
	Export   string
	Importer string
	Port     int // the gateway's port, for a tcp export
	State    string
	Created  time.Time
}

const exportCols = `id, exporter, name, owner, protocol, target_ip, target_port, instance, zone, proxy_protocol, created_at`

func scanExports(rows *sql.Rows, err error) ([]*Export, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Export
	for rows.Next() {
		var e Export
		var at int64
		if err := rows.Scan(&e.ID, &e.Exporter, &e.Name, &e.Owner, &e.Protocol, &e.TargetIP, &e.TargetPort,
			&e.Instance, &e.Zone, &e.ProxyProtocol, &at); err != nil {
			return nil, err
		}
		e.Created = store.Time(at)
		out = append(out, &e)
	}
	return out, rows.Err()
}

func one[T any](l []*T, err error) (*T, error) {
	if err != nil {
		return nil, err
	}
	if len(l) == 0 {
		return nil, ErrNotFound
	}
	return l[0], nil
}

// Create stores e. An export of the same name from the same silo is
// replaced only by Delete first; Create fails on one.
func Create(ctx context.Context, tx store.Tx, e *Export) error {
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	e.Created = now
	_, err = tx.Exec(ctx, `INSERT INTO exports (`+exportCols+`, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.Exporter, e.Name, e.Owner, e.Protocol, e.TargetIP, e.TargetPort, e.Instance, e.Zone, e.ProxyProtocol,
		store.Millis(now), store.Millis(now))
	return err
}

// Get is the export with id.
func Get(ctx context.Context, tx store.Tx, id string) (*Export, error) {
	return one(scanExports(tx.Query(ctx, `SELECT `+exportCols+` FROM exports WHERE id = ?`, id)))
}

// ByName is exporter's export called name.
func ByName(ctx context.Context, tx store.Tx, exporter, name string) (*Export, error) {
	return one(scanExports(tx.Query(ctx, `SELECT `+exportCols+` FROM exports WHERE exporter = ? AND name = ?`, exporter, name)))
}

// Of is exporter's exports, by name.
func Of(ctx context.Context, tx store.Tx, exporter string) ([]*Export, error) {
	return scanExports(tx.Query(ctx, `SELECT `+exportCols+` FROM exports WHERE exporter = ? ORDER BY name`, exporter))
}

// All is every export, by id.
func All(ctx context.Context, tx store.Tx) ([]*Export, error) {
	return scanExports(tx.Query(ctx, `SELECT `+exportCols+` FROM exports ORDER BY id`))
}

// SetTarget moves an instance export to the instance's new address.
func SetTarget(ctx context.Context, tx store.Tx, id, ip string) error {
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE exports SET target_ip = ?, updated_at = ? WHERE id = ?`, ip, store.Millis(now), id)
	return err
}

// Delete drops an export with its offers and imports.
func Delete(ctx context.Context, tx store.Tx, id string) error {
	for _, q := range []string{`DELETE FROM imports WHERE export = ?`, `DELETE FROM export_offers WHERE export = ?`, `DELETE FROM exports WHERE id = ?`} {
		if _, err := tx.Exec(ctx, q, id); err != nil {
			return err
		}
	}
	return nil
}

func scanOffers(rows *sql.Rows, err error) ([]*Offer, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Offer
	for rows.Next() {
		var o Offer
		if err := rows.Scan(&o.Export, &o.ToPath, &o.ToSilo); err != nil {
			return nil, err
		}
		out = append(out, &o)
	}
	return out, rows.Err()
}

// AddOffer offers export to the silo at path, toSilo being its id or ""
// for none. Offering it again changes nothing.
func AddOffer(ctx context.Context, tx store.Tx, export, path, toSilo string) error {
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO export_offers (export, to_path, to_silo, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (export, to_path) DO NOTHING`, export, path, toSilo, store.Millis(now))
	return err
}

// Offers is who export is offered to, by path.
func Offers(ctx context.Context, tx store.Tx, export string) ([]*Offer, error) {
	return scanOffers(tx.Query(ctx, `SELECT export, to_path, to_silo FROM export_offers WHERE export = ? ORDER BY to_path`, export))
}

// OffersTo is every offer made to silo.
func OffersTo(ctx context.Context, tx store.Tx, silo string) ([]*Offer, error) {
	if silo == "" {
		return nil, nil
	}
	return scanOffers(tx.Query(ctx, `SELECT export, to_path, to_silo FROM export_offers WHERE to_silo = ? ORDER BY export`, silo))
}

// DeleteOffer withdraws export's offer to path, and the import the
// silo offered it made of it.
func DeleteOffer(ctx context.Context, tx store.Tx, o *Offer) error {
	if o.ToSilo != "" {
		if _, err := tx.Exec(ctx, `DELETE FROM imports WHERE export = ? AND importer = ?`, o.Export, o.ToSilo); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `DELETE FROM export_offers WHERE export = ? AND to_path = ?`, o.Export, o.ToPath)
	return err
}

const importCols = `id, export, importer, port, state, created_at`

func scanImports(rows *sql.Rows, err error) ([]*Import, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Import
	for rows.Next() {
		var i Import
		var at int64
		if err := rows.Scan(&i.ID, &i.Export, &i.Importer, &i.Port, &i.State, &at); err != nil {
			return nil, err
		}
		i.Created = store.Time(at)
		out = append(out, &i)
	}
	return out, rows.Err()
}

// CreateImport stores i, giving it the lowest free TCP export port if
// tcp is set.
func CreateImport(ctx context.Context, tx store.Tx, i *Import, tcp bool) error {
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	if tcp {
		if i.Port, err = freePort(ctx, tx); err != nil {
			return err
		}
	}
	i.Created = now
	_, err = tx.Exec(ctx, `INSERT INTO imports (`+importCols+`, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		i.ID, i.Export, i.Importer, i.Port, i.State, store.Millis(now), store.Millis(now))
	return err
}

func freePort(ctx context.Context, tx store.Tx) (int, error) {
	rows, err := tx.Query(ctx, `SELECT port FROM imports WHERE port > 0 ORDER BY port`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	want := FirstPort
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return 0, err
		}
		if p > want {
			break
		}
		if p == want {
			want++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if want > LastPort {
		return 0, ErrPortsExhausted
	}
	return want, nil
}

// GetImport is the import with id.
func GetImport(ctx context.Context, tx store.Tx, id string) (*Import, error) {
	return one(scanImports(tx.Query(ctx, `SELECT `+importCols+` FROM imports WHERE id = ?`, id)))
}

// ImportOf is importer's import of export.
func ImportOf(ctx context.Context, tx store.Tx, export, importer string) (*Import, error) {
	return one(scanImports(tx.Query(ctx, `SELECT `+importCols+` FROM imports WHERE export = ? AND importer = ?`, export, importer)))
}

// ImportsOf is every import of export.
func ImportsOf(ctx context.Context, tx store.Tx, export string) ([]*Import, error) {
	return scanImports(tx.Query(ctx, `SELECT `+importCols+` FROM imports WHERE export = ? ORDER BY id`, export))
}

// ImportsBy is every import importer made.
func ImportsBy(ctx context.Context, tx store.Tx, importer string) ([]*Import, error) {
	return scanImports(tx.Query(ctx, `SELECT `+importCols+` FROM imports WHERE importer = ? ORDER BY id`, importer))
}

// AllImports is every import.
func AllImports(ctx context.Context, tx store.Tx) ([]*Import, error) {
	return scanImports(tx.Query(ctx, `SELECT `+importCols+` FROM imports ORDER BY id`))
}

// SetState sets an import's state.
func SetState(ctx context.Context, tx store.Tx, id, state string) error {
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE imports SET state = ?, updated_at = ? WHERE id = ?`, state, store.Millis(now), id)
	return err
}

// DeleteImport drops an import; its offer stays, so it can be accepted
// again.
func DeleteImport(ctx context.Context, tx store.Tx, id string) error {
	_, err := tx.Exec(ctx, `DELETE FROM imports WHERE id = ?`, id)
	return err
}

// Address is where an importer reaches an export through the gateway:
// http://silo.internal/x/<exporter-id>/<name>/ for http, and
// silo.internal:<port> for tcp.
func Address(e *Export, i *Import) string {
	if e.Protocol == TCP {
		return "silo.internal:" + strconv.Itoa(i.Port)
	}
	return fmt.Sprintf("http://silo.internal/x/%s/%s/", e.Exporter, e.Name)
}

// RuleName is the fence rule that lets the gateway reach e's target.
func RuleName(e *Export) string { return "export-" + e.ID + "-in" }
