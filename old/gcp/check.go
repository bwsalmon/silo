package gcp

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Client runs journey 2's account machinery against an API as the
// invoking user.
type Client struct {
	API API
	// Store is the user's accounts.json.
	Store *Store
	// Out gets progress and warnings; nothing is written when nil.
	Out io.Writer
	// Now and Sleep are time.Now and a context-aware sleep when nil.
	Now   func() time.Time
	Sleep func(context.Context, time.Duration) error
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) printf(format string, args ...any) {
	if c.Out != nil {
		fmt.Fprintf(c.Out, format, args...)
	}
}

// SetupPermissions are what the invoking user needs to set up, verify,
// reuse and gc accounts (design §11.2 step 1). Each maps to the
// predefined role that grants it; roles/owner grants them all.
var SetupPermissions = map[string]string{
	"iam.serviceAccounts.create":            "roles/iam.serviceAccountAdmin",
	"iam.serviceAccounts.get":               "roles/iam.serviceAccountAdmin",
	"iam.serviceAccounts.list":              "roles/iam.serviceAccountAdmin",
	"iam.serviceAccounts.update":            "roles/iam.serviceAccountAdmin",
	"iam.serviceAccounts.delete":            "roles/iam.serviceAccountAdmin",
	"iam.serviceAccounts.getIamPolicy":      "roles/iam.serviceAccountAdmin",
	"iam.serviceAccounts.setIamPolicy":      "roles/iam.serviceAccountAdmin",
	"resourcemanager.projects.getIamPolicy": "roles/resourcemanager.projectIamAdmin",
	"resourcemanager.projects.setIamPolicy": "roles/resourcemanager.projectIamAdmin",
}

// BasicRoles need allow_basic_roles: true (design §11.2).
var BasicRoles = map[string]bool{"roles/owner": true, "roles/editor": true, "roles/viewer": true}

// Quota thresholds on silo-* accounts in a project (design §11.7).
const (
	QuotaWarn   = 50
	QuotaRefuse = 90
)

// CheckInput is one projects[] entry as the invoking user.
type CheckInput struct {
	Project         string
	User            string
	Roles           []string
	AllowBasicRoles bool
}

// CheckReport is what Check found. Err turns it into a refusal.
type CheckReport struct {
	Spec Spec
	// MissingSetup are SetupPermissions the user lacks, sorted.
	MissingSetup []string
	Roles        []RoleCheck
	// SiloAccounts is how many silo-* accounts the project has, -1 if
	// they could not be listed; Exists says whether Spec's is one.
	SiloAccounts int
	Exists       bool
	Warnings     []string
}

// RoleCheck is one role's part of a CheckReport (design §11.2 step 2).
type RoleCheck struct {
	Role string
	// Error is set when the role could not be read.
	Error string
	// BasicNotAllowed: a basic role without allow_basic_roles.
	BasicNotAllowed bool
	// NotGrantable: not in queryGrantableRoles for the project.
	NotGrantable bool
	// Permissions is how many the role has; Missing are those the user
	// lacks; Untestable cannot be tested at project level.
	Permissions int
	Missing     []string
	Untestable  []string
	// Sample are permissions the account is probed for once it holds
	// the role (step 4).
	Sample []string
}

func (r RoleCheck) ok() bool {
	return r.Error == "" && !r.BasicNotAllowed && !r.NotGrantable && len(r.Missing) == 0
}

// testBatch is how many permissions one testIamPermissions call takes.
const testBatch = 100

// sampleSize is how many of a role's permissions propagation waits on.
const sampleSize = 3

// Check is steps 1 and 2 of setup (design §11.2), and what silo run
// --dry-run runs: may the user set up an account for in, and grant it
// each role? It changes nothing. An API failure that stops it from
// answering is returned as an error; what it finds is in the report,
// and report.Err says whether to refuse.
func (c *Client) Check(ctx context.Context, in CheckInput) (*CheckReport, error) {
	spec := NewSpec(in.Project, in.User, in.Roles)
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	rep := &CheckReport{Spec: spec, SiloAccounts: -1}

	setup := make([]string, 0, len(SetupPermissions))
	for p := range SetupPermissions {
		setup = append(setup, p)
	}
	sort.Strings(setup)
	held, err := c.API.TestPermissions(ctx, in.Project, setup)
	if err != nil {
		return nil, explain("testing "+in.User+"'s permissions on "+in.Project, err)
	}
	rep.MissingSetup = minus(setup, held)

	testable, err := c.API.TestablePermissions(ctx, in.Project)
	if err != nil {
		return nil, explain("listing the permissions testable on "+in.Project, err)
	}
	grantable, gerr := c.API.GrantableRoles(ctx, in.Project)
	if gerr != nil {
		rep.Warnings = append(rep.Warnings, explain("listing the roles grantable on "+in.Project, gerr).Error())
	}
	for _, role := range spec.Roles {
		rc := RoleCheck{Role: role}
		if BasicRoles[role] && !in.AllowBasicRoles {
			rc.BasicNotAllowed = true
		}
		if gerr == nil && !grantable[role] {
			rc.NotGrantable = true
		}
		r, err := c.API.GetRole(ctx, role)
		switch {
		case err != nil:
			rc.Error = explain("reading "+role, err).Error()
		case r.Deleted:
			rc.Error = role + " is deleted"
		default:
			if err := c.checkRole(ctx, in.Project, r, testable, &rc); err != nil {
				return nil, err
			}
		}
		if len(rc.Untestable) > 0 {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: %d of its permissions cannot be tested at project level, so silo cannot tell whether you hold them: %s",
				role, len(rc.Untestable), list(rc.Untestable)))
		}
		rep.Roles = append(rep.Roles, rc)
	}

	accounts, err := c.API.ListAccounts(ctx, in.Project)
	if err != nil {
		if len(rep.MissingSetup) == 0 {
			return nil, explain("listing service accounts in "+in.Project, err)
		}
	} else {
		rep.SiloAccounts = 0
		for _, a := range accounts {
			if strings.HasPrefix(a.Email, "silo-") {
				rep.SiloAccounts++
			}
			if a.Email == spec.Email() {
				rep.Exists = true
			}
		}
		if rep.SiloAccounts > QuotaWarn {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s has %d silo accounts of its default 100 service accounts: run `silo gc` to delete the ones gone unused",
				in.Project, rep.SiloAccounts))
		}
	}
	return rep, nil
}

