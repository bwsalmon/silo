package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Agent is the agent object (agents.md, "The agent object").
type Agent struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Silo     string `json:"silo"`
	SiloPath string `json:"silo_path"`
	Harness  struct {
		Adapter    string `json:"adapter"`
		Version    string `json:"version"`
		Credential string `json:"credential"`
	} `json:"harness"`
	Model  string `json:"model"`
	Budget struct {
		USD         string    `json:"usd"`
		SpentUSD    string    `json:"spent_usd"`
		PeriodStart time.Time `json:"period_start"`
	} `json:"budget"`
	ToolVM struct {
		ToolVM
		Instance   string `json:"instance"`
		IP         string `json:"ip"`
		ToolServer string `json:"tool_server"`
	} `json:"tool_vm"`
	Lease struct {
		ExpireTime time.Time `json:"expire_time"`
	} `json:"lease"`
	Prompt        string  `json:"prompt"`
	Owner         *string `json:"owner"`
	State         string  `json:"state"`
	StatusMessage string  `json:"status_message,omitempty"`
	Session       *struct {
		ID       string    `json:"id"`
		Started  time.Time `json:"started"`
		Turns    int       `json:"turns"`
		Restarts int       `json:"restarts"`
	} `json:"session"`
	CreateTime time.Time         `json:"create_time"`
	Labels     map[string]string `json:"labels"`
}

// ToolVM is the shape of an agent's tool VM.
type ToolVM struct {
	Zone        string            `json:"zone"`
	Subnet      string            `json:"subnet"`
	MachineType string            `json:"machine_type,omitempty"`
	Image       string            `json:"image,omitempty"`
	DiskGB      int64             `json:"disk_gb,omitempty"`
	Spot        bool              `json:"spot,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// AgentRequest creates an agent.
type AgentRequest struct {
	Name    string `json:"name"`
	Harness struct {
		Adapter    string `json:"adapter"`
		Credential string `json:"credential"`
	} `json:"harness"`
	Model  string `json:"model"`
	Budget struct {
		USD string `json:"usd"`
	} `json:"budget"`
	ToolVM ToolVM            `json:"tool_vm"`
	Lease  Lease             `json:"lease"`
	Prompt string            `json:"prompt,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

// Lease is a lease asked for: an expire_time, or a ttl from now.
type Lease struct {
	ExpireTime string `json:"expire_time,omitempty"`
	TTL        string `json:"ttl,omitempty"`
}

// AgentPatch changes an agent's budget or labels; nil leaves one as it
// is.
type AgentPatch struct {
	Budget *struct {
		USD string `json:"usd"`
	} `json:"budget,omitempty"`
	Labels *map[string]string `json:"labels,omitempty"`
}

// ModelPrice is an entry of the model price table, in dollars per
// million tokens.
type ModelPrice struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Input      string `json:"input"`
	Output     string `json:"output"`
	CacheWrite string `json:"cache_write"`
	CacheRead  string `json:"cache_read"`
	Source     string `json:"source,omitempty"`
}

// AgentPath is the URL path of the agent ref names: a path,
// /team-a/t-1945/agents/fixer, or its id (m3x8re, or -m3x8re as in a
// URL), which is looked up for its silo.
func (c *Client) AgentPath(ctx context.Context, ref string) (string, error) {
	if id := strings.TrimPrefix(ref, "-"); !strings.Contains(id, "/") {
		a, err := c.AgentByID(ctx, id)
		if err != nil {
			return "", err
		}
		return "silos/-" + a.Silo + "/agents/-" + a.ID, nil
	}
	silo, name, ok := strings.Cut(ref, "/agents/")
	if !ok || name == "" || strings.Contains(name, "/") {
		return "", &Error{Status: http.StatusBadRequest, Code: "INVALID_ARGUMENT",
			Message: "an agent is named <silo>/agents/<name>, as /team-a/agents/fixer, or -<id>"}
	}
	if silo == "" {
		silo = "/"
	}
	p, err := c.SiloPath(ctx, silo)
	if err != nil {
		return "", err
	}
	return p + "/agents/" + url.PathEscape(name), nil
}

// CreateAgent creates an agent in silo; the operation finishes when its
// tool server connects.
func (c *Client) CreateAgent(ctx context.Context, silo string, req AgentRequest, m Mutation) (*Operation, map[string]any, error) {
	p, err := c.SiloPath(ctx, silo)
	if err != nil {
		return nil, nil, err
	}
	var out map[string]any
	if err := c.Do(ctx, http.MethodPost, p+"/agents", m.query(nil), req, &out); err != nil {
		return nil, nil, err
	}
	if m.ValidateOnly {
		return nil, out, nil
	}
	b, _ := json.Marshal(out)
	var op Operation
	err = json.Unmarshal(b, &op)
	return &op, out, err
}

