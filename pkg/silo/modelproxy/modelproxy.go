// Package modelproxy is silod's model proxy (docs/silo/agents.md,
// "Metering", SV-110; call 47), served on each session socket at
// /model/<provider>/ and nowhere else.
//
// An API key never reaches a harness: the harness is pointed at the
// proxy with no key, and the proxy adds the credential's key on the way
// out. Before forwarding a call it works out the call's worst case (the
// prompt's tokens at the input price, plus max_tokens at the output
// price) and refuses it, with the provider's own 429 naming the budget,
// if that is more than the agent has left. After the call it charges
// the actual usage from the provider's response, cache reads and writes
// at their own prices, through the fence API's ChargeModel.
package modelproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/agents"
	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/ledger"
)

// The providers' APIs, by provider.
var DefaultUpstreams = map[string]string{
	"anthropic": "https://api.anthropic.com",
	"openai":    "https://api.openai.com",
}

// DefaultMaxOutput is the max_output_tokens the proxy sets on an OpenAI
// call that names none, so that its worst case is bounded.
const DefaultMaxOutput = 32000

// maxBody is the largest request the proxy reads.
const maxBody = 32 << 20

// Meter is what the proxy needs of the fence API (fenceapi's Server).
type Meter interface {
	ModelAllowanceOf(ctx context.Context, agent string) (fenceapi.ModelAllowance, error)
	ModelPriceFor(ctx context.Context, provider, model string) (agents.Price, bool, error)
	CredentialSecret(ctx context.Context, name string) (agents.Credential, []byte, error)
	ChargeModel(ctx context.Context, agent string, u fenceapi.ModelUsage) (int64, error)
}

// Proxy is the model proxy.
type Proxy struct {
	Meter Meter
	// Upstreams are the providers' base URLs; DefaultUpstreams if nil.
	Upstreams map[string]string
	// Client makes the calls; a client with no overall timeout if nil,
	// as a streamed call may run for minutes.
	Client *http.Client

	mu sync.Mutex
	// held is each agent's calls in flight, by their worst case, so
	// that concurrent calls can't together pass what one couldn't.
	held map[string]int64
}

// The calls the proxy carries: metered ones, priced on their usage, and
// free ones (counting tokens, listing models).
var metered = map[string][]string{
	"anthropic": {"/v1/messages"},
	"openai":    {"/v1/responses", "/v1/chat/completions"},
}

var free = map[string][]string{
	"anthropic": {"/v1/messages/count_tokens"},
}

func (p *Proxy) upstream(provider string) (string, bool) {
	u := p.Upstreams
	if u == nil {
		u = DefaultUpstreams
	}
	base, ok := u[provider]
	return strings.TrimSuffix(base, "/"), ok
}

// EstimateTokens is the proxy's count of a request's prompt tokens,
// without the provider's tokenizer: a token for every three bytes of
// the request body, which is more than the text's tokens for any
// English or code prompt, as the JSON around it counts too.
func EstimateTokens(body []byte) int64 { return int64(len(body)+2) / 3 }

