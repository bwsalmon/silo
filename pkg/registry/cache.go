package registry

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/egress"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/imagerepos"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// The pull-through cache, for public content (local-registry.md, §8,
// calls L70, L71 and L75): one cache for the whole host, reached by
// path (registry.silo.internal/docker.io/library/alpine), by a mirror
// request's ?ns=, and behind every facade pull (PullThrough and
// FetchThrough).
//
// A silo gets a cached object only if its own egress would have let it
// fetch the object from the upstream. Each fetch records the hosts it
// used (the API host, the token realm's host and every host a blob GET
// was redirected through), and every request, hit or miss, is checked
// against the caller's effective egress policy for each of them. A miss
// is the caller's own fetch, through the gateway's egress proxy as the
// calling silo (Options.Egress), which applies the same policy to every
// hop. The upstream list is not an allowlist.
//
// Tags are revalidated on every request; digests aren't. Concurrent
// misses on one object share one upstream fetch.
//
// The index is per sharing domain (call L73): the nearest silo, the
// caller or an ancestor, that sets images.sharing isolated itself, or
// the whole host. A hit is decided by the caller's domain's entries,
// never by the content store, so a miss in an isolated domain is a
// full upstream fetch even when the bytes are on disk, and the
// duplicate is dropped at commit. Facade pulls are requests like any
// other. Private content is a later step (32).

// Egress carries the cache's upstream requests through the gateway's
// egress proxy as a silo.
type Egress interface {
	// Transport is a round tripper whose every request goes through
	// the egress proxy as silo, so that the proxy applies silo's
	// effective policy to it and audits the connection. The cache
	// follows redirects itself, each hop a request of its own. A
	// request the proxy refuses is an error wrapping *EgressRefusal, or
	// a response carrying the proxy's Silo-Egress-Refusal header.
	Transport(silo string) http.RoundTripper
}

// EgressRefusal is the egress proxy's refusal of a connection to Host.
type EgressRefusal struct {
	Host   string
	Reason string
}

func (e *EgressRefusal) Error() string {
	return fmt.Sprintf("egress to %s is refused: %s", e.Host, e.Reason)
}

// EgressRefusalHeader is the header the gateway's egress proxy puts its
// refusal in (package gateway's egressRefusal).
const EgressRefusalHeader = "Silo-Egress-Refusal"

// The roles of a recorded host.
const (
	HostAPI      = "api"
	HostToken    = "token"
	HostRedirect = "redirect"
)

// CacheHost is one host an upstream fetch used, as egress judges it:
// the host and port, and whether it was reached over TLS (CONNECT,
// whose port-less allow entries mean 443) or plain HTTP (80).
type CacheHost struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	TLS  bool   `json:"tls,omitempty"`
	Role string `json:"role"`
}

func (h CacheHost) String() string { return net.JoinHostPort(h.Host, strconv.Itoa(h.Port)) }

func (h CacheHost) def() int {
	if h.TLS {
		return egress.DefaultConnectPort
	}
	return egress.DefaultHTTPPort
}

// hostOf is u's host as egress judges it, in role.
func hostOf(u *url.URL, role string) CacheHost {
	h := CacheHost{Host: egress.Normalize(u.Hostname()), TLS: u.Scheme == "https", Role: role}
	h.Port = h.def()
	if p, err := strconv.Atoi(u.Port()); err == nil {
		h.Port = p
	}
	return h
}

// The result of a cache request, as registry.cache records it.
const (
	CacheHit   = "hit"
	CacheMiss  = "miss"
	CacheStale = "stale"
)

// The cache's tables.
const (
	kindManifest = "manifest"
	kindBlob     = "blob"
)

// manifestAccept is every manifest type the cache asks an upstream for.
var manifestAccept = strings.Join([]string{MediaOCIIndex, MediaOCIManifest, MediaDockerList, MediaDockerManifest}, ", ")

// cacheObject is one row of registry_cache_objects.
type cacheObject struct {
	digest    string
	mediaType string
	size      int64
	hosts     []CacheHost
	fetchedAt time.Time
}

// cacheIndex is where a sharing domain's entries are (call L73): the
// whole host's in registry_cache_objects and registry_cache_tags, and
// an isolated domain's in the registry_cache_domain_ tables, under its
// ID.
type cacheIndex struct {
	objects, tags string
	cols, marks   string // the domain column and its placeholder, if any
	where         string
	key           []any
}

func indexOf(domain string) cacheIndex {
	if domain == "" {
		return cacheIndex{objects: "registry_cache_objects", tags: "registry_cache_tags"}
	}
	return cacheIndex{objects: "registry_cache_domain_objects", tags: "registry_cache_domain_tags",
		cols: "domain, ", marks: "?, ", where: "domain = ? AND ", key: []any{domain}}
}

func (x cacheIndex) args(a ...any) []any { return append(append([]any(nil), x.key...), a...) }

