package gateway

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
)

// Page links (design §6.5). The gateway keeps the record; the bridge
// serves /p/<slug>/ from it, proxying to Host:Port (over the agent's
// `silo connect` tunnel when the agent is not on this host) or linking out
// to URL.

var (
	slugRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	hostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
)

// reservedSlugs would shadow the bridge's own routes if the UI ever
// resolved /p/<slug> relative to them.
var reservedSlugs = map[string]bool{"api": true, "acp": true, "static": true, "login": true}

// PageRegisterParams is page.register. Give Port (optionally with Host and
// ExposePort) for a service the agent runs, or URL for an external link.
type PageRegisterParams struct {
	Auth
	Slug       string `json:"slug"`
	Title      string `json:"title"`
	Port       int    `json:"port,omitempty"`
	Host       string `json:"host,omitempty"`
	ExposePort int    `json:"expose_port,omitempty"`
	URL        string `json:"url,omitempty"`
}

// PageRegister records a page link for the caller. Registering a slug the
// caller already owns replaces it; an inactive link (its owner stopped)
// may be taken over by anyone.
func (g *Gateway) PageRegister(c Caller, p PageRegisterParams) (*Page, error) {
	if !slugRe.MatchString(p.Slug) || reservedSlugs[p.Slug] {
		return nil, invalidParams("slug %q: want 1-32 lower-case letters, digits and inner dashes, not api, acp, static or login", p.Slug)
	}
	if p.Title == "" || len(p.Title) > 100 {
		return nil, invalidParams("title: must be 1-100 bytes")
	}
	pg := &Page{Slug: p.Slug, Title: p.Title, Owner: c.Agent, Active: true, Created: g.d.Clock.Now()}
	switch {
	case p.URL != "" && (p.Port != 0 || p.Host != "" || p.ExposePort != 0):
		return nil, invalidParams("give url or port, not both")
	case p.URL != "":
		u, err := url.Parse(p.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || len(p.URL) > 2048 {
			return nil, invalidParams("url %q: want an http:// or https:// URL with a host and no credentials", p.URL)
		}
		pg.URL = u.String()
	case p.Port != 0:
		if p.Port < 1 || p.Port > 65535 {
			return nil, invalidParams("port %d: want 1-65535", p.Port)
		}
		host := p.Host
		if host == "" {
			host = "127.0.0.1"
		}
		if net.ParseIP(host) == nil && !hostRe.MatchString(host) {
			return nil, invalidParams("host %q: want an IP address or a host name", host)
		}
		if p.ExposePort != 0 && (p.ExposePort < 1024 || p.ExposePort > 65535) {
			return nil, invalidParams("expose_port %d: want 1024-65535", p.ExposePort)
		}
		pg.Port, pg.Host, pg.ExposePort = p.Port, host, p.ExposePort
	default:
		return nil, invalidParams("give port (an internal service) or url (an external link)")
	}
	owned := 0
	for _, o := range g.st.Pages {
		if !o.Active || o.Slug == p.Slug {
			continue
		}
		if o.Owner == c.Agent {
			owned++
		}
		if pg.ExposePort != 0 && o.ExposePort == pg.ExposePort {
			return nil, &Error{Code: CodeConflict, Message: fmt.Sprintf("port %d is already exposed for page %q", pg.ExposePort, o.Slug)}
		}
	}
	if !c.Human() && owned >= maxPages {
		return nil, &Error{Code: CodeConflict, Message: fmt.Sprintf("agent %q already has %d page links", c.Agent, owned)}
	}
	if old := g.st.Pages[p.Slug]; old != nil && old.Active && old.Owner != c.Agent {
		return nil, &Error{Code: CodeConflict, Message: fmt.Sprintf("page %q belongs to %s", p.Slug, ownerName(old.Owner))}
	}
	g.st.Pages[p.Slug] = pg
	if err := g.saveLocked(); err != nil {
		return nil, err
	}
	g.audit(c.Actor(), EvPageRegister, pg)
	return pg, nil
}

func ownerName(owner string) string {
	if owner == "" {
		return "the human"
	}
	return "agent " + owner
}

// PageUnregisterParams is page.unregister.
type PageUnregisterParams struct {
	Auth
	Slug string `json:"slug"`
}

// PageUnregister removes a page link. The owner, an ancestor of the owner,
// or the human may remove it.
func (g *Gateway) PageUnregister(c Caller, p PageUnregisterParams) (*Page, error) {
	pg := g.st.Pages[p.Slug]
	if pg == nil || !g.mayManagePage(c, pg) {
		return nil, &Error{Code: CodeNotFound, Message: fmt.Sprintf("no page %q", p.Slug)}
	}
	delete(g.st.Pages, p.Slug)
	if err := g.saveLocked(); err != nil {
		return nil, err
	}
	g.audit(c.Actor(), EvPageUnregister, map[string]any{"slug": pg.Slug, "owner": pg.Owner})
	return pg, nil
}

func (g *Gateway) mayManagePage(c Caller, pg *Page) bool {
	return c.Human() || (c.Agent != "" && (pg.Owner == c.Agent || g.isDescendant(pg.Owner, c.Agent)))
}

// PageListParams is page.list.
type PageListParams struct {
	Auth
	// All includes inactive links.
	All bool `json:"all,omitempty"`
}

// PageList returns the links the caller may see, by slug: the human sees
// all of them, an agent its own and its descendants'.
func (g *Gateway) PageList(c Caller, p PageListParams) ([]Page, error) {
	out := []Page{}
	for _, pg := range g.st.Pages {
		if (pg.Active || p.All) && g.mayManagePage(c, pg) {
			out = append(out, *pg)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}
