package modelproxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bwsalmon/silo/pkg/silo/agents"
	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
)

// fakeMeter is a Meter with one agent, "a", whose credential is
// anthropic's API key unless the test changes it.
type fakeMeter struct {
	mu        sync.Mutex
	allowance fenceapi.ModelAllowance
	allowErr  error
	prices    map[string]agents.Price // by provider/model
	priceErr  error
	secretErr error
	charges   []fenceapi.ModelUsage
}

func newMeter() *fakeMeter {
	return &fakeMeter{
		allowance: fenceapi.ModelAllowance{Agent: "a", Left: 1_000_000, State: agents.StateRunning,
			Credential: agents.Credential{Name: "cred", Provider: "anthropic", Kind: agents.CredentialAPIKey}},
		prices: map[string]agents.Price{
			"anthropic/claude-test": {Input: 1_000_000, Output: 10_000_000},
			"openai/gpt-test":       {Input: 1_000_000, Output: 10_000_000},
		},
	}
}

func (m *fakeMeter) ModelAllowanceOf(ctx context.Context, agent string) (fenceapi.ModelAllowance, error) {
	return m.allowance, m.allowErr
}

func (m *fakeMeter) ModelPriceFor(ctx context.Context, provider, model string) (agents.Price, bool, error) {
	p, ok := m.prices[provider+"/"+model]
	return p, ok, m.priceErr
}

func (m *fakeMeter) CredentialSecret(ctx context.Context, name string) (agents.Credential, []byte, error) {
	return m.allowance.Credential, []byte("sk-" + name), m.secretErr
}

func (m *fakeMeter) ChargeModel(ctx context.Context, agent string, u fenceapi.ModelUsage) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.charges = append(m.charges, u)
	return 0, nil
}

type env struct {
	t     *testing.T
	meter *fakeMeter
	proxy *Proxy
	// answer is the provider's handler.
	answer http.HandlerFunc
	got    *http.Request
	body   string
}

