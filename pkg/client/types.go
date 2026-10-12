package client

import (
	"encoding/json"
	"time"
)

// The wire types below are the client's own reading of the objects in
// docs/silo/fence-api.md. They are not shared with the server package,
// so that a client binary carries no store: v1 changes only by adding
// fields, and a field this client doesn't know is ignored.

// Silo is the silo object (fence-api.md, "Silo", SV-4).
type Silo struct {
	ID            string                            `json:"id"`
	Name          string                            `json:"name"`
	Path          string                            `json:"path"`
	Parent        *string                           `json:"parent"`
	Depth         int                               `json:"depth"`
	Backing       Backing                           `json:"backing"`
	Facade        Facade                            `json:"facade"`
	Regions       []string                          `json:"regions"`
	Quota         map[string]map[string]QuotaAmount `json:"quota"`
	ModelBudget   ModelBudget                       `json:"model_budget"`
	Address       Address                           `json:"address"`
	Egress        Owned[Egress]                     `json:"egress"`
	GrantPolicy   Owned[map[string]string]          `json:"grant_policy"`
	Control       Control                           `json:"control"`
	AgentPolicy   Owned[AgentPolicy]                `json:"agent_policy"`
	OwnerAgent    *string                           `json:"owner_agent"`
	State         string                            `json:"state"`
	OverCommitted []OverCommit                      `json:"over_committed"`
	CreateTime    time.Time                         `json:"create_time"`
	Labels        map[string]string                 `json:"labels"`
	Children      []*Silo                           `json:"children,omitempty"`
}

// Backing is how a silo is isolated (SV-5).
type Backing struct {
	Kind string `json:"kind"`
}

// Facade is where a silo's compute/v1 facade is.
type Facade struct {
	Kind     string `json:"kind"`
	Project  string `json:"project"`
	Endpoint string `json:"endpoint"`
	// NamePrefix is what every name in the facade must start with,
	// <id>- (gcp-backend.md, "Naming", SV-76).
	NamePrefix string `json:"name_prefix"`
}

// QuotaAmount is one class in one region of a silo's quota.
type QuotaAmount struct {
	Granted   json.Number `json:"granted"`
	Delegated json.Number `json:"delegated"`
	Used      json.Number `json:"used"`
}

// ModelBudget is the silo's model budget, in dollars.
type ModelBudget struct {
	Period       string `json:"period"`
	GrantedUSD   string `json:"granted_usd"`
	DelegatedUSD string `json:"delegated_usd"`
	UsedUSD      string `json:"used_usd"`
}

// Address is a silo's address space.
type Address struct {
	Blocks    []string    `json:"blocks"`
	MaxBlocks int         `json:"max_blocks"`
	Delegated []Delegated `json:"delegated"`
	OwnRanges []OwnRange  `json:"own_ranges"`
}

// Delegated is a range held by one of the silo's children.
type Delegated struct {
	Range string `json:"range"`
	Child string `json:"child"`
}

// OwnRange is a range held by one of the silo's subnets.
type OwnRange struct {
	Range  string `json:"range"`
	Subnet string `json:"subnet"`
	Region string `json:"region,omitempty"`
}

// Owned is a policy as the silo set it and as it is in effect.
type Owned[T any] struct {
	Own       T `json:"own"`
	Effective T `json:"effective"`
}

// Egress is an egress policy.
type Egress struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
}

// AgentPolicy is an agent policy (SV-23).
type AgentPolicy struct {
	Credentials []string `json:"credentials,omitempty"`
	Adapters    []string `json:"adapters,omitempty"`
	Models      []string `json:"models,omitempty"`
	MaxLease    string   `json:"max_lease,omitempty"`
}

// Control is the control policy, with the silo its effective value
// comes from.
type Control struct {
	Own           string `json:"own"`
	Effective     string `json:"effective"`
	EffectiveFrom string `json:"effective_from"`
}

// OverCommit is a class and region a silo holds more of than its grant.
type OverCommit struct {
	Class  string `json:"class"`
	Region string `json:"region"`
}

// QuotaMap is the quota of a create, a PATCH or a preview: region
// ("global" for a global class) to class name to amount.
type QuotaMap map[string]map[string]json.Number

// AddressWant asks for a block: a prefix length silod places, or an
// exact range.
type AddressWant struct {
	PrefixLength int    `json:"prefix_length,omitempty"`
	Range        string `json:"range,omitempty"`
}

