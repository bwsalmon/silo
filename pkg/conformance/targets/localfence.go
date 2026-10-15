package targets

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/conformance"
)

// The "local/sandbox" target's Fence is the local backend installed on
// this host (FindInstall), as local-conformance installs it:
//
//   - the fence API as the root's members on silo.sock, whose peer uid
//     is the root's admin; as any other silo, from a probe in it, to
//     the gateway's fence API at gw0:8080, which names the caller by
//     its address;
//   - the Docker facade as the root on its socket, and as any other
//     silo from a probe in it, at docker.silo.internal:2375;
//   - a probe is a sandbox-class container (HostConfig.Runtime
//     silo-sandbox, since the root's default class is vm wherever
//     /dev/kvm works) made through the silo's own Docker socket by the
//     root, of an image that is a static busybox and probeagent, which
//     serves TCP and UDP on probeagent.Ports and runs a Run's checks
//     by exec, streaming back ProbeScript's report lines.
//
// Each silo the Fence makes is deleted, after its probes, when the test
// ends: every test makes its own tree in the one install.
type localFence struct {
	t     testing.TB
	in    *Install
	ctx   context.Context
	agent string // the probe agent binary

	mu    sync.Mutex
	silos map[string]*localSilo // by id, the root's included
}

type localSilo struct {
	id     string
	image  string // the probe image, once loaded
	probes []*localProbe
}

// probeRuntime is the sandbox class's runtime name in the facade.
const probeRuntime = "silo-sandbox"

func (l Local) installedFence(t testing.TB) (conformance.Fence, error) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	fctx, fcancel := context.WithTimeout(ctx, time.Minute)
	defer fcancel()
	in, err := FindInstall(fctx)
	if err != nil {
		return nil, &conformance.NotRunnable{Why: "no local install answers (local-conformance makes one with silo install --backend local): " + err.Error()}
	}
	if _, err := exec.LookPath("busybox"); err != nil {
		return nil, &conformance.NotRunnable{Why: "no busybox to make the probe image of"}
	}
	agent, err := probeAgent()
	if err != nil {
		return nil, fmt.Errorf("building the probe agent: %v", err)
	}
	f := &localFence{t: t, in: in, ctx: ctx, agent: agent, silos: map[string]*localSilo{in.Root: {id: in.Root}}}
	code, out := f.rootAPI("GET", "/silo/v1/self", nil)
	if code != 200 {
		return nil, fmt.Errorf("the root's fence API on %s: %d %v", in.Socket, code, out)
	}
	// The cases' silos ask for any name, which no child may have that
	// its parent hasn't: local-conformance installs with
	// --egress-allow '*', where install's default is none.
	eg, _ := out["egress"].(map[string]any)
	own, _ := eg["own"].(map[string]any)
	if allow, _ := own["allow"].([]any); !slices.Contains(allow, any("*")) {
		return nil, &conformance.NotRunnable{Why: fmt.Sprintf("the installed root's egress allows %v, not *: install with --egress-allow '*' for the fence cases", allow)}
	}
	return f, nil
}

var agentOnce struct {
	sync.Once
	path string
	err  error
}

// probeAgent builds probeagent, static, once per test binary.
func probeAgent() (string, error) {
	agentOnce.Do(func() {
		dir, err := os.MkdirTemp("", "silo-probeagent")
		if err != nil {
			agentOnce.err = err
			return
		}
		agentOnce.path = filepath.Join(dir, "probeagent")
		cmd := exec.Command("go", "build", "-o", agentOnce.path, "github.com/bwsalmon/silo/pkg/conformance/targets/probeagent")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			agentOnce.err = fmt.Errorf("%v: %s", err, out)
		}
	})
	return agentOnce.path, agentOnce.err
}

func (f *localFence) Root() string { return f.in.Root }

func (f *localFence) Gateway() string { return "10.255.255.1" }

// API calls the fence API as the root's members on silo.sock, and as
// any other silo from a probe in it.
func (f *localFence) API(caller, method, path string, body any) (int, map[string]any) {
	f.t.Helper()
	switch caller {
	case "":
		return 0, map[string]any{"error": "not offered: on an installed local target, every call comes from a silo's member or a silo's container"}
	case f.in.Root:
		return f.rootAPI(method, path, body)
	}
	return f.fromProbe(caller, method, "http://"+f.Gateway()+":8080"+path, body)
}