func newEnv(t *testing.T) *env {
	e := &env{t: t, meter: newMeter()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		e.got, e.body = r, string(b)
		if e.answer != nil {
			e.answer(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"usage":{"input_tokens":10,"output_tokens":20}}`)
	}))
	t.Cleanup(srv.Close)
	e.proxy = &Proxy{Meter: e.meter, Upstreams: map[string]string{"anthropic": srv.URL + "/", "openai": srv.URL}}
	return e
}

func (e *env) do(method, provider, path, body string, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	e.proxy.ServeModel(w, r, "a", provider)
	return w
}

// errorType is the error type of a refusal in either provider's shape.
func errorType(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var m struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("refusal %q isn't JSON: %v", w.Body, err)
	}
	return m.Error.Type
}

func TestRefusals(t *testing.T) {
	const msg = `{"model":"claude-test","max_tokens":10}`
	for _, tc := range []struct {
		name     string
		setup    func(e *env)
		method   string
		provider string
		path     string
		body     string
		status   int
		typ      string
	}{
		{"unknown provider", nil, "POST", "gemini", "/v1/messages", msg, 404, "not_found_error"},
		{"no allowance", func(e *env) { e.meter.allowErr = errors.New("no such agent") }, "POST", "anthropic", "/v1/messages", msg, 403, "permission_error"},
		{"other provider's credential", nil, "POST", "openai", "/v1/responses", `{"model":"gpt-test"}`, 403, "permission_error"},
		{"subscription", func(e *env) { e.meter.allowance.Credential.Kind = "subscription" }, "POST", "anthropic", "/v1/messages", msg, 403, "permission_error"},
		{"budget exhausted", func(e *env) { e.meter.allowance.State = agents.StateBudgetExhausted }, "POST", "anthropic", "/v1/messages", msg, 429, "rate_limit_error"},
		{"nothing left", func(e *env) { e.meter.allowance.Left = 0 }, "POST", "anthropic", "/v1/messages", msg, 429, "rate_limit_error"},
		{"path not carried", nil, "POST", "anthropic", "/v1/files", msg, 404, "not_found_error"},
		{"wrong method", nil, "GET", "anthropic", "/v1/messages", "", 404, "not_found_error"},
		{"not JSON", nil, "POST", "anthropic", "/v1/messages", "nope", 400, "invalid_request_error"},
		{"unpriced model", nil, "POST", "anthropic", "/v1/messages", `{"model":"claude-free","max_tokens":10}`, 400, "invalid_request_error"},
		{"price lookup fails", func(e *env) { e.meter.priceErr = errors.New("db down") }, "POST", "anthropic", "/v1/messages", msg, 503, "api_error"},
		{"no max_tokens", nil, "POST", "anthropic", "/v1/messages", `{"model":"claude-test"}`, 400, "invalid_request_error"},
		{"zero max_tokens", nil, "POST", "anthropic", "/v1/messages", `{"model":"claude-test","max_tokens":0}`, 400, "invalid_request_error"},
		{"worst case over budget", nil, "POST", "anthropic", "/v1/messages", `{"model":"claude-test","max_tokens":1000000}`, 429, "rate_limit_error"},
		{"secret lookup fails", func(e *env) { e.meter.secretErr = errors.New("kms down") }, "POST", "anthropic", "/v1/messages", msg, 503, "api_error"},
		{"too large", nil, "POST", "anthropic", "/v1/messages/count_tokens", strings.Repeat("x", maxBody+1), 413, "invalid_request_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			if tc.setup != nil {
				tc.setup(e)
			}
			w := e.do(tc.method, tc.provider, tc.path, tc.body)
			if w.Code != tc.status {
				t.Fatalf("%d %s, want %d", w.Code, w.Body, tc.status)
			}
			if got := errorType(t, w); got != tc.typ {
				t.Errorf("error type %q, want %q: %s", got, tc.typ, w.Body)
			}
			if e.got != nil {
				t.Errorf("a refused call reached the provider: %s %s", e.got.Method, e.got.URL)
			}
			if len(e.meter.charges) != 0 {
				t.Errorf("a refused call was charged: %v", e.meter.charges)
			}
			if (w.Code == 429) != (w.Header().Get("Retry-After") != "") {
				t.Errorf("status %d with Retry-After %q", w.Code, w.Header().Get("Retry-After"))
			}
		})
	}
}

func TestOpenAIRefusalShape(t *testing.T) {
	e := newEnv(t)
	e.meter.allowance.Credential.Provider = "openai"
	e.meter.allowance.Left = 0
	w := e.do("POST", "openai", "/v1/chat/completions", `{"model":"gpt-test"}`)
	if w.Code != 429 || !strings.Contains(w.Body.String(), `"code":"insufficient_quota"`) || !strings.Contains(w.Body.String(), `"param":null`) {
		t.Errorf("%d %s", w.Code, w.Body)
	}
}

// TestForwards proves the proxy puts the credential's key on the call,
// strips the harness's own credentials and cookies, keeps the query
// and other headers, and charges the usage the provider reports.
func TestForwards(t *testing.T) {
	e := newEnv(t)
	e.answer = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req-1")
		w.WriteHeader(201)
		io.WriteString(w, `{"usage":{"input_tokens":10,"output_tokens":20,"cache_creation_input_tokens":3,"cache_read_input_tokens":4}}`)
	}
	w := e.do("POST", "anthropic", "/v1/messages?beta=true", `{"model":"claude-test","max_tokens":10}`,
		"X-Api-Key", "sk-harness", "Authorization", "Bearer harness", "Cookie", "c=1", "Anthropic-Version", "2023-06-01")
	if w.Code != 201 || w.Header().Get("X-Request-Id") != "req-1" {
		t.Fatalf("%d %v %s", w.Code, w.Header(), w.Body)
	}
	if e.got.URL.Path != "/v1/messages" || e.got.URL.RawQuery != "beta=true" {
		t.Errorf("forwarded to %s", e.got.URL)
	}
	h := e.got.Header
	if h.Get("X-Api-Key") != "sk-cred" || h.Get("Authorization") != "" || h.Get("Cookie") != "" || h.Get("Anthropic-Version") != "2023-06-01" {
		t.Errorf("forwarded headers %v", h)
	}
	want := fenceapi.ModelUsage{Model: "claude-test", Input: 10, Output: 20, CacheWrite: 3, CacheRead: 4, Proxied: true}
	if len(e.meter.charges) != 1 || e.meter.charges[0] != want {
		t.Errorf("charges %v, want %v", e.meter.charges, want)
	}
	if len(e.proxy.held) != 0 {
		t.Errorf("held after the call: %v", e.proxy.held)
	}
}

func TestOpenAIBearerAndDefaultMax(t *testing.T) {
	for _, tc := range []struct{ path, field string }{
		{"/v1/responses", "max_output_tokens"},
		{"/v1/chat/completions", "max_completion_tokens"},
	} {
		e := newEnv(t)
		e.meter.allowance.Credential.Provider = "openai"
		e.answer = func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":60}}}`)
		}
		w := e.do("POST", "openai", tc.path, `{"model":"gpt-test"}`)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body)
		}
		if e.got.Header.Get("Authorization") != "Bearer sk-cred" || e.got.Header.Get("X-Api-Key") != "" {
			t.Errorf("%s: forwarded headers %v", tc.path, e.got.Header)
		}
		var sent map[string]any
		json.Unmarshal([]byte(e.body), &sent)
		if sent[tc.field] != float64(DefaultMaxOutput) {
			t.Errorf("%s: forwarded %s, want %s set to %d", tc.path, e.body, tc.field, DefaultMaxOutput)
		}
		want := fenceapi.ModelUsage{Model: "gpt-test", Input: 40, CacheRead: 60, Output: 7, Proxied: true}
		if len(e.meter.charges) != 1 || e.meter.charges[0] != want {
			t.Errorf("%s: charges %v, want %v", tc.path, e.meter.charges, want)
		}
	}
}

