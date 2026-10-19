package security_test

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/hub/grainfiles"
)

// TestDeploymentAdminCannotBypassAcceptedUsers: adding a member admits a
// new user to the Hub's accepted users list, so only a hub sys admin may
// add a user who is not already on it. Otherwise any accepted user could
// create a self-service deployment and admit arbitrary outsiders through
// it, though /v1/users is sys-admin only.
func TestDeploymentAdminCannotBypassAcceptedUsers(t *testing.T) {
	hb := startHub(t, options{})
	hb.teams()
	const outsider = "outsider@example.com"

	hb.must(hb.user(outsider), "GET", "/v1/me", nil, http.StatusForbidden, nil)

	// A team admin of an existing deployment.
	hb.must(hb.user(alice), "PUT", "/v1/deployments/"+teamA+"/members/user:"+outsider, map[string]string{"role": "member"}, http.StatusForbidden, nil)
	// An accepted user with no team, through a self-service deployment.
	hb.must(hb.user(nina), "POST", "/v1/deployments", map[string]string{"id": "nina-dep"}, http.StatusCreated, nil)
	hb.must(hb.user(nina), "PUT", "/v1/deployments/nina-dep/members/user:"+outsider, map[string]string{"role": "admin"}, http.StatusForbidden, nil)
	hb.must(hb.user(nina), "PUT", "/v1/teams/nina-dep/members/user:"+outsider, map[string]string{"role": "viewer"}, http.StatusForbidden, nil)
	hb.must(hb.user(outsider), "GET", "/v1/me", nil, http.StatusForbidden, nil)

	// Already-accepted users may still be added by a deployment admin.
	hb.must(hb.user(nina), "PUT", "/v1/deployments/nina-dep/members/user:"+pat, map[string]string{"role": "member"}, http.StatusNoContent, nil)

	// A hub sys admin may admit a new user through a deployment.
	hb.must(hb.user(root), "PUT", "/v1/deployments/nina-dep/members/user:"+outsider, map[string]string{"role": "member"}, http.StatusNoContent, nil)
	hb.must(hb.user(outsider), "GET", "/v1/me", nil, http.StatusOK, nil)
}

// TestDeploymentNameCannotBeSquattedByNodeToken: a self-service
// deployment's ID is its node ID. Another owner's unreleased token for that
// ID would connect as the new deployment and be delivered its tasks and
// recipient Grainfiles, so creating it is refused (409) instead.
func TestDeploymentNameCannotBeSquattedByNodeToken(t *testing.T) {
	hb := startHub(t, options{})
	hb.teams()
	tok, _ := hb.mint("mallory@example.com", "payments-prod")

	hb.must(hb.user(alice), "POST", "/v1/deployments", map[string]string{"id": "payments-prod"}, http.StatusConflict, nil)
	hb.must(hb.user(alice), "GET", "/v1/deployments/payments-prod", nil, http.StatusNotFound, nil)
	if c, err := dial(t, hb, tok, "payments-prod"); err == nil {
		// The squatter's own node still connects, but as no deployment.
		c.Close()
	}

	// Creating a deployment over a node ID you hold yourself is fine.
	hb.mint(nina, "nina-host")
	hb.must(hb.user(nina), "POST", "/v1/deployments", map[string]string{"id": "nina-host"}, http.StatusCreated, nil)

	// Once the squatter releases the ID it can be created.
	hb.must(hb.user("mallory@example.com"), "DELETE", "/v1/node-tokens/nodes/payments-prod", nil, http.StatusNoContent, nil)
	hb.must(hb.user(alice), "POST", "/v1/deployments", map[string]string{"id": "payments-prod"}, http.StatusCreated, nil)
}

func (hb *hub) mintDeployment(who, id string) string {
	hb.t.Helper()
	var m struct {
		Token string `json:"token"`
	}
	hb.must(hb.user(who), "POST", "/v1/deployments/"+id+"/token", nil, http.StatusCreated, &m)
	return m.Token
}

func waitDisconnected(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(revokeBound):
		t.Fatalf("the connection outlived %s by %v", what, revokeBound)
	}
}

