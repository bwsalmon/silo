package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/bwsalmon/silo/v3/gateway/approval"
)

// dispatchApproval authenticates a run_as_root.* or approval.* call and
// hands it to the approval service (package approval). Unlike the rest of
// the API it runs outside the gateway's mutex, because run_as_root.wait
// blocks until the human decides.
//
// The root agent is recognised by its session token (or the bridge
// relaying for it), or by SO_PEERCRED: a call from the root agent's Unix
// user with no token, so `silo root propose` works from the root agent's
// shell.
func (s *Server) dispatchApproval(ctx context.Context, cred PeerCred, method string, raw json.RawMessage) (any, error) {
	g := s.G
	var auth Auth
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &auth); err != nil {
			return nil, invalidParams("params: %v", err)
		}
	}
	p, err := g.approvalPrincipal(cred, auth, method)
	if err != nil {
		return nil, err
	}
	res, err := s.Approvals.Call(ctx, p, method, raw)
	var ae *approval.Error
	if errors.As(err, &ae) {
		return nil, &Error{Code: ae.Code, Message: ae.Message}
	}
	return res, err
}

func (g *Gateway) approvalPrincipal(cred PeerCred, auth Auth, method string) (approval.Principal, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	c, err := g.identify(cred, auth)
	if err != nil {
		g.audit(fmt.Sprintf("uid=%d", cred.UID), EvAuthDeny, map[string]any{"method": method, "uid": cred.UID, "pid": cred.PID, "reason": err.Error()})
		return approval.Principal{}, &Error{Code: CodeForbidden, Message: "permission denied: " + err.Error()}
	}
	p := approval.Principal{Actor: c.Actor(), Human: c.Human(), Admin: c.Role == RoleAdmin}
	switch c.Role {
	case RoleAgent:
		if a := g.st.Agents[c.Agent]; a == nil || a.State != StateConnected {
			return p, g.deny(c, method, fmt.Sprintf("agent %q is not connected", c.Agent))
		}
		p.Agent = c.Agent
	case RoleConnect:
		root := g.st.Agents[RootAgentID]
		if root == nil || root.State == StateStopped || g.d.LookupUID == nil {
			return p, g.deny(c, method, fmt.Sprintf("uid %d is not an admin, the bridge or the root agent, and gave no agent token", cred.UID))
		}
		uid, err := g.d.LookupUID(root.User)
		if err != nil || uid < 0 || uid != cred.UID {
			return p, g.deny(c, method, fmt.Sprintf("uid %d is not an admin, the bridge or the root agent, and gave no agent token", cred.UID))
		}
		p.Agent, p.Actor = RootAgentID, "agent:"+RootAgentID
	}
	return p, nil
}