// cacheObject is domain's entry for digest of up's repo, or nil.
func (s *Service) cacheObject(ctx context.Context, tx store.Tx, domain, up, repo, digest, kind string) (*cacheObject, error) {
	x := indexOf(domain)
	var o cacheObject
	var hosts string
	var fetched int64
	err := tx.QueryRow(ctx, `SELECT digest, media_type, size, hosts, fetched_at FROM `+x.objects+`
		WHERE `+x.where+`upstream = ? AND repo = ? AND digest = ? AND kind = ?`, x.args(up, repo, digest, kind)...).
		Scan(&o.digest, &o.mediaType, &o.size, &hosts, &fetched)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(hosts), &o.hosts); err != nil {
		return nil, fmt.Errorf("cache entry %s/%s@%s: hosts: %w", up, repo, digest, err)
	}
	o.fetchedAt = time.UnixMilli(fetched)
	return &o, nil
}

func (s *Service) putCacheObject(ctx context.Context, domain, up, repo, kind string, o *cacheObject) error {
	hosts, err := json.Marshal(o.hosts)
	if err != nil {
		return err
	}
	x := indexOf(domain)
	now := store.Millis(s.opts.Now())
	return s.opts.Store.Tx(ctx, func(tx store.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO `+x.objects+`
			(`+x.cols+`upstream, repo, digest, kind, media_type, size, hosts, fetched_at, accessed_at) VALUES (`+x.marks+`?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (`+x.cols+`upstream, repo, digest) DO UPDATE SET kind = excluded.kind, media_type = excluded.media_type,
			size = excluded.size, hosts = excluded.hosts, fetched_at = excluded.fetched_at, accessed_at = excluded.accessed_at`,
			x.args(up, repo, o.digest, kind, o.mediaType, o.size, string(hosts), now, now)...)
		return err
	})
}

// cacheTag is a tag's digest as domain last revalidated it, and when.
func (s *Service) cacheTag(ctx context.Context, tx store.Tx, domain, up, repo, tag string) (string, time.Time, bool, error) {
	x := indexOf(domain)
	var d string
	var at int64
	err := tx.QueryRow(ctx, `SELECT digest, validated_at FROM `+x.tags+` WHERE `+x.where+`upstream = ? AND repo = ? AND tag = ?`,
		x.args(up, repo, tag)...).Scan(&d, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	return d, time.UnixMilli(at), err == nil, err
}

func (s *Service) putCacheTag(ctx context.Context, domain, up, repo, tag, digest string) error {
	x := indexOf(domain)
	return s.opts.Store.Tx(ctx, func(tx store.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO `+x.tags+` (`+x.cols+`upstream, repo, tag, digest, validated_at) VALUES (`+x.marks+`?, ?, ?, ?, ?)
			ON CONFLICT (`+x.cols+`upstream, repo, tag) DO UPDATE SET digest = excluded.digest, validated_at = excluded.validated_at`,
			x.args(up, repo, tag, digest, store.Millis(s.opts.Now()))...)
		return err
	})
}

// cacheReq is one request on the cache, by a client on an upstream
// path or by a facade pull, and what registry.cache records of it.
type cacheReq struct {
	caller fenceapi.Caller
	method string
	name   imagerepos.Name
	ref    string
	// facade is a facade pull's, which may name a registry that isn't
	// a configured upstream; pullID is its image.pull record's.
	facade bool
	pullID string

	up       fenceapi.CacheUpstream
	cfg      fenceapi.CacheSettings
	levels   []egress.Policy
	paths    []string
	siloPath string
	// domain is the caller's sharing domain's ID, "" for the whole
	// host, and domainPath its path: the index it hits in and
	// records its fetches in.
	domain     string
	domainPath string

	// What the audit record says.
	result string
	digest string
	bytes  int64
	hosts  []CacheHost
	auth   string
	denied string
	status int
}

// begin reads the configuration and the caller's effective egress
// policy, and resolves the upstream: a configured one by name or
// alias, or, for a facade pull only, the registry's own host.
func (s *Service) begin(ctx context.Context, cr *cacheReq) error {
	return s.opts.Store.Tx(ctx, func(tx store.Tx) error {
		var err error
		if cr.cfg, err = fenceapi.ReadCacheSettings(ctx, tx); err != nil {
			return err
		}
		if cr.levels, cr.paths, err = fenceapi.EffectiveEgress(ctx, tx, cr.caller.Silo); err != nil {
			if errors.Is(err, tree.ErrNotFound) {
				return rerr(http.StatusForbidden, codeDenied, "silo: the caller's silo is gone")
			}
			return err
		}
		cr.siloPath = cr.paths[len(cr.paths)-1]
		if cr.domain, cr.domainPath, err = fenceapi.SharingDomain(ctx, tx, cr.caller.Silo); err != nil {
			return err
		}
		up, ok := cr.cfg.Upstream(cr.name.Upstream)
		switch {
		case ok:
			cr.up = up
		case cr.facade:
			cr.up = fenceapi.CacheUpstream{Name: cr.name.Upstream, Endpoint: "https://" + cr.name.Upstream}
		default:
			return rerr(http.StatusNotFound, codeNameUnknown, "silo: %s is not an upstream of this host's cache", cr.name.Upstream)
		}
		return nil
	})
}

