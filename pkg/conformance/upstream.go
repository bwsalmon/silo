package conformance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// MediaOCIManifest is an OCI image manifest's media type.
const MediaOCIManifest = "application/vnd.oci.image.manifest.v1+json"

// Digest is b's sha256 digest, as the registry API writes it.
func Digest(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// Image is an image as the registry API carries it: its config, one
// layer, and the manifest naming them, with their digests.
type Image struct {
	Config, Layer, Manifest    []byte
	ConfigD, LayerD, ManifestD string
}

// NewImage is a one-layer image whose layer is layer.
func NewImage(config, layer string) Image {
	im := Image{Config: []byte(config), Layer: []byte(layer)}
	im.ConfigD, im.LayerD = Digest(im.Config), Digest(im.Layer)
	im.Manifest, _ = json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": MediaOCIManifest,
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": im.ConfigD, "size": len(im.Config)},
		"layers": []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": im.LayerD, "size": len(im.Layer)}}})
	im.ManifestD = Digest(im.Manifest)
	return im
}

// Upstream is the test upstream registry the targets' pull-through
// cache has as docker.io, served by host the way Docker Hub is: the
// registry at registry.test wants a token from auth.test, and
// redirects public blob GETs to a CDN at cdn.test, which serves them
// without the token.
//
// It has a public image, library/alpine:3 (Public), with an anonymous
// token, and a private one, private/app:1 (Private), whose token
// auth.test gives only for User and Pass; an anonymous token for it is
// given and refused, as Docker Hub does. The password and the private
// token are canaries: fresh per Upstream, so a case can look for them
// in everything the target wrote.
type Upstream struct {
	Public, Private Image
	User, Pass      string
	Token           string

	mu sync.Mutex
	// extra are public blobs beyond Public's, by digest (AddBlob).
	extra map[string][]byte
	// requests counts requests by "host method path".
	requests map[string]int
}

// NewUpstream is an Upstream with fresh canaries.
func NewUpstream() *Upstream {
	canary := fmt.Sprintf("%x", time.Now().UnixNano())
	return &Upstream{
		Public:   NewImage(`{"architecture":"amd64","os":"linux"}`, "the alpine layer"),
		Private:  NewImage(`{"architecture":"amd64","os":"linux","private":true}`, "the private layer"),
		User:     "robot",
		Pass:     "canary-pass-" + canary,
		Token:    "canary-token-" + canary,
		requests: map[string]int{},
		extra:    map[string][]byte{},
	}
}

// AddBlob makes b a public blob of library/alpine's, and returns its
// digest: a pull loop's material.
func (u *Upstream) AddBlob(b []byte) string {
	d := Digest(b)
	u.mu.Lock()
	u.extra[d] = b
	u.mu.Unlock()
	return d
}

// Hosts are the upstream's hosts: the registry, its token service and
// its CDN.
var Hosts = []string{"registry.test", "auth.test", "cdn.test"}

// Count is how many requests "host method path" the upstream has had.
func (u *Upstream) Count(key string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.requests[key]
}

func (u *Upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.requests[r.Host+" "+r.Method+" "+r.URL.Path]++
	u.mu.Unlock()
	switch {
	case r.Host == "auth.test" && r.URL.Path == "/token":
		tok := "anon-token"
		if user, pass, ok := r.BasicAuth(); ok {
			if user != u.User || pass != u.Pass {
				http.Error(w, "bad credentials", http.StatusUnauthorized)
				return
			}
			tok = u.Token
		}
		json.NewEncoder(w).Encode(map[string]string{"token": tok})
	case r.Host == "registry.test":
		repo, im, want := "library/alpine", u.Public, "Bearer anon-token"
		if strings.HasPrefix(r.URL.Path, "/v2/private/app/") {
			repo, im, want = "private/app", u.Private, "Bearer "+u.Token
		}
		if r.Header.Get("Authorization") != want {
			w.Header().Set("Www-Authenticate", fmt.Sprintf(`Bearer realm="http://auth.test/token",service="registry.test",scope="repository:%s:pull"`, repo))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		rest, ok := strings.CutPrefix(r.URL.Path, "/v2/"+repo+"/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		if ref, ok := strings.CutPrefix(rest, "manifests/"); ok {
			tag := "3"
			if repo == "private/app" {
				tag = "1"
			}
			if ref != tag && ref != im.ManifestD {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", MediaOCIManifest)
			w.Header().Set("Docker-Content-Digest", im.ManifestD)
			if r.Method != http.MethodHead {
				w.Write(im.Manifest)
			}
			return
		}
		d := strings.TrimPrefix(rest, "blobs/")
		if repo == "library/alpine" {
			http.Redirect(w, r, "http://cdn.test/b/"+d+"?sig=x", http.StatusTemporaryRedirect)
			return
		}
		u.blob(w, r, im, d)
	case r.Host == "cdn.test" && strings.HasPrefix(r.URL.Path, "/b/"):
		if r.Header.Get("Authorization") != "" {
			http.Error(w, "the token went to the CDN", http.StatusBadRequest)
			return
		}
		u.blob(w, r, u.Public, strings.TrimPrefix(r.URL.Path, "/b/"))
	default:
		http.Error(w, "no such host "+r.Host, http.StatusBadGateway)
	}
}

func (u *Upstream) blob(w http.ResponseWriter, r *http.Request, im Image, d string) {
	switch d {
	case im.ConfigD:
		w.Write(im.Config)
	case im.LayerD:
		w.Write(im.Layer)
	default:
		u.mu.Lock()
		b, ok := u.extra[d]
		u.mu.Unlock()
		if ok && im.LayerD == u.Public.LayerD {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}
}
