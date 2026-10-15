// Adapted from github.com/bwsalmon/grain pkg/gcpsetup/admin.go at c950ca0858bc4b381e5ce7e2bd716997f63bd24d, Apache License 2.0.

package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	crm "google.golang.org/api/cloudresourcemanager/v3"
	iam "google.golang.org/api/iam/v1"
	iamcredentials "google.golang.org/api/iamcredentials/v1"
	"google.golang.org/api/option"
)

// Scopes the user's ambient credentials are asked for: cloud-platform
// for every call, and the email scope so that tokeninfo names the user.
const (
	scopeCloudPlatform = "https://www.googleapis.com/auth/cloud-platform"
	scopeEmail         = "https://www.googleapis.com/auth/userinfo.email"
)

// Google is API against the real IAM, Cloud Resource Manager and IAM
// Credentials APIs. Policies keep the client's own value in Raw, and a
// write replaces only its bindings (and version), so audit configs and
// anything else silo does not model survive.
type Google struct {
	iam   *iam.Service
	crm   *crm.Service
	creds *iamcredentials.Service
	opts  []option.ClientOption

	mu       sync.Mutex
	testable map[string]map[string]bool
}

var _ API = (*Google)(nil)

// NewGoogle builds Google from client options; with none, the clients
// use Application Default Credentials.
func NewGoogle(ctx context.Context, opts ...option.ClientOption) (*Google, error) {
	i, err := iam.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("building an IAM client: %w", err)
	}
	c, err := crm.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("building a Cloud Resource Manager client: %w", err)
	}
	cr, err := iamcredentials.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("building an IAM Credentials client: %w", err)
	}
	return &Google{iam: i, crm: c, creds: cr, opts: opts, testable: map[string]map[string]bool{}}, nil
}

// Ambient builds Google from the invoking user's own login,
// {ambient: gcloud} (design §8.1): Application Default Credentials if
// there are any, else the gcloud CLI's active account. It returns the
// login's email, which is the spec's user.
func Ambient(ctx context.Context) (*Google, string, error) {
	var ts oauth2.TokenSource
	email := ""
	if creds, err := google.FindDefaultCredentials(ctx, scopeCloudPlatform, scopeEmail); err == nil {
		ts = creds.TokenSource
		var f struct {
			ClientEmail string `json:"client_email"`
		}
		if json.Unmarshal(creds.JSON, &f) == nil {
			email = f.ClientEmail
		}
	} else {
		ts = oauth2.ReuseTokenSource(nil, gcloudTokenSource{ctx: ctx})
		out, err := exec.CommandContext(ctx, "gcloud", "config", "get-value", "account").Output()
		if err != nil {
			return nil, "", fmt.Errorf("{ambient: gcloud}: no Application Default Credentials and no gcloud login: run `gcloud auth login` (or `gcloud auth application-default login`)")
		}
		email = strings.TrimSpace(string(out))
	}
	if email == "" {
		tok, err := ts.Token()
		if err != nil {
			return nil, "", fmt.Errorf("{ambient: gcloud}: %w", err)
		}
		if email, err = tokenEmail(ctx, tok.AccessToken); err != nil {
			return nil, "", err
		}
	}
	g, err := NewGoogle(ctx, option.WithTokenSource(ts))
	if err != nil {
		return nil, "", err
	}
	return g, strings.ToLower(email), nil
}

// gcloudTokenSource asks gcloud for the active account's token.
type gcloudTokenSource struct{ ctx context.Context }

func (s gcloudTokenSource) Token() (*oauth2.Token, error) {
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gcloud", "auth", "print-access-token")
	cmd.Env = append(cmd.Environ(), "CLOUDSDK_CORE_DISABLE_PROMPTS=1")
	out, err := cmd.Output()
	tok := strings.TrimSpace(string(out))
	if err != nil || tok == "" {
		return nil, fmt.Errorf("{ambient: gcloud}: gcloud printed no access token: run `gcloud auth login`")
	}
	// gcloud's tokens last an hour; ask again well before.
	return &oauth2.Token{AccessToken: tok, Expiry: time.Now().Add(5 * time.Minute)}, nil
}

// tokenEmail asks Google's tokeninfo endpoint whose token this is.
func tokenEmail(ctx context.Context, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://oauth2.googleapis.com/tokeninfo?access_token="+url.QueryEscape(token), nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("{ambient: gcloud}: asking tokeninfo who the login is: %w", err)
	}
	defer resp.Body.Close()
	var info struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil || info.Email == "" {
		return "", fmt.Errorf("{ambient: gcloud}: tokeninfo did not name the login's email (HTTP %d); log in with the email scope", resp.StatusCode)
	}
	return info.Email, nil
}

