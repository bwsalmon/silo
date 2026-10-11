package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/ids"
	"github.com/bwsalmon/silo/pkg/silo/tools"
)

// ProtocolVersion is the MCP revision /mcp answers initialize with when
// the harness asks for one it doesn't know.
const ProtocolVersion = "2025-06-18"

var knownVersions = map[string]bool{"2025-06-18": true, "2025-03-26": true, "2024-11-05": true}

// listTimeout bounds asking the tool server for its tools; past it the
// release's own list is given.
const listTimeout = 5 * time.Second

// maxEventContent caps a tool result's content as the session's event
// records it.
const maxEventContent = 16 << 10

type handler struct {
	s      *Sockets
	agent  string
	silo   string
	caller fenceapi.Caller
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// mcp serves MCP's streamable HTTP transport, answering each POST with
// one JSON response: initialize, ping, tools/list and tools/call. A
// notification gets 202. There is no server-sent stream, so GET is 405.
func (h *handler) mcp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error: " + err.Error()}})
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if req.Method == "tools/call" {
		var p struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if _, own := ownTools[p.Name]; !own {
			// The tool VM's: down the tool channel, as it came.
			h.proxyCall(w, r.Context(), req, p.Name, body)
			return
		}
	}
	writeRPC(w, h.handle(r.Context(), req))
}

func writeRPC(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (h *handler) handle(ctx context.Context, req rpcRequest) rpcResponse {
	out := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v := ProtocolVersion
		if knownVersions[p.ProtocolVersion] {
			v = p.ProtocolVersion
		}
		out.Result = map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "silo", "version": tools.Version},
			"instructions": "You are an agent in a Silo silo, and act as it: every call here is made as your silo, with its authority. " +
				"The tool VM's tools run on your tool VM; the silo_ and agent_ tools are the fence API, compute sends one request to the compute/v1 facade, " +
				"and ask and request_approval wait for a person's answer.",
		}
	case "ping":
		out.Result = map[string]any{}
	case "tools/list":
		list := h.toolVMTools(ctx)
		for _, t := range ownToolList() {
			list = append(list, t)
		}
		out.Result = map[string]any{"tools": list}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			out.Error = &rpcError{Code: -32602, Message: "invalid params: " + err.Error()}
			break
		}
		if p.Arguments == nil {
			p.Arguments = map[string]any{}
		}
		id := h.recordCall(ctx, p.Name, p.Arguments)
		text, isErr := ownTools[p.Name](h, ctx, p.Arguments)
		h.recordResult(ctx, id, p.Name, isErr, text)
		out.Result = map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr}
	default:
		out.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return out
}

// toolVMTools are the tool server's tools: as it lists them, or, if it
// can't be asked now, as this release's silo-tools does.
func (h *handler) toolVMTools(ctx context.Context) []any {
	if h.s.Tools != nil {
		lctx, cancel := context.WithTimeout(ctx, listTimeout)
		defer cancel()
		resp := h.s.Tools.MCP(lctx, h.agent, []byte(`{"jsonrpc":"2.0","id":"silo-list","method":"tools/list"}`))
		var r struct {
			Result *struct {
				Tools []any `json:"tools"`
			} `json:"result"`
		}
		if json.Unmarshal(resp, &r) == nil && r.Result != nil {
			return r.Result.Tools
		}
	}
	srv, err := tools.New(tools.Options{})
	if err != nil {
		return nil
	}
	var out []any
	for _, t := range srv.Tools() {
		out = append(out, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema})
	}
	return out
}

// proxyCall sends a tools/call for one of the tool VM's tools down the
// tool channel, and records it and its result in the session.
func (h *handler) proxyCall(w http.ResponseWriter, ctx context.Context, req rpcRequest, name string, raw []byte) {
	var p struct {
		Arguments map[string]any `json:"arguments"`
	}
	_ = json.Unmarshal(req.Params, &p)
	id := h.recordCall(ctx, name, p.Arguments)
	var resp []byte
	if h.s.Tools == nil {
		resp, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{
			"code": -32001, "message": "silo: the agent's tool server isn't connected; retry", "data": map[string]any{"retryable": true}}})
	} else {
		resp = h.s.Tools.MCP(ctx, h.agent, raw)
	}
	var r struct {
		Result *struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *rpcError `json:"error"`
	}
	_ = json.Unmarshal(resp, &r)
	switch {
	case r.Result != nil:
		var text strings.Builder
		for _, c := range r.Result.Content {
			text.WriteString(c.Text)
		}
		h.recordResult(ctx, id, name, r.Result.IsError, text.String())
	case r.Error != nil:
		h.recordResult(ctx, id, name, true, r.Error.Message)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(resp)
}

func (h *handler) recordCall(ctx context.Context, tool string, args map[string]any) string {
	id := "c-" + ids.New() + ids.New()
	if h.s.Control != nil {
		_ = h.s.Control.RecordEvent(ctx, h.agent, fenceapi.EventToolCall, map[string]any{"id": id, "tool": tool, "arguments": args})
	}
	return id
}

