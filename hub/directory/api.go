package directory

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/bwsalmon/silo/hub/identity"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/route"
)

// Paths the API serves.
const (
	// MePath answers the caller's principal, teams and roles.
	MePath = "/v1/me"
	// TeamsPath lists teams (the caller's own, or all for a hub admin)
	// and, under it, manages one team's members and repositories.
	TeamsPath = "/v1/teams"
	// DeploymentsPath is the unified deployments API (/v1/deployments),
	// backed by the same store as TeamsPath (/v1/teams).
	DeploymentsPath = "/v1/deployments"
	// UsersPath manages the Hub's accepted users list and dynamic sys admins.
	UsersPath = "/v1/users"
	// NodeTokensPath mints, lists and revokes node tokens.
	NodeTokensPath = "/v1/node-tokens"
)

// API is the team/deployment directory over HTTP, for humans signed in
// through Auth (docs/hub.md §5). Everything it answers is filtered by the
// caller's deployments/teams.
type API struct {
	Store    *Store
	Registry *registry.Registry
	Auth     identity.Authenticator
	// HubAdmins are the principals, user:<email> or group:<name>, who
	// hold the hub-wide admin role: they create teams and see
	// everything. An entry with no prefix is a user
	// (HubAdminPrincipal).
	HubAdmins []string
	// EnforceAcceptedUsers, when true, requires every signing-in principal
	// to be present in accepted_users (or match HubAdmins).
	EnforceAcceptedUsers bool
	// HubURL, when set, is the canonical base URL of the Hub used to construct
	// canonical deployment URLs (https://<hub-host>/deployments/<id>).
	HubURL string
	// Disconnect, when set, ends a node's connection when its token is
	// revoked: rendezvous.Server.Disconnect.
	Disconnect func(nodeID string) bool
	// Unbind, when set, is called once a node ID has been given up, so
	// that the node registry forgets the owner it bound the ID to:
	// registry.Registry.Unbind.
	Unbind func(nodeID string) error
	// ReclaimTasks, when set, immediately transitions any in-flight tasks
	// on nodeID back to queued when its host is replaced.
	ReclaimTasks func(ctx context.Context, nodeID, reason string) ([]string, error)
	// ReplayGrainfiles, when set, resets nodeID's Grainfile delivery cursor
	// and replays active standing processes and tasks when its host is replaced.
	ReplayGrainfiles func(ctx context.Context, nodeID string) (int, error)
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

// AdminModeHeader is the optional request header ("on" or "off") a hub
// admin's client sends to toggle hub-wide admin privileges on that
// request. When omitted, a configured hub admin keeps HubAdmin true.
const AdminModeHeader = "X-Silo-Admin-Mode"

// WriteAccessError writes 403 Forbidden when err is ErrNotAcceptedUser
// and 401 Unauthorized for any other authentication error.
func WriteAccessError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotAcceptedUser) {
		http.Error(w, ErrNotAcceptedUser.Error(), http.StatusForbidden)
		return
	}
	http.Error(w, "unauthenticated", http.StatusUnauthorized)
}

// Access authenticates r and loads what its caller may see.
func (a *API) Access(r *http.Request) (*Access, error) {
	p, err := a.Auth.Principal(r)
	if err != nil {
		return nil, err
	}
	cfgAdmin := a.isHubAdmin(p)
	var dbSysAdmin bool
	if a.Store != nil {
		accepted, sysAdmin, hasAnyUsers, err := a.Store.CheckAcceptedUser(r.Context(), p.Email)
		if err != nil {
			return nil, err
		}
		if a.Store.EnforceAcceptedUsers() {
			cfgAdmin = a.isHubAdminGroup(p)
		}
		enforce := hasAnyUsers || a.Store.EnforceAcceptedUsers() || a.EnforceAcceptedUsers
		if enforce && !accepted && !cfgAdmin {
			return nil, ErrNotAcceptedUser
		}
		dbSysAdmin = sysAdmin
	}
	canAdmin := cfgAdmin || dbSysAdmin
	acc, err := a.Store.Access(r.Context(), p, canAdmin)
	if err != nil {
		return nil, err
	}
	acc.CanHubAdmin = canAdmin
	if canAdmin && adminModeOff(r.Header.Get(AdminModeHeader)) {
		acc.HubAdmin = false
	}
	return acc, nil
}

func (a *API) disconnectAll(nodes []string) {
	if a.Disconnect == nil {
		return
	}
	for _, n := range nodes {
		a.Disconnect(n)
	}
}

