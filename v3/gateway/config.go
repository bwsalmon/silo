package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/bwsalmon/silo/v3/internal/tomlite"
)

// Default locations (design Appendix B).
const (
	DefaultConfigPath = "/etc/silo/gateway.toml"
	DefaultSocketPath = "/run/silo/gateway.sock"
	DefaultStateDir   = "/var/lib/silo/gateway"
	DefaultHomeRoot   = "/var/lib/silo/home"
	DefaultAuditLog   = "/var/log/silo/audit.jsonl"
	DefaultCLIsDir    = "/etc/silo/clis.d"
)

// Config is /etc/silo/gateway.toml. Every field is optional.
type Config struct {
	SocketPath  string `json:"socket"`
	SocketGroup string `json:"socket_group"`
	StateDir    string `json:"state_dir"`
	HomeRoot    string `json:"home_root"`
	AuditLog    string `json:"audit_log"`
	CLIsDir     string `json:"clis_dir"`

	// AdminUIDs are the humans' uids. uid 0 is always an admin.
	AdminUIDs []int `json:"admin_uids"`
	// BridgeUser is the session bridge's user (design §4.1). Its calls are
	// the human's, unless it relays a call on behalf of an agent.
	BridgeUser string `json:"bridge_user"`

	// UIDMin and UIDMax bound the uids silo allocates for agent users.
	UIDMin int `json:"uid_min"`
	UIDMax int `json:"uid_max"`

	// ReconcileIntervalSec is how often the gateway polls agent units for
	// exits.
	ReconcileIntervalSec int `json:"reconcile_interval_s"`

	// DefaultLimits apply to spawns that leave a limit unset.
	DefaultLimits Limits `json:"default_limits"`

	RootAgent RootAgentConfig `json:"root_agent"`

	// ExtraInaccessiblePaths are hidden from every agent in addition to
	// silo's own state, logs, secrets and socket (design §7.2).
	ExtraInaccessiblePaths []string `json:"inaccessible_paths"`
}

// RootAgentConfig configures the root agent (design §7).
type RootAgentConfig struct {
	// Enabled defaults to true. Turning it off is for tests.
	Enabled *bool `json:"enabled"`
	// CLI defaults to "idle" in M1a, which keeps the unit up without
	// running a model; M1b replaces this with the ACP session.
	CLI    string `json:"cli"`
	Brief  string `json:"brief"`
	Limits Limits `json:"limits"`
}

// RootEnabled reports whether the gateway should keep a root agent.
func (c *Config) RootEnabled() bool { return c.RootAgent.Enabled == nil || *c.RootAgent.Enabled }