// TestLosingStandingRevokesNodeTokens: a node token is minted on its
// owner's standing (accepted user; admin of the deployment it is for).
// Removing that standing revokes the token and ends its connection.
func TestLosingStandingRevokesNodeTokens(t *testing.T) {
	hb := startHub(t, options{})
	hb.teams()

	t.Run("removed deployment admin", func(t *testing.T) {
		hb.must(hb.user(nina), "POST", "/v1/deployments", map[string]string{"id": "ops"}, http.StatusCreated, nil)
		hb.must(hb.user(nina), "PUT", "/v1/deployments/ops/members/user:"+dave, map[string]string{"role": "admin"}, http.StatusNoContent, nil)
		tok := hb.mintDeployment(dave, "ops")
		c, err := dial(t, hb, tok, "ops")
		if err != nil {
			t.Fatal(err)
		}
		hb.must(hb.user(nina), "DELETE", "/v1/deployments/ops/members/user:"+dave, nil, http.StatusNoContent, nil)
		waitDisconnected(t, c.Done(), "removing its owner from the deployment")
		if _, err := dial(t, hb, tok, "ops"); err == nil {
			t.Fatal("a removed admin's token connected again")
		}
	})

	t.Run("removed accepted user", func(t *testing.T) {
		tok, _ := hb.mint(pat, "pat-laptop")
		c, err := dial(t, hb, tok, "pat-laptop")
		if err != nil {
			t.Fatal(err)
		}
		hb.must(hb.user(root), "DELETE", "/v1/users/"+pat, nil, http.StatusNoContent, nil)
		waitDisconnected(t, c.Done(), "removing its owner from accepted users")
		if _, err := dial(t, hb, tok, "pat-laptop"); err == nil {
			t.Fatal("a removed user's token connected again")
		}
	})

	t.Run("deleted deployment", func(t *testing.T) {
		hb.must(hb.user(nina), "POST", "/v1/deployments", map[string]string{"id": "tmp-dep"}, http.StatusCreated, nil)
		tok := hb.mintDeployment(nina, "tmp-dep")
		c, err := dial(t, hb, tok, "tmp-dep")
		if err != nil {
			t.Fatal(err)
		}
		hb.must(hb.user(nina), "DELETE", "/v1/deployments/tmp-dep", nil, http.StatusNoContent, nil)
		waitDisconnected(t, c.Done(), "deleting the deployment")
		if _, err := dial(t, hb, tok, "tmp-dep"); err == nil {
			t.Fatal("a deleted deployment's token connected again")
		}
		// The name is free again, for anyone.
		hb.must(hb.user(alice), "POST", "/v1/deployments", map[string]string{"id": "tmp-dep"}, http.StatusCreated, nil)
	})
}

