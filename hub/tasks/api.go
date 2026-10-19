package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/events"
	"github.com/bwsalmon/silo/hub/directory"
	"github.com/bwsalmon/silo/hub/identity"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/route"
)

// TasksPath is the backlog's HTTP API, and InboxPath is the unified
// cross-deployment inbox HTTP API.
const (
	TasksPath = "/v1/tasks"
	InboxPath = "/v1/inbox"
)

// Actionable item kinds returned by GET /v1/inbox.
const (
	InboxAwaitingReply   = "awaiting_reply"
	InboxApprovalRequest = "approval_request"
	InboxProposal        = "proposal"
	InboxFailed          = "failed"
)

// API is the backlog over HTTP, for humans and custom UIs signed in
// through the team directory's API (docs/hub.md §6.1):
//
//	POST /v1/tasks {"repo" | "team", "kind", "title", "prompt", "constraints", "depends_on"}  submit a task; kind "interactive" for a session
//	GET  /v1/tasks?state=queued,running&team=infra&limit=100            the tasks the caller sees, newest first
//	GET  /v1/tasks/{id}
//	POST /v1/tasks/{id}/cancel {"reason"}                               its submitter, a team admin or a hub admin
//	POST /v1/tasks/{id}/requeue                                         the same: a lost task back to the queue, or a queued one's exclusions cleared
//	POST /v1/tasks/{id}/approve                                         a member of the task's team: a proposed task queued
//	POST /v1/tasks/{id}/reject {"reason"}                               the same: a proposed task turned down
//	GET  /v1/tasks/{id}/approvals                                       the answers to the task's approval requests
//	POST /v1/tasks/{id}/approvals/{request_id} {"decision", "reason"}   a member of the task's team: allow or deny one
//	GET  /v1/tasks/{id}/messages                                        the messages humans sent an interactive task
//	POST /v1/tasks/{id}/messages {"id", "text", "attachments"}          its submitter or a member of its team: send it one
//
// The caller submits to a repository one of their teams owns, or to one
// of their teams, and sees those teams' tasks. Any signed-in caller may
// also propose a task in a repository a team they are not in owns: it
// is filed proposed, and waits for a member of that team to approve it.
// The caller sees the tasks they filed, but not, unless they are in its
// team, a task's events or artifacts (hub/live). A task the caller may
// not see answers 404, as one that does not exist.
//
// A task may depend on others (depends_on, hub task IDs the caller
// sees): it is not placed until each of them is done (Spec.DependsOn).
//
// Approving, rejecting and answering an approval request are for the
// members of the task's own team, as user:<email> or through a group,
// in either role (member or admin): not a hub admin who is not in the
// team, nor a team given only the repository's history. A task's
// submitter may not approve or reject it, but may withdraw it (cancel).
//
// Sending an interactive task a message is for its submitter and the
// members of its team in either role (posts): not a hub admin outside
// the team, nor a team given only the repository's history, who may
// watch the session but not talk to it.
type API struct {
	Store      *Store
	Dispatcher *Dispatcher
	// Directory signs the caller in and says what they may see.
	Directory *directory.API
	// Requests finds a task's approval requests among its stored events:
	// *live.Store. Without it no approval request is answered.
	Requests ApprovalRequests
	// Events finds a task's awaiting_reply and approval_request events for
	// GET /v1/inbox: *live.Store. When nil, Requests is used if it also
	// implements InboxEvents.
	Events InboxEvents
	// Registry lists connected nodes for GET /v1/inbox.
	Registry *registry.Registry
	// NodeUIDomain, when set, populates node_ui_url on inbox items.
	NodeUIDomain string
}

// ApprovalRequests finds the approval_request event a task's node sent
// with an ID, if it sent one.
type ApprovalRequests interface {
	ApprovalRequest(ctx context.Context, task, id string) (events.ApprovalRequest, bool, error)
}

// InboxEvents inspects a task's kept awaiting_reply, message, and
// approval_request events for GET /v1/inbox: *live.Store.
type InboxEvents interface {
	TaskInboxState(ctx context.Context, task string) (awaiting bool, question string, awaitingAt time.Time, approvals []events.ApprovalRequest, approvalTimes []time.Time, err error)
}

