package harness

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// The isolation of a session's container (agents.md, "Isolation on
// the control VM", SV-118; call 50). NewSpec builds a turn's spec with
// all of it, and Validate, which sessiond runs on every spec silod
// hands it before it touches the runtime, refuses any spec without it.
const (
	// UIDBase is the first session user id. Session n of a control VM
	// (0 <= n < MaxSessions) runs as UIDBase+n, its own and nobody
	// else's: not root, not silod's (2001) or the gateway's (2002).
	UIDBase = 30000
	// MaxSessions is the most sessions one control VM runs, and so the
	// size of the uid range.
	MaxSessions = 1000
	// DefaultMemoryBytes is a session's memory limit (agents.md,
	// "Sizing": 1 GB by default), with no swap above it.
	DefaultMemoryBytes = 1 << 30
	// DefaultCPUShares is a session's CPU weight: a fifth of the
	// runtime's default 1024, as a session is budgeted 0.2 vCPU.
	DefaultCPUShares = 205
	// DefaultPids is a session's process count limit: a CLI on Node
	// and its children, not a fork bomb.
	DefaultPids = 256
	// TmpBytes is the size of the session's /tmp.
	TmpBytes = 64 << 20
)

// MetadataServer is the address an iptables rule in every session's
// network namespace drops, so that no session can fetch the control
// VM's service account token.
const MetadataServer = "169.254.169.254"

// MetadataServerV6 is GCE's metadata server's IPv6 address.
const MetadataServerV6 = "fd20:ce::254"

// Mount is one bind mount into a session's container.
type Mount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// Spec is what one turn of a session runs as. Its JSON is sessiond's
// request body.
type Spec struct {
	// Agent is the agent's id; the containers are named after it.
	Agent string `json:"agent"`
	// Image is the harness image; sessiond runs its own, and refuses
	// any other.
	Image   string   `json:"image,omitempty"`
	Argv    []string `json:"argv"`
	Env     []string `json:"env"`
	WorkDir string   `json:"work_dir"`
	// UID is the session's own user id, and its group id.
	UID int `json:"uid"`
	// CapDrop is ALL; CapAdd is empty.
	CapDrop []string `json:"cap_drop"`
	CapAdd  []string `json:"cap_add,omitempty"`
	// NoNewPrivileges sets no_new_privs, so no setuid binary or file
	// capability raises the harness's privileges.
	NoNewPrivileges bool `json:"no_new_privileges"`
	// Seccomp is the seccomp profile: "" for the runtime's default.
	// "unconfined" or a custom profile is refused.
	Seccomp string `json:"seccomp,omitempty"`
	// ReadOnlyRoot makes the image's filesystem read-only: the private
	// directory and a small /tmp are the only places it writes.
	ReadOnlyRoot bool `json:"read_only_root"`
	// Network is "none" for an API-key session, whose only way out is
	// its socket, and "egress" for a subscription session, whose
	// namespace's only route is sessiond's egress proxy, which lets it
	// reach Provider's hosts (EgressHosts) and nothing else.
	Network string `json:"network"`
	// Provider is a subscription session's provider: which hosts its
	// egress proxy allows. Empty for an API-key session.
	Provider string `json:"provider,omitempty"`
	// Login are a subscription session's login files, by path under
	// Home, as silod decrypted them (agents.md, §6, SV-108). sessiond
	// puts them in the session's login directory, mounted read-only at
	// LoginPath, and harness-init copies them into Home before the CLI
	// runs. After the turn sessiond returns the files at those paths
	// as the harness left them (Result.Login), for silod to write back
	// if the harness refreshed its login. Empty for an API-key session.
	Login map[string][]byte `json:"login,omitempty"`
	// DropMetadata is the iptables drop of the metadata server in the
	// session's network namespace, put there before the harness runs.
	DropMetadata bool `json:"drop_metadata"`
	// Mounts are the session socket, read-write so the harness can
	// connect, and the private directory, at Home; and for a
	// subscription session its login directory, read-only, at
	// LoginPath. Nothing else of silod's.
	Mounts []Mount `json:"mounts"`
	// TmpBytes is the size of the tmpfs at /tmp.
	TmpBytes    int64 `json:"tmp_bytes"`
	MemoryBytes int64 `json:"memory_bytes"`
	CPUShares   int64 `json:"cpu_shares"`
	Pids        int64 `json:"pids"`
	// StatePaths are the paths under the private directory sessiond
	// returns after the turn, for silod to save.
	StatePaths []string `json:"state_paths"`
	// Restore, if set, is a saved state (a tar.gz of StatePaths) that
	// sessiond puts in a fresh private directory before the turn.
	Restore []byte `json:"restore,omitempty"`
}

