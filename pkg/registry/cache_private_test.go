package registry_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/registry"
	"github.com/bwsalmon/silo/pkg/store"
)

// privateEnv is cacheEnv with an image at the upstream's private repo,
// private/app:1, readable with user and a canary password, for which
// the token service gives a canary token.
type privateEnv struct {
	*cacheEnv
	user, pass, token        string
	pManifest, pLayer, pConf string
}

func newPrivateEnv(t *testing.T) *privateEnv {
	t.Helper()
	ce := newCacheEnv(t)
	canary := fmt.Sprintf("%x", time.Now().UnixNano())
	pe := &privateEnv{cacheEnv: ce, user: "robot", pass: "canary-pass-" + canary, token: "canary-token-" + canary}
	layer, conf := []byte("the private layer"), []byte(`{"architecture":"amd64","os":"linux","private":true}`)
	pe.pLayer, pe.pConf = registry.Digest("sha256", layer), registry.Digest("sha256", conf)
	m := image(pe.pConf, len(conf), map[string]int{pe.pLayer: len(layer)})
	pe.pManifest = registry.Digest("sha256", m)
	ce.up.mu.Lock()
	ce.up.private = map[string][]byte{"1": m, pe.pManifest: m, pe.pLayer: layer, pe.pConf: conf}
	ce.up.user, ce.up.pass, ce.up.privToken = pe.user, pe.pass, pe.token
	ce.up.mu.Unlock()
	return pe
}

// reqAs is a registry GET as caller, with Basic credentials if user
// isn't "".
func (pe *privateEnv) reqAs(caller, path, user, pass string) resp {
	pe.t.Helper()
	req, _ := http.NewRequest("GET", pe.srv.URL+path, nil)
	req.Header.Set("Silo-Caller", caller)
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		pe.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{code: res.StatusCode, header: res.Header, body: b}
}

func (pe *privateEnv) withCreds(caller, path string) resp {
	pe.t.Helper()
	return pe.reqAs(caller, path, pe.user, pe.pass)
}

