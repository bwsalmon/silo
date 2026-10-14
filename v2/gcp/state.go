package gcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// StateVersion is the version of accounts.json this silo writes.
const StateVersion = 1

// State is the user's local record of their accounts,
// $XDG_STATE_HOME/silo/accounts.json (design §11.1): the full spec,
// which the account's description only carries the hash of, and how
// far setup got.
type State struct {
	Version  int      `json:"version"`
	Accounts []Record `json:"accounts"`
}

// Record is one account in State.
type Record struct {
	Email    string `json:"email"`
	Project  string `json:"project"`
	User     string `json:"user"`
	Spec     Spec   `json:"spec"`
	UniqueID string `json:"unique_id,omitempty"`
	// TTL is how long the account may sit unused (design §11.5); 0
	// deletes it when its session ends.
	TTL Duration `json:"ttl"`
	// Incomplete is set from creation until every step has been done
	// (design §11.6); Done lists the steps that have.
	Incomplete bool     `json:"incomplete,omitempty"`
	Done       []string `json:"done,omitempty"`
	Created    time.Time `json:"created"`
	Used       time.Time `json:"used"`
	// Session is the session using the account, if any, so that gc
	// leaves an account in use alone.
	Session *SessionRef `json:"session,omitempty"`
}

// SessionRef names a session holding an account.
type SessionRef struct {
	ID  string `json:"id"`
	PID int    `json:"pid"`
}

// Duration is a time.Duration that is JSON as Go's duration string.
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	*d = Duration(v)
	return err
}

// Steps recorded in Record.Done.
const (
	stepCreate       = "create"
	stepBindPrefix   = "bind:"
	stepTokenCreator = "token-creator"
)

func (r *Record) done(step string) bool {
	for _, s := range r.Done {
		if s == step {
			return true
		}
	}
	return false
}

func (r *Record) mark(step string) {
	if !r.done(step) {
		r.Done = append(r.Done, step)
	}
}

// Store reads and writes accounts.json, under an exclusive lock so that
// two silo processes do not lose each other's records.
type Store struct {
	Path string
}

// StatePath is $XDG_STATE_HOME/silo/accounts.json, or
// ~/.local/state/silo/accounts.json.
func StatePath(getenv func(string) string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	d := getenv("XDG_STATE_HOME")
	if d == "" || !filepath.IsAbs(d) {
		home := getenv("HOME")
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		d = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(d, "silo", "accounts.json")
}

// Load reads the state without locking it.
func (s *Store) Load() (State, error) {
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return State{Version: StateVersion}, nil
	}
	if err != nil {
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, fmt.Errorf("%s: %w", s.Path, err)
	}
	if st.Version > StateVersion {
		return State{}, fmt.Errorf("%s is version %d: this silo reads version %d", s.Path, st.Version, StateVersion)
	}
	return st, nil
}

// Update runs fn on the state under the lock and writes what it leaves,
// atomically, unless fn fails.
func (s *Store) Update(fn func(*State) error) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck
	st, err := s.Load()
	if err != nil {
		return err
	}
	if err := fn(&st); err != nil {
		return err
	}
	st.Version = StateVersion
	sort.Slice(st.Accounts, func(i, j int) bool { return st.Accounts[i].Email < st.Accounts[j].Email })
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), ".accounts-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.Path)
}

// Get returns the record for email, if there is one.
func (s *Store) Get(email string) (Record, bool, error) {
	st, err := s.Load()
	if err != nil {
		return Record{}, false, err
	}
	for _, r := range st.Accounts {
		if r.Email == email {
			return r, true, nil
		}
	}
	return Record{}, false, nil
}

// Put replaces or adds r.
func (s *Store) Put(r Record) error {
	return s.Update(func(st *State) error {
		for i := range st.Accounts {
			if st.Accounts[i].Email == r.Email {
				st.Accounts[i] = r
				return nil
			}
		}
		st.Accounts = append(st.Accounts, r)
		return nil
	})
}

// Delete forgets email.
func (s *Store) Delete(email string) error {
	return s.Update(func(st *State) error {
		out := st.Accounts[:0]
		for _, r := range st.Accounts {
			if r.Email != email {
				out = append(out, r)
			}
		}
		st.Accounts = out
		return nil
	})
}
