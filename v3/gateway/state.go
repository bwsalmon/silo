package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Agent states (design §8.2).
const (
	// StateRegistered: recorded, with an unused registration token; no
	// `silo connect` has dialled in yet.
	StateRegistered = "registered"
	// StateConnected: a `silo connect` wrapper holds the agent's session.
	StateConnected = "connected"
	// StateDisconnected: the wrapper went away (it said so, or stopped
	// sending heartbeats). A standing agent is re-armed with a fresh token.
	StateDisconnected = "disconnected"
	// StateStopped: stopped on request, or with an ancestor. Final.
	StateStopped = "stopped"
)

// Location says where an agent runs. It is the registrar's claim, kept for
// the human and the UI; the gateway does not act on it.
type Location struct {
	// Kind is one of systemd, container, microvm, subprocess, remote, other.
	Kind string `json:"kind"`
	// Detail is free text: a unit name, an image, a VM name, a host.
	Detail string `json:"detail,omitempty"`
}

// Agent is the gateway's record of one agent incarnation.
type Agent struct {
	ID string `json:"id"`
	// Root marks the root agent (design §7); only the gateway seeds it.
	Root bool `json:"root,omitempty"`
	// Standing agents are seeded by the gateway (root and the default user
	// agent) and are re-armed with a fresh token whenever they disconnect.
	Standing bool `json:"standing,omitempty"`
	// User is the Unix user the agent runs as, if it runs on this host.
	User     string   `json:"user,omitempty"`
	Parent   string   `json:"parent,omitempty"` // "" for agents registered by the human
	Location Location `json:"location"`
	// Runtime names the coding-agent CLI, e.g. "claude" or "gemini".
	Runtime     string    `json:"runtime,omitempty"`
	Brief       string    `json:"brief,omitempty"`
	Limits      Limits    `json:"limits"`
	BudgetUSD   float64   `json:"budget_usd,omitempty"`
	MayRegister bool      `json:"may_register"`
	StopCmd     []string  `json:"stop_cmd,omitempty"`
	State       string    `json:"state"`
	Created     time.Time `json:"created"`
	// RegisteredBy is the audit actor that registered the agent.
	RegisteredBy string `json:"registered_by"`

	ConnectedAt *time.Time `json:"connected_at,omitempty"`
	LastSeen    *time.Time `json:"last_seen,omitempty"`
	// Peer is what the connecting wrapper reported about itself.
	Peer *Peer `json:"peer,omitempty"`
	// StopMode is set once the agent is stopped: the wrapper reads it on its
	// next heartbeat and signals its CLI's process group accordingly.
	StopMode  string     `json:"stop_mode,omitempty"`
	Tombstone *Tombstone `json:"tombstone,omitempty"`

	// TokenHash and SessionHash are hex sha256 of the registration and
	// session tokens. The tokens themselves are never stored.
	TokenHash   string `json:"token_hash,omitempty"`
	SessionHash string `json:"session_hash,omitempty"`

	// Inbox holds messages from the agent's parent (or the human) that the
	// wrapper has not collected yet, oldest first.
	Inbox   []Message `json:"inbox,omitempty"`
	NextMsg int64     `json:"next_msg,omitempty"`
}

// Peer describes a connected `silo connect` wrapper.
type Peer struct {
	Host string `json:"host,omitempty"`
	PID  int    `json:"pid,omitempty"`
	// UID is the socket peer's uid, when it dialled the local socket.
	UID int `json:"uid"`
}

// Tombstone is what is left of an agent that is no longer active.
type Tombstone struct {
	Time time.Time `json:"time"`
	// Cause says why it ended: "stopped by <actor>", "parent <id> stopped".
	Cause    string   `json:"cause"`
	Children []string `json:"children,omitempty"`
}

// Message is one parent → child message (design §8.7).
type Message struct {
	Seq int64 `json:"seq"`
	// FromAgent is the sending agent, "" for the human.
	FromAgent string    `json:"from_agent,omitempty"`
	From      string    `json:"from"` // the audit actor
	Text      string    `json:"text"`
	Time      time.Time `json:"time"`
}

// Page is a link in the UI (design §6.5): an agent's service on a port,
// reverse-proxied at /p/<slug>/, or an external http(s) URL.
type Page struct {
	Slug  string `json:"slug"`
	Title string `json:"title"`
	// Owner is the registering agent, "" for the human.
	Owner string `json:"owner,omitempty"`
	// Port and Host are an internal target; URL an external one. Exactly
	// one of Port and URL is set.
	Port       int    `json:"port,omitempty"`
	Host       string `json:"host,omitempty"`
	ExposePort int    `json:"expose_port,omitempty"`
	URL        string `json:"url,omitempty"`
	// Active is false once the owner has stopped; the UI greys it out.
	Active  bool      `json:"active"`
	Created time.Time `json:"created"`
}

// Secret request states.
const (
	SecretPending   = "pending"
	SecretStored    = "stored"
	SecretRejected  = "rejected"
	SecretCancelled = "cancelled" // the requesting agent stopped first
)

// SecretRequest is an agent asking the human for a secret (design §5.6).
// The value never passes through the gateway: the UI hands it to the
// agent's `silo connect`, which writes it to Path, and the bridge then
// records the outcome here.
type SecretRequest struct {
	ID       string     `json:"id"`
	Agent    string     `json:"agent"`
	Name     string     `json:"name"`
	Reason   string     `json:"reason"`
	Path     string     `json:"path"`
	State    string     `json:"state"`
	Created  time.Time  `json:"created"`
	Resolved *time.Time `json:"resolved,omitempty"`
	// Note is the human's reason for a rejection.
	Note string `json:"note,omitempty"`
}

// State is everything the gateway persists. It is small, so it is kept as
// one JSON document rewritten atomically on every change.
type State struct {
	Version int               `json:"version"`
	Agents  map[string]*Agent `json:"agents"`
	// History holds earlier incarnations whose id has been reused.
	History    []*Agent         `json:"history,omitempty"`
	Pages      map[string]*Page `json:"pages"`
	Secrets    []*SecretRequest `json:"secrets,omitempty"`
	NextSecret int64            `json:"next_secret,omitempty"`
}

// stateVersion 2 replaced the spawned-unit records of version 1 with
// registered agents. Version 1 state is refused rather than migrated: M1a
// had no users to migrate.
const stateVersion = 2

func newState() *State {
	return &State{Version: stateVersion, Agents: map[string]*Agent{}, Pages: map[string]*Page{}}
}

// Store persists State in a directory.
type Store struct{ Dir string }

func (s Store) path() string { return filepath.Join(s.Dir, "state.json") }

// Load reads the state, or returns an empty one if there is none yet.
func (s Store) Load() (*State, error) {
	data, err := os.ReadFile(s.path())
	if errors.Is(err, fs.ErrNotExist) {
		return newState(), nil
	}
	if err != nil {
		return nil, err
	}
	st := newState()
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path(), err)
	}
	if st.Version != stateVersion {
		return nil, fmt.Errorf("%s: state version %d, want %d", s.path(), st.Version, stateVersion)
	}
	if st.Agents == nil {
		st.Agents = map[string]*Agent{}
	}
	if st.Pages == nil {
		st.Pages = map[string]*Page{}
	}
	return st, nil
}

// Save writes the state atomically.
func (s Store) Save(st *State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.Dir, "state.json", data, 0o600)
}

// writeFileAtomic writes dir/name: temp file, fsync, rename, fsync dir.
func writeFileAtomic(dir, name string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