// TestSelfServiceRepoClaimRequiresProofOfAccess: any accepted user can create
// a self-service deployment and become its admin, so claiming an unowned
// repository via PUT /v1/{deployments|teams}/{id}/repos/{owner}/{name} must
// require proof of repository write access (.grain/hub-claim on the default
// branch naming the deployment). Without that proof, a self-service admin
// cannot claim victim-org/private-repo or read its Grainfiles and referenced
// files via GET /v1/grainfiles/content.
func TestSelfServiceRepoClaimRequiresProofOfAccess(t *testing.T) {
	const commitSHA = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	const secretGF = `---
grainfile: v1
kind: task
title: Internal migration
references:
  - /docs/internal.md
---
INTERNAL SECRET PLAN
`
	fetcher := &recordingFetcher{
		files: map[string][]byte{
			"https://github.com/victim-org/private-repo|tasks/secret.task.md|" + commitSHA: []byte(secretGF),
			"https://github.com/victim-org/private-repo|tasks/secret.task.md|":             []byte(secretGF),
			"https://github.com/victim-org/private-repo|docs/internal.md|" + commitSHA:     []byte("TOP SECRET DOC\n"),
			"https://github.com/victim-org/private-repo|docs/internal.md|":                 []byte("TOP SECRET DOC\n"),
		},
	}
	hb := startHub(t, options{grainfileFetcher: fetcher})
	hb.teams()

	// Index the private repo's Grainfile via signed push webhook (as happens when
	// the Grain Hub GitHub App is installed on victim-org/private-repo).
	whPayload, _ := jsonMarshal(map[string]any{
		"ref":   "refs/heads/main",
		"after": commitSHA,
		"repository": map[string]any{
			"html_url": "https://github.com/victim-org/private-repo",
		},
		"commits": []map[string]any{
			{
				"id":    commitSHA,
				"added": []string{"tasks/secret.task.md"},
			},
		},
	})
	whReq, _ := http.NewRequest("POST", hb.url+grainfiles.GitHubWebhookPath, bytes.NewReader(whPayload))
	whReq.Header.Set("Content-Type", "application/json")
	whReq.Header.Set("X-GitHub-Event", "push")
	whReq.Header.Set("X-Hub-Signature-256", signWebhook(webhookSecret, whPayload))
	whResp, err := http.DefaultClient.Do(whReq)
	if err != nil {
		t.Fatal(err)
	}
	whResp.Body.Close()
	if whResp.StatusCode != http.StatusOK {
		t.Fatalf("webhook status = %d, want 200", whResp.StatusCode)
	}

	// Accepted user nina creates a self-service deployment, mints a node token,
	// and registers subscriptions so only repository ownership gates content access.
	hb.must(hb.user(nina), "POST", "/v1/deployments", map[string]string{"id": "nina-dep"}, http.StatusCreated, nil)
	tokNina := hb.mintDeployment(nina, "nina-dep")
	if _, err := hb.h.Grainfiles.SetSubscriptions(context.Background(), grainfiles.Subscriptions{
		NodeID:     "nina-dep",
		Owner:      nina,
		Repos:      []string{"https://github.com/victim-org/private-repo"},
		Recipients: []string{"https://hub.test/deployments/nina-dep"},
	}); err != nil {
		t.Fatal(err)
	}

	gfURL := "https://github.com/victim-org/private-repo/blob/main/tasks/secret.task.md"
	gfQuery := url.Values{"grainfile_url": {gfURL}}.Encode()
	refQuery := url.Values{"grainfile_url": {gfURL}, "reference": {"/docs/internal.md"}}.Encode()

	// 1. Without .grain/hub-claim, nina cannot claim victim-org/private-repo via either route.
	hb.must(hb.user(nina), "PUT", "/v1/deployments/nina-dep/repos/victim-org/private-repo", nil, http.StatusForbidden, nil)
	hb.must(hb.user(nina), "PUT", "/v1/teams/nina-dep/repos/victim-org/private-repo", nil, http.StatusForbidden, nil)

	// nina's node cannot read the private repo's Grainfile or referenced content.
	if code, body, _ := hb.do(bearer(tokNina), "GET", grainfiles.ContentPath+"?"+gfQuery, nil); code != http.StatusForbidden || strings.Contains(string(body), "INTERNAL SECRET PLAN") {
		t.Fatalf("unclaimed grainfile fetch: got %d %q, want 403", code, body)
	}
	if code, body, _ := hb.do(bearer(tokNina), "GET", grainfiles.ContentPath+"?"+refQuery, nil); code != http.StatusForbidden || strings.Contains(string(body), "TOP SECRET DOC") {
		t.Fatalf("unclaimed reference fetch: got %d %q, want 403", code, body)
	}

	// 2. When .grain/hub-claim names a different deployment (victim-prod), nina is still refused.
	fetcher.mu.Lock()
	fetcher.files["https://github.com/victim-org/private-repo|.grain/hub-claim|"] = []byte("victim-prod\n")
	fetcher.mu.Unlock()
	hb.must(hb.user(nina), "PUT", "/v1/deployments/nina-dep/repos/victim-org/private-repo", nil, http.StatusForbidden, nil)
	if code, _, _ := hb.do(bearer(tokNina), "GET", grainfiles.ContentPath+"?"+gfQuery, nil); code != http.StatusForbidden {
		t.Fatalf("mismatched claim grainfile fetch: got %d, want 403", code)
	}

	// 3. Once .grain/hub-claim on the default branch names nina-dep, nina can claim the repo
	// and her deployment node is admitted to read its Grainfiles and references.
	fetcher.mu.Lock()
	fetcher.files["https://github.com/victim-org/private-repo|.grain/hub-claim|"] = []byte("# Authorized deployment\nnina-dep\n")
	fetcher.mu.Unlock()
	hb.must(hb.user(nina), "PUT", "/v1/deployments/nina-dep/repos/victim-org/private-repo", nil, http.StatusNoContent, nil)

	if code, body, _ := hb.do(bearer(tokNina), "GET", grainfiles.ContentPath+"?"+gfQuery, nil); code != http.StatusOK || !strings.Contains(string(body), "INTERNAL SECRET PLAN") {
		t.Fatalf("claimed grainfile fetch: got %d %q, want 200 with body", code, body)
	}
	if code, body, _ := hb.do(bearer(tokNina), "GET", grainfiles.ContentPath+"?"+refQuery, nil); code != http.StatusOK || !strings.Contains(string(body), "TOP SECRET DOC") {
		t.Fatalf("claimed reference fetch: got %d %q, want 200 with reference", code, body)
	}

	// 4. A Hub sys admin (root) may still assign an unowned repo without .grain/hub-claim.
	hb.must(hb.user(root), "PUT", "/v1/deployments/nina-dep/repos/victim-org/sysadmin-assigned", nil, http.StatusNoContent, nil)
}
