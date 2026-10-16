// Package fakegcp is an in-memory gcp.API for one project, for tests.
// It answers as Google does: *googleapi.Error with 404, 409 and 403
// codes, etag conflicts on stale policy writes.
package fakegcp

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/old/gcp"
	"google.golang.org/api/googleapi"
)

// Fake is one project and its caller, the invoking user.
type Fake struct {
	mu sync.Mutex

	Project string
	// Held are the permissions the user holds on the project.
	Held map[string]bool
	// Testable are the permissions testIamPermissions accepts.
	Testable  map[string]bool
	Roles     map[string]gcp.Role
	Grantable map[string]bool

	Accounts        map[string]*gcp.Account
	ProjectPolicy   *gcp.Policy
	AccountPolicies map[string]*gcp.Policy

	// TokenDenials is how many generateAccessToken calls return 403
	// before tokens are minted; AsDenials how many testIamPermissions
	// calls as an account see none of its permissions.
	TokenDenials int
	AsDenials    int

	// Calls lists every method called, in order.
	Calls []string

	faults map[string][]error
	etag   int
	nextID int
}

// New returns a project whose user holds perms.
func New(project string, perms ...string) *Fake {
	f := &Fake{
		Project: project, Held: map[string]bool{}, Testable: map[string]bool{},
		Roles: map[string]gcp.Role{}, Grantable: map[string]bool{},
		Accounts: map[string]*gcp.Account{}, AccountPolicies: map[string]*gcp.Policy{},
		ProjectPolicy: &gcp.Policy{Etag: "e0"}, faults: map[string][]error{},
	}
	for _, p := range perms {
		f.Held[p] = true
		f.Testable[p] = true
	}
	for p := range gcp.SetupPermissions {
		f.Testable[p] = true
	}
	return f
}

// AddRole defines a grantable role with perms, all testable.
func (f *Fake) AddRole(name string, perms ...string) {
	f.Roles[name] = gcp.Role{Name: name, Permissions: perms}
	f.Grantable[name] = true
	for _, p := range perms {
		f.Testable[p] = true
	}
}

// Fail makes the next calls of method return errs, one per call.
func (f *Fake) Fail(method string, errs ...error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults[method] = append(f.faults[method], errs...)
}

// Err is a Google API error with code.
func Err(code int, msg string) error { return &googleapi.Error{Code: code, Message: msg} }

func (f *Fake) call(method string) error {
	f.Calls = append(f.Calls, method)
	if q := f.faults[method]; len(q) > 0 {
		f.faults[method] = q[1:]
		return q[0]
	}
	return nil
}

// Count is how many times method was called.
func (f *Fake) Count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.Calls {
		if c == method {
			n++
		}
	}
	return n
}

func (f *Fake) project(p string) error {
	if p != f.Project {
		return Err(403, "permission denied on "+p)
	}
	return nil
}

