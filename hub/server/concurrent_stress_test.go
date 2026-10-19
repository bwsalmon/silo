package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/bwsalmon/silo/events"
	"github.com/bwsalmon/silo/hub/directory"
	"github.com/bwsalmon/silo/hub/grainfiles"
	"github.com/bwsalmon/silo/hub/live"
	"github.com/bwsalmon/silo/hub/nodeui"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
	"github.com/bwsalmon/silo/hub/tasks"
	"github.com/bwsalmon/silo/internal/pgtest"
)

type replicaPod struct {
	name   string
	h      *Hub
	url    string
	cancel context.CancelFunc
	done   chan error
	closed atomic.Bool
}

func startReplicaPod(t *testing.T, dbURL, name string, key []byte) *replicaPod {
	t.Helper()
	h, err := New(context.Background(), Config{
		Database:      dbURL,
		DevUserHeader: userHeader,
		HubAdmins:     []string{"admin@example.com"},
		NodeUIDomain:  "nodes.hub.test",
		Heartbeat:     150 * time.Millisecond,
		ShutdownGrace: 5 * time.Second,
		DispatchRetry: 80 * time.Millisecond,
		LeaseRetry:    80 * time.Millisecond,
		Holder:        name,
		Pod:           name,
		PodLease:      600 * time.Millisecond,
		InternalAddr:  "127.0.0.1:0",
		InternalKey:   key,
		Logf:          logf(t),
	})
	if err != nil {
		t.Fatalf("New(%s): %v", name, err)
	}
	origLost := h.Dispatcher.LostRequeueAfter
	h.Dispatcher.LostRequeueAfter = func(ctx context.Context, team string) (time.Duration, error) {
		d, err := origLost(ctx, team)
		if err != nil || d <= 0 {
			return d, err
		}
		return 150 * time.Millisecond, nil
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		h.Close()
		t.Fatalf("Listen(%s): %v", name, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &replicaPod{
		name:   name,
		h:      h,
		url:    "http://" + l.Addr().String(),
		cancel: cancel,
		done:   make(chan error, 1),
	}
	go func() { p.done <- h.Serve(ctx, l) }()
	t.Cleanup(func() { p.stop(t) })
	eventually(t, name+" ready and bus listening", func() bool {
		return call(t, "GET", p.url+ReadyzPath, "", nil, nil) == http.StatusOK && h.Bus.Listening()
	})
	return p
}

func (p *replicaPod) stop(t *testing.T) {
	t.Helper()
	if !p.closed.CompareAndSwap(false, true) {
		return
	}
	p.cancel()
	http.DefaultClient.CloseIdleConnections()
	select {
	case err := <-p.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("replica %s serve: %v", p.name, err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("replica %s did not stop within 15s", p.name)
	}
	_ = p.h.Close()
}

// simulatedCapNode is a multi-slot simulated Grain node with capabilities that
// executes offered tasks, emits in-run approval_request and awaiting_reply
// prompts for interactive tasks, and records which capabilities were offered to it.
type simulatedCapNode struct {
	id        string
	caps      map[string]string
	features  []string
	slots     int
	token     atomic.Value // string
	dropFirst atomic.Bool  // if true, drop connection without completing first accepted task

	mu          sync.Mutex
	offered     map[string]tasks.Spec
	completed   map[string]bool
	approvals   map[string]string
	replies     map[string]string
	droppedTask string
}

func newSimulatedCapNode(id string, caps map[string]string, features []string, slots int, tok string) *simulatedCapNode {
	n := &simulatedCapNode{
		id:        id,
		caps:      caps,
		features:  features,
		slots:     slots,
		offered:   make(map[string]tasks.Spec),
		completed: make(map[string]bool),
		approvals: make(map[string]string),
		replies:   make(map[string]string),
	}
	n.token.Store(tok)
	return n
}

func (n *simulatedCapNode) connectAndServe(ctx context.Context, hubURL string) (*rendezvous.Conn, chan struct{}, error) {
	cl := &rendezvous.Client{
		Hub: hubURL,
		Token: func(context.Context) (string, error) {
			v, _ := n.token.Load().(string)
			return v, nil
		},
		Node: registry.Node{
			ID:       n.id,
			Repos:    []string{"bwsalmon/silo"},
			Slots:    n.slots,
			Labels:   n.caps,
			Features: n.features,
		},
	}
	conn, err := cl.Connect(ctx)
	if err != nil {
		return nil, nil, err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		defer wg.Wait()
		for {
			c, err := conn.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func(stream net.Conn) {
				defer wg.Done()
				n.handleStream(ctx, conn, stream)
			}(c)
		}
	}()
	return conn, done, nil
}

func (n *simulatedCapNode) handleStream(ctx context.Context, conn *rendezvous.Conn, c net.Conn) {
	defer c.Close()
	st := tasks.NewStream(c)
	m, err := st.Read()
	if err != nil {
		return
	}
	if (m.Type != tasks.TypeOffer && m.Type != tasks.TypeResume) || m.Task == nil {
		return
	}
	task := *m.Task
	n.mu.Lock()
	if m.Type == tasks.TypeOffer {
		n.offered[task.ID] = task
	}
	shouldDrop := n.dropFirst.CompareAndSwap(true, false)
	if shouldDrop {
		n.droppedTask = task.ID
	}
	n.mu.Unlock()

	if err := st.Write(tasks.Message{Type: tasks.TypeAccept, NodeTask: "local-" + task.ID}); err != nil {
		return
	}
	if shouldDrop {
		// Simulate abrupt node disconnect after claiming task so lost_requeue_seconds reclaims it.
		_ = conn.Close()
		return
	}

	// Open an upstream event stream for this task.
	up, err := conn.Open(ctx)
	if err != nil {
		return
	}
	defer up.Close()

	seq := int64(1)
	emit := func(typ, payload string) error {
		_, err := fmt.Fprintf(up, `{"v":1,"seq":%d,"type":%q,"task":%q,%q:%s}`+"\n", seq, typ, task.ID, typ, payload)
		seq++
		return err
	}
	_ = emit(events.TypeStatus, `{"phase":"running","state":"info","message":"started"}`)

	// If the task prompt requests an in-run approval_request or awaiting_reply, exercise it.
	if strings.Contains(task.Prompt, "[need-approval]") {
		n.mu.Lock()
		alreadyApproved := m.Type == tasks.TypeResume && n.approvals[task.ID] != ""
		n.mu.Unlock()
		if !alreadyApproved {
			reqID := "req-" + task.ID
			_ = emit(events.TypeApprovalRequest, fmt.Sprintf(`{"id":%q,"tool":"deploy","message":"approve deploy"}`, reqID))
			for {
				msg, err := st.Read()
				if err != nil {
					return
				}
				if msg.Type == tasks.TypeApprove && msg.Request == reqID {
					n.mu.Lock()
					n.approvals[task.ID] = msg.Decision
					n.mu.Unlock()
					break
				}
			}
		}
	}
	if strings.Contains(task.Prompt, "[need-reply]") && task.Kind == tasks.KindInteractive {
		n.mu.Lock()
		alreadyReplied := m.Type == tasks.TypeResume && n.replies[task.ID] != ""
		n.mu.Unlock()
		if !alreadyReplied {
			_ = emit(events.TypeAwaitingReply, `{"state":"waiting","question":"Proceed with change?"}`)
			for {
				msg, err := st.Read()
				if err != nil {
					return
				}
				if msg.Type == tasks.TypeMessage && msg.Message != nil {
					n.mu.Lock()
					n.replies[task.ID] = msg.Message.Text
					n.mu.Unlock()
					_ = emit(events.TypeMessage, fmt.Sprintf(`{"id":%q,"sender":%q,"text":%q}`, msg.Message.ID, msg.Message.Sender, msg.Message.Text))
					_ = emit(events.TypeAwaitingReply, fmt.Sprintf(`{"state":"resumed","message_id":%q}`, msg.Message.ID))
					break
				}
			}
		}
	}

	_ = emit(events.TypeResult, `{"exit":{"code":0,"reason":"completed"}}`)
	_ = st.Write(tasks.Message{Type: tasks.TypeResult, State: tasks.Done, Reason: "completed by " + n.id})
	n.mu.Lock()
	n.completed[task.ID] = true
	n.mu.Unlock()
}

func (n *simulatedCapNode) runAutoReconnect(t *testing.T, ctx context.Context, pods []*replicaPod, startIdx int) {
	go func() {
		for i := 0; ctx.Err() == nil; i++ {
			var active []*replicaPod
			for _, p := range pods {
				if !p.closed.Load() {
					active = append(active, p)
				}
			}
			if len(active) == 0 {
				return
			}
			target := active[(startIdx+i)%len(active)]
			conn, done, err := n.connectAndServe(ctx, target.url)
			if err != nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(50 * time.Millisecond):
					continue
				}
			}
			select {
			case <-ctx.Done():
				_ = conn.Close()
				<-done
				return
			case <-done:
				_ = conn.Close()
				// If we deliberately dropped the first task to test lost_requeue_after_seconds,
				// stay offline for 250ms so the dispatcher reclaims the lost task onto another node.
				if i == 0 && n.id == "node-gpu" {
					select {
					case <-ctx.Done():
						return
					case <-time.After(250 * time.Millisecond):
					}
				} else {
					select {
					case <-ctx.Done():
						return
					case <-time.After(40 * time.Millisecond):
					}
				}
			}
		}
	}()
}

// TestConcurrentMultiReplicaDispatchAndApprovalRaces exercises Scope 1:
//   - 3 in-process Hub server replicas sharing one database (tested on both PostgreSQL and SQLite).
//   - Multiple simulated Grain nodes with overlapping and disjoint capabilities
//     ("gpu", "bazel-remote", and both) connecting, disconnecting, and reconnecting mid-run.
//   - Concurrent team members/admins approving/rejecting proposed tasks, toggling
//     anyone_may_propose and require_approval, and answering in-run approval_request
//     and awaiting_reply prompts across follower and leader replicas.
//   - Dispatcher lease handoff between replicas while tasks are being claimed, completed,
//     and reclaimed via lost_requeue_after_seconds.
func TestConcurrentMultiReplicaDispatchAndApprovalRaces(t *testing.T) {
	backends := []struct {
		name string
		url  func(t *testing.T) string
	}{
		{
			name: "sqlite",
			url: func(t *testing.T) string {
				return "sqlite:" + filepath.Join(t.TempDir(), "hub.db")
			},
		},
		{
			name: "postgres",
			url: func(t *testing.T) string {
				return pgtest.Start(t).URL
			},
		},
	}

	for _, be := range backends {
		t.Run(be.name, func(t *testing.T) {
			dbURL := be.url(t)
			key := []byte("0123456789abcdef0123456789abcdef")

			podA := startReplicaPod(t, dbURL, "pod-a", key)
			podB := startReplicaPod(t, dbURL, "pod-b", key)
			podC := startReplicaPod(t, dbURL, "pod-c", key)
			pods := []*replicaPod{podA, podB, podC}

			const (
				admin = "admin@example.com"
				alice = "alice@example.com" // team admin
				bob   = "bob@example.com"   // team admin
				dave  = "dave@example.com"  // team member
				erin  = "erin@example.com"  // outside user proposing tasks
			)

			call(t, "POST", podA.url+"/v1/teams", admin, map[string]string{"id": "infra", "name": "Infra"}, &struct{}{})
			call(t, "PUT", podA.url+"/v1/teams/infra/members/user:"+alice, admin, map[string]string{"role": "admin"}, &struct{}{})
			call(t, "PUT", podB.url+"/v1/teams/infra/members/user:"+bob, admin, map[string]string{"role": "admin"}, &struct{}{})
			call(t, "PUT", podC.url+"/v1/teams/infra/members/user:"+dave, admin, map[string]string{"role": "member"}, &struct{}{})
			call(t, "PUT", podA.url+"/v1/users/"+erin, admin, map[string]bool{"sys_admin": false}, nil)
			call(t, "PUT", podA.url+"/v1/teams/infra/repos/bwsalmon/silo", admin, nil, &struct{}{})

			// Configure lost_requeue_after_seconds = 300 (scaled to 150ms in test) so disconnected nodes' tasks are reclaimed quickly.
			call(t, "PUT", podB.url+"/v1/teams/infra/settings", alice, map[string]any{
				"lost_requeue_after_seconds": 300,
				"anyone_may_propose":         true,
				"require_approval":           false,
			}, &struct{}{})

			mint := func(podURL, owner, nodeID string) string {
				var out struct {
					Token string `json:"token"`
				}
				if code := call(t, "POST", podURL+"/v1/node-tokens", owner, map[string]string{"node": nodeID}, &out); code != http.StatusCreated {
					t.Fatalf("mint token for %s on %s: %d", nodeID, podURL, code)
				}
				return out.Token
			}

			tokGPU := mint(podA.url, alice, "node-gpu")
			tokBazel := mint(podB.url, bob, "node-bazel")
			tokCombo := mint(podC.url, dave, "node-combo")

			// node-gpu has gpu capability and drops its first task mid-run to trigger lost_requeue_seconds reclamation.
			nodeGPU := newSimulatedCapNode("node-gpu", map[string]string{"gpu": "true"}, []string{registry.FeatureInteractive}, 2, tokGPU)
			nodeGPU.dropFirst.Store(true)
			// node-bazel has bazel-remote capability.
			nodeBazel := newSimulatedCapNode("node-bazel", map[string]string{"bazel-remote": "true"}, []string{registry.FeatureInteractive}, 2, tokBazel)
			// node-combo has both gpu and bazel-remote capabilities.
			nodeCombo := newSimulatedCapNode("node-combo", map[string]string{"gpu": "true", "bazel-remote": "true"}, []string{registry.FeatureInteractive}, 3, tokCombo)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			nodeGPU.runAutoReconnect(t, ctx, pods, 1)
			nodeBazel.runAutoReconnect(t, ctx, pods, 2)
			nodeCombo.runAutoReconnect(t, ctx, pods, 0)

			for _, id := range []string{"node-gpu", "node-bazel", "node-combo"} {
				eventually(t, id+" online across pods", func() bool {
					return online(t, podA.url, id) && online(t, podB.url, id) && online(t, podC.url, id)
				})
			}

			// Concurrently toggle team settings while filing proposed tasks and approving/rejecting them across replicas.
			var settingsWG sync.WaitGroup
			settingsStop := make(chan struct{})
			settingsWG.Add(2)
			for g := 0; g < 2; g++ {
				go func(g int) {
					defer settingsWG.Done()
					for i := 0; ; i++ {
						select {
						case <-settingsStop:
							return
						default:
						}
						p := pods[(g+i)%len(pods)]
						actor := alice
						if i%2 == 1 {
							actor = bob
						}
						_ = call(t, "PUT", p.url+"/v1/teams/infra/settings", actor, map[string]any{
							"lost_requeue_after_seconds": 300,
							"anyone_may_propose":         true,
							"require_approval":           i%3 == 0,
						}, nil)
						time.Sleep(15 * time.Millisecond)
					}
				}(g)
			}

			// Create 12 proposed tasks (6 to be concurrently approved by multiple team admins/members, 4 to be rejected, 2 racing approve vs reject).
			type propSpec struct {
				id     string
				labels map[string]string
				mode   string // "approve", "reject", "race"
			}
			var props []propSpec
			for i := 0; i < 12; i++ {
				p := pods[i%len(pods)]
				lbls := map[string]string{"gpu": "true"}
				if i%2 == 1 {
					lbls = map[string]string{"bazel-remote": "true"}
				}
				if i%3 == 2 {
					lbls = map[string]string{"gpu": "true", "bazel-remote": "true"}
				}
				var tk tasks.Task
				code := call(t, "POST", p.url+tasks.TasksPath, erin, map[string]any{
					"repo":        "bwsalmon/silo",
					"prompt":      fmt.Sprintf("proposed task %d", i),
					"constraints": map[string]any{"labels": lbls},
				}, &tk)
				if code != http.StatusCreated {
					t.Fatalf("propose task %d: status %d", i, code)
				}
				if tk.State != tasks.Proposed {
					t.Fatalf("expected proposed state for erin's task %d, got %s", i, tk.State)
				}
				mode := "approve"
				if i >= 6 && i < 10 {
					mode = "reject"
				} else if i >= 10 {
					mode = "race"
				}
				props = append(props, propSpec{id: tk.ID, labels: lbls, mode: mode})
			}

			// Also file direct tasks including interactive tasks that require in-run approval_request and awaiting_reply.
			var directIDs []string
			var approvalTaskID, replyTaskID string
			for i := 0; i < 6; i++ {
				p := pods[i%len(pods)]
				lbls := map[string]string{"gpu": "true"}
				if i%2 == 1 {
					lbls = map[string]string{"bazel-remote": "true"}
				}
				prompt := fmt.Sprintf("direct task %d", i)
				kind := ""
				if i == 0 {
					prompt += " [need-approval]"
				}
				if i == 1 {
					prompt += " [need-reply]"
					kind = tasks.KindInteractive
				}
				body := map[string]any{
					"repo":        "bwsalmon/silo",
					"prompt":      prompt,
					"constraints": map[string]any{"labels": lbls},
				}
				if kind != "" {
					body["kind"] = kind
				}
				var tk tasks.Task
				if code := call(t, "POST", p.url+tasks.TasksPath, alice, body, &tk); code != http.StatusCreated {
					t.Fatalf("create direct task %d: %d", i, code)
				}
				// If require_approval happened to be true at the instant of creation, approve it immediately as bob.
				if tk.State == tasks.Proposed {
					_ = call(t, "POST", podB.url+tasks.TasksPath+"/"+tk.ID+"/approve", bob, nil, nil)
				}
				directIDs = append(directIDs, tk.ID)
				if i == 0 {
					approvalTaskID = tk.ID
				}
				if i == 1 {
					replyTaskID = tk.ID
				}
			}

			// Concurrently approve/reject all proposed tasks from multiple replicas and actors.
			var propWG sync.WaitGroup
			for idx, pr := range props {
				propWG.Add(1)
				go func(idx int, pr propSpec) {
					defer propWG.Done()
					var inner sync.WaitGroup
					switch pr.mode {
					case "approve":
						for a, actor := range []string{alice, bob, dave} {
							inner.Add(1)
							go func(a int, actor string) {
								defer inner.Done()
								p := pods[(idx+a)%len(pods)]
								code := call(t, "POST", p.url+tasks.TasksPath+"/"+pr.id+"/approve", actor, nil, nil)
								if code != http.StatusOK && code != http.StatusConflict {
									t.Errorf("approve %s by %s: unexpected status %d", pr.id, actor, code)
								}
							}(a, actor)
						}
					case "reject":
						for a, actor := range []string{alice, bob, dave} {
							inner.Add(1)
							go func(a int, actor string) {
								defer inner.Done()
								p := pods[(idx+a)%len(pods)]
								code := call(t, "POST", p.url+tasks.TasksPath+"/"+pr.id+"/reject", actor, map[string]string{"reason": "not needed"}, nil)
								if code != http.StatusOK && code != http.StatusConflict {
									t.Errorf("reject %s by %s: unexpected status %d", pr.id, actor, code)
								}
							}(a, actor)
						}
					case "race":
						inner.Add(2)
						go func() {
							defer inner.Done()
							code := call(t, "POST", podB.url+tasks.TasksPath+"/"+pr.id+"/approve", alice, nil, nil)
							if code != http.StatusOK && code != http.StatusConflict {
								t.Errorf("race approve %s: %d", pr.id, code)
							}
						}()
						go func() {
							defer inner.Done()
							code := call(t, "POST", podC.url+tasks.TasksPath+"/"+pr.id+"/reject", bob, map[string]string{"reason": "race reject"}, nil)
							if code != http.StatusOK && code != http.StatusConflict {
								t.Errorf("race reject %s: %d", pr.id, code)
							}
						}()
					}
					inner.Wait()
				}(idx, pr)
			}
			propWG.Wait()
			close(settingsStop)
			settingsWG.Wait()

			// Reset require_approval to false so any task that was approved is free to run.
			call(t, "PUT", podA.url+"/v1/teams/infra/settings", alice, map[string]any{
				"lost_requeue_after_seconds": 300,
				"anyone_may_propose":         true,
				"require_approval":           false,
			}, &struct{}{})

			// Answer the in-run approval_request and awaiting_reply from follower replicas concurrently!
			eventually(t, "approval_request event recorded", func() bool {
				_, ok, _ := podB.h.Events.ApprovalRequest(ctx, approvalTaskID, "req-"+approvalTaskID)
				return ok
			})
			var ansWG sync.WaitGroup
			var ansOK, ansConflict atomic.Int32
			for _, p := range []*replicaPod{podB, podC} {
				ansWG.Add(1)
				go func(p *replicaPod) {
					defer ansWG.Done()
					code := call(t, "POST", p.url+tasks.TasksPath+"/"+approvalTaskID+"/approvals/req-"+approvalTaskID, bob, map[string]string{"decision": tasks.Allow, "reason": "lgtm"}, nil)
					switch code {
					case http.StatusOK:
						ansOK.Add(1)
					case http.StatusConflict:
						ansConflict.Add(1)
					default:
						t.Errorf("answering approval_request on %s: %d", p.name, code)
					}
				}(p)
			}
			ansWG.Wait()
			if ansOK.Load() != 1 || ansConflict.Load() != 1 {
				t.Fatalf("expected 1 OK and 1 Conflict answering approval_request across replicas, got ok=%d conflict=%d", ansOK.Load(), ansConflict.Load())
			}

			eventually(t, "awaiting_reply event recorded", func() bool {
				awaiting, _, _, _, _, err := podC.h.Events.TaskInboxState(ctx, replyTaskID)
				return err == nil && awaiting
			})
			for idx, p := range []*replicaPod{podC, podB} {
				code := call(t, "POST", p.url+tasks.TasksPath+"/"+replyTaskID+"/messages", dave, map[string]string{"id": "msg-reply-1", "text": "yes proceed"}, nil)
				if idx == 0 && code != http.StatusCreated && code != http.StatusOK {
					t.Fatalf("posting interactive message on %s: %d", p.name, code)
				}
				if idx == 1 && code != http.StatusCreated && code != http.StatusOK && code != http.StatusConflict {
					t.Fatalf("posting duplicate interactive message on %s: %d", p.name, code)
				}
			}

			// Trigger dispatcher lease handoff mid-run: find the current leader pod and stop it!
			_, leaderName, err := podB.h.Leases.Holder(ctx, "dispatcher")
			if err != nil {
				t.Fatalf("Leases.Holder: %v", err)
			}
			var survivingPod *replicaPod
			for _, p := range pods {
				if p.name == leaderName {
					p.stop(t)
				} else if survivingPod == nil {
					survivingPod = p
				}
			}
			eventually(t, "dispatcher lease handoff to surviving replica", func() bool {
				_, holder, err := survivingPod.h.Leases.Holder(ctx, "dispatcher")
				return err == nil && holder != "" && holder != leaderName
			})

			// Wait for all approved proposed tasks and all direct tasks to reach Done, and rejected tasks to be Rejected.
			for _, pr := range props {
				switch pr.mode {
				case "approve":
					eventually(t, "approved task "+pr.id+" done", func() bool {
						return taskState(t, survivingPod.url, pr.id) == tasks.Done
					})
				case "reject":
					eventually(t, "rejected task "+pr.id+" rejected", func() bool {
						return taskState(t, survivingPod.url, pr.id) == tasks.Rejected
					})
				case "race":
					eventually(t, "raced task "+pr.id+" terminal", func() bool {
						st := taskState(t, survivingPod.url, pr.id)
						return st == tasks.Done || st == tasks.Rejected
					})
				}
			}
			for _, id := range directIDs {
				eventually(t, "direct task "+id+" done", func() bool {
					return taskState(t, survivingPod.url, id) == tasks.Done
				})
			}

			// Verify capability placement invariants: no node was ever offered a task whose labels it lacked.
			rejectedIDs := map[string]bool{}
			for _, pr := range props {
				if pr.mode == "reject" {
					rejectedIDs[pr.id] = true
				}
			}
			for _, simNode := range []*simulatedCapNode{nodeGPU, nodeBazel, nodeCombo} {
				simNode.mu.Lock()
				for id, spec := range simNode.offered {
					for k, wantV := range spec.Constraints.Labels {
						if simNode.caps[k] != wantV {
							t.Errorf("node %s (caps=%v) was offered task %s requiring label %s=%s",
								simNode.id, simNode.caps, id, k, wantV)
						}
					}
					if rejectedIDs[id] {
						t.Errorf("node %s was offered rejected task %s", simNode.id, id)
					}
				}
				simNode.mu.Unlock()
			}

			// Verify that the in-run approval and interactive reply reached the executing node.
			sawApproval := false
			sawReply := false
			for _, simNode := range []*simulatedCapNode{nodeGPU, nodeBazel, nodeCombo} {
				simNode.mu.Lock()
				if simNode.approvals[approvalTaskID] == tasks.Allow {
					sawApproval = true
				}
				if simNode.replies[replyTaskID] == "yes proceed" {
					sawReply = true
				}
				simNode.mu.Unlock()
			}
			if !sawApproval {
				t.Errorf("in-run approval_request for %s was not delivered to its node", approvalTaskID)
			}
			if !sawReply {
				t.Errorf("in-run awaiting_reply message for %s was not delivered to its node", replyTaskID)
			}
			cancel()
		})
	}
}

// streamingTransferNode accepts task offers, serves slow-streaming artifact views,
// and serves a nodeui HTTP + WebSocket echo server so token revocation and rotation
// can be tested while streams are actively transferring data across replicas.
type streamingTransferNode struct {
	id   string
	mu   sync.Mutex
	seen []string
}

func (n *streamingTransferNode) handle(c net.Conn) {
	br := bufio.NewReader(c)
	line, err := br.ReadBytes('\n')
	if err != nil {
		c.Close()
		return
	}
	var m struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(line, &m)
	n.mu.Lock()
	n.seen = append(n.seen, m.Type)
	n.mu.Unlock()

	switch m.Type {
	case live.TypeView:
		var v live.ViewMessage
		_ = json.Unmarshal(line, &v)
		live.ServeView(c, v, func(v live.ViewMessage) (io.ReadCloser, error) {
			pr, pw := io.Pipe()
			go func() {
				defer pw.Close()
				for i := 0; i < 200; i++ {
					if _, err := fmt.Fprintf(pw, "chunk-%03d-from-%s\n", i, n.id); err != nil {
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
			}()
			return pr, nil
		})
	case nodeui.TypeHTTP:
		var u nodeui.Message
		_ = json.Unmarshal(line, &u)
		mux := http.NewServeMux()
		mux.Handle("/ws", websocket.Handler(func(ws *websocket.Conn) {
			_, _ = io.Copy(ws, ws)
		}))
		mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			fl, _ := w.(http.Flusher)
			for i := 0; i < 200; i++ {
				if _, err := fmt.Fprintf(w, "ui-line-%d\n", i); err != nil {
					return
				}
				if fl != nil {
					fl.Flush()
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
		nodeui.Serve(c, u, mux)
	default:
		defer c.Close()
		if m.Type == tasks.TypeOffer || m.Type == tasks.TypeResume {
			b, _ := json.Marshal(tasks.Message{Type: tasks.TypeAccept, NodeTask: "local-1"})
			_, _ = c.Write(append(b, '\n'))
		}
		for {
			if _, err := br.ReadBytes('\n'); err != nil {
				return
			}
		}
	}
}

func dialNodeUIWS(hubURL, nodeID, user string) (*websocket.Conn, error) {
	u, err := url.Parse(hubURL)
	if err != nil {
		return nil, err
	}
	host := nodeID + ".nodes.hub.test"
	cfg, err := websocket.NewConfig("ws://"+host+"/ws", "http://"+host)
	if err != nil {
		return nil, err
	}
	cfg.Header.Set(userHeader, user)
	tc, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		return nil, err
	}
	ws, err := websocket.NewClient(cfg, tc)
	if err != nil {
		tc.Close()
		return nil, err
	}
	return ws, nil
}

// TestConcurrentRendezvousTokenRevocationAndStreamRaces exercises Scope 2:
//   - Mint, rotate, and revoke node tokens (POST /v1/node-tokens, DELETE /v1/node-tokens/{id},
//     DELETE /v1/node-tokens/nodes/{node}) across replicas while hub/live event streams,
//     artifact view-through streams, and hub/nodeui proxied WebSocket connections are
//     actively transferring data, verifying clean disconnection with no goroutine leaks,
//     deadlocks, or data races.
func TestConcurrentRendezvousTokenRevocationAndStreamRaces(t *testing.T) {
	backends := []struct {
		name string
		url  func(t *testing.T) string
	}{
		{
			name: "sqlite",
			url: func(t *testing.T) string {
				return "sqlite:" + filepath.Join(t.TempDir(), "hub.db")
			},
		},
		{
			name: "postgres",
			url: func(t *testing.T) string {
				return pgtest.Start(t).URL
			},
		},
	}

	for _, be := range backends {
		t.Run(be.name, func(t *testing.T) {
			baseGoroutines := runtime.NumGoroutine()
			dbURL := be.url(t)
			key := []byte("0123456789abcdef0123456789abcdef")

			podA := startReplicaPod(t, dbURL, "pod-a", key)
			podB := startReplicaPod(t, dbURL, "pod-b", key)

			const (
				admin = "admin@example.com"
				alice = "alice@example.com"
				bob   = "bob@example.com"
			)

			call(t, "POST", podA.url+"/v1/teams", admin, map[string]string{"id": "infra", "name": "Infra"}, &struct{}{})
			call(t, "PUT", podA.url+"/v1/teams/infra/members/user:"+alice, admin, map[string]string{"role": "admin"}, &struct{}{})
			call(t, "PUT", podA.url+"/v1/teams/infra/members/user:"+bob, admin, map[string]string{"role": "member"}, &struct{}{})
			call(t, "PUT", podA.url+"/v1/teams/infra/repos/bwsalmon/silo", admin, nil, &struct{}{})

			type mintedResp struct {
				NodeToken directory.NodeToken `json:"node_token"`
				Token     string              `json:"token"`
			}
			mint := func(podURL, user, nodeID string) mintedResp {
				var out mintedResp
				if code := call(t, "POST", podURL+"/v1/node-tokens", user, map[string]string{"node": nodeID}, &out); code != http.StatusCreated {
					t.Fatalf("mint token for %s on %s: %d", nodeID, podURL, code)
				}
				return out
			}

			tok1 := mint(podA.url, alice, "ws-revoke-id")
			tok2 := mint(podB.url, alice, "ws-rotate")
			tok3 := mint(podA.url, bob, "ws-release-node")

			connectNode := func(podURL, nodeID, tok string) (*rendezvous.Conn, *streamingTransferNode, chan struct{}) {
				sn := &streamingTransferNode{id: nodeID}
				cl := &rendezvous.Client{
					Hub:   podURL,
					Token: func(context.Context) (string, error) { return tok, nil },
					Node: registry.Node{
						ID:    nodeID,
						Repos: []string{"bwsalmon/silo"},
						Slots: 2,
						UI:    &registry.NodeUI{Offered: true},
						Labels: map[string]string{
							"target": nodeID,
						},
					},
				}
				c, err := cl.Connect(context.Background())
				if err != nil {
					t.Fatalf("connect %s: %v", nodeID, err)
				}
				closed := make(chan struct{})
				go func() {
					defer close(closed)
					for {
						st, err := c.Accept()
						if err != nil {
							return
						}
						go sn.handle(st)
					}
				}()
				return c, sn, closed
			}

			// Connect ws-revoke-id and ws-rotate to podA, and ws-release-node to podB.
			conn1, _, closed1 := connectNode(podA.url, "ws-revoke-id", tok1.Token)
			defer conn1.Close()
			conn2, _, closed2 := connectNode(podA.url, "ws-rotate", tok2.Token)
			defer conn2.Close()
			conn3, _, closed3 := connectNode(podB.url, "ws-release-node", tok3.Token)
			defer conn3.Close()

			for _, id := range []string{"ws-revoke-id", "ws-rotate", "ws-release-node"} {
				eventually(t, id+" online on both pods", func() bool {
					return online(t, podA.url, id) && online(t, podB.url, id)
				})
			}

			// File one task pinned to each node so we have active live streams and artifact view-throughs.
			startTaskOn := func(nodeID string, conn *rendezvous.Conn) (tasks.Task, net.Conn) {
				var tk tasks.Task
				code := call(t, "POST", podB.url+tasks.TasksPath, alice, map[string]any{
					"repo":        "bwsalmon/silo",
					"prompt":      "stream stress on " + nodeID,
					"constraints": map[string]any{"node": nodeID},
				}, &tk)
				if code != http.StatusCreated {
					t.Fatalf("create task for %s: %d", nodeID, code)
				}
				eventually(t, "task "+tk.ID+" running", func() bool {
					return taskState(t, podA.url, tk.ID) == tasks.Running
				})
				up, err := conn.Open(context.Background())
				if err != nil {
					t.Fatalf("open upstream event stream on %s: %v", nodeID, err)
				}
				// Emit an artifact event so ref "art-1" is registered in task_refs.
				fmt.Fprintf(up, `{"v":1,"seq":1,"type":"artifact","task":%q,"artifact":{"kind":"diff","ref":"art-1","title":"diff"}}`+"\n", tk.ID)
				eventually(t, "ref art-1 stored for "+tk.ID, func() bool {
					ok, _ := podB.h.Events.HasRef(context.Background(), tk.ID, "art-1")
					return ok
				})
				return tk, up
			}

			tk1, up1 := startTaskOn("ws-revoke-id", conn1)
			defer up1.Close()
			tk2, up2 := startTaskOn("ws-rotate", conn2)
			defer up2.Close()
			tk3, up3 := startTaskOn("ws-release-node", conn3)
			defer up3.Close()

			// Start active data transfers (live event emission, SSE subscriber, artifact view-through, and nodeui WebSocket)
			// across BOTH the holding pod and the forwarding pod for each node.
			var streamWG sync.WaitGroup
			startActiveStreams := func(nodeID string, owner string, tk tasks.Task, up net.Conn, closed <-chan struct{}) {
				// 1. Continuous upstream live events from node.
				streamWG.Add(1)
				go func() {
					defer streamWG.Done()
					for seq := int64(2); seq < 500; seq++ {
						select {
						case <-closed:
							return
						default:
						}
						if _, err := fmt.Fprintf(up, `{"v":1,"seq":%d,"type":"transcript","task":%q,"transcript":{"kind":"text","text":"chunk"}}`+"\n", seq, tk.ID); err != nil {
							return
						}
						time.Sleep(8 * time.Millisecond)
					}
				}()

				// 2. Downstream SSE subscriber on the opposite pod.
				streamWG.Add(1)
				go func() {
					defer streamWG.Done()
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					req, _ := http.NewRequestWithContext(ctx, "GET", podB.url+tasks.TasksPath+"/"+tk.ID+"/events", nil)
					req.Header.Set(userHeader, owner)
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						return
					}
					defer resp.Body.Close()
					_, _ = io.Copy(io.Discard, resp.Body)
				}()

				// 3. Artifact view-through stream via forwarding pod.
				streamWG.Add(1)
				go func() {
					defer streamWG.Done()
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					req, _ := http.NewRequestWithContext(ctx, "GET", podB.url+tasks.TasksPath+"/"+tk.ID+"/artifacts?ref=art-1", nil)
					req.Header.Set(userHeader, owner)
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						return
					}
					defer resp.Body.Close()
					_, _ = io.Copy(io.Discard, resp.Body)
				}()

				// 4. Proxied nodeui WebSocket connection via both pods.
				for _, p := range []*replicaPod{podA, podB} {
					ws, err := dialNodeUIWS(p.url, nodeID, owner)
					if err != nil {
						t.Errorf("dialNodeUIWS(%s, %s): %v", p.name, nodeID, err)
						continue
					}
					// Verify at least one frame echoes before revocation.
					_ = ws.SetDeadline(time.Now().Add(5 * time.Second))
					if err := websocket.Message.Send(ws, "ping"); err != nil {
						ws.Close()
						t.Errorf("ws send before revoke on %s/%s: %v", p.name, nodeID, err)
						continue
					}
					var reply string
					if err := websocket.Message.Receive(ws, &reply); err != nil || reply != "ping" {
						ws.Close()
						t.Errorf("ws receive before revoke on %s/%s: %q, %v", p.name, nodeID, reply, err)
						continue
					}
					streamWG.Add(1)
					go func(ws *websocket.Conn) {
						defer streamWG.Done()
						defer ws.Close()
						for i := 0; i < 200; i++ {
							_ = ws.SetDeadline(time.Now().Add(2 * time.Second))
							if err := websocket.Message.Send(ws, "data-frame"); err != nil {
								return
							}
							var got string
							if err := websocket.Message.Receive(ws, &got); err != nil {
								return
							}
							time.Sleep(8 * time.Millisecond)
						}
					}(ws)
				}
			}

			startActiveStreams("ws-revoke-id", alice, tk1, up1, closed1)
			startActiveStreams("ws-rotate", alice, tk2, up2, closed2)
			startActiveStreams("ws-release-node", bob, tk3, up3, closed3)

			// Give streams 50ms of active transfer before firing concurrent revocations and rotations across pods.
			time.Sleep(50 * time.Millisecond)

			var revokeWG sync.WaitGroup
			var rotatedToken atomic.Value

			// 1. Revoke ws-revoke-id by token ID on podB (while ws-revoke-id is held on podA!).
			revokeWG.Add(1)
			go func() {
				defer revokeWG.Done()
				code := call(t, "DELETE", podB.url+"/v1/node-tokens/"+tok1.NodeToken.ID, alice, nil, nil)
				if code != http.StatusNoContent && code != http.StatusNotFound {
					t.Errorf("DELETE /v1/node-tokens/%s on podB: %d", tok1.NodeToken.ID, code)
				}
			}()

			// 2. Concurrently rotate ws-rotate's token from both podA and podB by the same owner (alice).
			for _, p := range []*replicaPod{podA, podB} {
				revokeWG.Add(1)
				go func(p *replicaPod) {
					defer revokeWG.Done()
					var out mintedResp
					code := call(t, "POST", p.url+"/v1/node-tokens", alice, map[string]string{"node": "ws-rotate"}, &out)
					if code != http.StatusCreated {
						t.Errorf("concurrent rotate ws-rotate on %s: %d", p.name, code)
						return
					}
					rotatedToken.Store(out.Token)
				}(p)
			}

			// 3. Release/revoke ws-release-node by node ID on podA (while ws-release-node is held on podB!).
			revokeWG.Add(1)
			go func() {
				defer revokeWG.Done()
				code := call(t, "DELETE", podA.url+"/v1/node-tokens/nodes/ws-release-node", bob, nil, nil)
				if code != http.StatusNoContent {
					t.Errorf("DELETE /v1/node-tokens/nodes/ws-release-node on podA: %d", code)
				}
			}()

			revokeWG.Wait()

			// All three nodes' connections must be cleanly disconnected across replicas!
			for name, ch := range map[string]<-chan struct{}{
				"ws-revoke-id":    closed1,
				"ws-rotate":       closed2,
				"ws-release-node": closed3,
			} {
				select {
				case <-ch:
				case <-time.After(10 * time.Second):
					t.Fatalf("node %s did not disconnect after token revocation/rotation", name)
				}
			}

			// Wait for all active live/artifact/nodeui stream goroutines to exit cleanly.
			streamDone := make(chan struct{})
			go func() {
				streamWG.Wait()
				close(streamDone)
			}()
			select {
			case <-streamDone:
			case <-time.After(10 * time.Second):
				t.Fatal("active streams did not terminate cleanly after token revocation")
			}

			// Verify reconnecting with old tokens fails with 401/error.
			for name, oldTok := range map[string]string{
				"ws-revoke-id":    tok1.Token,
				"ws-rotate":       tok2.Token,
				"ws-release-node": tok3.Token,
			} {
				badClient := &rendezvous.Client{
					Hub:   podA.url,
					Token: func(context.Context) (string, error) { return oldTok, nil },
					Node:  registry.Node{ID: name, Repos: []string{"bwsalmon/silo"}, Slots: 1},
				}
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
				if c, err := badClient.Connect(cctx); err == nil {
					c.Close()
					t.Errorf("node %s reconnected with revoked/rotated token", name)
				}
				ccancel()
			}

			// Verify that after releasing ws-release-node, another team member (alice) can now claim and mint for ws-release-node.
			reclaimed := mint(podB.url, alice, "ws-release-node")
			connReclaimed, _, closedReclaimed := connectNode(podA.url, "ws-release-node", reclaimed.Token)
			eventually(t, "ws-release-node online under new owner alice", func() bool {
				return online(t, podB.url, "ws-release-node")
			})
			connReclaimed.Close()
			<-closedReclaimed

			podA.stop(t)
			podB.stop(t)

			// Verify no goroutine leaks after hubs and streams close.
			eventually(t, "goroutines settled", func() bool {
				runtime.GC()
				return runtime.NumGoroutine() <= baseGoroutines+12
			})
		})
	}
}

// TestConcurrentDatabaseContentionNoLostUpdates exercises Scope 3:
//   - High-concurrency SQLite SQLITE_BUSY and Postgres serialization contention across
//     hub/directory, hub/tasks, hub/live, and hub/grainfiles, verifying that concurrent
//     writers never drop updates, clobber orthogonal user preferences, or leave partial state.
func TestConcurrentDatabaseContentionNoLostUpdates(t *testing.T) {
	backends := []struct {
		name string
		url  func(t *testing.T) string
	}{
		{
			name: "sqlite",
			url: func(t *testing.T) string {
				return "sqlite:" + filepath.Join(t.TempDir(), "contention.db")
			},
		},
		{
			name: "postgres",
			url: func(t *testing.T) string {
				return pgtest.Start(t).URL
			},
		},
	}

	for _, be := range backends {
		t.Run(be.name, func(t *testing.T) {
			ctx := context.Background()
			dbURL := be.url(t)

			dirStore, err := directory.Open(ctx, dbURL)
			if err != nil {
				t.Fatalf("directory.Open: %v", err)
			}
			defer dirStore.Close()

			taskStore, err := tasks.Open(ctx, dbURL)
			if err != nil {
				t.Fatalf("tasks.Open: %v", err)
			}
			defer taskStore.Close()

			liveStore, err := live.Open(ctx, dbURL)
			if err != nil {
				t.Fatalf("live.Open: %v", err)
			}
			defer liveStore.Close()

			gfStore, err := grainfiles.Open(ctx, dbURL)
			if err != nil {
				t.Fatalf("grainfiles.Open: %v", err)
			}
			defer gfStore.Close()

			if err := dirStore.CreateTeam(ctx, "infra", "Infra"); err != nil {
				t.Fatalf("CreateTeam: %v", err)
			}
			if err := dirStore.CreateTeam(ctx, "other", "Other"); err != nil {
				t.Fatalf("CreateTeam other: %v", err)
			}

			var wg sync.WaitGroup

			// 1. hub/directory contention:
			//    - Concurrent SetUserTheme("dark") and SetUserInboxNodes(["node-a","node-b"]) for the SAME user:
			//      both columns must be preserved without clobbering each other!
			//    - Concurrent SetTeamSettings updates.
			//    - Concurrent SetMember for 20 members.
			//    - Concurrent AssignRepo by the same team (must all succeed idempotently).
			//    - Concurrent MintNodeToken rotations for the same node by the same owner (must all succeed).
			const prefUser = "prefs@example.com"
			wg.Add(2)
			go func() {
				defer wg.Done()
				for i := 0; i < 20; i++ {
					if _, err := dirStore.SetUserTheme(ctx, prefUser, "dark"); err != nil {
						t.Errorf("SetUserTheme(%d): %v", i, err)
					}
				}
			}()
			go func() {
				defer wg.Done()
				for i := 0; i < 20; i++ {
					if _, err := dirStore.SetUserInboxNodes(ctx, prefUser, []string{"node-b", "node-a"}); err != nil {
						t.Errorf("SetUserInboxNodes(%d): %v", i, err)
					}
				}
			}()

			for g := 0; g < 8; g++ {
				wg.Add(1)
				go func(g int) {
					defer wg.Done()
					member := fmt.Sprintf("user:member-%d@example.com", g)
					if err := dirStore.SetMember(ctx, "infra", member, directory.Member); err != nil {
						t.Errorf("SetMember(%d): %v", g, err)
					}
					if err := dirStore.SetTeamSettings(ctx, "infra", directory.Settings{
						LostRequeueAfter: 5 * time.Minute,
						AnyoneMayPropose: g%2 == 0,
						RequireApproval:  g%3 == 0,
					}); err != nil {
						t.Errorf("SetTeamSettings(%d): %v", g, err)
					}
					if err := dirStore.AssignRepo(ctx, "infra", "bwsalmon/silo"); err != nil {
						t.Errorf("AssignRepo same team(%d): %v", g, err)
					}
					if _, _, err := dirStore.MintNodeToken(ctx, "alice@example.com", "shared-rotate-node"); err != nil {
						t.Errorf("MintNodeToken same owner(%d): %v", g, err)
					}
				}(g)
			}

			// 2. hub/tasks + hub/live contention:
			//    - Concurrently create, propose, approve, offer, accept, post messages, answer approvals,
			//      append live events, add refs, and finish 16 tasks.
			for i := 0; i < 16; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					tk, err := taskStore.Propose(ctx, tasks.Spec{
						Team:      "infra",
						Repo:      "bwsalmon/silo",
						Kind:      tasks.KindInteractive,
						Title:     fmt.Sprintf("Task %d", i),
						Prompt:    fmt.Sprintf("Run contention workload %d", i),
						Submitter: "erin@example.com",
					})
					if err != nil {
						t.Errorf("Propose(%d): %v", i, err)
						return
					}
					if _, err := taskStore.Approve(ctx, tk.ID, "alice@example.com"); err != nil {
						t.Errorf("Approve(%d): %v", i, err)
						return
					}
					if _, err := taskStore.Offer(ctx, tk.ID, "node-1", "alice@example.com"); err != nil {
						t.Errorf("Offer(%d): %v", i, err)
						return
					}
					if _, err := taskStore.Accept(ctx, tk.ID, fmt.Sprintf("local-%d", i)); err != nil {
						t.Errorf("Accept(%d): %v", i, err)
						return
					}
					for seq := int64(1); seq <= 5; seq++ {
						ev := live.Event{
							Task: tk.ID,
							Event: events.Event{
								V:    1,
								Seq:  seq,
								Type: events.TypeStatus,
								Time: time.Now().UTC(),
								Status: &events.Status{
									Phase:   "running",
									State:   "info",
									Message: fmt.Sprintf("step %d", seq),
								},
							},
						}
						if err := liveStore.Append(ctx, ev, live.Lifecycle); err != nil {
							t.Errorf("liveStore.Append(%d, %d): %v", i, seq, err)
						}
						ref := fmt.Sprintf("ref-%d-%d", i, seq)
						if err := liveStore.AddRef(ctx, tk.ID, ref, seq); err != nil {
							t.Errorf("liveStore.AddRef(%d, %d): %v", i, seq, err)
						}
					}
					if _, _, err := taskStore.Post(ctx, tasks.TaskMessage{
						Task:   tk.ID,
						ID:     fmt.Sprintf("msg-%d", i),
						Text:   "hello interactive",
						Sender: "alice@example.com",
					}); err != nil {
						t.Errorf("Post(%d): %v", i, err)
					}
					if _, err := taskStore.Answer(ctx, tasks.Answer{
						Task:       tk.ID,
						Request:    fmt.Sprintf("req-%d", i),
						Decision:   tasks.Allow,
						AnsweredBy: "alice@example.com",
						Reason:     "approved under contention",
					}); err != nil {
						t.Errorf("Answer(%d): %v", i, err)
					}
					if _, err := taskStore.Finish(ctx, tk.ID, tasks.Done, "finished under contention"); err != nil {
						t.Errorf("Finish(%d): %v", i, err)
					}
				}(i)
			}

			// 3. hub/grainfiles contention:
			//    - Concurrently index grainfiles, register subscriptions, advance cursors, and record completions.
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					path := fmt.Sprintf(".grain/tasks/t%d.task.md", i)
					rec, err := gfStore.IndexGrainfile(ctx, &grainfiles.Grainfile{
						Grainfile: grainfiles.VersionV1,
						Kind:      grainfiles.KindTask,
						Repo:      "https://github.com/bwsalmon/silo",
						Path:      path,
						Commit:    fmt.Sprintf("deadbeef%02d", i),
						Ref:       "refs/heads/main",
						Title:     fmt.Sprintf("Contention Task %d", i),
						Recipient: "https://github.com/bwsalmon/silo",
						Body:      fmt.Sprintf("Prompt %d", i),
					})
					if err != nil {
						t.Errorf("IndexGrainfile(%d): %v", i, err)
						return
					}
					nodeID := fmt.Sprintf("gf-node-%d", i)
					if _, err := gfStore.SetSubscriptions(ctx, grainfiles.Subscriptions{
						NodeID:     nodeID,
						Owner:      "alice@example.com",
						Recipients: []string{"https://github.com/bwsalmon/silo"},
					}); err != nil {
						t.Errorf("SetSubscriptions(%d): %v", i, err)
						return
					}
					if err := gfStore.AdvanceCursor(ctx, nodeID, int64(i+1)); err != nil {
						t.Errorf("AdvanceCursor(%d): %v", i, err)
					}
					if _, err := gfStore.RecordDependencyCompleted(ctx, rec.URL, rec.CommitSHA, "done", "ok"); err != nil {
						t.Errorf("RecordDependencyCompleted(%d): %v", i, err)
					}
				}(i)
			}

			wg.Wait()

			// Verify no lost updates on user preferences!
			prefs, err := dirStore.UserPreferences(ctx, prefUser)
			if err != nil {
				t.Fatalf("GetUserPreferences: %v", err)
			}
			if prefs.Theme != "dark" {
				t.Errorf("expected theme 'dark', got %q (clobbered by concurrent SetUserInboxNodes!)", prefs.Theme)
			}
			if len(prefs.InboxNodes) != 2 || prefs.InboxNodes[0] != "node-b" || prefs.InboxNodes[1] != "node-a" || !prefs.InboxConfigured {
				t.Errorf("expected inbox_nodes [node-b node-a] configured=true, got %+v (clobbered by concurrent SetUserTheme!)", prefs)
			}

			// Verify all 8 members and repo ownership are present in directory.
			teams, err := dirStore.Teams(ctx)
			if err != nil {
				t.Fatalf("Teams: %v", err)
			}
			var infraTeam *directory.Team
			for i := range teams {
				if teams[i].ID == "infra" {
					infraTeam = &teams[i]
				}
			}
			if infraTeam == nil || len(infraTeam.Members) != 8 {
				t.Errorf("expected 8 members in infra team, got %+v", infraTeam)
			}

			// Verify all 16 tasks reached Done and all 80 live events were stored.
			doneTasks, err := taskStore.List(ctx, tasks.Filter{States: []tasks.State{tasks.Done}})
			if err != nil {
				t.Fatalf("taskStore.List: %v", err)
			}
			if len(doneTasks) != 16 {
				t.Errorf("expected 16 done tasks, got %d", len(doneTasks))
			}
			evCount, _, err := liveStore.Size(ctx)
			if err != nil {
				t.Fatalf("liveStore.Size: %v", err)
			}
			if evCount != 80 {
				t.Errorf("expected 80 stored live events, got %d", evCount)
			}
		})
	}
}
