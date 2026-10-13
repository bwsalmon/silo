package facade

import (
	"sort"
	"strconv"
	"strings"
)

// Fields is an allowlist as a tree of JSON keys (facades.md, "The
// pipeline every facade shares", steps 5 and 6). A nil subtree allows
// everything below its key.
type Fields map[string]Fields

// FieldDenied refuses a request field outside its method's allowlist,
// set to anything but its default.
type FieldDenied struct {
	Path string // the field, with list indexes: networkInterfaces[0].accessConfigs
}

func (e *FieldDenied) Error() string { return "silo: field " + e.Path + " is not allowed" }
func (*FieldDenied) refusal()        {}

// AllowFields builds the tree of paths, each a JSON path from the
// body's top: "name", "labels" (the whole map),
// "networkInterfaces[].subnetwork".
func AllowFields(paths []string) Fields {
	t := Fields{}
	for _, p := range paths {
		cur := t
		parts := strings.Split(strings.ReplaceAll(p, "[]", ""), ".")
		for i, part := range parts {
			next, seen := cur[part]
			if i == len(parts)-1 {
				cur[part] = nil // the whole subtree
				break
			}
			if seen && next == nil {
				break // already allowed whole
			}
			if next == nil {
				next = Fields{}
				cur[part] = next
			}
			cur = next
		}
	}
	return t
}

// Prune keeps only what t allows of v, a decoded JSON value: a
// response built from allowlisted fields only.
func (t Fields) Prune(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			sub, ok := t[k]
			if !ok {
				continue
			}
			if sub == nil {
				out[k] = val
			} else {
				out[k] = sub.Prune(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = t.Prune(e)
		}
		return out
	}
	return v
}

// CheckFields refuses, as *FieldDenied, the first field of body, a
// decoded JSON request, that t doesn't allow and that is set to
// anything but its default.
func CheckFields(body any, t Fields) error { return checkFields(body, t, "") }

func checkFields(body any, t Fields, path string) error {
	switch x := body.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			val := x[k]
			p := k
			if path != "" {
				p = path + "." + k
			}
			sub, ok := t[k]
			switch {
			case !ok:
				if !IsDefault(val) {
					return &FieldDenied{Path: p}
				}
			case sub != nil:
				if err := checkFields(val, sub, p); err != nil {
					return err
				}
			}
		}
	case []any:
		for i, e := range x {
			if err := checkFields(e, t, path+"["+strconv.Itoa(i)+"]"); err != nil {
				return err
			}
		}
	}
	return nil
}

// IsDefault reports whether v is a JSON field's default: null, false,
// 0, "", or an empty list or object, or an object of defaults.
func IsDefault(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case bool:
		return !x
	case float64:
		return x == 0
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		for _, e := range x {
			if !IsDefault(e) {
				return false
			}
		}
		return true
	}
	return false
}

// ReservedLabel reports whether a label key is in Silo's reserved
// namespace: one starting with silo, which only the facade sets. Each
// service knows where labels live in its own bodies.
func ReservedLabel(k string) bool { return strings.HasPrefix(k, "silo") }
