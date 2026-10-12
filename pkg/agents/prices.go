package agents

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/bwsalmon/silo/pkg/ledger"
)

// Price is what one model costs, in micro-dollars per million tokens of
// each kind (agents.md, "Prices", SV-111).
type Price struct {
	Provider   string
	Model      string
	Input      int64
	Output     int64
	CacheWrite int64
	CacheRead  int64
	// Override is set for a root member's entry rather than the
	// release's.
	Override bool
}

//go:embed prices.json
var pricesJSON []byte

type priceFile struct {
	Prices []struct {
		Provider   string `json:"provider"`
		Model      string `json:"model"`
		Input      string `json:"input"`
		Output     string `json:"output"`
		CacheWrite string `json:"cache_write"`
		CacheRead  string `json:"cache_read"`
	} `json:"prices"`
}

var releasePrices = func() []Price {
	var f priceFile
	if err := json.Unmarshal(pricesJSON, &f); err != nil {
		panic("agents: prices.json: " + err.Error())
	}
	var out []Price
	for _, p := range f.Prices {
		pr := Price{Provider: p.Provider, Model: p.Model}
		for _, v := range []struct {
			s   string
			dst *int64
		}{{p.Input, &pr.Input}, {p.Output, &pr.Output}, {p.CacheWrite, &pr.CacheWrite}, {p.CacheRead, &pr.CacheRead}} {
			n, err := ledger.ParseAmount(v.s)
			if err != nil {
				panic(fmt.Sprintf("agents: prices.json: %s %s: %v", p.Provider, p.Model, err))
			}
			*v.dst = n
		}
		out = append(out, pr)
	}
	return out
}()

// ReleasePrices is the price table this release carries.
func ReleasePrices() []Price { return append([]Price(nil), releasePrices...) }

// MergePrices is the release's table with overrides laid over it,
// sorted by provider and model.
func MergePrices(overrides []Price) []Price {
	type key struct{ p, m string }
	m := map[key]Price{}
	for _, p := range releasePrices {
		m[key{p.Provider, p.Model}] = p
	}
	for _, p := range overrides {
		p.Override = true
		m[key{p.Provider, p.Model}] = p
	}
	out := make([]Price, 0, len(m))
	for _, p := range m {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Model < out[j].Model
	})
	return out
}
