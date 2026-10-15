package gcp

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// TokenCreator is the role the invoking user holds on the account's own
// policy, and the only binding that policy may have.
const TokenCreator = "roles/iam.serviceAccountTokenCreator"

// DefaultTTL is how long an account may sit unused when its projects[]
// entry gives no ttl (design §11.5).
const DefaultTTL = 7 * 24 * time.Hour

// EnsureInput is one account to create or reuse.
type EnsureInput struct {
	Spec Spec
	TTL  time.Duration
	// Session, when set, is recorded as using the account, so gc leaves
	// it alone while the session lives.
	Session *SessionRef
}

// MismatchError refuses an account that does not match its spec
// (design §11.1, call 18). silo does not repair it: something other than
// silo changed it.
type MismatchError struct {
	Email   string
	Project string
	Diffs   []string
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("service account %s does not match what silo made, so silo will not use it (design §11.1):\n  - %s\n"+
		"something other than silo changed it; to start again, run `silo gc --project %s --account %s` and the next run recreates it",
		e.Email, strings.Join(e.Diffs, "\n  - "), e.Project, e.Email)
}

// Ensure is setup step 3 (design §11.2): it reuses the spec's account if
// it exists and verifies, finishes it if a previous run left it
// incomplete (design §11.6), or creates it. Each step is recorded in the
// store as it completes, so an interrupted run resumes. Ensure assumes
// Check has passed.
func (c *Client) Ensure(ctx context.Context, in EnsureInput) (Record, error) {
	spec := in.Spec
	if err := spec.Validate(); err != nil {
		return Record{}, err
	}
	email := spec.Email()
	rec, have, err := c.Store.Get(email)
	if err != nil {
		return Record{}, err
	}

	var acct Account
	err = c.retry(ctx, false, func() (err error) {
		acct, err = c.API.GetAccount(ctx, spec.Project, email)
		return err
	})
	switch {
	case Code(err) == 404:
		if have && !rec.Incomplete {
			c.printf("silo: %s was deleted outside silo; creating it again\n", email)
		}
		if rec, err = c.create(ctx, spec); err != nil {
			return Record{}, err
		}
	case err != nil:
		return Record{}, explain("reading "+email, err)
	default:
		var diffs []string
		if rec, diffs = c.adopt(spec, acct, rec, have); len(diffs) > 0 {
			return Record{}, &MismatchError{Email: email, Project: spec.Project, Diffs: diffs}
		}
	}
	rec.TTL = Duration(in.TTL)
	if in.Session != nil {
		rec.Session = in.Session
	}
	if err := c.Store.Put(rec); err != nil {
		return Record{}, err
	}

	if err := c.finish(ctx, spec, &rec); err != nil {
		return rec, err
	}
	rec.Incomplete = false
	rec.Done = nil
	return rec, c.Store.Put(rec)
}

// create makes the account and records it as incomplete.
func (c *Client) create(ctx context.Context, spec Spec) (Record, error) {
	email := spec.Email()
	now := c.now().Truncate(time.Second)
	desc := Description{SpecHash: spec.Hash(), User: spec.User, Created: now, Used: now}
	if !desc.Fits() {
		return Record{}, fmt.Errorf("the description of %s would be longer than GCP's 256 characters: %q", email, desc.String())
	}
	var acct Account
	err := c.retry(ctx, false, func() (err error) {
		acct, err = c.API.CreateAccount(ctx, spec.Project, spec.AccountID(), spec.DisplayName(), desc.String())
		return err
	})
	if Code(err) == 409 {
		// Another silo process created it first: verify it as any
		// existing account, from scratch.
		if acct, err = c.API.GetAccount(ctx, spec.Project, email); err != nil {
			return Record{}, explain("reading "+email+" after creating it raced with another", err)
		}
		rec, diffs := c.adopt(spec, acct, Record{}, false)
		if len(diffs) > 0 {
			return Record{}, &MismatchError{Email: email, Project: spec.Project, Diffs: diffs}
		}
		return rec, nil
	}
	if err != nil {
		return Record{}, explain("creating "+email+" in "+spec.Project, err)
	}
	c.printf("silo: created %s for %s\n", email, strings.Join(spec.Roles, ", "))
	rec := Record{
		Email: email, Project: spec.Project, User: spec.User, Spec: spec,
		UniqueID: acct.UniqueID, Incomplete: true, Done: []string{stepCreate},
		Created: now, Used: now,
	}
	return rec, c.Store.Put(rec)
}

// adopt checks an existing account's own record of itself and returns
// the local record to carry on with. Bindings are checked by finish.
func (c *Client) adopt(spec Spec, acct Account, rec Record, have bool) (Record, []string) {
	var diffs []string
	d, ok := ParseDescription(acct.Description)
	switch {
	case !ok:
		diffs = append(diffs, fmt.Sprintf("its description %q is not one silo writes", acct.Description))
	default:
		if d.SpecHash != spec.Hash() {
			diffs = append(diffs, fmt.Sprintf("its description has spec=%s, want spec=%s", d.SpecHash, spec.Hash()))
		}
		if d.User != spec.User {
			diffs = append(diffs, fmt.Sprintf("its description has user=%s, want user=%s", d.User, spec.User))
		}
	}
	if acct.Disabled {
		diffs = append(diffs, "it is disabled")
	}
	if have && rec.Incomplete && rec.UniqueID != "" && rec.UniqueID != acct.UniqueID {
		diffs = append(diffs, fmt.Sprintf("its unique id is %s, but the incomplete account silo was making had %s", acct.UniqueID, rec.UniqueID))
	}
	if !have || rec.UniqueID != acct.UniqueID {
		// A lost local record, or an account recreated elsewhere:
		// verify it exactly, as a complete one.
		rec = Record{Email: acct.Email, Project: spec.Project, User: spec.User, Spec: spec,
			UniqueID: acct.UniqueID, Created: d.Created, Used: d.Used}
	}
	return rec, diffs
}

