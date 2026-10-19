package security_test

import (
	"net/http"
	"testing"
	"time"
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
