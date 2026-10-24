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

// Agent states.
const (
	StateRunning = "running"
	StateStopped = "stopped" // stopped on request
	StateExited  = "exited"  // the unit ended on its own
)

// Agent is the gateway's record of one agent incarnation.
type Agent struct {
	ID       string    `json:"id"`
	Root     bool      `json:"root,omitempty"`
	User     string    `json:"user"`
	UID      int       `json:"uid"`
	Home     string    `json:"home"`
	CLI      string    `json:"cli"`
	Brief    string    `json:"brief,omitempty"`
	Parent   string    `json:"parent,omitempty"` // "" for top-level agents spawned by a human
	Unit     string    `json:"unit"`
	Slice    string    `json:"slice"`
	Limits   Limits    `json:"limits"`
	Lifetime Lifetime  `json:"lifetime"`
	State    string    `json:"state"`
	Created  time.Time `json:"created"`
	// SpawnedBy is the audit actor that asked for the spawn.
	SpawnedBy string     `json:"spawned_by"`
	Tombstone *Tombstone `json:"tombstone,omitempty"`
}

// Tombstone is what is left of an agent that is no longer running.
type Tombstone struct {
	Time time.Time `json:"time"`
	// LastState is the unit's state when the gateway last saw it, as
	// active_state/sub_state, or "gone".
	LastState  string `json:"last_state"`
	Result     string `json:"result,omitempty"`
	ExitStatus int    `json:"exit_status,omitempty"`
	// Cause says why it stopped: "stopped by <actor>", "parent <id>
	// stopped", "exited", ….
	Cause    string   `json:"cause"`
	Children []string `json:"children,omitempty"`
}

// State is everything the gateway persists. It is small, so it is kept as
// one JSON document rewritten atomically on every change.
type State struct {
	Version int               `json:"version"`
	Agents  map[string]*Agent `json:"agents"`
	// History holds earlier incarnations whose id has been reused.
	History []*Agent `json:"history,omitempty"`
	// Users are the Unix users silo created, by name, with their uid.
	Users map[string]int `json:"users"`
}

const stateVersion = 1

func newState() *State {
	return &State{Version: stateVersion, Agents: map[string]*Agent{}, Users: map[string]int{}}
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
	if st.Users == nil {
		st.Users = map[string]int{}
	}
	return st, nil
}

// Save writes the state atomically: temp file, fsync, rename, fsync dir.
func (s Store) Save(st *State) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, ".state-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
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
	if err := os.Rename(tmp.Name(), s.path()); err != nil {
		return err
	}
	d, err := os.Open(s.Dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
