package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/bwsalmon/silo/v3/acp"
	"github.com/bwsalmon/silo/v3/gateway"
	"github.com/bwsalmon/silo/v3/instructions"
)

// Handler returns an http.Handler serving the bridge's HTTP/WebSocket API,
// `/connect` WebSocket for `silo connect`, `/acp/v1/sessions/:id` WebSocket,
// and `/p/:slug/...` page link reverse proxy.
func (b *Bridge) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/connect", b.handleWSConnect)
	mux.HandleFunc("/p/", b.servePageProxy)
	mux.HandleFunc("/acp/v1/sessions/", b.handleACPSessionWS)
	mux.HandleFunc("/api/v1/agents", b.handleAgents)
	mux.HandleFunc("/api/v1/agents/", b.handleAgentByID)
	mux.HandleFunc("/api/v1/pages", b.handlePages)
	mux.HandleFunc("/api/v1/pages/", b.handlePageBySlug)
	mux.HandleFunc("/api/v1/secrets", b.handleSecrets)
	mux.HandleFunc("/api/v1/secrets/", b.handleSecretByID)
	mux.HandleFunc("/api/v1/sessions", b.handleSessions)
	mux.HandleFunc("/api/v1/sessions/", b.handleSessionByID)
	mux.HandleFunc("/api/v1/permissions", b.handlePermissions)
	mux.HandleFunc("/api/v1/permissions/", b.handlePermissionByID)
	mux.HandleFunc("/api/v1/requests", b.handleRequests)
	mux.HandleFunc("/api/v1/requests/", b.handleRequestByID)
	mux.HandleFunc("/api/v1/audit", b.handleAudit)
	mux.HandleFunc("/api/v1/instructions", b.handleInstructions)
	return mux
}

func extractAuth(r *http.Request) gateway.Auth {
	tok := strings.TrimSpace(r.Header.Get("X-Silo-Agent-Token"))
	if tok == "" {
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			tok = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		}
	}
	if tok != "" {
		return gateway.Auth{AgentToken: tok}
	}
	return gateway.Auth{}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var ge *gateway.Error
	status := http.StatusInternalServerError
	msg := err.Error()
	if errors.As(err, &ge) {
		msg = ge.Message
		switch ge.Code {
		case gateway.CodeInvalidParams, gateway.CodeInvalidRequest, gateway.CodeParseError:
			status = http.StatusBadRequest
		case gateway.CodeForbidden:
			status = http.StatusForbidden
		case gateway.CodeNotFound:
			status = http.StatusNotFound
		case gateway.CodeConflict:
			status = http.StatusConflict
		}
	}
	writeJSON(w, status, map[string]any{"error": msg})
}

func decodeBody(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	return json.Unmarshal(data, v)
}

// WS /connect: `silo connect --url ws://.../connect`
func (b *Bridge) handleWSConnect(w http.ResponseWriter, r *http.Request) {
	ws, err := UpgradeWS(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer ws.Close()
	rawHello, err := ws.ReadFrame()
	if err != nil {
		return
	}
	var hello acp.Frame
	if err := json.Unmarshal(rawHello, &hello); err != nil {
		return
	}
	if hello.Token == "" {
		hello.Token = r.URL.Query().Get("token")
	}
	_ = b.AttachConnect(r.Context(), ws, hello)
}

// WS /acp/v1/sessions/:id: replays transcript history (unless ?history=false)
// and streams live ACP updates, while accepting `session/prompt` and
// `session/cancel` JSON-RPC frames from the client.
func (b *Bridge) handleACPSessionWS(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/acp/v1/sessions/")
	id = strings.TrimSuffix(id, "/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	ws, err := UpgradeWS(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer ws.Close()

	includeHistory := r.URL.Query().Get("history") != "false"
	history, ch, cancel, err := b.Subscribe(id, includeHistory)
	if err != nil {
		return
	}
	defer cancel()

	for _, msg := range history {
		if err := ws.WriteFrame(msg); err != nil {
			return
		}
	}

	ctx, stop := context.WithCancel(r.Context())
	defer stop()

	// Forward live session updates to the WebSocket client.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					_ = ws.Close()
					return
				}
				if err := ws.WriteFrame(msg); err != nil {
					return
				}
			}
		}
	}()

	// Accept client ACP requests (`session/prompt`, `session/cancel`).
	for {
		frameBytes, err := ws.ReadFrame()
		if err != nil {
			return
		}
		var f acp.Frame
		if err := json.Unmarshal(frameBytes, &f); err != nil {
			continue
		}
		switch f.Method {
		case acp.MethodSessionPrompt:
			go func(req acp.Frame) {
				text := acp.PromptText(req.Params)
				res, perr := b.Prompt(ctx, id, text)
				resp := acp.Frame{JSONRPC: "2.0", ID: req.ID}
				if perr != nil {
					resp.Error = &acp.RPCError{Code: -32000, Message: perr.Error()}
				} else {
					rawRes, _ := json.Marshal(res)
					resp.Result = rawRes
				}
				rawResp, _ := json.Marshal(resp)
				_ = ws.WriteFrame(rawResp)
			}(f)
		case acp.MethodSessionCancel:
			_ = b.Cancel(ctx, id)
		}
	}
}

