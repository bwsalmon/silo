package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bwsalmon/silo/old/mcp"
)

// This file is what remote MCP servers (design §13.2, stage 5b) need of
// the ssh location: a server silo starts on the agent host runs over the
// master as its own channel, whose stdin and stdout are the server's;
// one already running there is reached through a local forward on the
// master. Both are proxied into the gateway, never offered to the CLI.

// Host runs commands on the agent host for remote MCP servers: over the
// master during a session, or on a connection of its own for --dry-run.
type Host struct {
	name string
	r    runner
}

// Host is the agent host, reached over the master.
func (l *Location) Host() *Host { return &Host{name: l.host, r: l.mux} }

// DirectHost is the agent host reached on its own connection, with
// BatchMode so that nothing prompts: --dry-run's, which has no master.
func DirectHost(t Transport) *Host { return &Host{name: t.Host(), r: direct(t, true)} }

// Name names the host in messages.
func (h *Host) Name() string { return h.name }

// Environ is the remote login environment: what a command run over ssh
// starts with, before silo filters it (design §13.3).
func (h *Host) Environ(ctx context.Context) ([]string, error) {
	res, err := h.r(ctx, "env", nil)
	if err != nil {
		return nil, err
	}
	if res.Code != 0 {
		return nil, res.err("reading the login environment on " + h.name)
	}
	return parseEnv(res.Stdout), nil
}

var envLine = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// parseEnv reads env's output. A line that does not start NAME= carries
// on the value before it, which held a newline.
func parseEnv(out string) []string {
	var env []string
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		switch {
		case envLine.MatchString(line):
			env = append(env, line)
		case len(env) > 0:
			env[len(env)-1] += "\n" + line
		}
	}
	return env
}

// ReadFile reads a file on the host. A file that is not there is an
// error wrapping os.ErrNotExist.
func (h *Host) ReadFile(ctx context.Context, p string) ([]byte, error) {
	q := Quote(p)
	res, err := h.r(ctx, "if [ -e "+q+" ]; then exec cat -- "+q+"; else exit 3; fi", nil)
	if err != nil {
		return nil, err
	}
	switch res.Code {
	case 0:
		return []byte(res.Stdout), nil
	case 3:
		return nil, fmt.Errorf("%s on %s: %w", p, h.name, os.ErrNotExist)
	}
	return nil, res.err("reading " + p + " on " + h.name)
}

// LookPath finds a command on the host as a server started there with
// PATH path would: on that PATH, or, for a name with a slash, relative
// to the remote home directory, where servers run.
func (h *Host) LookPath(ctx context.Context, name, path string) (string, error) {
	script := `if case "$1" in */*) true ;; *) false ;; esac; then [ -f "$1" ] && [ -x "$1" ] && printf '%s' "$1"; else command -v "$1"; fi`
	res, err := h.r(ctx, "env -i PATH="+Quote(path)+" /bin/sh -c "+Quote(script)+" sh "+Quote(name), nil)
	if err != nil {
		return "", err
	}
	if out := strings.TrimSpace(res.Stdout); res.Code == 0 && out != "" {
		return out, nil
	}
	if strings.Contains(name, "/") {
		return "", fmt.Errorf("%s is not an executable file on %s", name, h.name)
	}
	return "", fmt.Errorf("%s is not found on %s on the server's PATH (%s)", name, h.name, path)
}

// Gone reports a dropped master connection: every remote server's tools
// fail with a tool error naming it (design §13.4).
func (l *Location) Gone() error {
	if l.MasterGone() {
		return fmt.Errorf("the connection to %s is gone", l.host)
	}
	return nil
}

// RemoteServer is how the gateway's proxy starts and stops an MCP server
// on the host: Argv is a local ssh whose stdin and stdout are the
// server's, Prelaunch writes the server's env file before each launch,
// and Reap, once the channel is closed, gives the server the grace to
// go and then kills its process group.
type RemoteServer struct {
	Argv      []string
	Prelaunch func(ctx context.Context) error
	Reap      func(ctx context.Context, grace time.Duration)
}