func (c *Client) checkRole(ctx context.Context, project string, r Role, testable map[string]bool, rc *RoleCheck) error {
	perms := append([]string(nil), r.Permissions...)
	sort.Strings(perms)
	rc.Permissions = len(perms)
	var test []string
	for _, p := range perms {
		if testable[p] {
			test = append(test, p)
		} else {
			rc.Untestable = append(rc.Untestable, p)
		}
	}
	for i := 0; i < len(test); i += testBatch {
		batch := test[i:min(i+testBatch, len(test))]
		held, err := c.API.TestPermissions(ctx, project, batch)
		if err != nil {
			return explain("testing the permissions of "+r.Name+" on "+project, err)
		}
		rc.Missing = append(rc.Missing, minus(batch, held)...)
	}
	rc.Sample = sample(test, sampleSize)
	return nil
}

// sample picks up to n of perms, spread across the sorted list.
func sample(perms []string, n int) []string {
	if len(perms) <= n {
		return append([]string(nil), perms...)
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, perms[i*len(perms)/n])
	}
	return out
}

// Err is the refusal the report calls for, or nil. It names every
// missing permission and role, and what grants them.
func (r *CheckReport) Err() error {
	var lines []string
	if len(r.MissingSetup) > 0 {
		grants := map[string]bool{}
		for _, p := range r.MissingSetup {
			grants[SetupPermissions[p]] = true
		}
		lines = append(lines, fmt.Sprintf("you lack %s on %s, which setting up an account needs; %s grant them (or roles/owner)",
			list(r.MissingSetup), r.Spec.Project, list(keys(grants))))
	}
	for _, rc := range r.Roles {
		switch {
		case rc.Error != "":
			lines = append(lines, rc.Error)
		case rc.BasicNotAllowed:
			lines = append(lines, fmt.Sprintf("%s is a basic role: set allow_basic_roles: true on the projects[] entry to grant it", rc.Role))
		}
		if rc.NotGrantable {
			lines = append(lines, fmt.Sprintf("%s cannot be granted on %s (it is not in the project's grantable roles)", rc.Role, r.Spec.Project))
		}
		if len(rc.Missing) > 0 {
			lines = append(lines, fmt.Sprintf("%s: you lack %d of its %d permissions (%s); silo grants a role only if you hold every permission in it (design §11.2, call 19), so get %s yourself on %s first",
				rc.Role, len(rc.Missing), rc.Permissions, list(rc.Missing), rc.Role, r.Spec.Project))
		}
	}
	if !r.Exists && r.SiloAccounts > QuotaRefuse {
		lines = append(lines, fmt.Sprintf("%s already has %d silo accounts (more than %d of the default 100): run `silo gc` to delete the unused ones, or `silo gc --project %s --account <email>` for one",
			r.Spec.Project, r.SiloAccounts, QuotaRefuse, r.Spec.Project))
	}
	if len(lines) == 0 {
		return nil
	}
	return &RefusedError{Project: r.Spec.Project, User: r.Spec.User, Reasons: lines}
}

// RefusedError is a setup check's refusal.
type RefusedError struct {
	Project, User string
	Reasons       []string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("silo cannot set up an account in %s as %s:\n  - %s", e.Project, e.User, strings.Join(e.Reasons, "\n  - "))
}

// Samples maps each role to the permissions propagation waits on.
func (r *CheckReport) Samples() map[string][]string {
	out := map[string][]string{}
	for _, rc := range r.Roles {
		out[rc.Role] = rc.Sample
	}
	return out
}

func minus(all, held []string) []string {
	h := map[string]bool{}
	for _, p := range held {
		h[p] = true
	}
	var out []string
	for _, p := range all {
		if !h[p] {
			out = append(out, p)
		}
	}
	return out
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// list joins a list for a message, eliding the middle of a long one.
func list(xs []string) string {
	const max = 12
	if len(xs) > max {
		return strings.Join(xs[:max], ", ") + fmt.Sprintf(" and %d more", len(xs)-max)
	}
	return strings.Join(xs, ", ")
}
