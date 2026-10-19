package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/hub/directory"
	"github.com/bwsalmon/silo/hub/identity"
	"github.com/bwsalmon/silo/hub/registry"
)

// Filing is a task a node files at the hub (rendezvous TypeFileTask,
// docs/hub-interface.md): a task its agent proposed for a repository or
// team its own Grain does not serve, which the hub places as it does a
// human's.
type Filing struct {
	// Repo or Team is the task's target, as in POST /v1/tasks: exactly
	// one of them.
	Repo string `json:"repo,omitempty"`
	Team string `json:"team,omitempty"`
	// Title and Prompt are what to do.
	Title  string `json:"title,omitempty"`
	Prompt string `json:"prompt"`
	// Node, when set, is the filing node's ID, which must be the one its
	// connection was registered as: the hub records the filer from the
	// connection, never from the filing.
	Node string `json:"node,omitempty"`
	// OriginTask is the hub task whose run is filing this one, if the
	// node is filing from a hub task: one the node is running now. The
	// new task's submitter is that task's.
	OriginTask string `json:"origin_task,omitempty"`
	// DependsOn are hub tasks the new task waits for (Spec.DependsOn),
	// each one the node's owner may see.
	DependsOn []string `json:"depends_on,omitempty"`
	// NodeTask, when set, is the node's own ID for a task of its own
	// that waits on the new one: the hub tells the node when the new
	// task ends (TypeDependencyDone).
	NodeTask string `json:"node_task,omitempty"`
}

// Defaults for the Filer's rate limit.
const (
	DefaultFileBurst = 10
	DefaultFileEvery = time.Minute
)

// ErrRateLimited is a node filing faster than the Filer allows.
var ErrRateLimited = errors.New("tasks: filing too fast")

// Filer files the tasks nodes file at the hub (Filing): the hub's end of
// a Grain's propose_task for work another team owns. File is
// rendezvous.Server.FileTask.
//
// The hub decides a filed task's team and whether it is proposed, as
// for a human's submission (docs/hub.md §6.1), and never takes either
// from the filing. Its submitter is the human the work is for: the
// submitter of the hub task it was filed from (Filing.OriginTask), or,
// for a filing from no hub task, the node's owner. The node and its
// owner are recorded as its filer (Spec.FiledBy). The task is queued
// only if both its submitter and the node's owner are members of the
// team it is for, and proposed otherwise, for a member of that team to
// approve; an approval the origin task carried is never carried over.
// So an agent cannot launder approval by filing through a Grain: a
// task it files is approved for a team only by that team's members, as
// any task is, and its submitter, who may have asked for the work that
// led to it, is not one who may approve it.
//
// Membership is by user:<email>, as a node's admission is: the hub does
// not know the groups of a human who is not signed in, so one who is in
// the team only through a group sees their agent's filings proposed.
type Filer struct {
	Store     *Store
	Directory *directory.Store
	Registry  *registry.Registry
	// LookupGrainfile, when set, returns known repository, kind, title, and
	// commit SHA metadata for a registered Grainfile URL.
	LookupGrainfile func(ctx context.Context, url string) (repo, kind, title, commitSHA string, ok bool)
	// Dispatcher, when set, is woken for a task filed queued.
	Dispatcher *Dispatcher
	// Burst and Every are the rate limit on each node's filings: Burst
	// at once, and one more each Every; DefaultFileBurst and
	// DefaultFileEvery when zero. Filings refused count, as they cost
	// the hub as much to decide. The limit is per hub pod, which holds
	// a node's one connection: a node that reconnects to another pod
	// starts afresh there.
	Burst int
	Every time.Duration
	// Logf, when set, receives one line per task filed.
	Logf func(format string, args ...any)
	// OnFiled, when set, is called after a task is filed.
	OnFiled func(t Task)
	// Now, when set, is the rate limit's clock.
	Now func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

func (f *Filer) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *Filer) limits() (burst int, every time.Duration) {
	burst, every = f.Burst, f.Every
	if burst <= 0 {
		burst = DefaultFileBurst
	}
	if every <= 0 {
		every = DefaultFileEvery
	}
	return burst, every
}

