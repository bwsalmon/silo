package gateway

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
)

// Role is the kind of caller (design §4.1).
type Role string

const (
	// RoleAdmin is uid 0 or a configured admin uid: a human on the box.
	RoleAdmin Role = "admin"
	// RoleBridge is the session bridge acting for the human in the UI.
	RoleBridge Role = "bridge"
	// RoleAgent is an agent, identified by its session token or by the
	// bridge relaying for the agent's session.
	RoleAgent Role = "agent"
	// RoleConnect is an unidentified peer presenting a registration token
	// to agent.connect; it may call nothing else.
	RoleConnect Role = "connect"
)

// Caller is an authenticated caller.
type Caller struct {
	UID      int    `json:"uid"`
	PID      int    `json:"pid"`
	Role     Role   `json:"role"`
	Agent    string `json:"agent,omitempty"`
	Approved bool   `json:"approved,omitempty"`
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

// Auth is embedded in every request's params. An agent authenticates with
// the session token agent.connect gave its wrapper; the bridge may instead
// relay a call for an agent whose ACP session it carries (its MCP tool
// calls arrive that way, design §4.1). Neither: the caller is identified
// by its uid alone.
type Auth struct {
	AgentToken string    `json:"agent_token,omitempty"`
	OnBehalfOf *OnBehalf `json:"on_behalf_of,omitempty"`
}

// OnBehalf names the agent the bridge is relaying for.
type OnBehalf struct {
	Agent    string `json:"agent"`
	Approved bool   `json:"approved,omitempty"`
}

// Token prefixes, so a leaked token says what it is.
const (
	regTokenPrefix     = "silo_reg_"
	sessionTokenPrefix = "silo_ses_"
)

func newToken(prefix string) (string, string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err)) // never fails on Linux
	}
	t := prefix + hex.EncodeToString(b)
	return t, hashToken(t)
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func hashEqual(a, b string) bool {
	return a != "" && len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// identify turns peer credentials and a request's Auth into a Caller. An
// agent is returned whatever its state; dispatch decides which methods a
// caller in that state may use. Must be called with g.mu held.
func (g *Gateway) identify(cred PeerCred, auth Auth) (Caller, error) {
	c := Caller{UID: cred.UID, PID: cred.PID}
	isBridge := g.bridgeUID >= 0 && cred.UID == g.bridgeUID
	switch {
	case auth.AgentToken != "" && auth.OnBehalfOf != nil:
		return c, fmt.Errorf("give agent_token or on_behalf_of, not both")
	case auth.AgentToken != "":
		h := hashToken(auth.AgentToken)
		for _, a := range g.st.Agents {
			if hashEqual(a.SessionHash, h) {
				c.Role, c.Agent = RoleAgent, a.ID
				return c, nil
			}
		}
		return c, fmt.Errorf("unknown or revoked agent token")
	case auth.OnBehalfOf != nil:
		if !isBridge {
			return c, fmt.Errorf("only the bridge may relay calls for an agent")
		}
		a := g.st.Agents[auth.OnBehalfOf.Agent]
		if a == nil || a.State != StateConnected {
			return c, fmt.Errorf("agent %q is not connected", auth.OnBehalfOf.Agent)
		}
		c.Role, c.Agent, c.Approved = RoleAgent, a.ID, auth.OnBehalfOf.Approved
		return c, nil
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
	c.Role = RoleConnect
	return c, nil
}

// isDescendant reports whether id is a strict descendant of ancestor.
// Must be called with g.mu held.
func (g *Gateway) isDescendant(id, ancestor string) bool {
	if ancestor == "" {
		return false
	}
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
	return c.Human() || (c.Agent != "" && (c.Agent == id || g.isDescendant(id, c.Agent)))
}
