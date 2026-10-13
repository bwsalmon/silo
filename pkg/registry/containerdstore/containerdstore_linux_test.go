package containerdstore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1/random"

	"github.com/bwsalmon/silo/pkg/backend/local/nsd"
	"github.com/bwsalmon/silo/pkg/backend/local/rootless"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/registry"
	"github.com/bwsalmon/silo/pkg/registry/containerdstore"
	"github.com/bwsalmon/silo/pkg/registry/contenttest"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
)

// TestMain doubles as the holder.
func TestMain(m *testing.M) {
	if cfg := os.Getenv("_SILO_CONTAINERDSTORE_TEST_HOLDER"); cfg != "" {
		var c nsd.Config
		if err := json.Unmarshal([]byte(cfg), &c); err != nil {
			panic(err)
		}
		if err := nsd.Run(context.Background(), c); err != nil {
			os.Stderr.WriteString("holder: " + err.Error() + "\n")
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// need skips without a prerequisite, and fails instead under
// SILO_REQUIRE_NSD, which CI's nsd job sets.
func need(t *testing.T, what string, ok bool) {
	t.Helper()
	if ok {
		return
	}
	if os.Getenv("SILO_REQUIRE_NSD") != "" {
		t.Fatalf("SILO_REQUIRE_NSD is set, but %s", what)
	}
	t.Skip(what)
}

func subID(t *testing.T, file string, id int) nsd.SubID {
	t.Helper()
	b, err := os.ReadFile(file)
	need(t, file+" is unreadable", err == nil)
	user := os.Getenv("USER")
	if user == "" {
		out, _ := exec.Command("id", "-un").Output()
		user = strings.TrimSpace(string(out))
	}
	s, ok, err := nsd.ParseSubID(bytes.NewReader(b), user, id)
	need(t, "no range for "+user+" in "+file, err == nil && ok)
	return s
}

// start runs silo-containerd as step 16 does, in a holder of its own,
// and returns a Store on it.
func start(t *testing.T) *containerdstore.Store {
	var missing []string
	for _, c := range nsd.Failed(nsd.Probe()) {
		missing = append(missing, c.Name+": "+c.Detail)
	}
	need(t, "this host can't run the holder: "+strings.Join(missing, "; "), len(missing) == 0)
	bin, err := exec.LookPath("containerd")
	need(t, "containerd is not on PATH", err == nil)
	m := nsd.HolderMapping(os.Getuid(), os.Getgid(), subID(t, "/etc/subuid", os.Getuid()), subID(t, "/etc/subgid", os.Getgid()))
	dir := t.TempDir()
	cfg := nsd.Config{
		Socket: filepath.Join(dir, "nsd.sock"), State: filepath.Join(dir, "nsd.state"), PinDir: nsd.DefaultPinDir,
		Bridge: "gw0", Addrs: []netip.Prefix{netip.MustParsePrefix("10.98.0.1/16")},
	}
	b, _ := json.Marshal(cfg)
	holder, err := nsd.Start(os.Args[0], []string{"-test.run=^$"}, append(os.Environ(), "_SILO_CONTAINERDSTORE_TEST_HOLDER="+string(b)), m, os.Stderr, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Process.Kill(); holder.Wait() })
	for i := 0; ; i++ {
		c, err := nsd.Dial(cfg.Socket)
		if err == nil {
			c.Close()
			break
		}
		if i > 100 {
			t.Fatalf("holder never listened: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	base, err := os.MkdirTemp("", "silo-containerdstore-")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(base, 0o755)
	t.Cleanup(func() {
		nsd.JoinExec(holder.Process.Pid, os.Args[0], "rm", "-rf", base).Run()
		os.RemoveAll(base)
	})
	p := rootless.Paths{DataDir: filepath.Join(base, "data"), RuntimeDir: filepath.Join(base, "rt")}
	cmd, err := rootless.StartContainerd(p, holder.Process.Pid, os.Args[0], bin)
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGTERM)
		cmd.Wait()
		if t.Failed() {
			t.Logf("containerd's log:\n%s", log.String())
		}
	})
	s, err := containerdstore.New(containerdstore.Options{Address: p.ContainerdSocket(), Spool: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for i := 0; ; i++ {
		err := s.Ping(context.Background())
		if err == nil {
			break
		}
		if i > 100 {
			t.Fatalf("containerd never answered: %v\n%s", err, log.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	return s
}

// TestContract holds silo-containerd to the contract the registry's
// fake keeps (registry.Memory, TestMemoryContract).
func TestContract(t *testing.T) {
	contenttest.Run(t, start(t))
}

type asSilo struct{ silo string }

func (a asSilo) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Silo-Caller", a.silo)
	return http.DefaultTransport.RoundTrip(r)
}

// TestRegistry is the registry over silo-containerd, end to end: a
// push by crane in chunks, a pull of the same digests, a chunk refused
// for disk_gb and taken back, and a manifest delete after which
// containerd's GC removes the blobs only that manifest held.
func TestRegistry(t *testing.T) {
	s := start(t)
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}
	caller := func(r *http.Request) fenceapi.Caller {
		return fenceapi.Caller{Silo: r.Header.Get("Silo-Caller"), Via: fenceapi.ViaOrigin}
	}
	api := fenceapi.New(db, fenceapi.Options{Caller: func(r *http.Request) (fenceapi.Caller, error) { return caller(r), nil }})
	defer api.Close()
	apiSrv := httptest.NewServer(api)
	defer apiSrv.Close()
	// A grace of seconds, so that the upload sessions' leases end
	// within the test.
	reg := registry.New(registry.Options{Store: db, API: api, Content: s, Grace: 3 * time.Second})
	api.SetRegistry(reg)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reg.ServeHTTP(w, r.WithContext(fenceapi.ContextWithCaller(r.Context(), caller(r))))
	}))
	defer srv.Close()
	call := func(as, method, path string, body any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, apiSrv.URL+path, bytes.NewReader(b))
		req.Header.Set("Silo-Caller", as)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out := map[string]any{}
		json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode != 200 {
			t.Fatalf("%s %s: %d %v", method, path, resp.StatusCode, out)
		}
		return out
	}
	root := call("", "POST", "/silod/bootstrap", map[string]any{"regions": []string{"us-central1"},
		"capacity":     map[string]any{"global": map[string]any{"disk_gb": 100, "image_repos": 100}},
		"grant_policy": map[string]any{"exports": "allow"}, "admins": []string{"user:admin@example.com"}})["id"].(string)
	team := call(root, "POST", "/silo/v1/silos/-"+root+"/children", map[string]any{"name": "team",
		"address": map[string]any{"prefix_length": 16},
		"quota":   map[string]any{"global": map[string]any{"disk_gb": "0.004", "image_repos": 10}}})["id"].(string)
	host := strings.TrimPrefix(srv.URL, "http://")
	opts := []crane.Option{crane.Insecure, crane.WithTransport(asSilo{team})}

	img, err := random.Image(1<<20, 2)
	if err != nil {
		t.Fatal(err)
	}
	ref := host + "/team/app:1"
	if err := crane.Push(img, ref, opts...); err != nil {
		t.Fatalf("crane push: %v", err)
	}
	want, _ := img.Digest()
	got, err := crane.Pull(ref, opts...)
	if err != nil {
		t.Fatalf("crane pull: %v", err)
	}
	if d, _ := got.Digest(); d != want {
		t.Fatalf("pulled %s, pushed %s", d, want)
	}
	layers, _ := got.Layers()
	var blobs []string
	for _, l := range layers {
		d, _ := l.Digest()
		blobs = append(blobs, d.String())
		rc, err := l.Compressed()
		if err != nil {
			t.Fatalf("pulling a layer: %v", err)
		}
		rc.Close()
	}
	cfg, _ := got.ConfigName()
	blobs = append(blobs, cfg.String(), want.String())
	imgs, err := s.ListImages(ctx, registry.Namespace)
	if err != nil || len(imgs) != 1 || imgs[0].Digest != want.String() || len(imgs[0].Refs) != 3 {
		t.Fatalf("silo-registry's records: %+v %v", imgs, err)
	}

	// A push the silo has no room for: 0.004 GiB is about 4 MiB, and
	// the first image holds 2. Its chunk is taken back out of the
	// ingest's spool, since containerd can't truncate one itself.
	big, _ := random.Image(3<<20, 1)
	if err := crane.Push(big, host+"/team/big:1", opts...); err == nil {
		t.Fatal("a push over the silo's disk_gb succeeded")
	}

	// The manifest goes; its blobs go with containerd's GC once the
	// upload sessions' grace leases have expired.
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v2/team/app/manifests/"+want.String(), nil)
	req.Header.Set("Silo-Caller", team)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("manifest delete: %v %v", resp, err)
	}
	resp.Body.Close()
	time.Sleep(4200 * time.Millisecond)
	if err := s.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	for _, d := range blobs {
		if _, err := s.Size(ctx, d); !errors.Is(err, registry.ErrUnknown) {
			t.Errorf("%s is still in the content store after its manifest went: %v", d, err)
		}
	}
}