// Agents lists a silo's agents.
func (c *Client) Agents(ctx context.Context, silo string) ([]Agent, error) {
	p, err := c.SiloPath(ctx, silo)
	if err != nil {
		return nil, err
	}
	var all []Agent
	q := url.Values{}
	for {
		var out struct {
			Agents []Agent `json:"agents"`
			Next   string  `json:"next_page_token"`
		}
		if err := c.Do(ctx, http.MethodGet, p+"/agents", q, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Agents...)
		if out.Next == "" {
			return all, nil
		}
		q.Set("page_token", out.Next)
	}
}

// AgentByID reads an agent by its id.
func (c *Client) AgentByID(ctx context.Context, id string) (*Agent, error) {
	var out Agent
	if err := c.Do(ctx, http.MethodGet, "agents/-"+url.PathEscape(strings.TrimPrefix(id, "-")), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Agent reads an agent.
func (c *Client) Agent(ctx context.Context, ref string) (*Agent, error) {
	p, err := c.AgentPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out Agent
	if err := c.Do(ctx, http.MethodGet, p, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PatchAgent changes an agent's budget.usd or labels.
func (c *Client) PatchAgent(ctx context.Context, ref string, patch AgentPatch, m Mutation) (*Agent, error) {
	p, err := c.AgentPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out Agent
	if err := c.Do(ctx, http.MethodPatch, p, m.query(nil), patch, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteAgent deletes an agent, and what it owns; a long-running
// operation, or with validate_only what it would delete.
func (c *Client) DeleteAgent(ctx context.Context, ref string, m Mutation) (map[string]any, error) {
	p, err := c.AgentPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := c.Do(ctx, http.MethodDelete, p, m.query(nil), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// AgentAction is :stop, :resume or :renew (with body a Lease).
func (c *Client) AgentAction(ctx context.Context, ref, verb string, body any, m Mutation) (*Agent, error) {
	p, err := c.AgentPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	if body == nil {
		body = struct{}{}
	}
	var out Agent
	if err := c.Do(ctx, http.MethodPost, p+":"+verb, m.query(nil), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ModelPrices lists the model price table, overrides laid over the
// release's.
func (c *Client) ModelPrices(ctx context.Context) ([]ModelPrice, error) {
	var out struct {
		Prices []ModelPrice `json:"model_prices"`
	}
	if err := c.Do(ctx, http.MethodGet, "modelPrices", nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Prices, nil
}

// SetModelPrice overrides a model's price (root members only).
func (c *Client) SetModelPrice(ctx context.Context, p ModelPrice, m Mutation) (*ModelPrice, error) {
	var out ModelPrice
	if err := c.Do(ctx, http.MethodPut, "modelPrices/"+url.PathEscape(p.Provider)+"/"+url.PathEscape(p.Model), m.query(nil), p, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteModelPrice removes an override.
func (c *Client) DeleteModelPrice(ctx context.Context, provider, model string, m Mutation) error {
	return c.Do(ctx, http.MethodDelete, "modelPrices/"+url.PathEscape(provider)+"/"+url.PathEscape(model), m.query(nil), nil, nil)
}

// Credential is one of the deployment's agent credentials, as the API
// shows it: never its secret.
type Credential struct {
	Name        string    `json:"name"`
	Provider    string    `json:"provider"`
	Kind        string    `json:"kind"`
	MaxSessions int       `json:"max_sessions"`
	CreateTime  time.Time `json:"create_time"`
	UpdateTime  time.Time `json:"update_time"`
}

// CredentialRequest adds a credential: an api_key's Secret, or a
// subscription's login Files (by path under the harness's home, base64).
// MaxSessions nil takes the kind's default.
type CredentialRequest struct {
	Name        string            `json:"name"`
	Provider    string            `json:"provider"`
	Kind        string            `json:"kind"`
	MaxSessions *int              `json:"max_sessions,omitempty"`
	Secret      string            `json:"secret,omitempty"`
	Files       map[string]string `json:"files,omitempty"`
}

// Credentials lists the deployment's credentials (root members only).
func (c *Client) Credentials(ctx context.Context) ([]Credential, error) {
	var out struct {
		Credentials []Credential `json:"credentials"`
	}
	if err := c.Do(ctx, http.MethodGet, "credentials", nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Credentials, nil
}

// AddCredential adds a credential, or replaces the secret of one of the
// same provider and kind (root members only).
func (c *Client) AddCredential(ctx context.Context, req CredentialRequest, m Mutation) (*Credential, error) {
	var out Credential
	if err := c.Do(ctx, http.MethodPost, "credentials", m.query(nil), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RemoveCredential removes a credential (root members only).
func (c *Client) RemoveCredential(ctx context.Context, name string, m Mutation) error {
	return c.Do(ctx, http.MethodDelete, "credentials/"+url.PathEscape(name), m.query(nil), nil, nil)
}

// RemoveAllCredentials removes every credential, and returns their
// names: silo uninstall's first step (root members only).
func (c *Client) RemoveAllCredentials(ctx context.Context, m Mutation) ([]string, error) {
	var out struct {
		Deleted []string `json:"deleted"`
	}
	if err := c.Do(ctx, http.MethodDelete, "credentials", m.query(nil), nil, &out); err != nil {
		return nil, err
	}
	return out.Deleted, nil
}
