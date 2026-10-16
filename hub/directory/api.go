package directory

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/bwsalmon/silo/hub/identity"
	"github.com/bwsalmon/silo/hub/registry"
)

// Paths the API serves.
const (
	// MePath answers the caller's principal, teams and roles.
	MePath = "/v1/me"
	// TeamsPath lists teams (the caller's own, or all for a hub admin)
	// and, under it, manages one team's members and repositories.
	TeamsPath = "/v1/teams"
	// NodeTokensPath mints, lists and revokes node tokens.
	NodeTokensPath = "/v1/node-tokens"
)

// API is the team directory over HTTP, for humans signed in through
// Auth (docs/hub.md §5). Everything it answers is filtered by the
// caller's teams:
//
//	GET    /v1/me
//	GET    /v1/teams                                  the caller's teams; every team for a hub admin
//	POST   /v1/teams {"id","name"}                    hub admin
//	DELETE /v1/teams/{team}                           hub admin
//	PUT    /v1/teams/{team}/members/{principal} {"role"}   team admin
//	DELETE /v1/teams/{team}/members/{principal}       team admin
//	PUT    /v1/teams/{team}/repos/{owner}/{name} [{"history"}]   hub admin
//	DELETE /v1/teams/{team}/repos/{owner}/{name}      team admin
//	GET    /v1/teams/{team}/settings                  its members
//	PUT    /v1/teams/{team}/settings {"lost_requeue_after_seconds"}   team admin
//	GET    /v1/node-tokens                            the caller's; every owner's for a hub admin
//	POST   /v1/node-tokens {"node"}                   mint one for the caller: grain hub login
//	DELETE /v1/node-tokens/{id}                       its owner, or a hub admin
//	DELETE /v1/node-tokens/nodes/{node}               release the node ID: the caller's claim, or every owner's for a hub admin
//
// A team the caller is not in answers 404, as one that does not exist.
type API struct {
	Store *Store
	Auth  identity.Authenticator
	// HubAdmins are the principals, user:<email> or group:<name>, who
	// hold the hub-wide admin role: they create teams and see
	// everything. An entry with no prefix is a user
	// (HubAdminPrincipal).
	HubAdmins []string
	// Disconnect, when set, ends a node's connection when its token is
	// revoked: rendezvous.Server.Disconnect.
	Disconnect func(nodeID string) bool
	// Unbind, when set, is called once a node ID has been given up, so
	// that the node registry forgets the owner it bound the ID to:
	// registry.Registry.Unbind.
	Unbind func(nodeID string) error
	// Logf, when set, receives the audit lines of changes to which team
	// owns what: deleting a team, assigning and releasing a repository.
	Logf func(format string, args ...any)
}

// audit logs one change to the directory, with the principal who made
// it (O-10).
func (a *API) audit(acc *Access, format string, args ...any) {
	if a.Logf != nil {
		a.Logf("directory: audit: %s "+format, append([]any{acc.Email}, args...)...)
	}
}

// Access authenticates r and loads what its caller may see.
func (a *API) Access(r *http.Request) (*Access, error) {
	p, err := a.Auth.Principal(r)
	if err != nil {
		return nil, err
	}
	return a.Store.Access(r.Context(), p, a.isHubAdmin(p))
}

// isHubAdmin matches each HubAdmins entry only against its own claim:
// a user: entry against the email, a group: entry against the groups.
func (a *API) isHubAdmin(p identity.Principal) bool {
	for _, adm := range a.HubAdmins {
		adm, _ = HubAdminPrincipal(adm)
		if u, err := userPrincipal(p.Email); err == nil && adm == u {
			return true
		}
		for _, g := range p.Groups {
			if adm == GroupPrefix+strings.ToLower(strings.TrimSpace(g)) {
				return true
			}
		}
	}
	return false
}

// Visible is rendezvous.Server.Visible: the node directory, filtered to
// the nodes the caller may see.
func (a *API) Visible(r *http.Request) (func(registry.Entry) bool, error) {
	acc, err := a.Access(r)
	if err != nil {
		return nil, err
	}
	return acc.SeesNode, nil
}

