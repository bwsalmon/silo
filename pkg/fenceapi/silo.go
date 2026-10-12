package fenceapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/ids"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/ledger/address"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

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
	GrantPolicy   Owned[GrantPolicy]                `json:"grant_policy"`
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

// Facade is where the silo's compute/v1 facade is.
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

// ModelBudget is a view of the silo's model/usd ledger row.
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

// Delegated is one of a silo's children's blocks.
type Delegated struct {
	Range string `json:"range"`
	Child string `json:"child"`
}

// OwnRange is a range one of the silo's subnets holds.
type OwnRange struct {
	Range  string `json:"range"`
	Subnet string `json:"subnet"`
}

// Owned is a policy as the silo set it and as it is in effect.
type Owned[T any] struct {
	Own       T `json:"own"`
	Effective T `json:"effective"`
}

// Control is the control policy, with where its effective value comes
// from.
type Control struct {
	Own           string `json:"own"`
	Effective     string `json:"effective"`
	EffectiveFrom string `json:"effective_from"`
}

// OverCommit is a class and region the silo holds more of than its
// grant.
type OverCommit struct {
	Class  string `json:"class"`
	Region string `json:"region"`
}

func jsonAmount(a int64) json.Number { return json.Number(ledger.FormatAmount(a)) }

// usd writes micro-dollars with at least two places: "200.00".
func usd(a int64) string {
	s := ledger.FormatAmount(a)
	whole, frac, _ := strings.Cut(s, ".")
	for len(frac) < 2 {
		frac += "0"
	}
	return whole + "." + frac
}

// object builds the silo object for the last silo of chain.
func (s *Server) object(ctx context.Context, tx store.Tx, chain []*tree.Silo) (*Silo, error) {
	t := chain[len(chain)-1]
	out := &Silo{
		ID: t.ID, Name: t.Name, Path: t.Path, Depth: t.Depth,
		Backing:       Backing{Kind: t.Backing},
		Facade:        Facade{Kind: "gce", Project: "silo-" + t.ID, Endpoint: s.opts.FacadeEndpoint, NamePrefix: ids.NamePrefix(t.ID)},
		Quota:         map[string]map[string]QuotaAmount{},
		State:         t.State,
		OverCommitted: []OverCommit{},
		CreateTime:    t.Created,
		Labels:        t.Labels,
	}
	if !t.IsRoot() {
		p := t.Parent
		out.Parent = &p
	}
	owner, err := ownerAgent(ctx, tx, "silo", t.ID)
	if err != nil {
		return nil, err
	}
	if owner != "" {
		out.OwnerAgent = &owner
	}
	if out.Regions, err = ledger.Regions(ctx, tx, t.ID); err != nil {
		return nil, err
	}
	if out.Regions == nil {
		out.Regions = []string{}
	}
	rows, err := ledger.Quota(ctx, tx, t.ID)
	if err != nil {
		return nil, err
	}
	period, err := ledger.BudgetPeriod(ctx, tx)
	if err != nil {
		return nil, err
	}
	out.ModelBudget = ModelBudget{Period: period, GrantedUSD: usd(0), DelegatedUSD: usd(0), UsedUSD: usd(0)}
	for _, r := range rows {
		if r.OverCommitted {
			out.OverCommitted = append(out.OverCommitted, OverCommit{Class: className(r.Key), Region: r.Region})
		}
		if r.Key == ledger.ModelUSD {
			out.ModelBudget.GrantedUSD, out.ModelBudget.DelegatedUSD, out.ModelBudget.UsedUSD = usd(r.Granted), usd(r.Delegated), usd(r.Used)
			continue
		}
		if r.Kind == ledger.ModelUSD.Kind || (r.Granted == 0 && r.Delegated == 0 && r.Used == 0) {
			continue
		}
		if out.Quota[r.Region] == nil {
			out.Quota[r.Region] = map[string]QuotaAmount{}
		}
		out.Quota[r.Region][className(r.Key)] = QuotaAmount{Granted: jsonAmount(r.Granted), Delegated: jsonAmount(r.Delegated), Used: jsonAmount(r.Used)}
	}
	if out.Address, err = addressOf(ctx, tx, t); err != nil {
		return nil, err
	}
	own, err := readPolicies(ctx, tx, t.ID)
	if err != nil {
		return nil, err
	}
	eff, err := effectiveOf(ctx, tx, chain, nil)
	if err != nil {
		return nil, err
	}
	out.Egress = Owned[Egress]{Own: own.Egress, Effective: eff.Egress}
	out.GrantPolicy = Owned[GrantPolicy]{Own: own.Grant, Effective: eff.Grant}
	out.AgentPolicy = Owned[AgentPolicy]{Own: own.Agent, Effective: eff.Agent}
	out.Control = Control{Own: own.Control, Effective: eff.Control, EffectiveFrom: eff.ControlFrom}
	return out, nil
}

// className is how the quota maps name a class: its name, for the
// built-in gce kind, and kind/name for any other.
func className(k ledger.Key) string {
	if k.Kind == "gce" {
		return k.Class
	}
	return k.Kind + "/" + k.Class
}

