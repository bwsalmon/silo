package gce

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/bwsalmon/silo/pkg/facade/gcp/gce/filter"
)

var (
	treesMu sync.Mutex
	trees   = map[string]pathTree{}
)

func responseTree(kind string) (pathTree, bool) {
	treesMu.Lock()
	defer treesMu.Unlock()
	if t, ok := trees[kind]; ok {
		return t, true
	}
	paths, ok := responseFields[kind]
	if !ok {
		return nil, false
	}
	t := treeOf(paths)
	trees[kind] = t
	return t, true
}

// shape builds the response for obj, a GCE object of kind: its
// allowlisted fields only, rewritten for the tenant. A kind with no
// allowlist is never shown (SV-63).
func (q *request) shape(kind string, obj any) (map[string]any, error) {
	t, ok := responseTree(kind)
	if !ok {
		return nil, unavailable("silo: no response allowlist for %s", kind)
	}
	m, err := toMap(obj)
	if err != nil {
		return nil, err
	}
	out, _ := prune(m, t).(map[string]any)
	out["kind"] = kind
	q.n.out(out, "")
	return out, nil
}

func toMap(obj any) (map[string]any, error) {
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, unavailable("silo: encoding a GCE object: %v", err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, unavailable("silo: decoding a GCE object: %v", err)
	}
	return m, nil
}

// MaxResults is GCE's cap on maxResults, and its default.
const MaxResults = 500

// page reads maxResults and pageToken. Tokens are the facade's own and
// opaque: an offset into the facade's filtered list.
func (q *request) page() (offset, size int, err error) {
	size = MaxResults
	v := q.r.URL.Query()
	if s := v.Get("maxResults"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 || n > MaxResults {
			return 0, 0, invalid("Invalid value for field 'maxResults': '%s'. Must be in the range [0, %d].", s, MaxResults)
		}
		if n > 0 {
			size = n
		}
	}
	if t := v.Get("pageToken"); t != "" {
		b, err := base64.RawURLEncoding.DecodeString(t)
		rest, ok := strings.CutPrefix(string(b), "silo:")
		n, err2 := strconv.Atoi(rest)
		if err != nil || err2 != nil || !ok || n < 0 {
			return 0, 0, invalid("Invalid value for field 'pageToken': '%s'.", t)
		}
		offset = n
	}
	return offset, size, nil
}

func pageToken(next int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("silo:%d", next)))
}

// selfLink is the facade's URL for the call's own path.
func (s *Server) selfLink(q *request) string {
	return s.opts.Endpoint + "projects/" + q.call.Project + strings.TrimPrefix(q.rest, "projects/"+q.call.Project)
}

// query reads filter and orderBy (gcp-backend.md, "Lists and
// filters", SV-74). The facade evaluates both itself, on the tenant's
// view of the items, after names are mapped back; neither is ever
// passed to GCE, so a filter can't reach another silo's objects, nor
// see a real name or a silo label (call 26).
func (q *request) query() (*filter.Expr, filter.Order, error) {
	v := q.r.URL.Query()
	f, err := filter.Parse(v.Get("filter"))
	if err != nil {
		return nil, nil, invalid("Invalid value for field 'filter': '%s'. Invalid list filter expression: %v.", v.Get("filter"), err)
	}
	o, err := filter.ParseOrder(v.Get("orderBy"))
	if err != nil {
		return nil, nil, invalid("Invalid value for field 'orderBy': '%s'. %v.", v.Get("orderBy"), err)
	}
	return f, o, nil
}

// selected is items that match the call's filter, in its order.
func selected[T any](q *request, items []T, obj func(T) map[string]any) ([]T, error) {
	f, o, err := q.query()
	if err != nil {
		return nil, err
	}
	var out []T
	for _, it := range items {
		if f.Match(obj(it)) {
			out = append(out, it)
		}
	}
	if o != nil {
		sort.SliceStable(out, func(i, j int) bool { return o.Less(obj(out[i]), obj(out[j])) })
	}
	return out, nil
}

// list is a list response of items, filtered, ordered and paged.
func (s *Server) list(q *request, listKind string, items []map[string]any) (any, error) {
	items, err := selected(q, items, func(m map[string]any) map[string]any { return m })
	if err != nil {
		return nil, err
	}
	off, size, err := q.page()
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"kind":     listKind,
		"id":       "projects/" + q.call.Project + strings.TrimPrefix(q.rest, "projects/"+q.call.Project),
		"selfLink": s.selfLink(q),
		"items":    []map[string]any{},
	}
	if off < len(items) {
		end := min(off+size, len(items))
		out["items"] = items[off:end]
		if end < len(items) {
			out["nextPageToken"] = pageToken(end)
		}
	}
	return out, nil
}

// scoped is one item of an aggregated list, with its scope.
type scoped struct {
	scope string // zones/z, regions/r
	item  map[string]any
}

// aggregated is an aggregatedList response: items grouped by scope
// under field, filtered, ordered and paged across all of them.
func (s *Server) aggregated(q *request, listKind, field string, items []scoped) (any, error) {
	items, err := selected(q, items, func(it scoped) map[string]any { return it.item })
	if err != nil {
		return nil, err
	}
	off, size, err := q.page()
	if err != nil {
		return nil, err
	}
	groups := map[string]any{}
	end := len(items)
	if off < len(items) {
		end = min(off+size, len(items))
		for _, it := range items[off:end] {
			g, _ := groups[it.scope].(map[string]any)
			if g == nil {
				g = map[string]any{field: []map[string]any{}}
				groups[it.scope] = g
			}
			g[field] = append(g[field].([]map[string]any), it.item)
		}
	}
	out := map[string]any{
		"kind":     listKind,
		"id":       "projects/" + q.call.Project + "/aggregated/" + q.call.Collection,
		"selfLink": s.selfLink(q),
		"items":    groups,
	}
	if off < len(items) && end < len(items) {
		out["nextPageToken"] = pageToken(end)
	}
	return out, nil
}
