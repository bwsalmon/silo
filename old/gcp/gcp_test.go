package gcp_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/old/gcp"
	"github.com/bwsalmon/silo/old/internal/fakegcp"
)

const (
	proj = "acme-staging"
	user = "alice@example.com"
)

var roles = []string{"roles/logging.viewer", "roles/storage.objectAdmin"}

// env is a fake project the user may set up accounts in, and a client
// whose clock only moves when it sleeps.
type env struct {
	f   *fakegcp.Fake
	c   *gcp.Client
	out *bytes.Buffer

	mu  sync.Mutex
	now time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	var setup []string
	for p := range gcp.SetupPermissions {
		setup = append(setup, p)
	}
	f := fakegcp.New(proj, setup...)
	f.AddRole("roles/logging.viewer", "logging.logEntries.list", "logging.logs.list")
	f.AddRole("roles/storage.objectAdmin", "storage.objects.create", "storage.objects.delete", "storage.objects.get", "storage.objects.list")
	for _, r := range f.Roles {
		for _, p := range r.Permissions {
			f.Held[p] = true
		}
	}
	e := &env{f: f, out: &bytes.Buffer{}, now: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	e.c = &gcp.Client{
		API:   f,
		Store: &gcp.Store{Path: filepath.Join(t.TempDir(), "silo", "accounts.json")},
		Out:   e.out,
		Now: func() time.Time {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.now
		},
		Sleep: func(ctx context.Context, d time.Duration) error {
			e.advance(d)
			return ctx.Err()
		},
	}
	return e
}

func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	e.now = e.now.Add(d)
	e.mu.Unlock()
}

func (e *env) spec() gcp.Spec { return gcp.NewSpec(proj, user, roles) }

func (e *env) ensure(t *testing.T, ttl time.Duration) (gcp.Record, error) {
	t.Helper()
	return e.c.Ensure(context.Background(), gcp.EnsureInput{Spec: e.spec(), TTL: ttl})
}

func mustContain(t *testing.T, err error, subs ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("got no error, want one containing %q", subs)
	}
	for _, s := range subs {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q does not contain %q", err, s)
		}
	}
}

// The golden table was computed independently (Python's json.dumps with
// sort_keys and compact separators, hashlib and base64.b32encode).
func TestNameGolden(t *testing.T) {
	for _, tc := range []struct {
		project, user string
		roles         []string
		canonical     string
		id, hash      string
	}{
		{"acme-staging", "alice@example.com",
			[]string{"roles/logging.viewer", "roles/monitoring.viewer", "roles/storage.objectAdmin", "roles/bigquery.dataViewer", "roles/bigquery.jobUser"},
			`{"conditions":[],"project":"acme-staging","roles":["roles/bigquery.dataViewer","roles/bigquery.jobUser","roles/logging.viewer","roles/monitoring.viewer","roles/storage.objectAdmin"],"user":"alice@example.com","v":1}`,
			"silo-opdydt6leiktag4cra7b", "73c781cfcb2215301b82883e183c6283b915181e637f51eabdcdc9eec7125684"},
		{"p", "a@b.c", []string{"roles/viewer"},
			`{"conditions":[],"project":"p","roles":["roles/viewer"],"user":"a@b.c","v":1}`,
			"silo-lykgxctvykencclkytfe", "5e146b8a75c288d1096ac4ca4b88b3723aa2d50219d6f9f49b3c9da0f8deae21"},
		// Case and duplicates do not change the name.
		{"p", "A@B.C", []string{"roles/viewer", "roles/viewer"},
			`{"conditions":[],"project":"p","roles":["roles/viewer"],"user":"a@b.c","v":1}`,
			"silo-lykgxctvykencclkytfe", "5e146b8a75c288d1096ac4ca4b88b3723aa2d50219d6f9f49b3c9da0f8deae21"},
		{"projects/x", "u@x.com", []string{"roles/a", "projects/p/roles/custom"},
			`{"conditions":[],"project":"projects/x","roles":["projects/p/roles/custom","roles/a"],"user":"u@x.com","v":1}`,
			"silo-qgbu3kjelu4qrph6m65m", "81834da9245d3908bcfe67bac8fd0497c359fd957ba6849a81d2bdbee0263ce8"},
	} {
		s := gcp.NewSpec(tc.project, tc.user, tc.roles)
		if got := string(s.Canonical()); got != tc.canonical {
			t.Errorf("canonical:\n got %s\nwant %s", got, tc.canonical)
		}
		if got := s.AccountID(); got != tc.id || len(got) != 25 {
			t.Errorf("id %s, want %s", got, tc.id)
		}
		if got := s.Hash(); got != tc.hash {
			t.Errorf("hash %s, want %s", got, tc.hash)
		}
	}
	if got := gcp.NewSpec("p", "a@b.c", []string{"roles/viewer"}).Email(); got != "silo-lykgxctvykencclkytfe@p.iam.gserviceaccount.com" {
		t.Errorf("email %s", got)
	}
}

