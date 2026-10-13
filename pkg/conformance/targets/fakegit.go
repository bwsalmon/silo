package targets

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite" // the driver VACUUM INTO runs on

	"github.com/bwsalmon/silo/pkg/backend/local/gitstore"
	"github.com/bwsalmon/silo/pkg/backend/local/storage"
	"github.com/bwsalmon/silo/pkg/conformance"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/gateway"
	"github.com/bwsalmon/silo/pkg/gitserve"
	"github.com/bwsalmon/silo/pkg/mirror"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
)

// The fake's repos and mirror (local-git.md §11 item 24): silod's git
// service, gitstore over the dir driver and the outbound mirror, with
// the gateway in front, following silod's tables over a Unix socket as
// it does on a host. A probe's git clients are the gateway's port 80
// seen from the probe's address. A restore replaces all of it with a
// silod started on a copy of the store and the repos taken earlier, so
// that what a case sees after one is the restored silod.

var _ conformance.Git = (*fakeRegistry)(nil)

// hooked is whether this test binary runs the pre-receive hook
// (GitHook): only then does the fake's git service accept pushes.
var hooked atomic.Bool

// GitHook runs the test binary as the git service's pre-receive hook
// when receive-pack runs it so, as silod's main does, and otherwise
// notes that it will: the TestMain of a package whose cases push calls
// it first. Without it the fake's hook refuses every push, rather than
// the test binary running its own tests again as a hook.
func GitHook() {
	if len(os.Args) > 1 && os.Args[1] == gitserve.HookArg {
		os.Exit(gitserve.HookMain(os.Args[2:], os.Stdin, os.Stderr))
	}
	hooked.Store(true)
}

// fakeSilod is the part of the fake silod a restore replaces: its
// store, its repos, the git service, the mirror, the fence API that
// serves them, and the gateway that follows it.
type fakeSilod struct {
	db     *sqlite.Store
	dir    string // its data directory: silo.db, silos/, keys/ and cache/
	api    *fenceapi.Server
	mirror *mirror.Mirror
	gw     *gateway.Gateway
	stop   func()
}

// handlerSwap serves with the handler last stored.
type handlerSwap struct{ h atomic.Pointer[http.Handler] }

func (s *handlerSwap) set(h http.Handler) { s.h.Store(&h) }

func (s *handlerSwap) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	(*s.h.Load()).ServeHTTP(w, r)
}

// startSilod starts the git service, the mirror, a fence API serving
// them over db and the gateway in front, with dir as the data
// directory. opts is the fence API's, to which it adds the repos.
func (f *fakeRegistry) startSilod(t testing.TB, db *sqlite.Store, dir string, opts fenceapi.Options) (*fakeSilod, error) {
	s := &fakeSilod{db: db, dir: dir}
	hooks := filepath.Join(dir, "cache", "git-hooks")
	if err := gitserve.WriteHooks(hooks); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "silos"), 0o700); err != nil {
		return nil, err
	}
	hook := ""
	if hooked.Load() {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		hook = exe
	}
	host := &gitstore.Host{Storage: &storage.Dir{Root: filepath.Join(dir, "silos")}, HooksDir: hooks}
	git := gitserve.New(gitserve.Options{Host: host, Hook: hook})
	s.mirror = mirror.New(mirror.Options{Store: db, Repos: host, Keys: filepath.Join(dir, "keys", "mirror"),
		Dir: filepath.Join(dir, "cache", "mirror"), Poll: 50 * time.Millisecond, MinBackoff: 50 * time.Millisecond,
		MaxBackoff: 200 * time.Millisecond, PushTimeout: 20 * time.Second, IndexDebounce: 50 * time.Millisecond, Logf: t.Logf})
	opts.Repos, opts.Git, opts.Mirror = host, git, s.mirror
	s.api = fenceapi.New(db, opts)
	git.SetAPI(s.api)

	// silod's gateway socket, in a short path, as Unix socket paths are.
	sockDir, err := os.MkdirTemp("", "gw")
	if err != nil {
		return nil, err
	}
	sock := filepath.Join(sockDir, "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		os.RemoveAll(sockDir)
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	pub := gateway.NewPublisher(db)
	ssrv := &http.Server{Handler: gateway.Silod(db, gateway.SilodOptions{API: s.api, Publisher: pub, Git: git})}
	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); pub.Run(ctx) }()
	go func() { defer wg.Done(); ssrv.Serve(l) }()
	go func() { defer wg.Done(); s.mirror.Run(ctx) }()
	s.gw = gateway.New(gateway.Options{Retry: 50 * time.Millisecond, Dial: func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}})
	go func() { defer wg.Done(); s.gw.Follow(ctx) }()
	var once sync.Once
	s.stop = func() {
		once.Do(func() {
			cancel()
			ssrv.Close()
			wg.Wait()
			s.api.Close()
			os.RemoveAll(sockDir)
		})
	}
	t.Cleanup(s.stop)
	return s, nil
}