var serverName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// LaunchMCP is how to run argv on the host as the MCP server name, with
// env as its whole environment. Like the agent's (§5.3), the environment
// goes in a 0600 file in the remote session directory, which the
// command's preamble sources and deletes, so no value is in a remote
// argv; and env -i keeps the rest of the login environment out. The
// server's pid, which sshd made the leader of its own session and
// process group, is recorded beside it for Reap.
func (l *Location) LaunchMCP(name string, argv, env []string) (RemoteServer, error) {
	if l.rdir == "" {
		return RemoteServer{}, errors.New("ssh: LaunchMCP before Prepare")
	}
	if len(argv) == 0 {
		return RemoteServer{}, errors.New("ssh: empty command")
	}
	if !serverName.MatchString(name) {
		return RemoteServer{}, fmt.Errorf("ssh: %q cannot name a file", name)
	}
	var body bytes.Buffer
	// PATH first, so the preamble's rm is found.
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			fmt.Fprintf(&body, "export PATH=%s\n", Quote(kv[5:]))
		}
	}
	if body.Len() == 0 {
		body.WriteString("export PATH=/usr/bin:/bin\n")
	}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if !envName.MatchString(k) {
			return RemoteServer{}, fmt.Errorf("ssh: %q is not an environment variable name", k)
		}
		if k != "PATH" {
			fmt.Fprintf(&body, "export %s=%s\n", k, Quote(v))
		}
	}
	envFile := l.rdir + "/mcp-" + name + ".env"
	pidFile := l.rdir + "/mcp-" + name + ".pid"
	inner := `. "$0"; rm -f -- "$0"; exec "$@"`
	script := "echo $$ > " + Quote(pidFile) + " && exec env -i /bin/sh -c " + Quote(inner) + " " + Quote(envFile) + " " + QuoteArgv(argv)
	data := body.Bytes()
	return RemoteServer{
		Argv: l.t.Argv(append(l.muxFlags(), "-T"), "exec /bin/sh -c "+Quote(script)),
		Prelaunch: func(ctx context.Context) error {
			res, err := l.mux(ctx, "umask 077 && cat > "+Quote(envFile), data)
			if err != nil {
				return err
			}
			if res.Code != 0 {
				return res.err("writing the environment of MCP server " + name + " on " + l.host)
			}
			return nil
		},
		Reap: func(ctx context.Context, grace time.Duration) {
			if l.MasterGone() {
				return
			}
			n := int((grace + time.Second - 1) / time.Second)
			if n < 1 {
				n = 1
			}
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace+15*time.Second)
			defer cancel()
			reap := fmt.Sprintf(`p=$(cat %s 2>/dev/null); rm -f -- %s %s
[ -n "$p" ] || exit 0
i=0
while kill -s 0 -- "-$p" 2>/dev/null && [ "$i" -lt %d ]; do sleep 1; i=$((i+1)); done
kill -s KILL -- "-$p" 2>/dev/null
exit 0
`, Quote(pidFile), Quote(pidFile), Quote(envFile), n)
			_, _ = l.mux(ctx, "sh -s", []byte(reap))
		},
	}, nil
}

// ForwardMCP adds a local forward on the master to the host and port of
// rawURL as the agent host sees them (design §13.2), and returns an HTTP
// client that reaches them through it. The forward's local end is a Unix
// socket beside the control socket, which only this user can reach,
// rather than a loopback port any local user could; the URL, its Host
// header and TLS are left naming the server itself.
func (l *Location) ForwardMCP(ctx context.Context, name, rawURL string) (*http.Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	if host == "" || port == "" {
		return nil, fmt.Errorf("ssh: %s names no host and port to forward to", rawURL)
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if !serverName.MatchString(name) {
		return nil, fmt.Errorf("ssh: %q cannot name a file", name)
	}
	sock := filepath.Join(filepath.Dir(l.ctl), "mcp-"+name+".sock")
	if len(sock) >= 100 {
		d, err := os.MkdirTemp("", "silo-mcp-")
		if err != nil {
			return nil, err
		}
		l.mu.Lock()
		l.sockDirs = append(l.sockDirs, d)
		l.mu.Unlock()
		sock = filepath.Join(d, "s")
	}
	_ = os.Remove(sock)
	if err := l.ForwardLocal(ctx, sock+":"+host+":"+port); err != nil {
		return nil, err
	}
	return mcp.UnixClient(sock), nil
}
