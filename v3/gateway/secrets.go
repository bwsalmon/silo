package gateway

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Secret requests (design §5.6). No method here takes or returns a secret
// value: params are decoded strictly, so a "value" field is refused, and
// the value travels from the UI to the agent's `silo connect` over its
// session, never through the gateway, its state or the audit log.

var secretNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// DefaultSecretDir is where `silo connect` writes a secret when the
// request names no path, relative to the agent's home.
const DefaultSecretDir = "~/.config/silo/secrets"

// SecretRequestParams is secret.request.
type SecretRequestParams struct {
	Auth
	Name   string `json:"name"`
	Reason string `json:"reason"`
	// Path is where to write it in the agent's environment: absolute, or
	// relative to the agent's home as ~/....
	Path string `json:"path,omitempty"`
}

// SecretRequest files a request for the human. Only agents ask.
func (g *Gateway) SecretRequest(c Caller, p SecretRequestParams) (*SecretRequest, error) {
	if c.Human() {
		return nil, invalidParams("secret.request is for agents; the human writes their own files")
	}
	if !secretNameRe.MatchString(p.Name) {
		return nil, invalidParams("name %q: want 1-64 letters, digits, '.', '_' and '-'", p.Name)
	}
	if p.Reason == "" || len(p.Reason) > 1000 {
		return nil, invalidParams("reason: must be 1-1000 bytes")
	}
	pth, err := secretPath(p.Name, p.Path)
	if err != nil {
		return nil, err
	}
	pending := 0
	for _, s := range g.st.Secrets {
		if s.Agent == c.Agent && s.State == SecretPending {
			pending++
			if s.Name == p.Name {
				return nil, &Error{Code: CodeConflict, Message: fmt.Sprintf("secret %q is already requested (%s)", p.Name, s.ID)}
			}
		}
	}
	if pending >= maxPendingSecs {
		return nil, &Error{Code: CodeConflict, Message: fmt.Sprintf("agent %q has %d pending secret requests", c.Agent, pending)}
	}
	g.st.NextSecret++
	s := &SecretRequest{
		ID: fmt.Sprintf("sec-%d", g.st.NextSecret), Agent: c.Agent, Name: p.Name, Reason: p.Reason,
		Path: pth, State: SecretPending, Created: g.d.Clock.Now(),
	}
	g.st.Secrets = append(g.st.Secrets, s)
	if err := g.saveLocked(); err != nil {
		return nil, err
	}
	g.audit(c.Actor(), EvSecretRequest, s)
	return s, nil
}

func secretPath(name, p string) (string, error) {
	if p == "" {
		return DefaultSecretDir + "/" + name, nil
	}
	rest, home := strings.CutPrefix(p, "~/")
	if !home && !strings.HasPrefix(p, "/") {
		return "", invalidParams("path %q: want an absolute path or ~/...", p)
	}
	if !home {
		rest = p
	}
	for _, seg := range strings.Split(rest, "/") {
		if seg == ".." {
			return "", invalidParams("path %q: must not contain ..", p)
		}
	}
	if len(p) > 1024 || strings.ContainsAny(p, "\x00\n") || strings.HasSuffix(p, "/") {
		return "", invalidParams("path %q: want a file path", p)
	}
	if home {
		return "~/" + strings.TrimPrefix(path.Clean("/"+rest), "/"), nil
	}
	return path.Clean(p), nil
}

// SecretResolveParams is secret.provide and secret.reject.
type SecretResolveParams struct {
	Auth
	ID string `json:"id"`
	// Path is where `silo connect` reports it wrote the value
	// (secret.provide); it defaults to the requested path.
	Path string `json:"path,omitempty"`
	// Note is the human's reason (secret.reject).
	Note string `json:"note,omitempty"`
}

// SecretProvide records that the human's value has been stored in the
// agent's environment. The bridge calls it once `silo connect` has
// answered {status: "stored"}.
func (g *Gateway) SecretProvide(c Caller, p SecretResolveParams) (*SecretRequest, error) {
	s, err := g.pendingSecret(c, "secret.provide", p.ID)
	if err != nil {
		return nil, err
	}
	if p.Note != "" {
		return nil, invalidParams("note is for secret.reject")
	}
	if p.Path != "" {
		if s.Path, err = secretPath(s.Name, p.Path); err != nil {
			return nil, err
		}
	}
	now := g.d.Clock.Now()
	s.State, s.Resolved = SecretStored, &now
	if err := g.saveLocked(); err != nil {
		return nil, err
	}
	g.audit(c.Actor(), EvSecretProvide, map[string]any{"id": s.ID, "agent": s.Agent, "name": s.Name, "path": s.Path})
	return s, nil
}

// SecretReject records the human's refusal; the agent sees it in
// secret.list.
func (g *Gateway) SecretReject(c Caller, p SecretResolveParams) (*SecretRequest, error) {
	s, err := g.pendingSecret(c, "secret.reject", p.ID)
	if err != nil {
		return nil, err
	}
	if p.Path != "" {
		return nil, invalidParams("path is for secret.provide")
	}
	if len(p.Note) > 1000 {
		return nil, invalidParams("note: at most 1000 bytes")
	}
	now := g.d.Clock.Now()
	s.State, s.Resolved, s.Note = SecretRejected, &now, p.Note
	if err := g.saveLocked(); err != nil {
		return nil, err
	}
	g.audit(c.Actor(), EvSecretReject, map[string]any{"id": s.ID, "agent": s.Agent, "name": s.Name, "note": p.Note})
	return s, nil
}

func (g *Gateway) pendingSecret(c Caller, method, id string) (*SecretRequest, error) {
	if !c.Human() {
		return nil, g.deny(c, method, "only the human answers secret requests")
	}
	for _, s := range g.st.Secrets {
		if s.ID == id {
			if s.State != SecretPending {
				return nil, &Error{Code: CodeConflict, Message: fmt.Sprintf("secret request %s is %s", id, s.State)}
			}
			return s, nil
		}
	}
	return nil, &Error{Code: CodeNotFound, Message: fmt.Sprintf("no secret request %q", id)}
}

// SecretListParams is secret.list.
type SecretListParams struct {
	Auth
	// Pending limits the list to unanswered requests.
	Pending bool `json:"pending,omitempty"`
}

// SecretList returns the human every request, an agent its own.
func (g *Gateway) SecretList(c Caller, p SecretListParams) ([]SecretRequest, error) {
	out := []SecretRequest{}
	for _, s := range g.st.Secrets {
		if (c.Human() || s.Agent == c.Agent) && (!p.Pending || s.State == SecretPending) {
			out = append(out, *s)
		}
	}
	return out, nil
}