// apiHost is the upstream's API host.
func (cr *cacheReq) apiHost() CacheHost {
	u, err := url.Parse(cr.up.Endpoint)
	if err != nil {
		return CacheHost{Host: cr.up.Name, Port: egress.DefaultConnectPort, TLS: true, Role: HostAPI}
	}
	return hostOf(u, HostAPI)
}

// check is the egress check every request makes: the first of hosts
// the caller's effective policy doesn't allow, or nil.
func (cr *cacheReq) check(hosts []CacheHost) *CacheHost {
	for _, h := range hosts {
		if ok, _, _ := egress.Allowed(cr.levels, h.Host, h.Port, h.def()); !ok {
			return &h
		}
	}
	return nil
}

// refuse is the 403 DENIED of a host the caller's egress doesn't
// allow (call L71).
func (cr *cacheReq) refuse(h string) error {
	cr.denied = h
	return &denied{reason: "egress", regError: rerr(http.StatusForbidden, codeDenied,
		"silo: egress to %s is not allowed for %s", h, cr.siloPath)}
}

// hit serves o if the caller's policy allows every host it was fetched
// through, and refuses it naming the first host that isn't otherwise.
// A blob refused only for a redirect host is re-checked once, against
// the hosts the upstream redirects it through now (call L71).
func (s *Service) hit(ctx context.Context, cr *cacheReq, o *cacheObject, kind string) error {
	cr.digest, cr.bytes = o.digest, o.size
	cr.hosts = o.hosts
	h := cr.check(o.hosts)
	if h == nil {
		return nil
	}
	if kind != kindBlob || h.Role != HostRedirect || cr.check(withoutRole(o.hosts, HostRedirect)) != nil {
		return cr.refuse(h.Host)
	}
	now, err := s.probeBlob(ctx, cr, o.digest)
	if err != nil {
		var r *EgressRefusal
		if errors.As(err, &r) {
			return cr.refuse(r.Host)
		}
		log.Printf("registry: cache: re-checking %s@%s's redirects for %s: %v", cr.name, o.digest, cr.siloPath, err)
		return cr.refuse(h.Host)
	}
	cr.hosts = now
	if h := cr.check(now); h != nil {
		return cr.refuse(h.Host)
	}
	o.hosts = now
	if err := s.putCacheObject(ctx, cr.domain, cr.name.Upstream, cr.name.UpstreamRepo, kindBlob, o); err != nil {
		log.Printf("registry: cache: recording %s@%s's new redirect hosts: %v", cr.name, o.digest, err)
	}
	return nil
}

func withoutRole(hosts []CacheHost, role string) []CacheHost {
	var out []CacheHost
	for _, h := range hosts {
		if h.Role != role {
			out = append(out, h)
		}
	}
	return out
}

// upstreamErr is an upstream's answer the cache doesn't serve: unknown
// is a 404, unreachable a network error, a 5xx or a 429, which a stale
// tag may stand in for.
type upstreamErr struct {
	status      int
	unreachable bool
	msg         string
}

func (e *upstreamErr) Error() string { return e.msg }

// fetcher is one caller's requests to one upstream: through the egress
// proxy as the caller, following redirects and the anonymous token
// exchange itself, and recording every host it used.
type fetcher struct {
	cr     *cacheReq
	rt     http.RoundTripper
	hosts  []CacheHost
	token  string
	auth   string
	maxHop int
}

func (s *Service) fetcherFor(cr *cacheReq) (*fetcher, error) {
	if s.opts.Egress == nil {
		return nil, rerr(http.StatusServiceUnavailable, "UNAVAILABLE", "silo: this silod fetches from no upstream")
	}
	return &fetcher{cr: cr, rt: s.opts.Egress.Transport(cr.caller.Silo), auth: "none", maxHop: 10}, nil
}

func (f *fetcher) record(h CacheHost) {
	for _, o := range f.hosts {
		if o.Host == h.Host && o.Port == h.Port && o.Role == h.Role {
			return
		}
	}
	f.hosts = append(f.hosts, h)
}

// roundTrip makes one request through the egress proxy, recording its
// host, and turns the proxy's refusal into *EgressRefusal.
func (f *fetcher) roundTrip(ctx context.Context, method string, u *url.URL, hdr http.Header, role string) (*http.Response, error) {
	h := hostOf(u, role)
	if d := f.cr.check([]CacheHost{h}); d != nil {
		return nil, &EgressRefusal{Host: d.Host, Reason: "not allowed by the caller's egress policy"}
	}
	f.record(h)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := f.rt.RoundTrip(req)
	if err != nil {
		var r *EgressRefusal
		if errors.As(err, &r) {
			return nil, err
		}
		return nil, &upstreamErr{unreachable: true, msg: fmt.Sprintf("silo: %s: %v", h, err)}
	}
	if why := resp.Header.Get(EgressRefusalHeader); why != "" {
		resp.Body.Close()
		return nil, &EgressRefusal{Host: h.Host, Reason: why}
	}
	return resp, nil
}

