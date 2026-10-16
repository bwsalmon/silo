// Package registry is the Silo hub's node registry (docs/hub.md §3): the
// directory of the Grain nodes that have dialled in, which repositories
// each will execute tasks for, how many concurrency slots it has free,
// and whether it is online.
//
// A node is registered for as long as its rendezvous connection lasts
// (package rendezvous): Register returns a Lease, the connection
// heartbeats through it, and closing it marks the node offline. The
// registry keeps an offline node's entry, so that the directory can say
// when a node was last seen, until Forget removes it.
//
// The registry holds no task, code or log: only what a node says about
// itself. It is in memory, because everything in it is restated by the
// node on its next connection; a hub that restarts learns its nodes
// again as they reconnect.
package registry

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Node is what a Grain node says about itself when it registers.
type Node struct {
	// ID names the node, stable across its connections: a workstation's
	// grain keeps the same ID when it reconnects.
	ID string `json:"id"`
	// Repos are the repositories, "owner/name", the node will execute
	// tasks for.
	Repos []string `json:"repos"`
	// Slots is the node's concurrency: how many tasks it runs at once.
	Slots int `json:"slots"`
	// Version is the node's grain version, for the directory only.
	Version string `json:"version,omitempty"`
	// Labels are free-form facts about the node (its zone, its machine
	// type), for the directory and, later, for dispatch constraints.
	Labels map[string]string `json:"labels,omitempty"`
}

// Status is a node's health as the directory reports it.
type Status string

const (
	// Online is a connected node that has heartbeated within StaleAfter.
	Online Status = "online"
	// Stale is a connected node that has not heartbeated within
	// StaleAfter: its connection is up, but it may be wedged, so the
	// dispatcher passes it over.
	Stale Status = "stale"
	// Offline is a node with no connection.
	Offline Status = "offline"
)

// Entry is one node's line in the directory.
type Entry struct {
	Node
	// Owner is the identity the node authenticated as, not one it
	// claimed: the rendezvous server sets it from the credential.
	Owner  string `json:"owner"`
	Status Status `json:"status"`
	// Free is the slots the node last said it had free.
	Free int `json:"free"`
	// Claimed is the slots the hub has claimed on the node for tasks it
	// is dispatching and the node has not yet counted against Free.
	Claimed     int       `json:"claimed"`
	ConnectedAt time.Time `json:"connected_at"`
	LastSeen    time.Time `json:"last_seen"`
}

// Available is the slots the dispatcher may still claim on the node.
func (e Entry) Available() int {
	if e.Status != Online {
		return 0
	}
	return max(e.Free-e.Claimed, 0)
}

// Errors Register returns.
var (
	ErrInvalid = errors.New("registry: invalid node")
	// ErrOwner is a node ID already registered to another owner: one
	// user's node cannot take over another's by naming its ID.
	ErrOwner = errors.New("registry: node belongs to another owner")
)

// ErrNoNode is Claim finding no online node with a free slot for the
// repository.
var ErrNoNode = errors.New("registry: no online node with a free slot")

// DefaultStaleAfter is StaleAfter when zero: three of the rendezvous
// client's default heartbeats.
const DefaultStaleAfter = 45 * time.Second

// Registry is the node directory. Its zero value is not usable; call New.
type Registry struct {
	// StaleAfter is how long a connected node may go without a
	// heartbeat before it is Stale; DefaultStaleAfter when zero.
	StaleAfter time.Duration
	// Now is the clock; time.Now when nil.
	Now func() time.Time

	mu    sync.Mutex
	nodes map[string]*node
	subs  map[chan struct{}]struct{}
}

type node struct {
	entry Entry
	lease *Lease // nil when offline
	// epoch counts the heartbeats that settled claims, so that a claim
	// released after one does not give back a later claim's slot.
	epoch uint64
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{nodes: map[string]*node{}, subs: map[chan struct{}]struct{}{}}
}

func (r *Registry) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Registry) staleAfter() time.Duration {
	if r.StaleAfter > 0 {
		return r.StaleAfter
	}
	return DefaultStaleAfter
}