func addressOf(ctx context.Context, tx store.Tx, t *tree.Silo) (Address, error) {
	a := Address{Blocks: []string{}, Delegated: []Delegated{}, OwnRanges: []OwnRange{}}
	bs, err := address.Blocks(ctx, tx, t.ID)
	if err != nil {
		return a, err
	}
	for _, b := range bs {
		a.Blocks = append(a.Blocks, b.String())
	}
	if a.MaxBlocks, err = address.BlockCap(ctx, tx, t.ID); err != nil {
		return a, err
	}
	m, err := address.Map(ctx, tx, t.ID)
	if err != nil {
		return a, err
	}
	for _, r := range m {
		switch r.State {
		case address.StateDelegated:
			who := r.Holder
			if c, err := tree.Get(ctx, tx, r.Holder); err == nil {
				who = c.Path
			}
			a.Delegated = append(a.Delegated, Delegated{Range: r.Range.String(), Child: who})
		case address.StateOwn:
			a.OwnRanges = append(a.OwnRanges, OwnRange{Range: r.Range.String(), Subnet: address.ObjectName(t.ID, r.Holder)})
		}
	}
	return a, nil
}

// subtree fills in children recursively.
func (s *Server) subtree(ctx context.Context, tx store.Tx, chain []*tree.Silo, o *Silo) error {
	kids, err := tree.Children(ctx, tx, o.ID)
	if err != nil {
		return err
	}
	o.Children = []*Silo{}
	for _, k := range kids {
		cc := append(append([]*tree.Silo{}, chain...), k)
		ko, err := s.object(ctx, tx, cc)
		if err != nil {
			return err
		}
		if err := s.subtree(ctx, tx, cc, ko); err != nil {
			return err
		}
		o.Children = append(o.Children, ko)
	}
	return nil
}

func (s *Server) describe(q *request, ref string) {
	ctx := q.r.Context()
	var out *Silo
	err := s.tx(ctx, func(tx store.Tx) error {
		chain, _, err := s.resolve(ctx, tx, &q.caller, ref, "describe")
		if err != nil {
			return err
		}
		if out, err = s.object(ctx, tx, chain); err != nil {
			return err
		}
		if v := q.r.URL.Query().Get("view"); v == "subtree" {
			return s.subtree(ctx, tx, chain, out)
		} else if v != "" && v != "basic" {
			return errorf(InvalidArgument, "view %q: want subtree", v)
		}
		return nil
	})
	if err != nil {
		writeError(q.w, err)
		return
	}
	writeJSON(q.w, http.StatusOK, out)
}

func (s *Server) children(q *request, ref string) {
	ctx := q.r.Context()
	offset, size, err := page(q)
	if err != nil {
		writeError(q.w, err)
		return
	}
	out := struct {
		Silos         []*Silo `json:"silos"`
		NextPageToken string  `json:"next_page_token,omitempty"`
	}{Silos: []*Silo{}}
	err = s.tx(ctx, func(tx store.Tx) error {
		chain, _, err := s.resolve(ctx, tx, &q.caller, ref, "children")
		if err != nil {
			return err
		}
		kids, err := tree.Children(ctx, tx, chain[len(chain)-1].ID)
		if err != nil {
			return err
		}
		more := len(kids) > offset+size
		kids = kids[min(offset, len(kids)):min(offset+size, len(kids))]
		for _, k := range kids {
			o, err := s.object(ctx, tx, append(append([]*tree.Silo{}, chain...), k))
			if err != nil {
				return err
			}
			out.Silos = append(out.Silos, o)
		}
		out.NextPageToken = nextToken(offset, size, more)
		return nil
	})
	if err != nil {
		writeError(q.w, err)
		return
	}
	writeJSON(q.w, http.StatusOK, out)
}

// QuotaMap is the quota of a create or PATCH: region ("global" for a
// global class) to class name to amount.
type QuotaMap map[string]map[string]json.Number

// amounts reads a QuotaMap against the class table. A class name with
// "@" or "/" is reserved for custom classes; one the table doesn't
// have is INVALID_ARGUMENT.
func amounts(ctx context.Context, tx store.Tx, m QuotaMap) ([]ledger.Amount, error) {
	classes, err := ledger.Classes(ctx, tx)
	if err != nil {
		return nil, err
	}
	byName := map[string][]ledger.Class{}
	for _, c := range classes {
		if c.Kind == ledger.ModelUSD.Kind {
			continue // the model budget is model_budget_usd
		}
		byName[c.Name] = append(byName[c.Name], c)
	}
	var regions []string
	for r := range m {
		regions = append(regions, r)
	}
	sort.Strings(regions)
	var out []ledger.Amount
	for _, region := range regions {
		var names []string
		for n := range m[region] {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, name := range names {
			bad := func(format string, args ...any) error {
				return errorf(InvalidArgument, format, args...).with("class", name).with("region", region)
			}
			if strings.ContainsAny(name, "@/") {
				return nil, bad("quota class %q: names with @ or / are reserved for custom allocators", name)
			}
			cs := byName[name]
			if len(cs) != 1 {
				return nil, bad("unknown quota class %q", name)
			}
			a, err := ledger.ParseAmount(string(m[region][name]))
			if err != nil {
				return nil, bad("quota %s %s: %q is not a non-negative amount", region, name, m[region][name])
			}
			out = append(out, ledger.Amount{Key: ledger.Key{Kind: cs[0].Kind, Class: name, Region: region}, Amount: a})
		}
	}
	return out, nil
}

// chargeLines turns a ledger preview's payments into charge lines.
func chargeLines(ctx context.Context, tx store.Tx, ps []ledger.Payment, home, via string) []ChargeLine {
	out := []ChargeLine{}
	for _, p := range ps {
		payer := p.Payer
		if t, err := tree.Get(ctx, tx, p.Payer); err == nil {
			payer = t.Path
		}
		out = append(out, ChargeLine{Payer: payer, Home: home, Via: via, Kind: p.Kind, Class: p.Class, Region: p.Region, Amount: jsonAmount(p.Amount)})
	}
	return out
}