// Handler serves the API's paths.
func (a *API) Handler() http.Handler {
	m := http.NewServeMux()
	h := func(pattern string, f func(http.ResponseWriter, *http.Request, *Access) error) {
		m.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			acc, err := a.Access(r)
			if err != nil {
				http.Error(w, "unauthenticated", http.StatusUnauthorized)
				return
			}
			if err := f(w, r, acc); err != nil {
				writeError(w, err)
			}
		})
	}
	h("GET "+MePath, a.me)
	h("GET "+TeamsPath, a.listTeams)
	h("POST "+TeamsPath, a.createTeam)
	h("DELETE "+TeamsPath+"/{team}", a.deleteTeam)
	h("PUT "+TeamsPath+"/{team}/members/{principal}", a.setMember)
	h("DELETE "+TeamsPath+"/{team}/members/{principal}", a.removeMember)
	h("PUT "+TeamsPath+"/{team}/repos/{owner}/{name}", a.assignRepo)
	h("DELETE "+TeamsPath+"/{team}/repos/{owner}/{name}", a.releaseRepo)
	h("GET "+TeamsPath+"/{team}/settings", a.getSettings)
	h("PUT "+TeamsPath+"/{team}/settings", a.setSettings)
	h("GET "+NodeTokensPath, a.listTokens)
	h("POST "+NodeTokensPath, a.mintToken)
	h("DELETE "+NodeTokensPath+"/{id}", a.revokeToken)
	h("DELETE "+NodeTokensPath+"/nodes/{node}", a.releaseNodeID)
	return m
}

// httpError is an error with its status.
type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

var (
	errForbidden = &httpError{http.StatusForbidden, "forbidden"}
	errNoTeam    = &httpError{http.StatusNotFound, "no such team"}
)

func writeError(w http.ResponseWriter, err error) {
	var he *httpError
	code := http.StatusInternalServerError
	switch {
	case errors.As(err, &he):
		code = he.code
	case errors.Is(err, ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, ErrExists), errors.Is(err, ErrOwned), errors.Is(err, ErrNodeOwner):
		code = http.StatusConflict
	case errors.Is(err, ErrInvalid):
		code = http.StatusBadRequest
	}
	http.Error(w, err.Error(), code)
}

func writeJSON(w http.ResponseWriter, code int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	return json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &httpError{http.StatusBadRequest, "bad request body: " + err.Error()}
	}
	return nil
}

// teamAdmin checks the caller manages the request's team.
func teamAdmin(r *http.Request, acc *Access) (string, error) {
	team := r.PathValue("team")
	if !acc.SeesTeam(team) {
		return "", errNoTeam
	}
	if !acc.AdminOf(team) {
		return "", errForbidden
	}
	return team, nil
}

func (a *API) me(w http.ResponseWriter, r *http.Request, acc *Access) error {
	return writeJSON(w, http.StatusOK, struct {
		identity.Principal
		HubAdmin bool            `json:"hub_admin"`
		Teams    map[string]Role `json:"teams"`
	}{acc.Principal, acc.HubAdmin, acc.Teams})
}

func (a *API) listTeams(w http.ResponseWriter, r *http.Request, acc *Access) error {
	all, err := a.Store.Teams(r.Context())
	if err != nil {
		return err
	}
	teams := []Team{}
	for _, t := range all {
		if acc.SeesTeam(t.ID) {
			teams = append(teams, t)
		}
	}
	return writeJSON(w, http.StatusOK, struct {
		Teams []Team `json:"teams"`
	}{teams})
}

func (a *API) createTeam(w http.ResponseWriter, r *http.Request, acc *Access) error {
	if !acc.HubAdmin {
		return errForbidden
	}
	var req struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if err := a.Store.CreateTeam(r.Context(), req.ID, req.Name); err != nil {
		return err
	}
	w.WriteHeader(http.StatusCreated)
	return nil
}

func (a *API) deleteTeam(w http.ResponseWriter, r *http.Request, acc *Access) error {
	if !acc.HubAdmin {
		if acc.SeesTeam(r.PathValue("team")) {
			return errForbidden
		}
		return errNoTeam
	}
	if err := a.Store.DeleteTeam(r.Context(), r.PathValue("team")); err != nil {
		return err
	}
	a.audit(acc, "deleted team %s, leaving its repositories unowned", r.PathValue("team"))
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) getSettings(w http.ResponseWriter, r *http.Request, acc *Access) error {
	team := r.PathValue("team")
	if !acc.SeesTeam(team) {
		return errNoTeam
	}
	set, err := a.Store.TeamSettings(r.Context(), team)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, set)
}

func (a *API) setSettings(w http.ResponseWriter, r *http.Request, acc *Access) error {
	team, err := teamAdmin(r, acc)
	if err != nil {
		return err
	}
	var set Settings
	if err := readJSON(r, &set); err != nil {
		return err
	}
	if err := a.Store.SetTeamSettings(r.Context(), team, set); err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, set)
}