// get makes method on path of the upstream's API, with the anonymous
// token exchange on a Bearer challenge, following redirects. A
// redirect's hosts are recorded as role redirect, and carry no token.
func (f *fetcher) get(ctx context.Context, method, path string, accept string) (*http.Response, error) {
	base, err := url.Parse(f.cr.up.Endpoint)
	if err != nil {
		return nil, err
	}
	u := base.JoinPath(path)
	hdr := http.Header{}
	if accept != "" {
		hdr.Set("Accept", accept)
	}
	role := HostAPI
	for hop := 0; ; hop++ {
		h := hdr.Clone()
		if f.token != "" && role == HostAPI {
			h.Set("Authorization", "Bearer "+f.token)
		}
		resp, err := f.roundTrip(ctx, method, u, h, role)
		if err != nil {
			return nil, err
		}
		switch {
		case resp.StatusCode == http.StatusUnauthorized && role == HostAPI && f.token == "":
			ch := resp.Header.Get("Www-Authenticate")
			resp.Body.Close()
			if err := f.anonymousToken(ctx, ch); err != nil {
				return nil, err
			}
			hop--
			continue
		case resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.Header.Get("Location") != "":
			resp.Body.Close()
			if hop >= f.maxHop {
				return nil, &upstreamErr{status: http.StatusBadGateway, msg: "silo: the upstream redirects too often"}
			}
			next, err := u.Parse(resp.Header.Get("Location"))
			if err != nil || (next.Scheme != "https" && next.Scheme != "http") {
				return nil, &upstreamErr{status: http.StatusBadGateway, msg: "silo: the upstream redirects to a bad location"}
			}
			u, role = next, HostRedirect
			if method != http.MethodHead {
				method = http.MethodGet
			}
			continue
		}
		return resp, nil
	}
}

// anonymousToken does the distribution token exchange for an
// anonymous pull, as Docker does for a public image: a GET of the
// Bearer challenge's realm, with its service and scope.
func (f *fetcher) anonymousToken(ctx context.Context, challenge string) error {
	scheme, params := parseChallenge(challenge)
	if !strings.EqualFold(scheme, "bearer") || params["realm"] == "" {
		return &upstreamErr{status: http.StatusUnauthorized, msg: "silo: the upstream wants credentials; private upstream images are not served yet"}
	}
	realm, err := url.Parse(params["realm"])
	if err != nil || (realm.Scheme != "https" && realm.Scheme != "http") {
		return &upstreamErr{status: http.StatusBadGateway, msg: "silo: the upstream's token realm is not a URL"}
	}
	q := realm.Query()
	for _, k := range []string{"service", "scope"} {
		if v := params[k]; v != "" {
			q.Set(k, v)
		}
	}
	realm.RawQuery = q.Encode()
	resp, err := f.roundTrip(ctx, http.MethodGet, realm, nil, HostToken)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &upstreamErr{status: http.StatusUnauthorized, unreachable: resp.StatusCode >= 500,
			msg: fmt.Sprintf("silo: the upstream's token service answered %d", resp.StatusCode)}
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return &upstreamErr{status: http.StatusBadGateway, msg: "silo: the upstream's token service: " + err.Error()}
	}
	f.token = tok.Token
	if f.token == "" {
		f.token = tok.AccessToken
	}
	if f.token == "" {
		return &upstreamErr{status: http.StatusBadGateway, msg: "silo: the upstream's token service gave no token"}
	}
	f.auth = "anonymous"
	return nil
}

// parseChallenge reads a WWW-Authenticate challenge: its scheme and
// its quoted parameters.
func parseChallenge(s string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(s), " ")
	params := map[string]string{}
	for rest != "" {
		rest = strings.TrimLeft(rest, ", ")
		k, v, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		k = strings.ToLower(strings.TrimSpace(k))
		if strings.HasPrefix(v, `"`) {
			end := strings.Index(v[1:], `"`)
			if end < 0 {
				params[k] = v[1:]
				break
			}
			params[k], rest = v[1:1+end], v[2+end:]
		} else {
			val, r, _ := strings.Cut(v, ",")
			params[k], rest = strings.TrimSpace(val), r
		}
	}
	return scheme, params
}

// statusErr turns an upstream's unwanted answer into an upstreamErr.
func statusErr(resp *http.Response, what string) error {
	resp.Body.Close()
	return &upstreamErr{status: resp.StatusCode, unreachable: resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests,
		msg: fmt.Sprintf("silo: the upstream answered %d for %s", resp.StatusCode, what)}
}

// flight is one upstream fetch other requests for the same object wait
// on.
type flight struct {
	done chan struct{}
	obj  *cacheObject
	err  error
}

