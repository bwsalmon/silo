package ssh

import (
	"bufio"
	"context"
	"fmt"
	"strings"

	"github.com/bwsalmon/silo/v2/location"
)

// login is how the probe checks a CLI's own login on the agent host
// (design §5.3; cli-mapping.md §2), and what the user runs there to log
// in. silo never logs a CLI in itself (call 8).
type login struct {
	// Args are run with the binary; exit 0 means logged in.
	Args []string
	// File, instead, is a credential file under $HOME whose presence
	// means logged in.
	File string
	// Fix is the command that logs in.
	Fix string
}

var logins = map[string]login{
	"claude": {Args: []string{"auth", "status"}, Fix: "claude auth login"},
	"codex":  {Args: []string{"login", "status"}, Fix: "codex login"},
	"agy":    {File: ".gemini/oauth_creds.json", Fix: "agy (and sign in)"},
}

// Probe is what the probe script learned about the agent host.
type Probe struct {
	OS, Arch, Home string
	// Binary is the CLI's path, empty when it was not found.
	Binary string
	// VersionExit and Version are `<binary> --version`'s exit code and
	// first line.
	VersionExit int
	Version     string
	// LoginChecked is set when the probe checked the login; LoggedIn is
	// its answer.
	LoginChecked, LoggedIn bool
}

// probeScript is sent on stdin to `sh -s`, so nothing is installed on
// the host. It prints key=value lines.
func probeScript(s *location.Session) string {
	name := s.Binary
	if name == "" {
		name = s.CLI
	}
	var b strings.Builder
	b.WriteString(`echo "os=$(uname -s)"
echo "arch=$(uname -m)"
echo "home=$HOME"
`)
	fmt.Fprintf(&b, "p=$(command -v -- %s 2>/dev/null) || p=\n", Quote(name))
	b.WriteString(`case $p in /*) [ -x "$p" ] && [ ! -d "$p" ] || p= ;; *) p= ;; esac
echo "binary=$p"
[ -n "$p" ] || exit 0
`)
	if len(s.VersionArgs) > 0 {
		fmt.Fprintf(&b, "v=$(\"$p\" %s </dev/null 2>&1); echo \"versionexit=$?\"\n", QuoteArgv(s.VersionArgs))
		b.WriteString(`printf 'version=%s\n' "$(printf '%s\n' "$v" | head -n 1)"` + "\n")
	}
	if l, ok := logins[s.CLI]; ok && s.AmbientAuth {
		if l.File != "" {
			fmt.Fprintf(&b, "[ -f \"$HOME\"/%s ]; echo \"login=$?\"\n", Quote(l.File))
		} else {
			fmt.Fprintf(&b, "\"$p\" %s </dev/null >/dev/null 2>&1; echo \"login=$?\"\n", QuoteArgv(l.Args))
		}
	}
	return b.String()
}

func parseProbe(out string) Probe {
	var p Probe
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch k {
		case "os":
			p.OS = strings.ToLower(v)
		case "arch":
			p.Arch = goArch(v)
		case "home":
			p.Home = v
		case "binary":
			p.Binary = v
		case "versionexit":
			fmt.Sscan(v, &p.VersionExit)
		case "version":
			p.Version = v
		case "login":
			p.LoginChecked, p.LoggedIn = true, v == "0"
		}
	}
	return p
}

func goArch(m string) string {
	switch m {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	case "i386", "i686":
		return "386"
	}
	return m
}

// probe runs the probe script and turns what it found into the facts
// Prepare returns, or a *location.PrerequisiteError naming what is
// missing and the command that fixes it on the host.
func probe(ctx context.Context, r runner, host string, s *location.Session) (location.HostInfo, error) {
	res, err := r(ctx, "sh -s", []byte(probeScript(s)))
	if err != nil {
		return location.HostInfo{}, err
	}
	if res.Code != 0 {
		return location.HostInfo{}, &location.PrerequisiteError{What: res.err("the probe on " + host).Error(), Fix: "check that " + host + " has a POSIX sh"}
	}
	p := parseProbe(res.Stdout)
	info := location.HostInfo{OS: p.OS, Arch: p.Arch, Home: p.Home, Binary: p.Binary, Version: p.Version}
	return info, p.check(host, s)
}

func (p Probe) check(host string, s *location.Session) error {
	if p.Binary == "" {
		if s.Binary != "" {
			return &location.PrerequisiteError{
				What: fmt.Sprintf("agent.binary %s is not an executable file on %s", s.Binary, host),
				Fix:  "check agent.binary; silo never installs a CLI",
			}
		}
		return &location.PrerequisiteError{
			What: fmt.Sprintf("the agent CLI %s was not found on %s's PATH", s.CLI, host),
			Fix:  fmt.Sprintf("install %s on %s (silo never installs a CLI), or set agent.binary to its absolute path there", s.CLI, host),
		}
	}
	if len(s.VersionArgs) > 0 && p.VersionExit != 0 {
		return &location.PrerequisiteError{
			What: fmt.Sprintf("%s %s on %s exited %d: %s", p.Binary, strings.Join(s.VersionArgs, " "), host, p.VersionExit, p.Version),
			Fix:  "check that the CLI is installed and runs on " + host,
		}
	}
	if p.LoginChecked && !p.LoggedIn {
		return &location.PrerequisiteError{
			What: fmt.Sprintf("%s is not logged in on %s", s.CLI, host),
			Fix:  fmt.Sprintf("log in on the host (silo never logs a CLI in): ssh -t %s %s", host, logins[s.CLI].Fix),
		}
	}
	return nil
}
