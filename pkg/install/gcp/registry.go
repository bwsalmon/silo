package gcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"golang.org/x/oauth2"
)

// manifestTypes are the manifest media types the registry is asked for:
// an image index (multi-arch) or one image's manifest, OCI or Docker.
var manifestTypes = []string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}

// Registry is install.Registry over the OCI distribution API. Google's
// registries (*.pkg.dev, gcr.io) are called with the installer's own
// OAuth token; any other answers its own bearer challenge, anonymously,
// as a public release registry does.
type Registry struct {
	TokenSource oauth2.TokenSource
	HTTP        *http.Client

	mu     sync.Mutex
	tokens map[string]string // scope -> bearer token from a challenge
}

func (r *Registry) client() *http.Client {
	if r.HTTP != nil {
		return r.HTTP
	}
	return http.DefaultClient
}

func googleHost(host string) bool {
	return strings.HasSuffix(host, ".pkg.dev") || host == "gcr.io" || strings.HasSuffix(host, ".gcr.io")
}

// split is a repository's host and path.
func split(repo string) (host, path string, err error) {
	host, path, ok := strings.Cut(repo, "/")
	if !ok || path == "" {
		return "", "", fmt.Errorf("%q is not <host>/<repository>", repo)
	}
	return host, path, nil
}

// do sends req to the repository's registry, answering an auth
// challenge once.
func (r *Registry) do(ctx context.Context, host, path string, push bool, build func() (*http.Request, error)) (*http.Response, error) {
	scope := "repository:" + path + ":pull"
	if push {
		scope += ",push"
	}
	for attempt := 0; ; attempt++ {
		req, err := build()
		if err != nil {
			return nil, err
		}
		req = req.WithContext(ctx)
		if googleHost(host) && r.TokenSource != nil {
			t, err := r.TokenSource.Token()
			if err != nil {
				return nil, err
			}
			req.SetBasicAuth("oauth2accesstoken", t.AccessToken)
		} else {
			r.mu.Lock()
			tok := r.tokens[host+" "+scope]
			r.mu.Unlock()
			if tok != "" {
				req.Header.Set("Authorization", "Bearer "+tok)
			}
		}
		resp, err := r.client().Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized || attempt > 0 || googleHost(host) {
			return resp, nil
		}
		challenge := resp.Header.Get("Www-Authenticate")
		resp.Body.Close()
		if err := r.answer(ctx, host, scope, challenge); err != nil {
			return nil, err
		}
	}
}

// answer fetches an anonymous bearer token for a Bearer challenge.
func (r *Registry) answer(ctx context.Context, host, scope, challenge string) error {
	rest, ok := strings.CutPrefix(challenge, "Bearer ")
	if !ok {
		return fmt.Errorf("%s: unsupported auth challenge %q", host, challenge)
	}
	params := map[string]string{}
	for _, kv := range strings.Split(rest, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(kv), "=")
		params[k] = strings.Trim(v, `"`)
	}
	u, err := url.Parse(params["realm"])
	if err != nil || params["realm"] == "" {
		return fmt.Errorf("%s: bad auth challenge %q", host, challenge)
	}
	q := u.Query()
	if params["service"] != "" {
		q.Set("service", params["service"])
	}
	q.Set("scope", scope)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := r.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&t) != nil {
		return fmt.Errorf("%s: getting a token: %s", host, resp.Status)
	}
	if t.Token == "" {
		t.Token = t.AccessToken
	}
	r.mu.Lock()
	if r.tokens == nil {
		r.tokens = map[string]string{}
	}
	r.tokens[host+" "+scope] = t.Token
	r.mu.Unlock()
	return nil
}

func statusErr(what string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("%s: %s: %s", what, resp.Status, bytes.TrimSpace(b))
}

// manifest reads repo's manifest ref (a tag or digest).
func (r *Registry) manifest(ctx context.Context, repo, ref string) (body []byte, mediaType, digest string, err error) {
	host, path, err := split(repo)
	if err != nil {
		return nil, "", "", err
	}
	resp, err := r.do(ctx, host, path, false, func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, "https://"+host+"/v2/"+path+"/manifests/"+ref, nil)
		if err == nil {
			req.Header.Set("Accept", strings.Join(manifestTypes, ", "))
		}
		return req, err
	})
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", "", statusErr("reading "+repo+":"+ref, resp)
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return body, resp.Header.Get("Content-Type"), resp.Header.Get("Docker-Content-Digest"), err
}

