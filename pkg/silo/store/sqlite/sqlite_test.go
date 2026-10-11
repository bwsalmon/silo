package sqlite

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, storetest.Backend{
		NewDB: func(t *testing.T) func() (store.Database, error) {
			path := filepath.Join(t.TempDir(), "silo.db")
			return func() (store.Database, error) { return Open(path) }
		},
	})
}

func openMigrated(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Migrate(context.Background(), s.Latest()); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestNowIsTheDatabases proves Tx.Now, and every stamp the store writes
// with it, come from SQL rather than from Go's clock: swap the SQL for
// a fixed value and that value is what comes back.
func TestNowIsTheDatabases(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "silo.db"))
	s.dialect.NowSQL = `SELECT 1234567890123`
	ctx := context.Background()
	var now time.Time
	err := s.Tx(ctx, func(tx store.Tx) error {
		var err error
		if now, err = tx.Now(ctx); err != nil {
			return err
		}
		_, err = tx.Append(ctx, "k", "k", "")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.Millis(now) != 1234567890123 {
		t.Errorf("Now = %v (%d), want the database's 1234567890123", now, store.Millis(now))
	}
	ch, err := s.Changes(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if c := <-ch; store.Millis(c.At) != 1234567890123 {
		t.Errorf("the change was stamped %v, not with the database's time", c.At)
	}
}

// TestOpenSettings pins WAL mode and BEGIN IMMEDIATE.
func TestOpenSettings(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "silo.db"))
	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q (%v), want wal", mode, err)
	}
	// An immediate transaction holds the write lock from BEGIN: a
	// second connection cannot begin one while it is open.
	ctx := context.Background()
	first, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout = 0`); err != nil {
		t.Fatal(err)
	}
	second, err := conn.BeginTx(ctx, nil)
	if err == nil {
		second.Rollback()
		t.Fatal("a second transaction began while the first held the write lock: BEGIN is not IMMEDIATE")
	}
	if !s.dialect.IsConflict(err) {
		t.Errorf("a busy database gave %v, which is not classed as a conflict to retry", err)
	}
}

// TestSecondProcessRefused runs this test binary again as a second
// process on the same file, and proves it is refused while the first
// holds the file and admitted once it has let go.
func TestSecondProcessRefused(t *testing.T) {
	if path := os.Getenv("SILO_SQLITE_SECOND_PROCESS"); path != "" {
		s, err := Open(path)
		if errors.Is(err, store.ErrInUse) {
			os.Exit(3)
		}
		if err != nil {
			os.Exit(4)
		}
		s.Close()
		os.Exit(0)
	}
	path := filepath.Join(t.TempDir(), "silo.db")
	second := func() int {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSecondProcessRefused$")
		cmd.Env = append(os.Environ(), "SILO_SQLITE_SECOND_PROCESS="+path)
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		if err != nil {
			t.Fatalf("running the second process: %v\n%s", err, out)
		}
		return 0
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if code := second(); code != 3 {
		t.Errorf("second process exited %d while the file was held, want 3 (refused with ErrInUse)", code)
	}
	s.Close()
	if code := second(); code != 0 {
		t.Errorf("second process exited %d after the first closed, want 0", code)
	}
}

func TestOpenEmptyPath(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Error("Open(\"\") succeeded")
	}
}

// TestLockRows: with one writer there is nothing to lock, so LockRows
// only checks the rows are there.
func TestLockRows(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "silo.db"))
	ctx := context.Background()
	err := s.Tx(ctx, func(tx store.Tx) error {
		if _, err := tx.Exec(ctx, `CREATE TABLE locktest_silos (id TEXT PRIMARY KEY)`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO locktest_silos (id) VALUES ('root'), ('a')`); err != nil {
			return err
		}
		return tx.LockRows(ctx, "locktest_silos", "id", "root", "a")
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx store.Tx) error { return tx.LockRows(ctx, "locktest_silos", "id", "root", "nope") }); err == nil {
		t.Error("LockRows found a row that is not there")
	}
	if err := s.Tx(ctx, func(tx store.Tx) error { return tx.LockRows(ctx, "Locktest_silos", "id", "root") }); err == nil {
		t.Error("LockRows accepted a table name that is not a plain identifier")
	}
}