// A silo with credentials pulls a private image through the cache; a
// silo without them gets a Basic challenge for the manifest and 404 for
// each blob, under the private repo's path and under a public repo's.
// Every request for the private manifest revalidates it by digest with
// the caller's credentials; its blobs are served only through an
// authorisation, which ends after auth_ttl.
func TestCachePrivatePull(t *testing.T) {
	pe := newPrivateEnv(t)
	const repo = "/v2/docker.io/private/app/"
	r := pe.withCreds(pe.a, repo+"manifests/1")
	if r.code != 200 || r.header.Get("Docker-Content-Digest") != pe.pManifest {
		t.Fatalf("a, with credentials: %d %s", r.code, r.body)
	}
	if rec := pe.last(fenceapi.KindRegistryCache); rec["result"] != "miss" || rec["auth"] != "credentials" || rec["private"] != true {
		t.Fatalf("the private miss's record: %v", rec)
	}
	for _, d := range []string{pe.pConf, pe.pLayer} {
		if r := pe.withCreds(pe.a, repo+"blobs/"+d); r.code != 200 || registry.Digest("sha256", r.body) != d {
			t.Fatalf("a: blob %s: %d %s", d, r.code, r.body)
		}
	}

	// d, without credentials: a challenge for the manifest, by tag and
	// by digest, and 404 for every blob, under either repo's path.
	for _, ref := range []string{"1", pe.pManifest} {
		r := pe.reqAs(pe.d, repo+"manifests/"+ref, "", "")
		if r.code != 401 || r.errCode() != "UNAUTHORIZED" || r.header.Get("Www-Authenticate") != `Basic realm="registry.silo.internal"` {
			t.Fatalf("d, no credentials, manifest %s: %d %v %s", ref, r.code, r.header, r.body)
		}
	}
	for _, p := range []string{repo, "/v2/docker.io/library/alpine/"} {
		for _, d := range []string{pe.pConf, pe.pLayer} {
			if r := pe.reqAs(pe.d, p+"blobs/"+d, "", ""); r.code != 404 || r.errCode() != "BLOB_UNKNOWN" {
				t.Fatalf("d, no credentials, %sblobs/%s: %d %s", p, d, r.code, r.body)
			}
		}
	}
	// Nor with wrong ones: the upstream's refusal is passed back.
	if r := pe.reqAs(pe.d, repo+"manifests/"+pe.pManifest, pe.user, "wrong"); r.code != 401 {
		t.Fatalf("d, wrong credentials: %d %s", r.code, r.body)
	}
	if r := pe.reqAs(pe.d, repo+"blobs/"+pe.pLayer, pe.user, "wrong"); r.code != 404 {
		t.Fatalf("d, wrong credentials, a blob: %d %s", r.code, r.body)
	}
	// a's own credential-less request for the manifest is challenged
	// too: every request revalidates.
	if r := pe.reqAs(pe.a, repo+"manifests/"+pe.pManifest, "", ""); r.code != 401 {
		t.Fatalf("a, no credentials, the manifest: %d %s", r.code, r.body)
	}

	// A hit by digest is one HEAD by digest with the credentials, and no
	// GET; d's credentials let it have what a's fetch cached.
	heads := pe.up.count("registry.test HEAD /v2/private/app/manifests/" + pe.pManifest)
	if r := pe.withCreds(pe.d, repo+"manifests/"+pe.pManifest); r.code != 200 {
		t.Fatalf("d, with credentials: %d %s", r.code, r.body)
	}
	if n := pe.up.count("registry.test HEAD /v2/private/app/manifests/" + pe.pManifest); n != heads+1 {
		t.Fatalf("revalidating HEADs: %d, want %d", n, heads+1)
	}
	if n := pe.up.count("registry.test GET /v2/private/app/manifests/" + pe.pManifest); n != 1 {
		t.Fatalf("manifest GETs by digest: %d, want a's one", n)
	}
	if rec := pe.last(fenceapi.KindRegistryCache); rec["result"] != "hit" || rec["auth"] != "credentials" {
		t.Fatalf("the revalidated hit's record: %v", rec)
	}
	// d's authorisation serves the blobs from the cache, with or
	// without credentials on the blob request.
	if r := pe.reqAs(pe.d, repo+"blobs/"+pe.pLayer, "", ""); r.code != 200 {
		t.Fatalf("d, authorised, no credentials: %d %s", r.code, r.body)
	}
	if n := pe.up.count("registry.test GET /v2/private/app/blobs/" + pe.pLayer); n != 1 {
		t.Fatalf("private blob GETs: %d, want 1", n)
	}
	// Not under another repo's path.
	if r := pe.reqAs(pe.d, "/v2/docker.io/library/alpine/blobs/"+pe.pLayer, "", ""); r.code != 404 {
		t.Fatalf("d, authorised, under alpine's path: %d %s", r.code, r.body)
	}
	// Authorisations last auth_ttl.
	pe.now = pe.now.Add(301 * time.Second)
	if r := pe.reqAs(pe.d, repo+"blobs/"+pe.pLayer, "", ""); r.code != 404 {
		t.Fatalf("d, after auth_ttl: %d %s", r.code, r.body)
	}
	// Egress is still checked first.
	if r := pe.withCreds(pe.b, repo+"manifests/"+pe.pManifest); r.code != 403 {
		t.Fatalf("b, with credentials, no egress: %d %s", r.code, r.body)
	}
}