func adminModeOff(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "false", "0":
		return true
	default:
		return false
	}
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

// IsAcceptedUser reports whether email is an accepted Hub user (present in
// accepted_users, a configured HubAdmin, or, when accepted_users is empty and
// not strictly enforced, a member of at least one deployment/team).
func (a *API) IsAcceptedUser(ctx context.Context, email string) (bool, error) {
	email = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(email)), UserPrefix)
	if email == "" || !strings.Contains(email, "@") {
		return false, nil
	}
	if a.isHubAdmin(identity.Principal{Email: email}) {
		return true, nil
	}
	if a.Store == nil {
		return true, nil
	}
	accepted, _, hasAnyUsers, err := a.Store.CheckAcceptedUser(ctx, email)
	if err != nil {
		return false, err
	}
	if accepted {
		return true, nil
	}
	enforce := hasAnyUsers || a.Store.EnforceAcceptedUsers() || a.EnforceAcceptedUsers
	if enforce {
		return false, nil
	}
	acc, err := a.Store.Access(ctx, identity.Principal{Email: email}, false)
	if err != nil {
		return false, err
	}
	return len(acc.Teams) > 0, nil
}

// AccessForEmail resolves the Access record for a verified email address
// (e.g. from a Google Chat event), enforcing accepted_users and resolving
// HubAdmin / sys_admin status.
func (a *API) AccessForEmail(ctx context.Context, email string) (*Access, error) {
	email = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(email)), UserPrefix)
	if email == "" || !strings.Contains(email, "@") {
		return nil, ErrNotAcceptedUser
	}
	ok, err := a.IsAcceptedUser(ctx, email)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotAcceptedUser
	}
	p := identity.Principal{Email: email}
	cfgAdmin := a.isHubAdmin(p)
	var dbSysAdmin bool
	if a.Store != nil {
		_, sysAdmin, _, err := a.Store.CheckAcceptedUser(ctx, email)
		if err != nil {
			return nil, err
		}
		if a.Store.EnforceAcceptedUsers() {
			cfgAdmin = a.isHubAdminGroup(p)
		}
		dbSysAdmin = sysAdmin
	}
	canAdmin := cfgAdmin || dbSysAdmin
	if a.Store == nil {
		return &Access{
			Principal:   p,
			HubAdmin:    canAdmin,
			CanHubAdmin: canAdmin,
			Teams:       map[string]Role{},
		}, nil
	}
	acc, err := a.Store.Access(ctx, p, canAdmin)
	if err != nil {
		return nil, err
	}
	acc.CanHubAdmin = canAdmin
	return acc, nil
}