// LoginPath is where a subscription session's login directory is
// mounted, read-only.
const LoginPath = "/run/silo/login"

// Network modes of a session.
const (
	NetworkNone   = "none"
	NetworkEgress = "egress"
	// NetworkAmbient is an ambient session's, on the local backend's
	// host: the agent user's own network. sessiond refuses it.
	NetworkAmbient = "ambient"
)

// Layout is where a control VM keeps sessions' sockets, private
// directories and login files, as silod and sessiond both see them.
type Layout struct {
	// SocketDir holds /<agent>/silo.sock (package session's Dir).
	SocketDir string
	// StateDir holds each session's private directory, /<agent>.
	StateDir string
	// LinkDir is sessiond's own directory, root's, on the same mount
	// as SocketDir: it holds a hard link to each session's socket,
	// which is what a session's container mounts (Sessiond.socket).
	LinkDir string
	// LoginDir is sessiond's own directory, root's, on a tmpfs, that
	// holds each subscription session's login files, /<agent>: under
	// LinkDir, so that nothing of silod's is on the way to it.
	LoginDir string
}

// DefaultLayout is the control VM's.
var DefaultLayout = Layout{SocketDir: "/run/silo/sessions", StateDir: "/var/lib/silo/sessions", LinkDir: "/run/silo/sessiond", LoginDir: "/run/silo/sessiond/logins"}

// Login is agent's login directory.
func (l Layout) Login(agent string) string { return filepath.Join(l.LoginDir, agent) }

// Link is sessiond's hard link to agent's socket.
func (l Layout) Link(agent string) string { return filepath.Join(l.LinkDir, agent+".sock") }

// Socket is agent's session socket.
func (l Layout) Socket(agent string) string { return filepath.Join(l.SocketDir, agent, "silo.sock") }

// Private is agent's private directory.
func (l Layout) Private(agent string) string { return filepath.Join(l.StateDir, agent) }

// Limits are a session's resource limits; zero fields are the
// defaults.
type Limits struct {
	MemoryBytes, CPUShares, Pids int64
}

func (l Limits) orDefault() Limits {
	if l.MemoryBytes == 0 {
		l.MemoryBytes = DefaultMemoryBytes
	}
	if l.CPUShares == 0 {
		l.CPUShares = DefaultCPUShares
	}
	if l.Pids == 0 {
		l.Pids = DefaultPids
	}
	return l
}

// NewSpec is the spec of one turn of agent's session, in slot (which
// picks its uid), running cmd from adapter a.
func NewSpec(layout Layout, agent string, slot int, a Adapter, cmd Command, lim Limits) Spec {
	lim = lim.orDefault()
	return Spec{
		Agent:           agent,
		Argv:            append([]string{Init, "harness-init", "--socket", SocketPath, "--listen", strings.TrimPrefix(Endpoint, "http://"), "--"}, cmd.Argv...),
		Env:             cmd.Env,
		WorkDir:         WorkDir,
		UID:             UIDBase + slot,
		CapDrop:         []string{"ALL"},
		NoNewPrivileges: true,
		ReadOnlyRoot:    true,
		Network:         NetworkNone,
		DropMetadata:    true,
		Mounts: []Mount{
			{Source: layout.Socket(agent), Target: SocketPath},
			{Source: layout.Private(agent), Target: Home},
		},
		TmpBytes:    TmpBytes,
		MemoryBytes: lim.MemoryBytes,
		CPUShares:   lim.CPUShares,
		Pids:        lim.Pids,
		StatePaths:  a.StatePaths(),
	}
}

// Subscribe makes s a subscription session's: its login files in,
// read-only, and a namespace whose only route is the egress proxy for
// provider's hosts. harness-init copies the files into Home before the
// CLI runs (HarnessInit).
func (s *Spec) Subscribe(layout Layout, provider string, files map[string][]byte) {
	s.Network, s.Provider, s.Login = NetworkEgress, provider, files
	s.Mounts = append(s.Mounts, Mount{Source: layout.Login(s.Agent), Target: LoginPath, ReadOnly: true})
	// harness-init's own flags come before its "--".
	i := slices.Index(s.Argv, "--")
	s.Argv = slices.Insert(s.Argv, i, "--login", LoginPath)
}

// ValidLoginPath reports whether p can be a login file's path: relative,
// under the home, with no way out of it.
func ValidLoginPath(p string) bool {
	return p != "" && !filepath.IsAbs(p) && filepath.Clean(p) == p && p != "." && p != ".." && !strings.HasPrefix(p, "../")
}

