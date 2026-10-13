package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/agents"
	"github.com/bwsalmon/silo/pkg/backend/local/gitstore"
	"github.com/bwsalmon/silo/pkg/backend/local/storage"
	"github.com/bwsalmon/silo/pkg/client"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
)

// silod is an in-process silod serving the fence API on SQLite.
type silod struct {
	t    *testing.T
	url  string
	root string // the root silo's id
	// stdin is what the next command reads on its stdin.
	stdin string
}

func startSilod(t *testing.T) *silod {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}
	silos := filepath.Join(t.TempDir(), "silos")
	if err := os.Mkdir(silos, 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &agents.Fake{AutoConnect: true}
	api := fenceapi.New(db, fenceapi.Options{Caller: func(r *http.Request) (fenceapi.Caller, error) {
		return fenceapi.Caller{Silo: r.Header.Get("Silo-Caller"), Via: fenceapi.ViaInternal}, nil
	}, ToolVMs: toolVMs{}, Runner: runner, Credentials: agents.StaticCredentials{"anthropic-api": "anthropic"},
		CredentialKey: bytes.Repeat([]byte{1}, 32), Repos: &gitstore.Host{Storage: &storage.Dir{Root: silos}}})
	runner.Connected = api.ToolServerConnected
	srv := httptest.NewServer(api)
	t.Cleanup(func() { srv.Close(); api.Close() })
	body, _ := json.Marshal(map[string]any{
		"regions":          []string{"us-central1", "us-east1"},
		"capacity":         map[string]any{"us-central1": map[string]any{"vcpu": 100, "spot_vcpu": 200, "memory_mb": 1 << 20}, "global": map[string]any{"agent_sessions": 10}},
		"model_budget_usd": "1000",
		"grant_policy":     map[string]any{"exports": "allow", "links": "allow"},
		"egress":           map[string]any{"allow": []string{"*"}},
		"admins":           []string{"user:admin@example.com"},
	})
	resp, err := http.Post(srv.URL+"/silod/bootstrap", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var root client.Silo
	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil || resp.StatusCode != 200 {
		t.Fatalf("bootstrap: %d %v", resp.StatusCode, err)
	}
	return &silod{t: t, url: srv.URL + "/silo/v1/", root: root.ID}
}

// asCaller is the transport of a caller inside the cloud: the gateway
// would know the silo from the caller's address; here a header says it.
type asCaller struct{ silo string }

func (a asCaller) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Silo-Caller", a.silo)
	return http.DefaultTransport.RoundTrip(r)
}

type result struct {
	code           int
	stdout, stderr string
}

// silo runs one command line as caller against d.
func (d *silod) silo(caller string, args ...string) result {
	d.t.Helper()
	var out, errb bytes.Buffer
	c := &cli{stdin: strings.NewReader(d.stdin), stdout: &out, stderr: &errb, transport: asCaller{caller},
		getenv: func(k string) string {
			if k == "SILO_ENDPOINT" {
				return d.url
			}
			return ""
		},
		poll: 10 * time.Millisecond}
	code := c.run(context.Background(), args)
	return result{code, out.String(), errb.String()}
}

