package session

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bwsalmon/silo/v2/agentcli"
	"github.com/bwsalmon/silo/v2/location"
	sshloc "github.com/bwsalmon/silo/v2/location/ssh"
	"github.com/bwsalmon/silo/v2/silofile"
)

func init() { RegisterCheck("ssh", checkSSH) }

// checkSSH is --dry-run's part for location kind ssh (design §2.4): the
// host is reachable without prompting, its host key is accepted, and the
// CLI and its login are there. It changes nothing on the host.
func checkSSH(ctx context.Context, f *silofile.Silofile, environ []string) []CheckResult {
	if f.Location.Kind != "ssh" || f.Location.SSH == nil {
		return nil
	}
	ls := &location.Session{CLI: f.Agent.CLI, Binary: f.Agent.Binary, AmbientAuth: f.Agent.Auth.Ambient()}
	if m, err := agentcli.Lookup(f.Agent.CLI); err == nil {
		ls.VersionArgs = m.VersionArgs()
	}
	getenv := func(k string) string { return agentcli.Getenv(environ, k) }
	var out []CheckResult
	for _, c := range sshloc.Check(ctx, SSHTransport(f.Location.SSH, getenv), ls) {
		out = append(out, CheckResult{Name: c.Name, OK: c.OK, Detail: c.Detail})
	}
	return out
}

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
