package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// RegistryHost is the local registry's name in silo-net
// (local-registry.md, "The address", call L56).
const RegistryHost = "registry.silo.internal"

// ImageRepo is an image repo (local-registry.md, §3, call L58).
type ImageRepo struct {
	Name         string    `json:"name"`
	ID           string    `json:"id"`
	Silo         string    `json:"silo"`
	State        string    `json:"state"`
	Ref          string    `json:"ref"`
	RefByID      string    `json:"ref_by_id"`
	Tags         int       `json:"tags"`
	Manifests    int       `json:"manifests"`
	ChargedBytes int64     `json:"charged_bytes"`
	CreateTime   time.Time `json:"create_time"`
	LastPushTime time.Time `json:"last_push_time"`
}

// ImageTag is one tag of an image repo, and the digest it points at.
type ImageTag struct {
	Tag    string `json:"tag"`
	Digest string `json:"digest"`
}

// ImageManifest is a manifest an image repo holds, tagged or not, with
// its charged size. Referenced is set when another manifest of the repo
// references it, as an index does its children.
type ImageManifest struct {
	Digest       string    `json:"digest"`
	MediaType    string    `json:"media_type"`
	ChargedBytes int64     `json:"charged_bytes"`
	Tags         []string  `json:"tags"`
	Referenced   bool      `json:"referenced"`
	CreateTime   time.Time `json:"create_time"`
}

// ImageTags is GET .../imageRepos/{repo}/tags.
type ImageTags struct {
	Tags      []ImageTag      `json:"tags"`
	Manifests []ImageManifest `json:"manifests"`
}

// SplitImageRef reads an image repo reference, [silo/]name[:tag|@digest]:
// the repo as SplitRepo reads it, and the tag or digest after it, if
// any.
func (c *Client) SplitImageRef(ctx context.Context, ref string) (silo, name, tag, digest string, err error) {
	repo := ref
	if i := strings.Index(repo, "@"); i >= 0 {
		repo, digest = repo[:i], repo[i+1:]
	} else if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo, tag = repo[:i], repo[i+1:]
	}
	if (digest == "" && strings.HasSuffix(ref, "@")) || (tag == "" && strings.HasSuffix(ref, ":")) {
		return "", "", "", "", fmt.Errorf("image repo %q: want [silo/]name[:tag|@digest]", ref)
	}
	silo, name, err = c.SplitRepo(ctx, repo)
	return silo, name, tag, digest, err
}

func (c *Client) imageRepoPath(ctx context.Context, ref, name string) (string, error) {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return "", err
	}
	return p + "/imageRepos/" + url.PathEscape(name), nil
}

// ImageRepos lists ref's image repos, every page of them, and with
// recursive its descendants' too.
func (c *Client) ImageRepos(ctx context.Context, ref string, recursive bool) ([]*ImageRepo, error) {
	p, err := c.SiloPath(ctx, ref)
	if err != nil {
		return nil, err
	}
	base := url.Values{}
	if recursive {
		base.Set("recursive", "true")
	}
	var out []*ImageRepo
	err = c.pages(base, 0, func(q url.Values) (string, int, error) {
		var page struct {
			ImageRepos    []*ImageRepo `json:"image_repos"`
			NextPageToken string       `json:"next_page_token"`
		}
		if err := c.Do(ctx, http.MethodGet, p+"/imageRepos", q, nil, &page); err != nil {
			return "", 0, err
		}
		out = append(out, page.ImageRepos...)
		return page.NextPageToken, len(page.ImageRepos), nil
	})
	return out, err
}