// once runs fetch for key unless another request is already fetching
// it, in which case it waits for that fetch. shared is whether the
// result is another request's. Public content only: private fetches
// are never shared (call L72, step 32). key names the caller's sharing
// domain, so a request never waits on another domain's fetch (L73).
func (s *Service) once(key string, fetch func() (*cacheObject, error)) (o *cacheObject, shared bool, err error) {
	s.mu.Lock()
	if f, ok := s.flights[key]; ok {
		s.mu.Unlock()
		<-f.done
		return f.obj, true, f.err
	}
	f := &flight{done: make(chan struct{})}
	s.flights[key] = f
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.flights, key)
		s.mu.Unlock()
		close(f.done)
	}()
	f.obj, f.err = fetch()
	return f.obj, false, f.err
}

// fetchShared is once, falling back to the caller's own fetch when the
// shared one failed: another silo's fetch can fail for reasons that
// are that silo's, such as its egress.
func (s *Service) fetchShared(key string, fetch func() (*cacheObject, error)) (*cacheObject, error) {
	o, shared, err := s.once(key, fetch)
	if err != nil && shared {
		return fetch()
	}
	return o, err
}

// fetchManifest fetches ref's manifest from the upstream as the
// caller, stores it and records it under its digest, which is checked
// against the bytes.
func (s *Service) fetchManifest(ctx context.Context, cr *cacheReq, f *fetcher, ref string) (*cacheObject, error) {
	resp, err := f.get(ctx, http.MethodGet, "/v2/"+cr.name.UpstreamRepo+"/manifests/"+ref, manifestAccept)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr(resp, "manifest "+ref)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, s.opts.MaxManifest+1))
	if err != nil {
		return nil, &upstreamErr{unreachable: true, msg: "silo: reading the upstream's manifest: " + err.Error()}
	}
	if int64(len(b)) > s.opts.MaxManifest {
		return nil, &upstreamErr{status: http.StatusBadGateway, msg: "silo: the upstream's manifest is too large"}
	}
	d := Digest("sha256", b)
	if imagerepos.ValidDigest(ref) {
		alg, _, _ := strings.Cut(ref, ":")
		if d = Digest(alg, b); d != ref {
			return nil, &upstreamErr{status: http.StatusBadGateway, msg: "silo: the upstream's manifest doesn't match its digest " + ref}
		}
	}
	mt := resp.Header.Get("Content-Type")
	if mt == "" || mt == "application/json" {
		var probe struct {
			MediaType string `json:"mediaType"`
		}
		json.Unmarshal(b, &probe)
		mt = probe.MediaType
	}
	if err := s.store(ctx, cacheEntry(cr.domain, cr.name.Upstream, cr.name.UpstreamRepo, d), mt, d, int64(len(b)), strings.NewReader(string(b))); err != nil {
		return nil, err
	}
	o := &cacheObject{digest: d, mediaType: mt, size: int64(len(b)), hosts: f.hosts, fetchedAt: s.opts.Now()}
	return o, s.putCacheObject(ctx, cr.domain, cr.name.Upstream, cr.name.UpstreamRepo, kindManifest, o)
}

// fetchBlob fetches digest from the upstream as the caller, following
// its redirects, and stores it, checked against the digest.
func (s *Service) fetchBlob(ctx context.Context, cr *cacheReq, f *fetcher, digest string) (*cacheObject, error) {
	resp, err := f.get(ctx, http.MethodGet, "/v2/"+cr.name.UpstreamRepo+"/blobs/"+digest, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr(resp, "blob "+digest)
	}
	n := resp.ContentLength
	if err := s.store(ctx, cacheEntry(cr.domain, cr.name.Upstream, cr.name.UpstreamRepo, digest), "", digest, n, resp.Body); err != nil {
		return nil, err
	}
	if n < 0 {
		if n, err = s.opts.Content.Size(ctx, digest); err != nil {
			return nil, err
		}
	}
	o := &cacheObject{digest: digest, size: n, hosts: f.hosts, fetchedAt: s.opts.Now()}
	return o, s.putCacheObject(ctx, cr.domain, cr.name.Upstream, cr.name.UpstreamRepo, kindBlob, o)
}

// store writes r's bytes into the content store as digest, under a
// lease for the fetch, and records them as the cache entry named entry
// in silo-cache, which keeps them from GC once the lease ends (§8,
// "Eviction and the cache budget"). size -1 is unknown.
func (s *Service) store(ctx context.Context, entry, mediaType, digest string, size int64, r io.Reader) error {
	ref := "silo-cache-" + randomID()
	lease := "silo-cache/" + ref
	if err := s.opts.Content.Lease(ctx, lease, s.opts.Now().Add(s.opts.Grace)); err != nil {
		return err
	}
	defer s.opts.Content.DeleteLease(context.WithoutCancel(ctx), lease)
	in, err := s.opts.Content.Ingest(ctx, ref, lease)
	if err != nil {
		return err
	}
	defer in.Close()
	if _, err := io.Copy(in, r); err != nil {
		s.opts.Content.Abort(context.WithoutCancel(ctx), ref)
		return &upstreamErr{unreachable: true, msg: "silo: reading from the upstream: " + err.Error()}
	}
	if size < 0 {
		size = in.Offset()
	}
	if err := in.Commit(ctx, size, digest); err != nil {
		s.opts.Content.Abort(context.WithoutCancel(ctx), ref)
		if errors.Is(err, ErrDigest) {
			return &upstreamErr{status: http.StatusBadGateway, msg: "silo: the upstream's bytes don't match " + digest}
		}
		return err
	}
	return s.opts.Content.PutImage(context.WithoutCancel(ctx), CacheNamespace,
		Image{Name: entry, Digest: digest, MediaType: mediaType, Size: size})
}

