package fenceapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/egress"
	"github.com/bwsalmon/silo/pkg/imagerepos"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// The pull-through cache's audit kinds (local-registry.md, "Audit",
// call L67): registry.cache is each request on an upstream path and
// each upstream fetch behind a facade pull, as the calling silo;
// registry.cache.set is a change to the upstream list.
const (
	KindRegistryCache    = "registry.cache"
	KindRegistryCacheSet = "registry.cache.set"
)

// SettingRegistryCache is the settings row the cache's configuration
// is kept in, as JSON (local-registry.md, "Configuration").
const SettingRegistryCache = "registry.cache"

// CacheUpstream is one configured upstream of the pull-through cache.
// Name is how references name it, and has a dot; Aliases are other
// host names that mean the same upstream; Endpoint is where silod
// sends its API requests. There is no credential, by design.
type CacheUpstream struct {
	Name     string   `json:"name"`
	Endpoint string   `json:"endpoint"`
	Aliases  []string `json:"aliases,omitempty"`
}

// CacheSettings is the cache's configuration, the root's, set by PUT
// /silo/v1/registry/cache and shown by GET (silo registry cache set
// and status). The durations are Go durations ("24h", "300s").
type CacheSettings struct {
	Upstreams   []CacheUpstream `json:"upstreams"`
	SizeLimitGB float64         `json:"size_limit_gb"`
	StaleTagMax string          `json:"stale_tag_max"`
	PublicTTL   string          `json:"public_ttl"`
	AuthTTL     string          `json:"auth_ttl"`
}

// DefaultCacheSettings is what install writes: Docker Hub, gcr.io,
// ghcr.io and quay.io.
func DefaultCacheSettings() CacheSettings {
	return CacheSettings{
		Upstreams: []CacheUpstream{
			{Name: "docker.io", Endpoint: "https://registry-1.docker.io", Aliases: []string{"index.docker.io", "registry-1.docker.io"}},
			{Name: "gcr.io", Endpoint: "https://gcr.io"},
			{Name: "ghcr.io", Endpoint: "https://ghcr.io"},
			{Name: "quay.io", Endpoint: "https://quay.io"},
		},
		SizeLimitGB: 50,
		StaleTagMax: "24h",
		PublicTTL:   "24h",
		AuthTTL:     "300s",
	}
}

// Upstream is the configured upstream host names, by its name or one
// of its aliases.
func (s CacheSettings) Upstream(host string) (CacheUpstream, bool) {
	host = strings.ToLower(host)
	for _, u := range s.Upstreams {
		if u.Name == host {
			return u, true
		}
		for _, a := range u.Aliases {
			if a == host {
				return u, true
			}
		}
	}
	return CacheUpstream{}, false
}

// StaleTagMaxDuration is StaleTagMax, or 24 hours if it doesn't parse.
func (s CacheSettings) StaleTagMaxDuration() time.Duration {
	if d, err := time.ParseDuration(s.StaleTagMax); err == nil {
		return d
	}
	return 24 * time.Hour
}

// Valid checks s: every upstream named by a host with a dot, an http
// or https endpoint with no path, query or credentials, no name or
// alias twice, and durations that parse.
func (s CacheSettings) Valid() error {
	seen := map[string]bool{}
	for i, u := range s.Upstreams {
		field := fmt.Sprintf("upstreams[%d]", i)
		for _, h := range append([]string{u.Name}, u.Aliases...) {
			if !strings.Contains(h, ".") || h != strings.ToLower(h) || h == imagerepos.Host {
				return errorf(InvalidArgument, "upstream name %q: want a lower-case host name with a dot, not %s", h, imagerepos.Host).with("field", field)
			}
			if _, err := egress.Parse(h); err != nil || strings.Contains(h, "*") {
				return errorf(InvalidArgument, "upstream name %q: want a host name", h).with("field", field)
			}
			if seen[h] {
				return errorf(InvalidArgument, "upstream name %q is given twice", h).with("field", field)
			}
			seen[h] = true
		}
		e, err := url.Parse(u.Endpoint)
		if err != nil || (e.Scheme != "https" && e.Scheme != "http") || e.Host == "" || e.User != nil ||
			strings.Trim(e.Path, "/") != "" || e.RawQuery != "" || e.Fragment != "" {
			return errorf(InvalidArgument, "upstream %s endpoint %q: want https://<host>[:<port>], with no path or credentials", u.Name, u.Endpoint).with("field", field)
		}
	}
	if s.SizeLimitGB < 0 {
		return errorf(InvalidArgument, "size_limit_gb %v: want 0 or more", s.SizeLimitGB).with("field", "size_limit_gb")
	}
	for f, v := range map[string]string{"stale_tag_max": s.StaleTagMax, "public_ttl": s.PublicTTL, "auth_ttl": s.AuthTTL} {
		if d, err := time.ParseDuration(v); err != nil || d < 0 {
			return errorf(InvalidArgument, "%s %q: want a duration such as 24h", f, v).with("field", f)
		}
	}
	return nil
}

