// Adapted from github.com/bwsalmon/grain pkg/gitproxy/concurrency_test.go at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

package gitproxy

// A real deployment serves every dispatched sandbox slot's git traffic
// off the same *GitProxy through net/http, which hands each inbound
// connection its own goroutine -- so two (or twenty) sandboxes pushing or
// fetching at the same moment already call GitProxy.Handle concurrently
// today, whether or not anything here ever exercised that. These tests
// drive NewHandler's real http.Handler with many concurrent requests
// against many distinct sandboxes and repos, run with -race, to catch
// exactly the kind of shared-state bug credentials_test.go's
// TestSelectAndGetAreSafeForConcurrentCallers found in CredentialSet.

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// echoForwarder answers every call with the requested path as its body --
// deliberately stateless (no field any call writes to), so using it from
// many goroutines at once introduces no race of its own for a concurrency
// test to trip over by accident.
type echoForwarder struct{}

func (echoForwarder) Forward(method, path, query string, headers map[string]string, body []byte, credential *Credential) (UpstreamResponse, error) {
	return UpstreamResponse{Status: 200, Body: []byte(path + " " + credential.Token)}, nil
}

// tokenTable authenticates each of many callers by its own token.
type tokenTable map[string]string // token -> caller

func (t tokenTable) Authenticate(token string) (Caller, bool) {
	id, ok := t[token]
	return Caller{ID: id, Audit: map[string]string{"sandbox": id}}, ok
}

// syncAuditLog is a minimal locked audit log of the test's own, so the
// count it checks does not rest on RecordingAuditLog's lock.
type syncAuditLog struct {
	mu      sync.Mutex
	Entries []AuditEntry
}

func (l *syncAuditLog) Record(entry AuditEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Entries = append(l.Entries, entry)
}

func (l *syncAuditLog) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.Entries)
}

// TestHandlerServesManyConcurrentSandboxesAndRepos drives one GitProxy,
// wired to a real net/http server, with many distinct callers each
// requesting many distinct repos at once -- exercising StaticPolicy's
// lookups and credential sources, the Authenticator, and the audit log
// all under real concurrent HTTP load.
func TestHandlerServesManyConcurrentSandboxesAndRepos(t *testing.T) {
	repos := []struct{ owner, repo string }{
		{"acme", "widgets"}, {"acme", "gadgets"}, {"other", "thing"},
	}
	acme := staticToken("acme-cred", "acme-token")
	policy, err := NewStaticPolicy(StaticConfig{
		Repos: []StaticRepo{
			{Repo: "github.com/acme/widgets", Access: AccessRead, Credential: acme},
			{Repo: "github.com/acme/gadgets", Access: AccessRead, Credential: acme},
			{Repo: "github.com/other/thing", Access: AccessRead},
		},
		Credentials: map[string]*CredentialSource{"github.com": staticToken("global-cred", "global-token")},
	})
	if err != nil {
		t.Fatal(err)
	}

	const sandboxes = 16
	tokens := tokenTable{}
	for i := 0; i < sandboxes; i++ {
		tokens[fmt.Sprintf("tok-%d", i)] = fmt.Sprintf("sandbox-%d", i)
	}

	audit := &syncAuditLog{}
	proxy := &GitProxy{
		Policy:       policy,
		Tokens:       tokens,
		ForwarderFor: forwardTo(echoForwarder{}),
		Audit:        audit,
	}
	server := httptest.NewServer(NewHandler(proxy))
	defer server.Close()

	const requestsPerSandbox = 8
	total := sandboxes * requestsPerSandbox
	var wg sync.WaitGroup
	errs := make(chan error, total)
	wg.Add(total)
	for i := 0; i < sandboxes; i++ {
		token := fmt.Sprintf("tok-%d", i)
		for j := 0; j < requestsPerSandbox; j++ {
			r := repos[(i+j)%len(repos)]
			go func(token, owner, repo string) {
				defer wg.Done()
				path := fmt.Sprintf("/%s/%s.git/info/refs?service=git-upload-pack", owner, repo)
				req, err := http.NewRequest("GET", server.URL+path, nil)
				if err != nil {
					errs <- err
					return
				}
				req.Header.Set("User-Agent", "git/2.39.2")
				req.Header.Set("Accept", "*/*")
				req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("x:"+token)))

				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					errs <- err
					return
				}
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					errs <- err
					return
				}
				if resp.StatusCode != 200 {
					errs <- fmt.Errorf("%s %s: status = %d, body = %q", owner, repo, resp.StatusCode, body)
					return
				}
				wantBody := fmt.Sprintf("/%s/%s.git/info/refs global-token", owner, repo)
				if owner == "acme" {
					wantBody = fmt.Sprintf("/%s/%s.git/info/refs acme-token", owner, repo)
				}
				if string(body) != wantBody {
					errs <- fmt.Errorf("%s %s: body = %q, want %q", owner, repo, body, wantBody)
				}
			}(token, r.owner, r.repo)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	if got := audit.len(); got != total {
		t.Errorf("audit logged %d entries, want %d -- concurrent Record calls lost some", got, total)
	}
}