func (f *localFence) rootAPI(method, path string, body any) (int, map[string]any) {
	f.t.Helper()
	create := method == "POST" && strings.HasSuffix(path, "/children")
	if m, ok := body.(map[string]any); ok && create {
		// The cases' quotas are a big host's; a runner's disk has room
		// for a few gigabytes a silo.
		c := map[string]any{}
		for k, v := range m {
			c[k] = v
		}
		q := map[string]any{"vcpu_m": 2000, "memory_mb": 2048, "disk_gb": 6, "containers": 6, "networks": 2, "volumes": 2}
		if !strings.HasPrefix(path, "/silo/v1/silos/-") {
			q = map[string]any{"vcpu_m": 1000, "memory_mb": 1024, "disk_gb": 2, "containers": 2, "networks": 1, "volumes": 1}
		}
		c["quota"] = map[string]any{"global": q}
		body = c
	}
	code, raw := f.do(UnixClient(f.in.Socket), method, "http://silo"+path, "application/json", body)
	out := decodeJSON(raw)
	if code == 200 && create {
		id, _ := out["id"].(string)
		if id == "" {
			f.t.Fatalf("a made silo with no id: %v", out)
		}
		f.mu.Lock()
		f.silos[id] = &localSilo{id: id}
		f.mu.Unlock()
		f.t.Cleanup(func() {
			if code, out := f.rootAPI("DELETE", "/silo/v1/silos/-"+id+"?force=true", nil); code != 200 && code != 404 {
				f.t.Errorf("deleting %s: %d %v", id, code, out)
				return
			}
			// A delete is an operation: the next test's tree has the same
			// names, so wait for it to be done.
			for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(500 * time.Millisecond) {
				code, out := f.rootAPI("GET", "/silo/v1/silos/-"+id, nil)
				if code == 404 {
					return
				}
				if time.Now().After(deadline) {
					f.t.Errorf("%s still there two minutes after its delete: %d %v", id, code, out)
					return
				}
			}
		})
	}
	return code, out
}

// do makes one HTTP call, and returns its status and body.
func (f *localFence) do(c *http.Client, method, u, ctype string, body any) (int, []byte) {
	f.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		js, _ := json.Marshal(b)
		rd = bytes.NewReader(js)
	}
	req, _ := http.NewRequestWithContext(f.ctx, method, u, rd)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	res, err := c.Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, u, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, raw
}

// fromProbe makes an HTTP call from a probe in silo, one a case made
// there or one made for it, and decodes its JSON answer.
func (f *localFence) fromProbe(silo, method, u string, body any) (int, map[string]any) {
	f.t.Helper()
	code, raw := f.callFrom(silo, method, u, nil, body)
	return code, decodeJSON(raw)
}

func (f *localFence) callFrom(silo, method, u string, headers []string, body any) (int, []byte) {
	f.t.Helper()
	p := f.callerProbe(silo)
	c := conformance.Check{ID: "call", Kind: "call", Method: method, URL: u, Headers: headers}
	switch b := body.(type) {
	case nil:
	case []byte:
		c.Body = string(b)
	default:
		js, _ := json.Marshal(b)
		c.Body = string(js)
	}
	r := p.Run([]conformance.Check{c})["call"]
	if !r.OK {
		f.t.Fatalf("%s %s from a probe in %s: %s", method, u, silo, r.Detail)
	}
	fields := strings.Fields(r.Detail)
	code := 0
	var raw []byte
	if len(fields) > 0 {
		fmt.Sscanf(fields[0], "%d", &code)
	}
	if len(fields) > 1 {
		raw, _ = base64.StdEncoding.DecodeString(fields[1])
	}
	return code, raw
}

func (f *localFence) callerProbe(silo string) *localProbe {
	f.t.Helper()
	f.mu.Lock()
	s := f.silos[silo]
	var p *localProbe
	if s != nil && len(s.probes) > 0 {
		p = s.probes[0]
	}
	f.mu.Unlock()
	if s == nil {
		f.t.Fatalf("calling as %s: not a silo this Fence made", silo)
	}
	if p == nil {
		p = f.Probe(f.t, silo, "caller").(*localProbe)
	}
	return p
}

// Docker sends req as caller to its own silo's facade: the root's on
// its socket, any other's from a probe in it.
func (f *localFence) Docker(caller string, req *http.Request) (*http.Response, error) {
	f.t.Helper()
	path := req.URL.RequestURI()
	if caller == f.in.Root {
		r2 := req.Clone(f.ctx)
		r2.URL = &url.URL{Scheme: "http", Host: "docker", Path: req.URL.Path, RawQuery: req.URL.RawQuery}
		r2.Host = "docker"
		r2.RequestURI = ""
		return UnixClient(f.in.Docker).Do(r2)
	}
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	var headers []string
	for k, vs := range req.Header {
		if k != "Content-Type" || len(body) > 0 {
			headers = append(headers, k+": "+strings.Join(vs, ", "))
		}
	}
	var b any
	if len(body) > 0 {
		b = body
	}
	code, raw := f.callFrom(caller, req.Method, "http://docker.silo.internal:2375"+path, headers, b)
	return &http.Response{StatusCode: code, Status: http.StatusText(code), Header: http.Header{},
		Body: io.NopCloser(bytes.NewReader(raw)), Request: req}, nil
}

