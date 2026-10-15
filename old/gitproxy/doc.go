// Package gitproxy is a git smart-HTTP proxy that holds the git
// credential, so the agent behind it never does (design §9). Each request
// is checked against a Policy and audited once; what it may not do never
// reaches the upstream.
//
// The proxy matches one of the four legal smart-HTTP paths, authenticates
// the caller by its token (Authenticator; silo's SessionToken), asks the
// Policy what the caller may do with the repository, holds the body to a
// per-action limit, and for a push reads the receive-pack command list
// before anything is forwarded: every ref update must be one the Policy
// allows, a command list that cannot be read in full is refused (fail
// closed), and an update the Policy does not let force-move must be shown
// a fast-forward from the pushed pack's commit graph and the upstream's
// history. Only then does it ask the Policy for the credential, forward
// with it (HTTPS, redirects never followed), and stream the answer back.
//
// The Policy is the part a caller supplies (policy.go). silo builds a
// StaticPolicy from a session's Silofile `git:` entries; grain
// implements Policy over its own task model. The package depends on
// neither.
//
// # Provenance
//
// This package is copied and adapted from grain's pkg/gitproxy
// (github.com/bwsalmon/grain, Apache License 2.0) at commit
//
//	5789160c875aa7abb6b4978c3fa337ea6f985a94
//
// following design §9.2: protocol, pack, ancestry, spool, forward and the
// receive-pack parser copied; core, the authorizer (as StaticPolicy and
// RefRules), hosts, audit, server and tokens adapted; grain's credential
// ladder, App tokens, team audit, read scopes, packages and SSH left
// behind. Each file says which grain file it came from. A later sync
// diffs grain's pkg/gitproxy from that commit.
package gitproxy