// probeBlob asks the upstream for digest as the caller, follows its
// redirects without reading the body, and returns the hosts it used.
func (s *Service) probeBlob(ctx context.Context, cr *cacheReq, digest string) ([]CacheHost, error) {
	f, err := s.fetcherFor(cr)
	if err != nil {
		return nil, err
	}
	resp, err := f.get(ctx, http.MethodGet, "/v2/"+cr.name.UpstreamRepo+"/blobs/"+digest, "")
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("the upstream answered %d", resp.StatusCode)
	}
	return f.hosts, nil
}

func randomID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// cacheManifest resolves cr.ref to a cached manifest the caller may
// have, fetching it on a miss.
func (s *Service) cacheManifest(ctx context.Context, cr *cacheReq) (*cacheObject, error) {
	up, repo := cr.name.Upstream, cr.name.UpstreamRepo
	if d := cr.check([]CacheHost{cr.apiHost()}); d != nil {
		return nil, cr.refuse(d.Host)
	}
	lookup := func(digest string) (*cacheObject, error) {
		var o *cacheObject
		err := s.opts.Store.Tx(ctx, func(tx store.Tx) error {
			var err error
			o, err = s.cacheObject(ctx, tx, cr.domain, up, repo, digest, kindManifest)
			return err
		})
		return o, err
	}
	digest := cr.ref
	if !imagerepos.ValidDigest(cr.ref) {
		if !imagerepos.ValidTag(cr.ref) {
			return nil, rerr(http.StatusNotFound, codeManifestUnknown, "silo: %q is neither a tag nor a digest", cr.ref)
		}
		d, err := s.revalidate(ctx, cr)
		if err != nil {
			return nil, err
		}
		digest = d
	}
	o, err := lookup(digest)
	if err != nil {
		return nil, err
	}
	if o != nil {
		if cr.result == "" {
			cr.result = CacheHit
		}
		return o, s.hit(ctx, cr, o, kindManifest)
	}
	cr.result = CacheMiss
	f, err := s.fetcherFor(cr)
	if err != nil {
		return nil, err
	}
	o, err = s.fetchShared(cr.domain+" "+kindManifest+" "+up+"/"+repo+"@"+digest, func() (*cacheObject, error) {
		return s.fetchManifest(ctx, cr, f, digest)
	})
	cr.auth = f.auth
	if err != nil {
		return nil, cr.upstreamFailed(err)
	}
	return o, s.hit(ctx, cr, o, kindManifest)
}

// revalidate asks the upstream for the tag's current digest, as the
// caller, with a manifest HEAD (falling back to a GET when the HEAD
// carries no digest), and records it. An unreachable upstream leaves
// the digest last recorded, if it was revalidated less than
// stale_tag_max ago.
func (s *Service) revalidate(ctx context.Context, cr *cacheReq) (string, error) {
	up, repo, tag := cr.name.Upstream, cr.name.UpstreamRepo, cr.ref
	f, err := s.fetcherFor(cr)
	if err != nil {
		return "", err
	}
	digest, err := func() (string, error) {
		resp, err := f.get(ctx, http.MethodHead, "/v2/"+repo+"/manifests/"+tag, manifestAccept)
		if err != nil {
			return "", err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", statusErr(resp, "manifest "+tag)
		}
		if d := resp.Header.Get("Docker-Content-Digest"); imagerepos.ValidDigest(d) {
			return d, nil
		}
		o, err := s.fetchManifest(ctx, cr, f, tag)
		if err != nil {
			return "", err
		}
		cr.result = CacheMiss
		return o.digest, nil
	}()
	cr.auth, cr.hosts = f.auth, f.hosts
	if err == nil {
		return digest, s.putCacheTag(ctx, cr.domain, up, repo, tag, digest)
	}
	var ue *upstreamErr
	if !errors.As(err, &ue) || !ue.unreachable {
		return "", cr.upstreamFailed(err)
	}
	var at time.Time
	var ok bool
	if serr := s.opts.Store.Tx(ctx, func(tx store.Tx) error {
		var err error
		digest, at, ok, err = s.cacheTag(ctx, tx, cr.domain, up, repo, tag)
		return err
	}); serr != nil {
		return "", serr
	}
	if !ok || s.opts.Now().Sub(at) >= cr.cfg.StaleTagMaxDuration() {
		return "", cr.upstreamFailed(err)
	}
	cr.result = CacheStale
	return digest, nil
}