// CreateRequest is the body of create (SV-7). Fields left zero are
// left out, and take the server's defaults.
type CreateRequest struct {
	Name           string            `json:"name"`
	Backing        *Backing          `json:"backing,omitempty"`
	Regions        []string          `json:"regions,omitempty"`
	Quota          QuotaMap          `json:"quota,omitempty"`
	ModelBudgetUSD string            `json:"model_budget_usd,omitempty"`
	Address        *AddressWant      `json:"address"`
	MaxBlocks      int               `json:"max_blocks,omitempty"`
	Egress         *Egress           `json:"egress,omitempty"`
	GrantPolicy    map[string]string `json:"grant_policy,omitempty"`
	Control        string            `json:"control,omitempty"`
	AgentPolicy    *AgentPolicy      `json:"agent_policy,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
}

// Patch is the body of a PATCH (SV-9). Only the keys present are sent,
// so that a field left out is unchanged and a null one, where the API
// allows it, goes back to inheriting.
type Patch map[string]any

// GrowRequest is the body of :grow.
type GrowRequest struct {
	Block          string `json:"block,omitempty"`
	ToPrefixLength int    `json:"to_prefix_length"`
	AddBlock       bool   `json:"add_block,omitempty"`
}

// Operation is a long-running operation, at /silo/v1/operations/{id}.
type Operation struct {
	Name       string    `json:"name"`
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Target     string    `json:"target"`
	TargetID   string    `json:"target_id"`
	State      string    `json:"state"`
	Done       bool      `json:"done"`
	Error      *Error    `json:"error,omitempty"`
	CreateTime time.Time `json:"create_time"`
	UpdateTime time.Time `json:"update_time"`
}

// The states of a delete operation.
const (
	OpDeleting     = "DELETING"
	OpDeleteFailed = "DELETE_FAILED"
	OpDone         = "DONE"
)

// DeleteResult is what a delete returns: the operation, or, for a
// validate_only call, the silos it would delete.
type DeleteResult struct {
	Operation
	ValidateOnly bool     `json:"validate_only,omitempty"`
	Deletes      []string `json:"deletes,omitempty"`
}

// Member maps a front-door identity to a silo (SV-25).
type Member struct {
	Identity   string    `json:"identity"`
	Silo       string    `json:"silo"`
	Path       string    `json:"path"`
	UpdateTime time.Time `json:"update_time"`
}

// AuditRecord is one audit record (SV-26).
type AuditRecord struct {
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
	Silo struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	} `json:"silo"`
	Actor struct {
		Silo   string `json:"silo"`
		Member string `json:"member,omitempty"`
	} `json:"actor"`
	Via       string         `json:"via,omitempty"`
	Kind      string         `json:"kind"`
	RequestID string         `json:"request_id,omitempty"`
	Result    string         `json:"result"`
	Fields    map[string]any `json:"fields,omitempty"`
}

// AuditQuery narrows an audit read.
type AuditQuery struct {
	Since   time.Time
	Kind    string // a kind, or a prefix ending in *
	Subtree bool
	// Limit stops after this many records; 0 reads them all.
	Limit int
}

// FacadeRequest is a facade call to price, for quota:preview.
type FacadeRequest struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body,omitempty"`
}

// Allocation is an allocation change to price, for quota:preview.
type Allocation struct {
	Silo           string   `json:"silo"`
	Quota          QuotaMap `json:"quota,omitempty"`
	ModelBudgetUSD string   `json:"model_budget_usd,omitempty"`
}

// PreviewRequest is the body of quota:preview (SV-10): one of the two.
type PreviewRequest struct {
	FacadeRequest *FacadeRequest `json:"facade_request,omitempty"`
	Allocation    *Allocation    `json:"allocation,omitempty"`
}

// ChargeLine is one charge: who pays, for what.
type ChargeLine struct {
	Payer  string      `json:"payer"`
	Home   string      `json:"home,omitempty"`
	Via    string      `json:"via,omitempty"`
	Kind   string      `json:"kind"`
	Class  string      `json:"class"`
	Region string      `json:"region"`
	Amount json.Number `json:"amount"`
}

// PreviewLevel is one silo's free amount of one class, before and
// after.
type PreviewLevel struct {
	Silo       string      `json:"silo"`
	Kind       string      `json:"kind"`
	Class      string      `json:"class"`
	Region     string      `json:"region"`
	FreeBefore json.Number `json:"free_before"`
	FreeAfter  json.Number `json:"free_after"`
	Fits       bool        `json:"fits"`
}

// PreviewResponse is quota:preview's answer.
type PreviewResponse struct {
	Charges []ChargeLine   `json:"charges"`
	Levels  []PreviewLevel `json:"levels"`
	Fits    bool           `json:"fits"`
	Refusal *Error         `json:"refusal,omitempty"`
}
