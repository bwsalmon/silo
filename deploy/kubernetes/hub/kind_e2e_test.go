package hub_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/hub/grainfiles"
	"github.com/bwsalmon/silo/hub/identity"
	"github.com/bwsalmon/silo/hub/nodeui"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
	"github.com/bwsalmon/silo/hub/tasks"
)

const (
	kindCommitA         = "1111111111111111111111111111111111111111"
	kindCommitB         = "2222222222222222222222222222222222222222"
	kindRepoSlug        = "bwsalmon/silo"
	kindRepoURL         = "https://github.com/bwsalmon/silo"
	kindRecipientURL    = "https://github.com/bwsalmon/silo"
	kindNodeID          = "kind-node-1"
	kindUserEmail       = "alice@example.com"
	kindOIDCSessionKey  = "0123456789abcdef0123456789abcdef"
	kindWebhookSecret   = "kind-e2e-webhook-secret"
	kindInstallationTok = "ghs_kind_installation_token"
	kindTaskPath        = "tasks/01-deploy.task.md"
	kindRolloutTaskPath = "tasks/02-rollout.task.md"
	kindReferencedDoc   = "docs/arch.md"
	kindUnlistedDoc     = "docs/unlisted.md"
	kindTaskContent     = "---\ntitle: Deploy service on kind\nrecipient: https://github.com/bwsalmon/silo\nrequestor: Alice <alice@example.com>\nreferences:\n  - /docs/arch.md\n---\nDeploy the service according to docs/arch.md.\n"
	kindRolloutContent  = "---\ntitle: Verify rolling update on kind\nrecipient: https://github.com/bwsalmon/silo\nrequestor: Alice <alice@example.com>\nreferences:\n  - /docs/arch.md\n---\nVerify the rolling update completed with zero downtime.\n"
	kindArchDocContent  = "# Architecture\n\nKind 2-replica cluster architecture notes.\n"
	kindUnlistedContent = "# Unlisted\n\nThis file is not listed in the grainfile references.\n"
	kindNodeUIHost      = "kind-node-1.nodes.silo-hub.kind.test"
	kindNodeUIBody      = "kind-node-1 live UI response"
)

func TestMain(m *testing.M) {
	for _, arg := range os.Args[1:] {
		if addr, ok := strings.CutPrefix(arg, "-addr="); ok {
			if err := serveGitHubMock(addr); err != nil {
				log.Fatal(err)
			}
			return
		}
	}
	os.Exit(m.Run())
}

// serveGitHubMock runs the in-cluster GitHub REST API mock used by the Hub
// pods in deploy/kubernetes/hub/kind to mint GitHub App installation tokens
// and fetch Grainfiles and referenced files.
func serveGitHubMock(addr string) error {
	files := map[string]string{
		"/repos/bwsalmon/silo/contents/" + kindTaskPath + "@" + kindCommitA:        kindTaskContent,
		"/repos/bwsalmon/silo/contents/" + kindReferencedDoc + "@" + kindCommitA:   kindArchDocContent,
		"/repos/bwsalmon/silo/contents/" + kindUnlistedDoc + "@" + kindCommitA:     kindUnlistedContent,
		"/repos/bwsalmon/silo/contents/" + kindRolloutTaskPath + "@" + kindCommitB: kindRolloutContent,
		"/repos/bwsalmon/silo/contents/" + kindReferencedDoc + "@" + kindCommitB:   kindArchDocContent,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "missing app bearer JWT", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	})
	mux.HandleFunc("POST /app/installations/1/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "missing app bearer JWT", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      kindInstallationTok,
			"expires_at": time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		})
	})
	mux.HandleFunc("GET /repos/", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+kindInstallationTok {
			http.Error(w, "unexpected authorization: "+got, http.StatusUnauthorized)
			return
		}
		key := r.URL.Path + "@" + r.URL.Query().Get("ref")
		content, ok := files[key]
		if !ok {
			http.Error(w, "not found: "+key, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":     "file",
			"encoding": "base64",
			"size":     len(content),
			"sha":      "blobsha",
			"content":  base64.StdEncoding.EncodeToString([]byte(content)),
		})
	})
	return http.ListenAndServe(addr, mux)
}

