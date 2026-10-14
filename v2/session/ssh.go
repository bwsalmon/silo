package session

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	sshloc "github.com/bwsalmon/silo/v2/location/ssh"
	"github.com/bwsalmon/silo/v2/silofile"
)

// SSHTransport is location.ssh's OpenSSH transport (design §5.3).
func SSHTransport(c *silofile.SSH, getenv func(string) string) *sshloc.OpenSSH {
	id := c.Identity
	if strings.HasPrefix(id, "~/") {
		if home := getenv("HOME"); home != "" {
			id = filepath.Join(home, id[2:])
		}
	}
	return &sshloc.OpenSSH{Target: c.Host, User: c.User, Port: c.Port, Identity: id}
}

// SSHLocation is the ssh location for location.ssh, which at startup
// removes the host's stale remote session directories (design §5.3).
func SSHLocation(c *silofile.SSH, getenv func(string) string, stderr io.Writer) *sshloc.Location {
	return sshloc.New(sshloc.Config{
		Transport:        SSHTransport(c, getenv),
		GatewayTransport: c.GatewayTransport,
		GC:               sshloc.GCOptions{Live: Live(getenv)},
		Stderr:           stderr,
	})
}

// Live reports whether a session on this machine is still running: its
// session directory exists, and its silo run is alive (or it is too new
// to have written session.json). silo gc --host removes the remote
// directories of sessions that are not.
func Live(getenv func(string) string) func(id string) bool {
	if getenv == nil {
		getenv = os.Getenv
	}
	root := RuntimeRoot(getenv)
	return func(id string) bool {
		if !ValidID(id) {
			return false
		}
		dir := filepath.Join(root, id)
		rec, err := ReadRecord(dir)
		if err == nil {
			return alive(rec.PID)
		}
		fi, serr := os.Stat(dir)
		if serr != nil {
			return !errors.Is(serr, os.ErrNotExist)
		}
		return time.Since(fi.ModTime()) < time.Minute
	}
}
