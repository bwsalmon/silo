package conformance

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// F is a Fence with the steps the fence, Docker and structural cases
// share, each failing t when it doesn't do what it says.
type F struct {
	Fence
	T *testing.T
}

// Mk makes silo name under parent (a path such as "/a", or "" for the
// root) as the root's members, with a /prefix of the parent's blocks,
// room for a few probes (less for a nested silo, out of its parent's), and extra fields, and returns its id.
func (f F) Mk(parent, name string, prefix int, extra map[string]any) string {
	f.T.Helper()
	quota := map[string]any{"vcpu_m": 8000, "memory_mb": 8192, "disk_gb": 100, "containers": 8, "networks": 4, "volumes": 4}
	if parent != "" {
		quota = map[string]any{"vcpu_m": 2000, "memory_mb": 2048, "disk_gb": 30, "containers": 2, "networks": 1, "volumes": 1}
	}
	body := map[string]any{"name": name, "address": map[string]any{"prefix_length": prefix},
		"quota": map[string]any{"global": quota}}
	for k, v := range extra {
		body[k] = v
	}
	p := "/silo/v1/silos/-" + f.Root() + "/children"
	if parent != "" {
		p = "/silo/v1/silos" + parent + "/children"
	}
	code, out := f.API(f.Root(), "POST", p, body)
	if code != 200 {
		f.T.Fatalf("making %s/%s: %d %v", parent, name, code, out)
	}
	return out["id"].(string)
}

// OK makes an API call as caller that must answer 200.
func (f F) OK(caller, method, path string, body any) map[string]any {
	f.T.Helper()
	code, out := f.API(caller, method, path, body)
	if code != 200 {
		f.T.Fatalf("%s %s as %s: %d %v", method, path, caller, code, out)
	}
	return out
}

// Refused makes an API call as caller that must be refused with the
// HTTP status want and, if code isn't "", the API's error code.
func (f F) Refused(want int, code, caller, method, path string, body any) {
	f.T.Helper()
	got, out := f.API(caller, method, path, body)
	if got != want {
		f.T.Errorf("%s %s as %s: %d %v, want %d", method, path, caller, got, out, want)
		return
	}
	if code != "" && !strings.Contains(jsonString(out), code) {
		f.T.Errorf("%s %s as %s: %v, want %s", method, path, caller, out, code)
	}
}

// DockerDo sends a Docker Engine API request as caller and reads its
// answer whole.
func (f F) DockerDo(caller, method, path string, body any) Resp {
	f.T.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req, _ := http.NewRequest(method, "http://docker"+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	res, err := f.Docker(caller, req)
	if err == ErrNotOffered {
		f.T.Skip("not offered: no Docker facade")
	}
	return read(f.T, res, err)
}

// Expect checks that check id's outcome is want: reached or not. A
// check the target reported unsupported skips the test.
func Expect(t *testing.T, res map[string]ProbeResult, id string, want bool) {
	t.Helper()
	r, ok := res[id]
	switch {
	case !ok:
		t.Errorf("%s: no result", id)
	case !r.OK && strings.HasPrefix(r.Detail, Unsupported):
		t.Skipf("%s: %s", id, r.Detail)
	case r.OK != want:
		t.Errorf("%s: ok=%v (%s), want ok=%v", id, r.OK, r.Detail, want)
	}
}

// Reach is the checks that try TCP, UDP and ICMP from a probe to host,
// with ids prefix-tcp, prefix-udp and prefix-icmp.
func Reach(prefix, host string, port int) []Check {
	return []Check{
		{ID: prefix + "-tcp", Kind: "tcp", Host: host, Port: port},
		{ID: prefix + "-udp", Kind: "udp", Host: host, Port: port},
		{ID: prefix + "-icmp", Kind: "icmp", Host: host},
	}
}

// ExpectReach checks every result of Reach(prefix, ...) against want.
func ExpectReach(t *testing.T, res map[string]ProbeResult, prefix string, want bool) {
	t.Helper()
	for _, k := range []string{"tcp", "udp", "icmp"} {
		Expect(t, res, prefix+"-"+k, want)
	}
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