func TestKindClusterE2E(t *testing.T) {
	if os.Getenv("SILO_E2E_KIND") == "" {
		t.Skip("set SILO_E2E_KIND=1 (via deploy/kubernetes/hub/e2e-kind.sh) to run the live kind cluster E2E test")
	}
	kubeContext := os.Getenv("SILO_KIND_CONTEXT")
	if kubeContext == "" {
		kubeContext = "kind-silo-hub-e2e"
	}
	nodeToken := strings.TrimSpace(os.Getenv("SILO_KIND_NODE_TOKEN"))
	if nodeToken == "" {
		t.Fatal("SILO_KIND_NODE_TOKEN must be set to the token minted by `/silo hub token`")
	}
	webhookSecret := strings.TrimSpace(os.Getenv("SILO_KIND_WEBHOOK_SECRET"))
	if webhookSecret == "" {
		webhookSecret = kindWebhookSecret
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// 1. Inspect the two live silo-hub pods and start port-forwards to both.
	pods := listReadyHubPods(t, kubeContext)
	if len(pods) != 2 {
		t.Fatalf("expected 2 ready silo-hub pods, got %d: %v", len(pods), pods)
	}
	t.Logf("live silo-hub pods: %s and %s", pods[0], pods[1])

	pod0HTTP := portForward(t, kubeContext, pods[0], 8080)
	pod0Metrics := portForward(t, kubeContext, pods[0], 9091)
	pod1HTTP := portForward(t, kubeContext, pods[1], 8080)
	pod1Metrics := portForward(t, kubeContext, pods[1], 9091)

	for name, baseURL := range map[string]string{pods[0]: pod0HTTP, pods[1]: pod1HTTP} {
		assertStatus(t, "GET", baseURL+"/healthz", "", nil, http.StatusOK)
		assertStatus(t, "GET", baseURL+"/readyz", "", nil, http.StatusOK)
		_, uiBody := doRequest(t, "GET", baseURL+"/", "", "", nil)
		if !strings.Contains(uiBody, `type="module"`) {
			t.Fatalf("pod %s GET / did not serve embedded UI bundle: %q", name, uiBody)
		}
	}

	// 2. Connect a simulated Grain node to pod 0 over WebSocket (/v1/nodes/connect)
	// subscribing to recipient URL and repository.
	node := newSimulatedNode(t, pod0HTTP, nodeToken)
	conn0, err := node.client.Connect(ctx)
	if err != nil {
		t.Fatalf("connecting simulated Grain node to %s: %v", pods[0], err)
	}
	defer conn0.Close()
	go node.serveStreams(conn0)

	subs := conn0.Subscriptions()
	if subs == nil || len(subs.Recipients) != 1 || subs.Recipients[0] != kindRecipientURL {
		t.Fatalf("welcome subscriptions = %+v, want recipient %s", subs, kindRecipientURL)
	}

	sessionCookie := mintOIDCSessionCookie(t, kindUserEmail, kindOIDCSessionKey)

	// Verify the node is visible as online across BOTH pods via GET /v1/nodes
	// (testing PostgreSQL LISTEN/NOTIFY and shared registry).
	for _, podURL := range []string{pod0HTTP, pod1HTTP} {
		waitFor(t, "node visible on "+podURL, 15*time.Second, func() bool {
			code, body := doRequest(t, "GET", podURL+"/v1/nodes", sessionCookie, "", nil)
			if code != http.StatusOK {
				return false
			}
			var dir struct {
				Nodes []registry.Entry `json:"nodes"`
			}
			if err := json.Unmarshal([]byte(body), &dir); err != nil {
				return false
			}
			for _, e := range dir.Nodes {
				if e.ID == kindNodeID && e.Status == registry.Online {
					return true
				}
			}
			return false
		})
	}

	// 3. Verify cross-pod internal streams (:9092 with silo-hub-internal Secret)
	// and PostgreSQL LISTEN/NOTIFY between pod 0 and pod 1.
	// 3a. Request kind-node-1's Node UI on pod 1 (while kind-node-1 is held by pod 0).
	code, uiResp := doRequest(t, "GET", pod1HTTP+"/status", sessionCookie, kindNodeUIHost, nil)
	if code != http.StatusOK || uiResp != kindNodeUIBody {
		t.Fatalf("cross-pod node UI proxy via pod 1 = %d %q, want 200 %q", code, uiResp, kindNodeUIBody)
	}

	// 3b. Submit a task on pod 1 and subscribe to its live SSE stream on pod 1 while
	// the node is connected to pod 0.
	taskPayload, _ := json.Marshal(map[string]string{
		"repo":   kindRepoSlug,
		"prompt": "verify cross-pod dispatch and SSE streaming",
	})
	code, taskResp := doRequest(t, "POST", pod1HTTP+tasks.TasksPath, sessionCookie, "", taskPayload)
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("POST /v1/tasks on pod 1 = %d: %s", code, taskResp)
	}
	var createdTask tasks.Task
	if err := json.Unmarshal([]byte(taskResp), &createdTask); err != nil || createdTask.ID == "" {
		t.Fatalf("decoding created task: %v (%s)", err, taskResp)
	}

	waitFor(t, "task offered to node on pod 0", 15*time.Second, func() bool {
		return node.acceptedTask() == createdTask.ID
	})
	if err := node.emitAndFinish(ctx, conn0, createdTask.ID); err != nil {
		t.Fatalf("streaming task events from node: %v", err)
	}
	waitFor(t, "task completed on pod 1", 15*time.Second, func() bool {
		c, b := doRequest(t, "GET", pod1HTTP+tasks.TasksPath+"/"+createdTask.ID, sessionCookie, "", nil)
		if c != http.StatusOK {
			return false
		}
		var tk tasks.Task
		_ = json.Unmarshal([]byte(b), &tk)
		return tk.State == tasks.Done
	})

	// 4. End-to-End Webhook, Indexing & Content Proxy on kind.
	pushPayloadA := mustWebhookPayload(kindCommitA, kindTaskPath, kindReferencedDoc)

	// 4a. Reject unsigned and invalid-signature webhooks with 401.
	if got := postWebhook(t, pod1HTTP, pushPayloadA, ""); got != http.StatusUnauthorized {
		t.Fatalf("webhook without signature = %d, want 401", got)
	}
	if got := postWebhook(t, pod1HTTP, pushPayloadA, "sha256=deadbeef"); got != http.StatusUnauthorized {
		t.Fatalf("webhook with bad signature = %d, want 401", got)
	}

	// 4b. Deliver a valid signed push webhook to pod 1 (not holding the node).
	sigA := signWebhook(webhookSecret, pushPayloadA)
	if got := postWebhook(t, pod1HTTP, pushPayloadA, sigA); got != http.StatusOK {
		t.Fatalf("signed webhook to pod 1 = %d, want 200", got)
	}

	// Verify pod 1 indexes the Grainfile and notifies pod 0 over LISTEN/NOTIFY,
	// which delivers grainfile_event (task_ready) to the connected node.
	var firstEvent grainfiles.Event
	select {
	case firstEvent = <-conn0.GrainfileEvents():
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for grainfile_event (task_ready) on connected node")
	}
	if firstEvent.Type != grainfiles.EventTaskReady || firstEvent.Path != kindTaskPath || firstEvent.Recipient != kindRecipientURL {
		t.Fatalf("received grainfile_event = %+v, want task_ready for %s", firstEvent, kindTaskPath)
	}
	t.Logf("received live grainfile_event seq=%d type=%s path=%s", firstEvent.Seq, firstEvent.Type, firstEvent.Path)

	// 4c. Verify GET /v1/grainfiles/content on pod 1.
	// Unauthenticated -> 401.
	if c, _ := fetchGrainfileContent(t, pod1HTTP, "", url.Values{
		"repo": {kindRepoURL},
		"path": {kindTaskPath},
		"ref":  {kindCommitA},
	}); c != http.StatusUnauthorized {
		t.Fatalf("unauthenticated content fetch = %d, want 401", c)
	}
	// Authorized Grainfile itself -> 200.
	if c, body := fetchGrainfileContent(t, pod1HTTP, nodeToken, url.Values{
		"repo": {kindRepoURL},
		"path": {kindTaskPath},
		"ref":  {kindCommitA},
	}); c != http.StatusOK || body != kindTaskContent {
		t.Fatalf("Grainfile content fetch = %d %q, want 200 %q", c, body, kindTaskContent)
	}
	// Authorized same-repo reference (/docs/arch.md) -> 200.
	if c, body := fetchGrainfileContent(t, pod1HTTP, nodeToken, url.Values{
		"repo":      {kindRepoURL},
		"path":      {kindReferencedDoc},
		"ref":       {kindCommitA},
		"grainfile": {kindTaskPath},
	}); c != http.StatusOK || body != kindArchDocContent {
		t.Fatalf("authorized reference fetch = %d %q, want 200 %q", c, body, kindArchDocContent)
	}
	// Unauthorized unlisted same-repo path (/docs/unlisted.md) -> 403.
	if c, _ := fetchGrainfileContent(t, pod1HTTP, nodeToken, url.Values{
		"repo":      {kindRepoURL},
		"path":      {kindUnlistedDoc},
		"ref":       {kindCommitA},
		"grainfile": {kindTaskPath},
	}); c != http.StatusForbidden {
		t.Fatalf("unlisted same-repo path fetch = %d, want 403", c)
	}
	// Unauthorized cross-repo path -> 403.
	if c, _ := fetchGrainfileContent(t, pod1HTTP, nodeToken, url.Values{
		"url":       {"https://github.com/acme/other/blob/main/secret.md"},
		"ref":       {kindCommitA},
		"grainfile": {"https://github.com/bwsalmon/silo/blob/main/" + kindTaskPath},
	}); c != http.StatusForbidden {
		t.Fatalf("cross-repo path fetch = %d, want 403", c)
	}

	// 5. Scrape :9091/metrics from both pods and verify Prometheus metrics.
	metrics0 := scrapeMetrics(t, pod0Metrics)
	metrics1 := scrapeMetrics(t, pod1Metrics)
	combinedMetrics := metrics0 + "\n" + metrics1
	for _, want := range []string{
		`silo_hub_node_connects_total`,
		`silo_hub_webhooks_total{event="push",result="unauthorized"}`,
		`silo_hub_webhooks_total{event="push",result="accepted"} 1`,
		`silo_hub_grainfiles_indexed{kind="task"} 1`,
		`silo_hub_db_open_connections{store="grainfiles"}`,
	} {
		if !strings.Contains(combinedMetrics, want) {
			t.Fatalf("scraped pod metrics missing %q\npod0:\n%s\npod1:\n%s", want, metrics0, metrics1)
		}
	}

	// 6. Trigger a rolling update (`kubectl rollout restart deployment/silo-hub`),
	// verifying maxUnavailable: 0, goodbye delivery to connected nodes when an
	// old pod terminates, and clean node reconnect + catchup to the replacement pod.
	stopMonitor := make(chan struct{})
	minReadyCh := make(chan int, 1)
	go func() {
		minReady := 2
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopMonitor:
				minReadyCh <- minReady
				return
			case <-ticker.C:
				if r, ok := deploymentReadyReplicas(kubeContext); ok && r < minReady {
					minReady = r
				}
			}
		}
	}()

	runKubectl(t, kubeContext, "-n", "silo-hub", "rollout", "restart", "deployment/silo-hub")

	// Wait for the old pod holding conn0 to send goodbye during the rolling update,
	// and for the rollout to finish.
	rolloutDone := make(chan error, 1)
	go func() {
		cmd := exec.CommandContext(ctx, "kubectl", "--context", kubeContext, "-n", "silo-hub", "rollout", "status", "deployment/silo-hub", "--timeout=180s")
		out, err := cmd.CombinedOutput()
		if err != nil {
			rolloutDone <- fmt.Errorf("%v: %s", err, out)
			return
		}
		rolloutDone <- nil
	}()

	select {
	case <-conn0.Done():
	case <-time.After(120 * time.Second):
		t.Fatal("timed out waiting for old pod to close node connection during rolling update")
	}
	goodbye := conn0.Goodbye()
	if goodbye == "" {
		t.Fatalf("expected non-empty goodbye message from terminating pod, got %q", goodbye)
	}
	t.Logf("node received goodbye from terminating pod: %q", goodbye)

	if err := <-rolloutDone; err != nil {
		t.Fatalf("rollout status deployment/silo-hub: %v", err)
	}
	close(stopMonitor)
	if minReady := <-minReadyCh; minReady < 2 {
		t.Fatalf("rolling update violated maxUnavailable: 0 (minimum readyReplicas observed was %d, want >= 2)", minReady)
	}

	newPods := listReadyHubPods(t, kubeContext)
	if len(newPods) != 2 {
		t.Fatalf("expected 2 ready replacement pods after rollout, got %v", newPods)
	}
	for _, np := range newPods {
		if np == pods[0] || np == pods[1] {
			t.Fatalf("expected replacement pods after rollout, but old pod %s is still present in %v", np, newPods)
		}
	}
	t.Logf("replacement silo-hub pods ready: %v", newPods)

	replacementHTTP := portForward(t, kubeContext, newPods[0], 8080)
	assertStatus(t, "GET", replacementHTTP+"/readyz", "", nil, http.StatusOK)

	// Deliver a second signed webhook while the node is reconnecting so catchup
	// on the replacement pod has a new event after firstEvent.ID.
	pushPayloadB := mustWebhookPayload(kindCommitB, kindRolloutTaskPath, kindReferencedDoc)
	sigB := signWebhook(webhookSecret, pushPayloadB)
	if got := postWebhook(t, replacementHTTP, pushPayloadB, sigB); got != http.StatusOK {
		t.Fatalf("signed webhook B to replacement pod = %d, want 200", got)
	}

	// Reconnect node to the replacement pod and verify catchup.
	node2 := newSimulatedNode(t, replacementHTTP, nodeToken)
	conn2, err := node2.client.Connect(ctx)
	if err != nil {
		t.Fatalf("reconnecting node to replacement pod %s: %v", newPods[0], err)
	}
	defer conn2.Close()

	catchupEvents, newCursor, err := conn2.Catchup(ctx, firstEvent.Seq)
	if err != nil {
		t.Fatalf("Catchup(%d) on replacement pod: %v", firstEvent.Seq, err)
	}
	foundRolloutEvent := false
	for _, ev := range catchupEvents {
		if ev.Type == grainfiles.EventTaskReady && ev.Path == kindRolloutTaskPath && ev.CommitSHA == kindCommitB {
			foundRolloutEvent = true
			break
		}
	}
	if !foundRolloutEvent {
		// Also check initial welcome catch-up channel if delivered before explicit Catchup.
		select {
		case ev := <-conn2.GrainfileEvents():
			if ev.Type == grainfiles.EventTaskReady && ev.Path == kindRolloutTaskPath && ev.CommitSHA == kindCommitB {
				foundRolloutEvent = true
			}
		default:
		}
	}
	if !foundRolloutEvent {
		t.Fatalf("expected catch-up event for %s at %s after reconnect; got events=%+v cursor=%d", kindRolloutTaskPath, kindCommitB, catchupEvents, newCursor)
	}
	t.Logf("reconnect + catchup verified on replacement pod %s (cursor=%d)", newPods[0], newCursor)
}

