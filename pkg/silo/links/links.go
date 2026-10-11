// Package links is L3 links' shared half (docs/silo/fence-api.md,
// "Links (L3)", SV-15): the links table, and the rules a link carries.
// The fence API proposes, matches, suspends and deletes them; the fence
// writes link-<l>-* allow rules for the ACTIVE ones. This package only
// stores them and says what a valid set of rules is. Every time is
// database time (Tx.Now).
//
// A link is two rows, one per side, each that side's proposal. A
// proposal is PENDING until the peer has proposed its mirror image: the
// same name, the same rules, the opposite direction (both matching
// both). Then both rows carry the same link id, and are ACTIVE or
// SUSPENDED together.
package links

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/store"
)

// Directions, from the proposing side.
const (
	Outbound = "outbound" // connections start in the proposer
	Inbound  = "inbound"  // connections start in the peer
	Both     = "both"
)

// States. A proposal the peer hasn't mirrored is PENDING.
const (
	StatePending   = "PENDING"
	StateActive    = "ACTIVE"
	StateSuspended = "SUSPENDED"
)

// ChangeKind is the change-log kind of a link's change: a proposal, an
// activation, a deletion, a suspension or a resumption. Its key is the
// proposal's id. The fence follows it.
const ChangeKind = "link"

// ErrNotFound is a lookup that found nothing.
var ErrNotFound = errors.New("links: not found")

// Rule is one protocol a link opens, with its ports for tcp and udp
// (none meaning every port).
type Rule struct {
	Protocol string   `json:"protocol"`
	Ports    []string `json:"ports,omitempty"`
}

// Protocols are the protocols a link may name. "all" stands alone.
var Protocols = map[string]bool{"tcp": true, "udp": true, "icmp": true, "sctp": true, "esp": true, "ah": true, "all": true}

// portful are the protocols that take ports.
var portful = map[string]bool{"tcp": true, "udp": true}

// MaxRules and MaxPorts bound what one link may name, since every
// protocol and port is an attribute of the fence's quota.
const (
	MaxRules = 8
	MaxPorts = 32
)

// Canonical checks rules and returns them in one canonical form, so
// that two sides that name the same protocols and ports in another
// order or spelling agree: protocols lower-cased, sorted and merged;
// ports sorted and deduplicated, a whole-range rule dropping its ports.
func Canonical(rules []Rule) ([]Rule, error) {
	if len(rules) == 0 {
		return nil, errors.New("a link needs at least one rule")
	}
	if len(rules) > MaxRules {
		return nil, fmt.Errorf("a link names at most %d rules", MaxRules)
	}
	byProto := map[string]map[string]bool{} // nil: every port
	nports := 0
	for _, r := range rules {
		p := strings.ToLower(strings.TrimSpace(r.Protocol))
		if !Protocols[p] {
			return nil, fmt.Errorf("protocol %q: want one of tcp, udp, icmp, sctp, esp, ah or all", r.Protocol)
		}
		if len(r.Ports) > 0 && !portful[p] {
			return nil, fmt.Errorf("protocol %s takes no ports: ports are for tcp and udp", p)
		}
		cur, seen := byProto[p]
		if len(r.Ports) == 0 {
			byProto[p] = nil
			continue
		}
		if seen && cur == nil {
			continue // every port already
		}
		if cur == nil {
			cur = map[string]bool{}
			byProto[p] = cur
		}
		for _, s := range r.Ports {
			lo, hi, err := PortRange(s)
			if err != nil {
				return nil, fmt.Errorf("port %q: %v", s, err)
			}
			if lo == hi {
				s = strconv.Itoa(lo)
			} else {
				s = strconv.Itoa(lo) + "-" + strconv.Itoa(hi)
			}
			if !cur[s] {
				cur[s] = true
				nports++
			}
		}
	}
	if nports > MaxPorts {
		return nil, fmt.Errorf("a link names at most %d ports", MaxPorts)
	}
	if _, ok := byProto["all"]; ok && len(byProto) > 1 {
		return nil, errors.New(`{"protocol": "all"} stands alone: it already covers every other protocol`)
	}
	var out []Rule
	for p, ports := range byProto {
		r := Rule{Protocol: p}
		for s := range ports {
			r.Ports = append(r.Ports, s)
		}
		sort.Slice(r.Ports, func(i, j int) bool {
			a, _, _ := PortRange(r.Ports[i])
			b, _, _ := PortRange(r.Ports[j])
			if a != b {
				return a < b
			}
			return r.Ports[i] < r.Ports[j]
		})
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Protocol < out[j].Protocol })
	return out, nil
}

// PortRange reads "n" or "lo-hi".
func PortRange(s string) (lo, hi int, err error) {
	a, b, ok := strings.Cut(strings.TrimSpace(s), "-")
	if !ok {
		b = a
	}
	if lo, err = strconv.Atoi(a); err == nil {
		hi, err = strconv.Atoi(b)
	}
	if err != nil || lo < 1 || hi > 65535 || lo > hi {
		return 0, 0, errors.New("want a port or a range lo-hi, within 1-65535")
	}
	return lo, hi, nil
}

// Mirror is the direction the peer must propose.
func Mirror(direction string) string {
	switch direction {
	case Outbound:
		return Inbound
	case Inbound:
		return Outbound
	}
	return direction
}

// Link is one side's proposal.
type Link struct {
	ID        string
	Silo      string
	Name      string
	PeerPath  string
	PeerSilo  string // "" if no silo held PeerPath, or it is gone
	Direction string
	Rules     []Rule // canonical
	LinkID    string // "" until matched
	State     string
	Created   time.Time
}

