package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/hub/dbaccess"
	_ "github.com/jackc/pgx/v5/stdlib" // "pgx"
	_ "modernc.org/sqlite"             // "sqlite"
)

// Store is the shared registry (docs/hub.md §2.2.2, stage R2): every
// pod's registry, published to the hub's database so that the pods see
// each other's nodes. hub_pods holds each pod's lease, hub_nodes one row
// per node ever registered, naming the pod and the connection holding
// it. A Registry with a Store writes its own nodes through to it and
// mirrors everyone else's (Registry.Run).
type Store struct {
	db       *sql.DB
	postgres bool
}

// OpenStore opens the shared registry at url, as the hub's other stores
// take it: "postgres://..." (or "postgresql://...") for PostgreSQL,
// "sqlite:PATH" or a bare path for SQLite. It creates hub_pods and
// hub_nodes if they are missing.
func OpenStore(ctx context.Context, url string) (*Store, error) { return openStore(ctx, url, true) }

// OpenExistingStore opens the shared registry at url as OpenStore does,
// but creates nothing: the tables were made by OpenStore or `silo hub
// migrate` as a role that may create them, and url's role need only
// read and write their rows (docs/deploy.md). It fails if they are
// missing.
func OpenExistingStore(ctx context.Context, url string) (*Store, error) {
	return openStore(ctx, url, false)
}