type simulatedNode struct {
	t      *testing.T
	client *rendezvous.Client

	mu       sync.Mutex
	accepted string
	streams  map[string]*tasks.Stream
}

func newSimulatedNode(t *testing.T, hubURL, token string) *simulatedNode {
	n := &simulatedNode{
		t:       t,
		streams: make(map[string]*tasks.Stream),
	}
	n.client = &rendezvous.Client{
		Hub:   hubURL,
		Token: func(context.Context) (string, error) { return token, nil },
		Node: registry.Node{
			ID:         kindNodeID,
			Repos:      []string{kindRepoSlug},
			Slots:      2,
			Features:   []string{registry.FeatureGrainfiles, registry.FeatureNodeUI},
			Recipients: []string{kindRecipientURL},
			UI:         &registry.NodeUI{Team: "e2e"},
		},
		Subscriptions: &grainfiles.Subscriptions{
			Recipients: []string{kindRecipientURL},
			Repos:      []string{kindRepoURL},
		},
	}
	return n
}

func (n *simulatedNode) serveStreams(conn *rendezvous.Conn) {
	for {
		st, err := conn.Accept()
		if err != nil {
			return
		}
		go n.handleStream(st)
	}
}

func (n *simulatedNode) handleStream(c net.Conn) {
	br := bufio.NewReader(c)
	line, err := br.ReadBytes('\n')
	if err != nil {
		c.Close()
		return
	}
	var peek struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(line, &peek)
	switch peek.Type {
	case nodeui.TypeHTTP:
		var u nodeui.Message
		_ = json.Unmarshal(line, &u)
		nodeui.Serve(c, u, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, kindNodeUIBody)
		}))
	default:
		var m tasks.Message
		if err := json.Unmarshal(line, &m); err != nil {
			c.Close()
			return
		}
		if m.Type == tasks.TypeOffer && m.Task != nil {
			b, _ := json.Marshal(tasks.Message{Type: tasks.TypeAccept, NodeTask: "kind-local-1"})
			_, _ = c.Write(append(b, '\n'))
			n.mu.Lock()
			n.accepted = m.Task.ID
			n.streams[m.Task.ID] = tasks.NewStream(c)
			n.mu.Unlock()
		}
	}
}

