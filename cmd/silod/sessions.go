package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/datadir"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/harness"
	"github.com/bwsalmon/silo/pkg/leader"
)

// runSessiond is silod sessiond, the session launcher (package
// harness, Sessiond): the one process on a control VM with the
// container runtime's socket. It serves silod, and only silod's user,
// on --socket.
func runSessiond(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessiond", flag.ExitOnError)
	socket := fs.String("socket", "/run/silo/sessiond.sock", "serve silod on this Unix socket")
	owner := fs.Int("owner", -1, "the uid that alone may connect: silod's")
	image := fs.String("harness-image", "", "the harness image, the only one it runs")
	docker := fs.String("docker-socket", "/var/run/docker.sock", "the container runtime's socket")
	sockets := fs.String("session-dir", harness.DefaultLayout.SocketDir, "silod's --session-dir")
	state := fs.String("session-state-dir", harness.DefaultLayout.StateDir, "where each session's private directory is made")
	links := fs.String("link-dir", harness.DefaultLayout.LinkDir, "sessiond's own directory, on the same mount as --session-dir, for its links to the sessions' sockets")
	logins := fs.String("login-dir", harness.DefaultLayout.LoginDir, "sessiond's own directory, on a tmpfs, for subscription sessions' login files")
	fs.Parse(args)
	if *image == "" || *owner < 0 {
		return errors.New("give --harness-image and --owner")
	}
	d := &harness.Sessiond{Engine: &harness.Docker{Socket: *docker}, Image: *image, Owner: *owner,
		Layout: harness.Layout{SocketDir: *sockets, StateDir: *state, LinkDir: *links, LoginDir: *logins}}
	if err := d.Reset(ctx); err != nil {
		return fmt.Errorf("removing leftover session containers: %w", err)
	}
	l, err := harness.Listen(*socket, *owner, nil)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: d.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	log.Printf("sessiond: serving uid %d on %s, running %s", *owner, *socket, *image)
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// harnessInit is silod harness-init: a session container's entrypoint.
func harnessInit(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("harness-init", flag.ExitOnError)
	socket := fs.String("socket", harness.SocketPath, "the session socket")
	listen := fs.String("listen", strings.TrimPrefix(harness.Endpoint, "http://"), "the loopback address to forward from")
	login := fs.String("login", "", "a subscription session's read-only login directory, copied into the home before the command runs")
	fs.Parse(args)
	return harness.HarnessInit(ctx, *socket, *listen, *login, harness.Home, fs.Args())
}

// machineSize is this VM's vCPUs and memory in MiB, which its session
// slots are counted from.
func machineSize() (cpus, memMB int64) {
	cpus = int64(runtime.NumCPU())
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return cpus, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "MemTotal:"); ok {
			kb, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")), 10, 64)
			return cpus, kb / 1024
		}
	}
	return cpus, 0
}

// moveSessionsEvery is how often the leader looks for sessions on
// control VMs that are gone; a heartbeat is gone after 30s.
const moveSessionsEvery = 10 * time.Second

// moveSessionsDuty is the leader's duty of moving the sessions of a
// control VM that is gone to the others (agents.md, "silod restarts").
func moveSessionsDuty(api *fenceapi.Server) leader.Duty {
	return leader.Duty{Name: "move sessions", Every: moveSessionsEvery, Run: func(ctx context.Context) error {
		n, err := api.MoveSessions(ctx, leader.Guard)
		if n > 0 {
			log.Printf("moved %d agent sessions off control VMs that are gone", n)
		}
		if err != nil {
			return fmt.Errorf("moving sessions: %w", err)
		}
		return nil
	}}
}

// mcpStdio is silod mcp-stdio: an ambient session's MCP server, which
// its harness CLI starts (harness.MCPStdio).
func mcpStdio(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp-stdio", flag.ExitOnError)
	socket := fs.String("socket", "", "the session socket")
	fs.Parse(args)
	if *socket == "" {
		return errors.New("give --socket")
	}
	return harness.MCPStdio(ctx, *socket, os.Stdin, os.Stdout)
}

// ambientRunner is the local backend's harness Runner (local-backend.md
// §10): harnesses run on this host as silod's user, each turn in its
// session's systemd scope, reaching their sockets through silod
// mcp-stdio. It runs up to agentSessions sessions, or half the host's
// vCPUs (at least one). Unless they were given, the sockets are made
// under $XDG_RUNTIME_DIR/silo/sessions, and the sessions' working
// directories under the data directory's cache/sessions.
func ambientRunner(fs *flag.FlagSet, dd *datadir.Dir, host string, agentSessions int, sessionDir, stateDir *string) (*harness.Runner, error) {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if !set["session-dir"] {
		rt := os.Getenv("XDG_RUNTIME_DIR")
		if rt == "" {
			return nil, errors.New("--ambient-agents: give --session-dir, or set XDG_RUNTIME_DIR")
		}
		*sessionDir = filepath.Join(rt, "silo", "sessions")
	}
	if !set["session-state-dir"] {
		*stateDir = filepath.Join(dd.Cache(), "sessions")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if host == "" {
		host, _ = os.Hostname()
	}
	slots := agentSessions
	if slots <= 0 {
		slots = max(1, runtime.NumCPU()/2)
	}
	return &harness.Runner{Host: host, Slots: slots,
		Runtime: &harness.Scopes{StateDir: *stateDir},
		Layout:  harness.Layout{SocketDir: *sessionDir, StateDir: *stateDir},
		MCP: func(socket string) []string {
			return []string{exe, "mcp-stdio", "--socket", socket}
		}}, nil
}
