// Package agentcli maps a Silofile's agent, mode and remote_permissions
// onto one CLI's real command line, environment and staged files
// (design §6, cli-mapping.md). Each CLI is a Mapping in a subpackage
// (claude, codex, custom), registered by name; package session looks
// the Silofile's agent.cli up here.
package agentcli

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/v2/location"
	"github.com/bwsalmon/silo/v2/silofile"
)

// Modes.
const (
	Interactive = "interactive"
	Headless    = "headless"
	Attached    = "attached"
)

// Request is everything a mapping needs to build a CLI's command.
type Request struct {
	Agent silofile.Agent
	// Mode is the effective mode (interactive when the Silofile is
	// silent).
	Mode        string
	Permissions silofile.RemotePermissions
	// Prompt and SystemPrompt are the resolved texts, empty when unset.
	Prompt, SystemPrompt string
	// GatewayURL and GatewayToken are what the MCP config names.
	GatewayURL, GatewayToken string
	// GatewayTools names the tools the gateway lists.
	GatewayTools []string
	// Host is what the location learned in Prepare.
	Host location.HostInfo
	// Env is set in the CLI's environment on top of the host's: API-key
	// auth and credentials delivered {agent: env}.
	Env map[string]string
	// Deadline is agent.limits.deadline, 0 for none.
	Deadline time.Duration
}

// Plan is a CLI's staged files, and how to build its command once they
// are staged.
type Plan struct {
	Files []location.StagedFile
	// Command returns the CLI's command given the directory the files
	// were staged in.
	Command func(dir string) location.Command
	// Notes are things the user should be told at start.
	Notes []string
}

// Mapping is one CLI.
type Mapping interface {
	// VersionArgs asks the binary for its version; nil for none.
	VersionArgs() []string
	// ParseVersion picks the version out of VersionArgs' output.
	ParseVersion(out string) (string, error)
	// Build maps the request. A *Refusal is a setting the CLI cannot
	// express (exit 76); a *location.PrerequisiteError one that needs
	// the user to do something first (exit 69).
	Build(req Request) (Plan, error)
}

// Refusal is a setting the CLI cannot express exactly: silo refuses to
// start and exits 76 (design §6.1).
type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return r.Reason }

// Refusef returns a *Refusal.
func Refusef(format string, args ...any) error {
	return &Refusal{Reason: fmt.Sprintf(format, args...)}
}

// IsRefusal reports whether err is a *Refusal.
func IsRefusal(err error) bool {
	var r *Refusal
	return errors.As(err, &r)
}

var (
	mu       sync.RWMutex
	mappings = map[string]Mapping{}
)

// Register adds a CLI's mapping; subpackages call it from init.
func Register(cli string, m Mapping) {
	mu.Lock()
	defer mu.Unlock()
	mappings[cli] = m
}

// Lookup returns agent.cli's mapping.
func Lookup(cli string) (Mapping, error) {
	mu.RLock()
	defer mu.RUnlock()
	if m, ok := mappings[cli]; ok {
		return m, nil
	}
	names := make([]string, 0, len(mappings))
	for n := range mappings {
		names = append(names, n)
	}
	sort.Strings(names)
	return nil, Refusef("silo run does not drive cli %q yet; it drives %s", cli, strings.Join(names, ", "))
}

// CheckMode refuses attached, which v2.0 does not ship (design §6.2),
// and anything else that is not interactive or headless.
func CheckMode(mode string) error {
	switch mode {
	case Interactive, Headless:
		return nil
	case Attached:
		return Refusef("mode attached is reserved and not in v2.0: the CLIs' remote-control channels are new or experimental and unexercised (design §6.2); use interactive or headless")
	}
	return Refusef("unknown mode %q: use interactive or headless", mode)
}

var semver = regexp.MustCompile(`\d+\.\d+\.\d+`)

// SemVer returns the first x.y.z in out, for ParseVersion.
func SemVer(out string) (string, error) {
	v := semver.FindString(out)
	if v == "" {
		return "", fmt.Errorf("no version in %q", strings.TrimSpace(out))
	}
	return v, nil
}

// MergeEnv returns base with extra's keys set, extra winning, and the
// result sorted by key after base's order (stable for golden tests).
func MergeEnv(base []string, extra map[string]string) []string {
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := extra[k]; ok {
			continue
		}
		out = append(out, kv)
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+extra[k])
	}
	return out
}

// Getenv reads key from an environment list.
func Getenv(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && k == key {
			return v
		}
	}
	return ""
}

// ToolTimeout is how long a CLI should wait on one gateway call before
// giving up: the gateway's longest tool timeout (sandbox_exec's 30
// minute cap, design §4.4) plus slack, so the gateway's clock ends a
// call rather than the CLI abandoning it.
const ToolTimeout = 32 * time.Minute

// ServerName is the MCP server name every CLI's config gives the
// gateway.
const ServerName = "silo"
