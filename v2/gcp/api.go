package gcp

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"google.golang.org/api/googleapi"
)

// API is the slice of IAM, Cloud Resource Manager and IAM Credentials
// silo uses. Every call runs as the invoking user except
// TestPermissionsAs, which runs as whoever token belongs to. Errors from
// the real APIs are *googleapi.Error; a fake returns the same so that
// the retry and refusal rules are tested against what Google sends.
type API interface {
	// TestPermissions is projects.testIamPermissions: the subset of
	// perms the caller holds on the project.
	TestPermissions(ctx context.Context, project string, perms []string) ([]string, error)
	// TestPermissionsAs is TestPermissions with an access token.
	TestPermissionsAs(ctx context.Context, token, project string, perms []string) ([]string, error)
	// TestablePermissions lists every permission testIamPermissions
	// accepts on a project (permissions.queryTestablePermissions).
	TestablePermissions(ctx context.Context, project string) (map[string]bool, error)
	// GetRole is roles.get for roles/x, projects/p/roles/x or
	// organizations/o/roles/x.
	GetRole(ctx context.Context, name string) (Role, error)
	// GrantableRoles is roles.queryGrantableRoles on the project.
	GrantableRoles(ctx context.Context, project string) (map[string]bool, error)

	ListAccounts(ctx context.Context, project string) ([]Account, error)
	// GetAccount returns an error with code 404 if there is none.
	GetAccount(ctx context.Context, project, email string) (Account, error)
	// CreateAccount returns an error with code 409 if the id is taken.
	CreateAccount(ctx context.Context, project, id, displayName, description string) (Account, error)
	SetDescription(ctx context.Context, project, email, description string) error
	DeleteAccount(ctx context.Context, project, email string) error

	// Policies are read and written with their etag: a write whose etag
	// is stale fails with code 409.
	GetProjectPolicy(ctx context.Context, project string) (*Policy, error)
	SetProjectPolicy(ctx context.Context, project string, p *Policy) error
	GetAccountPolicy(ctx context.Context, project, email string) (*Policy, error)
	SetAccountPolicy(ctx context.Context, project, email string, p *Policy) error

	// GenerateAccessToken is iamcredentials generateAccessToken with the
	// cloud-platform scope.
	GenerateAccessToken(ctx context.Context, email string, lifetime time.Duration) (Token, error)
}

// Role is what roles.get says about a role.
type Role struct {
	Name        string
	Permissions []string
	Deleted     bool
}

// Account is a service account.
type Account struct {
	Email       string
	UniqueID    string
	DisplayName string
	Description string
	Disabled    bool
}

// Policy is an IAM policy, project-level or an account's own. Bindings
// silo does not touch, conditional ones included, are written back
// exactly as read.
type Policy struct {
	Etag     string
	Version  int64
	Bindings []Binding
	// Raw is the client's own policy value, which the real
	// implementation writes back with only Bindings replaced, so that
	// fields this type does not model (audit configs) survive.
	Raw any
}

// Binding is one role's members, under an optional condition.
type Binding struct {
	Role      string
	Members   []string
	Condition *Condition
}

// Condition is an IAM Condition. silo never writes one (open question
// 8) but keeps those it reads.
type Condition struct {
	Title, Description, Expression string
}

// Token is a minted access token.
type Token struct {
	Value  string
	Expiry time.Time
}

// Member is how an account appears in a policy.
func Member(email string) string { return "serviceAccount:" + email }

// UserMember is how the invoking principal appears in a policy: a
// service account when its email is one, a user otherwise.
func UserMember(email string) string {
	if strings.HasSuffix(email, ".gserviceaccount.com") {
		return "serviceAccount:" + email
	}
	return "user:" + email
}

// rolesOf lists the unconditional roles member holds in p, and whether
// member appears in any conditional binding.
func rolesOf(p *Policy, member string) (roles map[string]bool, conditional []string) {
	roles = map[string]bool{}
	for _, b := range p.Bindings {
		for _, m := range b.Members {
			if m != member {
				continue
			}
			if b.Condition != nil {
				conditional = append(conditional, b.Role)
			} else {
				roles[b.Role] = true
			}
		}
	}
	return roles, conditional
}

// addMember adds member to role's unconditional binding and reports
// whether p changed.
func addMember(p *Policy, role, member string) bool {
	for i := range p.Bindings {
		b := &p.Bindings[i]
		if b.Role != role || b.Condition != nil {
			continue
		}
		for _, m := range b.Members {
			if m == member {
				return false
			}
		}
		b.Members = append(b.Members, member)
		return true
	}
	p.Bindings = append(p.Bindings, Binding{Role: role, Members: []string{member}})
	return true
}

// removeMember takes every member for which drop is true out of every
// binding, dropping bindings left empty, and reports whether p changed.
func removeMember(p *Policy, drop func(string) bool) bool {
	changed := false
	var out []Binding
	for _, b := range p.Bindings {
		var keep []string
		for _, m := range b.Members {
			if drop(m) {
				changed = true
			} else {
				keep = append(keep, m)
			}
		}
		if len(keep) > 0 {
			b.Members = keep
			out = append(out, b)
		}
	}
	p.Bindings = out
	return changed
}

// Code is err's HTTP status if it is a Google API error, else 0.
func Code(err error) int {
	var g *googleapi.Error
	if errors.As(err, &g) {
		return g.Code
	}
	return 0
}

// retryable are the codes failed IAM writes are retried on (design
// §11.7): an etag conflict, rate limiting and unavailability. A 403 is
// never retried.
func retryable(err error, conflict bool) bool {
	switch Code(err) {
	case 409:
		return conflict
	case 429, 503:
		return true
	}
	return false
}

// Backoff bounds for IAM write retries.
const (
	retryAttempts = 6
	retryBase     = time.Second
	retryMax      = 30 * time.Second
)

// retry runs fn until it succeeds, fails with an error that is not
// retryable, or has run retryAttempts times. conflict says whether a 409
// is an etag conflict (fn re-reads what it modifies) and so retryable.
func (c *Client) retry(ctx context.Context, conflict bool, fn func() error) error {
	wait := retryBase
	for attempt := 1; ; attempt++ {
		err := fn()
		if err == nil || attempt == retryAttempts || !retryable(err, conflict) {
			return err
		}
		if err := c.sleep(ctx, wait); err != nil {
			return err
		}
		wait = min(wait*2, retryMax)
	}
}

var constraintRE = regexp.MustCompile(`constraints/[A-Za-z0-9_.]+`)

// explain adds to an API error what silo knows about it: the
// organization policy constraint it names (design §11.7), or that a 403
// means a missing permission.
func explain(what string, err error) error {
	if err == nil {
		return nil
	}
	var g *googleapi.Error
	if errors.As(err, &g) {
		err = shortError{g}
	}
	msg := err.Error()
	if c := constraintRE.FindString(msg); c != "" {
		return fmt.Errorf("%s: forbidden by organization policy %s: %w", what, c, err)
	}
	if Code(err) == 403 {
		return fmt.Errorf("%s: permission denied (silo retries no 403): %w", what, err)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// shortError prints a Google API error as its code and message, without
// the details JSON googleapi.Error appends.
type shortError struct{ g *googleapi.Error }

func (e shortError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.g.Code, e.g.Message) }
func (e shortError) Unwrap() error { return e.g }