// TestFreeCalls proves counting tokens and listing models are carried
// and not charged, and need no price or max_tokens.
func TestFreeCalls(t *testing.T) {
	e := newEnv(t)
	if w := e.do("POST", "anthropic", "/v1/messages/count_tokens", `{"model":"unpriced"}`); w.Code != 200 {
		t.Errorf("count_tokens: %d %s", w.Code, w.Body)
	}
	for _, p := range []string{"/v1/models", "/v1/models/claude-test"} {
		if w := e.do("GET", "anthropic", p, ""); w.Code != 200 || e.got.URL.Path != p {
			t.Errorf("GET %s: %d %s", p, w.Code, w.Body)
		}
	}
	if len(e.meter.charges) != 0 {
		t.Errorf("free calls charged: %v", e.meter.charges)
	}
}

// TestProviderErrorNotCharged proves a call the provider refuses is
// relayed as it is and not charged.
func TestProviderErrorNotCharged(t *testing.T) {
	e := newEnv(t)
	e.answer = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(529)
		io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error"}}`)
	}
	w := e.do("POST", "anthropic", "/v1/messages", `{"model":"claude-test","max_tokens":10}`)
	if w.Code != 529 || !strings.Contains(w.Body.String(), "overloaded_error") {
		t.Errorf("%d %s", w.Code, w.Body)
	}
	if len(e.meter.charges) != 0 {
		t.Errorf("charged %v", e.meter.charges)
	}
}

func TestProviderUnreachable(t *testing.T) {
	e := newEnv(t)
	e.proxy.Upstreams = map[string]string{"anthropic": "http://127.0.0.1:1"}
	w := e.do("POST", "anthropic", "/v1/messages", `{"model":"claude-test","max_tokens":10}`)
	if w.Code != 502 || errorType(t, w) != "api_error" {
		t.Errorf("%d %s", w.Code, w.Body)
	}
	if len(e.proxy.held) != 0 || len(e.meter.charges) != 0 {
		t.Errorf("held %v, charges %v", e.proxy.held, e.meter.charges)
	}
}