// ok runs a command that must succeed.
func (d *silod) ok(caller string, args ...string) string {
	d.t.Helper()
	r := d.silo(caller, args...)
	if r.code != 0 {
		d.t.Fatalf("silo %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.code, r.stdout, r.stderr)
	}
	return r.stdout
}

// json runs a command with --output json that must succeed, and
// decodes its output into v.
func (d *silod) json(caller string, v any, args ...string) {
	d.t.Helper()
	out := d.ok(caller, append(args, "--output", "json")...)
	if err := json.Unmarshal([]byte(out), v); err != nil {
		d.t.Fatalf("silo %s: output is not JSON: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func contains(t *testing.T, what, s string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("%s: no %q in\n%s", what, w, s)
		}
	}
}

func TestDescribe(t *testing.T) {
	d := startSilod(t)
	out := d.ok(d.root, "describe")
	contains(t, "describe self", out, "Path:", "/", d.root, "us-central1", "vcpu", "Control:", "own full, effective full")

	var s client.Silo
	d.json(d.root, &s, "describe", "/")
	if s.ID != d.root || s.Path != "/" || s.Parent != nil {
		t.Errorf("describe / --output json: %+v", s)
	}
	if got := s.Quota["us-central1"]["vcpu"].Granted; got != "100" {
		t.Errorf("root vcpu granted %q, want 100", got)
	}
	d.json(d.root, &s, "describe", d.root)
	if s.ID != d.root {
		t.Errorf("describe by id: %+v", s)
	}
}

func TestCreatePatchAndDescribe(t *testing.T) {
	d := startSilod(t)
	var team client.Silo
	d.json(d.root, &team, "create", "/team-a", "--address", "/16", "--regions", "us-central1",
		"--quota", "us-central1:vcpu=32,memory=128GiB,spot-vcpu=64,global:agent_sessions=2",
		"--egress-allow", "proxy.golang.org,*.debian.org", "--grant-policy", "exports=allow,links=deny",
		"--labels", "team=a", "--max-blocks", "4", "--model-budget", "50", "--request-id", "11111111-1111-1111-1111-111111111111")
	if team.Path != "/team-a" || len(team.Address.Blocks) != 1 || !strings.HasSuffix(team.Address.Blocks[0], "/16") {
		t.Fatalf("create /team-a: %+v", team)
	}
	if q := team.Quota["us-central1"]; q["vcpu"].Granted != "32" || q["memory_mb"].Granted != "131072" {
		t.Errorf("create quota: %+v", team.Quota)
	}
	if team.Quota["global"]["agent_sessions"].Granted != "2" || team.ModelBudget.GrantedUSD != "50.00" {
		t.Errorf("create global quota and budget: %+v %+v", team.Quota, team.ModelBudget)
	}
	if team.Address.MaxBlocks != 4 || team.Labels["team"] != "a" || team.GrantPolicy.Own["links"] != "deny" {
		t.Errorf("create max_blocks, labels, grant policy: %+v", team)
	}
	if strings.Join(team.Egress.Own.Allow, ",") != "proxy.golang.org,*.debian.org" {
		t.Errorf("create egress: %+v", team.Egress)
	}

	// The same request id returns the first call's result.
	var again client.Silo
	d.json(d.root, &again, "create", "/team-a", "--address", "/16", "--request-id", "11111111-1111-1111-1111-111111111111")
	if again.ID != team.ID {
		t.Errorf("replayed create: id %s, want %s", again.ID, team.ID)
	}

	// validate_only creates nothing.
	out := d.ok(d.root, "create", "/team-b", "--address", "/16", "--validate-only")
	contains(t, "create --validate-only", out, "validate only", "/team-b")
	if r := d.silo(d.root, "describe", "/team-b"); r.code != 1 || !strings.Contains(r.stderr, "NOT_FOUND") {
		t.Errorf("describe after validate-only create: %+v", r)
	}

	// The team makes its CI child, as itself.
	out = d.ok(team.ID, "create", "/team-a/ci", "--address", "/20", "--quota", "us-central1:vcpu=16,spot-vcpu=64", "--control", "read")
	contains(t, "create /team-a/ci", out, "created /team-a/ci", "own read, effective read", "spot_vcpu")

	out = d.ok(team.ID, "children")
	contains(t, "children", out, "PATH", "/team-a/ci", "ACTIVE", "read")
	var kids struct{ Silos []client.Silo }
	d.json(d.root, &kids, "children", "/team-a")
	if len(kids.Silos) != 1 || kids.Silos[0].Path != "/team-a/ci" {
		t.Errorf("children --output json: %+v", kids)
	}

	out = d.ok(d.root, "describe", "/team-a", "--subtree")
	contains(t, "describe --subtree", out, "Subtree:", "/team-a", "  /team-a/ci")
	var tree client.Silo
	d.json(d.root, &tree, "describe", "--subtree", "/team-a")
	if len(tree.Children) != 1 || tree.Children[0].Path != "/team-a/ci" {
		t.Errorf("describe --subtree --output json: %+v", tree.Children)
	}

	// patch sends only what is given.
	var ci client.Silo
	d.json(team.ID, &ci, "patch", "/team-a/ci", "--quota", "us-central1:vcpu=8", "--labels", "job=412")
	if ci.Quota["us-central1"]["vcpu"].Granted != "8" || ci.Quota["us-central1"]["spot_vcpu"].Granted != "64" || ci.Labels["job"] != "412" {
		t.Errorf("patch: %+v %+v", ci.Quota, ci.Labels)
	}
	if ci.Control.Own != "read" {
		t.Errorf("patch changed control: %+v", ci.Control)
	}
	out = d.ok(d.root, "patch", "/team-a/ci", "--control", "none", "--validate-only")
	contains(t, "patch --validate-only", out, "validate only", "own none")
	d.json(d.root, &ci, "describe", "/team-a/ci")
	if ci.Control.Own != "read" {
		t.Errorf("validate-only patch changed control: %+v", ci.Control)
	}
	d.json(d.root, &ci, "patch", "/team-a/ci", "--control", "inherit")
	if ci.Control.Own != "full" {
		t.Errorf("patch --control inherit: %+v", ci.Control)
	}

	// Isolation and ingress, on create and patch.
	out = d.ok(d.root, "create", "/team-a/vm", "--address", "/24", "--backing", "subnet",
		"--isolation", "vm,sandbox", "--isolation-default", "sandbox", "--ingress", "none")
	contains(t, "create --isolation", out, "Isolation:", "vm,sandbox, default sandbox", "Ingress:", "none (own: none)")
	var vm client.Silo
	d.json(d.root, &vm, "patch", "/team-a/vm", "--isolation", "vm")
	if strings.Join(vm.Isolation.Own.Allowed, ",") != "vm" || vm.Isolation.Own.Default != "vm" || vm.Ingress.Effective != "none" {
		t.Errorf("patch --isolation vm: %+v %+v", vm.Isolation, vm.Ingress)
	}
	r := d.silo(d.root, "create", "/team-a/vm/x", "--address", "/26", "--isolation", "sandbox")
	contains(t, "looser isolation", r.stderr, "LOOSER_THAN_PARENT")
	r = d.silo(d.root, "patch", "/team-a/vm", "--backing", "local")
	contains(t, "patch --backing", r.stderr, "--backing can't be patched")
}

func TestErrors(t *testing.T) {
	d := startSilod(t)
	d.ok(d.root, "create", "/team-a", "--address", "/16", "--quota", "us-central1:vcpu=32")

	// A refusal: the code, the message, and the details, one a line.
	r := d.silo(d.root, "patch", "/team-a", "--quota", "us-central1:vcpu=1000")
	if r.code != 1 {
		t.Fatalf("over-quota patch: %+v", r)
	}
	contains(t, "QUOTA_EXCEEDED", r.stderr, "silo: QUOTA_EXCEEDED: ", "  class: vcpu", "  region: us-central1")

	// With --output json, the API's own error object.
	r = d.silo(d.root, "patch", "/team-a", "--quota", "us-central1:vcpu=1000", "-o", "json")
	var env struct{ Error client.Error }
	if err := json.Unmarshal([]byte(r.stderr), &env); err != nil || env.Error.Code != "QUOTA_EXCEEDED" || env.Error.Details["class"] != "vcpu" {
		t.Errorf("JSON error: %v %q", err, r.stderr)
	}

	r = d.silo(d.root, "create", "/team-a", "--address", "/16")
	contains(t, "ALREADY_EXISTS", r.stderr, "ALREADY_EXISTS")
	r = d.silo(d.root, "create", "/team-b", "--address", "/16", "--control", "everything")
	contains(t, "bad control", r.stderr, "INVALID_ARGUMENT")
	// A silo outside the caller's subtree is not found, as one that
	// doesn't exist.
	var team client.Silo
	d.json(d.root, &team, "describe", "/team-a")
	r = d.silo(team.ID, "describe", "/")
	if r.code != 1 || !strings.Contains(r.stderr, "NOT_FOUND") {
		t.Errorf("describe / as /team-a: %+v", r)
	}

	// Command-line mistakes exit 2 and call nothing.
	for _, args := range [][]string{
		{"create", "/team-c"},
		{"create", "/team-c", "--address", "ten"},
		{"create", "/team-c", "--address", "/20", "--quota", "vcpu=3"},
		{"patch", "/team-a"},
		{"grow", "/team-a"},
		{"describe", "/a", "/b"},
		{"describe", "-o", "yaml"},
		{"block"},
		{"frobnicate"},
	} {
		if r := d.silo(d.root, args...); r.code != 2 {
			t.Errorf("silo %s: exit %d, want 2\n%s", strings.Join(args, " "), r.code, r.stderr)
		}
	}
	if r := d.silo(d.root, "describe", "-h"); r.code != 0 || !strings.Contains(r.stderr, "-subtree") {
		t.Errorf("describe -h: %+v", r)
	}
}

func TestAddressActions(t *testing.T) {
	d := startSilod(t)
	var team client.Silo
	d.json(d.root, &team, "create", "/team-a", "--address", "10.1.0.0/16", "--max-blocks", "4")
	var ci client.Silo
	d.json(team.ID, &ci, "create", "/team-a/ci", "--address", "10.1.16.0/20")

	out := d.ok(team.ID, "grow", "/team-a/ci", "--to", "/19", "--validate-only")
	contains(t, "grow --validate-only", out, "validate only", "10.1.0.0/19")
	d.json(team.ID, &ci, "grow", "/team-a/ci", "--to", "19")
	if strings.Join(ci.Address.Blocks, ",") != "10.1.0.0/19" {
		t.Errorf("grow: %v", ci.Address.Blocks)
	}

	d.json(d.root, &team, "block", "add", "/team-a", "--address", "/20")
	if len(team.Address.Blocks) != 2 {
		t.Fatalf("block add: %v", team.Address.Blocks)
	}
	added := team.Address.Blocks[1]
	if added == "10.1.0.0/16" {
		added = team.Address.Blocks[0]
	}
	out = d.ok(team.ID, "block", "release", "/team-a", added)
	contains(t, "block release", out, "released "+added)
	d.json(d.root, &team, "describe", "/team-a")
	if strings.Join(team.Address.Blocks, ",") != "10.1.0.0/16" {
		t.Errorf("after release: %v", team.Address.Blocks)
	}
	r := d.silo(team.ID, "block", "release", "/team-a", "10.1.0.0/16")
	contains(t, "releasing a used block", r.stderr, "FAILED_PRECONDITION")
}

func TestQuotaPreview(t *testing.T) {
	d := startSilod(t)
	var team client.Silo
	d.json(d.root, &team, "create", "/team-a", "--address", "/16", "--quota", "us-central1:vcpu=32")
	d.ok(team.ID, "create", "/team-a/ci", "--address", "/20")

	out := d.ok(team.ID, "quota", "preview", "/team-a/ci", "--quota", "us-central1:vcpu=4")
	contains(t, "quota preview", out, "Fits: yes", "PAYER", "/team-a", "vcpu", "4", "Free by level:")
	var p client.PreviewResponse
	d.json(team.ID, &p, "quota", "preview", "/team-a/ci", "--quota", "us-central1:vcpu=64")
	if p.Fits || p.Refusal == nil || p.Refusal.Code != "QUOTA_EXCEEDED" {
		t.Errorf("over-quota preview: %+v", p)
	}
	if r := d.silo(team.ID, "quota", "preview", "/team-a/ci"); r.code != 2 {
		t.Errorf("preview with nothing to price: %+v", r)
	}
}

func TestMembers(t *testing.T) {
	d := startSilod(t)
	var team client.Silo
	d.json(d.root, &team, "create", "/team-a", "--address", "/16")

	out := d.ok(d.root, "members", "put", "/team-a", "user:bob@example.com", "--validate-only")
	contains(t, "members put --validate-only", out, "validate only")
	out = d.ok(d.root, "members", "get", "/team-a")
	contains(t, "members after validate-only", out, "no members")

	out = d.ok(d.root, "members", "put", "/team-a", "user:bob@example.com")
	contains(t, "members put", out, "mapped user:bob@example.com to /team-a")
	out = d.ok(d.root, "members", "get", "/team-a")
	contains(t, "members get", out, "IDENTITY", "user:bob@example.com", "/team-a")
	var m client.Member
	d.json(d.root, &m, "members", "get", "/team-a", "user:bob@example.com")
	if m.Identity != "user:bob@example.com" || m.Silo != team.ID {
		t.Errorf("members get one: %+v", m)
	}
	// Only an ancestor maps members.
	r := d.silo(team.ID, "members", "put", "/team-a", "user:eve@example.com")
	contains(t, "members put as itself", r.stderr, "PERMISSION_DENIED")

	out = d.ok(d.root, "members", "delete", "/team-a", "user:bob@example.com")
	contains(t, "members delete", out, "removed user:bob@example.com")
	r = d.silo(d.root, "members", "get", "/team-a", "user:bob@example.com")
	contains(t, "members get after delete", r.stderr, "NOT_FOUND")
}

func TestAudit(t *testing.T) {
	d := startSilod(t)
	var team client.Silo
	d.json(d.root, &team, "create", "/team-a", "--address", "/16", "--request-id", "22222222-2222-2222-2222-222222222222")
	d.ok(team.ID, "create", "/team-a/ci", "--address", "/20")
	d.ok(d.root, "members", "put", "/team-a", "user:bob@example.com")

	out := d.ok(d.root, "audit", "/team-a")
	contains(t, "audit", out, "TIME", "silo.create", "member.put", "22222222-2222-2222-2222-222222222222")
	var recs struct{ Records []client.AuditRecord }
	d.json(d.root, &recs, "audit", "/team-a", "--subtree", "--kind", "silo.*")
	paths := map[string]bool{}
	for _, r := range recs.Records {
		if !strings.HasPrefix(r.Kind, "silo.") {
			t.Errorf("--kind silo.*: got %s", r.Kind)
		}
		paths[r.Silo.Path] = true
	}
	if !paths["/team-a"] || !paths["/team-a/ci"] {
		t.Errorf("audit --subtree: %v", paths)
	}
	d.json(d.root, &recs, "audit", "/team-a", "--since", "2099-01-01")
	if len(recs.Records) != 0 {
		t.Errorf("audit --since the future: %+v", recs.Records)
	}
	d.json(d.root, &recs, "audit", "/team-a", "--since", "1h", "--limit", "1")
	if len(recs.Records) != 1 {
		t.Errorf("audit --since 1h --limit 1: %d records", len(recs.Records))
	}
	if r := d.silo(d.root, "audit", "--since", "yesterday"); r.code != 2 {
		t.Errorf("audit --since yesterday: %+v", r)
	}
}

func TestDelete(t *testing.T) {
	d := startSilod(t)
	d.ok(d.root, "create", "/team-a", "--address", "/16")
	d.ok(d.root, "create", "/team-a/ci", "--address", "/20")

	r := d.silo(d.root, "delete", "/team-a")
	contains(t, "delete without force", r.stderr, "FAILED_PRECONDITION", "has children")
	out := d.ok(d.root, "delete", "/team-a", "--force", "--validate-only")
	contains(t, "delete --validate-only", out, "would delete", "/team-a", "/team-a/ci")

	r = d.silo(d.root, "delete", "/team-a", "--force")
	if r.code != 0 {
		t.Fatalf("delete --force: %+v", r)
	}
	contains(t, "delete --force", r.stdout, "deleted /team-a")
	contains(t, "delete --force progress", r.stderr, "deleting /team-a")
	if r := d.silo(d.root, "describe", "/team-a"); r.code != 1 {
		t.Errorf("describe after delete: %+v", r)
	}

	d.ok(d.root, "create", "/team-b", "--address", "/16")
	var op client.Operation
	d.json(d.root, &op, "delete", "/team-b")
	if !op.Done || op.State != client.OpDone || op.Target != "/team-b" {
		t.Errorf("delete --output json: %+v", op)
	}
}

// A sealed silo's calls to the API port time out, while its egress
// proxy answers: silo names the cause.
func TestSealedHint(t *testing.T) {
	stop := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-stop:
		case <-r.Context().Done():
		}
	}))
	defer hang.Close()
	defer close(stop)
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	go func() {
		for {
			c, err := proxy.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	run := func(proxyAddr string) result {
		var out, errb bytes.Buffer
		c := &cli{stdout: &out, stderr: &errb, getenv: func(string) string { return "" }, proxyAddr: proxyAddr}
		code := c.run(context.Background(), []string{"describe", "--endpoint", hang.URL + "/silo/v1/", "--timeout", "200ms"})
		return result{code, out.String(), errb.String()}
	}
	r := run(proxy.Addr().String())
	if r.code != 1 {
		t.Fatalf("exit %d", r.code)
	}
	contains(t, "sealed", r.stderr, "probably sealed", proxy.Addr().String())

	// With the proxy down too, it doesn't blame the seal.
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := dead.Addr().String()
	dead.Close()
	r = run(addr)
	if strings.Contains(r.stderr, "sealed") || !strings.Contains(r.stderr, "did not answer") {
		t.Errorf("proxy down: %s", r.stderr)
	}
}

func TestEndpoint(t *testing.T) {
	c := &cli{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, getenv: func(string) string { return "" }}
	fs := c.flags("describe")
	if _, err := c.parse(fs, nil, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
	if got := c.api.Endpoint().String(); got != client.DefaultEndpoint {
		t.Errorf("default endpoint %s", got)
	}
	c.getenv = func(k string) string { return map[string]string{"SILO_ENDPOINT": "http://localhost:8642/silo/v1"}[k] }
	fs = c.flags("describe")
	if _, err := c.parse(fs, nil, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
	if got := c.api.Endpoint().String(); got != "http://localhost:8642/silo/v1/" {
		t.Errorf("$SILO_ENDPOINT: %s", got)
	}
	fs = c.flags("describe")
	if _, err := c.parse(fs, []string{"--endpoint", "http://x:1/silo/v1/"}, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
	if got := c.api.Endpoint().Host; got != "x:1" {
		t.Errorf("--endpoint: %s", got)
	}
}

func TestParseQuota(t *testing.T) {
	q, err := parseQuota("us-central1:vcpu=32,memory=128GiB,ssd-gb=1000,agent-sessions=2,europe-west4:spot-vcpu=1.5")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]string{
		"us-central1":  {"vcpu": "32", "memory_mb": "131072", "ssd_disk_gb": "1000"},
		"global":       {"agent_sessions": "2"},
		"europe-west4": {"spot_vcpu": "1.5"},
	}
	got := map[string]map[string]string{}
	for r, m := range q {
		got[r] = map[string]string{}
		for k, v := range m {
			got[r][k] = string(v)
		}
	}
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Errorf("parseQuota: %s, want %s", gb, wb)
	}
	for _, bad := range []string{"vcpu=3", "us-central1:vcpu", "us-central1:vcpu=-1", "us-central1:memory=3furlongs", ""} {
		if _, err := parseQuota(bad); err == nil {
			t.Errorf("parseQuota(%q): no error", bad)
		}
	}
}

func TestParseAddressAndSince(t *testing.T) {
	for in, want := range map[string]client.AddressWant{
		"/20": {PrefixLength: 20}, "22": {PrefixLength: 22}, "10.1.16.0/20": {Range: "10.1.16.0/20"},
	} {
		got, err := parseAddress(in)
		if err != nil || *got != want {
			t.Errorf("parseAddress(%q) = %+v, %v", in, got, err)
		}
	}
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"2026-10-01T00:00:00Z": time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		"2026-10-01":           time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		"90m":                  now.Add(-90 * time.Minute),
		"7d":                   now.Add(-7 * 24 * time.Hour),
	} {
		if got, err := parseSince(in, now); err != nil || !got.Equal(want) {
			t.Errorf("parseSince(%q) = %v, %v", in, got, err)
		}
	}
}

func TestExportsAndImports(t *testing.T) {
	d := startSilod(t)
	var a, b client.Silo
	d.json(d.root, &a, "create", "/team-a", "--address", "/16", "--grant-policy", "exports=allow")
	d.json(d.root, &b, "create", "/team-b", "--address", "/16", "--grant-policy", "exports=allow")
	pfx, err := netip.ParsePrefix(a.Address.Blocks[0])
	if err != nil {
		t.Fatal(err)
	}
	target := netip.AddrPortFrom(pfx.Addr().Next().Next(), 8080).String()

	out := d.ok(a.ID, "export", "offer", "pkgcache", "--target", target, "--to", "/team-b")
	contains(t, "export offer", out, "offered pkgcache (http) to /team-b", "PENDING")
	out = d.ok(b.ID, "import", "list")
	contains(t, "import list before accept", out, "/team-a/pkgcache", "PENDING")
	out = d.ok(b.ID, "import", "accept", "/team-a/pkgcache")
	contains(t, "import accept", out, "accepted /team-a/pkgcache (ACTIVE): http://silo.internal/x/"+a.ID+"/pkgcache/")
	out = d.ok(a.ID, "export", "list")
	contains(t, "export list", out, "pkgcache", "/team-b", "ACTIVE")
	var ims struct {
		Imports []client.Import `json:"imports"`
	}
	d.json(b.ID, &ims, "import", "list")
	if len(ims.Imports) != 1 {
		t.Fatalf("imports: %+v", ims)
	}
	// On b's behalf, by the root.
	out = d.ok(d.root, "import", "delete", ims.Imports[0].ID, "--silo", "/team-b")
	contains(t, "import delete", out, "deleted the import of /team-a/pkgcache")
	out = d.ok(a.ID, "export", "withdraw", "pkgcache", "--to", "/team-b")
	contains(t, "export withdraw --to", out, "withdrew pkgcache from /team-b")
	out = d.ok(a.ID, "export", "withdraw", "pkgcache")
	contains(t, "export withdraw", out, "withdrew pkgcache")
	r := d.silo(b.ID, "import", "accept", "/team-a/pkgcache")
	contains(t, "accept after withdrawal", r.stderr, "NOT_FOUND")
	r = d.silo(a.ID, "export", "offer", "x", "--to", "/team-b")
	if r.code != 2 {
		t.Errorf("offer without a target: exit %d %s", r.code, r.stderr)
	}
}

func TestLinks(t *testing.T) {
	d := startSilod(t)
	var a, b client.Silo
	d.json(d.root, &a, "create", "/team-a", "--address", "/16", "--grant-policy", "exports=allow,links=allow")
	d.json(d.root, &b, "create", "/team-b", "--address", "/16", "--grant-policy", "exports=allow,links=allow")

	out := d.ok(a.ID, "link", "propose", "db", "--peer", "/team-b", "--direction", "outbound", "--rules", "tcp:5432")
	contains(t, "link propose", out, "proposed db to /team-b", "PENDING", "--direction inbound")
	// On b's behalf, by the root: the mirror image.
	out = d.ok(d.root, "link", "propose", "db", "--silo", "/team-b", "--peer", "/team-a", "--direction", "inbound", "--rules", "tcp:5432")
	contains(t, "link propose, mirrored", out, "linked db with /team-a: ACTIVE")
	out = d.ok(a.ID, "link", "list")
	contains(t, "link list", out, "db", "/team-b", "outbound", "tcp:5432", "ACTIVE")
	var ls struct {
		Links []client.Link `json:"links"`
	}
	d.json(b.ID, &ls, "link", "list")
	if len(ls.Links) != 1 || ls.Links[0].State != "ACTIVE" || len(ls.Links[0].FenceRules) != 2 {
		t.Fatalf("b's links: %+v", ls)
	}
	out = d.ok(b.ID, "link", "delete", "db")
	contains(t, "link delete", out, "deleted the link db with /team-a")
	out = d.ok(a.ID, "link", "list")
	contains(t, "link list after delete", out, "PENDING")
	r := d.silo(a.ID, "link", "propose", "x", "--peer", "/team-b")
	if r.code != 2 {
		t.Errorf("propose without rules: exit %d %s", r.code, r.stderr)
	}
}
