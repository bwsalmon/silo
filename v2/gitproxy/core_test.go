// Adapted from github.com/bwsalmon/grain pkg/gitproxy/core_test.go at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

package gitproxy

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestHandleUnknownPathIs404(t *testing.T) {
	p, forwarder, _ := newTestProxy(t)
	resp := p.Handle(context.Background(), "GET", "/owner/repo.git/HEAD", "", gitHeaders, nil)
	if resp.Status != 404 {
		t.Errorf("status = %d, want 404", resp.Status)
	}
	if len(forwarder.Calls) != 0 {
		t.Error("expected no forward call")
	}
}

func TestHandleNonGitUserAgentIs404(t *testing.T) {
	p, _, _ := newTestProxy(t)
	resp := p.Handle(context.Background(), "GET", "/owner/repo.git/info/refs", "",
		map[string]string{"User-Agent": "Mozilla/5.0"}, nil)
	if resp.Status != 404 {
		t.Errorf("status = %d, want 404", resp.Status)
	}
}

func TestHandleMissingAuthIs401BeforeAuthorizationIsConsulted(t *testing.T) {
	p, forwarder, audit := newTestProxy(t)
	resp := p.Handle(context.Background(), "GET", "/owner/repo.git/info/refs",
		"service=git-upload-pack", gitHeaders, nil)
	if resp.Status != 401 {
		t.Errorf("status = %d, want 401", resp.Status)
	}
	if _, ok := resp.Headers["WWW-Authenticate"]; !ok {
		t.Error("expected a WWW-Authenticate header")
	}
	if len(forwarder.Calls) != 0 {
		t.Error("expected no forward call")
	}
	// Audited like every other request, with nothing about any caller.
	if e := audit.Entries(); len(e) != 1 || e[0].Outcome != "denied: authentication required" || e[0].Caller != nil {
		t.Errorf("entries = %+v, want one denial naming no caller", e)
	}
}

func TestHandleUnknownTokenIs401(t *testing.T) {
	p, _, _ := newTestProxy(t)
	headers := merge(gitHeaders, map[string]string{"Authorization": basicAuthHeader("not-a-real-token")})
	resp := p.Handle(context.Background(), "GET", "/owner/repo.git/info/refs",
		"service=git-upload-pack", headers, nil)
	if resp.Status != 401 {
		t.Errorf("status = %d, want 401", resp.Status)
	}
}

