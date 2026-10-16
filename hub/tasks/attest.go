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
// task may run there. approved_by is the task's approver, empty for a
// task nobody approved at the hub; the digest does not cover it, as
// grain's does not. hub_task_id is the task's hub ID, s.ID.
//
// The digest's body is the prompt exactly as offered, and the node
// files the task with exactly that body (§6.4): what links the task
// to the hub is the signed hub_task_id, not anything added to the
// body, so the node can recompute the digest from the task it filed.
func Attest(s Spec, node string) claims.Claims {
	return claims.Claims{
		Audience:   node,
		HubTaskID:  s.ID,
		Requester:  s.Submitter,
		Team:       s.Team,
		ApprovedBy: s.ApprovedBy,
		SpecDigest: claims.SpecDigest(claims.Spec{
			Target: s.Repo,
			Title:  s.Title,
			Body:   s.Prompt,
		}),
	}
}
