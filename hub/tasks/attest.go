package tasks

import "github.com/bwsalmon/silo/hub/claims"

// Attest is what the hub attests about s to the node it offers s to
// (docs/hub.md §6.4): its submitter, as the requester, its team, and
// the digest of the spec the node is given. The digest covers the
// task's repository, title and prompt, as grain's target, title and
// body; the hub gives a task no base branch, reads, writes,
// capabilities or network access of its own, so those are empty, as
// the node sees them. A team-targeted task has no target: its node
// chooses the repository, and its own policy decides whether this
// task may run there. Nobody approves a hub task beyond its
// submission, so approved_by is empty.
func Attest(s Spec, node string) claims.Claims {
	return claims.Claims{
		Audience:  node,
		Requester: s.Submitter,
		Team:      s.Team,
		SpecDigest: claims.SpecDigest(claims.Spec{
			Target: s.Repo,
			Title:  s.Title,
			Body:   s.Prompt,
		}),
	}
}