func (f *Fake) TestPermissions(ctx context.Context, project string, perms []string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("TestPermissions"); err != nil {
		return nil, err
	}
	if len(perms) > 100 {
		return nil, Err(400, "too many permissions")
	}
	var out []string
	for _, p := range perms {
		if !f.Testable[p] {
			return nil, Err(400, "Permission "+p+" is not valid for this resource")
		}
		if f.Held[p] && project == f.Project {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *Fake) TestPermissionsAs(ctx context.Context, token, project string, perms []string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("TestPermissionsAs"); err != nil {
		return nil, err
	}
	email, ok := strings.CutPrefix(token, "token-for:")
	if !ok {
		return nil, Err(401, "bad token")
	}
	if f.AsDenials > 0 {
		f.AsDenials--
		return nil, nil
	}
	held := map[string]bool{}
	for _, b := range f.ProjectPolicy.Bindings {
		for _, m := range b.Members {
			if m == gcp.Member(email) && b.Condition == nil {
				for _, p := range f.Roles[b.Role].Permissions {
					held[p] = true
				}
			}
		}
	}
	var out []string
	for _, p := range perms {
		if held[p] {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *Fake) TestablePermissions(ctx context.Context, project string) (map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("TestablePermissions"); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for p := range f.Testable {
		out[p] = true
	}
	return out, nil
}

func (f *Fake) GetRole(ctx context.Context, name string) (gcp.Role, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("GetRole"); err != nil {
		return gcp.Role{}, err
	}
	r, ok := f.Roles[name]
	if !ok {
		return gcp.Role{}, Err(404, "role "+name+" not found")
	}
	return r, nil
}

func (f *Fake) GrantableRoles(ctx context.Context, project string) (map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("GrantableRoles"); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for r := range f.Grantable {
		out[r] = true
	}
	return out, nil
}

func (f *Fake) ListAccounts(ctx context.Context, project string) ([]gcp.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("ListAccounts"); err != nil {
		return nil, err
	}
	if err := f.project(project); err != nil {
		return nil, err
	}
	var out []gcp.Account
	for _, a := range f.Accounts {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out, nil
}

func (f *Fake) GetAccount(ctx context.Context, project, email string) (gcp.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("GetAccount"); err != nil {
		return gcp.Account{}, err
	}
	a, ok := f.Accounts[email]
	if !ok {
		return gcp.Account{}, Err(404, "account "+email+" not found")
	}
	return *a, nil
}

func (f *Fake) CreateAccount(ctx context.Context, project, id, displayName, description string) (gcp.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("CreateAccount"); err != nil {
		return gcp.Account{}, err
	}
	if err := f.project(project); err != nil {
		return gcp.Account{}, err
	}
	email := gcp.AccountEmail(id, project)
	if _, ok := f.Accounts[email]; ok {
		return gcp.Account{}, Err(409, "account "+email+" already exists")
	}
	if len(description) > 256 || len(displayName) > 100 {
		return gcp.Account{}, Err(400, "too long")
	}
	f.nextID++
	a := &gcp.Account{Email: email, UniqueID: fmt.Sprintf("1%05d", f.nextID), DisplayName: displayName, Description: description}
	f.Accounts[email] = a
	f.AccountPolicies[email] = &gcp.Policy{Etag: "a0"}
	return *a, nil
}

// PutAccount adds an account made outside silo, or by another user.
func (f *Fake) PutAccount(a gcp.Account) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a.UniqueID == "" {
		f.nextID++
		a.UniqueID = fmt.Sprintf("1%05d", f.nextID)
	}
	f.Accounts[a.Email] = &a
	if f.AccountPolicies[a.Email] == nil {
		f.AccountPolicies[a.Email] = &gcp.Policy{Etag: "a0"}
	}
}

func (f *Fake) SetDescription(ctx context.Context, project, email, description string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("SetDescription"); err != nil {
		return err
	}
	a, ok := f.Accounts[email]
	if !ok {
		return Err(404, "not found")
	}
	a.Description = description
	return nil
}

func (f *Fake) DeleteAccount(ctx context.Context, project, email string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("DeleteAccount"); err != nil {
		return err
	}
	if _, ok := f.Accounts[email]; !ok {
		return Err(404, "not found")
	}
	delete(f.Accounts, email)
	delete(f.AccountPolicies, email)
	return nil
}

func clone(p *gcp.Policy) *gcp.Policy {
	out := &gcp.Policy{Etag: p.Etag, Version: p.Version}
	for _, b := range p.Bindings {
		nb := b
		nb.Members = append([]string(nil), b.Members...)
		out.Bindings = append(out.Bindings, nb)
	}
	return out
}

func (f *Fake) GetProjectPolicy(ctx context.Context, project string) (*gcp.Policy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("GetProjectPolicy"); err != nil {
		return nil, err
	}
	if err := f.project(project); err != nil {
		return nil, err
	}
	return clone(f.ProjectPolicy), nil
}

func (f *Fake) SetProjectPolicy(ctx context.Context, project string, p *gcp.Policy) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("SetProjectPolicy"); err != nil {
		return err
	}
	if p.Etag != f.ProjectPolicy.Etag {
		return Err(409, "There were concurrent policy changes")
	}
	f.etag++
	np := clone(p)
	np.Etag = fmt.Sprintf("e%d", f.etag)
	f.ProjectPolicy = np
	return nil
}

func (f *Fake) GetAccountPolicy(ctx context.Context, project, email string) (*gcp.Policy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("GetAccountPolicy"); err != nil {
		return nil, err
	}
	p, ok := f.AccountPolicies[email]
	if !ok {
		return nil, Err(404, "not found")
	}
	return clone(p), nil
}

func (f *Fake) SetAccountPolicy(ctx context.Context, project, email string, p *gcp.Policy) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("SetAccountPolicy"); err != nil {
		return err
	}
	cur, ok := f.AccountPolicies[email]
	if !ok {
		return Err(404, "not found")
	}
	if p.Etag != cur.Etag {
		return Err(409, "There were concurrent policy changes")
	}
	f.etag++
	np := clone(p)
	np.Etag = fmt.Sprintf("a%d", f.etag)
	f.AccountPolicies[email] = np
	return nil
}

func (f *Fake) GenerateAccessToken(ctx context.Context, email string, lifetime time.Duration) (gcp.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("GenerateAccessToken"); err != nil {
		return gcp.Token{}, err
	}
	if _, ok := f.Accounts[email]; !ok {
		return gcp.Token{}, Err(404, "not found")
	}
	if f.TokenDenials > 0 {
		f.TokenDenials--
		return gcp.Token{}, Err(403, "Permission 'iam.serviceAccounts.getAccessToken' denied")
	}
	return gcp.Token{Value: "token-for:" + email, Expiry: time.Now().Add(lifetime)}, nil
}

// Binds lists the unconditional roles member holds on the project.
func (f *Fake) Binds(member string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, b := range f.ProjectPolicy.Bindings {
		for _, m := range b.Members {
			if m == member && b.Condition == nil {
				out = append(out, b.Role)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Bind adds member to role on the project, outside silo.
func (f *Fake) Bind(role, member string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ProjectPolicy.Bindings = append(f.ProjectPolicy.Bindings, gcp.Binding{Role: role, Members: []string{member}})
	f.etag++
	f.ProjectPolicy.Etag = fmt.Sprintf("e%d", f.etag)
}

// BindAccount adds member to role on an account's own policy.
func (f *Fake) BindAccount(email, role, member string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.AccountPolicies[email]
	p.Bindings = append(p.Bindings, gcp.Binding{Role: role, Members: []string{member}})
}
