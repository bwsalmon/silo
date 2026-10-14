// Adapted from github.com/bwsalmon/grain pkg/gitproxy/authorize.go (ModelAuthorizer) and pkg/gitproxy/refupdates.go (AuthorizeRefUpdates) at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

package gitproxy

// StaticPolicy: a Policy fixed when it is built, for one silo session.
//
// grain's ModelAuthorizer reads a live task model: the task's target and
// reads, its branch, its template's declared push set, its team's
// credential. A silo session has all of that written down before it
// starts, in its Silofile's `git:` entries (design §9.1), so the same
// decisions are a lookup in what NewStaticPolicy was given: the
// repositories, each one's access, push patterns and force, and a
// credential per host.

import (
	"context"
	"fmt"
	"strings"
)

// StaticRepo is one repository a StaticPolicy allows: a Silofile `git:`
// entry.
type StaticRepo struct {
	// Repo is host/owner/name, such as github.com/acme/widgets. A
	// trailing .git is ignored, and owner and name compare
	// case-insensitively, as GitHub compares them.
	Repo string
	// Access is AccessRead or AccessPush.
	Access Access
	// Push is the refs a push may update; required for AccessPush.
	Push RefRules
	// Force allows every Push rule's refs to be force-moved (`force:
	// true`); false refuses any update the proxy cannot show is a
	// fast-forward, unless its own rule says Force.
	Force bool
	// Credential, when set, is what this repository's requests go out
	// with, in place of its host's (StaticConfig.Credentials).
	Credential *CredentialSource
}

// CredentialSource is a credential a StaticPolicy hands out: its name and
// how to get its secret, asked again for every request so that an ambient
// login's token (credentials.Resolver, which caches it) stays current.
type CredentialSource struct {
	// Name is the credential's name in the audit log.
	Name string
	// Username is sent beside the token; empty sends the token as the
	// username (Credential.Username).
	Username string
	// Token returns the secret.
	Token func(ctx context.Context) (string, error)
	// Refused, when set, is called when the upstream answered 401 to a
	// request sent with this credential, so a cached token can be
	// dropped.
	Refused func()
}

// StaticConfig is what NewStaticPolicy builds a policy from.
type StaticConfig struct {
	Repos []StaticRepo
	// Credentials is the credential for each git host (host[:port], as
	// NormalizeHost writes it). A repository on a host with none, and no
	// Credential of its own, is reached anonymously.
	Credentials map[string]*CredentialSource
	// Source is where these grants are written, for refusals to name:
	// "the Silofile's git: entries" when empty.
	Source string
}

// StaticPolicy is a Policy built once from a StaticConfig. It answers
// every caller alike: one silo session has one Silofile.
type StaticPolicy struct {
	repos       map[string]StaticRepo // by repoKey
	credentials map[string]*CredentialSource
	source      string
	byName      map[string]*CredentialSource // CredentialRefused's lookup
}

// NewStaticPolicy checks cfg and builds its policy: every repository
// host/owner/name, listed once, with a known access, and push patterns
// that ValidateRefPattern accepts exactly when it may push.
func NewStaticPolicy(cfg StaticConfig) (*StaticPolicy, error) {
	p := &StaticPolicy{
		repos:       map[string]StaticRepo{},
		credentials: map[string]*CredentialSource{},
		source:      cfg.Source,
		byName:      map[string]*CredentialSource{},
	}
	if p.source == "" {
		p.source = "the Silofile's git: entries"
	}
	for host, c := range cfg.Credentials {
		h, err := NormalizeHost(host)
		if err != nil || h == "" {
			return nil, fmt.Errorf("gitproxy: credential host %q: not a git host", host)
		}
		if err := checkSource(c); err != nil {
			return nil, fmt.Errorf("gitproxy: credential for %s: %w", h, err)
		}
		p.credentials[h] = c
		p.byName[c.Name] = c
	}
	for _, r := range cfg.Repos {
		repo, err := ParseRepo(r.Repo)
		if err != nil {
			return nil, fmt.Errorf("gitproxy: %w", err)
		}
		key := repoKey(repo)
		if _, dup := p.repos[key]; dup {
			return nil, fmt.Errorf("gitproxy: %s is listed twice", repo)
		}
		switch r.Access {
		case AccessRead:
			if len(r.Push) > 0 || r.Force {
				return nil, fmt.Errorf("gitproxy: %s: push patterns and force are only for push access", repo)
			}
		case AccessPush:
			if len(r.Push) == 0 {
				return nil, fmt.Errorf("gitproxy: %s: push access needs the ref patterns a push may update", repo)
			}
			if err := r.Push.Validate(); err != nil {
				return nil, fmt.Errorf("gitproxy: %s: %w", repo, err)
			}
		default:
			return nil, fmt.Errorf("gitproxy: %s: access must be read or push", repo)
		}
		if r.Credential != nil {
			if err := checkSource(r.Credential); err != nil {
				return nil, fmt.Errorf("gitproxy: credential for %s: %w", repo, err)
			}
			p.byName[r.Credential.Name] = r.Credential
		}
		r.Repo = repo.String()
		if r.Force {
			rules := make(RefRules, len(r.Push))
			for i, rule := range r.Push {
				rule.Force = true
				rules[i] = rule
			}
			r.Push = rules
		}
		p.repos[key] = r
	}
	return p, nil
}