// allow takes one of node's tokens, if it has one.
func (f *Filer) allow(node string) bool {
	n, every := f.limits()
	burst := float64(n)
	now := f.now()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.buckets == nil {
		f.buckets = map[string]*bucket{}
	}
	b := f.buckets[node]
	if b == nil {
		b = &bucket{tokens: burst, at: now}
		f.buckets[node] = b
	}
	b.tokens = min(burst, b.tokens+float64(now.Sub(b.at))/float64(every))
	b.at = now
	// A bucket full again is forgotten, so the map holds only the nodes
	// filing lately.
	for id, o := range f.buckets {
		if id != node && o.tokens+float64(now.Sub(o.at))/float64(every) >= burst {
			delete(f.buckets, id)
		}
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// File files the task filing (a Filing, as JSON) describes, for nodeID,
// registered by owner, and returns it as JSON.
func (f *Filer) File(ctx context.Context, nodeID, owner string, filing json.RawMessage) (json.RawMessage, error) {
	var req Filing
	dec := json.NewDecoder(bytes.NewReader(filing))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return nil, fmt.Errorf("%w filing: %v", ErrInvalid, err)
	}
	if req.Node != "" && req.Node != nodeID {
		return nil, fmt.Errorf("%w filing: it names node %q, but comes from %q", ErrInvalid, req.Node, nodeID)
	}
	if !f.allow(nodeID) {
		burst, every := f.limits()
		return nil, fmt.Errorf("%w: node %s may file %d tasks at once and one more every %s", ErrRateLimited, nodeID, burst, every)
	}
	t, err := f.file(ctx, nodeID, owner, req)
	if err != nil {
		return nil, err
	}
	return json.Marshal(t)
}

func (f *Filer) access(ctx context.Context, email string) (*directory.Access, error) {
	return f.Directory.Access(ctx, identity.Principal{Email: email}, false)
}

func (f *Filer) file(ctx context.Context, nodeID, owner string, req Filing) (Task, error) {
	if req.NodeTask != "" && (len(req.NodeTask) > maxNodeTask || strings.ContainsAny(req.NodeTask, "\r\n")) {
		return Task{}, fmt.Errorf("%w node task %q", ErrInvalid, req.NodeTask)
	}
	ownerEmail := strings.ToLower(strings.TrimPrefix(owner, directory.UserPrefix))
	ownerAcc, err := f.access(ctx, ownerEmail)
	if err != nil {
		return Task{}, err
	}
	submitter, subAcc := ownerEmail, ownerAcc
	if req.OriginTask != "" {
		o, err := f.Store.Get(ctx, req.OriginTask)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return Task{}, err
		}
		// Only a task this node runs now, as its owner, may stand behind
		// a filing; anything else is answered as if it did not exist.
		if err != nil || o.State != Running || o.Node != nodeID || o.NodeOwner != owner {
			return Task{}, fmt.Errorf("%w origin task %s: not one this node is running", ErrInvalid, req.OriginTask)
		}
		submitter = o.Submitter
		if subAcc, err = f.access(ctx, submitter); err != nil {
			return Task{}, err
		}
	}
	spec := Spec{
		Repo: req.Repo, Title: req.Title, Prompt: req.Prompt, Submitter: submitter,
		FiledBy: nodeID, FiledByOwner: owner, OriginTask: req.OriginTask,
	}
	propose := false
	switch {
	case (req.Repo == "") == (req.Team == ""):
		return Task{}, fmt.Errorf(`%w filing: want one target, "repo" or "team"`, ErrInvalid)
	case req.Repo != "":
		team, err := f.Directory.RepoOwner(ctx, req.Repo)
		if err != nil {
			return Task{}, err
		}
		if team == "" {
			return Task{}, fmt.Errorf("%w: no team owns the repository %s", ErrInvalid, req.Repo)
		}
		propose = !subAcc.MemberOf(team) || !ownerAcc.MemberOf(team)
		if settings, sErr := f.Directory.TeamSettings(ctx, team); sErr == nil {
			if propose && !settings.AnyoneMayPropose && subAcc.Teams[team] != directory.Proposer {
				return Task{}, fmt.Errorf("%w: team %s does not accept proposals from %s", ErrInvalid, team, submitter)
			}
			if !propose && settings.RequireApproval {
				propose = true
			}
		}
		spec.Team = team
	default:
		linkedAllowed := false
		if req.OriginTask != "" {
			if chat, ok, _ := f.Store.GetChatByTask(ctx, req.OriginTask); ok && slices.Contains(chat.LinkedDeployments, req.Team) {
				if exists, _ := f.Directory.HasTeam(ctx, req.Team); exists {
					linkedAllowed = true
				}
			}
		}
		if !subAcc.MemberOf(req.Team) || !ownerAcc.MemberOf(req.Team) {
			if !linkedAllowed {
				return Task{}, fmt.Errorf("%w: no such team %s, or its submitter and the node's owner are not both in it", ErrInvalid, req.Team)
			}
			propose = true
		}
		if settings, sErr := f.Directory.TeamSettings(ctx, req.Team); sErr == nil && settings.RequireApproval {
			propose = true
		}
		spec.Team = req.Team
	}
	if spec.DependsOn, err = dependencies(ctx, f.Store, ownerAcc, req.DependsOn); err != nil {
		return Task{}, err
	}
	var t Task
	if propose {
		t, err = f.Store.Propose(ctx, spec)
	} else {
		t, err = f.Store.Create(ctx, spec)
	}
	if err != nil {
		return Task{}, err
	}
	if req.NodeTask != "" {
		if err := f.Store.Watch(ctx, Watcher{Task: t.ID, Node: nodeID, NodeOwner: owner, NodeTask: req.NodeTask}); err != nil {
			return Task{}, fmt.Errorf("tasks: filed %s, but could not record that %s waits on it: %w", t.ID, req.NodeTask, err)
		}
	}
	if f.Logf != nil {
		f.Logf("tasks: node %s of %s filed %s for team %s on behalf of %s, %s", nodeID, owner, t.ID, t.Team, t.Submitter, t.State)
	}
	if t.State == Queued && f.Dispatcher != nil {
		f.Dispatcher.Wake()
	}
	if f.OnFiled != nil {
		f.OnFiled(t)
	}
	return t, nil
}