// GitURL is a listener on this host that serves the gateway's port 80
// to each connection as coming from p's address, as silo-net's gw0
// sees a probe's, and its /git/.
func (f *fakeRegistry) GitURL(t testing.TB, p conformance.Probe) string {
	t.Helper()
	from := p.Addr().String() + ":40000"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = from
		f.silod.Load().gw.HTTP().ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/git/"
}

// fakeUber is an uber repo served by git http-backend, with a switch
// in front that refuses every request while it is down.
type fakeUber struct {
	t    testing.TB
	dir  string
	url  string
	down atomic.Bool
}

func (f *fakeRegistry) Uber(t testing.TB) conformance.Uber {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on $PATH")
	}
	u := &fakeUber{t: t, dir: filepath.Join(t.TempDir(), "uber.git")}
	run := exec.Command(gitBin, "init", "--quiet", "--bare", "-b", "main", u.dir)
	run.Env = gitstore.Env()
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	u.git("config", "http.receivepack", "true")
	backend := &cgi.Handler{Path: gitBin, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + filepath.Dir(u.dir), "GIT_HTTP_EXPORT_ALL=1"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u.down.Load() {
			http.Error(w, "the uber repo is down", http.StatusServiceUnavailable)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
			// http-backend wants a length, which git's chunked posts lack.
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			r.Header.Set("Content-Length", fmt.Sprint(len(body)))
			r.Header.Del("Transfer-Encoding")
			r.TransferEncoding = nil
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	u.url = srv.URL + "/uber.git"
	return u
}

func (u *fakeUber) git(args ...string) string {
	u.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = u.dir
	cmd.Env = gitstore.Env()
	out, err := cmd.CombinedOutput()
	if err != nil {
		u.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (u *fakeUber) URL() string { return u.url }

func (u *fakeUber) Credential() string { return "token" }

func (u *fakeUber) SetDown(down bool) { u.down.Store(down) }

func (u *fakeUber) Refs() map[string]string {
	u.t.Helper()
	out := map[string]string{}
	for _, line := range strings.Split(u.git("for-each-ref", "--format=%(objectname) %(refname)"), "\n") {
		if sha, name, ok := strings.Cut(line, " "); ok {
			out[name] = sha
		}
	}
	return out
}

// Backup copies silod's store (VACUUM INTO, a consistent copy of a
// live SQLite database) and its data directory. The restore stops
// silod and starts another on the copies, in report-only mode, as
// silod starts after it detects a restore; the fence API's calls and
// the gateway's port 80 go to it from then on. The fake's facade,
// registry and fence stay as they were: a case that restores uses
// repos and the mirror only.
func (f *fakeRegistry) Backup(t testing.TB) (restore func()) {
	t.Helper()
	cur := f.silod.Load()
	to := t.TempDir()
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(cur.dir, "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`VACUUM INTO ?`, filepath.Join(to, "silo.db"))
	raw.Close()
	if err != nil {
		t.Fatalf("backing up the store: %v", err)
	}
	for _, d := range []string{"silos", "keys"} {
		if _, err := os.Stat(filepath.Join(cur.dir, d)); err != nil {
			continue
		}
		if out, err := exec.Command("cp", "-a", filepath.Join(cur.dir, d), to).CombinedOutput(); err != nil {
			t.Fatalf("backing up %s: %v\n%s", d, err, out)
		}
	}
	return func() {
		t.Helper()
		f.silod.Load().stop()
		db, err := sqlite.Open(filepath.Join(to, "silo.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		ctx := context.Background()
		if err := store.SetSetting(ctx, db, store.SettingReconcile, store.ReconcileReportOnly); err != nil {
			t.Fatal(err)
		}
		s, err := f.startSilod(t, db, to, fenceapi.Options{Caller: byHeader(fenceapi.ViaOrigin)})
		if err != nil {
			t.Fatalf("restarting silod: %v", err)
		}
		f.silod.Store(s)
		f.apiH.set(s.api)
	}
}
