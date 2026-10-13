package docker

import (
	"encoding/json"
	"io"
	"net/http"
	goruntime "runtime"
	"sort"
	"strings"
)

// filterSet is Docker's filters parameter: a name to the values it was
// given.
type filterSet map[string][]string

// filters reads q's filters parameter, in either of Docker's forms
// ({"label": {"a=b": true}} or {"label": ["a=b"]}), and refuses a name
// not in known.
func filters(q *request, known ...string) (filterSet, error) {
	out := filterSet{}
	raw := q.query("filters")
	if raw == "" {
		return out, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, invalid("invalid filter: %v", err)
	}
	for k, v := range m {
		ok := false
		for _, n := range known {
			ok = ok || n == k
		}
		if !ok {
			return nil, invalid("invalid filter '%s'", k)
		}
		var set map[string]bool
		if json.Unmarshal(v, &set) == nil {
			for x, on := range set {
				if on {
					out[k] = append(out[k], x)
				}
			}
			sort.Strings(out[k])
			continue
		}
		var list []string
		if err := json.Unmarshal(v, &list); err != nil {
			return nil, invalid("invalid filter '%s'", k)
		}
		out[k] = list
	}
	return out, nil
}

// one is the filter's one value, if it has one.
func (f filterSet) one(k string) (string, bool) {
	if len(f[k]) == 0 {
		return "", false
	}
	return f[k][0], true
}

// labels reports whether labels match every label filter: a key, or
// key=value.
func (f filterSet) labels(labels map[string]string) bool {
	for _, l := range f["label"] {
		k, v, hasV := strings.Cut(l, "=")
		got, ok := labels[k]
		if !ok || (hasV && got != v) {
			return false
		}
	}
	return true
}

// match reports whether s matches one of the filter's values, by fn,
// or the filter isn't given.
func (f filterSet) match(k string, fn func(v string) bool) bool {
	if len(f[k]) == 0 {
		return true
	}
	for _, v := range f[k] {
		if fn(v) {
			return true
		}
	}
	return false
}

// jsonLines returns a function that writes one JSON object a line to
// w, flushing each, as Docker streams progress.
func jsonLines(w http.ResponseWriter) func(v any) {
	enc := json.NewEncoder(w)
	fl, _ := w.(http.Flusher)
	return func(v any) {
		_ = enc.Encode(v)
		if fl != nil {
			fl.Flush()
		}
	}
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// goarch is the host's architecture, as Docker names it.
func goarch() string { return goruntime.GOARCH }

// archName is the host's architecture as /info's Architecture names it.
func archName() string {
	switch goruntime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	}
	return goruntime.GOARCH
}

// discard drains and closes r.
func discard(r io.ReadCloser) {
	_, _ = io.Copy(io.Discard, r)
	_ = r.Close()
}