func TestSpecValidate(t *testing.T) {
	s := gcp.NewSpec(proj, user, roles)
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	s.Conditions = []string{"resource.name.startsWith('x')"}
	mustContain(t, s.Validate(), "open question 8")
	mustContain(t, gcp.NewSpec(proj, user, []string{"logging.viewer"}).Validate(), "not a role name")
	mustContain(t, gcp.NewSpec(proj, "alice", roles).Validate(), "not an email")
	mustContain(t, gcp.NewSpec(proj, user, nil).Validate(), "no roles")
}

func TestDescription(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	d := gcp.Description{SpecHash: strings.Repeat("a", 64), User: user, Created: ts, Used: ts.Add(time.Hour)}
	want := "silo/v2 spec=" + strings.Repeat("a", 64) + " user=alice@example.com created=2026-01-02T03:04:05Z used=2026-01-02T04:04:05Z"
	if d.String() != want {
		t.Fatalf("got %q", d.String())
	}
	got, ok := gcp.ParseDescription(want)
	if !ok || !reflect.DeepEqual(got, d) {
		t.Fatalf("parse: %+v %v", got, ok)
	}
	for _, s := range []string{"", "made by hand", "silo/v2 spec=x user=y", "silo/v2 spec=x user=y created=bad used=2026-01-02T04:04:05Z"} {
		if _, ok := gcp.ParseDescription(s); ok {
			t.Errorf("%q parsed", s)
		}
	}
	if (gcp.Description{SpecHash: strings.Repeat("a", 64), User: strings.Repeat("u", 200) + "@x.com"}).Fits() {
		t.Error("a 300-character description fits")
	}
	if got := gcp.NewSpec(proj, strings.Repeat("u", 120)+"@x.com", roles).DisplayName(); len(got) != 100 {
		t.Errorf("display name is %d characters", len(got))
	}
}

func TestCheckPasses(t *testing.T) {
	e := newEnv(t)
	rep, err := e.c.Check(context.Background(), gcp.CheckInput{Project: proj, User: user, Roles: roles})
	if err != nil {
		t.Fatal(err)
	}
	if err := rep.Err(); err != nil {
		t.Fatal(err)
	}
	if len(rep.Warnings) != 0 || rep.SiloAccounts != 0 || rep.Exists {
		t.Fatalf("report %+v", rep)
	}
	if got := rep.Samples()["roles/storage.objectAdmin"]; len(got) != 3 {
		t.Fatalf("sample %v", got)
	}
}

func TestCheckRefusals(t *testing.T) {
	ctx := context.Background()

	e := newEnv(t)
	delete(e.f.Held, "resourcemanager.projects.setIamPolicy")
	delete(e.f.Held, "iam.serviceAccounts.create")
	rep, err := e.c.Check(ctx, gcp.CheckInput{Project: proj, User: user, Roles: roles})
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, rep.Err(), "iam.serviceAccounts.create, resourcemanager.projects.setIamPolicy",
		"roles/iam.serviceAccountAdmin, roles/resourcemanager.projectIamAdmin grant them")

	e = newEnv(t)
	delete(e.f.Held, "storage.objects.delete")
	rep, _ = e.c.Check(ctx, gcp.CheckInput{Project: proj, User: user, Roles: roles})
	mustContain(t, rep.Err(), "roles/storage.objectAdmin: you lack 1 of its 4 permissions (storage.objects.delete)", "call 19")

	e = newEnv(t)
	delete(e.f.Grantable, "roles/logging.viewer")
	rep, _ = e.c.Check(ctx, gcp.CheckInput{Project: proj, User: user, Roles: roles})
	mustContain(t, rep.Err(), "roles/logging.viewer cannot be granted on acme-staging")

	e = newEnv(t)
	rep, _ = e.c.Check(ctx, gcp.CheckInput{Project: proj, User: user, Roles: []string{"roles/nope"}})
	mustContain(t, rep.Err(), "reading roles/nope", "not found")

	e = newEnv(t)
	e.f.AddRole("roles/viewer", "logging.logs.list")
	rep, _ = e.c.Check(ctx, gcp.CheckInput{Project: proj, User: user, Roles: []string{"roles/viewer"}})
	mustContain(t, rep.Err(), "allow_basic_roles: true")
	rep, _ = e.c.Check(ctx, gcp.CheckInput{Project: proj, User: user, Roles: []string{"roles/viewer"}, AllowBasicRoles: true})
	if err := rep.Err(); err != nil {
		t.Fatal(err)
	}

	// A failure to test at all is an error, not a report.
	e = newEnv(t)
	e.f.Fail("TestPermissions", fakegcp.Err(403, "denied"))
	if _, err := e.c.Check(ctx, gcp.CheckInput{Project: proj, User: user, Roles: roles}); err == nil {
		t.Fatal("no error")
	}
}

