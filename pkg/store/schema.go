package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SchemaCheck says why a release carrying migrations up to latest, whose
// queries need schema oldest or newer, can't serve a database at schema
// version v whose contract steps require requires: "schema_mismatch",
// or "" if it can (state.md "Schema migrations and upgrades", SV-53).
// A schema newer than latest is fine as long as every step past latest
// was an expand step, which is how release N keeps running while N+1's
// migrations are applied, and how a rollback runs on the newer schema.
func SchemaCheck(v, requires, oldest, latest int) string {
	if v < oldest || requires > latest {
		return "schema_mismatch"
	}
	return ""
}

// Replica is one silod process's heartbeat row.
type Replica struct {
	ID      string
	Release string
	// Latest is the newest migration the replica's binary carries.
	Latest  int
	Started time.Time
	// Sessions is how many sessions the replica serves, and Address
	// where it serves them (from LeaderSchema on; written as 0 and ""
	// before).
	Sessions int
	Address  string
	// Host is the control VM the replica runs on, AgentSlots how many
	// harness sessions it can run and AgentSessions how many it runs
	// (from HostSchema on).
	Host          string
	AgentSlots    int
	AgentSessions int
}

// MirrorSchema is the first schema with the outbound mirror's tables
// (local-git.md §8).
const MirrorSchema = 26

// HostSchema is the first schema with agents' hosts, the heartbeat's
// session slots and the harnesses' saved state.
const HostSchema = 17

// LeaderSchema is the first schema with the leader epoch, the
// heartbeat's sessions and address, and the reconcile's orphans.
const LeaderSchema = 12

// ReplicaTimeout is how long a replica's heartbeat may go unwritten
// before it counts as gone: the leader adopts its GCE operations
// (state.md "More than one silod, and the leader", SV-52).
const ReplicaTimeout = 30 * time.Second

// HeartbeatSchema is the first schema with the replicas and settings
// tables.
const HeartbeatSchema = 10

// Heartbeat writes r's heartbeat at the database's time.
func Heartbeat(ctx context.Context, s Store, r Replica) error {
	v, err := s.Schema(ctx)
	if err != nil {
		return err
	}
	return s.Tx(ctx, func(tx Tx) error {
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		if v >= HostSchema {
			_, err = tx.Exec(ctx, `INSERT INTO replicas (id, release, schema_latest, started_at, heartbeat_at, sessions, address, host, agent_slots, agent_sessions)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET release = excluded.release, schema_latest = excluded.schema_latest,
			heartbeat_at = excluded.heartbeat_at, sessions = excluded.sessions, address = excluded.address,
			host = excluded.host, agent_slots = excluded.agent_slots, agent_sessions = excluded.agent_sessions`,
				r.ID, r.Release, r.Latest, Millis(r.Started), Millis(now), r.Sessions, r.Address, r.Host, r.AgentSlots, r.AgentSessions)
			return err
		}
		if v >= LeaderSchema {
			_, err = tx.Exec(ctx, `INSERT INTO replicas (id, release, schema_latest, started_at, heartbeat_at, sessions, address) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET release = excluded.release, schema_latest = excluded.schema_latest,
			heartbeat_at = excluded.heartbeat_at, sessions = excluded.sessions, address = excluded.address`,
				r.ID, r.Release, r.Latest, Millis(r.Started), Millis(now), r.Sessions, r.Address)
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO replicas (id, release, schema_latest, started_at, heartbeat_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET release = excluded.release, schema_latest = excluded.schema_latest,
			heartbeat_at = excluded.heartbeat_at`,
			r.ID, r.Release, r.Latest, Millis(r.Started), Millis(now))
		return err
	})
}

// Depart deletes r's heartbeat, as a replica stops.
func Depart(ctx context.Context, s Store, id string) error {
	return s.Tx(ctx, func(tx Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM replicas WHERE id = ?`, id)
		return err
	})
}

// Setting names.
const (
	// SettingReconcile is how reconcile treats orphans: "report-only"
	// after install adopts a restored instance, until an operator runs
	// silo reconcile --apply; absent otherwise.
	SettingReconcile    = "reconcile"
	ReconcileReportOnly = "report-only"
	// ReconcileApply is the setting silo reconcile --apply writes:
	// orphans are deleted after their grace period, as when unset.
	ReconcileApply = "apply"
	// SettingDataDirID is, on the local backend, the id in the data
	// directory's layout.json that this store belongs with
	// (local-rootless-storage.md call L35). Written at the first start
	// on a data directory, and checked at every later one.
	SettingDataDirID = "data_dir_id"
	// SettingStorageDriver is, on the local backend, the storage
	// driver the data directory's silos are kept with ("dir"). It is
	// written once and never changes (call L37).
	SettingStorageDriver = "storage_driver"
	// SettingHolderMapping is, on the local backend, the uid and gid
	// mapping of silo-nsd's user namespace (nsd.Mapping's String). It
	// is written at the first start, and every later one, a reboot's
	// fresh namespaces included, must find the same, or every
	// container range on disk would belong to other ids
	// (local-rootless-storage.md calls L29 and L33).
	SettingHolderMapping = "holder_mapping"
)

// SetSetting writes a setting.
func SetSetting(ctx context.Context, s Store, name, value string) error {
	return s.Tx(ctx, func(tx Tx) error { return WriteSetting(ctx, tx, name, value) })
}

// WriteSetting writes a setting, in tx.
func WriteSetting(ctx context.Context, tx Tx, name, value string) error {
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO settings (name, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		name, value, Millis(now))
	return err
}

// GetSetting reads a setting; ok is false if it isn't set.
func GetSetting(ctx context.Context, s Store, name string) (value string, ok bool, err error) {
	err = s.Tx(ctx, func(tx Tx) error {
		value, ok, err = ReadSetting(ctx, tx, name)
		return err
	})
	return value, ok, err
}

// ReadSetting reads a setting, in tx; ok is false if it isn't set.
func ReadSetting(ctx context.Context, tx Tx, name string) (value string, ok bool, err error) {
	err = tx.QueryRow(ctx, `SELECT value FROM settings WHERE name = ?`, name).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading setting %s: %w", name, err)
	}
	return value, true, nil
}
