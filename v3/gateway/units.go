package gateway

import (
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Unit layout (design §3):
//
//	silo.slice
//	├─ silo-agent-root.service              the root agent
//	├─ silo-agent-<id>.slice                 a top-level agent's subtree
//	│   ├─ silo-agent-<id>.service
//	│   └─ silo-agent-<id>-<id2>.slice       its child's subtree, nested
//	│       └─ silo-agent-<id2>.service
//
// systemd derives a slice's parent from the dashes in its name, so dashes
// inside an id are escaped (\x2d) in slice names.

const rootSlice = "silo.slice"

// RootAgentID is the root agent's id; its user is RootUser.
const (
	RootAgentID = "root"
	RootUser    = "silo-root"
	// ChildUserPrefix prefixes the users of agents other than root.
	ChildUserPrefix = "silo-a-"
)

func unitName(id string) string { return "silo-agent-" + id + ".service" }

func escapeSliceComponent(id string) string { return strings.ReplaceAll(id, "-", `\x2d`) }

// sliceName is the slice holding the agent at the end of path, where path
// is the chain of ids from the top-level ancestor (excluding root) down to
// the agent.
func sliceName(path []string) string {
	esc := make([]string, len(path))
	for i, id := range path {
		esc[i] = escapeSliceComponent(id)
	}
	return "silo-agent-" + strings.Join(esc, "-") + ".slice"
}

// limitProps renders limits as unit properties.
func limitProps(l Limits) []Property {
	var p []Property
	if l.MemoryMax != "" {
		p = append(p, Property{"MemoryMax", l.MemoryMax})
	}
	if l.MemoryHigh != "" {
		p = append(p, Property{"MemoryHigh", l.MemoryHigh})
	}
	if l.CPUQuota != "" {
		p = append(p, Property{"CPUQuota", l.CPUQuota})
	}
	if l.TasksMax > 0 {
		p = append(p, Property{"TasksMax", strconv.Itoa(l.TasksMax)})
	}
	return p
}

// agentInaccessible are hidden from every agent (design §7.2). The leading
// "-" tells systemd to skip paths that do not exist.
func agentInaccessible(c *Config) []string {
	paths := []string{"/etc/silo/secrets", c.StateDir, filepath.Dir(c.AuditLog), c.SocketPath}
	return append(paths, c.ExtraInaccessiblePaths...)
}

// serviceProps is the sandbox of an agent's service (design §4.2, §7.2).
func serviceProps(c *Config, a *Agent, u UserInfo) []Property {
	p := []Property{
		{"User", u.Name},
		{"Group", strconv.Itoa(u.GID)},
		{"WorkingDirectory", u.Home},
		{"PrivateTmp", "yes"},
		{"NoNewPrivileges", "yes"},
		{"ProtectSystem", "full"},
		{"ProtectKernelTunables", "yes"},
		{"ProtectKernelModules", "yes"},
		{"ProtectControlGroups", "yes"},
		{"RestrictSUIDSGID", "yes"},
		{"TimeoutStopSec", "30"},
	}
	if a.Root {
		// Root reads everything (CAP_DAC_READ_SEARCH and nothing else) and
		// writes only its home.
		p = append(p,
			Property{"ProtectHome", "read-only"},
			Property{"AmbientCapabilities", "CAP_DAC_READ_SEARCH"},
			Property{"CapabilityBoundingSet", "CAP_DAC_READ_SEARCH"},
		)
	} else {
		// Children see no other home: /home, /root and /run/user are empty,
		// and so is the silo home root apart from their own.
		p = append(p,
			Property{"ProtectHome", "tmpfs"},
			Property{"CapabilityBoundingSet", ""},
			Property{"TemporaryFileSystem", c.HomeRoot + ":ro"},
			Property{"BindPaths", u.Home},
		)
	}
	inacc := agentInaccessible(c)
	for i := range inacc {
		inacc[i] = "-" + inacc[i]
	}
	p = append(p, Property{"InaccessiblePaths", strings.Join(inacc, " ")})
	if a.Lifetime.Kind == LifetimeTTL {
		p = append(p, Property{"RuntimeMaxSec", strconv.Itoa(a.Lifetime.Seconds)})
	}
	return append(p, limitProps(a.Limits)...)
}

func sortStrings(s []string) { sort.Strings(s) }