func TestCheckUntestableIsAWarning(t *testing.T) {
	e := newEnv(t)
	e.f.AddRole("roles/custom", "logging.logs.list", "billing.accounts.get")
	delete(e.f.Testable, "billing.accounts.get")
	rep, err := e.c.Check(context.Background(), gcp.CheckInput{Project: proj, User: user, Roles: []string{"roles/custom"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := rep.Err(); err != nil {
		t.Fatal(err)
	}
	if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "billing.accounts.get") {
		t.Fatalf("warnings %q", rep.Warnings)
	}
}

func TestCheckBatchesOf100(t *testing.T) {
	e := newEnv(t)
	var perms []string
	for i := range 250 {
		p := fmt.Sprintf("svc.things.p%03d", i)
		perms = append(perms, p)
		e.f.Held[p] = true
	}
	e.f.AddRole("projects/acme-staging/roles/big", perms...)
	delete(e.f.Held, "svc.things.p249")
	rep, err := e.c.Check(context.Background(), gcp.CheckInput{Project: proj, User: user, Roles: []string{"projects/acme-staging/roles/big"}})
	if err != nil {
		t.Fatal(err)
	}
	if n := e.f.Count("TestPermissions"); n != 1+3 {
		t.Fatalf("%d testIamPermissions calls, want 1 setup + 3 batches", n)
	}
	mustContain(t, rep.Err(), "you lack 1 of its 250 permissions (svc.things.p249)")
}

func TestQuota(t *testing.T) {
	ctx := context.Background()
	in := gcp.CheckInput{Project: proj, User: user, Roles: roles}
	e := newEnv(t)
	for i := range 51 {
		e.f.PutAccount(gcp.Account{Email: fmt.Sprintf("silo-%03d@%s.iam.gserviceaccount.com", i, proj)})
	}
	e.f.PutAccount(gcp.Account{Email: "other@" + proj + ".iam.gserviceaccount.com"})
	rep, _ := e.c.Check(ctx, in)
	if rep.SiloAccounts != 51 || rep.Err() != nil || len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "silo gc") {
		t.Fatalf("51: %+v %v", rep, rep.Err())
	}
	for i := 51; i < 91; i++ {
		e.f.PutAccount(gcp.Account{Email: fmt.Sprintf("silo-%03d@%s.iam.gserviceaccount.com", i, proj)})
	}
	rep, _ = e.c.Check(ctx, in)
	mustContain(t, rep.Err(), "91 silo accounts", "silo gc")
	// An account that already exists is reused, quota or not.
	e.f.PutAccount(gcp.Account{Email: e.spec().Email()})
	rep, _ = e.c.Check(ctx, in)
	if !rep.Exists || rep.Err() != nil {
		t.Fatalf("existing: %+v %v", rep, rep.Err())
	}
	// Exactly 90 is allowed.
	e = newEnv(t)
	for i := range 90 {
		e.f.PutAccount(gcp.Account{Email: fmt.Sprintf("silo-%03d@%s.iam.gserviceaccount.com", i, proj)})
	}
	if rep, _ = e.c.Check(ctx, in); rep.Err() != nil {
		t.Fatal(rep.Err())
	}
}

func TestEnsureCreatesThenReuses(t *testing.T) {
	e := newEnv(t)
	rec, err := e.ensure(t, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	email := e.spec().Email()
	if rec.Email != email || rec.Incomplete || rec.UniqueID == "" || time.Duration(rec.TTL) != 7*24*time.Hour {
		t.Fatalf("record %+v", rec)
	}
	if got := e.f.Binds(gcp.Member(email)); !reflect.DeepEqual(got, roles) {
		t.Fatalf("bindings %v", got)
	}
	ap := e.f.AccountPolicies[email]
	if len(ap.Bindings) != 1 || ap.Bindings[0].Role != gcp.TokenCreator || !reflect.DeepEqual(ap.Bindings[0].Members, []string{"user:" + user}) {
		t.Fatalf("account policy %+v", ap)
	}
	a := e.f.Accounts[email]
	d, ok := gcp.ParseDescription(a.Description)
	if !ok || d.SpecHash != e.spec().Hash() || d.User != user || !d.Created.Equal(e.now) || a.DisplayName != "silo: "+user {
		t.Fatalf("account %+v", a)
	}
	st, _ := e.c.Store.Load()
	if len(st.Accounts) != 1 || !reflect.DeepEqual(st.Accounts[0].Spec, e.spec()) {
		t.Fatalf("state %+v", st)
	}

	writes := e.f.Count("SetProjectPolicy") + e.f.Count("SetAccountPolicy") + e.f.Count("CreateAccount")
	if _, err := e.ensure(t, time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := e.f.Count("SetProjectPolicy") + e.f.Count("SetAccountPolicy") + e.f.Count("CreateAccount"); got != writes {
		t.Fatalf("reuse wrote: %d writes, want %d", got, writes)
	}
	// Reuse with the local state lost verifies against the project.
	if err := e.c.Store.Delete(email); err != nil {
		t.Fatal(err)
	}
	if rec, err := e.ensure(t, time.Hour); err != nil || rec.UniqueID != a.UniqueID {
		t.Fatalf("reuse without state: %+v %v", rec, err)
	}
}

func TestVerifyMismatches(t *testing.T) {
	email := gcp.NewSpec(proj, user, roles).Email()
	for _, tc := range []struct {
		name   string
		mutate func(f *fakegcp.Fake)
		want   string
	}{
		{"extra binding", func(f *fakegcp.Fake) { f.Bind("roles/owner", gcp.Member(email)) }, "it holds roles/owner on acme-staging, which the spec does not have"},
		{"missing binding", func(f *fakegcp.Fake) {
			p := f.ProjectPolicy
			for i, b := range p.Bindings {
				if b.Role == "roles/logging.viewer" {
					p.Bindings = append(p.Bindings[:i], p.Bindings[i+1:]...)
					break
				}
			}
		}, "it does not hold roles/logging.viewer on acme-staging"},
		{"conditional binding", func(f *fakegcp.Fake) {
			f.ProjectPolicy.Bindings = append(f.ProjectPolicy.Bindings, gcp.Binding{Role: "roles/editor", Members: []string{gcp.Member(email)}, Condition: &gcp.Condition{Expression: "true"}})
		}, "it holds roles/editor under a condition"},
		{"foreign token creator", func(f *fakegcp.Fake) { f.BindAccount(email, gcp.TokenCreator, "user:mallory@example.com") },
			"user:mallory@example.com holds roles/iam.serviceAccountTokenCreator on it; only user:alice@example.com may"},
		{"other role on the account", func(f *fakegcp.Fake) {
			f.BindAccount(email, "roles/iam.serviceAccountUser", "user:mallory@example.com")
		},
			"user:mallory@example.com holds roles/iam.serviceAccountUser on it"},
		{"user not token creator", func(f *fakegcp.Fake) { f.AccountPolicies[email].Bindings = nil }, "user:alice@example.com does not hold roles/iam.serviceAccountTokenCreator"},
		{"wrong spec", func(f *fakegcp.Fake) {
			f.Accounts[email].Description = strings.Replace(f.Accounts[email].Description, "spec=", "spec=0", 1)
		}, "its description has spec=0"},
		{"wrong user", func(f *fakegcp.Fake) {
			f.Accounts[email].Description = strings.Replace(f.Accounts[email].Description, "user=alice", "user=bob", 1)
		}, "its description has user=bob@example.com, want user=alice@example.com"},
		{"not silo's", func(f *fakegcp.Fake) { f.Accounts[email].Description = "hand made" }, `its description "hand made" is not one silo writes`},
		{"disabled", func(f *fakegcp.Fake) { f.Accounts[email].Disabled = true }, "it is disabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			if _, err := e.ensure(t, time.Hour); err != nil {
				t.Fatal(err)
			}
			tc.mutate(e.f)
			sets := e.f.Count("SetProjectPolicy") + e.f.Count("SetAccountPolicy")
			_, err := e.ensure(t, time.Hour)
			var mm *gcp.MismatchError
			if !errors.As(err, &mm) {
				t.Fatalf("got %v, want a MismatchError", err)
			}
			mustContain(t, err, tc.want, "silo gc --project acme-staging --account "+email)
			if got := e.f.Count("SetProjectPolicy") + e.f.Count("SetAccountPolicy"); got != sets {
				t.Fatal("silo repaired an altered account")
			}
		})
	}
}

func TestEtagConflictIsRetried(t *testing.T) {
	e := newEnv(t)
	e.f.Fail("SetProjectPolicy", fakegcp.Err(409, "concurrent policy changes"), fakegcp.Err(429, "slow down"))
	e.f.Fail("SetAccountPolicy", fakegcp.Err(503, "unavailable"))
	e.f.Fail("CreateAccount", fakegcp.Err(503, "unavailable"))
	if _, err := e.ensure(t, time.Hour); err != nil {
		t.Fatal(err)
	}
	if n := e.f.Count("SetProjectPolicy"); n != len(roles)+2 {
		t.Fatalf("%d project policy writes, want %d", n, len(roles)+2)
	}
	if got := e.f.Binds(gcp.Member(e.spec().Email())); !reflect.DeepEqual(got, roles) {
		t.Fatalf("bindings %v", got)
	}
}

func TestNoRetryOn403AndResume(t *testing.T) {
	e := newEnv(t)
	email := e.spec().Email()
	// The first binding lands; the second is refused.
	e.f.Fail("SetProjectPolicy", nil, fakegcp.Err(403, "denied"))
	_, err := e.ensure(t, time.Hour)
	mustContain(t, err, "stopped after bind:roles/logging.viewer, create", "permission denied")
	if n := e.f.Count("SetProjectPolicy"); n != 2 {
		t.Fatalf("%d writes: a 403 was retried", n)
	}
	rec, ok, _ := e.c.Store.Get(email)
	if !ok || !rec.Incomplete || !reflect.DeepEqual(rec.Done, []string{"create", "bind:roles/logging.viewer"}) {
		t.Fatalf("record %+v", rec)
	}

	// The next run finishes it without creating anything again.
	rec, err = e.ensure(t, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Incomplete || e.f.Count("CreateAccount") != 1 || e.f.Count("SetProjectPolicy") != 3 {
		t.Fatalf("resume: %+v, %d creates, %d writes", rec, e.f.Count("CreateAccount"), e.f.Count("SetProjectPolicy"))
	}
	if got := e.f.Binds(gcp.Member(email)); !reflect.DeepEqual(got, roles) {
		t.Fatalf("bindings %v", got)
	}
}

func TestResumeVerifiesWhatIsThere(t *testing.T) {
	e := newEnv(t)
	email := e.spec().Email()
	e.f.Fail("SetProjectPolicy", fakegcp.Err(403, "denied"))
	if _, err := e.ensure(t, time.Hour); err == nil {
		t.Fatal("no error")
	}
	e.f.Bind("roles/owner", gcp.Member(email))
	_, err := e.ensure(t, time.Hour)
	mustContain(t, err, "roles/owner")

	// An incomplete account replaced by another of the same name.
	e = newEnv(t)
	e.f.Fail("SetProjectPolicy", fakegcp.Err(403, "denied"))
	if _, err := e.ensure(t, time.Hour); err == nil {
		t.Fatal("no error")
	}
	a := *e.f.Accounts[email]
	a.UniqueID = "999"
	e.f.PutAccount(a)
	_, err = e.ensure(t, time.Hour)
	mustContain(t, err, "unique id is 999")
}

func TestCreateErrors(t *testing.T) {
	e := newEnv(t)
	e.f.Fail("CreateAccount", fakegcp.Err(400, "Precondition failed: constraints/iam.disableServiceAccountCreation is enforced"))
	_, err := e.ensure(t, time.Hour)
	mustContain(t, err, "forbidden by organization policy constraints/iam.disableServiceAccountCreation")
	if n := e.f.Count("CreateAccount"); n != 1 {
		t.Fatalf("%d creates", n)
	}

	// Another process created it between our get and create.
	e = newEnv(t)
	e.f.Fail("CreateAccount", fakegcp.Err(409, "exists"))
	if _, err := e.ensure(t, time.Hour); err == nil {
		t.Fatal("no error for a 409 on an account that is not there")
	}
}

func TestWaitReady(t *testing.T) {
	e := newEnv(t)
	rec, err := e.ensure(t, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	e.f.TokenDenials, e.f.AsDenials = 3, 2
	samples := map[string][]string{"roles/logging.viewer": {"logging.logs.list"}, "roles/storage.objectAdmin": {"storage.objects.get"}}
	tok, err := e.c.WaitReady(context.Background(), proj, rec.Email, samples, 0)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Value != "token-for:"+rec.Email || e.f.Count("GenerateAccessToken") != 4 {
		t.Fatalf("token %+v after %d mints", tok, e.f.Count("GenerateAccessToken"))
	}
	if !strings.Contains(e.out.String(), "✓ roles/logging.viewer") || !strings.Contains(e.out.String(), "✓ roles/storage.objectAdmin") {
		t.Fatalf("progress %q", e.out.String())
	}

	// Never granted: exit 69 after ten minutes.
	e.f.TokenDenials = 1 << 30
	start := e.now
	_, err = e.c.WaitReady(context.Background(), proj, rec.Email, samples, 0)
	if !errors.Is(err, gcp.ErrPropagation) || e.now.Sub(start) < gcp.PropagationTimeout || e.now.Sub(start) > gcp.PropagationTimeout+time.Minute {
		t.Fatalf("got %v after %s", err, e.now.Sub(start))
	}
	if gcp.ExitPropagation != 69 {
		t.Fatal("exit code")
	}

	// A role that never takes effect.
	e.f.TokenDenials = 0
	samples["roles/never"] = []string{"compute.instances.list"}
	_, err = e.c.WaitReady(context.Background(), proj, rec.Email, samples, time.Minute)
	if !errors.Is(err, gcp.ErrPropagation) {
		t.Fatal(err)
	}
	mustContain(t, err, "roles/never")

	// Other errors are not waited out.
	e.f.Fail("GenerateAccessToken", fakegcp.Err(400, "bad request"))
	if _, err := e.c.WaitReady(context.Background(), proj, rec.Email, samples, 0); err == nil || errors.Is(err, gcp.ErrPropagation) {
		t.Fatalf("got %v", err)
	}
}

func TestSetup(t *testing.T) {
	e := newEnv(t)
	e.f.TokenDenials = 1
	rec, tok, err := e.c.Setup(context.Background(), gcp.SetupInput{
		CheckInput: gcp.CheckInput{Project: proj, User: user, Roles: roles}, TTL: time.Hour,
	})
	if err != nil || tok.Value == "" || rec.Incomplete {
		t.Fatalf("%+v %+v %v", rec, tok, err)
	}
	e = newEnv(t)
	delete(e.f.Held, "iam.serviceAccounts.create")
	_, _, err = e.c.Setup(context.Background(), gcp.SetupInput{CheckInput: gcp.CheckInput{Project: proj, User: user, Roles: roles}})
	var ref *gcp.RefusedError
	if !errors.As(err, &ref) || e.f.Count("CreateAccount") != 0 {
		t.Fatalf("got %v", err)
	}
}

func TestGC(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	day := 24 * time.Hour

	mk := func(rs []string, ttl time.Duration, session *gcp.SessionRef) string {
		t.Helper()
		rec, err := e.c.Ensure(ctx, gcp.EnsureInput{Spec: gcp.NewSpec(proj, user, rs), TTL: ttl, Session: session})
		if err != nil {
			t.Fatal(err)
		}
		return rec.Email
	}
	old := mk([]string{"roles/logging.viewer"}, 7*day, nil)
	zero := mk([]string{"roles/storage.objectAdmin"}, 0, nil)
	zeroLive := mk(roles, 0, &gcp.SessionRef{ID: "s1", PID: 1})
	e.advance(8 * day)
	e.f.AddRole("roles/custom", "logging.logs.list")
	fresh := mk([]string{"roles/custom"}, 30*day, nil)

	// Another user's account, old, in the same project.
	bob := gcp.NewSpec(proj, "bob@example.com", roles)
	e.f.PutAccount(gcp.Account{Email: bob.Email(), Description: gcp.Description{SpecHash: bob.Hash(), User: bob.User}.String()})
	e.f.Bind("roles/logging.viewer", gcp.Member(bob.Email()))
	// A silo account of this user's with no local record, 10 days unused.
	lost := gcp.NewSpec(proj, user, []string{"roles/monitoring.viewer"})
	e.f.PutAccount(gcp.Account{Email: lost.Email(), Description: gcp.Description{SpecHash: lost.Hash(), User: user, Created: e.now.Add(-10 * day), Used: e.now.Add(-10 * day)}.String()})
	e.f.Bind("roles/monitoring.viewer", gcp.Member(lost.Email()))

	rep, err := e.c.GC(ctx, gcp.GCOptions{User: user, Alive: func(s gcp.SessionRef) bool { return s.ID == "s1" }})
	if err != nil {
		t.Fatal(err)
	}
	deleted := map[string]bool{}
	for _, d := range rep.Deleted {
		deleted[d] = true
	}
	if !deleted[old] || !deleted[zero] || !deleted[lost.Email()] || deleted[zeroLive] || deleted[fresh] || deleted[bob.Email()] || len(rep.Deleted) != 3 {
		t.Fatalf("deleted %v, kept %v", rep.Deleted, rep.Kept)
	}
	for _, d := range rep.Deleted {
		if _, ok := e.f.Accounts[d]; ok {
			t.Errorf("%s still exists", d)
		}
		if b := e.f.Binds(gcp.Member(d)); len(b) != 0 {
			t.Errorf("%s still bound to %v", d, b)
		}
		if _, ok, _ := e.c.Store.Get(d); ok {
			t.Errorf("%s still in local state", d)
		}
	}
	if b := e.f.Binds(gcp.Member(bob.Email())); len(b) != 1 {
		t.Fatalf("bob's bindings were touched: %v", b)
	}
	if _, ok := e.f.Accounts[bob.Email()]; !ok {
		t.Fatal("bob's account was deleted")
	}

	// --account: another user's is refused, the user's own is deleted
	// whatever its ttl.
	_, err = e.c.GC(ctx, gcp.GCOptions{User: user, Projects: []string{proj}, Account: bob.Email()})
	mustContain(t, err, "belongs to bob@example.com", "never touches another user's account")
	_, err = e.c.GC(ctx, gcp.GCOptions{User: user, Projects: []string{proj}, Account: "x@" + proj + ".iam.gserviceaccount.com"})
	mustContain(t, err, "not a silo account")
	rep, err = e.c.GC(ctx, gcp.GCOptions{User: user, Projects: []string{proj}, Account: fresh})
	if err != nil || len(rep.Deleted) != 1 {
		t.Fatalf("%+v %v", rep, err)
	}
	if _, ok := e.f.Accounts[fresh]; ok {
		t.Fatal("fresh still exists")
	}
	if _, err := e.c.GC(ctx, gcp.GCOptions{User: user, Account: fresh}); err == nil {
		t.Fatal("--account without --project")
	}

	// An account deleted outside silo is forgotten.
	if err := e.f.DeleteAccount(ctx, proj, zeroLive); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.GC(ctx, gcp.GCOptions{User: user, Alive: func(gcp.SessionRef) bool { return false }}); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.c.Store.Load(); len(st.Accounts) != 0 {
		t.Fatalf("state %+v", st.Accounts)
	}
}

func TestStateConcurrentUpdates(t *testing.T) {
	s := &gcp.Store{Path: filepath.Join(t.TempDir(), "silo", "accounts.json")}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Put(gcp.Record{Email: fmt.Sprintf("silo-%02d@p.iam.gserviceaccount.com", i), TTL: gcp.Duration(time.Hour)}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	st, err := s.Load()
	if err != nil || len(st.Accounts) != 20 || time.Duration(st.Accounts[0].TTL) != time.Hour {
		t.Fatalf("%d accounts, %v", len(st.Accounts), err)
	}
	if got := gcp.StatePath(func(k string) string {
		return map[string]string{"XDG_STATE_HOME": "/s", "HOME": "/h"}[k]
	}); got != "/s/silo/accounts.json" {
		t.Fatal(got)
	}
	if got := gcp.StatePath(func(k string) string { return map[string]string{"HOME": "/h"}[k] }); got != "/h/.local/state/silo/accounts.json" {
		t.Fatal(got)
	}
}