func TestHandleOutOfScopeRepoIs403WithLegibleError(t *testing.T) {
	p, forwarder, audit := newTestProxy(t)
	headers := merge(gitHeaders, map[string]string{"Authorization": basicAuthHeader("tok0")})
	resp := p.Handle(context.Background(), "GET", "/owner/unlisted-repo.git/info/refs",
		"service=git-upload-pack", headers, nil)
	if resp.Status != 403 {
		t.Errorf("status = %d, want 403", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "github.com/owner/unlisted-repo is not one of this session's repositories") ||
		!strings.Contains(string(resp.Body), "Silofile") {
		t.Errorf("body = %q, want a legible reason", resp.Body)
	}
	if len(forwarder.Calls) != 0 {
		t.Error("expected no forward call")
	}
	if e := audit.Entries()[0]; e.Outcome != "denied: not in scope" || e.Repo != "github.com/owner/unlisted-repo" || e.Status != 403 {
		t.Errorf("entry = %+v", e)
	}
}

func TestHandleAllowedRepoIsForwardedWithTheSelectedCredential(t *testing.T) {
	p, forwarder, audit := newTestProxy(t)
	headers := merge(gitHeaders, map[string]string{"Authorization": basicAuthHeader("tok0")})
	resp := p.Handle(context.Background(), "GET", "/owner/repo.git/info/refs",
		"service=git-upload-pack", headers, nil)
	if resp.Status != 200 || string(resp.Body) != "ok" {
		t.Fatalf("resp = %+v", resp)
	}
	call := forwarder.Calls[0]
	if call.Credential == nil || call.Credential.Token != "bot-token" {
		t.Errorf("credential = %+v, want bot-token", call.Credential)
	}
	if call.Path != "/owner/repo.git/info/refs" {
		t.Errorf("path = %q", call.Path)
	}
	if e := audit.Entries()[0]; e.Credential != "bot" || e.Caller["session"] != "sandbox-0" ||
		e.Outcome != "forwarded: 200" || e.Service != "git-upload-pack" {
		t.Errorf("entry = %+v", e)
	}
}

func TestHandlePostBodyIsPassedThroughUntouched(t *testing.T) {
	p, forwarder, _ := newTestProxy(t)
	headers := merge(gitHeaders, map[string]string{
		"Authorization": basicAuthHeader("tok0"),
		"Content-Type":  "application/x-git-upload-pack-request",
		"Accept":        "application/x-git-upload-pack-result",
	})
	resp := p.Handle(context.Background(), "POST", "/owner/repo.git/git-upload-pack", "",
		headers, []byte("some pack request bytes"))
	if resp.Status != 200 {
		t.Fatalf("status = %d", resp.Status)
	}
	if string(forwarder.Calls[0].Body) != "some pack request bytes" {
		t.Errorf("body = %q", forwarder.Calls[0].Body)
	}
}

// policyStub is a Policy whose every answer is scripted.
type policyStub struct {
	access     Access
	accessErr  error
	credential *Credential
	credErr    error
	refused    []*Credential
}

func (s *policyStub) Access(context.Context, Caller, Repo) (Access, string, error) {
	return s.access, "", s.accessErr
}

func (s *policyStub) RefUpdates(context.Context, Caller, Repo, []RefUpdate, FastForward) (RefDecision, error) {
	return RefDecision{}, nil
}

func (s *policyStub) Credential(context.Context, Caller, Repo) (*Credential, error) {
	return s.credential, s.credErr
}

func (s *policyStub) CredentialRefused(c *Credential) { s.refused = append(s.refused, c) }

func TestHandleSurfacesAPolicyErrorAs500(t *testing.T) {
	for name, policy := range map[string]*policyStub{
		"access":     {accessErr: errors.New("db down")},
		"credential": {access: AccessRead, credErr: errors.New("gh auth token: not logged in")},
	} {
		p, forwarder, audit := newTestProxy(t)
		p.Policy = policy
		headers := merge(gitHeaders, map[string]string{"Authorization": basicAuthHeader("tok0")})
		resp := p.Handle(context.Background(), "GET", "/owner/repo.git/info/refs",
			"service=git-upload-pack", headers, nil)
		if resp.Status != 500 {
			t.Errorf("%s: status = %d, want 500", name, resp.Status)
		}
		if len(forwarder.Calls) != 0 {
			t.Errorf("%s: expected no forward call", name)
		}
		if e := audit.Entries(); len(e) != 1 || !strings.HasPrefix(e[0].Outcome, "error: ") {
			t.Errorf("%s: entries = %+v", name, e)
		}
	}
}

// A credential the Policy says is for another host is refused rather
// than sent.
func TestHandleRefusesACredentialForAnotherHost(t *testing.T) {
	p, forwarder, _ := newTestProxy(t)
	p.Policy = &policyStub{access: AccessRead, credential: &Credential{Name: "ghe", Host: "ghe.example.com", Token: "t"}}
	headers := merge(gitHeaders, map[string]string{"Authorization": basicAuthHeader("tok0")})
	resp := p.Handle(context.Background(), "GET", "/owner/repo.git/info/refs", "service=git-upload-pack", headers, nil)
	if resp.Status != 500 || !strings.Contains(string(resp.Body), "is for git host ghe.example.com, not github.com") {
		t.Errorf("resp = %d %q", resp.Status, resp.Body)
	}
	if len(forwarder.Calls) != 0 {
		t.Error("a wrong-host credential was sent")
	}
}

// No credential is an anonymous request, for a public repository.
func TestHandleWithNoCredentialForwardsAnonymously(t *testing.T) {
	p, forwarder, audit := newTestProxy(t)
	p.Policy = &policyStub{access: AccessRead}
	headers := merge(gitHeaders, map[string]string{"Authorization": basicAuthHeader("tok0")})
	resp := p.Handle(context.Background(), "GET", "/owner/repo.git/info/refs", "service=git-upload-pack", headers, nil)
	if resp.Status != 200 || len(forwarder.Calls) != 1 || forwarder.Calls[0].Credential != nil {
		t.Fatalf("resp = %d, calls = %+v", resp.Status, forwarder.Calls)
	}
	if e := audit.Entries()[0]; e.Credential != "" {
		t.Errorf("entry = %+v", e)
	}
}

// An upstream 401 is answered as a refusal naming the credential, never
// with the upstream's WWW-Authenticate (git would ask for a new password
// for the proxy), and the policy is told so it can drop a cached token.
func TestHandleAnUpstream401NamesTheCredentialAndTellsThePolicy(t *testing.T) {
	p, forwarder, _ := newTestProxy(t)
	policy := &policyStub{access: AccessRead, credential: &Credential{Name: "gh", Token: "stale"}}
	p.Policy = policy
	forwarder.Response = UpstreamResponse{Status: 401, Headers: map[string]string{"WWW-Authenticate": "Basic"},
		Stream: io.NopCloser(strings.NewReader("nope"))}
	headers := merge(gitHeaders, map[string]string{"Authorization": basicAuthHeader("tok0")})
	resp := p.Handle(context.Background(), "GET", "/owner/repo.git/info/refs", "service=git-upload-pack", headers, nil)
	if resp.Status != 403 || resp.Headers["WWW-Authenticate"] != "" || !strings.Contains(string(resp.Body), "credential gh") {
		t.Errorf("resp = %d %v %q", resp.Status, resp.Headers, resp.Body)
	}
	if len(policy.refused) != 1 || policy.refused[0].Name != "gh" {
		t.Errorf("refused = %+v", policy.refused)
	}
}

// A request naming its host and one naming none are one repository on
// the default host; a host the policy does not list is refused.
func TestHandleReadsTheHostFromThePath(t *testing.T) {
	p, forwarder, audit := newTestProxy(t)
	var hosts []string
	p.ForwarderFor = func(host string) (Forwarder, error) { hosts = append(hosts, host); return forwarder, nil }
	headers := merge(gitHeaders, map[string]string{"Authorization": basicAuthHeader("tok0")})
	for _, path := range []string{"/owner/repo.git/info/refs", "/github.com/owner/repo.git/info/refs", "/GitHub.com:443/Owner/Repo/info/refs"} {
		if resp := p.Handle(context.Background(), "GET", path, "service=git-upload-pack", headers, nil); resp.Status != 200 {
			t.Errorf("%s: %d %q", path, resp.Status, resp.Body)
		}
	}
	if resp := p.Handle(context.Background(), "GET", "/ghe.example.com/owner/repo.git/info/refs", "service=git-upload-pack", headers, nil); resp.Status != 403 {
		t.Errorf("another host: %d, want 403", resp.Status)
	}
	if strings.Join(hosts, " ") != "github.com github.com github.com" {
		t.Errorf("forwarded to %v", hosts)
	}
	if e := audit.Entries(); len(e) != 4 || e[3].Repo != "ghe.example.com/owner/repo" {
		t.Errorf("entries = %+v", e)
	}
}
