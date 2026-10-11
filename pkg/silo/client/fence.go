package client

import (
	"context"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// Describe reads a silo (SV-6). With subtree, its descendants come in
// Children, recursively (ancestors only).
func (c *Client) Describe(ctx context.Context, ref string, subtree bool) (*Silo, error) {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	var q url.Values
	if subtree {
		q = url.Values{"view": {"subtree"}}
	}
	var out Silo
	if err := c.Do(ctx, http.MethodGet, p, q, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Children lists a silo's direct children, every page of them.
func (c *Client) Children(ctx context.Context, ref string) ([]*Silo, error) {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out []*Silo
	err = c.pages(nil, 0, func(q url.Values) (string, int, error) {
		var page struct {
			Silos         []*Silo `json:"silos"`
			NextPageToken string  `json:"next_page_token"`
		}
		if err := c.Do(ctx, http.MethodGet, p+"/children", q, nil, &page); err != nil {
			return "", 0, err
		}
		out = append(out, page.Silos...)
		return page.NextPageToken, len(page.Silos), nil
	})
	return out, err
}

// pages calls fetch with each page's query until there is no next page
// or limit (if not 0) items have been read.
func (c *Client) pages(base url.Values, limit int, fetch func(q url.Values) (string, int, error)) error {
	tok, n := "", 0
	for {
		q := url.Values{}
		for k, v := range base {
			q[k] = v
		}
		if tok != "" {
			q.Set("page_token", tok)
		}
		if limit > 0 {
			q.Set("page_size", strconv.Itoa(min(limit-n, 1000)))
		}
		next, got, err := fetch(q)
		if err != nil {
			return err
		}
		n += got
		if next == "" || (limit > 0 && n >= limit) || got == 0 {
			return nil
		}
		tok = next
	}
}

// Create makes the silo at childPath ("/team-a/ci"): req.Name, if
// empty, is the path's last segment, and the call goes to its parent
// (SV-7).
func (c *Client) Create(ctx context.Context, childPath string, req CreateRequest, m Mutation) (*Silo, error) {
	if !strings.HasPrefix(childPath, "/") || strings.Trim(childPath, "/") == "" {
		return nil, &Error{Code: "INVALID_ARGUMENT", Message: "create needs the new silo's path, such as /team-a/ci"}
	}
	clean := "/" + strings.Trim(childPath, "/")
	parent, name := path.Split(clean)
	if req.Name == "" {
		req.Name = name
	}
	p, err := c.SiloPath(ctx, path.Clean(parent))
	if err != nil {
		return nil, err
	}
	var out Silo
	if err := c.Do(ctx, http.MethodPost, p+"/children", m.query(nil), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Patch changes a silo's allocations and policies (SV-9).
func (c *Client) Patch(ctx context.Context, ref string, body Patch, m Mutation) (*Silo, error) {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out Silo
	if err := c.Do(ctx, http.MethodPatch, p, m.query(nil), map[string]any(body), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Delete starts deleting a silo, and with force its subtree (SV-8). It
// returns the operation; WaitOperation follows it. A validate_only call
// returns the silos it would delete instead.
func (c *Client) Delete(ctx context.Context, ref string, force bool, m Mutation) (*DeleteResult, error) {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if force {
		q.Set("force", "true")
	}
	var out DeleteResult
	if err := c.Do(ctx, http.MethodDelete, p, m.query(q), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Operation reads a long-running operation by its id or its name
// ("operations/<id>").
func (c *Client) Operation(ctx context.Context, id string) (*Operation, error) {
	id = strings.TrimPrefix(id, "operations/")
	var out Operation
	if err := c.Do(ctx, http.MethodGet, "operations/"+url.PathEscape(id), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WaitOperation polls an operation every interval until it is done or
// has failed, and returns it. A failed operation is returned with its
// error, which is also the error returned.
func (c *Client) WaitOperation(ctx context.Context, id string, interval time.Duration) (*Operation, error) {
	if interval <= 0 {
		interval = time.Second
	}
	for {
		op, err := c.Operation(ctx, id)
		if err != nil {
			return nil, err
		}
		if op.State == OpDeleteFailed || (op.Done && op.Error != nil) {
			e := op.Error
			if e == nil {
				e = &Error{Code: "INTERNAL", Message: "the operation failed without saying why"}
			}
			return op, e
		}
		if op.Done {
			return op, nil
		}
		select {
		case <-ctx.Done():
			return op, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// Grow widens one of a silo's blocks, or with AddBlock adds one of the
// new size if widening fails (SV-9).
func (c *Client) Grow(ctx context.Context, ref string, req GrowRequest, m Mutation) (*Silo, error) {
	return c.action(ctx, ref, "grow", req, m)
}

// AddBlock adds a block to a silo: a prefix length, or a range.
func (c *Client) AddBlock(ctx context.Context, ref string, want AddressWant, m Mutation) (*Silo, error) {
	return c.action(ctx, ref, "addBlock", want, m)
}

// ReleaseBlock gives a free block of a silo back to its parent.
func (c *Client) ReleaseBlock(ctx context.Context, ref, block string, m Mutation) (*Silo, error) {
	return c.action(ctx, ref, "releaseBlock", map[string]string{"block": block}, m)
}

func (c *Client) action(ctx context.Context, ref, verb string, body any, m Mutation) (*Silo, error) {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out Silo
	if err := c.Do(ctx, http.MethodPost, p+":"+verb, m.query(nil), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PreviewQuota prices a facade call or an allocation change and says
// whether it fits, changing nothing (SV-10).
func (c *Client) PreviewQuota(ctx context.Context, req PreviewRequest) (*PreviewResponse, error) {
	var out PreviewResponse
	if err := c.Do(ctx, http.MethodPost, "quota:preview", nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Members lists the identities mapped to a silo (SV-25).
func (c *Client) Members(ctx context.Context, ref string) ([]Member, error) {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out []Member
	err = c.pages(nil, 0, func(q url.Values) (string, int, error) {
		var page struct {
			Members       []Member `json:"members"`
			NextPageToken string   `json:"next_page_token"`
		}
		if err := c.Do(ctx, http.MethodGet, p+"/members", q, nil, &page); err != nil {
			return "", 0, err
		}
		out = append(out, page.Members...)
		return page.NextPageToken, len(page.Members), nil
	})
	return out, err
}

// Member reads one identity's mapping to a silo.
func (c *Client) Member(ctx context.Context, ref, identity string) (*Member, error) {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out Member
	if err := c.Do(ctx, http.MethodGet, p+"/members/"+url.PathEscape(identity), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PutMember maps identity to a silo, moving it if it was mapped
// elsewhere.
func (c *Client) PutMember(ctx context.Context, ref, identity string, m Mutation) (*Member, error) {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out Member
	if err := c.Do(ctx, http.MethodPut, p+"/members/"+url.PathEscape(identity), m.query(nil), struct{}{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteMember unmaps identity from a silo.
func (c *Client) DeleteMember(ctx context.Context, ref, identity string, m Mutation) error {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return err
	}
	return c.Do(ctx, http.MethodDelete, p+"/members/"+url.PathEscape(identity), m.query(nil), nil, nil)
}

// Audit reads a silo's audit records, oldest first (SV-26).
func (c *Client) Audit(ctx context.Context, ref string, aq AuditQuery) ([]AuditRecord, error) {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	base := url.Values{}
	if !aq.Since.IsZero() {
		base.Set("since", aq.Since.UTC().Format(time.RFC3339))
	}
	if aq.Kind != "" {
		base.Set("kind", aq.Kind)
	}
	if aq.Subtree {
		base.Set("subtree", "true")
	}
	out := []AuditRecord{}
	err = c.pages(base, aq.Limit, func(q url.Values) (string, int, error) {
		var page struct {
			Records       []AuditRecord `json:"records"`
			NextPageToken string        `json:"next_page_token"`
		}
		if err := c.Do(ctx, http.MethodGet, p+"/audit", q, nil, &page); err != nil {
			return "", 0, err
		}
		out = append(out, page.Records...)
		return page.NextPageToken, len(page.Records), nil
	})
	if aq.Limit > 0 && len(out) > aq.Limit {
		out = out[:aq.Limit]
	}
	return out, err
}

// ReconcileMode is silod's reconcile mode: "apply", or "report-only"
// after install adopted a restored database (state.md "Backups and
// recovery", SV-54). Only the root silo's callers may read it.
func (c *Client) ReconcileMode(ctx context.Context) (string, error) {
	var out struct {
		Mode string `json:"mode"`
	}
	err := c.Do(ctx, http.MethodGet, "reconcile", nil, nil, &out)
	return out.Mode, err
}

// ReconcileApply leaves report-only mode: the reconcile deletes orphans
// again once their grace period has passed. It returns the mode after.
// Only the root silo's callers may call it.
func (c *Client) ReconcileApply(ctx context.Context, m Mutation) (string, error) {
	var out struct {
		Mode string `json:"mode"`
	}
	err := c.Do(ctx, http.MethodPost, "reconcile:apply", m.query(nil), struct{}{}, &out)
	return out.Mode, err
}