// TestStream proves a stream is relayed line by line and charged the
// largest count of each kind it reports.
func TestStream(t *testing.T) {
	e := newEnv(t)
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":50,"output_tokens":1,"cache_read_input_tokens":5}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"text":"hi"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":30}}` + "\n\n"
	e.answer = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, stream)
	}
	w := e.do("POST", "anthropic", "/v1/messages", `{"model":"claude-test","max_tokens":100,"stream":true}`)
	if w.Code != 200 || w.Body.String() != stream {
		t.Fatalf("%d %q", w.Code, w.Body)
	}
	if !w.Flushed {
		t.Error("the stream wasn't flushed")
	}
	want := fenceapi.ModelUsage{Model: "claude-test", Input: 50, Output: 30, CacheRead: 5, Proxied: true}
	if len(e.meter.charges) != 1 || e.meter.charges[0] != want {
		t.Errorf("charges %v, want %v", e.meter.charges, want)
	}
}

// TestNoUsageChargesWorstCase proves a response that never says what it
// used is charged its estimated prompt and its whole max_tokens.
func TestNoUsageChargesWorstCase(t *testing.T) {
	e := newEnv(t)
	body := `{"model":"claude-test","max_tokens":100}`
	e.answer = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: ping\ndata: {}\n\n")
	}
	if w := e.do("POST", "anthropic", "/v1/messages", body); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	want := fenceapi.ModelUsage{Model: "claude-test", Input: EstimateTokens([]byte(body)), Output: 100, Proxied: true}
	if len(e.meter.charges) != 1 || e.meter.charges[0] != want {
		t.Errorf("charges %v, want %v", e.meter.charges, want)
	}
}

// brokenWriter is a harness that has hung up.
type brokenWriter struct {
	h    http.Header
	code int
}

func (b *brokenWriter) Header() http.Header         { return b.h }
func (b *brokenWriter) WriteHeader(c int)           { b.code = c }
func (b *brokenWriter) Write(p []byte) (int, error) { return 0, errors.New("broken pipe") }

// TestHungUpStillCharged proves the proxy keeps reading a stream after
// the harness stops taking it, so the usage at its end is charged.
func TestHungUpStillCharged(t *testing.T) {
	e := newEnv(t)
	e.answer = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"message":{"usage":{"input_tokens":5}}}`+"\n\n"+`data: {"usage":{"output_tokens":9}}`+"\n\n")
	}
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-test","max_tokens":10}`))
	e.proxy.ServeModel(&brokenWriter{h: http.Header{}}, r, "a", "anthropic")
	want := fenceapi.ModelUsage{Model: "claude-test", Input: 5, Output: 9, Proxied: true}
	if len(e.meter.charges) != 1 || e.meter.charges[0] != want {
		t.Errorf("charges %v, want %v", e.meter.charges, want)
	}
}

// TestHold proves concurrent calls can't together pass what's left,
// and a release gives it back.
func TestHold(t *testing.T) {
	var p Proxy
	if !p.hold("a", 60, 100) {
		t.Fatal("first hold refused")
	}
	if p.hold("a", 50, 100) {
		t.Error("a second hold past what's left was allowed")
	}
	if !p.hold("b", 50, 100) {
		t.Error("another agent's hold was refused")
	}
	if !p.hold("a", 40, 100) {
		t.Error("a hold of exactly what's left was refused")
	}
	p.release("a", 60)
	p.release("a", 40)
	p.release("b", 50)
	if len(p.held) != 0 {
		t.Errorf("held after every release: %v", p.held)
	}
}

func TestMaxTokensOf(t *testing.T) {
	obj := func(s string) map[string]json.RawMessage { return mustObject([]byte(s)) }
	for _, tc := range []struct {
		provider, body string
		n              int64
		set            bool
	}{
		{"anthropic", `{"max_tokens":5}`, 5, true},
		{"anthropic", `{"max_output_tokens":5}`, 0, false},
		{"anthropic", `{"max_tokens":"5"}`, 0, false},
		{"anthropic", `{"max_tokens":-1}`, 0, false},
		{"openai", `{"max_output_tokens":7}`, 7, true},
		{"openai", `{"max_completion_tokens":8}`, 8, true},
		{"openai", `{"max_tokens":9}`, 9, true},
		{"openai", `{"max_tokens":0,"max_completion_tokens":3}`, 3, true},
		{"openai", `{}`, 0, false},
	} {
		n, set := maxTokensOf(tc.provider, "", obj(tc.body))
		if n != tc.n || set != tc.set {
			t.Errorf("maxTokensOf(%s, %s) = %d, %v; want %d, %v", tc.provider, tc.body, n, set, tc.n, tc.set)
		}
	}
}

func TestUpstreamDefaults(t *testing.T) {
	var p Proxy
	if base, ok := p.upstream("anthropic"); !ok || base != "https://api.anthropic.com" {
		t.Errorf("upstream(anthropic) = %s, %v", base, ok)
	}
	if _, ok := p.upstream("gemini"); ok {
		t.Error("upstream(gemini) found")
	}
}

func TestEstimateTokens(t *testing.T) {
	for n, want := range map[int]int64{0: 0, 1: 1, 3: 1, 4: 2, 300: 100} {
		if got := EstimateTokens(make([]byte, n)); got != want {
			t.Errorf("EstimateTokens(%d bytes) = %d, want %d", n, got, want)
		}
	}
}
