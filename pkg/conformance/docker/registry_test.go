// Package docker holds the conformance suite's Docker facade cases
// (local-backend.md §13, call L24): today the registry's, of
// local-registry.md §12, run against every target whose registry has a
// Docker facade.
package docker_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/conformance"
	"github.com/bwsalmon/silo/pkg/conformance/targets"
)

type obj = map[string]any

const v = "/v1.47"

func each(t *testing.T, fn func(t *testing.T, r conformance.R)) {
	conformance.ForEach(t, targets.All(), func(t *testing.T, reg conformance.Registry) {
		fn(t, conformance.R{Registry: reg, T: t})
	})
}

// ok makes a facade call that must answer 2xx.
func ok(r conformance.R, caller, method, path string, body any) conformance.Resp {
	r.T.Helper()
	res := r.Dock(caller, method, path, body, nil)
	if res.Code/100 != 2 {
		r.T.Fatalf("%s %s as %s: %d %s", method, path, caller, res.Code, res.Body)
	}
	return res
}

// fails checks a facade call answers code with want in its message.
func fails(r conformance.R, code int, want, caller, method, path string, body any, hdr map[string]string) {
	r.T.Helper()
	res := r.Dock(caller, method, path, body, hdr)
	var out struct{ Message string }
	json.Unmarshal(res.Body, &out)
	if res.Code != code || !strings.Contains(out.Message+string(res.Body), want) {
		r.T.Errorf("%s %s as %s: %d %s, want %d with %q", method, path, caller, res.Code, res.Body, code, want)
	}
}

// pulled is a pull's progress stream, which must not end in an error.
func pulled(r conformance.R, caller, ref string, hdr map[string]string) {
	r.T.Helper()
	res := r.Dock(caller, "POST", v+"/images/create?fromImage="+ref, nil, hdr)
	if res.Code != 200 || strings.Contains(string(res.Body), `"error"`) {
		r.T.Fatalf("pull %s as %s: %d %s", ref, caller, res.Code, res.Body)
	}
}

// run creates and starts a container of image as caller.
func run(r conformance.R, caller, name, image string) {
	r.T.Helper()
	var out struct{ Id string }
	json.Unmarshal(ok(r, caller, "POST", v+"/containers/create?name="+name, obj{"Image": image, "Cmd": []string{"/bin/app"}}).Body, &out)
	ok(r, caller, "POST", v+"/containers/"+out.Id+"/start", nil)
}

// push tags caller's alpine:3 as registry.silo.internal/<repo>:tag and
// pushes it.
func push(r conformance.R, caller, repo, tag string) {
	r.T.Helper()
	pulled(r, caller, "alpine:3", nil)
	ok(r, caller, "POST", v+"/images/alpine:3/tag?repo=registry.silo.internal/"+repo+"&tag="+tag, nil)
	res := r.Dock(caller, "POST", v+"/images/registry.silo.internal/"+repo+"/push?tag="+tag, nil, nil)
	if res.Code != 200 || !strings.Contains(string(res.Body), `"aux"`) {
		r.T.Fatalf("push %s:%s: %d %s", repo, tag, res.Code, res.Body)
	}
}

func silos(r conformance.R) (a, b, c string) {
	all := map[string]any{"allow": conformance.Hosts}
	return r.Mk("", "team-a", obj{"egress": all}), r.Mk("", "team-b", obj{"egress": all}), r.Mk("", "team-c", obj{"egress": all})
}

// docker tag and docker push to registry.silo.internal/<own path>/app,
// then docker pull and docker run of it from another container of the
// same silo and from an importer; a sibling can't pull it.
func TestDockerPushPullRun(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		a, b, c := silos(r)
		push(r, a, "team-a/app", "1")
		ok(r, a, "DELETE", v+"/images/registry.silo.internal/team-a/app:1", nil)
		pulled(r, a, "registry.silo.internal/team-a/app:1", nil)
		run(r, a, "from-repo", "registry.silo.internal/team-a/app:1")
		fails(r, 404, "manifest unknown", c, "POST", v+"/images/create?fromImage=registry.silo.internal/team-a/app&tag=1", nil, nil)
		r.OK(a, "POST", "/silo/v1/self/exports", obj{"name": "imgs", "protocol": "registry", "repos": []string{"app"}, "to": []string{"/team-b"}})
		r.OK(b, "POST", "/silo/v1/self/imports", obj{"export": "/team-a/imgs"})
		pulled(r, b, "registry.silo.internal/team-a/app:1", nil)
		run(r, b, "imported", "registry.silo.internal/team-a/app:1")
	})
}

// A push to any other registry is 501 with the message.
func TestDockerPushElsewhere(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		a, _, _ := silos(r)
		pulled(r, a, "alpine:3", nil)
		for _, name := range []string{"ghcr.io/o/r", "alpine", "registry.example.com/x"} {
			fails(r, 501, "pushes go to registry.silo.internal only", a, "POST", v+"/images/"+name+"/push?tag=latest", nil, nil)
		}
	})
}

