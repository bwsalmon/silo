package tasks

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/bwsalmon/silo/hub/directory"
	"github.com/bwsalmon/silo/hub/identity"
)

// TasksPath is the backlog's HTTP API.
const TasksPath = "/v1/tasks"

// API is the backlog over HTTP, for humans and custom UIs signed in
// through the team directory's API (docs/hub.md §6.1):
//
//	POST /v1/tasks {"repo" | "team", "title", "prompt", "constraints"}   submit a task
//	GET  /v1/tasks?state=queued,running&team=infra&limit=100            the tasks the caller sees, newest first
//	GET  /v1/tasks/{id}
//	POST /v1/tasks/{id}/cancel {"reason"}                               its submitter, a team admin or a hub admin
//	POST /v1/tasks/{id}/requeue                                         the same: a lost task back to the queue, or a queued one's exclusions cleared
//
// The caller submits only to a repository one of their teams owns, or
// to one of their teams, and sees only those teams' tasks. A task the
// caller may not see answers 404, as one that does not exist.
type API struct {
	Store      *Store
	Dispatcher *Dispatcher
	// Directory signs the caller in and says what they may see.
	Directory *directory.API
}

// Handler serves the API's paths. A write a browser sent from anywhere
// but the hub's own origin, or with a body that is not JSON, is refused
// (identity.SameOrigin).
func (a *API) Handler() http.Handler {
	m := http.NewServeMux()
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
	return identity.SameOrigin(m)
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

// sees is whether the caller may see t: a member of its team, or, for a
// repository's task, of the team that owns the repository now.
func sees(acc *directory.Access, t Task) bool {
	return acc.SeesTeam(t.Team) || (t.Repo != "" && acc.SeesRepo(t.Repo))
}

// manages is whether the caller may cancel or requeue t.
func manages(acc *directory.Access, t Task) bool {
	return strings.EqualFold(t.Submitter, acc.Email) || acc.AdminOf(t.Team)
}

func (a *API) submit(w http.ResponseWriter, r *http.Request, acc *directory.Access) error {
	var req struct {
		Repo        string      `json:"repo"`
		Team        string      `json:"team"`
		Title       string      `json:"title"`
		Prompt      string      `json:"prompt"`
		Constraints Constraints `json:"constraints"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	spec := Spec{Repo: req.Repo, Title: req.Title, Prompt: req.Prompt, Submitter: strings.ToLower(acc.Email), Constraints: req.Constraints}
	switch {
	case (req.Repo == "") == (req.Team == ""):
		return &httpError{http.StatusBadRequest, `want one target: "repo" or "team"`}
	case req.Repo != "":
		team, err := a.Directory.Store.RepoOwner(r.Context(), req.Repo)
		if err != nil {
			return err
		}
		// No team owns it: no node could be admitted to serve it.
		if team == "" || !acc.SeesRepo(req.Repo) {
			return &httpError{http.StatusNotFound, "no such repository in your teams: " + req.Repo}
		}
		spec.Team = team
	default:
		if !acc.SeesTeam(req.Team) {
			return &httpError{http.StatusNotFound, "no such team"}
		}
		if _, err := a.Directory.Store.TeamRepos(r.Context(), req.Team); err != nil {
			return err
		}
		spec.Team = req.Team
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
		f.Teams = []string{}
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