func checkSource(c *CredentialSource) error {
	if c == nil || c.Token == nil {
		return fmt.Errorf("no token source")
	}
	if c.Name == "" {
		return fmt.Errorf("a credential needs a name for the audit log")
	}
	return nil
}

// ParseRepo reads host/owner/name (a Silofile's `repo:`) as a Repo, its
// host normalized and any .git dropped.
func ParseRepo(s string) (Repo, error) {
	parts := strings.Split(strings.TrimSuffix(strings.TrimSpace(s), ".git"), "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return Repo{}, fmt.Errorf("%q is not host/owner/name", s)
	}
	host, err := NormalizeHost(parts[0])
	if err != nil || host == "" {
		return Repo{}, fmt.Errorf("%q: %q is not a git host", s, parts[0])
	}
	return Repo{Host: host, Owner: parts[1], Name: parts[2]}, nil
}

// repoKey is how two spellings of one repository compare: owner and
// name case-insensitively, as GitHub does.
func repoKey(r Repo) string {
	return r.Host + "/" + strings.ToLower(r.Owner) + "/" + strings.ToLower(r.Name)
}

func (p *StaticPolicy) lookup(repo Repo) (StaticRepo, bool) {
	r, ok := p.repos[repoKey(repo)]
	return r, ok
}

// Access implements Policy.
func (p *StaticPolicy) Access(_ context.Context, _ Caller, repo Repo) (Access, string, error) {
	r, ok := p.lookup(repo)
	if !ok {
		return AccessNone, fmt.Sprintf("%s is not one of this session's repositories: it is not in %s", repo, p.source), nil
	}
	if r.Access == AccessRead {
		return AccessRead, fmt.Sprintf("%s is read-only for this session (access: read in %s)", repo, p.source), nil
	}
	return r.Access, "", nil
}

// RefUpdates implements Policy: every update must be allowed by the
// repository's push patterns (RefRules.Check), or the whole push is
// refused naming each update that is not.
func (p *StaticPolicy) RefUpdates(_ context.Context, _ Caller, repo Repo, updates []RefUpdate, fastForward FastForward) (RefDecision, error) {
	r, ok := p.lookup(repo)
	if !ok || r.Access != AccessPush {
		return RefDecision{Refusal: fmt.Sprintf("%s may not be pushed to (%s)", repo, p.source)}, nil
	}
	var problems []string
	var grants []RefGrant
	for _, u := range updates {
		kind, by, problem := r.Push.Check(u, fastForward)
		if problem != "" {
			problems = append(problems, problem)
			continue
		}
		grants = append(grants, RefGrant{Ref: u.Ref, Kind: kind, Caller: map[string]string{"pattern": by}})
	}
	if len(problems) > 0 {
		patterns := make([]string, len(r.Push))
		for i, rule := range r.Push {
			patterns[i] = rule.String()
		}
		return RefDecision{Refusal: fmt.Sprintf("refusing to update %s: %s. This session may push %s, as %s says",
			repo, strings.Join(problems, "; "), strings.Join(patterns, ", "), p.source)}, nil
	}
	return RefDecision{Grants: grants}, nil
}

// Credential implements Policy: the repository's own credential, else
// its host's, else none.
func (p *StaticPolicy) Credential(ctx context.Context, _ Caller, repo Repo) (*Credential, error) {
	src := p.credentials[repo.Host]
	if r, ok := p.lookup(repo); ok && r.Credential != nil {
		src = r.Credential
	}
	if src == nil {
		return nil, nil
	}
	token, err := src.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("credential %s: %w", src.Name, err)
	}
	return &Credential{Name: src.Name, Host: repo.Host, Username: src.Username, Token: token}, nil
}

// CredentialRefused implements CredentialRefuser.
func (p *StaticPolicy) CredentialRefused(c *Credential) {
	if src := p.byName[c.Name]; src != nil && src.Refused != nil {
		src.Refused()
	}
}
