package gitproxy

// The Policy interface: everything the proxy asks of whoever runs it.
//
// grain's pkg/gitproxy asked these questions of model.Store through a
// handful of lookups (Authorizer, BranchLookup, DeclaredPushLookup,
// CredentialOverrideLookup, TeamCredentialLookup ...). Here they are one
// interface, so that silo answers them from a Silofile (StaticPolicy,
// static.go) and grain from its task model, each with its own
// implementation and the same proxy in front of it.

import (
	"context"
	"encoding/base64"
	"fmt"
)

// Repo is one repository a request names: its git host, normalized
// (NormalizeHost) and never empty, and its owner and name as the request
// spelt them, without ".git".
type Repo struct {
	Host  string
	Owner string
	Name  string
}

// String is host/owner/name, the form a Silofile's `repo:` takes.
func (r Repo) String() string { return r.Host + "/" + r.Owner + "/" + r.Name }

// Access is what a caller may do with a repository.
type Access int

const (
	// AccessNone refuses every request for the repository.
	AccessNone Access = iota
	// AccessRead allows fetches and clones: info/refs for
	// git-upload-pack, and git-upload-pack.
	AccessRead
	// AccessPush also allows pushes, each of whose ref updates
	// Policy.RefUpdates then decides.
	AccessPush
)

func (a Access) String() string {
	switch a {
	case AccessRead:
		return "read"
	case AccessPush:
		return "push"
	default:
		return "none"
	}
}

// Caller is who made a request, as the proxy's Authenticator resolved
// its token.
type Caller struct {
	// ID is what the Policy keys its answers on: a session for silo, a
	// sandbox for grain.
	ID string
	// Audit is copied into the caller map of every audit entry the
	// caller's requests produce (AuditEntry.Caller) -- silo's session,
	// grain's sandbox, task and run. The proxy does not read it.
	Audit map[string]string
}

// Authenticator turns the token a git client sent (as the password of
// its Basic auth) into a Caller. SessionToken is silo's: one token, in
// memory, for the life of a session.
type Authenticator interface {
	Authenticate(token string) (Caller, bool)
}

// Credential is what a request goes upstream with. It never reaches the
// caller.
type Credential struct {
	// Name is how the audit log names it: a Silofile's credentials[]
	// entry, or grain's credential name. Never the secret.
	Name string
	// Host, when set, is the one git host the credential belongs to. A
	// request for any other host is refused rather than sent with it
	// (grain's Credential.WrongHost), whatever the Policy answered.
	Host string
	// Username is the Basic auth username. Empty sends Token as the
	// username with an empty password, GitHub's convention for a token.
	Username string
	// Token is the secret: a token, or a password with Username.
	Token string
}

// basicAuth is the Authorization header c is sent as.
func (c *Credential) basicAuth() string {
	pair := c.Token + ":"
	if c.Username != "" {
		pair = c.Username + ":" + c.Token
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(pair))
}

// FastForward reports whether newTip is shown to descend from oldTip:
// by the commits the push carries or, where those run out, by the
// commits the upstream already has (ancestry.go). why says what stopped
// a proof that was not made, when that is more than the walk running
// out. It reads the push's pack the first time it is called, and only
// then; a Policy that never calls it never makes the proxy read past
// the command list.
type FastForward func(newTip, oldTip string) (ok bool, why string)

// RefGrant is one ref update a Policy let through, and on whose
// say-so: what the request's audit entry records for it.
type RefGrant struct {
	Ref string `json:"ref"`
	// Kind is "create", "update", "force" or "delete" (UpdateKind).
	Kind string `json:"kind"`
	// Caller is what allowed the update, for the audit log: silo's
	// matching pattern; grain's template and declaration file.
	Caller map[string]string `json:"caller,omitempty"`
}

// RefDecision is a Policy's answer to a push's command list.
type RefDecision struct {
	// Refusal, when non-empty, refuses the whole push before anything is
	// forwarded, and is what the git client prints.
	Refusal string
	// Grants is every update allowed, when Refusal is empty.
	Grants []RefGrant
}

// Policy is what the proxy asks about each request, after it has
// authenticated the caller and before anything is forwarded. Its methods
// are called concurrently, one request per goroutine.
//
// The proxy owns the protocol: it parses the path, holds the body to its
// limits, reads a push's command list (failing closed on one it cannot
// read), proves fast-forwards, forwards, and audits every request once.
// The Policy owns only the decisions, which is the part grain and silo
// answer differently:
//
//   - silo's StaticPolicy answers from one session's Silofile `git:`
//     entries: each repository's access, its `push:` patterns and
//     `force:`, and a credential per host.
//   - grain's implementation answers from model.Store: the live task's
//     target and reads become Access; "only the run's own branch" is a
//     RefRule for refs/heads/<branch>, a template's declared push set
//     more of them (RefRules.Check does both), with the template and
//     declaration in each RefGrant's Caller map; the credential is the
//     task's grant or its team's.
type Policy interface {
	// Access is what caller may do with repo. refusal is what the git
	// client is told when that is not enough for the request -- AccessNone
	// for any request, AccessRead for a push -- and should name where the
	// caller's grants are written (silo: the Silofile). err is a failure
	// to decide, answered as a 500 and never as an allowance.
	Access(ctx context.Context, caller Caller, repo Repo) (access Access, refusal string, err error)

	// RefUpdates decides a push to repo, which Access allowed to push,
	// from its command list: every update in updates must be allowed, or
	// the whole push is refused. fastForward proves an update is a
	// fast-forward when the policy asks (RefRules.Check does when no rule
	// allows a force-push).
	RefUpdates(ctx context.Context, caller Caller, repo Repo, updates []RefUpdate, fastForward FastForward) (RefDecision, error)

	// Credential is what a request for repo goes upstream with, or nil
	// to send it anonymously (a public repository). It is asked for every
	// forwarded request, so a credential that expires (an ambient login)
	// is fetched afresh each time from whatever caches it.
	Credential(ctx context.Context, caller Caller, repo Repo) (*Credential, error)
}

// CredentialRefuser is an optional half of Policy: told when the
// upstream answered 401 to a request sent with credential, so that a
// cached ambient token is asked for again next time rather than reused
// (grain's CredentialSet.Refused).
type CredentialRefuser interface {
	CredentialRefused(credential *Credential)
}

// errPolicy wraps a Policy's error for the audit log.
func errPolicy(what string, err error) string {
	return fmt.Sprintf("error: %s: %v", what, err)
}