// InboxItem is one actionable item returned by GET /v1/inbox across the
// caller's selected Grain deployments.
type InboxItem struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	TaskID     string    `json:"task_id"`
	NodeID     string    `json:"node_id,omitempty"`
	NodeUIURL  string    `json:"node_ui_url,omitempty"`
	Team       string    `json:"team"`
	Repo       string    `json:"repo,omitempty"`
	TaskKind   string    `json:"task_kind,omitempty"`
	Title      string    `json:"title"`
	State      State     `json:"state"`
	Summary    string    `json:"summary,omitempty"`
	Question   string    `json:"question,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Submitter  string    `json:"submitter,omitempty"`
	Proposer   string    `json:"proposer,omitempty"`
	ApprovalID string    `json:"approval_id,omitempty"`
	Tool       string    `json:"tool,omitempty"`
	ViewerRole string    `json:"viewer_role,omitempty"`
	CanAct     bool      `json:"can_act"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Handler serves the API's paths. A write a browser sent from anywhere
// but the hub's own origin, or with a body that is not JSON, is refused
// (identity.SameOrigin).
func (a *API) Handler() http.Handler {
	m := route.NewMux()
	h := func(pattern string, f func(http.ResponseWriter, *http.Request, *directory.Access) error) {
		m.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			acc, err := a.Directory.Access(r)
			if err != nil {
				http.Error(w, "unauthenticated", http.StatusUnauthorized)
				return
			}
			if err := f(w, r, acc); err != nil {
				writeError(w, err)
			}
		})
	}
	h("GET "+InboxPath, a.inbox)
	h("POST "+TasksPath, a.submit)
	h("GET "+TasksPath, a.list)
	h("GET "+TasksPath+"/{id}", a.get)
	h("POST "+TasksPath+"/{id}/cancel", a.cancel)
	h("POST "+TasksPath+"/{id}/requeue", a.requeue)
	h("POST "+TasksPath+"/{id}/approve", a.approve)
	h("POST "+TasksPath+"/{id}/reject", a.reject)
	h("GET "+TasksPath+"/{id}/approvals", a.answers)
	h("POST "+TasksPath+"/{id}/approvals/{request}", a.answer)
	h("GET "+TasksPath+"/{id}/messages", a.messages)
	h("POST "+TasksPath+"/{id}/messages", a.post)
	return route.Wrap(m, identity.SameOrigin(m))
}

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

var errNoTask = &httpError{http.StatusNotFound, "no such task"}

func writeError(w http.ResponseWriter, err error) {
	var he *httpError
	code := http.StatusInternalServerError
	switch {
	case errors.As(err, &he):
		code = he.code
	case errors.Is(err, ErrNotFound), errors.Is(err, directory.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, ErrState):
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
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxPrompt+1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &httpError{http.StatusBadRequest, "bad request body: " + err.Error()}
	}
	return nil
}

// sees is whether the caller may see t: its submitter, a member of its
// team, or, for a repository's task, of the team a hub admin transferred
// the repository to with its history (directory.Access.SeesTask).
func sees(acc *directory.Access, t Task) bool {
	return strings.EqualFold(t.Submitter, acc.Email) || acc.SeesTask(t.Team, t.Repo)
}

// member is whether the caller has role member or admin in t's team:
// who may approve or reject it, answer its approval requests, and send
// messages to its interactive sessions without having submitted it.
func member(acc *directory.Access, t Task) bool {
	r := acc.Teams[t.Team]
	return r == directory.Member || r == directory.Admin
}

// posts is whether the caller may send t a message: its submitter, or a
// member/admin of its team (directory.Access.Teams, which counts
// user:<email> and group membership alike).
func posts(acc *directory.Access, t Task) bool {
	return strings.EqualFold(t.Submitter, acc.Email) || member(acc, t)
}

// manages is whether the caller may cancel or requeue t.
func manages(acc *directory.Access, t Task) bool {
	return strings.EqualFold(t.Submitter, acc.Email) || acc.AdminOf(t.Team)
}

