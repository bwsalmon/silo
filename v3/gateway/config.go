package gateway

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bwsalmon/silo/v3/internal/tomlite"
)

// Default locations (design Appendix B).
const (
	DefaultConfigPath = "/etc/silo/gateway.toml"
	DefaultSocketPath = "/run/silo/gateway.sock"
	DefaultStateDir   = "/var/lib/silo/gateway"
	DefaultAuditLog   = "/var/log/silo/audit.jsonl"
)

// The standing agents' defaults (design §7, §8.8).
const (
	RootAgentID     = "root"
	RootUser        = "silo-root"
	UserAgentID     = "main"
	UserAgentUser   = "silo-user"
	tokensSubdir    = "tokens"
	defaultRuntime  = "claude"
	defaultTimeout  = 90
	defaultInterval = 15
)

// Config is /etc/silo/gateway.toml. Every field is optional.
type Config struct {
	SocketPath  string `json:"socket"`
	SocketGroup string `json:"socket_group"`
	StateDir    string `json:"state_dir"`
	AuditLog    string `json:"audit_log"`

	// HubURL is the endpoint `agent.register` tells new agents to dial.
	// Agents on this host dial the socket; remote ones the bridge's
	// /connect endpoint, which relays to it.
	HubURL string `json:"hub_url"`

	// AdminUIDs are the humans' uids. uid 0 is always an admin.
	AdminUIDs []int `json:"admin_uids"`
	// BridgeUser is the session bridge's user (design §4.1). Its calls are
	// the human's, unless it relays a call on behalf of an agent.
	BridgeUser string `json:"bridge_user"`

	// HeartbeatTimeoutSec is how long a connected agent may go without a
	// heartbeat before it is marked disconnected; ReconcileIntervalSec is
	// how often that is checked.
	HeartbeatTimeoutSec  int `json:"heartbeat_timeout_s"`
	ReconcileIntervalSec int `json:"reconcile_interval_s"`

	// DefaultLimits apply to registrations that leave a limit unset.
	DefaultLimits Limits `json:"default_limits"`

	RootAgent StandingAgentConfig `json:"root_agent"`
	UserAgent StandingAgentConfig `json:"user_agent"`
}

// StandingAgentConfig configures one of the two agents the gateway seeds
// on first start: the root agent (design §7) and the default user agent.
type StandingAgentConfig struct {
	// Enabled defaults to true. Turning it off is for tests and for boxes
	// that want no default user agent.
	Enabled *bool `json:"enabled"`
	// Name is the agent id; only the user agent's may be changed.
	Name    string `json:"name"`
	User    string `json:"user"`
	Runtime string `json:"runtime"`
	Brief   string `json:"brief"`
	Limits  Limits `json:"limits"`
}

// RootEnabled reports whether the gateway should keep a root agent.
func (c *Config) RootEnabled() bool { return c.RootAgent.Enabled == nil || *c.RootAgent.Enabled }

// UserAgentEnabled reports whether the gateway should keep the default
// user agent.
func (c *Config) UserAgentEnabled() bool { return c.UserAgent.Enabled == nil || *c.UserAgent.Enabled }

// TokenDir is where standing agents' registration tokens are written, one
// root-only file per agent, for their units to load with LoadCredential=.
func (c *Config) TokenDir() string { return filepath.Join(c.StateDir, tokensSubdir) }

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
	def(&c.AuditLog, DefaultAuditLog)
	def(&c.HubURL, "unix://"+c.SocketPath)
	def(&c.BridgeUser, "silo")
	def(&c.RootAgent.Name, RootAgentID)
	def(&c.RootAgent.User, RootUser)
	def(&c.RootAgent.Runtime, defaultRuntime)
	def(&c.UserAgent.Name, UserAgentID)
	def(&c.UserAgent.User, UserAgentUser)
	def(&c.UserAgent.Runtime, defaultRuntime)
	if c.HeartbeatTimeoutSec == 0 {
		c.HeartbeatTimeoutSec = defaultTimeout
	}
	if c.ReconcileIntervalSec == 0 {
		c.ReconcileIntervalSec = defaultInterval
	}
	c.DefaultLimits = c.DefaultLimits.withDefaults(Limits{MemoryMax: "2G", CPUQuota: "100%", TasksMax: 512})
	c.RootAgent.Limits = c.RootAgent.Limits.withDefaults(Limits{MemoryMax: "4G", CPUQuota: "200%", TasksMax: 1024})
	c.UserAgent.Limits = c.UserAgent.Limits.withDefaults(Limits{MemoryMax: "4G", CPUQuota: "200%", TasksMax: 1024})
}

func (c *Config) validate() error {
	for _, p := range []string{c.StateDir, c.AuditLog, c.SocketPath} {
		if !filepath.IsAbs(p) || strings.ContainsAny(p, " \t\n\"'\\") {
			return fmt.Errorf("path %q: must be absolute with no spaces, quotes or backslashes", p)
		}
	}
	if c.HeartbeatTimeoutSec < 0 || c.ReconcileIntervalSec < 0 {
		return errors.New("heartbeat_timeout_s and reconcile_interval_s must not be negative")
	}
	if c.RootAgent.Name != RootAgentID {
		return fmt.Errorf("root_agent.name: the root agent is always %q", RootAgentID)
	}
	if !agentIDRe.MatchString(c.UserAgent.Name) || c.UserAgent.Name == RootAgentID {
		return fmt.Errorf("user_agent.name %q: want 1-25 lower-case letters, digits and inner dashes, not %q", c.UserAgent.Name, RootAgentID)
	}
	for _, u := range []string{c.RootAgent.User, c.UserAgent.User} {
		if !unixUserRe.MatchString(u) || u == "root" {
			return fmt.Errorf("user %q: want a Unix user name other than root", u)
		}
	}
	if c.UserAgent.User == c.RootAgent.User {
		return fmt.Errorf("user_agent.user %q: must differ from the root agent's", c.UserAgent.User)
	}
	if err := c.DefaultLimits.Validate(); err != nil {
		return fmt.Errorf("default_limits: %w", err)
	}
	if err := c.RootAgent.Limits.Validate(); err != nil {
		return fmt.Errorf("root_agent.limits: %w", err)
	}
	if err := c.UserAgent.Limits.Validate(); err != nil {
		return fmt.Errorf("user_agent.limits: %w", err)
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
	// DiskQuota is in the design's registration request but not enforced yet;
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