func (h *handler) recordResult(ctx context.Context, id, tool string, isErr bool, text string) {
	if h.s.Control == nil {
		return
	}
	if len(text) > maxEventContent {
		text = text[:maxEventContent] + fmt.Sprintf("\n[%d more bytes]", len(text)-maxEventContent)
	}
	_ = h.s.Control.RecordEvent(context.WithoutCancel(ctx), h.agent, fenceapi.EventToolResult,
		map[string]any{"id": id, "tool": tool, "is_error": isErr, "content": text})
}

// A tool of the socket's own: it returns its result's text, and whether
// it is an error.
type ownTool func(h *handler, ctx context.Context, args map[string]any) (string, bool)

var ownTools map[string]ownTool

func init() {
	ownTools = map[string]ownTool{
		"silo_describe": func(h *handler, ctx context.Context, a map[string]any) (string, bool) {
			return h.fence(ctx, http.MethodGet, siloRef(str(a, "silo")), nil, nil)
		},
		"silo_create_child": func(h *handler, ctx context.Context, a map[string]any) (string, bool) {
			return h.fence(ctx, http.MethodPost, siloRef(str(a, "parent"))+"/children", nil, a["body"])
		},
		"silo_delete": func(h *handler, ctx context.Context, a map[string]any) (string, bool) {
			if str(a, "silo") == "" {
				return "silo_delete needs silo: the silo to delete, by path or -id", true
			}
			var q url.Values
			if f, _ := a["force"].(bool); f {
				q = url.Values{"force": {"true"}}
			}
			return h.fence(ctx, http.MethodDelete, siloRef(str(a, "silo")), q, nil)
		},
		"silo_patch": func(h *handler, ctx context.Context, a map[string]any) (string, bool) {
			return h.fence(ctx, http.MethodPatch, siloRef(str(a, "silo")), nil, a["body"])
		},
		"silo_export": func(h *handler, ctx context.Context, a map[string]any) (string, bool) {
			return h.fence(ctx, http.MethodPost, siloRef(str(a, "silo"))+"/exports", nil, a["body"])
		},
		"silo_import": func(h *handler, ctx context.Context, a map[string]any) (string, bool) {
			return h.fence(ctx, http.MethodPost, siloRef(str(a, "silo"))+"/imports", nil, a["body"])
		},
		"agent_create": func(h *handler, ctx context.Context, a map[string]any) (string, bool) {
			return h.fence(ctx, http.MethodPost, siloRef(str(a, "silo"))+"/agents", nil, a["body"])
		},
		"agent_delete": func(h *handler, ctx context.Context, a map[string]any) (string, bool) {
			if str(a, "agent") == "" {
				return "agent_delete needs agent: its name, or -id", true
			}
			return h.fence(ctx, http.MethodDelete, siloRef(str(a, "silo"))+"/agents/"+url.PathEscape(str(a, "agent")), nil, nil)
		},
		"agent_message": func(h *handler, ctx context.Context, a map[string]any) (string, bool) {
			if str(a, "agent") == "" {
				return "agent_message needs agent: its name, or -id", true
			}
			return h.fence(ctx, http.MethodPost, siloRef(str(a, "silo"))+"/agents/"+url.PathEscape(str(a, "agent"))+"/messages", nil,
				map[string]any{"text": str(a, "text")})
		},
		"compute":          (*handler).compute,
		"ask":              (*handler).ask,
		"request_approval": (*handler).requestApproval,
	}
}

func ownToolList() []map[string]any {
	s := map[string]any{"type": "string"}
	obj := map[string]any{"type": "object"}
	silo := map[string]any{"type": "string", "description": "A silo by path (/team/ci) or -id; your own silo if omitted"}
	in := func(props map[string]any, required ...string) map[string]any {
		if required == nil {
			required = []string{}
		}
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	t := func(name, desc string, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "description": desc, "inputSchema": schema}
	}
	return []map[string]any{
		t("silo_describe", "Describe a silo: its quota, usage, policies and children (GET /silo/v1/{silo}).", in(map[string]any{"silo": silo})),
		t("silo_create_child", "Create a child silo (POST /silo/v1/{parent}/children); body is the child: name, address, quota, policies.",
			in(map[string]any{"parent": silo, "body": obj}, "body")),
		t("silo_delete", "Delete a silo (DELETE /silo/v1/{silo}); force deletes what it holds too.",
			in(map[string]any{"silo": silo, "force": map[string]any{"type": "boolean", "default": false}}, "silo")),
		t("silo_patch", "Change a silo (PATCH /silo/v1/{silo}); body is the fields to change.", in(map[string]any{"silo": silo, "body": obj}, "body")),
		t("silo_export", "Offer an export from a silo (POST /silo/v1/{silo}/exports); body is the export.", in(map[string]any{"silo": silo, "body": obj}, "body")),
		t("silo_import", "Accept an export into a silo (POST /silo/v1/{silo}/imports); body is the import.", in(map[string]any{"silo": silo, "body": obj}, "body")),
		t("agent_create", "Create an agent in a silo (POST /silo/v1/{silo}/agents); body is the agent: name, harness, model, budget, tool_vm, lease, prompt. "+
			"An agent your session creates is deleted with you.", in(map[string]any{"silo": silo, "body": obj}, "body")),
		t("agent_delete", "Delete an agent (DELETE /silo/v1/{silo}/agents/{agent}).", in(map[string]any{"silo": silo, "agent": s}, "agent")),
		t("agent_message", "Send an agent a message (POST /silo/v1/{silo}/agents/{agent}/messages): an idle agent starts a turn with it.",
			in(map[string]any{"silo": silo, "agent": s, "text": s}, "agent", "text")),
		t("compute", "Send one request to the compute/v1 facade, as gcloud would: method (GET by default), path (projects/... under /compute/v1/, with any query), and a JSON body.",
			in(map[string]any{"method": s, "path": s, "body": obj}, "path")),
		t("ask", "Ask the people watching you a question, and wait for their answer. You are WAITING until it comes.",
			in(map[string]any{"question": s}, "question")),
		t("request_approval", "Propose an action and wait for a person to approve or refuse it. You are WAITING until they answer.",
			in(map[string]any{"action": map[string]any{"type": "string", "description": "What you propose to do, exactly"}, "reason": s}, "action")),
	}
}