// dependencies checks a new task's dependencies: each must be a task
// acc may see. One that is not is refused as one that does not exist.
func dependencies(ctx context.Context, s *Store, acc *directory.Access, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > maxDepends {
		return nil, fmt.Errorf("%w: over %d dependencies", ErrInvalid, maxDepends)
	}
	for _, id := range ids {
		if !taskID.MatchString(id) {
			return nil, fmt.Errorf("%w dependency %q: not a task ID", ErrInvalid, id)
		}
	}
	for _, id := range ids {
		t, err := s.Get(ctx, id)
		if errors.Is(err, ErrNotFound) || (err == nil && !sees(acc, t)) {
			return nil, fmt.Errorf("%w dependency %s: no such task", ErrInvalid, id)
		}
		if err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// RegisterRunRaw registers a node-originated or Grainfile-triggered run from a
// JSON RunRegistration payload and returns the resulting Task as JSON.
func (f *Filer) RegisterRunRaw(ctx context.Context, nodeID, owner string, raw json.RawMessage) (json.RawMessage, error) {
	var req RunRegistration
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("%w run registration: %v", ErrInvalid, err)
	}
	t, err := f.RegisterRun(ctx, nodeID, owner, req)
	if err != nil {
		return nil, err
	}
	return json.Marshal(t)
}

// RegisterRun verifies directory admission for (nodeID, owner) and registers
// req directly in the Running state in f.Store.
func (f *Filer) RegisterRun(ctx context.Context, nodeID, owner string, req RunRegistration) (Task, error) {
	if f.Store == nil {
		return Task{}, fmt.Errorf("%w: no store", ErrInvalid)
	}
	nodeID = strings.TrimSpace(nodeID)
	owner = strings.TrimSpace(owner)
	if nodeID == "" || owner == "" {
		return Task{}, fmt.Errorf("%w: missing node or owner", ErrInvalid)
	}
	if req.Node != "" && req.Node != nodeID {
		return Task{}, fmt.Errorf("%w run registration: it names node %q, but comes from %q", ErrInvalid, req.Node, nodeID)
	}
	if req.NodeTask != "" && (len(req.NodeTask) > maxNodeTask || strings.ContainsAny(req.NodeTask, "\r\n")) {
		return Task{}, fmt.Errorf("%w node task %q", ErrInvalid, req.NodeTask)
	}

	grainfileURL := strings.TrimSpace(req.GrainfileURL)
	if grainfileURL == "" {
		grainfileURL = strings.TrimSpace(req.Grainfile)
	}
	if grainfileURL != "" && f.LookupGrainfile != nil {
		if gfRepo, gfKind, gfTitle, gfCommit, ok := f.LookupGrainfile(ctx, grainfileURL); ok {
			if strings.TrimSpace(req.Repo) == "" {
				req.Repo = gfRepo
			}
			if strings.TrimSpace(req.Kind) == "" {
				req.Kind = gfKind
			}
			if strings.TrimSpace(req.Title) == "" {
				req.Title = gfTitle
			}
			if strings.TrimSpace(req.CommitSHA) == "" && strings.TrimSpace(req.Commit) == "" {
				req.CommitSHA = gfCommit
			}
		}
	}

	repo := NormalizeRunRepo(req.Repo, grainfileURL)
	var regNode *registry.Entry
	if f.Registry != nil {
		if entry, ok := f.Registry.Get(nodeID); ok {
			regNode = &entry
		}
	}
	if repo == "" && regNode != nil && len(regNode.Repos) > 0 {
		repo = NormalizeRunRepo(regNode.Repos[0], "")
	}

	if f.Directory != nil {
		ownerEmail := strings.ToLower(strings.TrimPrefix(owner, directory.UserPrefix))
		ownerAcc, err := f.access(ctx, ownerEmail)
		if err != nil {
			return Task{}, err
		}
		switch {
		case repo != "":
			team, err := f.Directory.RepoOwner(ctx, repo)
			if err != nil {
				return Task{}, err
			}
			if team == "" {
				return Task{}, fmt.Errorf("%w: no team owns the repository %s", directory.ErrNotAdmitted, repo)
			}
			if !ownerAcc.MemberOf(team) {
				return Task{}, fmt.Errorf("%w: %s is not in team %s for repository %s", directory.ErrNotAdmitted, owner, team, repo)
			}
			req.Team = team
			req.Repo = repo
		case strings.TrimSpace(req.Team) != "":
			team := strings.ToLower(strings.TrimSpace(req.Team))
			if !ownerAcc.MemberOf(team) {
				return Task{}, fmt.Errorf("%w: %s is not in team %s", directory.ErrNotAdmitted, owner, team)
			}
			req.Team = team
		case len(ownerAcc.Teams) == 1:
			for team := range ownerAcc.Teams {
				if !ownerAcc.MemberOf(team) {
					return Task{}, fmt.Errorf("%w: %s is not in team %s", directory.ErrNotAdmitted, owner, team)
				}
				req.Team = team
			}
			if repos, err := f.Directory.TeamRepos(ctx, req.Team); err == nil && len(repos) == 1 {
				req.Repo = repos[0]
			}
		default:
			return Task{}, fmt.Errorf("%w: run registration requires a repository or team", ErrInvalid)
		}
	} else {
		req.Repo = repo
	}

	req.Node = nodeID
	req.NodeOwner = owner
	req.GrainfileURL = grainfileURL
	t, err := f.Store.RegisterRun(ctx, req)
	if err != nil {
		return Task{}, err
	}
	if f.Logf != nil {
		f.Logf("tasks: node %s of %s registered run %s (%s) for team %s repo %s", nodeID, owner, t.ID, t.Kind, t.Team, t.Repo)
	}
	return t, nil
}