// dock makes a Docker call as the root, to silo's facade on its own
// socket, and fails the test on an error status.
func (f *localFence) dock(t testing.TB, silo, method, path, ctype string, body any) []byte {
	t.Helper()
	sock := filepath.Join(filepath.Dir(f.in.Docker), silo+".sock")
	// silod opens a silo's socket within a few seconds of its making.
	for deadline := time.Now().Add(30 * time.Second); ; {
		if fi, err := os.Stat(sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s's Docker socket %s never appeared", silo, sock)
		}
		time.Sleep(200 * time.Millisecond)
	}
	code, raw := f.do(UnixClient(sock), method, "http://docker/v1.47"+path, ctype, body)
	if code/100 != 2 {
		t.Fatalf("%s %s in %s: %d %s", method, path, silo, code, raw)
	}
	return raw
}

// Probe makes a sandbox-class container in silo's default network,
// serving on probeagent's ports, and deletes it when t ends.
func (f *localFence) Probe(t testing.TB, silo, name string) conformance.Probe {
	t.Helper()
	f.mu.Lock()
	s := f.silos[silo]
	f.mu.Unlock()
	if s == nil {
		t.Fatalf("a probe in %s: not a silo this Fence made", silo)
	}
	if s.image == "" {
		image := fmt.Sprintf("silo-probe:%d", time.Now().UnixNano())
		f.dock(t, silo, "POST", "/images/load?quiet=1", "application/x-tar", probeArchive(t, f.agent, image))
		s.image = image
	}
	var made struct{ Id string }
	json.Unmarshal(f.dock(t, silo, "POST", "/containers/create?name="+url.QueryEscape(name), "application/json", map[string]any{
		"Image": s.image, "Cmd": []string{"/bin/probeagent", "serve"}, "OpenStdin": true,
		// Small, so that a nested silo's quota holds two.
		"HostConfig": map[string]any{"Runtime": probeRuntime, "StorageOpt": map[string]string{"size": "256M"},
			"Memory": 128 << 20, "NanoCpus": 250_000_000}}), &made)
	if made.Id == "" {
		t.Fatalf("probe %s in %s: no container id", name, silo)
	}
	p := &localProbe{f: f, t: t, silo: silo, id: made.Id, name: name}
	t.Cleanup(func() {
		f.do(UnixClient(p.socket()), "DELETE", "http://docker/v1.47/containers/"+p.id+"?force=1", "", nil)
	})
	f.dock(t, silo, "POST", "/containers/"+made.Id+"/start", "", nil)
	var in struct {
		NetworkSettings struct {
			Networks map[string]struct{ IPAddress string }
		}
	}
	json.Unmarshal(f.dock(t, silo, "GET", "/containers/"+made.Id+"/json", "", nil), &in)
	for _, n := range in.NetworkSettings.Networks {
		if a, err := netip.ParseAddr(n.IPAddress); err == nil {
			p.addr = a
		}
	}
	if !p.addr.IsValid() {
		t.Fatalf("probe %s in %s has no address: %v", name, silo, in.NetworkSettings.Networks)
	}
	// Ready when its servers are: its own log says so.
	for deadline := time.Now().Add(time.Minute); ; {
		logs := string(f.dock(t, silo, "GET", "/containers/"+made.Id+"/logs?stdout=1&stderr=1", "", nil))
		if strings.Contains(logs, "SILO-PROBE ready ok") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe %s in %s never got ready: %q", name, silo, logs)
		}
		time.Sleep(300 * time.Millisecond)
	}
	f.mu.Lock()
	s.probes = append(s.probes, p)
	f.mu.Unlock()
	return p
}

type localProbe struct {
	f        *localFence
	t        testing.TB
	silo, id string
	name     string
	addr     netip.Addr

	mu     sync.Mutex
	rounds int
}

func (p *localProbe) socket() string {
	return filepath.Join(filepath.Dir(p.f.in.Docker), p.silo+".sock")
}

func (p *localProbe) Addr() netip.Addr { return p.addr }