func (n *simulatedNode) acceptedTask() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.accepted
}

func (n *simulatedNode) emitAndFinish(ctx context.Context, conn *rendezvous.Conn, taskID string) error {
	es, err := conn.Open(ctx)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(es, `{"v":1,"seq":1,"task":%q,"type":"status","status":{"phase":"running","state":"info","message":"working on kind"}}`+"\n", taskID)
	_ = es.Close()
	if err != nil {
		return err
	}
	n.mu.Lock()
	st := n.streams[taskID]
	n.mu.Unlock()
	if st == nil {
		return errors.New("missing task stream")
	}
	defer st.Close()
	return st.Write(tasks.Message{Type: tasks.TypeResult, State: tasks.Done, Reason: "completed on kind"})
}

func mintOIDCSessionCookie(t *testing.T, email, key string) string {
	t.Helper()
	payload, err := json.Marshal(struct {
		identity.Principal
		Expires int64 `json:"exp"`
	}{
		Principal: identity.Principal{Email: email},
		Expires:   time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	p := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(identity.SessionCookie + "\x00" + p))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return identity.SessionCookie + "=" + p + "." + sig
}

func mustWebhookPayload(commitSHA string, added ...string) []byte {
	b, _ := json.Marshal(map[string]any{
		"ref":   "refs/heads/main",
		"after": commitSHA,
		"repository": map[string]any{
			"full_name": kindRepoSlug,
			"html_url":  kindRepoURL,
		},
		"commits": []map[string]any{
			{
				"id":    commitSHA,
				"added": added,
			},
		},
	})
	return b
}

func signWebhook(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func postWebhook(t *testing.T, baseURL string, payload []byte, sig string) int {
	t.Helper()
	req, err := http.NewRequest("POST", baseURL+grainfiles.GitHubWebhookPath, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "push")
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func fetchGrainfileContent(t *testing.T, baseURL, token string, params url.Values) (int, string) {
	t.Helper()
	req, err := http.NewRequest("GET", baseURL+grainfiles.ContentPath+"?"+params.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func scrapeMetrics(t *testing.T, metricsBaseURL string) string {
	t.Helper()
	code, body := doRequest(t, "GET", metricsBaseURL+"/metrics", "", "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET %s/metrics = %d: %s", metricsBaseURL, code, body)
	}
	return body
}

func assertStatus(t *testing.T, method, reqURL, cookie string, body []byte, want int) {
	t.Helper()
	code, respBody := doRequest(t, method, reqURL, cookie, "", body)
	if code != want {
		t.Fatalf("%s %s = %d (%s), want %d", method, reqURL, code, respBody, want)
	}
}

func doRequest(t *testing.T, method, reqURL, cookie, host string, body []byte) (int, string) {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, reqURL, r)
	if err != nil {
		t.Fatal(err)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if host != "" {
		req.Host = host
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func runKubectl(t *testing.T, kubeContext string, args ...string) string {
	t.Helper()
	full := append([]string{"--context", kubeContext}, args...)
	out, err := exec.Command("kubectl", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(full, " "), err, out)
	}
	return string(out)
}

func listReadyHubPods(t *testing.T, kubeContext string) []string {
	t.Helper()
	out := runKubectl(t, kubeContext, "-n", "silo-hub", "get", "pods", "-l", "app=silo-hub", "-o", "json")
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string `json:"name"`
				DeletionTimestamp string `json:"deletionTimestamp"`
			} `json:"metadata"`
			Status struct {
				Phase      string `json:"phase"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("parsing pods JSON: %v", err)
	}
	var ready []string
	for _, item := range list.Items {
		if item.Metadata.DeletionTimestamp != "" || item.Status.Phase != "Running" {
			continue
		}
		for _, c := range item.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				ready = append(ready, item.Metadata.Name)
				break
			}
		}
	}
	return ready
}

func deploymentReadyReplicas(kubeContext string) (int, bool) {
	out, err := exec.Command("kubectl", "--context", kubeContext, "-n", "silo-hub", "get", "deployment", "silo-hub", "-o", "json").Output()
	if err != nil {
		return 0, false
	}
	var dep struct {
		Status struct {
			ReadyReplicas int `json:"readyReplicas"`
		} `json:"status"`
	}
	if err := json.Unmarshal(out, &dep); err != nil {
		return 0, false
	}
	return dep.Status.ReadyReplicas, true
}

func portForward(t *testing.T, kubeContext, pod string, targetPort int) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	localPort := l.Addr().(*net.TCPAddr).Port
	l.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "kubectl", "--context", kubeContext, "-n", "silo-hub",
		"port-forward", "pod/"+pod, fmt.Sprintf("%d:%d", localPort, targetPort))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("starting port-forward to %s:%d: %v", pod, targetPort, err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort))
	waitFor(t, fmt.Sprintf("port-forward %s:%d on %s", pod, targetPort, addr), 15*time.Second, func() bool {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return false
		}
		c.Close()
		return true
	})
	return "http://" + addr
}
