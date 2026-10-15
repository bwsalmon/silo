package ssh

import (
	"strconv"
	"strings"
)

// Transport builds the command lines that reach the agent host. OpenSSH
// is the only one in v2.0 (call 7); the vm location (stage 10) supplies
// one that runs `gcloud compute ssh --tunnel-through-iap` instead, and
// reuses everything else in this package.
type Transport interface {
	// Argv returns the command line that runs the transport with the
	// given ssh flags against the host and, when remote is not empty,
	// runs remote there (a command line for the remote login shell).
	Argv(flags []string, remote string) []string
	// Host names the host in messages.
	Host() string
}

// OpenSSH runs the system ssh binary, so ~/.ssh/config applies in full
// (design §5.3): aliases, ProxyJump, identity files, certificates.
type OpenSSH struct {
	// Program is the ssh binary; "ssh" when empty.
	Program string
	// Target is location.ssh.host: a Host alias or a hostname.
	Target string
	// User, Port and Identity are the Silofile's optional overrides,
	// passed as -l, -p and -i.
	User     string
	Port     int
	Identity string
	// Options are more flags passed before every other, such as -F for a
	// test's own ssh_config.
	Options []string
}

func (o *OpenSSH) Host() string { return o.Target }

func (o *OpenSSH) Argv(flags []string, remote string) []string {
	prog := o.Program
	if prog == "" {
		prog = "ssh"
	}
	argv := append([]string{prog}, o.Options...)
	// silo never adds a host key: an unknown host fails (design §5.3).
	// StrictHostKeyChecking=yes is the strictest setting there is, so it
	// cannot loosen a user's own.
	argv = append(argv, "-o", "StrictHostKeyChecking=yes")
	if o.User != "" {
		argv = append(argv, "-l", o.User)
	}
	if o.Port != 0 {
		argv = append(argv, "-p", strconv.Itoa(o.Port))
	}
	if o.Identity != "" {
		argv = append(argv, "-i", o.Identity)
	}
	argv = append(argv, flags...)
	argv = append(argv, o.Target)
	if remote != "" {
		argv = append(argv, "--", remote)
	}
	return argv
}

// Quote quotes s for a POSIX shell: single quotes, with each single quote
// spelled '\”.
func Quote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./=:,+@%") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// QuoteArgv quotes each argument and joins them with spaces.
func QuoteArgv(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = Quote(a)
	}
	return strings.Join(q, " ")
}
