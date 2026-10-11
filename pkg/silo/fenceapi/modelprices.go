package fenceapi

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/bwsalmon/silo/pkg/silo/agents"
	"github.com/bwsalmon/silo/pkg/silo/ledger"
	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/tree"
)

// The model price table (agents.md, "Prices", SV-111): the release's,
// in package agents, with root members' overrides in model_prices. A
// model with no price can't be used.

// ModelPrice is one entry, in dollars per million tokens.
type ModelPrice struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Input      string `json:"input"`
	Output     string `json:"output"`
	CacheWrite string `json:"cache_write"`
	CacheRead  string `json:"cache_read"`
	// Source is "release" or "override".
	Source string `json:"source"`
}

func shownPrice(p agents.Price) ModelPrice {
	src := "release"
	if p.Override {
		src = "override"
	}
	return ModelPrice{Provider: p.Provider, Model: p.Model, Input: usd(p.Input), Output: usd(p.Output),
		CacheWrite: usd(p.CacheWrite), CacheRead: usd(p.CacheRead), Source: src}
}

func priceOverrides(ctx context.Context, tx store.Tx, where string, args ...any) ([]agents.Price, error) {
	q := `SELECT provider, model, input, output, cache_write, cache_read FROM model_prices`
	if where != "" {
		q += ` WHERE ` + where
	}
	rows, err := tx.Query(ctx, q+` ORDER BY provider, model`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []agents.Price
	for rows.Next() {
		p := agents.Price{Override: true}
		if err := rows.Scan(&p.Provider, &p.Model, &p.Input, &p.Output, &p.CacheWrite, &p.CacheRead); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// modelPrice is a model's price under provider: a root member's
// override, or the release's.
func modelPrice(ctx context.Context, tx store.Tx, provider, model string) (agents.Price, bool, error) {
	over, err := priceOverrides(ctx, tx, `provider = ? AND model = ?`, provider, model)
	if err != nil || len(over) > 0 {
		if len(over) > 0 {
			return over[0], true, err
		}
		return agents.Price{}, false, err
	}
	for _, p := range agents.ReleasePrices() {
		if p.Provider == provider && p.Model == model {
			return p, true, nil
		}
	}
	return agents.Price{}, false, nil
}

// ModelPriceOf is a model's price, for the model proxy and the
// adapters that meter against it.
func ModelPriceOf(ctx context.Context, tx store.Tx, provider, model string) (agents.Price, bool, error) {
	return modelPrice(ctx, tx, provider, model)
}

func (s *Server) listModelPrices(q *request) {
	ctx := q.r.Context()
	var out []ModelPrice
	err := s.tx(ctx, func(tx store.Tx) error {
		if _, err := tree.Get(ctx, tx, q.caller.Silo); err != nil {
			if errors.Is(err, tree.ErrNotFound) {
				return errorf(PermissionDenied, "the caller is no silo")
			}
			return err
		}
		over, err := priceOverrides(ctx, tx, "")
		if err != nil {
			return err
		}
		out = []ModelPrice{}
		for _, p := range agents.MergePrices(over) {
			out = append(out, shownPrice(p))
		}
		return nil
	})
	if err != nil {
		writeError(q.w, err)
		return
	}
	writeJSON(q.w, http.StatusOK, map[string]any{"model_prices": out})
}

// mustRoot refuses a caller that isn't the root: only root members may
// change the price table.
func mustRoot(ctx context.Context, tx store.Tx, c Caller) (*tree.Silo, error) {
	root, err := tree.Root(ctx, tx)
	if err != nil {
		return nil, err
	}
	if c.Silo != root.ID {
		return nil, errorf(PermissionDenied, "only the root's members may change model prices")
	}
	return root, nil
}

// setModelPrice is PUT /silo/v1/modelPrices/{provider}/{model}, with
// the four prices in dollars per million tokens.
func (s *Server) setModelPrice(q *request, provider, model string) (any, error) {
	ctx := q.r.Context()
	var req ModelPrice
	if err := decode(q, &req); err != nil {
		return nil, err
	}
	p := agents.Price{Provider: provider, Model: model, Override: true}
	if provider == "" || model == "" {
		return nil, errorf(InvalidArgument, "name a provider and a model")
	}
	for _, f := range []struct {
		name, v string
		dst     *int64
	}{{"input", req.Input, &p.Input}, {"output", req.Output, &p.Output}, {"cache_write", req.CacheWrite, &p.CacheWrite}, {"cache_read", req.CacheRead, &p.CacheRead}} {
		n, err := ledger.ParseAmount(f.v)
		if err != nil {
			return nil, errorf(InvalidArgument, "%s %q: want dollars per million tokens, such as 3.00", f.name, f.v).with("field", f.name)
		}
		*f.dst = n
	}
	out := shownPrice(p)
	err := s.tx(ctx, func(tx store.Tx) error {
		if _, err := mustRoot(ctx, tx, q.caller); err != nil {
			return err
		}
		if q.validateOnly {
			return errValidateOnly
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO model_prices (provider, model, input, output, cache_write, cache_read, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (provider, model) DO UPDATE SET input = excluded.input, output = excluded.output,
				cache_write = excluded.cache_write, cache_read = excluded.cache_read, updated_at = excluded.updated_at`,
			provider, model, p.Input, p.Output, p.CacheWrite, p.CacheRead, store.Millis(now)); err != nil {
			return err
		}
		return s.finish(ctx, tx, q, out)
	})
	if err != nil && !errors.Is(err, errValidateOnly) {
		return nil, err
	}
	return out, nil
}

// deleteModelPrice is DELETE /silo/v1/modelPrices/{provider}/{model}:
// the override goes, and the release's price, if any, applies again.
func (s *Server) deleteModelPrice(q *request, provider, model string) (any, error) {
	ctx := q.r.Context()
	out := map[string]any{"provider": provider, "model": model, "deleted": !q.validateOnly}
	err := s.tx(ctx, func(tx store.Tx) error {
		if _, err := mustRoot(ctx, tx, q.caller); err != nil {
			return err
		}
		var n int
		err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM model_prices WHERE provider = ? AND model = ?`, provider, model).Scan(&n)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if n == 0 {
			return errorf(NotFound, "no override of %s %s", provider, model)
		}
		if q.validateOnly {
			return errValidateOnly
		}
		if _, err := tx.Exec(ctx, `DELETE FROM model_prices WHERE provider = ? AND model = ?`, provider, model); err != nil {
			return err
		}
		return s.finish(ctx, tx, q, out)
	})
	if err != nil && !errors.Is(err, errValidateOnly) {
		return nil, err
	}
	return out, nil
}
