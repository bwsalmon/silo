package gitproxy

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestValidateRefPattern(t *testing.T) {
	for _, ok := range []string{"refs/heads/silo/*", "refs/heads/main", "refs/tags/v*", "refs/heads/release/?"} {
		if err := ValidateRefPattern(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", " refs/heads/x", "main", "refs/*", "*", "refs/heads/*", "refs/tags/**", "refs/notes/x",
		"refs/heads/[ab]", "refs/heads/a..b", "refs/heads//x", "refs/heads/x/", "refs/heads/x.lock",
	} {
		if err := ValidateRefPattern(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestNewStaticPolicyRefusesWhatASilofileCouldNotMean(t *testing.T) {
	gh := staticToken("gh", "t")
	for name, cfg := range map[string]StaticConfig{
		"not host/owner/name": {Repos: []StaticRepo{{Repo: "acme/widgets", Access: AccessRead}}},
		"listed twice": {Repos: []StaticRepo{
			{Repo: "github.com/acme/widgets", Access: AccessRead},
			{Repo: "GitHub.com/Acme/Widgets.git", Access: AccessRead},
		}},
		"no access":          {Repos: []StaticRepo{{Repo: "github.com/acme/widgets"}}},
		"push without refs":  {Repos: []StaticRepo{{Repo: "github.com/acme/widgets", Access: AccessPush}}},
		"read with refs":     {Repos: []StaticRepo{{Repo: "github.com/acme/widgets", Access: AccessRead, Push: RefRules{{Pattern: "refs/heads/x"}}}}},
		"read with force":    {Repos: []StaticRepo{{Repo: "github.com/acme/widgets", Access: AccessRead, Force: true}}},
		"every branch":       {Repos: []StaticRepo{{Repo: "github.com/acme/widgets", Access: AccessPush, Push: RefRules{{Pattern: "refs/heads/*"}}}}},
		"bad host":           {Credentials: map[string]*CredentialSource{"user@github.com": gh}},
		"no token source":    {Credentials: map[string]*CredentialSource{"github.com": {Name: "gh"}}},
		"unnamed credential": {Credentials: map[string]*CredentialSource{"github.com": staticToken("", "t")}},
	} {
		if _, err := NewStaticPolicy(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestStaticPolicyAccess(t *testing.T) {
	p := testPolicy(t)
	ctx := context.Background()
	for repo, want := range map[Repo]Access{
		{Host: "github.com", Owner: "owner", Name: "repo"}:     AccessPush,
		{Host: "github.com", Owner: "OWNER", Name: "Repo"}:     AccessPush,
		{Host: "github.com", Owner: "owner", Name: "readonly"}: AccessRead,
		{Host: "github.com", Owner: "owner", Name: "other"}:    AccessNone,
		{Host: "gitlab.com", Owner: "owner", Name: "repo"}:     AccessNone,
	} {
		got, refusal, err := p.Access(ctx, Caller{}, repo)
		if err != nil || got != want {
			t.Errorf("%s: %v, %v; want %v", repo, got, err, want)
		}
		if want != AccessPush && !strings.Contains(refusal, "Silofile") {
			t.Errorf("%s: refusal %q does not name the Silofile", repo, refusal)
		}
	}
}

func TestStaticPolicyCredentialIsTheReposOwnElseItsHosts(t *testing.T) {
	var refused int
	own := &CredentialSource{Name: "own", Username: "bot", Token: func(context.Context) (string, error) { return "own-token", nil },
		Refused: func() { refused++ }}
	failing := &CredentialSource{Name: "ambient", Token: func(context.Context) (string, error) { return "", errors.New("gh: not logged in") }}
	p, err := NewStaticPolicy(StaticConfig{
		Repos: []StaticRepo{
			{Repo: "github.com/acme/own", Access: AccessRead, Credential: own},
			{Repo: "github.com/acme/host", Access: AccessRead},
			{Repo: "ghe.example.com/acme/failing", Access: AccessRead},
			{Repo: "gitlab.com/acme/anonymous", Access: AccessRead},
		},
		Credentials: map[string]*CredentialSource{"https://GitHub.com/": staticToken("gh", "gh-token"), "ghe.example.com:443": failing},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c, err := p.Credential(ctx, Caller{}, Repo{Host: "github.com", Owner: "acme", Name: "own"})
	if err != nil || c.Name != "own" || c.Username != "bot" || c.Token != "own-token" || c.Host != "github.com" {
		t.Errorf("own: %+v, %v", c, err)
	}
	p.CredentialRefused(c)
	if refused != 1 {
		t.Errorf("Refused called %d times", refused)
	}
	if c, err := p.Credential(ctx, Caller{}, Repo{Host: "github.com", Owner: "acme", Name: "host"}); err != nil || c.Name != "gh" || c.Token != "gh-token" {
		t.Errorf("host: %+v, %v", c, err)
	}
	if _, err := p.Credential(ctx, Caller{}, Repo{Host: "ghe.example.com", Owner: "acme", Name: "failing"}); err == nil ||
		!strings.Contains(err.Error(), "not logged in") {
		t.Errorf("failing: %v", err)
	}
	if c, err := p.Credential(ctx, Caller{}, Repo{Host: "gitlab.com", Owner: "acme", Name: "anonymous"}); err != nil || c != nil {
		t.Errorf("anonymous: %+v, %v", c, err)
	}
}

func TestParseRepo(t *testing.T) {
	r, err := ParseRepo("GitHub.com:443/acme/widgets.git")
	if err != nil || r != (Repo{Host: "github.com", Owner: "acme", Name: "widgets"}) || r.String() != "github.com/acme/widgets" {
		t.Errorf("%+v, %v", r, err)
	}
	for _, bad := range []string{"", "acme/widgets", "github.com/acme", "github.com/acme/widgets/x", "user@github.com/a/b", "github.com//b"} {
		if _, err := ParseRepo(bad); err == nil {
			t.Errorf("%q was parsed", bad)
		}
	}
}
