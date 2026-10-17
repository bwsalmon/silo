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
//
// With a Store, the registry is shared between the hub's pods
// (docs/hub.md §2.2.2, stage R2): each pod's registry stays
// authoritative for the connections that pod holds and writes them
// through to the database, under the pod's lease, and mirrors the rest
// from it (Run). Ownership and supersession are then conditional
// statements in the database, so they hold across pods, and a node held
// by another pod is online only while that pod's lease is.
package registry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/hub/notify"
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
	// Features are the parts of the task interface the node supports
	// beyond the ones every node speaks (docs/hub-interface.md):
	// FeatureInteractive. The dispatcher places a task that needs one
	// only on a node that lists it.
	Features []string `json:"features,omitempty"`
}

// FeatureInteractive is a node that runs interactive tasks: sessions
// that take a human's messages (tasks.KindInteractive).
const FeatureInteractive = "interactive"

// maxFeatures bounds what a node may list.
const maxFeatures = 32

// Supports is whether the node listed feature.
func (n Node) Supports(feature string) bool { return slices.Contains(n.Features, feature) }

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

// DefaultPodLease is PodLease when zero: a pod renews its lease every
// third of it, every five seconds.
const DefaultPodLease = 15 * time.Second

// storeTimeout bounds each of the registry's writes through to its
// Store.
const storeTimeout = 5 * time.Second

// Registry is the node directory. Its zero value is not usable; call New.
type Registry struct {
	// StaleAfter is how long a connected node may go without a
	// heartbeat before it is Stale; DefaultStaleAfter when zero.
	StaleAfter time.Duration
	// Now is the clock; time.Now when nil.
	Now func() time.Time

	// Store, when set, shares the registry with the hub's other pods,
	// as Pod (docs/hub.md §2.2.2). Pod names this pod, and PodAddr is
	// where the other pods reach it; set all three before the first
	// Register, and run Run.
	Store   *Store
	Pod     string
	PodAddr string
	// PodLease is how long each renewal of the pod's lease lasts;
	// DefaultPodLease when zero. Run renews it every third of that.
	PodLease time.Duration
	// Logf, when set, receives the Store's failures.
	Logf func(format string, args ...any)
	// Bus, when set, tells the other pods of each change this pod
	// writes through to the Store, as {"node":"<id>"} (docs/hub.md
	// §2.2.4); hand its notices to Notice, which refills the mirror.
	Bus *notify.Bus

	// wmu serialises Register, so that the database and the memory
	// agree on which of two connections for a node came last.
	wmu   sync.Mutex
	mu    sync.Mutex
	nodes map[string]*node
	subs  map[chan struct{}]struct{}
	// kick asks Run to refill the mirror now: a change notice.
	kick chan struct{}
	// seq counts this pod's own changes to nodes, so that a refill read
	// from the database before one does not undo it.
	seq uint64
}

type node struct {
	entry Entry
	lease *Lease // nil unless this pod holds the node's connection
	// conn names the connection holding the node, here or on pod; empty
	// when it is offline.
	conn string
	pod  string
	// podUntil is pod's lease, as the last refill read it, for a node
	// another pod holds.
	podUntil time.Time
	// settled is the database's count of the node's heartbeats, as the
	// last refill read it: a refill that sees it move settles the
	// node's claims, as a heartbeat here does.
	settled int64
	// changed is seq when this pod last changed the node.
	changed uint64
	// epoch counts the heartbeats that settled claims, so that a claim
	// released after one does not give back a later claim's slot.
	epoch uint64
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{nodes: map[string]*node{}, subs: map[chan struct{}]struct{}{}, kick: make(chan struct{}, 1)}
}

// announce tells the other pods, through the Bus, that node id's row
// changed. It is called after the write, outside r.mu, and does not
// wait for the database: the bus sends the notice soon after,
// coalesced with the node's later ones (Bus.Post). A notice that is
// lost leaves the other pods to see the change at their next refill.
func (r *Registry) announce(id string) {
	if r.Bus == nil {
		return
	}
	r.Bus.Post(notify.Message{Node: id})
}

// Notice is a notice from the Bus: a node's row changed, on this pod or
// another, or the bus reconnected and may have missed one. Either asks
// Run to refill the mirror now, which wakes the Changes subscribers (the
// dispatcher) if anything changed.
func (r *Registry) Notice(m notify.Message) {
	if m.Node == "" && !m.Reconnected {
		return
	}
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Holds is whether this pod holds node id's connection.
func (r *Registry) Holds(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.nodes[id]
	return n != nil && n.lease != nil
}

func (r *Registry) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Registry) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

