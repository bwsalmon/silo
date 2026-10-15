package gcp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"syscall"
	"time"
)

// GCOptions configure GC.
type GCOptions struct {
	// User is the invoking principal's email. GC never touches an
	// account whose description names anybody else.
	User string
	// Projects are scanned for the user's silo-* accounts besides those
	// in local state (so a lost state file strands nothing).
	Projects []string
	// Account, with exactly one Project, deletes that one account
	// whatever its ttl (silo gc --project p --account email).
	Account string
	// Alive says whether a session recorded as using an account still
	// runs; a signal-0 check of its pid when nil.
	Alive func(SessionRef) bool
}

// GCReport says what GC did.
type GCReport struct {
	Deleted []string
	// Kept are the user's accounts still within their ttl or in use.
	Kept []string
}

// GC deletes the user's accounts that have sat unused for longer than
// their ttl (design §11.5): first their project bindings, then the
// account, then the local record. An account with ttl 0 and no live
// session is expired. Accounts found only in a project, with no local
// record, get DefaultTTL. It carries on past a failure and returns
// every error it met.
func (c *Client) GC(ctx context.Context, opts GCOptions) (GCReport, error) {
	var rep GCReport
	user := strings.ToLower(opts.User)
	if user == "" {
		return rep, errors.New("silo gc: the invoking user's email is unknown")
	}
	if opts.Account != "" {
		if len(opts.Projects) != 1 {
			return rep, errors.New("silo gc --account needs exactly one --project")
		}
		err := c.gcOne(ctx, user, opts.Projects[0], opts.Account)
		if err == nil {
			rep.Deleted = append(rep.Deleted, opts.Account)
		}
		return rep, err
	}
	alive := opts.Alive
	if alive == nil {
		alive = pidAlive
	}
	st, err := c.Store.Load()
	if err != nil {
		return rep, err
	}

	type candidate struct {
		project string
		rec     *Record
		desc    *Description
	}
	cands := map[string]*candidate{}
	projects := map[string]bool{}
	for _, p := range opts.Projects {
		projects[p] = true
	}
	for i := range st.Accounts {
		r := &st.Accounts[i]
		if strings.EqualFold(r.User, user) {
			cands[r.Email] = &candidate{project: r.Project, rec: r}
			projects[r.Project] = true
		}
	}
	var errs []error
	listed := map[string]bool{}
	for _, p := range keys(projects) {
		accounts, err := c.API.ListAccounts(ctx, p)
		if err != nil {
			errs = append(errs, explain("listing service accounts in "+p, err))
			continue
		}
		listed[p] = true
		for _, a := range accounts {
			if !strings.HasPrefix(a.Email, "silo-") {
				continue
			}
			d, ok := ParseDescription(a.Description)
			if !ok || d.User != user {
				continue // not silo's, or another user's: never touched
			}
			cand := cands[a.Email]
			if cand == nil {
				cand = &candidate{project: p}
				cands[a.Email] = cand
			}
			cand.desc = &d
		}
	}

	now := c.now()
	emails := make([]string, 0, len(cands))
	for e := range cands {
		emails = append(emails, e)
	}
	sort.Strings(emails)
	for _, email := range emails {
		cand := cands[email]
		if !listed[cand.project] {
			continue
		}
		if cand.desc == nil {
			// In local state but gone from the project: deleted
			// elsewhere. Clear any bindings it left, and forget it.
			if err := c.unbindProject(ctx, cand.project, email); err != nil {
				errs = append(errs, err)
				continue
			}
			if err := c.Store.Delete(email); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		ttl := DefaultTTL
		used := cand.desc.Used
		if r := cand.rec; r != nil {
			if r.Session != nil && alive(*r.Session) {
				rep.Kept = append(rep.Kept, email)
				continue
			}
			ttl = time.Duration(r.TTL)
			if r.Used.After(used) {
				used = r.Used
			}
		}
		if ttl != 0 && now.Sub(used) <= ttl {
			rep.Kept = append(rep.Kept, email)
			continue
		}
		if err := c.delete(ctx, cand.project, email); err != nil {
			errs = append(errs, err)
			continue
		}
		rep.Deleted = append(rep.Deleted, email)
	}
	return rep, errors.Join(errs...)
}

// gcOne deletes one named account, if the user's.
func (c *Client) gcOne(ctx context.Context, user, project, email string) error {
	if !strings.HasPrefix(email, "silo-") || !strings.HasSuffix(email, "@"+project+".iam.gserviceaccount.com") {
		return fmt.Errorf("%s is not a silo account in %s", email, project)
	}
	a, err := c.API.GetAccount(ctx, project, email)
	if Code(err) == 404 {
		if err := c.unbindProject(ctx, project, email); err != nil {
			return err
		}
		return c.Store.Delete(email)
	}
	if err != nil {
		return explain("reading "+email, err)
	}
	d, ok := ParseDescription(a.Description)
	if !ok {
		return fmt.Errorf("%s's description %q is not one silo writes; silo gc leaves it alone", email, a.Description)
	}
	if d.User != user {
		return fmt.Errorf("%s belongs to %s, not %s; silo gc never touches another user's account", email, d.User, user)
	}
	return c.delete(ctx, project, email)
}

// delete removes an account's project bindings, then the account, then
// its local record.
func (c *Client) delete(ctx context.Context, project, email string) error {
	if err := c.unbindProject(ctx, project, email); err != nil {
		return err
	}
	err := c.retry(ctx, false, func() error { return c.API.DeleteAccount(ctx, project, email) })
	if err != nil && Code(err) != 404 {
		return explain("deleting "+email, err)
	}
	c.printf("silo: deleted %s\n", email)
	return c.Store.Delete(email)
}

func pidAlive(s SessionRef) bool {
	if s.PID <= 0 {
		return false
	}
	err := syscall.Kill(s.PID, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
