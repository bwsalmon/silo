package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// Repo is a silo repo (local-git.md, §2 "The object", call L41).
type Repo struct {
	Name          string     `json:"name"`
	ID            string     `json:"id"`
	Silo          string     `json:"silo"`
	DefaultBranch string     `json:"default_branch"`
	CloneURL      string     `json:"clone_url"`
	CloneURLByID  string     `json:"clone_url_by_id"`
	SizeBytes     int64      `json:"size_bytes"`
	CreateTime    time.Time  `json:"create_time"`
	Mirror        RepoMirror `json:"mirror"`
}

// RepoMirror is where a repo stands in the uber repo: OFF where the
// effective repos policy keeps it out (From names the silo that does),
// UNCONFIGURED where there is no uber repo yet.
type RepoMirror struct {
	State          string  `json:"state"`
	ExternalPrefix string  `json:"external_prefix,omitempty"`
	From           string  `json:"from,omitempty"`
	LastError      *string `json:"last_error"`
}

// RepoSeed names a repo, by path, for a new one to be copied from.
type RepoSeed struct {
	Repo string `json:"repo"`
}

// RepoRequest is POST .../repos.
type RepoRequest struct {
	Name          string    `json:"name"`
	DefaultBranch string    `json:"default_branch,omitempty"`
	Seed          *RepoSeed `json:"seed,omitempty"`
}

// RelativeSilo is ref as a silo reference for the repo commands, whose
// paths are relative to the caller's own silo unless they start with
// "/": "" and "self" are the caller's, "/..." a path, "-id" an id, and
// anything else a path below the caller's silo.
func (c *Client) RelativeSilo(ctx context.Context, ref string) (string, error) {
	switch {
	case ref == "" || ref == "self" || strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, "-"):
		return ref, nil
	}
	self, err := c.Describe(ctx, "self", false)
	if err != nil {
		return "", err
	}
	return path.Join(self.Path, ref), nil
}

// SplitRepo reads a repo reference: its last segment is the repo's
// name, and what comes before it the silo (RelativeSilo), "self" when
// there is nothing before it.
func (c *Client) SplitRepo(ctx context.Context, ref string) (silo, name string, err error) {
	dir, name := path.Split(strings.TrimSuffix(ref, ".git"))
	if name == "" {
		return "", "", fmt.Errorf("repo %q: want [silo/]name", ref)
	}
	switch {
	case dir == "":
		return "self", name, nil
	case dir == "/":
		return "/", name, nil
	}
	silo, err = c.RelativeSilo(ctx, strings.TrimSuffix(dir, "/"))
	return silo, name, err
}

// CreateRepo makes a repo in ref's silo.
func (c *Client) CreateRepo(ctx context.Context, ref string, req RepoRequest, m Mutation) (*Repo, error) {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out Repo
	if err := c.Do(ctx, http.MethodPost, p+"/repos", m.query(nil), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Repos lists ref's repos, every page of them, and with recursive its
// descendants' too.
func (c *Client) Repos(ctx context.Context, ref string, recursive bool) ([]*Repo, error) {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	base := url.Values{}
	if recursive {
		base.Set("recursive", "true")
	}
	var out []*Repo
	err = c.pages(base, 0, func(q url.Values) (string, int, error) {
		var page struct {
			Repos         []*Repo `json:"repos"`
			NextPageToken string  `json:"next_page_token"`
		}
		if err := c.Do(ctx, http.MethodGet, p+"/repos", q, nil, &page); err != nil {
			return "", 0, err
		}
		out = append(out, page.Repos...)
		return page.NextPageToken, len(page.Repos), nil
	})
	return out, err
}

// Repo reads ref's repo name.
func (c *Client) Repo(ctx context.Context, ref, name string) (*Repo, error) {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out Repo
	if err := c.Do(ctx, http.MethodGet, p+"/repos/"+url.PathEscape(name), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PatchRepo changes ref's repo name's default branch.
func (c *Client) PatchRepo(ctx context.Context, ref, name, defaultBranch string, m Mutation) (*Repo, error) {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out Repo
	if err := c.Do(ctx, http.MethodPatch, p+"/repos/"+url.PathEscape(name), m.query(nil),
		map[string]string{"default_branch": defaultBranch}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteRepo deletes ref's repo name, and returns its operation.
func (c *Client) DeleteRepo(ctx context.Context, ref, name string, m Mutation) (*Operation, error) {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out Operation
	if err := c.Do(ctx, http.MethodDelete, p+"/repos/"+url.PathEscape(name), m.query(nil), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RepoArchive is one of a repo's local archive refs (local-git.md,
// "Local archive refs", call L48): a tip a force-push or a deletion
// would have orphaned.
type RepoArchive struct {
	// Name is the ref's name under refs/silo-archive/, which
	// restore-ref takes.
	Name   string    `json:"name"`
	Ref    string    `json:"ref"`
	Commit string    `json:"commit"`
	Time   time.Time `json:"time"`
}

// RepoArchives lists ref's repo name's local archive refs.
func (c *Client) RepoArchives(ctx context.Context, ref, name string) ([]RepoArchive, error) {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out struct {
		Archives []RepoArchive `json:"archives"`
	}
	if err := c.Do(ctx, http.MethodGet, p+"/repos/"+url.PathEscape(name)+"/archives", nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Archives, nil
}

// Restored is what restore-ref made.
type Restored struct {
	Repo    string `json:"repo"`
	Archive string `json:"archive"`
	Branch  string `json:"branch"`
	Commit  string `json:"commit"`
}

// RestoreRef makes branch in ref's repo name at archive's commit.
func (c *Client) RestoreRef(ctx context.Context, ref, name, archive, branch string, m Mutation) (*Restored, error) {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out Restored
	if err := c.Do(ctx, http.MethodPost, p+"/repos/"+url.PathEscape(name)+":restoreRef", m.query(nil),
		map[string]string{"archive": archive, "branch": branch}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GitConnect opens a connection that speaks git's own protocol to
// service (git-upload-pack or git-receive-pack) in ref's repo name:
// what git-remote-silo's connect capability needs. protocol is git's
// GIT_PROTOCOL, passed on if it is set.
func (c *Client) GitConnect(ctx context.Context, ref, name, service, protocol string) (io.ReadWriteCloser, error) {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	base := *c.base
	base.RawQuery, base.Fragment = "", ""
	u := base.String() + p + "/repos/" + url.PathEscape(name) + ":git?" + url.Values{"service": {service}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "git")
	req.Header.Set("User-Agent", c.ua)
	if protocol != "" {
		req.Header.Set("Git-Protocol", protocol)
	}
	// No timeout: the connection lasts as long as git needs it.
	resp, err := (&http.Client{Transport: c.hc.Transport}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusSwitchingProtocols {
		if rw, ok := resp.Body.(io.ReadWriteCloser); ok {
			return rw, nil
		}
		resp.Body.Close()
		return nil, fmt.Errorf("%s: the connection can't carry git", u)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env struct {
		Error *Error `json:"error"`
	}
	if json.Unmarshal(b, &env) == nil && env.Error != nil && env.Error.Code != "" {
		env.Error.Status = resp.StatusCode
		return nil, env.Error
	}
	return nil, &Error{Status: resp.StatusCode, Code: fmt.Sprintf("HTTP_%d", resp.StatusCode),
		Message: fmt.Sprintf("connecting to %s: %s", service, strings.TrimSpace(string(b)))}
}