// LoadConfig reads the config file at path. A missing file is not an error:
// the defaults apply.
func LoadConfig(path string) (*Config, error) {
	c := &Config{}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := tomlite.Unmarshal(data, c); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	c.setDefaults()
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func (c *Config) setDefaults() {
	def := func(s *string, v string) {
		if *s == "" {
			*s = v
		}
	}
	def(&c.SocketPath, DefaultSocketPath)
	def(&c.SocketGroup, "silo-gw")
	def(&c.StateDir, DefaultStateDir)
	def(&c.HomeRoot, DefaultHomeRoot)
	def(&c.AuditLog, DefaultAuditLog)
	def(&c.CLIsDir, DefaultCLIsDir)
	def(&c.BridgeUser, "silo")
	def(&c.RootAgent.CLI, "idle")
	if c.UIDMin == 0 {
		c.UIDMin = 61000
	}
	if c.UIDMax == 0 {
		c.UIDMax = 61999
	}
	if c.ReconcileIntervalSec == 0 {
		c.ReconcileIntervalSec = 5
	}
	c.DefaultLimits = c.DefaultLimits.withDefaults(Limits{MemoryMax: "2G", CPUQuota: "100%", TasksMax: 512})
	c.RootAgent.Limits = c.RootAgent.Limits.withDefaults(Limits{MemoryMax: "4G", CPUQuota: "200%", TasksMax: 1024})
}

func (c *Config) validate() error {
	if c.UIDMin < 1000 || c.UIDMax < c.UIDMin {
		return fmt.Errorf("uid range %d-%d: must be at least 1000 and non-empty", c.UIDMin, c.UIDMax)
	}
	for _, p := range append([]string{c.StateDir, c.HomeRoot, c.AuditLog, c.SocketPath, c.CLIsDir}, c.ExtraInaccessiblePaths...) {
		if !filepath.IsAbs(p) || strings.ContainsAny(p, " \t\n\"'\\") {
			return fmt.Errorf("path %q: must be absolute with no spaces, quotes or backslashes", p)
		}
	}
	if err := c.DefaultLimits.Validate(); err != nil {
		return fmt.Errorf("default_limits: %w", err)
	}
	if err := c.RootAgent.Limits.Validate(); err != nil {
		return fmt.Errorf("root_agent.limits: %w", err)
	}
	return nil
}

// Limits are an agent's resource limits, applied to its service and, for
// agents with a slice, to the slice that holds its whole subtree.
type Limits struct {
	MemoryMax  string `json:"memory_max,omitempty"`
	MemoryHigh string `json:"memory_high,omitempty"`
	CPUQuota   string `json:"cpu_quota,omitempty"`
	TasksMax   int    `json:"tasks_max,omitempty"`
	// DiskQuota is in the design's SpawnRequest but not implemented yet;
	// setting it is refused rather than ignored.
	DiskQuota string `json:"disk_quota,omitempty"`
}

var (
	memoryRe = regexp.MustCompile(`^([0-9]+[KMGT]?|[0-9]{1,2}%|100%|infinity)$`)
	cpuRe    = regexp.MustCompile(`^[0-9]{1,5}%$`)
)

func (l Limits) withDefaults(d Limits) Limits {
	if l.MemoryMax == "" {
		l.MemoryMax = d.MemoryMax
	}
	if l.MemoryHigh == "" {
		l.MemoryHigh = d.MemoryHigh
	}
	if l.CPUQuota == "" {
		l.CPUQuota = d.CPUQuota
	}
	if l.TasksMax == 0 {
		l.TasksMax = d.TasksMax
	}
	return l
}

// Validate checks every limit's syntax, so nothing but a well-formed value
// reaches a unit property.
func (l Limits) Validate() error {
	if l.MemoryMax != "" && !memoryRe.MatchString(l.MemoryMax) {
		return fmt.Errorf("memory_max %q: want e.g. 512M, 2G, 50%% or infinity", l.MemoryMax)
	}
	if l.MemoryHigh != "" && !memoryRe.MatchString(l.MemoryHigh) {
		return fmt.Errorf("memory_high %q: want e.g. 512M, 2G, 50%% or infinity", l.MemoryHigh)
	}
	if l.CPUQuota != "" && !cpuRe.MatchString(l.CPUQuota) {
		return fmt.Errorf("cpu_quota %q: want a percentage such as 150%%", l.CPUQuota)
	}
	if l.TasksMax < 0 {
		return fmt.Errorf("tasks_max %d: must not be negative", l.TasksMax)
	}
	if l.DiskQuota != "" {
		return errors.New("disk_quota is not supported yet")
	}
	return nil
}

// CLIDef is one file in /etc/silo/clis.d: how to start a coding-agent CLI.
//
//	name = "claude"                       # defaults to the file's base name
//	command = ["/opt/silo/cli/claude", "--print", "{brief}"]
//	[env]
//	DISABLE_AUTOUPDATER = "1"
//
// In command, {brief}, {agent} and {home} are replaced by the spawn's
// brief, the agent id and the agent's home directory.
type CLIDef struct {
	Name    string            `json:"name"`
	Command []string          `json:"command"`
	Env     map[string]string `json:"env"`
}

// BuiltinCLIs are available without any clis.d file. A file of the same
// name replaces them.
func BuiltinCLIs() map[string]CLIDef {
	return map[string]CLIDef{
		// idle keeps an agent's unit up without running anything; it is the
		// root agent's default until the ACP launcher lands (M1b).
		"idle": {Name: "idle", Command: []string{"/bin/sleep", "infinity"}},
	}
}

var cliNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// LoadCLIs reads every *.toml and *.json file in dir on top of the
// built-in definitions. A missing directory yields only the built-ins.
func LoadCLIs(dir string) (map[string]CLIDef, error) {
	out := BuiltinCLIs()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	seen := map[string]string{}
	for _, n := range names {
		ext := filepath.Ext(n)
		if ext != ".toml" && ext != ".json" {
			continue
		}
		path := filepath.Join(dir, n)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var d CLIDef
		if ext == ".toml" {
			err = tomlite.Unmarshal(data, &d)
		} else {
			dec := json.NewDecoder(strings.NewReader(string(data)))
			dec.DisallowUnknownFields()
			err = dec.Decode(&d)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if d.Name == "" {
			d.Name = strings.TrimSuffix(n, ext)
		}
		if err := d.validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if prev, dup := seen[d.Name]; dup {
			return nil, fmt.Errorf("%s: cli %q is already defined in %s", path, d.Name, prev)
		}
		seen[d.Name] = path
		out[d.Name] = d
	}
	return out, nil
}

func (d CLIDef) validate() error {
	if !cliNameRe.MatchString(d.Name) {
		return fmt.Errorf("cli name %q: want lower-case letters, digits, - and _", d.Name)
	}
	if len(d.Command) == 0 || !filepath.IsAbs(d.Command[0]) {
		return errors.New("command: must be non-empty and start with an absolute path")
	}
	for k := range d.Env {
		if !envNameRe.MatchString(k) {
			return fmt.Errorf("env: bad variable name %q", k)
		}
	}
	return nil
}