func (a *API) submit(w http.ResponseWriter, r *http.Request, acc *directory.Access) error {
	var req struct {
		Repo        string      `json:"repo"`
		Team        string      `json:"team"`
		Kind        string      `json:"kind"`
		Title       string      `json:"title"`
		Prompt      string      `json:"prompt"`
		Constraints Constraints `json:"constraints"`
		DependsOn   []string    `json:"depends_on"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	spec := Spec{Repo: req.Repo, Kind: req.Kind, Title: req.Title, Prompt: req.Prompt, Submitter: strings.ToLower(acc.Email), Constraints: req.Constraints}
	deps, err := dependencies(r.Context(), a.Store, acc, req.DependsOn)
	if err != nil {
		return err
	}
	spec.DependsOn = deps
	switch {
	case (req.Repo == "") == (req.Team == ""):
		return &httpError{http.StatusBadRequest, `want one target: "repo" or "team"`}
	case req.Repo != "":
		team, err := a.Directory.Store.RepoOwner(r.Context(), req.Repo)
		if err != nil {
			return err
		}
		// No team owns it: no node could be admitted to serve it.
		if team == "" {
			return &httpError{http.StatusNotFound, "no team owns the repository: " + req.Repo}
		}
		spec.Team = team
	default:
		if _, err := a.Directory.Store.TeamRepos(r.Context(), req.Team); err != nil {
			if !acc.SeesTeam(req.Team) && errors.Is(err, directory.ErrNotFound) {
				return &httpError{http.StatusNotFound, "no such team"}
			}
			return err
		}
		spec.Team = req.Team
	}
	settings, err := a.Directory.Store.TeamSettings(r.Context(), spec.Team)
	if err != nil {
		return err
	}
	role := acc.Teams[spec.Team]
	propose := false
	switch {
	case role == directory.Admin || role == directory.Member || acc.HubAdmin:
		propose = settings.RequireApproval
	case role == directory.Proposer:
		propose = true
	default:
		if !settings.AnyoneMayPropose {
			return &httpError{http.StatusForbidden, "team " + spec.Team + " does not accept task proposals from this user"}
		}
		if req.Repo == "" && !acc.SeesTeam(req.Team) {
			return &httpError{http.StatusNotFound, "no such team"}
		}
		propose = true
	}
	if propose {
		t, err := a.Store.Propose(r.Context(), spec)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusCreated, t)
	}
	t, err := a.Store.Create(r.Context(), spec)
	if err != nil {
		return err
	}
	if a.Dispatcher != nil {
		a.Dispatcher.Wake()
	}
	return writeJSON(w, http.StatusCreated, t)
}

func (a *API) list(w http.ResponseWriter, r *http.Request, acc *directory.Access) error {
	f := Filter{Limit: 100}
	q := r.URL.Query()
	if s := q.Get("state"); s != "" {
		for _, st := range strings.Split(s, ",") {
			if !State(st).valid() {
				return &httpError{http.StatusBadRequest, "no such state: " + st}
			}
			f.States = append(f.States, State(st))
		}
	}
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 1000 {
			return &httpError{http.StatusBadRequest, "limit: want 1-1000"}
		}
		f.Limit = n
	}
	if team := q.Get("team"); team != "" {
		f.Teams = []string{team}
	} else if !acc.HubAdmin {
		f.Teams, f.Submitter = []string{}, strings.ToLower(acc.Email)
		for team := range acc.Teams {
			f.Teams = append(f.Teams, team)
		}
	}
	ts, err := a.Store.List(r.Context(), f)
	if err != nil {
		return err
	}
	seen := []Task{}
	for _, t := range ts {
		if sees(acc, t) {
			seen = append(seen, t)
		}
	}
	return writeJSON(w, http.StatusOK, struct {
		Tasks []Task `json:"tasks"`
	}{seen})
}

// task loads the request's task, if the caller may see it.
func (a *API) task(r *http.Request, acc *directory.Access) (Task, error) {
	t, err := a.Store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrNotFound) || (err == nil && !sees(acc, t)) {
		return Task{}, errNoTask
	}
	return t, err
}

func (a *API) get(w http.ResponseWriter, r *http.Request, acc *directory.Access) error {
	t, err := a.task(r, acc)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, t)
}

func (a *API) cancel(w http.ResponseWriter, r *http.Request, acc *directory.Access) error {
	t, err := a.task(r, acc)
	if err != nil {
		return err
	}
	if !manages(acc, t) {
		return &httpError{http.StatusForbidden, "forbidden"}
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(r, &req); err != nil {
			return err
		}
	}
	if len(req.Reason) > maxReason {
		return &httpError{http.StatusBadRequest, fmt.Sprintf("the reason is over %d bytes", maxReason)}
	}
	reason := "cancelled by " + acc.Email
	if req.Reason != "" {
		reason += ": " + req.Reason
	}
	if a.Dispatcher != nil {
		t, err = a.Dispatcher.Cancel(r.Context(), t.ID, reason)
	} else {
		t, err = a.Store.Cancel(r.Context(), t.ID, reason)
	}
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, t)
}

func (a *API) requeue(w http.ResponseWriter, r *http.Request, acc *directory.Access) error {
	t, err := a.task(r, acc)
	if err != nil {
		return err
	}
	if !manages(acc, t) {
		return &httpError{http.StatusForbidden, "forbidden"}
	}
	if t, err = a.Store.Retry(r.Context(), t.ID); err != nil {
		return err
	}
	if a.Dispatcher != nil {
		a.Dispatcher.Wake()
	}
	return writeJSON(w, http.StatusOK, t)
}

func (a *API) approve(w http.ResponseWriter, r *http.Request, acc *directory.Access) error {
	t, err := a.task(r, acc)
	if err != nil {
		return err
	}
	if !member(acc, t) || strings.EqualFold(t.Submitter, acc.Email) {
		return &httpError{http.StatusForbidden, "forbidden: only a member of team " + t.Team + " other than its submitter may approve the task"}
	}
	if t, err = a.Store.Approve(r.Context(), t.ID, acc.Email); err != nil {
		return err
	}
	if a.Dispatcher != nil {
		a.Dispatcher.Wake()
	}
	return writeJSON(w, http.StatusOK, t)
}

func (a *API) reject(w http.ResponseWriter, r *http.Request, acc *directory.Access) error {
	t, err := a.task(r, acc)
	if err != nil {
		return err
	}
	if !member(acc, t) || strings.EqualFold(t.Submitter, acc.Email) {
		return &httpError{http.StatusForbidden, "forbidden: only a member of team " + t.Team + " other than its submitter may reject the task"}
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(r, &req); err != nil {
			return err
		}
	}
	if len(req.Reason) > maxReason {
		return &httpError{http.StatusBadRequest, fmt.Sprintf("the reason is over %d bytes", maxReason)}
	}
	reason := "rejected by " + strings.ToLower(acc.Email)
	if req.Reason != "" {
		reason += ": " + req.Reason
	}
	if t, err = a.Store.Reject(r.Context(), t.ID, reason); err != nil {
		return err
	}
	if a.Dispatcher != nil {
		a.Dispatcher.finished(t)
	}
	return writeJSON(w, http.StatusOK, t)
}

func (a *API) answers(w http.ResponseWriter, r *http.Request, acc *directory.Access) error {
	t, err := a.task(r, acc)
	if err != nil {
		return err
	}
	as, err := a.Store.Answers(r.Context(), t.ID, false)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, struct {
		Approvals []Answer `json:"approvals"`
	}{as})
}

// answer is a member of the task's team answering one of its approval
// requests: one its node sent, of a task still running, before the
// request's deadline, and only once. Anything else is refused as stale.
func (a *API) answer(w http.ResponseWriter, r *http.Request, acc *directory.Access) error {
	t, err := a.task(r, acc)
	if err != nil {
		return err
	}
	if !member(acc, t) {
		return &httpError{http.StatusForbidden, "forbidden: only a member of team " + t.Team + " may answer the task's approval requests"}
	}
	var req struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if req.Decision != Allow && req.Decision != Deny {
		return &httpError{http.StatusBadRequest, `decision: want "allow" or "deny"`}
	}
	if len(req.Reason) > maxReason {
		return &httpError{http.StatusBadRequest, fmt.Sprintf("the reason is over %d bytes", maxReason)}
	}
	id := r.PathValue("request")
	if a.Requests == nil {
		return &httpError{http.StatusNotFound, "no such approval request"}
	}
	ar, ok, err := a.Requests.ApprovalRequest(r.Context(), t.ID, id)
	if err != nil {
		return err
	}
	if !ok {
		return &httpError{http.StatusNotFound, "no such approval request"}
	}
	if t.State != Running && t.State != Offered {
		return &httpError{http.StatusConflict, "stale approval request: task " + t.ID + " is " + string(t.State)}
	}
	if ar.Deadline != nil && time.Now().After(*ar.Deadline) {
		return &httpError{http.StatusConflict, "stale approval request: its deadline has passed"}
	}
	ans, err := a.Store.Answer(r.Context(), Answer{Task: t.ID, Request: id, Decision: req.Decision, AnsweredBy: strings.ToLower(acc.Email), Reason: req.Reason})
	if err != nil {
		return err
	}
	if a.Dispatcher != nil {
		a.Dispatcher.Answered(r.Context(), t.ID)
	}
	return writeJSON(w, http.StatusOK, ans)
}

func (a *API) messages(w http.ResponseWriter, r *http.Request, acc *directory.Access) error {
	t, err := a.task(r, acc)
	if err != nil {
		return err
	}
	ms, err := a.Store.Messages(r.Context(), t.ID, false)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, struct {
		Messages []TaskMessage `json:"messages"`
	}{ms})
}

// post is the task's submitter or a member of its team sending an
// interactive task a message, which the hub keeps and sends its node.
// A message to a task that has not started yet, or whose node is away,
// is kept and sent when the node next accepts the task; one to a task
// that is over, or still a proposal, is refused (409), as is one to a
// task that is not interactive. A post retried with the same id is
// answered with the message already kept (200), not sent again.
func (a *API) post(w http.ResponseWriter, r *http.Request, acc *directory.Access) error {
	t, err := a.task(r, acc)
	if err != nil {
		return err
	}
	if !posts(acc, t) {
		return &httpError{http.StatusForbidden, "forbidden: only the task's submitter or a member of team " + t.Team + " may send it a message"}
	}
	var req struct {
		ID          string              `json:"id"`
		Text        string              `json:"text"`
		Attachments []events.Attachment `json:"attachments"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	switch {
	case !takesMessages(t):
		return &httpError{http.StatusConflict, "task " + t.ID + " is not interactive: it takes no messages"}
	case t.State.Terminal() || t.State == Proposed:
		return &httpError{http.StatusConflict, "task " + t.ID + " is " + string(t.State) + ": it takes no messages"}
	}
	m, dup, err := a.Store.Post(r.Context(), TaskMessage{Task: t.ID, ID: req.ID, Text: req.Text, Attachments: req.Attachments, Sender: acc.Email})
	if err != nil {
		return err
	}
	if dup {
		return writeJSON(w, http.StatusOK, m)
	}
	if a.Dispatcher != nil {
		a.Dispatcher.Posted(r.Context(), t.ID)
	}
	return writeJSON(w, http.StatusCreated, m)
}

func (a *API) inbox(w http.ResponseWriter, r *http.Request, acc *directory.Access) error {
	ctx := r.Context()
	kindFilter := map[string]bool{}
	for _, raw := range r.URL.Query()["kind"] {
		for _, part := range strings.Split(raw, ",") {
			k := strings.TrimSpace(part)
			if k == "" {
				continue
			}
			switch k {
			case InboxAwaitingReply, InboxApprovalRequest, InboxProposal, InboxFailed:
				kindFilter[k] = true
			default:
				return &httpError{http.StatusBadRequest, "bad inbox kind: " + k}
			}
		}
	}
	kindAllowed := func(k string) bool {
		return len(kindFilter) == 0 || kindFilter[k]
	}

	reg := a.Registry
	if reg == nil && a.Directory != nil {
		reg = a.Directory.Registry
	}
	if reg == nil && a.Dispatcher != nil {
		reg = a.Dispatcher.Registry
	}
	var entries []registry.Entry
	if reg != nil {
		entries = reg.List()
	}

	allByID := map[string]registry.Entry{}
	resolvedByID := map[string]registry.Entry{}
	for _, raw := range entries {
		allByID[raw.ID] = raw
		if res, ok := acc.ResolveEntry(raw); ok {
			resolvedByID[res.ID] = res
		}
	}

	var selectedNodes []string
	var hasExplicitFilter bool
	if r.URL.Query().Has("nodes") {
		hasExplicitFilter = true
		seen := map[string]bool{}
		for _, raw := range r.URL.Query()["nodes"] {
			for _, part := range strings.Split(raw, ",") {
				id := strings.TrimSpace(part)
				if id == "" || seen[id] {
					continue
				}
				if _, exists := allByID[id]; exists {
					if _, visible := resolvedByID[id]; !visible {
						continue
					}
				}
				seen[id] = true
				selectedNodes = append(selectedNodes, id)
			}
		}
	} else {
		nodes, configured, err := acc.EffectiveInboxNodes(ctx, entries)
		if err != nil {
			return err
		}
		hasExplicitFilter = configured
		seen := map[string]bool{}
		for _, id := range nodes {
			if seen[id] {
				continue
			}
			if _, exists := allByID[id]; exists {
				if _, visible := resolvedByID[id]; !visible {
					continue
				}
			}
			seen[id] = true
			selectedNodes = append(selectedNodes, id)
		}
	}
	if selectedNodes == nil {
		selectedNodes = []string{}
	}
	if hasExplicitFilter && len(selectedNodes) == 0 {
		return writeJSON(w, http.StatusOK, struct {
			Items []InboxItem `json:"items"`
			Nodes []string    `json:"nodes"`
		}{Items: []InboxItem{}, Nodes: selectedNodes})
	}

	selectedSet := map[string]bool{}
	for _, id := range selectedNodes {
		selectedSet[id] = true
	}

	var f Filter
	if !acc.HubAdmin {
		f.Submitter = strings.ToLower(acc.Email)
		for team := range acc.Teams {
			f.Teams = append(f.Teams, team)
		}
	}
	f.States = []State{Running, Proposed, Failed, Lost}
	ts, err := a.Store.List(ctx, f)
	if err != nil {
		return err
	}

	ie := a.Events
	if ie == nil {
		ie, _ = a.Requests.(InboxEvents)
	}

	nodeServesTask := func(e registry.Entry, t Task) bool {
		if t.Constraints.Node != "" {
			return e.ID == t.Constraints.Node
		}
		if t.Repo != "" {
			for _, rp := range e.Repos {
				if strings.EqualFold(rp, t.Repo) {
					return true
				}
			}
		}
		if t.Team != "" {
			if e.UI != nil && strings.EqualFold(e.UI.Team, t.Team) {
				return true
			}
			if a.Directory != nil && a.Directory.Store != nil {
				for _, rp := range e.Repos {
					if ownerTeam, err := a.Directory.Store.RepoOwner(ctx, rp); err == nil && strings.EqualFold(ownerTeam, t.Team) {
						return true
					}
				}
			}
		}
		return false
	}

	nodeUIURLFor := func(nodeID string) string {
		if nodeID == "" {
			return ""
		}
		if res, ok := resolvedByID[nodeID]; ok {
			if res.UI != nil && res.UI.URL != "" {
				return res.UI.URL
			}
			if a.NodeUIDomain != "" && ((res.UI != nil && res.UI.Offered) || res.Supports(registry.FeatureNodeUI) || strings.EqualFold(res.Owner, acc.Email)) {
				return "https://" + nodeID + "." + a.NodeUIDomain
			}
		}
		if a.NodeUIDomain != "" {
			return "https://" + nodeID + "." + a.NodeUIDomain
		}
		return ""
	}

	items := []InboxItem{}
	for _, t := range ts {
		if !sees(acc, t) {
			continue
		}
		nodeID := t.Node
		if nodeID == "" && t.Constraints.Node != "" {
			nodeID = t.Constraints.Node
		}
		if nodeID != "" {
			if len(selectedSet) > 0 || hasExplicitFilter {
				if !selectedSet[nodeID] {
					continue
				}
			}
		} else {
			for _, id := range selectedNodes {
				if res, ok := resolvedByID[id]; ok && nodeServesTask(res, t) {
					nodeID = id
					break
				}
			}
			if nodeID == "" && (hasExplicitFilter || len(entries) > 0) {
				continue
			}
		}

		viewerRole := ""
		if res, ok := resolvedByID[nodeID]; ok && res.ViewerRole != "" {
			viewerRole = res.ViewerRole
		} else if t.Team != "" {
			viewerRole = string(acc.Teams[t.Team])
		}
		uiURL := nodeUIURLFor(nodeID)

		switch t.State {
		case Running:
			if ie == nil {
				continue
			}
			awaiting, question, awaitingAt, approvals, approvalTimes, err := ie.TaskInboxState(ctx, t.ID)
			if err != nil {
				return err
			}
			if awaiting && kindAllowed(InboxAwaitingReply) {
				msgs, err := a.Store.Messages(ctx, t.ID, false)
				if err != nil {
					return err
				}
				if len(msgs) > 0 {
					lastMsg := msgs[len(msgs)-1]
					if awaitingAt.IsZero() || !lastMsg.SentAt.Before(awaitingAt) {
						awaiting = false
					}
				}
				if awaiting {
					summary := question
					if summary == "" {
						summary = "Interactive session is waiting for a reply"
					}
					stamp := awaitingAt
					if stamp.IsZero() {
						stamp = t.Updated
					}
					items = append(items, InboxItem{
						ID:         t.ID + ":awaiting_reply",
						Kind:       InboxAwaitingReply,
						TaskID:     t.ID,
						NodeID:     nodeID,
						NodeUIURL:  uiURL,
						Team:       t.Team,
						Repo:       t.Repo,
						TaskKind:   t.Kind,
						Title:      t.Title,
						State:      t.State,
						Summary:    summary,
						Question:   question,
						Submitter:  t.Submitter,
						ViewerRole: viewerRole,
						CanAct:     posts(acc, t),
						UpdatedAt:  stamp,
					})
				}
			}
			if len(approvals) > 0 && kindAllowed(InboxApprovalRequest) {
				ans, err := a.Store.Answers(ctx, t.ID, false)
				if err != nil {
					return err
				}
				answered := map[string]bool{}
				for _, an := range ans {
					answered[an.Request] = true
				}
				now := time.Now()
				for i, ar := range approvals {
					if answered[ar.ID] {
						continue
					}
					if ar.Deadline != nil && now.After(*ar.Deadline) {
						continue
					}
					summary := ar.Message
					if summary == "" {
						summary = "Tool " + ar.Tool + " requires approval"
					}
					stamp := approvalTimes[i]
					if stamp.IsZero() {
						stamp = t.Updated
					}
					items = append(items, InboxItem{
						ID:         t.ID + ":approval:" + ar.ID,
						Kind:       InboxApprovalRequest,
						TaskID:     t.ID,
						NodeID:     nodeID,
						NodeUIURL:  uiURL,
						Team:       t.Team,
						Repo:       t.Repo,
						TaskKind:   t.Kind,
						Title:      t.Title,
						State:      t.State,
						Summary:    summary,
						Submitter:  t.Submitter,
						ApprovalID: ar.ID,
						Tool:       ar.Tool,
						ViewerRole: viewerRole,
						CanAct:     member(acc, t),
						UpdatedAt:  stamp,
					})
				}
			}
		case Proposed:
			if kindAllowed(InboxProposal) {
				items = append(items, InboxItem{
					ID:         t.ID + ":proposal",
					Kind:       InboxProposal,
					TaskID:     t.ID,
					NodeID:     nodeID,
					NodeUIURL:  uiURL,
					Team:       t.Team,
					Repo:       t.Repo,
					TaskKind:   t.Kind,
					Title:      t.Title,
					State:      t.State,
					Summary:    t.Prompt,
					Submitter:  t.Submitter,
					Proposer:   t.Submitter,
					ViewerRole: viewerRole,
					CanAct:     member(acc, t) && !strings.EqualFold(t.Submitter, acc.Email),
					UpdatedAt:  t.Updated,
				})
			}
		case Failed, Lost:
			if kindAllowed(InboxFailed) {
				summary := t.Reason
				if summary == "" {
					if t.State == Lost {
						summary = "Node connection lost"
					} else {
						summary = "Task failed"
					}
				}
				items = append(items, InboxItem{
					ID:         t.ID + ":failed",
					Kind:       InboxFailed,
					TaskID:     t.ID,
					NodeID:     nodeID,
					NodeUIURL:  uiURL,
					Team:       t.Team,
					Repo:       t.Repo,
					TaskKind:   t.Kind,
					Title:      t.Title,
					State:      t.State,
					Summary:    summary,
					Reason:     t.Reason,
					Submitter:  t.Submitter,
					ViewerRole: viewerRole,
					CanAct:     manages(acc, t),
					UpdatedAt:  t.Updated,
				})
			}
		}
	}

	slices.SortFunc(items, func(a, b InboxItem) int {
		if c := b.UpdatedAt.Compare(a.UpdatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})

	return writeJSON(w, http.StatusOK, struct {
		Items []InboxItem `json:"items"`
		Nodes []string    `json:"nodes"`
	}{Items: items, Nodes: selectedNodes})
}