// ValidAgent reports whether id can name a session's containers and
// directories: an agent id, lower-case letters and digits.
func ValidAgent(id string) bool {
	if id == "" || len(id) > 32 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// Validate refuses a spec that loosens any part of a session's
// isolation: sessiond runs it on every request, so that even silod
// can't have it start a container with more.
func (s *Spec) Validate(layout Layout) error {
	var errs []string
	bad := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	if !ValidAgent(s.Agent) {
		bad("agent %q is not an agent id", s.Agent)
	}
	if s.UID < UIDBase || s.UID >= UIDBase+MaxSessions {
		bad("uid %d is outside the session range %d-%d", s.UID, UIDBase, UIDBase+MaxSessions-1)
	}
	if len(s.CapDrop) != 1 || s.CapDrop[0] != "ALL" {
		bad("cap_drop must be [ALL], not %v", s.CapDrop)
	}
	if len(s.CapAdd) != 0 {
		bad("cap_add must be empty, not %v", s.CapAdd)
	}
	if !s.NoNewPrivileges {
		bad("no_new_privileges must be set")
	}
	if s.Seccomp != "" {
		bad("seccomp must be the runtime's default, not %q", s.Seccomp)
	}
	if !s.ReadOnlyRoot {
		bad("the root filesystem must be read-only")
	}
	if !s.DropMetadata {
		bad("the metadata server must be dropped")
	}
	want := map[string]string{SocketPath: layout.Socket(s.Agent), Home: layout.Private(s.Agent)}
	switch s.Network {
	case NetworkNone:
		if s.Provider != "" || len(s.Login) > 0 {
			bad("an API-key session (network none) gets no provider and no login files")
		}
	case NetworkEgress:
		if _, ok := EgressHosts[s.Provider]; !ok {
			bad("a subscription session needs a provider with egress hosts, not %q", s.Provider)
		}
		if len(s.Login) == 0 {
			bad("a subscription session needs its login files")
		}
		for p := range s.Login {
			if !ValidLoginPath(p) {
				bad("login file %q must be relative, under the home", p)
			}
		}
		want[LoginPath] = layout.Login(s.Agent)
	default:
		bad("network must be none or egress, not %q", s.Network)
	}
	if len(s.Mounts) != len(want) {
		bad("want exactly the socket, the private directory and (for a subscription) the login files mounted, not %d mounts", len(s.Mounts))
	}
	for _, m := range s.Mounts {
		if src, ok := want[m.Target]; !ok || m.Source != src {
			bad("mount %s at %s: only %s at %s, %s at %s, and a subscription's %s at %s", m.Source, m.Target,
				want[SocketPath], SocketPath, want[Home], Home, layout.Login(s.Agent), LoginPath)
		}
		if m.Target == LoginPath && !m.ReadOnly {
			bad("the login files must be mounted read-only")
		}
	}
	if s.MemoryBytes <= 0 || s.MemoryBytes > 8<<30 {
		bad("memory limit %d: want 1 byte to 8 GB", s.MemoryBytes)
	}
	if s.CPUShares <= 0 || s.CPUShares > 1024 {
		bad("cpu shares %d: want 1 to 1024", s.CPUShares)
	}
	if s.Pids <= 0 || s.Pids > 4096 {
		bad("process limit %d: want 1 to 4096", s.Pids)
	}
	if s.TmpBytes <= 0 || s.TmpBytes > 1<<30 {
		bad("/tmp size %d: want 1 byte to 1 GB", s.TmpBytes)
	}
	if len(s.Argv) == 0 || s.Argv[0] != Init {
		bad("the entrypoint must be %s harness-init", Init)
	}
	for _, p := range s.StatePaths {
		if c := filepath.Clean(p); c != p || filepath.IsAbs(p) || c == "." || strings.HasPrefix(c, "..") {
			bad("state path %q must be relative, under the private directory", p)
		}
	}
	if len(errs) > 0 {
		return errors.New("session spec refused: " + strings.Join(errs, "; "))
	}
	return nil
}

// ContainerName is the name of agent's harness container; NetName is
// that of the container that holds its network namespace, and
// FirewallName the one-shot container that puts the metadata rule in
// it.
func ContainerName(agent string) string { return "silo-session-" + agent }
func NetName(agent string) string       { return "silo-session-" + agent + "-net" }
func FirewallName(agent string) string  { return "silo-session-" + agent + "-fw" }

// LabelSession is the label every session container carries, with the
// agent's id: what silod's start removes.
const LabelSession = "silo-session"
