package gce

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"google.golang.org/api/googleapi"
)

// MaxNameLength is GCE's limit on a resource name.
const MaxNameLength = 63

var nameRE = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

// ValidName reports whether name is a valid GCE resource name: RFC 1035,
// lower case, at most 63 characters.
func ValidName(name string) bool {
	return len(name) <= MaxNameLength && nameRE.MatchString(name)
}

// SelfLinkBase is the prefix of every selfLink GCE writes.
const SelfLinkBase = "https://www.googleapis.com/compute/v1/"

// Ref is a reference to a GCE resource, parsed from a full URL, a
// partial path or a bare name.
type Ref struct {
	Project string
	Zone    string
	Region  string
	// Global is set for a path under projects/<p>/global.
	Global bool
	// Collection is the resource type, such as instances or
	// machineTypes. Empty for a bare name.
	Collection string
	Name       string
}

// ParseRef parses a GCE resource reference: a full URL
// (https://www.googleapis.com/compute/v1/..., or compute.googleapis.com,
// or any host followed by /compute/v1/), a path starting at projects/,
// zones/, regions/ or global/, or a bare name. Fields the reference
// doesn't give are left empty.
func ParseRef(s string) Ref {
	if i := strings.Index(s, "/compute/v1/"); i >= 0 {
		s = s[i+len("/compute/v1/"):]
	}
	s = strings.Trim(s, "/")
	parts := strings.Split(s, "/")
	var r Ref
	if len(parts) == 1 {
		r.Name = parts[0]
		return r
	}
	for len(parts) >= 2 {
		switch parts[0] {
		case "projects":
			r.Project = parts[1]
			parts = parts[2:]
			continue
		case "zones":
			r.Zone = parts[1]
			parts = parts[2:]
			continue
		case "regions":
			r.Region = parts[1]
			parts = parts[2:]
			continue
		case "global":
			r.Global = true
			parts = parts[1:]
			continue
		}
		r.Collection, r.Name = parts[0], parts[1]
		return r
	}
	if len(parts) == 1 {
		r.Name = parts[0]
	}
	return r
}

// LastSegment is the last path segment of a URL or path: the name of
// the resource it names.
func LastSegment(s string) string {
	return s[strings.LastIndex(s, "/")+1:]
}

// ZoneRegion is the region of zone: us-central1-a is in us-central1.
func ZoneRegion(zone string) string {
	if i := strings.LastIndex(zone, "-"); i > 0 {
		return zone[:i]
	}
	return zone
}

// ListFilter is what silod asks GCE to filter a list by: labels that
// must all match exactly, and a name prefix. It is rendered as GCE's
// regular-expression filter syntax, (field eq "re") terms joined by a
// space, which GCE ANDs; ParseListFilter reads back exactly that.
type ListFilter struct {
	Labels     map[string]string
	NamePrefix string
}

// IsZero reports whether f matches everything.
func (f ListFilter) IsZero() bool { return len(f.Labels) == 0 && f.NamePrefix == "" }

// String is f in GCE's filter syntax, for the list call's filter
// parameter.
func (f ListFilter) String() string {
	var terms []string
	keys := make([]string, 0, len(f.Labels))
	for k := range f.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		terms = append(terms, fmt.Sprintf("(labels.%s eq %q)", k, regexp.QuoteMeta(f.Labels[k])))
	}
	if f.NamePrefix != "" {
		terms = append(terms, fmt.Sprintf("(name eq %q)", regexp.QuoteMeta(f.NamePrefix)+".*"))
	}
	return strings.Join(terms, " ")
}

var filterTermRE = regexp.MustCompile(`^\((labels\.[a-z][-_a-z0-9]*|name) eq ("(?:[^"\\]|\\.)*")\)`)

