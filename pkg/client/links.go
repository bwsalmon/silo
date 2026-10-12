package client

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// LinkRule is one protocol a link opens, with its ports for tcp and
// udp (none meaning every port).
type LinkRule struct {
	Protocol string   `json:"protocol"`
	Ports    []string `json:"ports,omitempty"`
}

// Link is an L3 link, as one side sees it (fence-api.md, "Links (L3)",
// SV-15): its own proposal, and the link's state.
type Link struct {
	ID         string     `json:"id,omitempty"` // the link's id, once both sides agree
	Name       string     `json:"name"`
	Silo       string     `json:"silo"`
	Peer       string     `json:"peer"`
	Direction  string     `json:"direction"` // outbound, inbound or both
	Rules      []LinkRule `json:"rules"`
	State      string     `json:"state"` // PENDING, ACTIVE or SUSPENDED
	FenceRules []string   `json:"fence_rules,omitempty"`
	CreateTime time.Time  `json:"create_time"`
}

// LinkRequest is POST .../links.
type LinkRequest struct {
	Name      string     `json:"name"`
	Peer      string     `json:"peer"`
	Direction string     `json:"direction"`
	Rules     []LinkRule `json:"rules"`
}

// ProposeLink proposes ref's side of a link: the caller's own ("self"),
// or a descendant's, on its behalf. It is ACTIVE once the peer has
// proposed the mirror image.
func (c *Client) ProposeLink(ctx context.Context, ref string, req LinkRequest, m Mutation) (*Link, error) {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out Link
	if err := c.Do(ctx, http.MethodPost, p+"/links", m.query(nil), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteLink ends ref's link name, for both sides.
func (c *Client) DeleteLink(ctx context.Context, ref, name string, m Mutation) (*Link, error) {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out Link
	if err := c.Do(ctx, http.MethodDelete, p+"/links/"+url.PathEscape(name), m.query(nil), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Links lists ref's links, every page of them.
func (c *Client) Links(ctx context.Context, ref string) ([]*Link, error) {
	var out []*Link
	err := c.grantList(ctx, ref, "links", func(page *grantPage) int {
		out = append(out, page.Links...)
		return len(page.Links)
	})
	return out, err
}
