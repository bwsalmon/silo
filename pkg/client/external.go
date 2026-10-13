package client

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// RepoExternal is where a repo is in the uber repo (local-git.md, §9).
type RepoExternal struct {
	Repo          string       `json:"repo"`
	ID            string       `json:"id"`
	Silo          string       `json:"silo"`
	SiloID        string       `json:"silo_id"`
	State         string       `json:"state"`
	From          string       `json:"from,omitempty"`
	LastError     string       `json:"last_error,omitempty"`
	MirroredTime  *time.Time   `json:"mirrored_time,omitempty"`
	URL           string       `json:"url,omitempty"`
	Branches      string       `json:"branches,omitempty"`
	Tags          string       `json:"tags,omitempty"`
	Archives      string       `json:"archives,omitempty"`
	ArchiveRefs   int          `json:"archive_refs"`
	DefaultBranch string       `json:"default_branch,omitempty"`
	Index         string       `json:"index,omitempty"`
	Ref           *ExternalRef `json:"ref,omitempty"`
}

// ExternalRef is one of a repo's refs in both views.
type ExternalRef struct {
	Local    string `json:"local"`
	External string `json:"external"`
	Commit   string `json:"commit,omitempty"`
}

// RefWhich is the repo an uber-repo ref belongs to.
type RefWhich struct {
	Ref    string `json:"ref"`
	Kind   string `json:"kind"`
	Repo   string `json:"repo"`
	Silo   string `json:"silo"`
	Local  string `json:"local,omitempty"`
	Exists bool   `json:"exists"`
	ID     string `json:"id,omitempty"`
	SiloID string `json:"silo_id,omitempty"`
	From   string `json:"from,omitempty"`
	Commit string `json:"commit,omitempty"`
}

// RepoExternal reads where ref's repo name is in the uber repo, and,
// with branch or tag, that one ref.
func (c *Client) RepoExternal(ctx context.Context, ref, name, branch, tag string) (*RepoExternal, error) {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if branch != "" {
		q.Set("branch", branch)
	}
	if tag != "" {
		q.Set("tag", tag)
	}
	var out RepoExternal
	if err := c.Do(ctx, http.MethodGet, p+"/repos/"+url.PathEscape(name)+"/external", q, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WhichRef finds the repo an uber-repo ref belongs to.
func (c *Client) WhichRef(ctx context.Context, ref string) (*RefWhich, error) {
	var out RefWhich
	if err := c.Do(ctx, http.MethodGet, "mirror:which", url.Values{"ref": {ref}}, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
