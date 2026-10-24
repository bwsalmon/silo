package gateway

import (
	"fmt"
	"strings"
)

// Role is the kind of caller (design §4.1).
type Role string

const (
	// RoleAdmin is uid 0 or a configured admin uid: a human on the box.
	RoleAdmin Role = "admin"
	// RoleBridge is the session bridge acting for the human in the UI.
	RoleBridge Role = "bridge"
	// RoleAgent is an agent, identified by its uid and its unit's cgroup.
	RoleAgent Role = "agent"
)

// Caller is an authenticated caller.
type Caller struct {
	UID   int    `json:"uid"`
	PID   int    `json:"pid"`
	Role  Role   `json:"role"`
	Agent string `json:"agent,omitempty"`
}

// Human reports whether the caller acts for the human: an admin, or the
// bridge (UI) when it is not relaying for an agent.
func (c Caller) Human() bool { return c.Role == RoleAdmin || c.Role == RoleBridge }

// Actor is the caller as the audit log names it.
func (c Caller) Actor() string {
	switch c.Role {
	case RoleAdmin:
		return fmt.Sprintf("admin:uid=%d", c.UID)
	case RoleBridge:
		return "bridge"
	case RoleAgent:
		return "agent:" + c.Agent
	}
	return fmt.Sprintf("uid=%d", c.UID)
}

// PeerCred is what SO_PEERCRED reports about the other end of the socket.
type PeerCred struct {
	UID, GID, PID int
}

// Relay lets the bridge make a call on behalf of an agent, which it
// identified from that agent's MCP connection (design §4.1). The gateway
// re-checks that pid is inside the agent's unit.
type Relay struct {
	OnBehalfOf *OnBehalf `json:"on_behalf_of,omitempty"`
}

// OnBehalf names the agent and one of its processes.
type OnBehalf struct {
	Agent string `json:"agent"`
	PID   int    `json:"pid"`
}

// identify turns peer credentials (and a bridge relay) into a Caller.
// Must be called with g.mu held.
func (g *Gateway) identify(cred PeerCred, relay *OnBehalf) (Caller, error) {
	c := Caller{UID: cred.UID, PID: cred.PID}
	isBridge := g.bridgeUID >= 0 && cred.UID == g.bridgeUID
	if relay != nil {
		if !isBridge {
			return c, fmt.Errorf("only the bridge may relay calls for an agent")
		}
		return g.identifyAgent(c, relay.Agent, relay.PID)
	}
	if cred.UID == 0 {
		c.Role = RoleAdmin
		return c, nil
	}
	for _, u := range g.cfg.AdminUIDs {
		if cred.UID == u {
			c.Role = RoleAdmin
			return c, nil
		}
	}
	if isBridge {
		c.Role = RoleBridge
		return c, nil
	}
	for _, a := range g.st.Agents {
		if a.State == StateRunning && a.UID == cred.UID {
			return g.identifyAgent(c, a.ID, cred.PID)
		}
	}
	return c, fmt.Errorf("uid %d is not an admin, the bridge or a running agent", cred.UID)
}

// identifyAgent checks that pid runs inside agent id's unit.
func (g *Gateway) identifyAgent(c Caller, id string, pid int) (Caller, error) {
	a := g.st.Agents[id]
	if a == nil || a.State != StateRunning {
		return c, fmt.Errorf("agent %q is not running", id)
	}
	cg, err := g.d.Procs.Cgroup(pid)
	if err != nil {
		return c, fmt.Errorf("agent %q: cannot read the cgroup of pid %d: %v", id, pid, err)
	}
	if !strings.HasSuffix(cg, "/"+a.Unit) {
		return c, fmt.Errorf("agent %q: pid %d is in %s, not in %s", id, pid, cg, a.Unit)
	}
	c.Role, c.Agent, c.PID = RoleAgent, id, pid
	return c, nil
}

// isDescendant reports whether id is a strict descendant of ancestor.
// Must be called with g.mu held.
func (g *Gateway) isDescendant(id, ancestor string) bool {
	seen := map[string]bool{}
	for a := g.st.Agents[id]; a != nil && a.Parent != "" && !seen[a.ID]; a = g.st.Agents[a.Parent] {
		seen[a.ID] = true
		if a.Parent == ancestor {
			return true
		}
	}
	return false
}

// canSee reports whether caller may list or inspect agent id: humans see
// every agent, agents see themselves and their descendants.
func (g *Gateway) canSee(c Caller, id string) bool {
	return c.Human() || c.Agent == id || g.isDescendant(id, c.Agent)
}
