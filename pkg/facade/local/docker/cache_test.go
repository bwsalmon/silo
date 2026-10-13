package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/bwsalmon/silo/pkg/facade"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/registry"
)

// fetcher is the cache's fetcher as the facade sees it: an index of
// one image at docker.io/library/alpine:3, in the content store as it
// is fetched, and a refusal for silo deny.
type fetcher struct {
	content *registry.Memory
	objects map[string][]byte // by digest
	index   string
	deny    string
	// private wants credentials, user and pass, for every call.
	private bool
	creds   []*registry.Credentials
	calls   []string
	pullIDs map[string]bool
}

func newFetcher(content *registry.Memory) *fetcher {
	f := &fetcher{content: content, objects: map[string][]byte{}, pullIDs: map[string]bool{}}
	put := func(b []byte) string {
		d := registry.Digest("sha256", b)
		f.objects[d] = b
		return d
	}
	cfg := put([]byte(`{"architecture":"` + goarch() + `","os":"linux","config":{"Cmd":["/bin/sh"]}}`))
	layer := put([]byte("a layer"))
	m, _ := json.Marshal(obj{"schemaVersion": 2, "mediaType": mediaOCIManifest,
		"config": obj{"mediaType": mediaOCIConfig, "digest": cfg, "size": len(f.objects[cfg])},
		"layers": []obj{{"mediaType": mediaOCILayer, "digest": layer, "size": len(f.objects[layer])}}})
	md := put(m)
	idx, _ := json.Marshal(obj{"schemaVersion": 2, "mediaType": mediaOCIIndex,
		"manifests": []obj{{"mediaType": mediaOCIManifest, "digest": md, "size": len(m), "platform": obj{"os": "linux", "architecture": goarch()}}}})
	f.index = put(idx)
	return f
}

func (f *fetcher) PullThrough(_ context.Context, c fenceapi.Caller, name, ref, pullID string, creds *registry.Credentials) (string, []byte, string, error) {
	f.calls = append(f.calls, "manifest "+name+" "+ref)
	f.pullIDs[pullID] = true
	f.creds = append(f.creds, creds)
	if f.private && (creds == nil || creds.Username != "user" || creds.Password != "pass") {
		return "", nil, "", &registry.Error{Status: 401, Code: "UNAUTHORIZED", Message: "authentication required"}
	}
	if c.Silo == f.deny {
		return "", nil, "", &registry.Error{Status: 403, Code: "DENIED", Message: "silo: egress to registry-1.docker.io is not allowed for /team-b"}
	}
	if name != "docker.io/library/alpine" {
		return "", nil, "", &registry.Error{Status: 404, Code: "MANIFEST_UNKNOWN", Message: "unknown"}
	}
	d := ref
	if ref == "3" {
		d = f.index
	}
	b, ok := f.objects[d]
	if !ok {
		return "", nil, "", &registry.Error{Status: 404, Code: "MANIFEST_UNKNOWN", Message: "unknown"}
	}
	var mt struct {
		MediaType string `json:"mediaType"`
	}
	json.Unmarshal(b, &mt)
	f.content.Put(b)
	return mt.MediaType, b, d, nil
}

func (f *fetcher) FetchThrough(_ context.Context, c fenceapi.Caller, name, digest, pullID string, creds *registry.Credentials) error {
	f.calls = append(f.calls, "blob "+name+" "+digest)
	f.pullIDs[pullID] = true
	f.creds = append(f.creds, creds)
	f.content.Put(f.objects[digest])
	return nil
}

// A facade pull from an external registry goes through the cache's
// fetcher as the caller: the index, its child for this platform, then
// the config and the layers, all under one pull id, which the
// facade.call record carries too. A refusal is Docker's 403.
func TestPullThroughFetcher(t *testing.T) {
	e := newEnv(t)
	f := newFetcher(e.content)
	e.facade.opts.Fetcher = f
	e.pull(e.a, "alpine:3")
	if len(f.calls) != 4 || f.calls[0] != "manifest docker.io/library/alpine 3" || len(f.pullIDs) != 1 {
		t.Fatalf("the fetcher's calls: %v, pull ids %v", f.calls, f.pullIDs)
	}
	if len(e.images.pulls) != 0 {
		t.Fatalf("the old puller pulled: %v", e.images.pulls)
	}
	img := e.ok(e.a, "GET", "/v1.47/images/alpine:3/json", nil)
	if ds, _ := img["RepoDigests"].([]any); len(ds) != 1 || ds[0] != "alpine@"+f.index {
		t.Fatalf("RepoDigests: %v", img["RepoDigests"])
	}
	var id string
	for k := range f.pullIDs {
		id = k
	}
	found := false
	for _, r := range e.auditOf(facade.KindFacadeCall) {
		if r["pull_id"] == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("no facade.call record carries pull id %s", id)
	}
	f.deny = e.b
	e.fail(http.StatusForbidden, "egress to registry-1.docker.io is not allowed", e.b, "POST", "/v1.47/images/create?fromImage=alpine&tag=3", nil)
}

// A pull's X-Registry-Auth reaches the fetcher, on every call; a private
// image without credentials is Docker's 401, which the CLI answers by
// asking for a login.
func TestPullThroughFetcherCredentials(t *testing.T) {
	e := newEnv(t)
	f := newFetcher(e.content)
	f.private = true
	e.facade.opts.Fetcher = f
	path := "/v1.47/images/create?fromImage=alpine&tag=3"
	if code, out := e.do(e.a, "POST", path, nil, nil); code != 401 || out["message"] != "unauthorized: authentication required" {
		t.Fatalf("no credentials: %d %v", code, out)
	}
	auth := base64.URLEncoding.EncodeToString([]byte(`{"username":"user","password":"pass","serveraddress":"https://index.docker.io/v1/"}`))
	if code, b, _ := e.raw(e.a, "POST", path, nil, map[string]string{"X-Registry-Auth": auth}); code != 200 {
		t.Fatalf("with credentials: %d %s", code, b)
	}
	for i, c := range f.creds[1:] {
		if c == nil || c.Username != "user" {
			t.Fatalf("call %d (%s) had no credentials", i+1, f.calls[i+1])
		}
	}
}