func (a *API) isHubAdminGroup(p identity.Principal) bool {
	for _, adm := range a.HubAdmins {
		adm, _ = HubAdminPrincipal(adm)
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

// ResolveEntries is rendezvous.Server.ResolveEntries: the node directory,
// filtered to the nodes the caller may see and annotated with viewer_role
// and require_approval.
func (a *API) ResolveEntries(r *http.Request) (func(registry.Entry) (registry.Entry, bool), error) {
	acc, err := a.Access(r)
	if err != nil {
		return nil, err
	}
	return acc.ResolveEntry, nil
}

// Handler serves the API's paths. A write a browser sent from anywhere
// but the hub's own origin, or with a body that is not JSON, is refused
// before it is signed in (identity.SameOrigin).
func (a *API) Handler() http.Handler {
	m := route.NewMux()
	h := func(pattern string, f func(http.ResponseWriter, *http.Request, *Access) error) {
		m.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			acc, err := a.Access(r)
			if err != nil {
				WriteAccessError(w, err)
				return
			}
			if err := f(w, r, acc); err != nil {
				writeError(w, err)
			}
		})
	}
	h("GET "+MePath, a.me)
	h("PUT "+MePath+"/theme", a.setMyTheme)
	h("GET "+MePath+"/inbox", a.getMyInbox)
	h("PUT "+MePath+"/inbox", a.setMyInbox)
	h("GET "+UsersPath, a.listUsers)
	h("PUT "+UsersPath+"/{email}", a.setUser)
	h("DELETE "+UsersPath+"/{email}", a.deleteUser)
	h("GET "+TeamsPath, a.listTeams)
	h("POST "+TeamsPath, a.createTeam)
	h("GET "+TeamsPath+"/{team}", a.getTeam)
	h("DELETE "+TeamsPath+"/{team}", a.deleteTeam)
	h("PUT "+TeamsPath+"/{team}/members/{principal}", a.setMember)
	h("DELETE "+TeamsPath+"/{team}/members/{principal}", a.removeMember)
	h("PUT "+TeamsPath+"/{team}/repos/{owner}/{name}", a.assignRepo)
	h("DELETE "+TeamsPath+"/{team}/repos/{owner}/{name}", a.releaseRepo)
	h("GET "+TeamsPath+"/{team}/settings", a.getSettings)
	h("PUT "+TeamsPath+"/{team}/settings", a.setSettings)
	h("GET "+DeploymentsPath, a.listTeams)
	h("POST "+DeploymentsPath, a.createTeam)
	h("GET "+DeploymentsPath+"/{id}", a.getTeam)
	h("DELETE "+DeploymentsPath+"/{id}", a.deleteTeam)
	h("PUT "+DeploymentsPath+"/{id}/members/{principal}", a.setMember)
	h("DELETE "+DeploymentsPath+"/{id}/members/{principal}", a.removeMember)
	h("PUT "+DeploymentsPath+"/{id}/repos/{owner}/{name}", a.assignRepo)
	h("DELETE "+DeploymentsPath+"/{id}/repos/{owner}/{name}", a.releaseRepo)
	h("GET "+DeploymentsPath+"/{id}/settings", a.getSettings)
	h("PUT "+DeploymentsPath+"/{id}/settings", a.setSettings)
	h("POST "+DeploymentsPath+"/{id}/token", a.mintDeploymentToken)
	h("POST "+DeploymentsPath+"/{id}/replace-host", a.replaceDeploymentHost)
	h("GET "+NodeTokensPath, a.listTokens)
	h("POST "+NodeTokensPath, a.mintToken)
	h("DELETE "+NodeTokensPath+"/{id}", a.revokeToken)
	h("DELETE "+NodeTokensPath+"/nodes/{node}", a.releaseNodeID)
	return route.Wrap(m, identity.SameOrigin(m))
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
	case errors.Is(err, ErrExists), errors.Is(err, ErrOwned), errors.Is(err, ErrNodeOwner), errors.Is(err, ErrLastSysAdmin):
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

func teamParam(r *http.Request) string {
	if id := r.PathValue("id"); id != "" {
		return id
	}
	return r.PathValue("team")
}

func (a *API) effectiveHubURL(r *http.Request) string {
	if u := strings.TrimRight(strings.TrimSpace(a.HubURL), "/"); u != "" {
		return u
	}
	if r != nil && strings.TrimSpace(r.Host) != "" {
		return "https://" + strings.TrimSpace(r.Host)
	}
	return ""
}

func (a *API) decorateTeam(r *http.Request, t Team) Team {
	u := CanonicalDeploymentURL(a.effectiveHubURL(r), t.ID)
	t.URL = u
	t.RecipientURL = u
	return t
}

// teamAdmin checks the caller manages the request's team / deployment.
func teamAdmin(r *http.Request, acc *Access) (string, error) {
	team := teamParam(r)
	if !acc.SeesTeam(team) {
		return "", errNoTeam
	}
	if !acc.AdminOf(team) {
		return "", errForbidden
	}
	return team, nil
}

func (a *API) listUsers(w http.ResponseWriter, r *http.Request, acc *Access) error {
	if !acc.HubAdmin {
		return errForbidden
	}
	users, err := a.Store.Users(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, struct {
		Users []AcceptedUser `json:"users"`
	}{Users: users})
}

func (a *API) setUser(w http.ResponseWriter, r *http.Request, acc *Access) error {
	if !acc.HubAdmin {
		return errForbidden
	}
	var req struct {
		SysAdmin bool `json:"sys_admin"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(r, &req); err != nil {
			return err
		}
	}
	rawEmail := r.PathValue("email")
	if err := a.Store.SetUser(r.Context(), rawEmail, req.SysAdmin, acc.Email); err != nil {
		return err
	}
	norm, _ := normEmail(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(rawEmail)), UserPrefix))
	a.audit(acc, "added/updated accepted user %s (sys_admin: %t)", norm, req.SysAdmin)
	users, err := a.Store.Users(r.Context())
	if err == nil {
		for _, u := range users {
			if u.Email == norm {
				return writeJSON(w, http.StatusOK, u)
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) deleteUser(w http.ResponseWriter, r *http.Request, acc *Access) error {
	if !acc.HubAdmin {
		return errForbidden
	}
	rawEmail := r.PathValue("email")
	if err := a.Store.DeleteUser(r.Context(), rawEmail); err != nil {
		return err
	}
	norm, _ := normEmail(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(rawEmail)), UserPrefix))
	a.audit(acc, "removed accepted user %s", norm)
	// A user removed from the accepted users list may no longer reach the
	// Hub at all, including through the node tokens they minted.
	nodes, err := a.Store.RevokeOwnerTokens(r.Context(), norm, "")
	if err != nil {
		return err
	}
	a.disconnectAll(nodes)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) me(w http.ResponseWriter, r *http.Request, acc *Access) error {
	prefs, err := a.Store.UserPreferences(r.Context(), acc.Email)
	if err != nil {
		return err
	}
	inboxNodes := prefs.InboxNodes
	if !prefs.InboxConfigured {
		var entries []registry.Entry
		if a.Registry != nil {
			entries = a.Registry.List()
		}
		inboxNodes = acc.DefaultInboxNodes(entries)
	}
	if inboxNodes == nil {
		inboxNodes = []string{}
	}
	return writeJSON(w, http.StatusOK, struct {
		identity.Principal
		HubAdmin        bool            `json:"hub_admin"`
		CanHubAdmin     bool            `json:"can_hub_admin"`
		Teams           map[string]Role `json:"teams"`
		Theme           string          `json:"theme"`
		InboxNodes      []string        `json:"inbox_nodes"`
		InboxConfigured bool            `json:"inbox_configured"`
	}{
		Principal:       acc.Principal,
		HubAdmin:        acc.HubAdmin,
		CanHubAdmin:     acc.CanHubAdmin,
		Teams:           acc.Teams,
		Theme:           prefs.Theme,
		InboxNodes:      inboxNodes,
		InboxConfigured: prefs.InboxConfigured,
	})
}

func (a *API) setMyTheme(w http.ResponseWriter, r *http.Request, acc *Access) error {
	var req struct {
		Theme string `json:"theme"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	theme, err := a.Store.SetUserTheme(r.Context(), acc.Email, req.Theme)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, struct {
		Theme string `json:"theme"`
	}{Theme: theme})
}

func (a *API) getMyInbox(w http.ResponseWriter, r *http.Request, acc *Access) error {
	var entries []registry.Entry
	if a.Registry != nil {
		entries = a.Registry.List()
	}
	nodes, configured, err := acc.EffectiveInboxNodes(r.Context(), entries)
	if err != nil {
		return err
	}
	if nodes == nil {
		nodes = []string{}
	}
	return writeJSON(w, http.StatusOK, struct {
		Nodes      []string `json:"nodes"`
		Configured bool     `json:"configured"`
	}{Nodes: nodes, Configured: configured})
}

func (a *API) setMyInbox(w http.ResponseWriter, r *http.Request, acc *Access) error {
	var req struct {
		Nodes []string `json:"nodes"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	for _, rawID := range req.Nodes {
		id := strings.TrimSpace(rawID)
		if id == "" {
			continue
		}
		allowed := false
		if a.Registry != nil {
			if e, ok := a.Registry.Get(id); ok {
				if resolved, visible := acc.ResolveEntry(e); visible && (resolved.ViewerRole != "" || acc.HubAdmin) {
					allowed = true
				}
			}
		}
		if !allowed && a.Store != nil {
			if toks, err := a.Store.NodeTokens(r.Context(), acc.Email); err == nil {
				for _, t := range toks {
					if t.Node == id && t.Revoked == nil && t.Released == nil {
						allowed = true
						break
					}
				}
			}
		}
		if !allowed {
			return errForbidden
		}
	}
	saved, err := a.Store.SetUserInboxNodes(r.Context(), acc.Email, req.Nodes)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, struct {
		Nodes      []string `json:"nodes"`
		Configured bool     `json:"configured"`
	}{Nodes: saved, Configured: true})
}

func (a *API) listTeams(w http.ResponseWriter, r *http.Request, acc *Access) error {
	all, err := a.Store.Teams(r.Context())
	if err != nil {
		return err
	}
	teams := []Team{}
	for _, t := range all {
		if acc.SeesTeam(t.ID) {
			teams = append(teams, a.decorateTeam(r, t))
		}
	}
	return writeJSON(w, http.StatusOK, struct {
		Teams       []Team `json:"teams"`
		Deployments []Team `json:"deployments"`
	}{Teams: teams, Deployments: teams})
}

// getTeam answers one team / deployment, as listTeams would: its members, roles and
// repositories, to anyone who sees it.
func (a *API) getTeam(w http.ResponseWriter, r *http.Request, acc *Access) error {
	id := teamParam(r)
	if !acc.SeesTeam(id) {
		return errNoTeam
	}
	all, err := a.Store.Teams(r.Context())
	if err != nil {
		return err
	}
	for _, t := range all {
		if t.ID == id {
			return writeJSON(w, http.StatusOK, a.decorateTeam(r, t))
		}
	}
	return errNoTeam
}

func (a *API) createTeam(w http.ResponseWriter, r *http.Request, acc *Access) error {
	var req struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	creator := acc.Email
	if acc.HubAdmin && strings.HasSuffix(r.URL.Path, "/teams") {
		creator = ""
	}
	if err := a.Store.CreateDeployment(r.Context(), req.ID, req.Name, creator); err != nil {
		return err
	}
	a.audit(acc, "created deployment %s", req.ID)
	name := req.Name
	if name == "" {
		name = req.ID
	}
	members := map[string]Role{}
	if strings.TrimSpace(creator) != "" {
		members[UserPrefix+strings.ToLower(strings.TrimSpace(creator))] = Admin
	}
	return writeJSON(w, http.StatusCreated, a.decorateTeam(r, Team{
		ID:      req.ID,
		Name:    name,
		Members: members,
		Repos:   []string{},
	}))
}

func (a *API) deleteTeam(w http.ResponseWriter, r *http.Request, acc *Access) error {
	id, err := teamAdmin(r, acc)
	if err != nil {
		return err
	}
	if err := a.Store.DeleteTeam(r.Context(), id); err != nil {
		return err
	}
	// The deleted deployment's node tokens go with it: no former admin's
	// host stays connected as it, and the name may be created again.
	if n, err := a.Store.ReleaseNodeTokens(r.Context(), id); err != nil {
		return err
	} else if n > 0 {
		a.disconnectAll([]string{id})
		if a.Unbind != nil {
			_ = a.Unbind(id)
		}
	}
	a.audit(acc, "deleted team %s, leaving its repositories unowned", id)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) getSettings(w http.ResponseWriter, r *http.Request, acc *Access) error {
	team := teamParam(r)
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
	if err := a.requireAcceptedPrincipal(r.Context(), acc, r.PathValue("principal")); err != nil {
		return err
	}
	if err := a.Store.SetMember(r.Context(), team, r.PathValue("principal"), req.Role); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// errNotAcceptedMember refuses a deployment admin adding a user who is not
// on the Hub's accepted users list. Store.SetMember admits a new user:
// member into accepted_users so that a sys admin can build a deployment in
// one step; without this check any accepted user could create a
// self-service deployment and admit arbitrary outsiders to the Hub through
// it, bypassing the sys-admin-only /v1/users allowlist.
var errNotAcceptedMember = &httpError{http.StatusForbidden, "forbidden: principal is not on the hub accepted users list; a hub sys admin must add them first"}

// requireAcceptedPrincipal is nil when acc may make principal a member of
// a deployment: acc is an active hub admin, principal is a group, the Hub
// does not enforce its accepted users list, or principal is already on it
// (or is a configured hub admin).
func (a *API) requireAcceptedPrincipal(ctx context.Context, acc *Access, principal string) error {
	if acc.HubAdmin || a.Store == nil {
		return nil
	}
	p, err := normPrincipal(principal)
	if err != nil {
		return err
	}
	email, isUser := strings.CutPrefix(p, UserPrefix)
	if !isUser {
		return nil
	}
	if a.isHubAdmin(identity.Principal{Email: email}) {
		return nil
	}
	accepted, _, hasAnyUsers, err := a.Store.CheckAcceptedUser(ctx, email)
	if err != nil {
		return err
	}
	if accepted || !(hasAnyUsers || a.Store.EnforceAcceptedUsers() || a.EnforceAcceptedUsers) {
		return nil
	}
	return errNotAcceptedMember
}

func (a *API) removeMember(w http.ResponseWriter, r *http.Request, acc *Access) error {
	team, err := teamAdmin(r, acc)
	if err != nil {
		return err
	}
	if err := a.Store.RemoveMember(r.Context(), team, r.PathValue("principal")); err != nil {
		return err
	}
	// A user removed from a deployment loses the standing any node token
	// they minted for its node was issued on: revoke it and end the
	// connection, so a former admin's host does not stay connected as the
	// deployment and keep receiving its tasks and Grainfiles.
	if p, err := normPrincipal(r.PathValue("principal")); err == nil {
		if email, ok := strings.CutPrefix(p, UserPrefix); ok {
			nodes, err := a.Store.RevokeOwnerTokens(r.Context(), email, team)
			if err != nil {
				return err
			}
			a.disconnectAll(nodes)
			if len(nodes) > 0 {
				a.audit(acc, "removed %s from deployment %s and revoked their node tokens for it", p, team)
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) assignRepo(w http.ResponseWriter, r *http.Request, acc *Access) error {
	team, err := teamAdmin(r, acc)
	if err != nil {
		return err
	}
	var req struct {
		History bool `json:"history"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(r, &req); err != nil {
			return err
		}
	}
	if req.History && !acc.HubAdmin {
		return errForbidden
	}
	repo := r.PathValue("owner") + "/" + r.PathValue("name")
	allowedSourceTeams := map[string]bool{}
	for t, role := range acc.Teams {
		if role == Admin {
			allowedSourceTeams[t] = true
		}
	}
	if err := a.Store.TransferRepoByAdmin(r.Context(), team, repo, req.History, allowedSourceTeams, false); err != nil {
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
		Node        string `json:"node"`
		ReplaceHost bool   `json:"replace_host"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	hasTeam, err := a.Store.HasTeam(r.Context(), req.Node)
	if err != nil {
		return err
	}
	if hasTeam && !acc.AdminOf(req.Node) {
		return ErrNodeOwner
	}
	forceOwner := acc.AdminOf(req.Node)
	return a.issueNodeToken(w, r, acc, req.Node, forceOwner, req.ReplaceHost)
}

func (a *API) mintDeploymentToken(w http.ResponseWriter, r *http.Request, acc *Access) error {
	id, err := teamAdmin(r, acc)
	if err != nil {
		return err
	}
	hasTeam, err := a.Store.HasTeam(r.Context(), id)
	if err != nil {
		return err
	}
	if !hasTeam {
		return errNoTeam
	}
	var req struct {
		ReplaceHost bool `json:"replace_host"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(r, &req); err != nil {
			return err
		}
	}
	return a.issueNodeToken(w, r, acc, id, true, req.ReplaceHost)
}

func (a *API) replaceDeploymentHost(w http.ResponseWriter, r *http.Request, acc *Access) error {
	id, err := teamAdmin(r, acc)
	if err != nil {
		return err
	}
	hasTeam, err := a.Store.HasTeam(r.Context(), id)
	if err != nil {
		return err
	}
	if !hasTeam {
		return errNoTeam
	}
	if r.ContentLength != 0 {
		var req struct{}
		if err := readJSON(r, &req); err != nil {
			return err
		}
	}
	return a.issueNodeToken(w, r, acc, id, true, true)
}

func (a *API) issueNodeToken(w http.ResponseWriter, r *http.Request, acc *Access, node string, forceOwner, replaceHost bool) error {
	tok, rec, revoked, err := a.Store.MintDeploymentToken(r.Context(), acc.Email, node, forceOwner)
	if err != nil {
		return err
	}
	if (revoked > 0 || forceOwner || replaceHost) && a.Disconnect != nil {
		a.Disconnect(rec.Node)
	}
	if (forceOwner || replaceHost) && a.Unbind != nil {
		_ = a.Unbind(rec.Node)
	}
	var reclaimed []string
	var replayed int
	if replaceHost {
		if a.ReclaimTasks != nil {
			reclaimed, err = a.ReclaimTasks(r.Context(), rec.Node, "deployment host replaced")
			if err != nil {
				return err
			}
		}
		if a.ReplayGrainfiles != nil {
			replayed, err = a.ReplayGrainfiles(r.Context(), rec.Node)
			if err != nil {
				return err
			}
		}
		a.audit(acc, "replaced host for deployment %s (reclaimed_tasks: %d, replayed_events: %d)", rec.Node, len(reclaimed), replayed)
	}
	w.Header().Set("Cache-Control", "no-store")
	return writeJSON(w, http.StatusCreated, struct {
		Token          string    `json:"token"`
		NodeToken      NodeToken `json:"node_token"`
		ReplacedHost   bool      `json:"replaced_host,omitempty"`
		ReclaimedTasks []string  `json:"reclaimed_tasks,omitempty"`
		ReplayedEvents int       `json:"replayed_events,omitempty"`
	}{
		Token:          tok,
		NodeToken:      rec,
		ReplacedHost:   replaceHost,
		ReclaimedTasks: reclaimed,
		ReplayedEvents: replayed,
	})
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