func openStore(ctx context.Context, url string, create bool) (*Store, error) {
	s := &Store{postgres: strings.HasPrefix(url, "postgres://") || strings.HasPrefix(url, "postgresql://")}
	var err error
	if s.postgres {
		s.db, err = sql.Open("pgx", url)
	} else {
		path := strings.TrimPrefix(url, "sqlite:")
		s.db, err = sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
		s.db.SetMaxOpenConns(1)
	}
	if err != nil {
		return nil, err
	}
	if !create {
		if err := dbaccess.Check(ctx, s.db, "registry", "hub_pods", "hub_nodes"); err != nil {
			s.db.Close()
			return nil, err
		}
		return s, nil
	}
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS hub_pods (
	pod TEXT PRIMARY KEY,
	addr TEXT NOT NULL,
	lease_until BIGINT NOT NULL
)`,
		`CREATE TABLE IF NOT EXISTS hub_nodes (
	id TEXT PRIMARY KEY,
	owner TEXT NOT NULL,
	pod TEXT NOT NULL,
	conn TEXT NOT NULL,
	repos TEXT NOT NULL,
	slots BIGINT NOT NULL,
	version TEXT NOT NULL,
	labels TEXT NOT NULL,
	free BIGINT NOT NULL,
	settled BIGINT NOT NULL,
	connected_at BIGINT NOT NULL,
	last_seen BIGINT NOT NULL
)`,
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			s.db.Close()
			return nil, fmt.Errorf("registry: creating tables: %w", err)
		}
	}
	return s, nil
}

// Close closes the store.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) q(query string) string {
	if !s.postgres {
		return query
	}
	var b strings.Builder
	n := 0
	for _, c := range query {
		if c == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

func (s *Store) exec(ctx context.Context, query string, args ...any) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.q(query), args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// renewPod writes pod's lease, until until.
func (s *Store) renewPod(ctx context.Context, pod, addr string, until time.Time) error {
	_, err := s.exec(ctx, `INSERT INTO hub_pods (pod, addr, lease_until) VALUES (?, ?, ?)
		ON CONFLICT (pod) DO UPDATE SET addr = excluded.addr, lease_until = excluded.lease_until`,
		pod, addr, until.UnixMilli())
	return err
}

// deletePod removes pod's lease, taking its nodes offline at once.
func (s *Store) deletePod(ctx context.Context, pod string) error {
	_, err := s.exec(ctx, `DELETE FROM hub_pods WHERE pod = ?`, pod)
	return err
}

// row is one hub_nodes row.
type row struct {
	Entry
	pod, conn  string
	settled    int64
	leaseUntil time.Time // the pod's, from hub_pods; zero when it has none
}

// register inserts the node's row, or takes it over for a new
// connection, only where it belongs to e.Owner: ErrOwner otherwise.
func (s *Store) register(ctx context.Context, e Entry, pod, conn string) error {
	repos, labels, err := encode(e.Node)
	if err != nil {
		return err
	}
	n, err := s.exec(ctx, `INSERT INTO hub_nodes (id, owner, pod, conn, repos, slots, version, labels, free, settled, connected_at, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)
		ON CONFLICT (id) DO UPDATE SET pod = excluded.pod, conn = excluded.conn, repos = excluded.repos,
			slots = excluded.slots, version = excluded.version, labels = excluded.labels, free = excluded.free,
			settled = hub_nodes.settled + 1, connected_at = excluded.connected_at, last_seen = excluded.last_seen
		WHERE hub_nodes.owner = excluded.owner`,
		e.ID, e.Owner, pod, conn, repos, e.Slots, e.Version, labels, e.Free, e.ConnectedAt.UnixMilli(), e.LastSeen.UnixMilli())
	if err != nil {
		return fmt.Errorf("registry: registering %s: %w", e.ID, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrOwner, e.ID)
	}
	return nil
}

// heartbeat records connection conn's heartbeat. It reports false when
// conn no longer holds the node: a newer connection, on any pod,
// superseded it.
func (s *Store) heartbeat(ctx context.Context, id, conn string, free int, seen time.Time) (bool, error) {
	n, err := s.exec(ctx, `UPDATE hub_nodes SET free = ?, settled = settled + 1, last_seen = ? WHERE id = ? AND conn = ?`,
		free, seen.UnixMilli(), id, conn)
	return n > 0, err
}

// close takes the node offline, if connection conn still holds it.
func (s *Store) close(ctx context.Context, id, conn string) error {
	_, err := s.exec(ctx, `UPDATE hub_nodes SET pod = '', conn = '', free = 0 WHERE id = ? AND conn = ?`, id, conn)
	return err
}

// clearPod takes offline the nodes rows say pod holds that it does not:
// an earlier process under the same pod name ended without closing them.
func (s *Store) clearPod(ctx context.Context, pod string, held map[string]string) error {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT id, conn FROM hub_nodes WHERE pod = ?`), pod)
	if err != nil {
		return err
	}
	var stale [][2]string
	for rows.Next() {
		var id, conn string
		if err := rows.Scan(&id, &conn); err != nil {
			rows.Close()
			return err
		}
		if held[id] != conn {
			stale = append(stale, [2]string{id, conn})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range stale {
		if err := s.close(ctx, r[0], r[1]); err != nil {
			return err
		}
	}
	return nil
}

// forget deletes the node's row, if conn (empty for an offline node) is
// still the connection it names.
func (s *Store) forget(ctx context.Context, id, conn string) (bool, error) {
	n, err := s.exec(ctx, `DELETE FROM hub_nodes WHERE id = ? AND conn = ?`, id, conn)
	return n > 0, err
}

// unbind deletes the node's row, whoever holds it.
func (s *Store) unbind(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM hub_nodes WHERE id = ?`, id)
	return err
}

// load reads every node, with its pod's lease.
func (s *Store) load(ctx context.Context) ([]row, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT n.id, n.owner, n.pod, n.conn, n.repos, n.slots, n.version, n.labels,
		n.free, n.settled, n.connected_at, n.last_seen, p.lease_until
		FROM hub_nodes n LEFT JOIN hub_pods p ON p.pod = n.pod`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []row
	for rows.Next() {
		var (
			r                   row
			repos, labels       string
			connected, lastSeen int64
			until               sql.NullInt64
		)
		if err := rows.Scan(&r.ID, &r.Owner, &r.pod, &r.conn, &repos, &r.Slots, &r.Version, &labels,
			&r.Free, &r.settled, &connected, &lastSeen, &until); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(repos), &r.Repos); err != nil {
			return nil, fmt.Errorf("registry: node %s: repos: %w", r.ID, err)
		}
		if err := json.Unmarshal([]byte(labels), &r.Labels); err != nil {
			return nil, fmt.Errorf("registry: node %s: labels: %w", r.ID, err)
		}
		r.ConnectedAt, r.LastSeen = time.UnixMilli(connected), time.UnixMilli(lastSeen)
		if until.Valid {
			r.leaseUntil = time.UnixMilli(until.Int64)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func encode(n Node) (repos, labels string, err error) {
	r, err := json.Marshal(append([]string{}, n.Repos...))
	if err != nil {
		return "", "", err
	}
	l, err := json.Marshal(n.Labels)
	if err != nil {
		return "", "", err
	}
	return string(r), string(l), nil
}

// holder reads the pod holding the node's connection, that pod's
// address and its lease; pod is empty when no pod holds it.
func (s *Store) holder(ctx context.Context, id string) (pod, addr string, until time.Time, err error) {
	var ms int64
	err = s.db.QueryRowContext(ctx, s.q(`SELECT n.pod, p.addr, p.lease_until FROM hub_nodes n
		JOIN hub_pods p ON p.pod = n.pod WHERE n.id = ? AND n.conn <> ''`), id).Scan(&pod, &addr, &ms)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", time.Time{}, nil
	}
	if err != nil {
		return "", "", time.Time{}, err
	}
	return pod, addr, time.UnixMilli(ms), nil
}