// ParseListFilter reads a filter written by ListFilter.String. Anything
// else is an error: the fake serves only what silod sends.
func ParseListFilter(s string) (ListFilter, error) {
	var f ListFilter
	s = strings.TrimSpace(s)
	for s != "" {
		m := filterTermRE.FindStringSubmatch(s)
		if m == nil {
			return ListFilter{}, fmt.Errorf("unsupported filter at %q", s)
		}
		var v string
		if _, err := fmt.Sscanf(m[2], "%q", &v); err != nil {
			return ListFilter{}, fmt.Errorf("filter value %s: %v", m[2], err)
		}
		if key, ok := strings.CutPrefix(m[1], "labels."); ok {
			lit, ok := unquoteMeta(v)
			if !ok {
				return ListFilter{}, fmt.Errorf("label filter %s is not a literal", m[2])
			}
			if f.Labels == nil {
				f.Labels = map[string]string{}
			}
			f.Labels[key] = lit
		} else {
			prefix, ok := strings.CutSuffix(v, ".*")
			lit, ok2 := unquoteMeta(prefix)
			if !ok || !ok2 {
				return ListFilter{}, fmt.Errorf("name filter %s is not a prefix", m[2])
			}
			f.NamePrefix = lit
		}
		s = strings.TrimSpace(s[len(m[0]):])
	}
	return f, nil
}

// unquoteMeta undoes regexp.QuoteMeta, failing on an unescaped
// metacharacter.
func unquoteMeta(s string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			i++
			b.WriteByte(s[i])
			continue
		}
		if strings.IndexByte(`\.+*?()|[]{}^$`, c) >= 0 {
			return "", false
		}
		b.WriteByte(c)
	}
	return b.String(), true
}

// Match reports whether an object with name and labels passes f.
func (f ListFilter) Match(name string, labels map[string]string) bool {
	if !strings.HasPrefix(name, f.NamePrefix) {
		return false
	}
	for k, v := range f.Labels {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// GCE error reasons silod acts on.
const (
	ReasonNotFound        = "notFound"
	ReasonAlreadyExists   = "alreadyExists"
	ReasonInvalid         = "invalid"
	ReasonForbidden       = "forbidden"
	ReasonQuotaExceeded   = "quotaExceeded"
	ReasonConditionNotMet = "conditionNotMet"
	ReasonResourceInUse   = "resourceInUseByAnotherResource"
	ReasonNotReady        = "resourceNotReady"
	ReasonBackendError    = "backendError"
)

// Operation error codes GCE puts in a failed operation's error.errors[].
const (
	CodeQuotaExceeded = "QUOTA_EXCEEDED"
	CodeStockout      = "ZONE_RESOURCE_POOL_EXHAUSTED"
	CodeInternal      = "INTERNAL_ERROR"
)

// NewError is a GCE-shaped error: HTTP code, one error item with reason
// and message.
func NewError(code int, reason, format string, args ...any) *googleapi.Error {
	msg := fmt.Sprintf(format, args...)
	return &googleapi.Error{
		Code:    code,
		Message: msg,
		Errors:  []googleapi.ErrorItem{{Reason: reason, Message: msg}},
	}
}

// NotFound is GCE's 404 for the resource at path.
func NotFound(path string) *googleapi.Error {
	return NewError(http.StatusNotFound, ReasonNotFound, "The resource '%s' was not found", path)
}

// Reason is the reason of err's first error item, or "" if err isn't a
// GCE error.
func Reason(err error) string {
	var ge *googleapi.Error
	if errors.As(err, &ge) && len(ge.Errors) > 0 {
		return ge.Errors[0].Reason
	}
	return ""
}

// Code is err's HTTP status code, or 0 if err isn't a GCE error.
func Code(err error) int {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	return 0
}

// IsNotFound reports whether err is GCE's 404.
func IsNotFound(err error) bool { return Code(err) == http.StatusNotFound }

// IsAlreadyExists reports whether err is GCE's 409 alreadyExists.
func IsAlreadyExists(err error) bool { return Reason(err) == ReasonAlreadyExists }