func (b *Bridge) handleAgents(w http.ResponseWriter, r *http.Request) {
	auth := extractAuth(r)
	switch r.Method {
	case http.MethodGet:
		var list []gateway.AgentInfo
		if err := b.gwCall(r.Context(), "agent.list", withAuth(auth, nil), &list); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	case http.MethodPost:
		var m map[string]any
		if err := decodeBody(r, &m); err != nil {
			writeErr(w, &gateway.Error{Code: gateway.CodeInvalidParams, Message: err.Error()})
			return
		}
		res, err := b.RegisterAgent(r.Context(), auth, m)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (b *Bridge) handleAgentByID(w http.ResponseWriter, r *http.Request) {
	auth := extractAuth(r)
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/agents/")
	id, action, _ := strings.Cut(rest, "/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	switch action {
	case "":
		switch r.Method {
		case http.MethodGet:
			var info gateway.AgentInfo
			if err := b.gwCall(r.Context(), "agent.status", withAuth(auth, map[string]any{"agent": id}), &info); err != nil {
				writeErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, info)
		case http.MethodPatch:
			var body struct {
				Permissions string `json:"permissions"`
			}
			if err := decodeBody(r, &body); err != nil {
				writeErr(w, &gateway.Error{Code: gateway.CodeInvalidParams, Message: err.Error()})
				return
			}
			info, err := b.SetAgentPermissions(r.Context(), id, body.Permissions)
			if err != nil {
				writeErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, info)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	case "stop":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Mode string `json:"mode"`
		}
		_ = decodeBody(r, &body)
		res, err := b.StopAgent(r.Context(), auth, id, body.Mode)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	case "permissions":
		if r.Method != http.MethodPost && r.Method != http.MethodPut {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Permissions string `json:"permissions"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeErr(w, &gateway.Error{Code: gateway.CodeInvalidParams, Message: err.Error()})
			return
		}
		info, err := b.SetAgentPermissions(r.Context(), id, body.Permissions)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, info)
	case "approve":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Approve *bool  `json:"approve"`
			Comment string `json:"comment"`
		}
		_ = decodeBody(r, &body)
		approve := body.Approve == nil || *body.Approve
		info, err := b.ApproveAgent(r.Context(), id, approve, body.Comment)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, info)
	case "read":
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		out, err := b.ReadAgent(r.Context(), auth, id, since)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	default:
		http.NotFound(w, r)
	}
}

func (b *Bridge) handlePages(w http.ResponseWriter, r *http.Request) {
	auth := extractAuth(r)
	switch r.Method {
	case http.MethodGet:
		all := r.URL.Query().Get("all") == "true" || r.URL.Query().Get("all") == "1"
		list, err := b.ListPages(r.Context(), auth, all)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	case http.MethodPost:
		var p gateway.PageRegisterParams
		if err := decodeBody(r, &p); err != nil {
			writeErr(w, &gateway.Error{Code: gateway.CodeInvalidParams, Message: err.Error()})
			return
		}
		pg, err := b.RegisterPage(r.Context(), auth, p)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, pg)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (b *Bridge) handlePageBySlug(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	auth := extractAuth(r)
	slug := strings.TrimPrefix(r.URL.Path, "/api/v1/pages/")
	slug = strings.TrimSuffix(slug, "/")
	pg, err := b.UnregisterPage(r.Context(), auth, slug)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pg)
}

func (b *Bridge) handleSecrets(w http.ResponseWriter, r *http.Request) {
	auth := extractAuth(r)
	switch r.Method {
	case http.MethodGet:
		pending := r.URL.Query().Get("pending") == "true" || r.URL.Query().Get("pending") == "1"
		var list []gateway.SecretRequest
		if err := b.gwCall(r.Context(), "secret.list", withAuth(auth, map[string]any{"pending": pending}), &list); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	case http.MethodPost:
		var body struct {
			Name   string `json:"name"`
			Reason string `json:"reason"`
			Path   string `json:"path"`
			Wait   *bool  `json:"wait"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeErr(w, &gateway.Error{Code: gateway.CodeInvalidParams, Message: err.Error()})
			return
		}
		wait := body.Wait == nil || *body.Wait
		res, err := b.RequestSecret(r.Context(), auth, body.Name, body.Reason, body.Path, wait)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (b *Bridge) handleSecretByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/secrets/")
	id, action, _ := strings.Cut(rest, "/")
	switch action {
	case "provide":
		var body struct {
			Value string `json:"value"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeErr(w, &gateway.Error{Code: gateway.CodeInvalidParams, Message: err.Error()})
			return
		}
		res, err := b.ProvideSecret(r.Context(), id, body.Value)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	case "reject":
		var body struct {
			Note string `json:"note"`
		}
		_ = decodeBody(r, &body)
		res, err := b.RejectSecret(r.Context(), id, body.Note)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	default:
		http.NotFound(w, r)
	}
}

func (b *Bridge) handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	list, err := b.ListSessions(r.URL.Query().Get("agent"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (b *Bridge) handleSessionByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/sessions/")
	id, action, _ := strings.Cut(rest, "/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	switch action {
	case "transcript":
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		evs, err := b.ReadTranscript(id, after)
		if err != nil {
			writeErr(w, err)
			return
		}
		if strings.Contains(r.Header.Get("Accept"), "application/x-ndjson") || r.URL.Query().Get("format") == "jsonl" {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			enc := json.NewEncoder(w)
			for _, ev := range evs {
				_ = enc.Encode(ev)
			}
			return
		}
		writeJSON(w, http.StatusOK, evs)
	case "prompt":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var raw json.RawMessage
		if err := decodeBody(r, &raw); err != nil {
			writeErr(w, &gateway.Error{Code: gateway.CodeInvalidParams, Message: err.Error()})
			return
		}
		text := acp.PromptText(raw)
		res, err := b.Prompt(r.Context(), id, text)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	case "cancel":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := b.Cancel(r.Context(), id); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"cancelled": id})
	default:
		http.NotFound(w, r)
	}
}

func (b *Bridge) handlePermissions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	agent := r.URL.Query().Get("agent")
	pending := r.URL.Query().Get("pending") == "true" || r.URL.Query().Get("pending") == "1"
	writeJSON(w, http.StatusOK, b.ListPermissions(agent, pending))
}

func (b *Bridge) handlePermissionByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/permissions/")
	id, action, _ := strings.Cut(rest, "/")
	if action != "decision" && action != "decide" && action != "" {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Decision string `json:"decision"`
		Approved *bool  `json:"approved"`
		OptionID string `json:"option_id"`
		Comment  string `json:"comment"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, &gateway.Error{Code: gateway.CodeInvalidParams, Message: err.Error()})
		return
	}
	var approve bool
	if body.Approved != nil {
		approve = *body.Approved
	} else {
		switch body.Decision {
		case "approve", "approved", "allow":
			approve = true
		case "reject", "rejected", "deny", "denied":
			approve = false
		default:
			writeErr(w, &gateway.Error{Code: gateway.CodeInvalidParams, Message: "specify decision (approve|reject) or approved (bool)"})
			return
		}
	}
	pr, err := b.DecidePermission(r.Context(), id, approve, body.OptionID, body.Comment)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pr)
}

func (b *Bridge) handleRequests(w http.ResponseWriter, r *http.Request) {
	auth := extractAuth(r)
	switch r.Method {
	case http.MethodGet:
		pending := r.URL.Query().Get("pending") == "true" || r.URL.Query().Get("pending") == "1"
		var list []gateway.AgentRequest
		if err := b.gwCall(r.Context(), "request.list", withAuth(auth, map[string]any{"pending": pending}), &list); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	case http.MethodPost:
		var m map[string]any
		if err := decodeBody(r, &m); err != nil {
			writeErr(w, &gateway.Error{Code: gateway.CodeInvalidParams, Message: err.Error()})
			return
		}
		var req gateway.AgentRequest
		if err := b.gwCall(r.Context(), "request.file", withAuth(auth, m), &req); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, req)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (b *Bridge) handleRequestByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/requests/")
	id, action, _ := strings.Cut(rest, "/")
	if action != "resolve" && action != "" {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Resolution string `json:"resolution"`
		Note       string `json:"note"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, &gateway.Error{Code: gateway.CodeInvalidParams, Message: err.Error()})
		return
	}
	var req gateway.AgentRequest
	if err := b.gwCall(r.Context(), "request.resolve", map[string]any{
		"id":         id,
		"resolution": body.Resolution,
		"note":       body.Note,
	}, &req); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, req)
}

func (b *Bridge) handleAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	var entries []gateway.AuditEntry
	if err := b.gwCall(r.Context(), "audit.read", map[string]any{"after": after, "limit": limit}, &entries); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (b *Bridge) handleInstructions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	text, srcs, err := instructions.EffectiveRoot("", "", "")
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"root_md": text,
		"sources": srcs,
	})
}

// Main runs the session bridge daemon until ctx is cancelled.
func Main(ctx context.Context, cfg Config) error {
	cfg.setDefaults()
	b, err := New(cfg)
	if err != nil {
		return err
	}
	defer b.Close()

	sockListener, err := ListenSocket(cfg.BridgeSocket)
	if err != nil {
		return err
	}
	defer sockListener.Close()

	httpListener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return err
	}
	defer httpListener.Close()

	sockSrv := &SocketServer{B: b}
	httpSrv := &http.Server{Handler: b.Handler()}

	errCh := make(chan error, 2)
	go func() {
		errCh <- sockSrv.Serve(ctx, sockListener)
	}()
	go func() {
		if err := httpSrv.Serve(httpListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		} else {
			errCh <- nil
		}
	}()
	go func() {
		<-ctx.Done()
		_ = sockListener.Close()
		_ = httpSrv.Close()
	}()

	_ = gateway.Notify("READY=1")
	log.Printf("silo bridge listening on %s and http://%s", cfg.BridgeSocket, cfg.HTTPAddr)
	return <-errCh
}