// A pull from a local repo in a silo with an empty egress policy works,
// and a pull from the upstream doesn't.
func TestDockerLocalPullWithoutEgress(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		a, _, _ := silos(r)
		push(r, a, "team-a/app", "1")
		r.OK(r.Root(), "PATCH", "/silo/v1/silos/team-a", obj{"egress": obj{"allow": []string{}}})
		ok(r, a, "DELETE", v+"/images/alpine:3", nil)
		res := r.Dock(a, "POST", v+"/images/create?fromImage=alpine:3", nil, nil)
		if res.Code == 200 && !strings.Contains(string(res.Body), "egress") {
			t.Errorf("an upstream pull with empty egress: %d %s", res.Code, res.Body)
		}
		pulled(r, a, "registry.silo.internal/team-a/app:1", nil)
	})
}

// docker save then docker load round-trips an image with the same
// digests; a load of an archive with a mismatched digest is refused.
func TestDockerSaveLoad(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		a, b, _ := silos(r)
		pulled(r, a, "alpine:3", nil)
		var before, after struct{ Id string }
		json.Unmarshal(ok(r, a, "GET", v+"/images/alpine:3/json", nil).Body, &before)
		archive := ok(r, a, "GET", v+"/images/get?names=alpine:3", nil).Body
		load := func(caller string, body []byte) conformance.Resp {
			t.Helper()
			resp, err := r.Docker(caller, rawPost(v+"/images/load", body))
			return readResp(t, resp, err)
		}
		if res := load(b, archive); res.Code != 200 || !strings.Contains(string(res.Body), "Loaded image") {
			t.Fatalf("load: %d %s", res.Code, res.Body)
		}
		json.Unmarshal(ok(r, b, "GET", v+"/images/alpine:3/json", nil).Body, &after)
		if after.Id != before.Id {
			t.Errorf("load gave %s, saved %s", after.Id, before.Id)
		}
		// The layer's bytes changed under its name: refused.
		up := r.Upstream()
		bad := []byte(strings.Replace(string(archive), string(up.Public.Layer), "the alpine LAYER", 1))
		c := r.Mk("", "team-load", nil)
		if res := load(c, bad); res.Code == 200 && !strings.Contains(string(res.Body), "error") {
			t.Errorf("a load with a mismatched digest: %d %s", res.Code, res.Body)
		}
		if res := r.Dock(c, "GET", v+"/images/alpine:3/json", nil, nil); res.Code != 404 {
			t.Errorf("a refused load wrote the image: %d", res.Code)
		}
	})
}

// docker commit of a vm container is 501; docker build is still 501,
// and its message names kaniko and BuildKit.
func TestDockerCommitVMAndBuild(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		a, _, _ := silos(r)
		pulled(r, a, "alpine:3", nil)
		var out struct{ Id string }
		json.Unmarshal(ok(r, a, "POST", v+"/containers/create?name=vm1", obj{"Image": "alpine:3", "Cmd": []string{"/bin/app"}, "HostConfig": obj{"Runtime": "silo-vm"}}).Body, &out)
		fails(r, 501, "", a, "POST", v+"/commit?container=vm1&repo=x&tag=1", nil, nil)
		fails(r, 501, "kaniko or BuildKit", a, "POST", v+"/build", nil, nil)
	})
}

// docker login to an upstream with good and bad credentials gives
// Login Succeeded and 401, and stores nothing.
func TestDockerLogin(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		a, _, _ := silos(r)
		up := r.Upstream()
		var got struct{ Status string }
		json.Unmarshal(ok(r, a, "POST", v+"/auth", obj{"username": up.User, "password": up.Pass, "serveraddress": "https://index.docker.io/v1/"}).Body, &got)
		if got.Status != "Login Succeeded" {
			t.Errorf("good login: %+v", got)
		}
		fails(r, 401, "unauthorized", a, "POST", v+"/auth", obj{"username": up.User, "password": "wrong", "serveraddress": "docker.io"}, nil)
		for _, kind := range []string{"registry.cache", "registry.pull", "registry.denied", "docker.auth"} {
			b, _ := json.Marshal(r.Audit(kind))
			if strings.Contains(string(b), up.Pass) {
				t.Errorf("a %s record holds the password", kind)
			}
		}
	})
}

// docker pull of a private upstream image with X-Registry-Auth works,
// and without it fails with Docker's unauthorized.
func TestDockerPrivatePull(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		a, b, _ := silos(r)
		up := r.Upstream()
		auth, _ := json.Marshal(obj{"username": up.User, "password": up.Pass, "serveraddress": "docker.io"})
		pulled(r, a, "private/app:1", map[string]string{"X-Registry-Auth": base64.URLEncoding.EncodeToString(auth)})
		fails(r, 401, "unauthorized", b, "POST", v+"/images/create?fromImage=private/app:1", nil, nil)
	})
}

func rawPost(path string, body []byte) *http.Request {
	req, _ := http.NewRequest("POST", "http://docker.silo.internal"+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-tar")
	return req
}

func readResp(t *testing.T, res *http.Response, err error) conformance.Resp {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return conformance.Resp{Code: res.StatusCode, Header: res.Header, Body: b}
}
