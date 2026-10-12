package fenceapi

import (
	"context"
	"encoding/json"

	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// FacadeRequest is the facade-request form of quota:preview: a facade
// call, as the tenant would make it.
type FacadeRequest struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body"`
}

// Pricer prices a facade request for quota:preview: which silo it
// would charge, and what. The facade supplies it; with none, the
// facade-request form is UNIMPLEMENTED.
type Pricer interface {
	Price(ctx context.Context, tx store.Tx, caller Caller, req FacadeRequest) (silo string, amounts []ledger.Amount, err error)
}

// PreviewRequest is the body of quota:preview (SV-10): one of the two
// forms.
type PreviewRequest struct {
	FacadeRequest *FacadeRequest `json:"facade_request"`
	Allocation    *struct {
		Silo           string   `json:"silo"`
		Quota          QuotaMap `json:"quota"`
		ModelBudgetUSD string   `json:"model_budget_usd"`
	} `json:"allocation"`
}

// PreviewLevel is one key's free amount at one silo, before and after.
type PreviewLevel struct {
	Silo       string      `json:"silo"`
	Kind       string      `json:"kind"`
	Class      string      `json:"class"`
	Region     string      `json:"region"`
	FreeBefore json.Number `json:"free_before"`
	FreeAfter  json.Number `json:"free_after"`
	Fits       bool        `json:"fits"`
}

// PreviewResponse is what quota:preview returns. Nothing changes.
type PreviewResponse struct {
	Charges []ChargeLine   `json:"charges"`
	Levels  []PreviewLevel `json:"levels"`
	Fits    bool           `json:"fits"`
	Refusal *Error         `json:"refusal,omitempty"`
}

func (s *Server) preview(q *request) (any, error) {
	ctx := q.r.Context()
	var req PreviewRequest
	if err := decode(q, &req); err != nil {
		return nil, err
	}
	if (req.FacadeRequest == nil) == (req.Allocation == nil) {
		return nil, errorf(InvalidArgument, "give one of facade_request or allocation")
	}
	var out *PreviewResponse
	err := s.tx(ctx, func(tx store.Tx) error {
		var p *ledger.Preview
		var home *tree.Silo
		via := ""
		if a := req.Allocation; a != nil {
			ref := a.Silo
			if len(ref) > 0 && ref[0] == '/' {
				ref = ref[1:]
			}
			chain, rel, err := s.resolve(ctx, tx, &q.caller, ref, "preview")
			if err != nil {
				return err
			}
			home = chain[len(chain)-1]
			if home.IsRoot() {
				return errorf(InvalidArgument, "the root's quota is its capacity, which is set rather than granted")
			}
			if rel == relSelf {
				return errorf(PermissionDenied, "only an ancestor may change %s's allocation", home.Path)
			}
			as, err := amounts(ctx, tx, a.Quota)
			if err != nil {
				return err
			}
			if a.ModelBudgetUSD != "" {
				m, err := ledger.ParseAmount(a.ModelBudgetUSD)
				if err != nil {
					return err
				}
				as = append(as, ledger.Amount{Key: ledger.ModelUSD, Amount: m})
			}
			via = viaOf(chain, q.caller)
			if p, err = ledger.PreviewGrant(ctx, tx, home.ID, as); err != nil {
				return err
			}
		} else {
			if s.opts.Pricer == nil {
				return errorf(Unimplemented, "the facade-request form of quota:preview is not available in this deployment")
			}
			id, as, err := s.opts.Pricer.Price(ctx, tx, q.caller, *req.FacadeRequest)
			if err != nil {
				return err
			}
			chain, _, err := s.resolve(ctx, tx, &q.caller, "-"+id, "preview")
			if err != nil {
				return err
			}
			home = chain[len(chain)-1]
			via = ledger.ViaFacade
			if p, err = ledger.PreviewCharge(ctx, tx, home.ID, as); err != nil {
				return err
			}
		}
		out = &PreviewResponse{Charges: chargeLines(ctx, tx, p.Charges, home.Path, via), Levels: []PreviewLevel{}, Fits: p.Refusal == nil}
		for _, l := range p.Levels {
			out.Levels = append(out.Levels, PreviewLevel{Silo: l.Path, Kind: l.Kind, Class: l.Class, Region: l.Region,
				FreeBefore: jsonAmount(l.FreeBefore), FreeAfter: jsonAmount(l.FreeAfter), Fits: l.FreeAfter >= 0})
		}
		if p.Refusal != nil {
			out.Refusal = asError(ctx, tx, p.Refusal)
		}
		return nil
	})
	return out, err
}