// upstreamFailed is the registry's answer for a failed upstream fetch.
func (cr *cacheReq) upstreamFailed(err error) error {
	var r *EgressRefusal
	if errors.As(err, &r) {
		return cr.refuse(r.Host)
	}
	var ue *upstreamErr
	if !errors.As(err, &ue) {
		return err
	}
	switch {
	case ue.unreachable:
		return rerr(http.StatusServiceUnavailable, "UNAVAILABLE", "%s", ue.msg)
	case ue.status == http.StatusNotFound:
		if strings.Contains(ue.msg, "blob") {
			return rerr(http.StatusNotFound, codeBlobUnknown, "%s", ue.msg)
		}
		return rerr(http.StatusNotFound, codeManifestUnknown, "%s", ue.msg)
	case ue.status == http.StatusUnauthorized || ue.status == http.StatusForbidden:
		return rerr(http.StatusNotFound, codeNameUnknown, "silo: %s needs credentials; the cache serves public upstream images only", cr.name)
	}
	return rerr(http.StatusBadGateway, "UNKNOWN", "%s", ue.msg)
}

// cacheBlob resolves digest to a cached blob of the repo the caller
// may have, fetching it on a miss.
func (s *Service) cacheBlob(ctx context.Context, cr *cacheReq, digest string) (*cacheObject, error) {
	up, repo := cr.name.Upstream, cr.name.UpstreamRepo
	if !imagerepos.ValidDigest(digest) {
		return nil, rerr(http.StatusNotFound, codeBlobUnknown, "blob unknown to registry")
	}
	if d := cr.check([]CacheHost{cr.apiHost()}); d != nil {
		return nil, cr.refuse(d.Host)
	}
	var o *cacheObject
	if err := s.opts.Store.Tx(ctx, func(tx store.Tx) error {
		var err error
		o, err = s.cacheObject(ctx, tx, cr.domain, up, repo, digest, kindBlob)
		return err
	}); err != nil {
		return nil, err
	}
	if o != nil {
		cr.result = CacheHit
		return o, s.hit(ctx, cr, o, kindBlob)
	}
	cr.result = CacheMiss
	f, err := s.fetcherFor(cr)
	if err != nil {
		return nil, err
	}
	o, err = s.fetchShared(cr.domain+" "+kindBlob+" "+up+"/"+repo+"@"+digest, func() (*cacheObject, error) {
		return s.fetchBlob(ctx, cr, f, digest)
	})
	cr.auth = f.auth
	if err != nil {
		return nil, cr.upstreamFailed(err)
	}
	return o, s.hit(ctx, cr, o, kindBlob)
}

// auditCache writes cr's registry.cache record, as the calling silo,
// under its own silo, and a refusal's registry.denied.
func (s *Service) auditCache(ctx context.Context, cr *cacheReq, kind string, reqErr error) {
	ctx = context.WithoutCancel(ctx)
	hosts := []string{}
	for _, h := range cr.hosts {
		hosts = append(hosts, h.String())
	}
	decision := "allowed"
	if cr.denied != "" {
		decision = "denied"
	}
	status := http.StatusOK
	var re *regError
	if errors.As(reqErr, &re) {
		status = re.status
	} else if reqErr != nil {
		status = http.StatusInternalServerError
	}
	fields := map[string]any{"upstream": cr.name.Upstream, "repo": cr.name.UpstreamRepo, "reference": cr.ref,
		"object": kind, "method": cr.method, "egress": decision, "hosts": hosts, "auth": cr.auth, "status": status, "domain": cr.domainPath}
	if cr.result != "" {
		fields["result"] = cr.result
	}
	if cr.digest != "" {
		fields["digest"] = cr.digest
		fields["bytes"] = cr.bytes
	}
	if cr.denied != "" {
		fields["denied_host"] = cr.denied
	}
	if cr.pullID != "" {
		fields["pull_id"] = cr.pullID
	}
	if cr.caller.FromToolVM != "" && cr.caller.Agent == "" {
		fields["agent"] = cr.caller.FromToolVM
	}
	result := "OK"
	if re != nil {
		result = re.code
	}
	if aerr := s.opts.Store.Tx(ctx, func(tx store.Tx) error {
		t, err := tree.Get(ctx, tx, cr.caller.Silo)
		if err != nil {
			return err
		}
		if err := fenceapi.Audit(ctx, tx, t, cr.caller, fenceapi.KindRegistryCache, "", result, fields); err != nil {
			return err
		}
		var d *denied
		if errors.As(reqErr, &d) {
			return fenceapi.Audit(ctx, tx, t, cr.caller, fenceapi.KindRegistryDenied, "", d.code,
				map[string]any{"repo": cr.name.String(), "method": cr.method, "reason": d.reason, "message": d.message})
		}
		return nil
	}); aerr != nil && !errors.Is(aerr, tree.ErrNotFound) {
		log.Printf("registry: auditing a cache request for %s: %v", cr.name, aerr)
	}
}