func str(a map[string]any, k string) string {
	s, _ := a[k].(string)
	return s
}

// siloRef is a silo argument as a fence API path: self, silos/-id or
// silos/<path>.
func siloRef(s string) string {
	s = strings.TrimSpace(s)
	switch {
	case s == "" || s == "self":
		return "self"
	case strings.HasPrefix(s, "-"):
		return "silos/" + s
	}
	return "silos/" + strings.Trim(s, "/")
}

// fence makes one fence API call as the session's caller, and returns
// its body.
func (h *handler) fence(ctx context.Context, method, rest string, q url.Values, body any) (string, bool) {
	u := "/silo/v1/" + rest
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	if h.s.API == nil {
		return "silo: the fence API isn't served here", true
	}
	return h.send(ctx, h.s.API, method, u, body)
}

// compute sends one facade request as the session's caller.
func (h *handler) compute(ctx context.Context, a map[string]any) (string, bool) {
	method := strings.ToUpper(str(a, "method"))
	if method == "" {
		method = http.MethodGet
	}
	p := strings.TrimPrefix(strings.TrimPrefix(str(a, "path"), "/"), "compute/v1/")
	if p == "" || strings.Contains(p, "..") {
		return "compute needs path: the request's path under /compute/v1/, such as projects/silo-abc123/zones/us-central1-a/instances", true
	}
	if h.s.Facade == nil {
		return "silo: the compute facade isn't served here", true
	}
	return h.send(ctx, h.s.Facade, method, "/compute/v1/"+p, a["body"])
}

func (h *handler) send(ctx context.Context, to http.Handler, method, u string, body any) (string, bool) {
	var r io.Reader = http.NoBody
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return "the body is not JSON: " + err.Error(), true
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(fenceapi.ContextWithCaller(ctx, h.caller), method, "http://silo.internal"+u, r)
	if err != nil {
		return err.Error(), true
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := &recorder{header: http.Header{}, status: http.StatusOK}
	to.ServeHTTP(rec, req)
	text := strings.TrimSpace(rec.body.String())
	if rec.status >= 400 {
		return fmt.Sprintf("HTTP %d: %s", rec.status, text), true
	}
	return text, false
}

// recorder is the response of a call the socket's tools make in
// process.
type recorder struct {
	header http.Header
	status int
	wrote  bool
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.body.Write(b)
}

// ask records a question and waits for its answer.
func (h *handler) ask(ctx context.Context, a map[string]any) (string, bool) {
	r, err := h.request(ctx, fenceapi.RequestQuestion, str(a, "question"))
	if err != nil {
		return err.Error(), true
	}
	return r.Answer.Text, false
}

// requestApproval records a proposed action and waits for its answer.
func (h *handler) requestApproval(ctx context.Context, a map[string]any) (string, bool) {
	text := str(a, "action")
	if reason := str(a, "reason"); reason != "" && text != "" {
		text += "\n\nReason: " + reason
	}
	r, err := h.request(ctx, fenceapi.RequestApproval, text)
	if err != nil {
		return err.Error(), true
	}
	verdict := "Not approved."
	if r.Answer.Approve != nil && *r.Answer.Approve {
		verdict = "Approved."
	}
	if r.Answer.Text != "" {
		verdict += " " + r.Answer.Text
	}
	return verdict, false
}

func (h *handler) request(ctx context.Context, kind, text string) (*fenceapi.AgentControlRequest, error) {
	if h.s.Control == nil {
		return nil, fmt.Errorf("silo: the control surface isn't served here")
	}
	r, err := h.s.Control.OpenRequest(ctx, h.caller, kind, text)
	if err != nil {
		return nil, err
	}
	got, err := h.s.Control.WaitAnswer(ctx, h.agent, r.ID)
	if err != nil {
		return nil, fmt.Errorf("silo: no answer yet to request %s, which stays open: %v", r.ID, err)
	}
	return got, nil
}