// ServeModel carries one call of agent's to provider; r's path is what
// follows /model/<provider>.
func (p *Proxy) ServeModel(w http.ResponseWriter, r *http.Request, agent, provider string) {
	ctx := r.Context()
	base, ok := p.upstream(provider)
	if !ok {
		refuse(w, provider, http.StatusNotFound, "not_found_error", fmt.Sprintf("silo: the model proxy has no provider %q", provider))
		return
	}
	al, err := p.Meter.ModelAllowanceOf(ctx, agent)
	if err != nil {
		refuse(w, provider, http.StatusForbidden, "permission_error", "silo: "+err.Error())
		return
	}
	switch {
	case al.Credential.Provider != provider:
		refuse(w, provider, http.StatusForbidden, "permission_error",
			fmt.Sprintf("silo: this agent's credential %s is %s's, not %s's", al.Credential.Name, al.Credential.Provider, provider))
		return
	case al.Credential.Kind != agents.CredentialAPIKey:
		refuse(w, provider, http.StatusForbidden, "permission_error",
			fmt.Sprintf("silo: credential %s is a subscription, which the harness uses directly, not through the model proxy", al.Credential.Name))
		return
	case al.State == agents.StateBudgetExhausted || al.Left <= 0:
		refuse(w, provider, http.StatusTooManyRequests, rateType(provider), "silo: this agent's model budget for this period is spent")
		return
	}
	path := r.URL.Path
	isMetered := r.Method == http.MethodPost && hasPath(metered[provider], path)
	isFree := (r.Method == http.MethodPost && hasPath(free[provider], path)) ||
		(r.Method == http.MethodGet && (path == "/v1/models" || strings.HasPrefix(path, "/v1/models/")))
	if !isMetered && !isFree {
		refuse(w, provider, http.StatusNotFound, "not_found_error", fmt.Sprintf("silo: the model proxy doesn't carry %s %s", r.Method, path))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		refuse(w, provider, http.StatusRequestEntityTooLarge, "invalid_request_error", "silo: the request is too large for the model proxy")
		return
	}
	var price agents.Price
	var worst int64
	var model string
	if isMetered {
		var req map[string]json.RawMessage
		if err := json.Unmarshal(body, &req); err != nil {
			refuse(w, provider, http.StatusBadRequest, "invalid_request_error", "silo: the request body isn't a JSON object")
			return
		}
		_ = json.Unmarshal(req["model"], &model)
		var priced bool
		if price, priced, err = p.Meter.ModelPriceFor(ctx, provider, model); err != nil {
			refuse(w, provider, http.StatusServiceUnavailable, "api_error", "silo: "+err.Error())
			return
		} else if !priced {
			refuse(w, provider, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("silo: model %q has no %s price, so can't be used", model, provider))
			return
		}
		maxTokens, set := maxTokensOf(provider, path, req)
		if !set {
			if provider == "anthropic" {
				refuse(w, provider, http.StatusBadRequest, "invalid_request_error", "silo: the model proxy needs max_tokens to price a call's worst case")
				return
			}
			maxTokens = DefaultMaxOutput
			field := "max_output_tokens"
			if path == "/v1/chat/completions" {
				field = "max_completion_tokens"
			}
			req[field], _ = json.Marshal(maxTokens)
			body, _ = json.Marshal(req)
		}
		worst = fenceapi.WorstCase(price, EstimateTokens(body), maxTokens)
		if !p.hold(agent, worst, al.Left) {
			refuse(w, provider, http.StatusTooManyRequests, rateType(provider), fmt.Sprintf(
				"silo: this call could cost up to $%s (its prompt at the input price plus max_tokens %d at the output price), more than the $%s left of this agent's model budget for this period",
				ledger.FormatAmount(worst), maxTokens, ledger.FormatAmount(max(al.Left, 0))))
			return
		}
		defer p.release(agent, worst)
	}
	_, key, err := p.Meter.CredentialSecret(ctx, al.Credential.Name)
	if err != nil {
		refuse(w, provider, http.StatusServiceUnavailable, "api_error", "silo: "+err.Error())
		return
	}
	out, err := http.NewRequestWithContext(ctx, r.Method, base+path, bytes.NewReader(body))
	if err != nil {
		refuse(w, provider, http.StatusBadRequest, "invalid_request_error", "silo: "+err.Error())
		return
	}
	out.URL.RawQuery = r.URL.RawQuery
	for k, vs := range r.Header {
		switch strings.ToLower(k) {
		case "authorization", "x-api-key", "host", "content-length", "accept-encoding", "connection", "cookie":
			continue
		}
		out.Header[k] = vs
	}
	switch provider {
	case "anthropic":
		out.Header.Set("X-Api-Key", string(key))
	default:
		out.Header.Set("Authorization", "Bearer "+string(key))
	}
	client := p.Client
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(out)
	if err != nil {
		refuse(w, provider, http.StatusBadGateway, "api_error", "silo: the provider didn't answer: "+err.Error())
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		switch strings.ToLower(k) {
		case "content-length", "content-encoding", "connection", "transfer-encoding":
			continue
		}
		w.Header()[k] = vs
	}
	w.WriteHeader(resp.StatusCode)
	u := &usage{provider: provider}
	stream := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	_ = relay(w, resp.Body, u, stream)
	if !isMetered || resp.StatusCode/100 != 2 {
		return
	}
	mu := u.modelUsage(model)
	if !u.seen {
		// A response that broke off before saying what it used is
		// charged its worst case.
		maxTokens, _ := maxTokensOf(provider, path, mustObject(body))
		mu = fenceapi.ModelUsage{Model: model, Input: EstimateTokens(body), Output: maxTokens}
	}
	mu.Proxied = true
	// The charge stands even if the harness has hung up.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if _, err := p.Meter.ChargeModel(cctx, agent, mu); err != nil {
		log.Printf("model proxy: charging agent %s for a call to %s: %v", agent, model, err)
	}
}

func mustObject(b []byte) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(b, &m)
	return m
}

func hasPath(l []string, p string) bool {
	for _, x := range l {
		if x == p {
			return true
		}
	}
	return false
}