func (r *Registry) podLease() time.Duration {
	if r.PodLease > 0 {
		return r.PodLease
	}
	return DefaultPodLease
}

func storeContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), storeTimeout)
}

// newConn names a connection.
func newConn() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
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
	// A feature name is one a node does not know is simply never asked
	// of it, so any short name is accepted.
	featurePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
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
	if len(n.Features) > maxFeatures {
		return n, fmt.Errorf("%w: over %d features", ErrInvalid, maxFeatures)
	}
	var features []string
	for _, f := range n.Features {
		if !featurePattern.MatchString(f) {
			return n, fmt.Errorf("%w: feature %q", ErrInvalid, f)
		}
		if !slices.Contains(features, f) {
			features = append(features, f)
		}
	}
	sort.Strings(features)
	n.Features = features
	return n, nil
}

// Register adds a connected node to the directory, or brings an
// offline one back, as owner. A node already online under the same
// owner is superseded: its old lease is ended (Lease.Done closes) and
// the new connection takes its place, which is how a node that lost its
// connection without the hub noticing reconnects at once.
//
// With a Store, the owner is checked in the database, whichever pod
// registered the node first, and a connection on another pod is
// superseded there: its pod learns so at its next heartbeat. An error
// that is neither ErrInvalid nor ErrOwner is the database's.
func (r *Registry) Register(n Node, owner string) (*Lease, error) {
	n, err := Validate(n)
	if err != nil {
		return nil, err
	}
	if owner == "" {
		return nil, fmt.Errorf("%w: no owner", ErrInvalid)
	}
	r.wmu.Lock()
	defer r.wmu.Unlock()
	now := r.now()
	l := &Lease{r: r, id: n.ID, conn: newConn(), done: make(chan struct{})}
	e := Entry{Node: n, Owner: owner, Free: n.Slots, ConnectedAt: now, LastSeen: now}
	if r.Store != nil {
		ctx, cancel := storeContext()
		err := r.Store.register(ctx, e, r.Pod, l.conn)
		cancel()
		if err != nil {
			return nil, err
		}
		defer r.announce(n.ID)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.nodes[n.ID]
	// The database has checked the owner; a mirrored entry may be older
	// than it.
	if r.Store == nil && cur != nil && cur.entry.Owner != owner {
		return nil, fmt.Errorf("%w: %s", ErrOwner, n.ID)
	}
	if cur != nil && cur.lease != nil {
		cur.lease.end()
	}
	r.seq++
	r.nodes[n.ID] = &node{entry: e, lease: l, conn: l.conn, pod: r.Pod, changed: r.seq}
	r.notify()
	return l, nil
}

// Forget removes an offline node from the directory, and with a Store
// from the database, which unbinds its ID from its owner. It reports
// whether there was one to remove; a node that is not offline is not
// removed.
func (r *Registry) Forget(id string) bool {
	r.mu.Lock()
	n := r.nodes[id]
	if n == nil || r.status(n, r.now()) != Offline {
		r.mu.Unlock()
		return false
	}
	conn := n.conn
	r.mu.Unlock()
	if r.Store != nil {
		ctx, cancel := storeContext()
		ok, err := r.Store.forget(ctx, id, conn)
		cancel()
		if err != nil {
			r.logf("registry: forgetting %s: %v", id, err)
		}
		if !ok {
			return false
		}
		r.announce(id)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur := r.nodes[id]; cur != nil && cur.lease == nil && cur.conn == conn {
		delete(r.nodes, id)
		r.notify()
	}
	return true
}

// Unbind removes a node from the directory whatever its status, and
// with a Store from the database, so that its ID belongs to nobody: the
// ID was given up (docs/hub.md §5.2). A connection this pod holds for it
// is ended (Lease.Done closes); one on another pod finds at its next
// heartbeat that it no longer holds the node, and ends too.
func (r *Registry) Unbind(id string) error {
	r.wmu.Lock()
	defer r.wmu.Unlock()
	if r.Store != nil {
		ctx, cancel := storeContext()
		err := r.Store.unbind(ctx, id)
		cancel()
		if err != nil {
			return err
		}
		defer r.announce(id)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if n := r.nodes[id]; n != nil {
		if n.lease != nil {
			n.lease.end()
		}
		delete(r.nodes, id)
		r.notify()
	}
	return nil
}

// status is n's status at now; r.mu is held. A node held by another pod
// is offline once that pod's lease has lapsed, with no pod having to
// notice; one the database says this pod holds, but whose connection
// this pod does not have, is offline too.
func (r *Registry) status(n *node, now time.Time) Status {
	switch {
	case n.lease == nil && (n.conn == "" || n.pod == r.Pod || !now.Before(n.podUntil)):
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
	e.Features = slices.Clone(e.Features)
	return e
}

// Available is the slots each online node has available now, by node
// ID: what the dispatcher compares to tell a change that may let it
// place a task from one that cannot.
func (r *Registry) Available() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	out := make(map[string]int, len(r.nodes))
	for id, n := range r.nodes {
		if r.status(n, now) == Online {
			out[id] = max(n.entry.Free-n.entry.Claimed, 0)
		}
	}
	return out
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

// forRepos is ForRepo for the nodes registered for any of repos, the
// ones with the same available slots by ID. It copies only those nodes'
// entries: the dispatcher calls it for each task it places.
func (r *Registry) forRepos(repos []string) []Entry {
	want := map[string]bool{}
	for _, repo := range repos {
		want[strings.ToLower(repo)] = true
	}
	var out []Entry
	r.mu.Lock()
	now := r.now()
	for _, n := range r.nodes {
		for _, repo := range n.entry.Repos {
			if want[repo] {
				out = append(out, r.entry(n, now))
				break
			}
		}
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if a, b := out[i].Available(), out[j].Available(); a != b {
			return a > b
		}
		return out[i].ID < out[j].ID
	})
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
			c := &Claim{r: r, NodeID: e.ID, conn: n.conn, epoch: n.epoch}
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
	conn string
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

// offline marks n, which l held, offline; r.mu is held.
func (l *Lease) offline(n *node) {
	n.lease, n.conn = nil, ""
	n.entry.Free, n.entry.Claimed = 0, 0
	l.r.seq++
	n.changed = l.r.seq
	l.r.notify()
}

// Heartbeat records that the node is alive with free slots free, and
// settles the hub's outstanding claims against it. It reports false
// when the lease has ended: with a Store, also when a newer connection
// on another pod has superseded it, which ends the lease.
func (l *Lease) Heartbeat(free int) bool {
	r := l.r
	r.mu.Lock()
	n := l.current()
	if n == nil {
		r.mu.Unlock()
		return false
	}
	free = min(max(free, 0), n.entry.Slots)
	moved := free != n.entry.Free
	now := r.now()
	r.mu.Unlock()
	if r.Store != nil {
		ctx, cancel := storeContext()
		held, err := r.Store.heartbeat(ctx, l.id, l.conn, free, now)
		cancel()
		switch {
		case err != nil:
			// The node is still connected here; the database will hear
			// from it at the next heartbeat.
			r.logf("registry: heartbeat of %s: %v", l.id, err)
		case held && moved:
			// Only a change of free slots: the dispatcher's claims settle
			// at its next refill anyway.
			r.announce(l.id)
		case !held:
			r.mu.Lock()
			if n := l.current(); n != nil {
				l.offline(n)
			}
			r.mu.Unlock()
			l.end()
			return false
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if n = l.current(); n == nil {
		return false
	}
	n.entry.Free = free
	n.entry.Claimed = 0
	n.epoch++
	n.entry.LastSeen = now
	r.notify()
	return true
}

// Close ends the lease and marks the node offline, unless a newer
// connection has already superseded it.
func (l *Lease) Close() {
	l.r.mu.Lock()
	n := l.current()
	if n != nil {
		l.offline(n)
	}
	l.r.mu.Unlock()
	l.end()
	if n != nil && l.r.Store != nil {
		// Conditional on the connection, so a late close never takes a
		// newer connection, on any pod, offline.
		ctx, cancel := storeContext()
		if err := l.r.Store.close(ctx, l.id, l.conn); err != nil {
			l.r.logf("registry: closing %s: %v", l.id, err)
		}
		cancel()
		l.r.announce(l.id)
	}
}

// Claim is one slot the dispatcher holds on a node.
type Claim struct {
	r      *Registry
	NodeID string
	conn   string
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
		if n != nil && n.conn != "" && n.conn == c.conn && n.epoch == c.epoch && n.entry.Claimed > 0 {
			n.entry.Claimed--
			c.r.notify()
		}
	})
}