var (
	idPattern   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)
	repoPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*/[a-zA-Z0-9._-]+$`)
)

// Validate checks a node's registration, and returns it with its
// repositories lower-cased, sorted and deduplicated.
func Validate(n Node) (Node, error) {
	if !idPattern.MatchString(n.ID) {
		return n, fmt.Errorf("%w: id %q: want 1-63 letters, digits, '.', '_' or '-'", ErrInvalid, n.ID)
	}
	if n.Slots < 0 {
		return n, fmt.Errorf("%w: slots %d is negative", ErrInvalid, n.Slots)
	}
	seen := map[string]bool{}
	var repos []string
	for _, r := range n.Repos {
		if !repoPattern.MatchString(r) {
			return n, fmt.Errorf("%w: repo %q: want owner/name", ErrInvalid, r)
		}
		r = strings.ToLower(r)
		if !seen[r] {
			seen[r] = true
			repos = append(repos, r)
		}
	}
	sort.Strings(repos)
	n.Repos = repos
	return n, nil
}

// Register adds a connected node to the directory, or brings an
// offline one back, as owner. A node already online under the same
// owner is superseded: its old lease is ended (Lease.Done closes) and
// the new connection takes its place, which is how a node that lost its
// connection without the hub noticing reconnects at once.
func (r *Registry) Register(n Node, owner string) (*Lease, error) {
	n, err := Validate(n)
	if err != nil {
		return nil, err
	}
	if owner == "" {
		return nil, fmt.Errorf("%w: no owner", ErrInvalid)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	cur := r.nodes[n.ID]
	if cur != nil && cur.entry.Owner != owner {
		return nil, fmt.Errorf("%w: %s", ErrOwner, n.ID)
	}
	if cur != nil && cur.lease != nil {
		cur.lease.end()
	}
	l := &Lease{r: r, id: n.ID, done: make(chan struct{})}
	r.nodes[n.ID] = &node{
		entry: Entry{Node: n, Owner: owner, Free: n.Slots, ConnectedAt: now, LastSeen: now},
		lease: l,
	}
	r.notify()
	return l, nil
}

// Forget removes an offline node from the directory. It reports whether
// there was one to remove; an online node is not removed.
func (r *Registry) Forget(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.nodes[id]
	if n == nil || n.lease != nil {
		return false
	}
	delete(r.nodes, id)
	r.notify()
	return true
}

// status is n's status at now; r.mu is held.
func (r *Registry) status(n *node, now time.Time) Status {
	switch {
	case n.lease == nil:
		return Offline
	case now.Sub(n.entry.LastSeen) > r.staleAfter():
		return Stale
	}
	return Online
}

func (r *Registry) entry(n *node, now time.Time) Entry {
	e := n.entry
	e.Status = r.status(n, now)
	e.Repos = append([]string(nil), e.Repos...)
	if e.Labels != nil {
		labels := make(map[string]string, len(e.Labels))
		for k, v := range e.Labels {
			labels[k] = v
		}
		e.Labels = labels
	}
	return e
}

// Get returns one node's entry.
func (r *Registry) Get(id string) (Entry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.nodes[id]
	if n == nil {
		return Entry{}, false
	}
	return r.entry(n, r.now()), true
}

// List returns the directory, ordered by node ID.
func (r *Registry) List() []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	out := make([]Entry, 0, len(r.nodes))
	for _, n := range r.nodes {
		out = append(out, r.entry(n, now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ForRepo returns the nodes registered for repo, whatever their status,
// the ones with the most available slots first.
func (r *Registry) ForRepo(repo string) []Entry {
	return r.forRepos([]string{repo})
}

// forRepos is ForRepo for the nodes registered for any of repos.
func (r *Registry) forRepos(repos []string) []Entry {
	want := map[string]bool{}
	for _, repo := range repos {
		want[strings.ToLower(repo)] = true
	}
	var out []Entry
	for _, e := range r.List() {
		for _, repo := range e.Repos {
			if want[repo] {
				out = append(out, e)
				break
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Available() > out[j].Available() })
	return out
}

// Claim reserves one slot for a task on repo on the online node with the
// most available slots, so that two dispatches do not both count the
// same free slot. The claim lasts until it is released, or until the
// node's next heartbeat, whose free count is taken to include it: the
// dispatcher releases a claim whose task the node refused, and lets the
// heartbeat settle one it accepted.
func (r *Registry) Claim(repo string) (*Claim, error) {
	return r.ClaimFor([]string{repo}, nil)
}

// ClaimFor is Claim for a task that any node registered for one of
// repos may run (a team's task, docs/hub.md §6.2), on a node ok accepts:
// the task's constraints, and the nodes it is not to go to. ok sees a
// copy of each candidate's entry and is called without the registry's
// lock held; nil accepts every node.
func (r *Registry) ClaimFor(repos []string, ok func(Entry) bool) (*Claim, error) {
	for _, e := range r.forRepos(repos) {
		if e.Available() == 0 {
			break
		}
		if ok != nil && !ok(e) {
			continue
		}
		r.mu.Lock()
		n := r.nodes[e.ID]
		if n != nil && r.status(n, r.now()) == Online && n.entry.Free-n.entry.Claimed > 0 {
			n.entry.Claimed++
			c := &Claim{r: r, NodeID: e.ID, lease: n.lease, epoch: n.epoch}
			r.notify()
			r.mu.Unlock()
			return c, nil
		}
		r.mu.Unlock()
	}
	return nil, fmt.Errorf("%w for %s", ErrNoNode, strings.Join(repos, ", "))
}

// Changes returns a channel that receives (without blocking the
// registry, coalesced) whenever the directory changes, for a UI that
// pushes it, and a function that stops it.
func (r *Registry) Changes() (<-chan struct{}, func()) {
	c := make(chan struct{}, 1)
	r.mu.Lock()
	r.subs[c] = struct{}{}
	r.mu.Unlock()
	return c, func() {
		r.mu.Lock()
		delete(r.subs, c)
		r.mu.Unlock()
	}
}

// notify wakes every Changes subscriber; r.mu is held.
func (r *Registry) notify() {
	for c := range r.subs {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

// Lease is one connection's registration of a node.
type Lease struct {
	r    *Registry
	id   string
	once sync.Once
	done chan struct{}
}

// NodeID is the node the lease registers.
func (l *Lease) NodeID() string { return l.id }

// Done is closed when the lease ends: Close was called, or a newer
// connection for the same node superseded it.
func (l *Lease) Done() <-chan struct{} { return l.done }

func (l *Lease) end() { l.once.Do(func() { close(l.done) }) }

// current is the node l still registers, or nil; r.mu is held.
func (l *Lease) current() *node {
	n := l.r.nodes[l.id]
	if n == nil || n.lease != l {
		return nil
	}
	return n
}

// Heartbeat records that the node is alive with free slots free, and
// settles the hub's outstanding claims against it. It reports false
// when the lease has ended.
func (l *Lease) Heartbeat(free int) bool {
	l.r.mu.Lock()
	defer l.r.mu.Unlock()
	n := l.current()
	if n == nil {
		return false
	}
	n.entry.Free = min(max(free, 0), n.entry.Slots)
	n.entry.Claimed = 0
	n.epoch++
	n.entry.LastSeen = l.r.now()
	l.r.notify()
	return true
}

// Close ends the lease and marks the node offline, unless a newer
// connection has already superseded it.
func (l *Lease) Close() {
	l.r.mu.Lock()
	defer l.r.mu.Unlock()
	if n := l.current(); n != nil {
		n.lease = nil
		n.entry.Free, n.entry.Claimed = 0, 0
		l.r.notify()
	}
	l.end()
}

// Claim is one slot the dispatcher holds on a node.
type Claim struct {
	r      *Registry
	NodeID string
	lease  *Lease
	epoch  uint64
	once   sync.Once
}

// Release gives the slot back, for a task the node did not take. It is
// a no-op after the node has heartbeated (which settled the claim) or
// reconnected.
func (c *Claim) Release() {
	c.once.Do(func() {
		c.r.mu.Lock()
		defer c.r.mu.Unlock()
		n := c.r.nodes[c.NodeID]
		if n != nil && n.lease == c.lease && n.epoch == c.epoch && n.entry.Claimed > 0 {
			n.entry.Claimed--
			c.r.notify()
		}
	})
}
