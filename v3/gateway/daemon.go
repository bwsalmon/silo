package gateway

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"

	"github.com/bwsalmon/silo/v3/gateway/approval"
)

// Listen creates the API socket at path, mode 0660 and group gid (root's
// group if gid < 0), replacing a stale socket from an earlier run.
func Listen(path string, gid int) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if c, err := net.Dial("unix", path); err == nil {
			c.Close()
			return nil, fmt.Errorf("%s: another gateway is listening", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	l.SetUnlinkOnClose(true)
	if gid >= 0 {
		if err := os.Chown(path, 0, gid); err != nil {
			l.Close()
			return nil, err
		}
	}
	if err := os.Chmod(path, 0o660); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// Notify sends a state string to systemd if the daemon runs under
// Type=notify, and does nothing otherwise.
func Notify(state string) error {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return nil
	}
	if addr[0] == '@' {
		addr = "\x00" + addr[1:]
	}
	c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Write([]byte(state))
	return err
}

// Main runs the gateway daemon until ctx is done. It must run as root.
func Main(ctx context.Context, configPath string) error {
	if os.Geteuid() != 0 {
		return errors.New("silo gateway must run as root")
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(cfg.StateDir, 0o700); err != nil {
		return err
	}
	audit, err := OpenAuditLog(cfg.AuditLog, SystemClock{}, true)
	if err != nil {
		return err
	}
	defer audit.Close()
	g, err := New(cfg, Deps{
		Clock:     SystemClock{},
		Audit:     audit,
		Store:     Store{Dir: cfg.StateDir},
		LookupUID: lookupUID,
	})
	if err != nil {
		return err
	}
	gid, err := NewGroups().EnsureGroup(cfg.SocketGroup)
	if err != nil {
		return fmt.Errorf("socket group %s: %w", cfg.SocketGroup, err)
	}
	if err := g.Start(ctx); err != nil {
		return err
	}
	l, err := Listen(cfg.SocketPath, gid)
	if err != nil {
		return err
	}
	approvals, err := approval.New(cfg.RunAsRoot, approval.Deps{
		Dir: cfg.StateDir, RootAgent: RootAgentID, Logf: log.Printf,
		Audit: func(actor, event string, payload any) {
			if _, err := audit.Append(actor, event, payload); err != nil {
				log.Printf("AUDIT WRITE FAILED: %s %s: %v", actor, event, err)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("run_as_root: %w", err)
	}
	srv := &Server{G: g, Approvals: approvals}
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	go g.Run(ctx)
	if err := Notify("READY=1"); err != nil {
		log.Printf("sd_notify: %v", err)
	}
	log.Printf("silo gateway listening on %s", cfg.SocketPath)
	// Agents run outside the gateway, so they keep running when it exits;
	// their wrappers reconnect with the tokens it re-arms on start.
	return srv.Serve(ctx, l)
}

// lookupUID returns the uid of user name, or -1 if there is no such user.
func lookupUID(name string) (int, error) {
	u, err := user.Lookup(name)
	var unknown user.UnknownUserError
	if errors.As(err, &unknown) {
		return -1, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(u.Uid)
}