// ReadCacheSettings is the cache's configuration, in tx: what was set,
// or DefaultCacheSettings if nothing was.
func ReadCacheSettings(ctx context.Context, tx store.Tx) (CacheSettings, error) {
	v, ok, err := store.ReadSetting(ctx, tx, SettingRegistryCache)
	if err != nil || !ok {
		return DefaultCacheSettings(), err
	}
	var s CacheSettings
	if err := json.Unmarshal([]byte(v), &s); err != nil {
		return CacheSettings{}, fmt.Errorf("setting %s: %w", SettingRegistryCache, err)
	}
	return s, nil
}

// WriteCacheSettings stores s, in tx, after checking it.
func WriteCacheSettings(ctx context.Context, tx store.Tx, s CacheSettings) error {
	if err := s.Valid(); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return store.WriteSetting(ctx, tx, SettingRegistryCache, string(b))
}

// EffectiveEgress is silo's effective egress policy, as the levels it
// is the conjunction of, root first, and the paths of the silos they
// are (gateway.md, "Policy"), as the gateway's tables carry it. The
// cache checks every request against it (local-registry.md, call L71).
func EffectiveEgress(ctx context.Context, tx store.Tx, silo string) ([]egress.Policy, []string, error) {
	chain, err := tree.Ancestry(ctx, tx, silo)
	if err != nil {
		return nil, nil, err
	}
	own, err := OwnEgress(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	var levels []egress.Policy
	var paths []string
	for _, s := range chain {
		levels = append(levels, own[s.ID])
		paths = append(paths, s.Path)
	}
	return levels, paths, nil
}

// cachePath reports whether rest is the cache's configuration: GET
// registry/cache is silo registry cache status, PUT sets it.
func cachePath(rest string) bool { return rest == "registry/cache" }

// registryCache serves registry/cache, for root members with full
// control only (local-registry.md, "Configuration").
func (s *Server) registryCache(q *request) {
	ctx := q.r.Context()
	if q.r.Method != http.MethodGet && q.r.Method != http.MethodPut {
		writeError(q.w, errorf(NotFound, "registry/cache is GET or PUT"))
		return
	}
	var body CacheSettings
	if q.r.Method == http.MethodPut {
		if err := json.NewDecoder(http.MaxBytesReader(q.w, q.r.Body, 1<<20)).Decode(&body); err != nil {
			writeError(q.w, errorf(InvalidArgument, "the body: %v", err))
			return
		}
	}
	var out CacheSettings
	err := s.tx(ctx, func(tx store.Tx) error {
		root, err := tree.Root(ctx, tx)
		if err != nil {
			return err
		}
		if q.caller.Silo != root.ID || q.caller.Manage != "" {
			return errorf(PermissionDenied, "only the root silo's callers may see or change the registry cache")
		}
		if q.r.Method == http.MethodGet {
			out, err = ReadCacheSettings(ctx, tx)
			return err
		}
		eff, err := effectiveOf(ctx, tx, []*tree.Silo{root}, nil)
		if err != nil {
			return err
		}
		if eff.Control != ControlFull {
			return errorf(PermissionDenied, "setting the registry cache needs full control; the root's control policy is %s", eff.Control)
		}
		if err := WriteCacheSettings(ctx, tx, body); err != nil {
			return err
		}
		names := []string{}
		for _, u := range body.Upstreams {
			names = append(names, u.Name)
		}
		out = body
		return Audit(ctx, tx, root, q.caller, KindRegistryCacheSet, "", "OK",
			map[string]any{"upstreams": names, "stale_tag_max": body.StaleTagMax, "size_limit_gb": body.SizeLimitGB})
	})
	if err != nil {
		writeError(q.w, err)
		return
	}
	writeJSON(q.w, http.StatusOK, out)
}