// Run sends the checks to the probe's agent as a round on its stdin,
// each check's id the round's name, a dot and the case's id, with a
// last check that marks the round done, and reads the round's report
// from the probe's log.
func (p *localProbe) Run(checks []conformance.Check) map[string]conformance.ProbeResult {
	p.t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rounds++
	round := fmt.Sprintf("r%d", p.rounds)
	var cs []conformance.Check
	for _, c := range checks {
		c.ID = round + "." + c.ID
		cs = append(cs, c)
	}
	cs = append(cs, conformance.Check{ID: round + ".done", Kind: "sh", Script: "true"})
	js, _ := json.Marshal(cs)
	req, _ := http.NewRequestWithContext(p.f.ctx, "POST", "http://docker/v1.47/containers/"+p.id+"/attach?stream=1&stdin=1", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	// No client timeout: one wraps a 101's body, which hides its Write.
	c := UnixClient(p.socket())
	c.Timeout = 0
	res, err := c.Do(req)
	if err != nil {
		p.t.Fatalf("probe %s: attach: %v", p.name, err)
	}
	if res.StatusCode != http.StatusSwitchingProtocols && res.StatusCode/100 != 2 {
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		p.t.Fatalf("probe %s: attach: %d %s", p.name, res.StatusCode, b)
	}
	w, ok := res.Body.(io.Writer)
	if !ok {
		p.t.Fatalf("probe %s: attach: %d, not a stream", p.name, res.StatusCode)
	}
	if _, err := w.Write([]byte(base64.StdEncoding.EncodeToString(js) + "\n")); err != nil {
		p.t.Fatalf("probe %s: sending round %s: %v", p.name, round, err)
	}
	deadline := time.Now().Add(5 * time.Minute)
	defer res.Body.Close()
	for {
		logs := string(p.f.dock(p.t, p.silo, "GET", "/containers/"+p.id+"/logs?stdout=1&stderr=1", "", nil))
		report, _ := conformance.ParseProbeReport(logs)
		if _, ok := report[round+".done"]; ok {
			out := map[string]conformance.ProbeResult{}
			for id, r := range report {
				if k, ok := strings.CutPrefix(id, round+"."); ok && k != "done" {
					out[k] = r
				}
			}
			return out
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("probe %s never reported round %s: %q", p.name, round, logs)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// Spoof isn't offered: a sandbox-class container's netstack is
// gVisor's, which gives a container raw sockets only when runsc runs
// with --net-raw, and the facade's containers have no CAP_NET_RAW.
// local/fake's probes spoof through the nftables evaluator.
func (p *localProbe) Spoof(netip.Addr, []conformance.Check) (map[string]conformance.ProbeResult, error) {
	return nil, conformance.ErrNotOffered
}

func decodeJSON(raw []byte) map[string]any {
	out := map[string]any{}
	json.Unmarshal(raw, &out)
	if len(out) == 0 && len(raw) > 0 {
		out["raw"] = string(raw)
	}
	return out
}

// probeArchive is a docker save archive of one image, tagged tag,
// whose one layer is a static busybox and its links, and probeagent.
func probeArchive(t testing.TB, agent, tag string) []byte {
	t.Helper()
	bbPath, err := exec.LookPath("busybox")
	if err != nil {
		t.Fatal(err)
	}
	bb, err := os.ReadFile(bbPath)
	if err != nil {
		t.Fatal(err)
	}
	ag, err := os.ReadFile(agent)
	if err != nil {
		t.Fatal(err)
	}
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	for _, dir := range []string{"bin", "dev", "etc", "proc", "root", "sys", "tmp"} {
		tw.WriteHeader(&tar.Header{Name: dir + "/", Typeflag: tar.TypeDir, Mode: 0o755})
	}
	for name, b := range map[string][]byte{"bin/busybox": bb, "bin/probeagent": ag} {
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(b))})
		tw.Write(b)
	}
	for _, a := range []string{"sh", "echo", "id", "ip", "cat", "sleep", "nc", "ping", "wget", "nslookup"} {
		tw.WriteHeader(&tar.Header{Name: "bin/" + a, Typeflag: tar.TypeSymlink, Linkname: "busybox"})
	}
	tw.Close()
	digest := func(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }
	ld := digest(layer.Bytes())
	cfg, _ := json.Marshal(map[string]any{"architecture": goruntime.GOARCH, "os": "linux",
		"config": map[string]any{"Env": []string{"PATH=/bin"}, "Cmd": []string{"sh"}},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{"sha256:" + ld}}})
	cd := digest(cfg)
	manifest, _ := json.Marshal([]map[string]any{{"Config": "blobs/sha256/" + cd, "RepoTags": []string{tag}, "Layers": []string{"blobs/sha256/" + ld}}})
	var out bytes.Buffer
	aw := tar.NewWriter(&out)
	for _, e := range []struct {
		name string
		b    []byte
	}{{"blobs/sha256/" + ld, layer.Bytes()}, {"blobs/sha256/" + cd, cfg}, {"manifest.json", manifest}} {
		aw.WriteHeader(&tar.Header{Name: e.name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(e.b))})
		aw.Write(e.b)
	}
	aw.Close()
	return out.Bytes()
}