// Mirrors reports whether a and b are each other's mirror image: the
// same name and rules, between the same two silos, the opposite
// directions.
func Mirrors(a, b *Link) bool {
	if a.Silo == "" || b.Silo == "" || a.PeerSilo != b.Silo || b.PeerSilo != a.Silo || a.Name != b.Name ||
		Mirror(a.Direction) != b.Direction {
		return false
	}
	ra, _ := json.Marshal(a.Rules)
	rb, _ := json.Marshal(b.Rules)
	return string(ra) == string(rb)
}

// Starts reports whether connections may start in l's own silo.
func (l *Link) Starts() bool { return l.Direction == Outbound || l.Direction == Both }

// Accepts reports whether connections may start in l's peer.
func (l *Link) Accepts() bool { return l.Direction == Inbound || l.Direction == Both }

const cols = `id, silo, name, peer_path, peer_silo, direction, rules, link, state, created_at`

func scan(rows *sql.Rows, err error) ([]*Link, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Link
	for rows.Next() {
		var l Link
		var rules string
		var at int64
		if err := rows.Scan(&l.ID, &l.Silo, &l.Name, &l.PeerPath, &l.PeerSilo, &l.Direction, &rules, &l.LinkID, &l.State, &at); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(rules), &l.Rules); err != nil {
			return nil, fmt.Errorf("link %s rules: %w", l.ID, err)
		}
		l.Created = store.Time(at)
		out = append(out, &l)
	}
	return out, rows.Err()
}

func one(l []*Link, err error) (*Link, error) {
	if err != nil {
		return nil, err
	}
	if len(l) == 0 {
		return nil, ErrNotFound
	}
	return l[0], nil
}

// Create stores l, whose rules are canonical.
func Create(ctx context.Context, tx store.Tx, l *Link) error {
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	rules, err := json.Marshal(l.Rules)
	if err != nil {
		return err
	}
	l.Created = now
	_, err = tx.Exec(ctx, `INSERT INTO links (`+cols+`, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.ID, l.Silo, l.Name, l.PeerPath, l.PeerSilo, l.Direction, string(rules), l.LinkID, l.State,
		store.Millis(now), store.Millis(now))
	return err
}

// Get is the proposal with id.
func Get(ctx context.Context, tx store.Tx, id string) (*Link, error) {
	return one(scan(tx.Query(ctx, `SELECT `+cols+` FROM links WHERE id = ?`, id)))
}

// ByName is silo's proposal called name.
func ByName(ctx context.Context, tx store.Tx, silo, name string) (*Link, error) {
	return one(scan(tx.Query(ctx, `SELECT `+cols+` FROM links WHERE silo = ? AND name = ?`, silo, name)))
}

// Of is silo's proposals, by name.
func Of(ctx context.Context, tx store.Tx, silo string) ([]*Link, error) {
	return scan(tx.Query(ctx, `SELECT `+cols+` FROM links WHERE silo = ? ORDER BY name`, silo))
}

// Partner is the other side's row of l's link: the proposal silo
// l.PeerSilo made under l's name, naming l's silo as its peer. It is
// ErrNotFound when the peer has proposed nothing that could match.
func Partner(ctx context.Context, tx store.Tx, l *Link) (*Link, error) {
	if l.PeerSilo == "" {
		return nil, ErrNotFound
	}
	return one(scan(tx.Query(ctx, `SELECT `+cols+` FROM links WHERE silo = ? AND name = ? AND peer_silo = ?`,
		l.PeerSilo, l.Name, l.Silo)))
}

// Matched is every matched row, ordered by link id and then silo, so
// each link's two rows are adjacent.
func Matched(ctx context.Context, tx store.Tx) ([]*Link, error) {
	return scan(tx.Query(ctx, `SELECT `+cols+` FROM links WHERE link <> '' ORDER BY link, silo`))
}

// Pairs is every matched link as its two rows, the one of the lower
// silo id first. A row whose partner is missing is left out.
func Pairs(ctx context.Context, tx store.Tx) ([][2]*Link, error) {
	ls, err := Matched(ctx, tx)
	if err != nil {
		return nil, err
	}
	var out [][2]*Link
	for i := 0; i+1 < len(ls); i++ {
		if ls[i].LinkID == ls[i+1].LinkID {
			out = append(out, [2]*Link{ls[i], ls[i+1]})
			i++
		}
	}
	return out, nil
}

// SetMatch records l's link id and state.
func SetMatch(ctx context.Context, tx store.Tx, id, link, state string) error {
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE links SET link = ?, state = ?, updated_at = ? WHERE id = ?`, link, state, store.Millis(now), id)
	return err
}

// Delete drops one proposal.
func Delete(ctx context.Context, tx store.Tx, id string) error {
	_, err := tx.Exec(ctx, `DELETE FROM links WHERE id = ?`, id)
	return err
}

// ForgetPeer clears peer_silo on every proposal naming silo, as silo
// is deleted: a silo made later at the same path is another silo, and
// must propose again for itself.
func ForgetPeer(ctx context.Context, tx store.Tx, silo string) error {
	_, err := tx.Exec(ctx, `UPDATE links SET peer_silo = '' WHERE peer_silo = ?`, silo)
	return err
}

// RuleNames are the fence rules a link may write: link-<l>-out and
// link-<l>-in for one direction, and, for a two-way link,
// link-<l>-rev-out and link-<l>-rev-in for the other.
func RuleNames(link string) (out, in, revOut, revIn string) {
	p := "link-" + link
	return p + "-out", p + "-in", p + "-rev-out", p + "-rev-in"
}
