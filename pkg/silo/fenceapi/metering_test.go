package fenceapi_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/agents"
	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/modelproxy"
	"github.com/bwsalmon/silo/pkg/silo/store"
)

// fakeProvider is a model provider: it answers each call with the next
// of answers, and records what it was sent.
type fakeProvider struct {
	mu      sync.Mutex
	answers []string // bodies; one starting "event:" is a stream
	calls   []*http.Request
	bodies  []string
}

func (f *fakeProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls = append(f.calls, r)
	f.bodies = append(f.bodies, string(b))
	ans := `{"usage":{}}`
	if len(f.answers) > 0 {
		ans, f.answers = f.answers[0], f.answers[1:]
	}
	f.mu.Unlock()
	if strings.HasPrefix(ans, "event:") {
		w.Header().Set("Content-Type", "text/event-stream")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	io.WriteString(w, ans)
}

func (f *fakeProvider) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type proxyEnv struct {
	*agentEnv
	provider *fakeProvider
	proxy    *modelproxy.Proxy
}

// newProxyEnv has the anthropic-api and openai-api keys stored, and
// priced test models with round prices: claude-test at $1 in, $10 out,
// $2 cache write and $0.10 cache read a million tokens; gpt-test at $1
// in, $10 out, $0.50 cache read.
func newProxyEnv(t *testing.T) *proxyEnv {
	e := &proxyEnv{agentEnv: newAgentEnv(t), provider: &fakeProvider{}}
	srv := httptest.NewServer(e.provider)
	t.Cleanup(srv.Close)
	e.proxy = &modelproxy.Proxy{Meter: e.api, Upstreams: map[string]string{"anthropic": srv.URL, "openai": srv.URL}}
	e.ok(e.root, "POST", "/silo/v1/credentials", obj{"name": "anthropic-api", "provider": "anthropic", "kind": "api_key", "secret": "sk-ant-test"})
	e.ok(e.root, "POST", "/silo/v1/credentials", obj{"name": "openai-api", "provider": "openai", "kind": "api_key", "secret": "sk-oai-test"})
	e.ok(e.root, "PUT", "/silo/v1/modelPrices/anthropic/claude-test", obj{"input": "1", "output": "10", "cache_write": "2", "cache_read": "0.10"})
	e.ok(e.root, "PUT", "/silo/v1/modelPrices/openai/gpt-test", obj{"input": "1", "output": "10", "cache_write": "1", "cache_read": "0.50"})
	return e
}

// agent makes a working agent in /team with budget usd: its tool server
// connected and its first turn over, IDLE.
func (e *proxyEnv) agent(name, usd string, extra obj) string {
	e.t.Helper()
	b := obj{"budget": obj{"usd": usd}, "model": "claude-test"}
	for k, v := range extra {
		b[k] = v
	}
	id := e.create(e.root, "team", name, b)
	if err := e.api.ToolServerConnected(ctx, id); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.api.EndTurn(ctx, id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// call makes a model call of agent's through the proxy.
func (e *proxyEnv) call(agent, provider, path string, body obj) *httptest.ResponseRecorder {
	e.t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", path, strings.NewReader(string(b)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Api-Key", "silo-model-proxy")
	w := httptest.NewRecorder()
	e.proxy.ServeModel(w, r, agent, provider)
	return w
}

func (e *proxyEnv) agentState(id string) (state, spent string) {
	e.t.Helper()
	a := e.ok(e.root, "GET", "/silo/v1/agents/-"+id, nil)
	return a["state"].(string), get(a, "budget", "spent_usd").(string)
}

func TestModelProxyChargesActualUsage(t *testing.T) {
	e := newProxyEnv(t)
	id := e.agent("a", "1", nil)
	e.provider.answers = []string{`{"id":"msg_1","usage":{"input_tokens":1000,"output_tokens":2000,"cache_creation_input_tokens":10000,"cache_read_input_tokens":100000}}`}
	w := e.call(id, "anthropic", "/v1/messages", obj{"model": "claude-test", "max_tokens": 4000, "messages": []obj{{"role": "user", "content": "hi"}}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "msg_1") {
		t.Fatalf("call: %d %s", w.Code, w.Body)
	}
	// The key is added on the way out, in place of the harness's.
	got := e.provider.calls[0]
	if got.Header.Get("X-Api-Key") != "sk-ant-test" || got.Header.Get("Authorization") != "" || got.URL.Path != "/v1/messages" {
		t.Errorf("upstream request: %s %v", got.URL.Path, got.Header)
	}
	// $1 x 1000 + $10 x 2000 + $2 x 10000 (cache writes) + $0.10 x
	// 100000 (cache reads), a million: 0.001 + 0.02 + 0.02 + 0.01.
	if _, spent := e.agentState(id); spent != "0.051" {
		t.Errorf("spent_usd %s, want 0.051", spent)
	}
	recs := e.records(e.team, "model.call")
	if len(recs) != 1 || get(recs[0], "fields", "usd") != "0.051" || get(recs[0], "fields", "metered") != "proxied" ||
		get(recs[0], "fields", "credential") != "anthropic-api" || get(recs[0], "fields", "tokens", "cache_read") != float64(100000) {
		t.Errorf("model.call audit: %v", recs)
	}

	// A stream's usage: input at its start, output at its end.
	e.provider.answers = []string{"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":500,\"output_tokens\":1}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":100}}\n\n"}
	w = e.call(id, "anthropic", "/v1/messages", obj{"model": "claude-test", "max_tokens": 4000, "stream": true})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "message_delta") {
		t.Fatalf("stream: %d %s", w.Code, w.Body)
	}
	// 0.051 + $1 x 500 + $10 x 100, a million.
	if _, spent := e.agentState(id); spent != "0.0525" {
		t.Errorf("spent_usd %s after the stream, want 0.0525", spent)
	}

	// Free calls go through uncharged; others don't go at all.
	e.call(id, "anthropic", "/v1/messages/count_tokens", obj{"model": "claude-test"})
	if w := e.call(id, "anthropic", "/v1/messages/batches", obj{}); w.Code != 404 {
		t.Errorf("batches: %d", w.Code)
	}
	// A model with no price can't be used.
	if w := e.call(id, "anthropic", "/v1/messages", obj{"model": "claude-unpriced", "max_tokens": 10}); w.Code != 400 {
		t.Errorf("unpriced: %d %s", w.Code, w.Body)
	}
	// Another provider's path isn't this credential's.
	if w := e.call(id, "openai", "/v1/responses", obj{"model": "gpt-test"}); w.Code != 403 {
		t.Errorf("openai with an anthropic credential: %d", w.Code)
	}
	if len(e.records(e.team, "model.call")) != 2 {
		t.Errorf("model.call records: %v", e.records(e.team, "model.call"))
	}
}

func TestModelProxyRefusesTheWorstCase(t *testing.T) {
	e := newProxyEnv(t)
	id := e.agent("a", "1", nil)
	// 200000 output tokens at $10 a million is $2, more than the $1 left.
	w := e.call(id, "anthropic", "/v1/messages", obj{"model": "claude-test", "max_tokens": 200000, "messages": []obj{{"role": "user", "content": "hi"}}})
	if w.Code != 429 {
		t.Fatalf("over budget: %d %s", w.Code, w.Body)
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Type != "error" || body.Error.Type != "rate_limit_error" ||
		!strings.Contains(body.Error.Message, "budget") {
		t.Errorf("refusal isn't Anthropic's 429 naming the budget: %s", w.Body)
	}
	if e.provider.n() != 0 {
		t.Error("a refused call reached the provider")
	}
	// A prompt alone can be the worst case: 3 bytes a token, $1 a
	// million: 3.6 MB is over $1.
	big := strings.Repeat("x", 3_600_000)
	if w := e.call(id, "anthropic", "/v1/messages", obj{"model": "claude-test", "max_tokens": 1, "messages": []obj{{"role": "user", "content": big}}}); w.Code != 429 {
		t.Errorf("a huge prompt: %d", w.Code)
	}
	// Under it, it goes.
	if w := e.call(id, "anthropic", "/v1/messages", obj{"model": "claude-test", "max_tokens": 50000}); w.Code != 200 {
		t.Errorf("within budget: %d %s", w.Code, w.Body)
	}
	if _, spent := e.agentState(id); spent != "0.00" {
		t.Errorf("refusals charged: %s", spent)
	}
}

func TestModelProxyOpenAICachePricing(t *testing.T) {
	e := newProxyEnv(t)
	id := e.agent("c", "1", obj{"harness": obj{"adapter": "codex", "credential": "openai-api"}, "model": "gpt-test"})
	e.provider.answers = []string{"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10000,\"input_tokens_details\":{\"cached_tokens\":8000},\"output_tokens\":1000}}}\n\n"}
	w := e.call(id, "openai", "/v1/responses", obj{"model": "gpt-test", "stream": true})
	if w.Code != 200 {
		t.Fatalf("call: %d %s", w.Code, w.Body)
	}
	got := e.provider.calls[0]
	if got.Header.Get("Authorization") != "Bearer sk-oai-test" || got.Header.Get("X-Api-Key") != "" {
		t.Errorf("upstream auth: %v", got.Header)
	}
	// With no max_output_tokens, the proxy sets one, so the worst case
	// is bounded.
	if !strings.Contains(e.provider.bodies[0], fmt.Sprintf(`"max_output_tokens":%d`, modelproxy.DefaultMaxOutput)) {
		t.Errorf("forwarded body: %s", e.provider.bodies[0])
	}
	// 2000 uncached at $1, 8000 cached at $0.50, 1000 out at $10, a
	// million: 0.002 + 0.004 + 0.01.
	if _, spent := e.agentState(id); spent != "0.016" {
		t.Errorf("spent_usd %s, want 0.016", spent)
	}
	// OpenAI's own 429.
	w = e.call(id, "openai", "/v1/responses", obj{"model": "gpt-test", "max_output_tokens": 1000000})
	if w.Code != 429 || !strings.Contains(w.Body.String(), `"code":"insufficient_quota"`) {
		t.Errorf("refusal: %d %s", w.Code, w.Body)
	}
}

func TestBudgetExhaustedAndRaised(t *testing.T) {
	e := newProxyEnv(t)
	id := e.agent("a", "0.06", nil)
	usage := `{"usage":{"input_tokens":1000,"output_tokens":2000,"cache_creation_input_tokens":10000,"cache_read_input_tokens":100000}}`
	e.provider.answers = []string{usage, usage}
	for i := 0; i < 2; i++ {
		if w := e.call(id, "anthropic", "/v1/messages", obj{"model": "claude-test", "max_tokens": 10}); w.Code != 200 {
			t.Fatalf("call %d: %d %s", i, w.Code, w.Body)
		}
	}
	state, spent := e.agentState(id)
	if state != agents.StateBudgetExhausted || spent != "0.102" {
		t.Fatalf("after spending past the budget: %s, spent %s", state, spent)
	}
	if !strings.Contains(strings.Join(e.runner.Log(), " "), "stop "+id) {
		t.Errorf("the harness wasn't stopped: %v", e.runner.Log())
	}
	recs := e.records(e.team, "agent.budget_exhausted")
	if len(recs) != 1 || get(recs[0], "fields", "overshoot_usd") != "0.042" {
		t.Errorf("agent.budget_exhausted audit: %v", recs)
	}
	e.wantUsed("exhausted", e.team, "0.06", "0")
	// The proxy refuses its next call outright.
	if w := e.call(id, "anthropic", "/v1/messages", obj{"model": "claude-test", "max_tokens": 1}); w.Code != 429 {
		t.Errorf("an exhausted agent's call: %d", w.Code)
	}
	if e.provider.n() != 2 {
		t.Errorf("the provider was called %d times", e.provider.n())
	}
	// :resume is no way out; raising the budget is.
	e.fail(fenceapi.FailedPrecondition, e.root, "POST", "/silo/v1/silos/team/agents/a:resume", nil)
	e.ok(e.root, "PATCH", "/silo/v1/silos/team/agents/a", obj{"budget": obj{"usd": "1"}})
	if state, _ := e.agentState(id); state != agents.StateIdle {
		t.Errorf("after raising the budget: %s", state)
	}
	if l := e.runner.Log(); !strings.Contains(l[len(l)-1], "resume "+id) && !strings.Contains(l[len(l)-1], id) {
		t.Errorf("the session wasn't resumed: %v", l)
	}
	e.wantUsed("raised", e.team, "1", "1")

	// Lowering the team's model budget below what its agents hold fails.
	e.fail(fenceapi.FailedPrecondition, e.root, "PATCH", "/silo/v1/silos/team", obj{"model_budget_usd": "0.50"})
}

func TestSubscriptionMeteringAndOvershoot(t *testing.T) {
	e := newProxyEnv(t)
	e.ok(e.root, "DELETE", "/silo/v1/credentials/anthropic-api", nil)
	e.ok(e.root, "POST", "/silo/v1/credentials", obj{"name": "anthropic-api", "provider": "anthropic", "kind": "subscription",
		"files": obj{".claude/.credentials.json": base64.StdEncoding.EncodeToString([]byte("{}"))}})
	id := e.agent("s", "0.03", nil)
	// The proxy doesn't carry a subscription's calls.
	if w := e.call(id, "anthropic", "/v1/messages", obj{"model": "claude-test", "max_tokens": 1}); w.Code != 403 {
		t.Errorf("a subscription through the proxy: %d", w.Code)
	}
	// Each turn's reported usage is charged at the API list price.
	turn := map[string]any{"source": "harness", "tokens": map[string]any{"input_tokens": float64(1000), "output_tokens": float64(1000),
		"cache_creation_input_tokens": float64(0), "cache_read_input_tokens": float64(100000)}}
	if err := e.api.ReportUsage(ctx, id, turn); err != nil {
		t.Fatal(err)
	}
	// $1 x 1000 + $10 x 1000 + $0.10 x 100000, a million: 0.021.
	if state, spent := e.agentState(id); state != agents.StateIdle || spent != "0.021" {
		t.Errorf("after a turn: %s, spent %s", state, spent)
	}
	// The turn that crosses is charged whole: the overshoot is recorded,
	// and the session stopped.
	if err := e.api.ReportUsage(ctx, id, turn); err != nil {
		t.Fatal(err)
	}
	if state, spent := e.agentState(id); state != agents.StateBudgetExhausted || spent != "0.042" {
		t.Errorf("after crossing: %s, spent %s", state, spent)
	}
	if recs := e.records(e.team, "agent.budget_exhausted"); len(recs) != 1 || get(recs[0], "fields", "overshoot_usd") != "0.012" {
		t.Errorf("overshoot: %v", recs)
	}
	if recs := e.records(e.team, "model.call"); len(recs) != 2 || get(recs[0], "fields", "metered") != "reported" {
		t.Errorf("model.call audit: %v", recs)
	}
	// An API-key session's report is only recorded: the proxy charged it.
	e.ok(e.root, "POST", "/silo/v1/credentials", obj{"name": "openai-api", "provider": "openai", "kind": "api_key", "secret": "k"})
	c := e.agent("c", "1", obj{"harness": obj{"adapter": "codex", "credential": "openai-api"}, "model": "gpt-test"})
	if err := e.api.ReportUsage(ctx, c, turn); err != nil {
		t.Fatal(err)
	}
	if _, spent := e.agentState(c); spent != "0.00" {
		t.Errorf("an API-key session charged on its report: %s", spent)
	}
}

func TestBudgetPeriodReset(t *testing.T) {
	e := newProxyEnv(t)
	id := e.agent("a", "0.01", nil)
	turn := map[string]any{"tokens": map[string]any{"output_tokens": float64(2000)}}
	if _, err := e.api.ChargeModel(ctx, id, fenceapi.HarnessUsage(turn)); err != nil {
		t.Fatal(err)
	}
	if state, _ := e.agentState(id); state != agents.StateBudgetExhausted {
		t.Fatalf("not exhausted: %s", state)
	}
	if n, err := e.api.ResetBudgetPeriods(ctx); n != 0 || err != nil {
		t.Fatalf("reset within the period: %d %v", n, err)
	}
	// The period started a month ago, by the database's clock.
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agents SET period_start = ? WHERE id = ?`, store.Millis(time.Now().AddDate(0, -1, -1)), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := e.api.ResetBudgetPeriods(ctx); n != 1 || err != nil {
		t.Fatalf("reset: %d %v", n, err)
	}
	a := e.ok(e.root, "GET", "/silo/v1/agents/-"+id, nil)
	if a["state"] != agents.StateIdle || get(a, "budget", "spent_usd") != "0.00" {
		t.Errorf("after the new period: %v %v", a["state"], a["budget"])
	}
	if ps, _ := time.Parse(time.RFC3339, get(a, "budget", "period_start").(string)); ps.Day() != 1 && ps.Weekday() != time.Monday {
		t.Errorf("period_start %v", ps)
	}
	if l := e.runner.Log(); !strings.Contains(l[len(l)-1], id) {
		t.Errorf("not resumed: %v", l)
	}
	if n, err := e.api.ResetBudgetPeriods(ctx); n != 0 || err != nil {
		t.Errorf("second reset: %d %v", n, err)
	}
}

func TestLeaseWarningAndExpiryCascade(t *testing.T) {
	e := newProxyEnv(t)
	id := e.agent("a", "1", nil)
	setExpiry := func(d time.Duration) {
		if err := e.db.Tx(ctx, func(tx store.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE agents SET expire_at = ? WHERE id = ?`, store.Millis(time.Now().Add(d)), id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := e.api.WarnLeases(ctx); n != 0 || err != nil {
		t.Fatalf("warned early: %d %v", n, err)
	}
	setExpiry(5 * time.Minute)
	if n, err := e.api.WarnLeases(ctx); n != 1 || err != nil {
		t.Fatalf("warn: %d %v", n, err)
	}
	if n, _ := e.api.WarnLeases(ctx); n != 0 {
		t.Error("warned twice for one expire_time")
	}
	msgs := e.ok(e.root, "GET", "/silo/v1/silos/team/agents/a/messages", nil)
	if !strings.Contains(str(msgs), "lease ends") {
		t.Errorf("messages: %v", msgs)
	}
	// An IDLE agent starts a turn on it.
	if state, _ := e.agentState(id); state != agents.StateRunning {
		t.Errorf("after the warning: %s", state)
	}
	// A renewal (here, a new expire_time) is warned of again.
	setExpiry(6 * time.Minute)
	if n, _ := e.api.WarnLeases(ctx); n != 1 {
		t.Error("not warned of the new expire_time")
	}

	// What the agent's session made goes with it, bottom-up.
	child := e.mk(e.team+"/agent:"+id, "self", "scratch", nil)
	e.mk(e.root, "team/scratch", "deeper", nil)
	if err := e.api.RecordEvent(ctx, id, fenceapi.EventError, map[string]any{"message": "kept"}); err != nil {
		t.Fatal(err)
	}
	setExpiry(-time.Second)
	if n, err := e.api.ExpireAgents(ctx); n != 1 || err != nil {
		t.Fatalf("expire: %d %v", n, err)
	}
	e.gone("team", "a")
	if code, _ := e.do(e.root, "GET", "/silo/v1/silos/-"+child, nil); code != 404 {
		t.Errorf("the agent's child silo survived: %d", code)
	}
	if code, _ := e.do(e.root, "GET", "/silo/v1/silos/team/scratch/deeper", nil); code != 404 {
		t.Errorf("its grandchild survived: %d", code)
	}
	if d := e.vms.deletes(); len(d) != 1 || d[0] != "/team/agent-a" {
		t.Errorf("tool VM deletes: %v", d)
	}
	if recs := e.records(e.team, "agent.expire"); len(recs) != 1 {
		t.Errorf("agent.expire audit: %v", recs)
	}
	e.wantUsed("expired", e.team, "0", "0")
	// The transcript is kept, for the audit retention.
	var events int
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM agent_events WHERE agent = ?`, id).Scan(&events)
	}); err != nil || events == 0 {
		t.Errorf("transcript: %d events, %v", events, err)
	}
}