func projectResource(project string) string { return "projects/" + project }

func accountResource(project, email string) string {
	return "projects/" + project + "/serviceAccounts/" + email
}

func fullResourceName(project string) string {
	return "//cloudresourcemanager.googleapis.com/projects/" + project
}

func (g *Google) TestPermissions(ctx context.Context, project string, perms []string) ([]string, error) {
	return testPermissions(ctx, g.crm, project, perms)
}

func (g *Google) TestPermissionsAs(ctx context.Context, token, project string, perms []string) ([]string, error) {
	svc, err := crm.NewService(ctx, option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})))
	if err != nil {
		return nil, err
	}
	return testPermissions(ctx, svc, project, perms)
}

func testPermissions(ctx context.Context, svc *crm.Service, project string, perms []string) ([]string, error) {
	resp, err := svc.Projects.TestIamPermissions(projectResource(project),
		&crm.TestIamPermissionsRequest{Permissions: perms}).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return resp.Permissions, nil
}

func (g *Google) TestablePermissions(ctx context.Context, project string) (map[string]bool, error) {
	g.mu.Lock()
	cached := g.testable[project]
	g.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	out := map[string]bool{}
	err := g.iam.Permissions.QueryTestablePermissions(&iam.QueryTestablePermissionsRequest{
		FullResourceName: fullResourceName(project), PageSize: 1000,
	}).Pages(ctx, func(r *iam.QueryTestablePermissionsResponse) error {
		for _, p := range r.Permissions {
			out[p.Name] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.testable[project] = out
	g.mu.Unlock()
	return out, nil
}

func (g *Google) GetRole(ctx context.Context, name string) (Role, error) {
	var r *iam.Role
	var err error
	switch {
	case strings.HasPrefix(name, "projects/"):
		r, err = g.iam.Projects.Roles.Get(name).Context(ctx).Do()
	case strings.HasPrefix(name, "organizations/"):
		r, err = g.iam.Organizations.Roles.Get(name).Context(ctx).Do()
	default:
		r, err = g.iam.Roles.Get(name).Context(ctx).Do()
	}
	if err != nil {
		return Role{}, err
	}
	return Role{Name: r.Name, Permissions: r.IncludedPermissions, Deleted: r.Deleted}, nil
}

func (g *Google) GrantableRoles(ctx context.Context, project string) (map[string]bool, error) {
	out := map[string]bool{}
	err := g.iam.Roles.QueryGrantableRoles(&iam.QueryGrantableRolesRequest{
		FullResourceName: fullResourceName(project), PageSize: 1000,
	}).Pages(ctx, func(r *iam.QueryGrantableRolesResponse) error {
		for _, role := range r.Roles {
			out[role.Name] = true
		}
		return nil
	})
	return out, err
}

func account(a *iam.ServiceAccount) Account {
	return Account{Email: a.Email, UniqueID: a.UniqueId, DisplayName: a.DisplayName, Description: a.Description, Disabled: a.Disabled}
}

func (g *Google) ListAccounts(ctx context.Context, project string) ([]Account, error) {
	var out []Account
	err := g.iam.Projects.ServiceAccounts.List(projectResource(project)).PageSize(100).Pages(ctx,
		func(r *iam.ListServiceAccountsResponse) error {
			for _, a := range r.Accounts {
				out = append(out, account(a))
			}
			return nil
		})
	return out, err
}

func (g *Google) GetAccount(ctx context.Context, project, email string) (Account, error) {
	a, err := g.iam.Projects.ServiceAccounts.Get(accountResource(project, email)).Context(ctx).Do()
	if err != nil {
		return Account{}, err
	}
	return account(a), nil
}

func (g *Google) CreateAccount(ctx context.Context, project, id, displayName, description string) (Account, error) {
	a, err := g.iam.Projects.ServiceAccounts.Create(projectResource(project), &iam.CreateServiceAccountRequest{
		AccountId:      id,
		ServiceAccount: &iam.ServiceAccount{DisplayName: displayName, Description: description},
	}).Context(ctx).Do()
	if err != nil {
		return Account{}, err
	}
	return account(a), nil
}

func (g *Google) SetDescription(ctx context.Context, project, email, description string) error {
	_, err := g.iam.Projects.ServiceAccounts.Patch(accountResource(project, email), &iam.PatchServiceAccountRequest{
		ServiceAccount: &iam.ServiceAccount{Description: description},
		UpdateMask:     "description",
	}).Context(ctx).Do()
	return err
}

func (g *Google) DeleteAccount(ctx context.Context, project, email string) error {
	_, err := g.iam.Projects.ServiceAccounts.Delete(accountResource(project, email)).Context(ctx).Do()
	return err
}

// policyVersion is the IAM policy version silo reads, so that
// conditional bindings come back whole and are written back unchanged.
const policyVersion = 3

func (g *Google) GetProjectPolicy(ctx context.Context, project string) (*Policy, error) {
	p, err := g.crm.Projects.GetIamPolicy(projectResource(project), &crm.GetIamPolicyRequest{
		Options: &crm.GetPolicyOptions{RequestedPolicyVersion: policyVersion},
	}).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	out := &Policy{Etag: p.Etag, Version: p.Version, Raw: p}
	for _, b := range p.Bindings {
		nb := Binding{Role: b.Role, Members: append([]string(nil), b.Members...)}
		if b.Condition != nil {
			nb.Condition = &Condition{Title: b.Condition.Title, Description: b.Condition.Description, Expression: b.Condition.Expression}
		}
		out.Bindings = append(out.Bindings, nb)
	}
	return out, nil
}

func (g *Google) SetProjectPolicy(ctx context.Context, project string, p *Policy) error {
	raw, ok := p.Raw.(*crm.Policy)
	if !ok {
		return fmt.Errorf("SetProjectPolicy: a policy not read by GetProjectPolicy")
	}
	raw.Etag = p.Etag
	raw.Bindings = nil
	conditional := false
	for _, b := range p.Bindings {
		nb := &crm.Binding{Role: b.Role, Members: b.Members}
		if b.Condition != nil {
			conditional = true
			nb.Condition = &crm.Expr{Title: b.Condition.Title, Description: b.Condition.Description, Expression: b.Condition.Expression}
		}
		raw.Bindings = append(raw.Bindings, nb)
	}
	if conditional {
		raw.Version = policyVersion
	}
	_, err := g.crm.Projects.SetIamPolicy(projectResource(project), &crm.SetIamPolicyRequest{Policy: raw}).Context(ctx).Do()
	return err
}

func (g *Google) GetAccountPolicy(ctx context.Context, project, email string) (*Policy, error) {
	p, err := g.iam.Projects.ServiceAccounts.GetIamPolicy(accountResource(project, email)).
		OptionsRequestedPolicyVersion(policyVersion).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	out := &Policy{Etag: p.Etag, Version: p.Version, Raw: p}
	for _, b := range p.Bindings {
		nb := Binding{Role: b.Role, Members: append([]string(nil), b.Members...)}
		if b.Condition != nil {
			nb.Condition = &Condition{Title: b.Condition.Title, Description: b.Condition.Description, Expression: b.Condition.Expression}
		}
		out.Bindings = append(out.Bindings, nb)
	}
	return out, nil
}

func (g *Google) SetAccountPolicy(ctx context.Context, project, email string, p *Policy) error {
	raw, ok := p.Raw.(*iam.Policy)
	if !ok {
		return fmt.Errorf("SetAccountPolicy: a policy not read by GetAccountPolicy")
	}
	raw.Etag = p.Etag
	raw.Bindings = nil
	conditional := false
	for _, b := range p.Bindings {
		nb := &iam.Binding{Role: b.Role, Members: b.Members}
		if b.Condition != nil {
			conditional = true
			nb.Condition = &iam.Expr{Title: b.Condition.Title, Description: b.Condition.Description, Expression: b.Condition.Expression}
		}
		raw.Bindings = append(raw.Bindings, nb)
	}
	if conditional {
		raw.Version = policyVersion
	}
	_, err := g.iam.Projects.ServiceAccounts.SetIamPolicy(accountResource(project, email), &iam.SetIamPolicyRequest{Policy: raw}).Context(ctx).Do()
	return err
}

func (g *Google) GenerateAccessToken(ctx context.Context, email string, lifetime time.Duration) (Token, error) {
	r, err := g.creds.Projects.ServiceAccounts.GenerateAccessToken("projects/-/serviceAccounts/"+email,
		&iamcredentials.GenerateAccessTokenRequest{
			Scope:    []string{scopeCloudPlatform},
			Lifetime: fmt.Sprintf("%ds", int(lifetime.Seconds())),
		}).Context(ctx).Do()
	if err != nil {
		return Token{}, err
	}
	exp, _ := time.Parse(time.RFC3339, r.ExpireTime)
	return Token{Value: r.AccessToken, Expiry: exp}, nil
}