// ImageRepo reads ref's image repo name.
func (c *Client) ImageRepo(ctx context.Context, ref, name string) (*ImageRepo, error) {
	p, err := c.imageRepoPath(ctx, ref, name)
	if err != nil {
		return nil, err
	}
	var out ImageRepo
	if err := c.Do(ctx, http.MethodGet, p, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ImageTags reads ref's image repo name's tags and manifests.
func (c *Client) ImageTags(ctx context.Context, ref, name string) (*ImageTags, error) {
	p, err := c.imageRepoPath(ctx, ref, name)
	if err != nil {
		return nil, err
	}
	var out ImageTags
	if err := c.Do(ctx, http.MethodGet, p+"/tags", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UntagImage removes a tag of ref's image repo name; its manifest
// stays.
func (c *Client) UntagImage(ctx context.Context, ref, name, tag string, m Mutation) (*ImageRepo, error) {
	p, err := c.imageRepoPath(ctx, ref, name)
	if err != nil {
		return nil, err
	}
	var out ImageRepo
	if err := c.Do(ctx, http.MethodDelete, p+"/tags/"+url.PathEscape(tag), m.query(nil), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteImageManifest deletes a manifest of ref's image repo name and
// every tag on it.
func (c *Client) DeleteImageManifest(ctx context.Context, ref, name, digest string, m Mutation) (*ImageRepo, error) {
	p, err := c.imageRepoPath(ctx, ref, name)
	if err != nil {
		return nil, err
	}
	var out ImageRepo
	if err := c.Do(ctx, http.MethodDelete, p+"/manifests/"+url.PathEscape(digest), m.query(nil), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteImageRepo deletes ref's image repo name and everything in it,
// and returns its operation.
func (c *Client) DeleteImageRepo(ctx context.Context, ref, name string, m Mutation) (*Operation, error) {
	p, err := c.imageRepoPath(ctx, ref, name)
	if err != nil {
		return nil, err
	}
	var out Operation
	if err := c.Do(ctx, http.MethodDelete, p, m.query(nil), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ImageRef is the name to push to and pull from silo siloPath's image
// repo name: registry.silo.internal/<silo path>/<repo>, or by the
// silo's id, registry.silo.internal/id_<id>/<repo>, when byID is set.
// Names are absolute: there is no self form.
func ImageRef(siloPath, siloID, name string, byID bool) string {
	if byID {
		return RegistryHost + "/id_" + siloID + "/" + name
	}
	return RegistryHost + path.Join("/", siloPath, name)
}

// CacheUpstream is one upstream of the pull-through cache
// (local-registry.md, "Configuration").
type CacheUpstream struct {
	Name     string   `json:"name"`
	Endpoint string   `json:"endpoint"`
	Aliases  []string `json:"aliases,omitempty"`
}

// CacheSettings is the pull-through cache's configuration, the root's.
type CacheSettings struct {
	Upstreams   []CacheUpstream `json:"upstreams"`
	SizeLimitGB float64         `json:"size_limit_gb"`
	StaleTagMax string          `json:"stale_tag_max"`
	PublicTTL   string          `json:"public_ttl"`
	AuthTTL     string          `json:"auth_ttl"`
}

// RegistryCache reads the cache's configuration. Only the root silo's
// callers may.
func (c *Client) RegistryCache(ctx context.Context) (*CacheSettings, error) {
	var out CacheSettings
	err := c.Do(ctx, http.MethodGet, "registry/cache", nil, nil, &out)
	return &out, err
}

// SetRegistryCache replaces the cache's configuration. Only root
// members with full control may.
func (c *Client) SetRegistryCache(ctx context.Context, s CacheSettings) (*CacheSettings, error) {
	var out CacheSettings
	err := c.Do(ctx, http.MethodPut, "registry/cache", nil, s, &out)
	return &out, err
}

// CacheEviction is what silo registry cache evict did: the entries it
// dropped, the cache-only bytes that freed, and the cache-only bytes
// left against the budget.
type CacheEviction struct {
	Entries     int   `json:"entries"`
	FreedBytes  int64 `json:"freed_bytes"`
	CacheBytes  int64 `json:"cache_bytes"`
	BudgetBytes int64 `json:"budget_bytes"`
}

// EvictRegistryCache drops the cache's entries for ref,
// <upstream>[/<repo>[@digest]], now; "" is every entry. Only root
// members with full control may.
func (c *Client) EvictRegistryCache(ctx context.Context, ref string) (*CacheEviction, error) {
	var out CacheEviction
	err := c.Do(ctx, http.MethodPost, "registry/cache:evict", nil, map[string]string{"ref": ref}, &out)
	return &out, err
}