// Resolve is ref's manifest digest.
func (r *Registry) Resolve(ctx context.Context, ref string) (string, error) {
	repo, tag := ref, "latest"
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		repo, tag = ref[:i], ref[i+1:]
	}
	_, _, d, err := r.manifest(ctx, repo, tag)
	if err != nil {
		return "", err
	}
	if d == "" {
		return "", fmt.Errorf("%s: the registry gave no digest", ref)
	}
	return d, nil
}

// Has reports whether repo holds the manifest digest.
func (r *Registry) Has(ctx context.Context, repo, digest string) (bool, error) {
	host, path, err := split(repo)
	if err != nil {
		return false, err
	}
	resp, err := r.do(ctx, host, path, false, func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodHead, "https://"+host+"/v2/"+path+"/manifests/"+digest, nil)
		if err == nil {
			req.Header.Set("Accept", strings.Join(manifestTypes, ", "))
		}
		return req, err
	})
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, fmt.Errorf("reading %s@%s: %s", repo, digest, resp.Status)
}

// Copy copies src@digest into dst: an index's manifests first, each
// manifest's config and layers before it, so that every manifest put
// refers only to what dst already holds.
func (r *Registry) Copy(ctx context.Context, src, digest, dst string) error {
	if ok, err := r.Has(ctx, dst, digest); err != nil || ok {
		return err
	}
	body, mediaType, _, err := r.manifest(ctx, src, digest)
	if err != nil {
		return err
	}
	var m struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return fmt.Errorf("reading %s@%s: %v", src, digest, err)
	}
	if mediaType == "" {
		mediaType = m.MediaType
	}
	for _, child := range m.Manifests {
		if err := r.Copy(ctx, src, child.Digest, dst); err != nil {
			return err
		}
	}
	var blobs []string
	if m.Config.Digest != "" {
		blobs = append(blobs, m.Config.Digest)
	}
	for _, l := range m.Layers {
		blobs = append(blobs, l.Digest)
	}
	for _, b := range blobs {
		if err := r.copyBlob(ctx, src, dst, b); err != nil {
			return err
		}
	}
	host, path, err := split(dst)
	if err != nil {
		return err
	}
	resp, err := r.do(ctx, host, path, true, func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPut, "https://"+host+"/v2/"+path+"/manifests/"+digest, bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", mediaType)
		}
		return req, err
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return statusErr("putting "+dst+"@"+digest, resp)
	}
	return nil
}

// copyBlob copies one blob, unless dst has it.
func (r *Registry) copyBlob(ctx context.Context, src, dst, digest string) error {
	dhost, dpath, err := split(dst)
	if err != nil {
		return err
	}
	head, err := r.do(ctx, dhost, dpath, false, func() (*http.Request, error) {
		return http.NewRequest(http.MethodHead, "https://"+dhost+"/v2/"+dpath+"/blobs/"+digest, nil)
	})
	if err != nil {
		return err
	}
	head.Body.Close()
	if head.StatusCode == http.StatusOK {
		return nil
	}
	shost, spath, err := split(src)
	if err != nil {
		return err
	}
	get, err := r.do(ctx, shost, spath, false, func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, "https://"+shost+"/v2/"+spath+"/blobs/"+digest, nil)
	})
	if err != nil {
		return err
	}
	defer get.Body.Close()
	if get.StatusCode != http.StatusOK {
		return statusErr("reading blob "+digest+" of "+src, get)
	}
	data, err := io.ReadAll(get.Body)
	if err != nil {
		return err
	}
	start, err := r.do(ctx, dhost, dpath, true, func() (*http.Request, error) {
		return http.NewRequest(http.MethodPost, "https://"+dhost+"/v2/"+dpath+"/blobs/uploads/", nil)
	})
	if err != nil {
		return err
	}
	start.Body.Close()
	if start.StatusCode != http.StatusAccepted {
		return fmt.Errorf("starting the upload of %s to %s: %s", digest, dst, start.Status)
	}
	loc, err := start.Request.URL.Parse(start.Header.Get("Location"))
	if err != nil {
		return err
	}
	q := loc.Query()
	q.Set("digest", digest)
	loc.RawQuery = q.Encode()
	put, err := r.do(ctx, dhost, dpath, true, func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPut, loc.String(), bytes.NewReader(data))
		if err == nil {
			req.Header.Set("Content-Type", "application/octet-stream")
		}
		return req, err
	})
	if err != nil {
		return err
	}
	defer put.Body.Close()
	if put.StatusCode != http.StatusCreated {
		return statusErr("uploading "+digest+" to "+dst, put)
	}
	return nil
}