// Classification is re-checked anonymously after public_ttl: an image
// made private upstream stops being served to callers without
// credentials, and is served after revalidation to those with them.
func TestCachePrivateReclassified(t *testing.T) {
	pe := newPrivateEnv(t)
	for _, p := range []string{"manifests/3", "blobs/" + pe.layer} {
		if r := pe.req(pe.a, "GET", "/v2/docker.io/library/alpine/"+p, "", nil); r.code != 200 {
			t.Fatalf("a: GET %s: %d %s", p, r.code, r.body)
		}
	}
	pe.up.mu.Lock()
	pe.up.alpinePrivate = true
	pe.up.mu.Unlock()
	// Within public_ttl, a digest is served as public, without asking.
	pe.now = pe.now.Add(time.Hour)
	if r := pe.req(pe.d, "GET", "/v2/docker.io/library/alpine/blobs/"+pe.layer, "", nil); r.code != 200 {
		t.Fatalf("d, within public_ttl: %d %s", r.code, r.body)
	}
	pe.now = pe.now.Add(24 * time.Hour)
	for _, p := range []string{"manifests/" + pe.manifest, "manifests/3"} {
		if r := pe.req(pe.d, "GET", "/v2/docker.io/library/alpine/"+p, "", nil); r.code != 401 {
			t.Fatalf("d, after public_ttl, %s: %d %s", p, r.code, r.body)
		}
	}
	if r := pe.req(pe.d, "GET", "/v2/docker.io/library/alpine/blobs/"+pe.layer, "", nil); r.code != 404 {
		t.Fatalf("d, after public_ttl, the layer: %d %s", r.code, r.body)
	}
	if r := pe.withCreds(pe.d, "/v2/docker.io/library/alpine/manifests/3"); r.code != 200 || r.header.Get("Docker-Content-Digest") != pe.manifest {
		t.Fatalf("d, with credentials: %d %s", r.code, r.body)
	}
	if r := pe.req(pe.d, "GET", "/v2/docker.io/library/alpine/blobs/"+pe.layer, "", nil); r.code != 200 {
		t.Fatalf("d, authorised: %d %s", r.code, r.body)
	}
}

// Private fetches are never shared in flight: two silos missing on the
// same private blob each fetch it, with their own credentials.
func TestCachePrivateNotShared(t *testing.T) {
	pe := newPrivateEnv(t)
	const repo = "/v2/docker.io/private/app/"
	for _, s := range []string{pe.a, pe.d} {
		if r := pe.withCreds(s, repo+"manifests/1"); r.code != 200 {
			t.Fatalf("manifest: %d %s", r.code, r.body)
		}
	}
	pe.up.mu.Lock()
	pe.up.hold, pe.up.held, pe.up.privateHolding = make(chan struct{}), make(chan struct{}, 4), true
	pe.up.mu.Unlock()
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, s := range []string{pe.a, pe.d} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = pe.withCreds(s, repo+"blobs/"+pe.pLayer).code
		}()
		<-pe.up.held
	}
	close(pe.up.hold)
	wg.Wait()
	if codes[0] != 200 || codes[1] != 200 {
		t.Fatalf("codes: %v", codes)
	}
	if n := pe.up.count("registry.test GET /v2/private/app/blobs/" + pe.pLayer); n != 2 {
		t.Fatalf("private blob GETs: %d, want one per silo", n)
	}
}

// The facade's pull carries X-Registry-Auth's credentials; without
// them a private image is a 401.
func TestCachePrivatePullThrough(t *testing.T) {
	pe := newPrivateEnv(t)
	auth := base64.URLEncoding.EncodeToString([]byte(`{"username":"` + pe.user + `","password":"` + pe.pass + `","serveraddress":"docker.io"}`))
	creds := registry.RegistryAuth(auth)
	if creds == nil || creds.Username != pe.user || creds.Password != pe.pass {
		t.Fatalf("RegistryAuth: %+v", creds)
	}
	if s := fmt.Sprintf("%v %+v %#v %s", creds, creds, creds, *creds); strings.Contains(s, pe.pass) {
		t.Fatalf("credentials format with their password: %s", s)
	}
	if registry.RegistryAuth("e30=") != nil || registry.RegistryAuth("") != nil {
		t.Fatal("empty X-Registry-Auth gave credentials")
	}
	ctx := context.Background()
	c := fenceapi.Caller{Silo: pe.a, Via: fenceapi.ViaOrigin}
	_, _, d, err := pe.reg.PullThrough(ctx, c, "docker.io/private/app", "1", "pull-1", creds)
	if err != nil || d != pe.pManifest {
		t.Fatalf("PullThrough: %v %s", err, d)
	}
	for _, b := range []string{pe.pConf, pe.pLayer} {
		if err := pe.reg.FetchThrough(ctx, c, "docker.io/private/app", b, "pull-1", creds); err != nil {
			t.Fatalf("FetchThrough %s: %v", b, err)
		}
	}
	cd := fenceapi.Caller{Silo: pe.d, Via: fenceapi.ViaOrigin}
	var re *registry.Error
	if _, _, _, err := pe.reg.PullThrough(ctx, cd, "docker.io/private/app", "1", "pull-2", nil); !errors.As(err, &re) || re.Status != 401 {
		t.Fatalf("d: PullThrough without credentials: %v", err)
	}
	if err := pe.reg.FetchThrough(ctx, cd, "docker.io/private/app", pe.pLayer, "pull-2", nil); !errors.As(err, &re) || re.Status != 404 {
		t.Fatalf("d: FetchThrough without credentials: %v", err)
	}
}

