package client

import (
	"context"
	"net/http"
	"net/url"
)

// MirrorSettings is what silo mirror set sends (local-git.md, call
// L52). Credential is the credential file's contents.
type MirrorSettings struct {
	URL         string `json:"url"`
	Credential  string `json:"credential,omitempty"`
	Prefix      string `json:"prefix,omitempty"`
	IndexBranch string `json:"index_branch,omitempty"`
}

// MirrorStatus is the outbound mirror's configuration and the repos
// that aren't in sync (local-git.md, §8).
type MirrorStatus struct {
	Configured     bool         `json:"configured"`
	URL            string       `json:"url,omitempty"`
	Prefix         string       `json:"prefix,omitempty"`
	IndexBranch    string       `json:"index_branch,omitempty"`
	Credential     string       `json:"credential,omitempty"`
	Paused         string       `json:"paused,omitempty"`
	Refs           int          `json:"refs"`
	EstimatedBytes int64        `json:"estimated_bytes"`
	InSync         int          `json:"in_sync"`
	PendingDeletes int          `json:"pending_deletes"`
	Repos          []MirrorRepo `json:"repos"`
}

// MirrorRepo is one repo's mirror.
type MirrorRepo struct {
	ID         string        `json:"id"`
	Path       string        `json:"path"`
	State      string        `json:"state"`
	LagSeconds int64         `json:"lag_seconds,omitempty"`
	LastError  string        `json:"last_error,omitempty"`
	Diverged   []DivergedRef `json:"diverged,omitempty"`
}

// DivergedRef is an uber-repo ref whose lease failed.
type DivergedRef struct {
	Ref      string `json:"ref"`
	Local    string `json:"local"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
}

// SetMirror sets the outbound mirror. Only the root silo's callers may.
func (c *Client) SetMirror(ctx context.Context, s MirrorSettings) (*MirrorStatus, error) {
	var out MirrorStatus
	err := c.Do(ctx, http.MethodPut, "mirror", nil, s, &out)
	return &out, err
}

// MirrorStatus reads the mirror's status: the repos that aren't in
// sync, or every repo with all.
func (c *Client) MirrorStatus(ctx context.Context, all bool) (*MirrorStatus, error) {
	var q url.Values
	if all {
		q = url.Values{"all": {"true"}}
	}
	var out MirrorStatus
	err := c.Do(ctx, http.MethodGet, "mirror", q, nil, &out)
	return &out, err
}

// RetryMirror runs a pass over repo (a path, /team-a/x/foo) now, or
// retries every repo with "".
func (c *Client) RetryMirror(ctx context.Context, repo string) (*MirrorStatus, error) {
	var out MirrorStatus
	err := c.Do(ctx, http.MethodPost, "mirror:retry", nil, map[string]string{"repo": repo}, &out)
	return &out, err
}

// AdoptMirror resolves repo's diverged refs (or those of refs): archive
// the uber repo's tips and overwrite them, or keep them.
func (c *Client) AdoptMirror(ctx context.Context, repo string, refs []string, keepExternal bool) (*MirrorStatus, error) {
	var out MirrorStatus
	body := map[string]any{"repo": repo, "refs": refs, "keep_external": keepExternal}
	err := c.Do(ctx, http.MethodPost, "mirror:adopt", nil, body, &out)
	return &out, err
}