// finish verifies the account's bindings and, for an incomplete
// account, adds the missing ones. A complete account must match exactly;
// an incomplete one may only lack bindings.
func (c *Client) finish(ctx context.Context, spec Spec, rec *Record) error {
	email := spec.Email()
	member, user := Member(email), UserMember(spec.User)

	pp, err := c.getProjectPolicy(ctx, spec.Project)
	if err != nil {
		return err
	}
	held, conditional := rolesOf(pp, member)
	want := map[string]bool{}
	for _, r := range spec.Roles {
		want[r] = true
	}
	var diffs, missing []string
	for _, r := range conditional {
		diffs = append(diffs, fmt.Sprintf("it holds %s under a condition on %s", r, spec.Project))
	}
	for _, r := range keys(held) {
		if !want[r] {
			diffs = append(diffs, fmt.Sprintf("it holds %s on %s, which the spec does not have", r, spec.Project))
		}
	}
	for _, r := range spec.Roles {
		if !held[r] {
			missing = append(missing, r)
		}
	}

	ap, err := c.getAccountPolicy(ctx, spec.Project, email)
	if err != nil {
		return err
	}
	userIsCreator := false
	for _, b := range ap.Bindings {
		for _, m := range b.Members {
			switch {
			case b.Role == TokenCreator && m == user && b.Condition == nil:
				userIsCreator = true
			case b.Role == TokenCreator:
				diffs = append(diffs, fmt.Sprintf("%s holds %s on it; only %s may", m, TokenCreator, user))
			default:
				diffs = append(diffs, fmt.Sprintf("%s holds %s on it; its own policy may only make %s a token creator", m, b.Role, user))
			}
		}
	}

	if !rec.Incomplete {
		for _, r := range missing {
			diffs = append(diffs, fmt.Sprintf("it does not hold %s on %s", r, spec.Project))
		}
		if !userIsCreator {
			diffs = append(diffs, fmt.Sprintf("%s does not hold %s on it", user, TokenCreator))
		}
	}
	if len(diffs) > 0 {
		return &MismatchError{Email: email, Project: spec.Project, Diffs: diffs}
	}

	for _, role := range missing {
		if err := c.bindProject(ctx, spec.Project, role, member); err != nil {
			return c.stopped(rec, err)
		}
		rec.mark(stepBindPrefix + role)
		if err := c.Store.Put(*rec); err != nil {
			return err
		}
	}
	if !userIsCreator {
		if err := c.bindAccount(ctx, spec.Project, email, TokenCreator, user); err != nil {
			return c.stopped(rec, err)
		}
		rec.mark(stepTokenCreator)
		if err := c.Store.Put(*rec); err != nil {
			return err
		}
	}
	return nil
}

// stopped reports a setup failure part way through, saying what was done
// (design §11.7).
func (c *Client) stopped(rec *Record, err error) error {
	done := append([]string(nil), rec.Done...)
	sort.Strings(done)
	return fmt.Errorf("setting up %s stopped after %s; the next run resumes it: %w", rec.Email, list(done), err)
}

func (c *Client) getProjectPolicy(ctx context.Context, project string) (*Policy, error) {
	var p *Policy
	err := c.retry(ctx, false, func() (err error) {
		p, err = c.API.GetProjectPolicy(ctx, project)
		return err
	})
	return p, explain("reading the IAM policy of "+project, err)
}

func (c *Client) getAccountPolicy(ctx context.Context, project, email string) (*Policy, error) {
	var p *Policy
	err := c.retry(ctx, false, func() (err error) {
		p, err = c.API.GetAccountPolicy(ctx, project, email)
		return err
	})
	return p, explain("reading the IAM policy of "+email, err)
}

// bindProject adds member to role on the project: a read-modify-write
// with the policy's etag, retried on conflict.
func (c *Client) bindProject(ctx context.Context, project, role, member string) error {
	return explain(fmt.Sprintf("granting %s on %s to %s", role, project, member), c.retry(ctx, true, func() error {
		p, err := c.API.GetProjectPolicy(ctx, project)
		if err != nil {
			return err
		}
		if !addMember(p, role, member) {
			return nil
		}
		return c.API.SetProjectPolicy(ctx, project, p)
	}))
}

// bindAccount adds member to role on the account's own policy.
func (c *Client) bindAccount(ctx context.Context, project, email, role, member string) error {
	return explain(fmt.Sprintf("granting %s on %s to %s", role, email, member), c.retry(ctx, true, func() error {
		p, err := c.API.GetAccountPolicy(ctx, project, email)
		if err != nil {
			return err
		}
		if !addMember(p, role, member) {
			return nil
		}
		return c.API.SetAccountPolicy(ctx, project, email, p)
	}))
}

// unbindProject removes every binding of email from the project policy,
// including the deleted:serviceAccount: form a deleted account's
// bindings take.
func (c *Client) unbindProject(ctx context.Context, project, email string) error {
	member := Member(email)
	deleted := "deleted:" + member + "?uid="
	return explain(fmt.Sprintf("removing %s's bindings from %s", email, project), c.retry(ctx, true, func() error {
		p, err := c.API.GetProjectPolicy(ctx, project)
		if err != nil {
			return err
		}
		if !removeMember(p, func(m string) bool { return m == member || strings.HasPrefix(m, deleted) }) {
			return nil
		}
		return c.API.SetProjectPolicy(ctx, project, p)
	}))
}