// No log line, audit record, store row, file in the data directory or
// cached object holds the credentials a test presented, in any form,
// or the token the upstream gave for them.
func TestCachePrivateCanary(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	pe := newPrivateEnv(t)
	const repo = "/v2/docker.io/private/app/"
	for _, p := range []string{"manifests/1", "manifests/" + pe.pManifest, "blobs/" + pe.pConf, "blobs/" + pe.pLayer} {
		if r := pe.withCreds(pe.a, repo+p); r.code != 200 {
			t.Fatalf("GET %s: %d %s", p, r.code, r.body)
		}
	}
	// Refusals too: wrong credentials, the canary as a user name, and
	// requests a public repo answers.
	pe.reqAs(pe.d, repo+"manifests/1", pe.pass, "x")
	pe.reqAs(pe.d, repo+"manifests/1", pe.user, pe.pass+"-wrong")
	pe.reqAs(pe.d, repo+"blobs/"+pe.pLayer, pe.user, pe.pass)
	pe.withCreds(pe.b, repo+"manifests/1")
	pe.withCreds(pe.a, "/v2/docker.io/library/alpine/manifests/3")
	c := fenceapi.Caller{Silo: pe.d, Via: fenceapi.ViaOrigin}
	pe.reg.PullThrough(context.Background(), c, "docker.io/private/app", "1", "pull-1", &registry.Credentials{Username: pe.user, Password: pe.pass})
	pe.reg.PullThrough(context.Background(), c, "docker.io/private/app", "1", "pull-2", &registry.Credentials{Username: pe.user, Password: "x" + pe.pass})

	canaries := []string{pe.pass, pe.token,
		base64.StdEncoding.EncodeToString([]byte(pe.user + ":" + pe.pass)),
		base64.StdEncoding.EncodeToString([]byte(pe.pass)),
		strings.TrimPrefix(pe.pass, "canary-pass-")}
	check := func(where string, b []byte) {
		t.Helper()
		for _, c := range canaries {
			if bytes.Contains(b, []byte(c)) {
				t.Errorf("%s holds the canary %q", where, c)
			}
		}
	}
	check("the log", logs.Bytes())

	// Every row of every table, the audit's included.
	var rows int
	err := pe.db.Tx(context.Background(), func(tx store.Tx) error {
		ts, err := tx.Query(context.Background(), `SELECT name FROM sqlite_master WHERE type = 'table'`)
		if err != nil {
			return err
		}
		var tables []string
		for ts.Next() {
			var n string
			ts.Scan(&n)
			tables = append(tables, n)
		}
		ts.Close()
		for _, tb := range tables {
			rs, err := tx.Query(context.Background(), `SELECT * FROM "`+tb+`"`)
			if err != nil {
				return err
			}
			cols, _ := rs.Columns()
			for rs.Next() {
				vals := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := rs.Scan(ptrs...); err != nil {
					rs.Close()
					return err
				}
				rows++
				check("table "+tb, []byte(fmt.Sprint(vals...)))
				for _, v := range vals {
					if b, ok := v.([]byte); ok {
						check("table "+tb, b)
					}
				}
			}
			rs.Close()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if rows == 0 || len(pe.audit(fenceapi.KindRegistryCache)) == 0 {
		t.Fatal("the store holds nothing to check")
	}

	// The data directory, the database's files.
	if err := filepath.WalkDir(pe.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		check(p, b)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The cached objects.
	for _, d := range []string{pe.pManifest, pe.pConf, pe.pLayer} {
		ra, n, closer, err := pe.content.Open(context.Background(), d)
		if err != nil {
			t.Fatalf("%s isn't cached: %v", d, err)
		}
		b := make([]byte, n)
		ra.ReadAt(b, 0)
		closer.Close()
		check("cached "+d, b)
	}
	for _, img := range pe.content.Images() {
		check("image "+img.Name, []byte(fmt.Sprint(img)))
	}
}