func (a *API) setMember(w http.ResponseWriter, r *http.Request, acc *Access) error {
	team, err := teamAdmin(r, acc)
	if err != nil {
		return err
	}
	var req struct {
		Role Role `json:"role"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if err := a.Store.SetMember(r.Context(), team, r.PathValue("principal"), req.Role); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) removeMember(w http.ResponseWriter, r *http.Request, acc *Access) error {
	team, err := teamAdmin(r, acc)
	if err != nil {
		return err
	}
	if err := a.Store.RemoveMember(r.Context(), team, r.PathValue("principal")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// assignRepo is a hub admin's: a team admin who could assign
// repositories could claim any one no team owned at that moment, one
// released for a transfer or left by a deleted team, and with it the
// work of its nodes and the rightful team's 409 (SA1-M2). {"history":
// true} also gives the team the tasks earlier teams filed in it
// (Store.TransferRepo); without it, they stay with those teams.
func (a *API) assignRepo(w http.ResponseWriter, r *http.Request, acc *Access) error {
	team := r.PathValue("team")
	if !acc.HubAdmin {
		if acc.SeesTeam(team) {
			return errForbidden
		}
		return errNoTeam
	}
	var req struct {
		History bool `json:"history"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(r, &req); err != nil {
			return err
		}
	}
	repo := r.PathValue("owner") + "/" + r.PathValue("name")
	assign := a.Store.AssignRepo
	if req.History {
		assign = a.Store.TransferRepo
	}
	if err := assign(r.Context(), team, repo); err != nil {
		return err
	}
	a.audit(acc, "assigned repository %s to team %s (history: %t)", strings.ToLower(repo), team, req.History)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) releaseRepo(w http.ResponseWriter, r *http.Request, acc *Access) error {
	team, err := teamAdmin(r, acc)
	if err != nil {
		return err
	}
	repo := r.PathValue("owner") + "/" + r.PathValue("name")
	if err := a.Store.ReleaseRepo(r.Context(), team, repo); err != nil {
		return err
	}
	a.audit(acc, "released repository %s from team %s", strings.ToLower(repo), team)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) listTokens(w http.ResponseWriter, r *http.Request, acc *Access) error {
	owner := acc.Email
	if acc.HubAdmin {
		owner = ""
	}
	toks, err := a.Store.NodeTokens(r.Context(), owner)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, struct {
		NodeTokens []NodeToken `json:"node_tokens"`
	}{toks})
}

func (a *API) mintToken(w http.ResponseWriter, r *http.Request, acc *Access) error {
	var req struct {
		Node string `json:"node"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	tok, rec, revoked, err := a.Store.mintNodeToken(r.Context(), acc.Email, req.Node)
	if err != nil {
		return err
	}
	// Minting again is how a stolen token is recovered from, so the
	// connection an earlier token holds ends with it, as on revoke.
	if revoked > 0 && a.Disconnect != nil {
		a.Disconnect(rec.Node)
	}
	w.Header().Set("Cache-Control", "no-store")
	return writeJSON(w, http.StatusCreated, struct {
		Token     string    `json:"token"`
		NodeToken NodeToken `json:"node_token"`
	}{tok, rec})
}

func (a *API) revokeToken(w http.ResponseWriter, r *http.Request, acc *Access) error {
	t, err := a.Store.NodeToken(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	if !acc.HubAdmin && !strings.EqualFold(t.Owner, acc.Email) {
		return &httpError{http.StatusNotFound, "no such node token"}
	}
	if err := a.Store.RevokeNodeToken(r.Context(), t.ID); err != nil {
		return err
	}
	if a.Disconnect != nil {
		a.Disconnect(t.Node)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// releaseNodeID gives up a node ID, so that another owner may mint a
// token for it (Store.ReleaseNodeID). The node, if connected with one of
// the tokens it revokes, is disconnected.
func (a *API) releaseNodeID(w http.ResponseWriter, r *http.Request, acc *Access) error {
	owner, node := acc.Email, r.PathValue("node")
	if acc.HubAdmin {
		owner = ""
	}
	if err := a.Store.ReleaseNodeID(r.Context(), owner, node); err != nil {
		return err
	}
	if a.Disconnect != nil {
		a.Disconnect(node)
	}
	if a.Unbind != nil {
		if err := a.Unbind(node); err != nil {
			return err
		}
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
