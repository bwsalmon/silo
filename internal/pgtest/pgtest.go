// Package pgtest starts throwaway PostgreSQL servers for silo's tests:
// one per test, initialised with initdb into the test's temporary
// directory and listening on a free port on 127.0.0.1, from the
// binaries of Debian's postgresql package. It came from Silo v1's
// pkg/internal/pgtest, which was itself grain's
// pkg/model/postgres/embedded.go; old/internal/pgtest is the retired
// runner's own copy.
//
// When the binaries are not installed the test is skipped, unless
// SILO_REQUIRE_POSTGRES is set, as the CI job that installs them does,
// in which case a server that cannot be started fails the test.
package pgtest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// RequireEnv names the environment variable that turns a missing
// PostgreSQL into a test failure rather than a skip.
const RequireEnv = "SILO_REQUIRE_POSTGRES"

// Required reports whether RequireEnv is set.
func Required() bool {
	v := os.Getenv(RequireEnv)
	return v != "" && v != "0" && v != "false"
}

// Server is a running throwaway server. It is stopped, and its files
// removed, when the test that started it ends.
type Server struct {
	// URL connects to its postgres database as the superuser postgres,
	// over TCP.
	URL  string
	Port int
}

// Start starts a server for t, or skips t when PostgreSQL is not
// installed (fails it, when RequireEnv is set).
func Start(t testing.TB) *Server {
	t.Helper()
	initdb, err := findBinary("initdb")
	if err == nil {
		_, err = findBinary("postgres")
	}
	if err != nil {
		if Required() {
			t.Fatalf("%v (%s is set, so postgres is required)", err, RequireEnv)
		}
		t.Skipf("%v; install Debian's postgresql package to run this test", err)
	}
	postgres, _ := findBinary("postgres")

	dir := filepath.Join(t.TempDir(), "pg")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := pgCommand(ctx, initdb, "-D", dir, "-A", "trust", "-U", "postgres", "--no-locale", "-E", "UTF8")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("initdb: %v\n%s", err, out)
	}

	// A port that was free when we looked can be taken before postgres
	// binds it, so a start that fails is retried on another.
	var lastErr error
	for range 3 {
		port, err := freePort()
		if err != nil {
			t.Fatal(err)
		}
		stop, err := start(ctx, postgres, dir, port)
		if err == nil {
			t.Cleanup(stop)
			return &Server{
				URL:  fmt.Sprintf("postgres://postgres@127.0.0.1:%d/postgres?sslmode=disable", port),
				Port: port,
			}
		}
		lastErr = err
	}
	t.Fatalf("starting postgres: %v", lastErr)
	return nil
}

// start runs postgres on port and waits for it to accept connections.
// Durability is traded for speed: the data directory is thrown away
// with the test.
func start(ctx context.Context, postgres, dir string, port int) (stop func(), err error) {
	cmd := pgCommand(context.Background(), postgres,
		// No Unix socket: a test's temporary directory can be longer
		// than a socket path may be, and the tests connect over TCP.
		"-D", dir, "-k", "", "-h", "127.0.0.1", "-p", strconv.Itoa(port),
		"-F", "-c", "full_page_writes=off", "-c", "synchronous_commit=off",
		"-c", "max_connections=200")
	logf, err := os.Create(filepath.Join(filepath.Dir(dir), "postgres.log"))
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		logf.Close()
		return nil, fmt.Errorf("starting postgres: %w", err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		logf.Close()
		close(exited)
	}()
	stop = func() {
		_ = cmd.Process.Signal(os.Interrupt) // fast shutdown
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
	}
	for {
		select {
		case <-exited:
			log, _ := os.ReadFile(logf.Name())
			return nil, fmt.Errorf("postgres exited before accepting connections:\n%s", log)
		case <-ctx.Done():
			stop()
			return nil, errors.New("timed out waiting for postgres to accept connections")
		case <-time.After(50 * time.Millisecond):
		}
		if ready(ctx, port) {
			return stop, nil
		}
	}
}

// ready asks pg_isready, when it is installed, whether the server takes
// connections; otherwise a TCP connect is the best it can do.
func ready(ctx context.Context, port int) bool {
	if isready, err := findBinary("pg_isready"); err == nil {
		return pgCommand(ctx, isready, "-q", "-h", "127.0.0.1", "-p", strconv.Itoa(port)).Run() == nil
	}
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 100*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// findBinary looks on PATH, then in Debian's versioned directories.
func findBinary(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	matches, _ := filepath.Glob("/usr/lib/postgresql/*/bin/" + name)
	for i := len(matches) - 1; i >= 0; i-- { // the newest version
		if fi, err := os.Stat(matches[i]); err == nil && fi.Mode()&0o111 != 0 {
			return matches[i], nil
		}
	}
	return "", fmt.Errorf("postgres binary %q not found", name)
}

// pgCommand runs a postgres binary with only the environment it needs,
// so the server never sees the test's credentials.
func pgCommand(ctx context.Context, path string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, path, args...)
	for _, name := range []string{"PATH", "HOME", "USER", "LOGNAME", "TMPDIR", "TZ", "LANG", "LC_ALL"} {
		if v, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+v)
		}
	}
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	return cmd
}

// Run runs one of PostgreSQL's client programs (pg_dump, pg_restore,
// psql) with args, failing t with its output if it fails. A missing
// program skips t, or fails it when RequireEnv is set, as Start does.
func Run(t testing.TB, program string, args ...string) {
	t.Helper()
	path, err := findBinary(program)
	if err != nil {
		if Required() {
			t.Fatalf("%v (%s is set, so postgres is required)", err, RequireEnv)
		}
		t.Skipf("%v; install Debian's postgresql package to run this test", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if out, err := pgCommand(ctx, path, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", program, args, err, out)
	}
}
