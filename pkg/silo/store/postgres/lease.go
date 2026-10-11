package postgres

import (
	"context"
	"hash/fnv"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bwsalmon/silo/pkg/silo/store"
)

// lockNamespace is the first half of every advisory lock key the store
// takes, so silod's locks stay apart from anything else that uses
// advisory locks on the same database. It is "silo" in ASCII.
const lockNamespace int32 = 0x73696c6f

// lockKey is the fixed advisory lock key of a lease name: the silo
// namespace, and the name's 32-bit FNV-1a hash.
func lockKey(name string) (int32, int32) {
	h := fnv.New32a()
	h.Write([]byte(name))
	return lockNamespace, int32(h.Sum32())
}

// Lead implements store.Store (SV-52, "Leader election" and "Stepping
// down").
//
// The lease is a session-level advisory lock, pg_try_advisory_lock on
// the name's fixed key, held on a connection of its own. A waiter tries
// it every five seconds; Postgres frees it when the holder's session
// ends. Within one process, waiters for a name queue on a semaphore
// first, so only one of them at a time tries the lock and one that is
// handed the lease by its own process takes it at once rather than at
// its next try.
//
// The holder pings its connection every two seconds and closes lost on
// the first ping that fails or takes longer than two seconds. The
// connection's TCP keepalive, on both ends, is about ten seconds, so a
// holder cut off from the database learns it has lost the lease before
// Postgres frees the lock. A holder whose session is killed outright
// (pg_terminate_backend, a server restart) can't be warned first: the
// lock is free at once, and the holder closes lost at its next ping, up
// to two seconds later. That is the window the leader epoch and the
// fence fingerprint guard (SV-52); they are not this package's.
//
// The lease is held until ctx ends, the store closes or the connection
// fails; lost is closed first, then the lock released.
func (s *Store) Lead(ctx context.Context, name string) (<-chan struct{}, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.closed() {
		return nil, store.ErrClosed
	}
	s.mu.Lock()
	sem, ok := s.leases[name]
	if !ok {
		sem = make(chan struct{}, 1)
		s.leases[name] = sem
	}
	s.mu.Unlock()
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.ctx.Done():
		return nil, store.ErrClosed
	}
	conn, err := s.acquire(ctx, name)
	if err != nil {
		<-sem
		return nil, err
	}

	lost := make(chan struct{})
	s.running.Add(1)
	go func() {
		defer s.running.Done()
		defer func() { <-sem }()
		tick := time.NewTicker(leasePing)
		defer tick.Stop()
	hold:
		for {
			select {
			case <-ctx.Done():
				break hold
			case <-s.ctx.Done():
				break hold
			case <-tick.C:
				pctx, cancel := context.WithTimeout(s.ctx, leasePingLimit)
				err := conn.Ping(pctx)
				cancel()
				if err != nil {
					break hold
				}
			}
		}
		close(lost)
		// Closing the session releases the lock. An explicit unlock
		// first lets the next holder in without waiting on a Close
		// that might hang on a dead connection.
		uctx, cancel := context.WithTimeout(context.Background(), time.Second)
		ns, key := lockKey(name)
		_, _ = conn.Exec(uctx, `SELECT pg_advisory_unlock($1, $2)`, ns, key)
		cancel()
		closeConn(conn)
	}()
	return lost, nil
}

// acquire tries the lease's lock, on a new connection, every leaseRetry
// until it holds it, ctx ends or the store closes. A failure to connect
// or to ask is retried the same way.
func (s *Store) acquire(ctx context.Context, name string) (*pgx.Conn, error) {
	ns, key := lockKey(name)
	var conn *pgx.Conn
	for {
		if conn == nil {
			conn, _ = s.dial(ctx, leaseParams)
		}
		if conn != nil {
			var got bool
			err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`, ns, key).Scan(&got)
			if err == nil && got {
				return conn, nil
			}
			if err != nil {
				closeConn(conn)
				conn = nil
			}
		}
		select {
		case <-ctx.Done():
		case <-s.ctx.Done():
		case <-time.After(leaseRetry):
			continue
		}
		if conn != nil {
			closeConn(conn)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, store.ErrClosed
	}
}

// leaseParams set the server's TCP keepalive on a lease connection, so
// Postgres notices a vanished holder only after the holder's own pings
// have told it so: no sooner than keepalive, then three probes two
// seconds apart.
var leaseParams = map[string]string{
	"tcp_keepalives_idle":     strconv.Itoa(int(keepalive / time.Second)),
	"tcp_keepalives_interval": "2",
	"tcp_keepalives_count":    "3",
}