// maxTokensOf is the call's output cap and whether it sets one.
func maxTokensOf(provider, path string, req map[string]json.RawMessage) (int64, bool) {
	fields := []string{"max_tokens"}
	if provider == "openai" {
		fields = []string{"max_output_tokens", "max_completion_tokens", "max_tokens"}
	}
	for _, f := range fields {
		var n int64
		if raw, ok := req[f]; ok && json.Unmarshal(raw, &n) == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
}

// hold reserves worst of left for agent's call in flight.
func (p *Proxy) hold(agent string, worst, left int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.held == nil {
		p.held = map[string]int64{}
	}
	if worst > left-p.held[agent] {
		return false
	}
	p.held[agent] += worst
	return true
}

func (p *Proxy) release(agent string, worst int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.held[agent] -= worst; p.held[agent] <= 0 {
		delete(p.held, agent)
	}
}

func rateType(provider string) string {
	if provider == "openai" {
		return "insufficient_quota"
	}
	return "rate_limit_error"
}

// refuse answers in the provider's own error shape, so the harness
// reports it as it would the provider's.
func refuse(w http.ResponseWriter, provider string, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	if status == http.StatusTooManyRequests {
		// Not soon: the budget comes back when someone raises it or a
		// new period starts.
		w.Header().Set("Retry-After", "3600")
	}
	w.WriteHeader(status)
	var body any
	switch provider {
	case "openai":
		body = map[string]any{"error": map[string]any{"message": msg, "type": typ, "param": nil, "code": typ}}
	default:
		body = map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": msg}}
	}
	_ = json.NewEncoder(w).Encode(body)
}

// relay copies the provider's answer to the harness as it comes,
// flushing each line of a stream, and reads its usage on the way.
func relay(w http.ResponseWriter, body io.Reader, u *usage, stream bool) error {
	if !stream {
		b, err := io.ReadAll(body)
		if _, werr := w.Write(b); werr != nil && err == nil {
			err = werr
		}
		u.absorbJSON(b)
		return err
	}
	f, _ := w.(http.Flusher)
	br := bufio.NewReaderSize(body, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				// Keep reading what the provider sends, so its usage is
				// still charged.
				w = discard{}
			}
			if bytes.HasPrefix(line, []byte("data:")) {
				u.absorbJSON(bytes.TrimSpace(line[len("data:"):]))
			}
			if f != nil && len(bytes.TrimSpace(line)) == 0 {
				f.Flush()
			}
		}
		if err != nil {
			if f != nil {
				f.Flush()
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

type discard struct{}

func (discard) Header() http.Header         { return http.Header{} }
func (discard) Write(b []byte) (int, error) { return len(b), nil }
func (discard) WriteHeader(int)             {}

// usage is a call's usage as its response reports it, the largest of
// each count seen (a stream reports input at its start and output, as a
// running total, at its end).
type usage struct {
	provider                                 string
	seen                                     bool
	input, output, cacheWrite, cacheRead     int64
	openaiCached                             int64
	openaiPrompt, openaiCompletion, openaiIn int64
}

func (u *usage) absorbJSON(b []byte) {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return
	}
	for _, path := range [][]string{{"usage"}, {"message", "usage"}, {"response", "usage"}} {
		if v, ok := dig(m, path...).(map[string]any); ok {
			u.absorb(v)
		}
	}
}

func dig(m map[string]any, path ...string) any {
	var cur any = m
	for _, k := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func num(v any) int64 {
	f, _ := v.(float64)
	return int64(f)
}

func up(dst *int64, v int64) {
	if v > *dst {
		*dst = v
	}
}

func (u *usage) absorb(v map[string]any) {
	u.seen = true
	if u.provider == "openai" {
		up(&u.openaiIn, num(v["input_tokens"]))
		up(&u.openaiPrompt, num(v["prompt_tokens"]))
		up(&u.output, num(v["output_tokens"]))
		up(&u.openaiCompletion, num(v["completion_tokens"]))
		up(&u.openaiCached, num(dig(v, "input_tokens_details", "cached_tokens")))
		up(&u.openaiCached, num(dig(v, "prompt_tokens_details", "cached_tokens")))
		return
	}
	up(&u.input, num(v["input_tokens"]))
	up(&u.output, num(v["output_tokens"]))
	up(&u.cacheWrite, num(v["cache_creation_input_tokens"]))
	up(&u.cacheRead, num(v["cache_read_input_tokens"]))
}

// modelUsage is the usage by kind: OpenAI counts cached tokens within
// its input, which are taken out to be priced as cache reads.
func (u *usage) modelUsage(model string) fenceapi.ModelUsage {
	if u.provider == "openai" {
		in := max(u.openaiIn, u.openaiPrompt)
		return fenceapi.ModelUsage{Model: model, Input: max(in-u.openaiCached, 0), CacheRead: u.openaiCached,
			Output: max(u.output, u.openaiCompletion)}
	}
	return fenceapi.ModelUsage{Model: model, Input: u.input, Output: u.output, CacheWrite: u.cacheWrite, CacheRead: u.cacheRead}
}
