package session

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// RecordFile is the file in the session directory describing what was
// set up, so a crash can be undone (design §3, step 1).
const RecordFile = "session.json"

// NewID returns a session id: s- plus 12 base32 characters.
func NewID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "s-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))[:12]
}

// ValidID reports whether id is a session id.
func ValidID(id string) bool {
	if len(id) != 14 || !strings.HasPrefix(id, "s-") {
		return false
	}
	for _, c := range id[2:] {
		if !(c >= 'a' && c <= 'z' || c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}

// RuntimeRoot is where session directories live: $XDG_RUNTIME_DIR/silo,
// or, where XDG_RUNTIME_DIR is unset, silo-<uid> in the temp directory.
func RuntimeRoot(getenv func(string) string) string {
	if d := getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "silo")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("silo-%d", os.Getuid()))
}

// StateRoot is where audit logs are kept after a session:
// $XDG_STATE_HOME/silo/sessions, or ~/.local/state/silo/sessions
// (design §4.2, call 4).
func StateRoot(getenv func(string) string) string {
	d := getenv("XDG_STATE_HOME")
	if d == "" {
		home := getenv("HOME")
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		d = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(d, "silo", "sessions")
}

// Record is session.json.
type Record struct {
	ID      string    `json:"id"`
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
	// Silofile is the Silofile's absolute path.
	Silofile    string `json:"silofile,omitempty"`
	Name        string `json:"name"`
	CLI         string `json:"cli"`
	CLIBinary   string `json:"cli_binary,omitempty"`
	CLIVersion  string `json:"cli_version,omitempty"`
	Location    string `json:"location"`
	Mode        string `json:"mode"`
	Permissions string `json:"remote_permissions"`
	// StateDir holds the audit log, which outlives the session.
	StateDir string `json:"state_dir"`
	// Events is the session's events.jsonl, in StateDir.
	Events string `json:"events,omitempty"`
	// Labels are the caller's (silo run --label), for silo ls.
	Labels map[string]string `json:"labels,omitempty"`
	// Detached: the session runs under its own supervisor, whose PID is
	// PID (silo run --detach).
	Detached bool `json:"detached,omitempty"`
	// Resources is everything set up outside the session directory, in
	// the order it was set up.
	Resources []Resource `json:"resources"`
	Result    *Result    `json:"result,omitempty"`
}

// Resource kinds.
const (
	// KindContainers: containers labelled with the session, run by
	// Runtime.
	KindContainers = "containers"
	// KindProcessGroup: a process group, PGID, silo started.
	KindProcessGroup = "process-group"
	// KindDir: a directory, Path, removed by teardown.
	KindDir = "dir"
	// KindPod: the k8s location's pod and Secret, labelled with the
	// session, in Context and Namespace.
	KindPod = "k8s-pod"
	// KindNetwork: the session's private network, Network, made by
	// the container Runtime for the git proxy.
	KindNetwork = "network"
)

// Resource is one thing set up that teardown, or silo gc after a crash,
// removes.
type Resource struct {
	Kind    string `json:"kind"`
	Runtime string `json:"runtime,omitempty"`
	PGID    int    `json:"pgid,omitempty"`
	Path    string `json:"path,omitempty"`
	// Context and Namespace are KindPod's kubeconfig context and
	// namespace, empty for kubectl's defaults.
	Context   string `json:"context,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	// Network is KindNetwork's network name.
	Network string `json:"network,omitempty"`
}

// Result is how the session ended.
type Result struct {
	Ended    time.Time `json:"ended"`
	ExitCode int       `json:"exit_code"`
	Reason   string    `json:"reason"`
	Errors   []string  `json:"errors,omitempty"`
}

// recorder writes session.json each time it changes.
type recorder struct {
	mu   sync.Mutex
	path string
	rec  Record
}

func (r *recorder) update(fn func(*Record)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(&r.rec)
	return writeJSON(r.path, r.rec)
}

func (r *recorder) snapshot() Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rec
}

// writeJSON writes v to path atomically, mode 0600.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadRecord reads a session directory's session.json.
func ReadRecord(dir string) (Record, error) {
	var rec Record
	b, err := os.ReadFile(filepath.Join(dir, RecordFile))
	if err != nil {
		return rec, err
	}
	return rec, json.Unmarshal(b, &rec)
}

// alive reports whether pid is a running process.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
