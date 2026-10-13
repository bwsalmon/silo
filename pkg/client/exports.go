package client

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Export is an export, as its exporter sees it (fence-api.md,
// "Exports (proxied)", SV-14).
type Export struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Exporter      string     `json:"exporter"`
	ExporterPath  string     `json:"exporter_path"`
	Owner         string     `json:"owner"`
	Protocol      string     `json:"protocol"`
	Target        any        `json:"target"` // "ip:port", or {instance, zone, port}
	AddressIP     string     `json:"address_ip,omitempty"`
	ProxyProtocol bool       `json:"proxy_protocol"`
	Repos         []string   `json:"repos,omitempty"` // a registry export's repo names and prefixes
	To            []ExportTo `json:"to"`
	CreateTime    time.Time  `json:"create_time"`
}

// ExportTo is one silo an export is offered to, and how far it got.
type ExportTo struct {
	Silo    string `json:"silo"`
	State   string `json:"state"` // PENDING, ACTIVE or SUSPENDED
	Import  string `json:"import,omitempty"`
	Address string `json:"address,omitempty"`
}

// Import is an accepted export, as its importer sees it, or an offer
// not yet accepted (State PENDING, no ID).
type Import struct {
	ID            string   `json:"id,omitempty"`
	Export        string   `json:"export"` // the exporter's path and the export's name
	ExportID      string   `json:"export_id"`
	Exporter      string   `json:"exporter"`
	Importer      string   `json:"importer"`
	Name          string   `json:"name"`
	Protocol      string   `json:"protocol"`
	ProxyProtocol bool     `json:"proxy_protocol"`
	State         string   `json:"state"`
	Address       string   `json:"address,omitempty"`
	Port          int      `json:"port,omitempty"`
	Repos         []string `json:"repos,omitempty"` // a registry import's names to pull by
}

// ExportTarget is an instance target.
type ExportTarget struct {
	Instance string `json:"instance"`
	Zone     string `json:"zone"`
	Port     int    `json:"port"`
}

// OfferRequest is POST .../exports. Target is "ip:port" or an
// ExportTarget; a registry export has none, and names Repos instead.
type OfferRequest struct {
	Name          string   `json:"name"`
	Target        any      `json:"target,omitempty"`
	Protocol      string   `json:"protocol"`
	To            []string `json:"to"`
	ProxyProtocol bool     `json:"proxy_protocol,omitempty"`
	Repos         []string `json:"repos,omitempty"`
}

// OfferExport offers an export from ref's silo: the caller's own
// ("self"), or a descendant's, on its behalf.
func (c *Client) OfferExport(ctx context.Context, ref string, req OfferRequest, m Mutation) (*Export, error) {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out Export
	if err := c.Do(ctx, http.MethodPost, p+"/exports", m.query(nil), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WithdrawExport withdraws ref's export name: for everyone, or, with
// to, for that silo alone.
func (c *Client) WithdrawExport(ctx context.Context, ref, name, to string, m Mutation) (*Export, error) {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	p += "/exports/" + url.PathEscape(name)
	if to != "" {
		segs := strings.Split(strings.Trim(to, "/"), "/")
		for i, s := range segs {
			segs[i] = url.PathEscape(s)
		}
		p += "/to/" + strings.Join(segs, "/")
	}
	var out Export
	if err := c.Do(ctx, http.MethodDelete, p, m.query(nil), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AcceptExport accepts the export named "<exporter path>/<name>" as
// ref's silo, and returns the import with its address.
func (c *Client) AcceptExport(ctx context.Context, ref, export string, m Mutation) (*Import, error) {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out Import
	if err := c.Do(ctx, http.MethodPost, p+"/imports", m.query(nil), map[string]string{"export": export}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteImport gives up one of ref's imports.
func (c *Client) DeleteImport(ctx context.Context, ref, id string, m Mutation) (*Import, error) {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out Import
	if err := c.Do(ctx, http.MethodDelete, p+"/imports/"+url.PathEscape(id), m.query(nil), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Exports lists ref's exports, every page of them.
func (c *Client) Exports(ctx context.Context, ref string) ([]*Export, error) {
	var out []*Export
	err := c.grantList(ctx, ref, "exports", func(page *grantPage) int {
		out = append(out, page.Exports...)
		return len(page.Exports)
	})
	return out, err
}

// Imports lists ref's imports.
func (c *Client) Imports(ctx context.Context, ref string) ([]*Import, error) {
	var out []*Import
	err := c.grantList(ctx, ref, "imports", func(page *grantPage) int {
		out = append(out, page.Imports...)
		return len(page.Imports)
	})
	return out, err
}

// Offers lists the exports offered to ref and not yet accepted.
func (c *Client) Offers(ctx context.Context, ref string) ([]*Import, error) {
	var out []*Import
	err := c.grantList(ctx, ref, "offers", func(page *grantPage) int {
		out = append(out, page.Offers...)
		return len(page.Offers)
	})
	return out, err
}

type grantPage struct {
	Exports       []*Export `json:"exports"`
	Imports       []*Import `json:"imports"`
	Offers        []*Import `json:"offers"`
	Links         []*Link   `json:"links"`
	NextPageToken string    `json:"next_page_token"`
}

func (c *Client) grantList(ctx context.Context, ref, what string, add func(*grantPage) int) error {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return err
	}
	return c.pages(nil, 0, func(q url.Values) (string, int, error) {
		var page grantPage
		if err := c.Do(ctx, http.MethodGet, p+"/"+what, q, nil, &page); err != nil {
			return "", 0, err
		}
		return page.NextPageToken, add(&page), nil
	})
}