// serveCache serves GET and HEAD of a manifest or a blob on an
// upstream path. Tag lists and referrers of upstream repos aren't
// served.
func (s *Service) serveCache(q *request, rest string) {
	ctx := q.ctx()
	cr := &cacheReq{caller: q.caller, method: q.r.Method, name: q.name}
	var kind string
	switch {
	case strings.HasPrefix(rest, "manifests/"):
		kind, cr.ref = kindManifest, strings.TrimPrefix(rest, "manifests/")
	case strings.HasPrefix(rest, "blobs/") && !strings.HasPrefix(rest, "blobs/uploads"):
		kind, cr.ref = kindBlob, strings.TrimPrefix(rest, "blobs/")
	default:
		writeErr(q.w, q.r, rerr(http.StatusNotFound, codeUnsupported, "silo: the cache serves manifests and blobs of upstream repos only"))
		return
	}
	o, err := s.cacheRequest(ctx, cr, kind)
	if err != nil {
		writeErr(q.w, q.r, err)
		return
	}
	ra, n, closer, err := s.opts.Content.Open(ctx, o.digest)
	if err != nil {
		if errors.Is(err, ErrUnknown) {
			err = rerr(http.StatusNotFound, codeBlobUnknown, "silo: the cached content is missing")
		}
		writeErr(q.w, q.r, err)
		return
	}
	defer closer.Close()
	if kind == kindManifest {
		q.w.Header().Set("Content-Type", o.mediaType)
	} else {
		q.w.Header().Set("Content-Type", "application/octet-stream")
	}
	q.w.Header().Set("Docker-Content-Digest", o.digest)
	q.w.Header().Set("Etag", `"`+o.digest+`"`)
	serveContent(q.w, q.r, ra, n)
}

// cacheRequest resolves cr to a cached object of kind the caller may
// have, and audits it.
func (s *Service) cacheRequest(ctx context.Context, cr *cacheReq, kind string) (o *cacheObject, err error) {
	defer func() { s.auditCache(ctx, cr, kind, err) }()
	if err := s.begin(ctx, cr); err != nil {
		return nil, err
	}
	if kind == kindManifest {
		o, err = s.cacheManifest(ctx, cr)
	} else {
		o, err = s.cacheBlob(ctx, cr, cr.ref)
	}
	if err == nil {
		s.touch(ctx, cr, o.digest)
	}
	return o, err
}

// PullThrough is a facade pull's manifest through the cache (§8, "What
// it is"): name's manifest at ref, a tag or a digest, as c, with the
// same egress rule and the same registry.cache record as a request on
// an upstream path, carrying pullID. name is an upstream name as
// imagerepos.ParseName takes it (docker.io/library/alpine); a registry
// that isn't a configured upstream is fetched from at https://<host>.
func (s *Service) PullThrough(ctx context.Context, c fenceapi.Caller, name, ref, pullID string) (mediaType string, manifest []byte, digest string, err error) {
	n, ok := imagerepos.ParseName(name, "")
	if !ok || n.Local() {
		return "", nil, "", &Error{Status: http.StatusNotFound, Code: codeNameInvalid, Message: fmt.Sprintf("silo: invalid upstream repository name %q", name)}
	}
	c.Manage = ""
	cr := &cacheReq{caller: c, method: http.MethodGet, name: n, ref: ref, facade: true, pullID: pullID}
	o, err := s.cacheRequest(ctx, cr, kindManifest)
	if err != nil {
		return "", nil, "", asError(err)
	}
	b, err := s.readContent(ctx, o.digest, s.opts.MaxManifest)
	if err != nil {
		return "", nil, "", err
	}
	return o.mediaType, b, o.digest, nil
}

// FetchThrough is a facade pull's blob through the cache: digest of
// name, as c, fetched into the content store on a miss, with the same
// egress rule and record as PullThrough.
func (s *Service) FetchThrough(ctx context.Context, c fenceapi.Caller, name, digest, pullID string) error {
	n, ok := imagerepos.ParseName(name, "")
	if !ok || n.Local() {
		return &Error{Status: http.StatusNotFound, Code: codeNameInvalid, Message: fmt.Sprintf("silo: invalid upstream repository name %q", name)}
	}
	c.Manage = ""
	cr := &cacheReq{caller: c, method: http.MethodGet, name: n, ref: digest, facade: true, pullID: pullID}
	_, err := s.cacheRequest(ctx, cr, kindBlob)
	return asError(err)
}

// asError is a refusal as an *Error, as PushHeld and Pull return them.
func asError(err error) error {
	var re *regError
	if errors.As(err, &re) {
		return &Error{Status: re.status, Code: re.code, Message: re.message}
	}
	return err
}

// readContent reads a stored blob of at most max bytes.
func (s *Service) readContent(ctx context.Context, digest string, max int64) ([]byte, error) {
	ra, n, closer, err := s.opts.Content.Open(ctx, digest)
	if err != nil {
		return nil, err
	}
	defer closer.Close()
	if n > max {
		return nil, fmt.Errorf("%s is %d bytes, more than %d", digest, n, max)
	}
	return io.ReadAll(io.NewSectionReader(ra, 0, n))
}
