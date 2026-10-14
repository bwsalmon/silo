// Package gcp is journey 2's Google Cloud side (design §11): the
// per-user, reusable service account each projects[] entry gets, the
// checks that the invoking user may set it up, creating, verifying and
// reusing it, waiting for IAM to propagate, and silo gc for accounts.
//
// Everything here runs as the invoking user's own gcloud login
// ({ambient: gcloud}, design §8.1), on the invoking machine only, through
// the small API interface: Google implements it against the real APIs
// and unit tests use a fake.
package gcp

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SpecVersion is the v of every spec silo writes.
const SpecVersion = 1

// Spec is the canonical description of one account (design §11.1). Its
// hash names the account, so two specs that differ in anything get two
// accounts.
type Spec struct {
	V       int
	Project string
	User    string
	Roles   []string
	// Conditions is always empty in v2.0 (open question 8): it is in the
	// hash from the start so that adding conditions later renames no
	// existing account.
	Conditions []string
}

// NewSpec builds the spec for a project, the invoking user's email and a
// role list, which it sorts and de-duplicates.
func NewSpec(project, user string, roles []string) Spec {
	set := map[string]bool{}
	var rs []string
	for _, r := range roles {
		if !set[r] {
			set[r] = true
			rs = append(rs, r)
		}
	}
	sort.Strings(rs)
	return Spec{V: SpecVersion, Project: project, User: strings.ToLower(user), Roles: rs, Conditions: []string{}}
}

// Canonical is the spec serialised as JSON with sorted keys and no
// insignificant whitespace: the bytes that are hashed.
func (s Spec) Canonical() []byte {
	roles, conds := s.Roles, s.Conditions
	if roles == nil {
		roles = []string{}
	}
	if conds == nil {
		conds = []string{}
	}
	// encoding/json writes map keys sorted; HTML escaping is off so the
	// bytes are plain JSON any implementation reproduces.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]any{
		"conditions": conds,
		"project":    s.Project,
		"roles":      roles,
		"user":       s.User,
		"v":          s.V,
	}); err != nil {
		panic(err) // strings and an int always marshal
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// Hash is the lowercase hex SHA-256 of Canonical, which the account's
// description carries as spec=.
func (s Spec) Hash() string {
	sum := sha256.Sum256(s.Canonical())
	return hex.EncodeToString(sum[:])
}

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// AccountID is silo- and the first 20 characters of the lowercase
// unpadded base32 of SHA-256(Canonical): 25 characters, a valid service
// account id.
func (s Spec) AccountID() string {
	sum := sha256.Sum256(s.Canonical())
	return "silo-" + strings.ToLower(b32.EncodeToString(sum[:]))[:20]
}

// Email is the account's email in its project.
func (s Spec) Email() string {
	return AccountEmail(s.AccountID(), s.Project)
}

// AccountEmail is a service account's email from its id and project.
func AccountEmail(id, project string) string {
	return id + "@" + project + ".iam.gserviceaccount.com"
}

// Validate refuses what v2.0 cannot set up: conditions (open question 8)
// and a spec missing its project, user or roles.
func (s Spec) Validate() error {
	switch {
	case s.V != SpecVersion:
		return fmt.Errorf("account spec version %d: this silo knows version %d", s.V, SpecVersion)
	case s.Project == "":
		return fmt.Errorf("account spec has no project")
	case s.User == "" || !strings.Contains(s.User, "@"):
		return fmt.Errorf("account spec user %q is not an email", s.User)
	case len(s.Roles) == 0:
		return fmt.Errorf("account spec for %s has no roles", s.Project)
	case len(s.Conditions) != 0:
		return fmt.Errorf("IAM Conditions are not supported yet (design open question 8): grants are project-level and unconditional")
	}
	for _, r := range s.Roles {
		if !validRole(r) {
			return fmt.Errorf("%q is not a role name (roles/x, projects/p/roles/x or organizations/o/roles/x)", r)
		}
	}
	return nil
}

func validRole(r string) bool {
	parts := strings.Split(r, "/")
	switch {
	case len(parts) == 2 && parts[0] == "roles":
	case len(parts) == 4 && (parts[0] == "projects" || parts[0] == "organizations") && parts[2] == "roles":
	default:
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
	}
	return true
}

// Limits GCP puts on an account's display name and description.
const (
	maxDisplayName = 100
	maxDescription = 256
)

// DisplayName is "silo: <user>", truncated to GCP's 100 characters.
func (s Spec) DisplayName() string {
	d := "silo: " + s.User
	if len(d) > maxDisplayName {
		d = d[:maxDisplayName]
	}
	return d
}

// timeFormat is how created= and used= are written: RFC 3339 in UTC to
// the second, because a ttl can be shorter than a day.
const timeFormat = "2006-01-02T15:04:05Z"

// Description is what the account records about itself (design §11.1,
// call 17): one line naming the spec's hash, the user and two times.
type Description struct {
	SpecHash string
	User     string
	Created  time.Time
	Used     time.Time
}

const descriptionPrefix = "silo/v2 "

// String formats d. It is at most 256 characters for any user email up
// to 140 characters; Valid says whether it fits.
func (d Description) String() string {
	return fmt.Sprintf("%sspec=%s user=%s created=%s used=%s", descriptionPrefix,
		d.SpecHash, d.User, d.Created.UTC().Format(timeFormat), d.Used.UTC().Format(timeFormat))
}

// Fits reports whether d fits GCP's 256-character description limit.
func (d Description) Fits() bool { return len(d.String()) <= maxDescription }

// ParseDescription reads a description silo wrote. ok is false for any
// other description, which marks an account silo did not make.
func ParseDescription(s string) (d Description, ok bool) {
	rest, found := strings.CutPrefix(s, descriptionPrefix)
	if !found {
		return d, false
	}
	seen := 0
	for _, f := range strings.Fields(rest) {
		k, v, found := strings.Cut(f, "=")
		if !found {
			return d, false
		}
		var err error
		switch k {
		case "spec":
			d.SpecHash = v
		case "user":
			d.User = v
		case "created":
			d.Created, err = time.Parse(timeFormat, v)
		case "used":
			d.Used, err = time.Parse(timeFormat, v)
		default:
			return d, false
		}
		if err != nil {
			return d, false
		}
		seen++
	}
	return d, seen == 4 && d.SpecHash != "" && d.User != ""
}
