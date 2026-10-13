package conformance

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// R is a Registry with the steps the registry cases share, each
// failing t when it doesn't do what it says.
type R struct {
	Registry
	T *testing.T
}

// Resp is a registry or facade answer, read whole.
type Resp struct {
	Code   int
	Header http.Header
	Body   []byte
}

// ErrCode is the first OCI error code of the body, or "".
func (r Resp) ErrCode() string {
	var out struct {
		Errors []struct{ Code string } `json:"errors"`
	}
	json.Unmarshal(r.Body, &out)
	if len(out.Errors) == 0 {
		return ""
	}
	return out.Errors[0].Code
}

func read(t *testing.T, res *http.Response, err error) Resp {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return Resp{Code: res.StatusCode, Header: res.Header, Body: b}
}

func request(method, path, ctype string, body []byte, hdr map[string]string) *http.Request {
	req, _ := http.NewRequest(method, "http://registry.silo.internal"+path, bytes.NewReader(body))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return req
}

// Req is a registry request as caller; path is /v2/... or an upload's
// Location.
func (r R) Req(caller, method, path, ctype string, body []byte) Resp {
	r.T.Helper()
	if i := strings.Index(path, "/v2/"); i > 0 {
		path = path[i:]
	}
	res, err := r.Do(caller, request(method, path, ctype, body, nil))
	return read(r.T, res, err)
}

// ReqAuth is a registry GET as caller with Basic credentials.
func (r R) ReqAuth(caller, path, user, pass string) Resp {
	r.T.Helper()
	req := request("GET", path, "", nil, nil)
	req.SetBasicAuth(user, pass)
	res, err := r.Do(caller, req)
	return read(r.T, res, err)
}

// Dock is a Docker Engine API request as caller, with a JSON body if
// body isn't nil, and headers.
func (r R) Dock(caller, method, path string, body any, hdr map[string]string) Resp {
	r.T.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	ctype := ""
	if body != nil {
		ctype = "application/json"
	}
	res, err := r.Docker(caller, request(method, path, ctype, b, hdr))
	return read(r.T, res, err)
}

// OK is a fence API call that must answer 200.
func (r R) OK(caller, method, path string, body any) map[string]any {
	r.T.Helper()
	code, out := r.API(caller, method, path, body)
	if code != 200 {
		r.T.Fatalf("%s %s as %s: %d %v", method, path, caller, code, out)
	}
	return out
}

// Mk makes a silo name under parent (a path, or "" for the root's
// child) as the root, with quota enough for the cases (a fifth of it
// for a grandchild, which its parent delegates) and extra fields, and
// returns its id.
func (r R) Mk(parent, name string, extra map[string]any) string {
	r.T.Helper()
	bits, n := 24, 1
	if parent == "" {
		parent, bits, n = "-"+r.Root(), 20, 5
	}
	body := map[string]any{"name": name, "address": map[string]any{"prefix_length": bits},
		"quota": map[string]any{"global": map[string]any{"vcpu_m": 400 * n, "memory_mb": 400 * n, "disk_gb": 4 * n,
			"containers": n, "networks": n, "volumes": n, "image_repos": n}}}
	for k, v := range extra {
		body[k] = v
	}
	return r.OK(r.Root(), "POST", "/silo/v1/silos/"+parent+"/children", body)["id"].(string)
}

// PushBlob uploads b to name monolithically as caller and returns its
// digest.
func (r R) PushBlob(caller, name string, b []byte) string {
	r.T.Helper()
	d := Digest(b)
	if res := r.Req(caller, "POST", "/v2/"+name+"/blobs/uploads/?digest="+d, "application/octet-stream", b); res.Code != 201 {
		r.T.Fatalf("pushing a blob to %s as %s: %d %s", name, caller, res.Code, res.Body)
	}
	return d
}

// Push pushes im to name:tag as caller.
func (r R) Push(caller, name, tag string, im Image) {
	r.T.Helper()
	r.PushBlob(caller, name, im.Config)
	r.PushBlob(caller, name, im.Layer)
	if res := r.Req(caller, "PUT", "/v2/"+name+"/manifests/"+tag, MediaOCIManifest, im.Manifest); res.Code != 201 {
		r.T.Fatalf("pushing %s:%s as %s: %d %s", name, tag, caller, res.Code, res.Body)
	}
}

// Last is the newest audit record of kind.
func (r R) Last(kind string) map[string]any {
	r.T.Helper()
	recs := r.Audit(kind)
	if len(recs) == 0 {
		r.T.Fatalf("no %s record", kind)
	}
	return recs[len(recs)-1]
}
