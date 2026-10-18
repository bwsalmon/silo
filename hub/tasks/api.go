package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/events"
	"github.com/bwsalmon/silo/hub/directory"
	"github.com/bwsalmon/silo/hub/identity"
	"github.com/bwsalmon/silo/hub/route"
)

// TasksPath is the backlog's HTTP API.
const TasksPath = "/v1/tasks"

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
}

// ApprovalRequests finds the approval_request event a task's node sent
// with an ID, if it sent one.
type ApprovalRequests interface {
	ApprovalRequest(ctx context.Context, task, id string) (events.ApprovalRequest, bool, error)
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
	if t.State != Running {
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
