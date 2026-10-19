package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/events"
	"github.com/bwsalmon/silo/hub/directory"
)

var errNoChatHTTP = &httpError{http.StatusNotFound, "no such chat session"}

// chatRole returns the caller's effective role in chat:
// "admin" or "member" for members of chat.Deployment,
// "participant" for chat.CreatedBy or an invited participant with role "participant",
// "viewer" for an invited participant with role "viewer",
// or "" if the caller has no direct role in the chat.
func (a *API) chatRole(acc *directory.Access, chat ChatSession) string {
	if acc == nil {
		return ""
	}
	if r := acc.Teams[chat.Deployment]; r == directory.Admin || r == directory.Member {
		return string(r)
	}
	for _, p := range chat.Participants {
		if strings.EqualFold(p.Email, acc.Email) {
			if p.Role == ChatRoleViewer {
				return ChatRoleViewer
			}
			return ChatRoleParticipant
		}
	}
	if strings.EqualFold(chat.CreatedBy, acc.Email) {
		return ChatRoleParticipant
	}
	return ""
}

func (a *API) chatSees(acc *directory.Access, chat ChatSession) bool {
	if acc == nil {
		return false
	}
	return a.chatRole(acc, chat) != "" || acc.HubAdmin
}

func (a *API) chatCanPost(acc *directory.Access, chat ChatSession) bool {
	role := a.chatRole(acc, chat)
	return role == string(directory.Admin) || role == string(directory.Member) || role == ChatRoleParticipant
}

func (a *API) chatCanUpdate(acc *directory.Access, chat ChatSession) bool {
	return a.chatCanPost(acc, chat) || (acc != nil && acc.HubAdmin)
}

// ChatSees reports whether acc may view chat.
func (a *API) ChatSees(acc *directory.Access, chat ChatSession) bool {
	return a.chatSees(acc, chat)
}

// ChatCanPost reports whether acc may post messages and answer approvals in chat.
func (a *API) ChatCanPost(acc *directory.Access, chat ChatSession) bool {
	return a.chatCanPost(acc, chat)
}

// ChatCanUpdate reports whether acc may update metadata or bindings on chat.
func (a *API) ChatCanUpdate(acc *directory.Access, chat ChatSession) bool {
	return a.chatCanUpdate(acc, chat)
}

// EnrichChat populates chat's merged participants, awaiting_reply, pending_approvals,
// and optionally its merged transcript.
func (a *API) EnrichChat(ctx context.Context, chat *ChatSession, includeTranscript bool) error {
	return a.enrichChat(ctx, chat, includeTranscript)
}

// PublishChatEvent broadcasts ev to all live SSE subscribers of chatID.
func (a *API) PublishChatEvent(chatID string, ev ChatEvent) {
	a.publishChatEvent(chatID, ev)
}

func (a *API) validateAcceptedParticipants(ctx context.Context, participants []ChatParticipant) error {
	if a.Directory == nil {
		return nil
	}
	for _, p := range participants {
		email := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(p.Email), "user:")))
		if email == "" {
			continue
		}
		ok, err := a.Directory.IsAcceptedUser(ctx, email)
		if err != nil {
			return err
		}
		if !ok {
			return &httpError{http.StatusBadRequest, fmt.Sprintf("participant %s is not an accepted Hub user", email)}
		}
	}
	return nil
}

func (a *API) enrichChat(ctx context.Context, chat *ChatSession, includeTranscript bool) error {
	// Merge deployment members (owner/admin/member) with invited participants.
	merged := []ChatParticipant{}
	seen := map[string]int{}
	if a.Directory != nil && a.Directory.Store != nil {
		teams, err := a.Directory.Store.Teams(ctx)
		if err == nil {
			for _, tm := range teams {
				if tm.ID != chat.Deployment {
					continue
				}
				var emails []string
				roleByEmail := map[string]directory.Role{}
				for principal, role := range tm.Members {
					if !strings.HasPrefix(principal, directory.UserPrefix) {
						continue
					}
					if role != directory.Admin && role != directory.Member {
						continue
					}
					email := strings.TrimPrefix(principal, directory.UserPrefix)
					emails = append(emails, email)
					roleByEmail[email] = role
				}
				slices.Sort(emails)
				for _, email := range emails {
					seen[email] = len(merged)
					merged = append(merged, ChatParticipant{
						Email:  email,
						Role:   string(roleByEmail[email]),
						Source: "deployment",
					})
				}
			}
		}
	}
	for _, inv := range chat.Participants {
		email := strings.ToLower(inv.Email)
		if idx, exists := seen[email]; exists {
			// Deployment members already have deployment-level access, but if they
			// were also listed, preserve their deployment role unless it was viewer.
			if merged[idx].Source == "invited" {
				merged[idx] = inv
			}
			continue
		}
		seen[email] = len(merged)
		merged = append(merged, inv)
	}
	chat.Participants = merged

	// Populate awaiting_reply and pending_approvals from the backing task's live events.
	ie := a.Events
	if ie == nil {
		ie, _ = a.Requests.(InboxEvents)
	}
	if ie != nil && chat.TaskID != "" {
		awaiting, question, awaitingAt, approvals, _, err := ie.TaskInboxState(ctx, chat.TaskID)
		if err == nil {
			if awaiting {
				msgs, merr := a.Store.Messages(ctx, chat.TaskID, false)
				if merr == nil && len(msgs) > 0 {
					lastMsg := msgs[len(msgs)-1]
					if awaitingAt.IsZero() || !lastMsg.SentAt.Before(awaitingAt) {
						awaiting = false
						question = ""
					}
				}
			}
			chat.AwaitingReply = awaiting
			chat.Question = question

			if len(approvals) > 0 {
				ans, aerr := a.Store.Answers(ctx, chat.TaskID, false)
				if aerr == nil {
					answered := map[string]bool{}
					for _, an := range ans {
						answered[an.Request] = true
					}
					now := time.Now()
					var pending []events.ApprovalRequest
					for _, ar := range approvals {
						if answered[ar.ID] {
							continue
						}
						if ar.Deadline != nil && now.After(*ar.Deadline) {
							continue
						}
						pending = append(pending, ar)
					}
					chat.PendingApprovals = pending
				}
			}
		}
	}

	if includeTranscript {
		agentMsgs := a.agentMessagesForTask(ctx, *chat)
		if len(agentMsgs) > 0 {
			combined := make([]ChatMessage, 0, len(chat.Messages)+len(agentMsgs))
			combined = append(combined, chat.Messages...)
			combined = append(combined, agentMsgs...)
			slices.SortStableFunc(combined, func(x, y ChatMessage) int {
				if c := x.CreatedAt.Compare(y.CreatedAt); c != 0 {
					return c
				}
				if x.Role != y.Role {
					if x.Role == "user" {
						return -1
					}
					return 1
				}
				return int(x.Seq - y.Seq)
			})
			for i := range combined {
				combined[i].Seq = int64(i + 1)
			}
			chat.Messages = combined
		}
	} else {
		chat.Messages = nil
	}
	return nil
}

func (a *API) agentMessagesForTask(ctx context.Context, chat ChatSession) []ChatMessage {
	if a.TaskEvents == nil || chat.TaskID == "" {
		return nil
	}
	evs, err := a.TaskEvents(ctx, chat.TaskID)
	if err != nil {
		return nil
	}
	var out []ChatMessage
	for _, e := range evs {
		switch e.Type {
		case events.TypeTranscript:
			if e.Transcript != nil && (e.Transcript.Kind == events.ItemText || e.Transcript.Kind == "") && strings.TrimSpace(e.Transcript.Text) != "" {
				out = append(out, ChatMessage{
					Seq:       e.Seq,
					ID:        fmt.Sprintf("ev-%d", e.Seq),
					ChatID:    chat.ID,
					Sender:    chat.Deployment,
					Role:      "agent",
					Text:      e.Transcript.Text,
					CreatedAt: e.Time,
				})
			}
		case events.TypeAwaitingReply:
			if e.AwaitingReply != nil && e.AwaitingReply.State == events.AwaitingWaiting && strings.TrimSpace(e.AwaitingReply.Question) != "" {
				out = append(out, ChatMessage{
					Seq:       e.Seq,
					ID:        fmt.Sprintf("ev-%d", e.Seq),
					ChatID:    chat.ID,
					Sender:    chat.Deployment,
					Role:      "agent",
					Text:      e.AwaitingReply.Question,
					CreatedAt: e.Time,
				})
			}
		case events.TypeResult:
			if e.Result != nil && strings.TrimSpace(e.Result.FinalText) != "" {
				out = append(out, ChatMessage{
					Seq:       e.Seq,
					ID:        fmt.Sprintf("ev-%d", e.Seq),
					ChatID:    chat.ID,
					Sender:    chat.Deployment,
					Role:      "agent",
					Text:      e.Result.FinalText,
					CreatedAt: e.Time,
				})
			}
		}
	}
	return out
}

func (a *API) listChats(w http.ResponseWriter, r *http.Request, acc *directory.Access) error {
	ctx := r.Context()
	all, err := a.Store.ListChats(ctx)
	if err != nil {
		return err
	}
	depFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("deployment")))
	visible := []ChatSession{}
	for _, c := range all {
		if !a.chatSees(acc, c) {
			continue
		}
		if depFilter != "" && c.Deployment != depFilter && !slices.Contains(c.LinkedDeployments, depFilter) {
			continue
		}
		_ = a.enrichChat(ctx, &c, false)
		visible = append(visible, c)
	}
	return writeJSON(w, http.StatusOK, struct {
		Chats []ChatSession `json:"chats"`
	}{Chats: visible})
}

func (a *API) createChat(w http.ResponseWriter, r *http.Request, acc *directory.Access) error {
	ctx := r.Context()
	var req CreateChatOptions
	if err := readJSON(r, &req); err != nil {
		return err
	}
	dep := strings.ToLower(strings.TrimSpace(req.Deployment))
	if dep == "" {
		return &httpError{http.StatusBadRequest, "deployment is required"}
	}
	if a.Directory != nil && a.Directory.Store != nil {
		exists, err := a.Directory.Store.HasTeam(ctx, dep)
		if err != nil {
			return err
		}
		if !exists {
			return &httpError{http.StatusNotFound, "no such deployment: " + dep}
		}
		role := acc.Teams[dep]
		if role != directory.Admin && role != directory.Member && !acc.HubAdmin {
			return &httpError{http.StatusForbidden, "forbidden: caller does not have access to deployment " + dep}
		}
		for _, ld := range req.LinkedDeployments {
			ldNorm := strings.ToLower(strings.TrimSpace(ld))
			if ldNorm == "" {
				continue
			}
			ldExists, err := a.Directory.Store.HasTeam(ctx, ldNorm)
			if err != nil {
				return err
			}
			if !ldExists {
				return &httpError{http.StatusNotFound, "no such linked deployment: " + ldNorm}
			}
		}
	}
	if err := a.validateAcceptedParticipants(ctx, req.Participants); err != nil {
		return err
	}
	req.CreatedBy = strings.ToLower(acc.Email)
	chat, taskMsg, err := a.Store.CreateChat(ctx, req)
	if err != nil {
		return err
	}
	if a.Dispatcher != nil {
		a.Dispatcher.Wake()
		if taskMsg != nil {
			a.Dispatcher.Posted(ctx, chat.TaskID)
		}
	}
	_ = a.enrichChat(ctx, &chat, true)
	return writeJSON(w, http.StatusCreated, chat)
}

func (a *API) loadChat(r *http.Request, acc *directory.Access) (ChatSession, error) {
	chat, err := a.Store.GetChat(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrNoChat) || (err == nil && !a.chatSees(acc, chat)) {
		return ChatSession{}, errNoChatHTTP
	}
	return chat, err
}

func (a *API) getChat(w http.ResponseWriter, r *http.Request, acc *directory.Access) error {
	chat, err := a.loadChat(r, acc)
	if err != nil {
		return err
	}
	_ = a.enrichChat(r.Context(), &chat, true)
	return writeJSON(w, http.StatusOK, chat)
}

func (a *API) updateChat(w http.ResponseWriter, r *http.Request, acc *directory.Access) error {
	ctx := r.Context()
	chat, err := a.loadChat(r, acc)
	if err != nil {
		return err
	}
	if !a.chatCanUpdate(acc, chat) {
		return &httpError{http.StatusForbidden, "forbidden: viewers cannot modify a shared chat session"}
	}
	var req UpdateChatOptions
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if req.LinkedDeployments != nil && a.Directory != nil && a.Directory.Store != nil {
		for _, ld := range *req.LinkedDeployments {
			ldNorm := strings.ToLower(strings.TrimSpace(ld))
			if ldNorm == "" {
				continue
			}
			exists, err := a.Directory.Store.HasTeam(ctx, ldNorm)
			if err != nil {
				return err
			}
			if !exists {
				return &httpError{http.StatusNotFound, "no such linked deployment: " + ldNorm}
			}
		}
	}
	if req.Participants != nil {
		if err := a.validateAcceptedParticipants(ctx, *req.Participants); err != nil {
			return err
		}
	}
	req.UpdatedBy = strings.ToLower(acc.Email)
	updated, err := a.Store.UpdateChat(ctx, chat.ID, req)
	if err != nil {
		return err
	}
	_ = a.enrichChat(ctx, &updated, true)
	a.publishChatEvent(updated.ID, ChatEvent{
		Type:   "chat_updated",
		Time:   updated.UpdatedAt,
		ChatID: updated.ID,
		TaskID: updated.TaskID,
		Chat:   &updated,
	})
	return writeJSON(w, http.StatusOK, updated)
}

func (a *API) bindGoogleChat(w http.ResponseWriter, r *http.Request, acc *directory.Access) error {
	ctx := r.Context()
	chat, err := a.loadChat(r, acc)
	if err != nil {
		return err
	}
	if !a.chatCanUpdate(acc, chat) {
		return &httpError{http.StatusForbidden, "forbidden: viewers cannot modify a shared chat session"}
	}
	var req GoogleChatBindingRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	updated, err := a.Store.BindGoogleChat(ctx, chat.ID, req.SpaceName, req.ThreadName, req.Metadata)
	if err != nil {
		return err
	}
	_ = a.enrichChat(ctx, &updated, true)
	a.publishChatEvent(updated.ID, ChatEvent{
		Type:   "chat_updated",
		Time:   updated.UpdatedAt,
		ChatID: updated.ID,
		TaskID: updated.TaskID,
		Chat:   &updated,
	})
	return writeJSON(w, http.StatusOK, updated)
}

func (a *API) unbindGoogleChat(w http.ResponseWriter, r *http.Request, acc *directory.Access) error {
	ctx := r.Context()
	chat, err := a.loadChat(r, acc)
	if err != nil {
		return err
	}
	if !a.chatCanUpdate(acc, chat) {
		return &httpError{http.StatusForbidden, "forbidden: viewers cannot modify a shared chat session"}
	}
	updated, err := a.Store.UnbindGoogleChat(ctx, chat.ID)
	if err != nil {
		return err
	}
	_ = a.enrichChat(ctx, &updated, true)
	a.publishChatEvent(updated.ID, ChatEvent{
		Type:   "chat_updated",
		Time:   updated.UpdatedAt,
		ChatID: updated.ID,
		TaskID: updated.TaskID,
		Chat:   &updated,
	})
	return writeJSON(w, http.StatusOK, updated)
}

func (a *API) postChatMessage(w http.ResponseWriter, r *http.Request, acc *directory.Access) error {
	ctx := r.Context()
	chat, err := a.loadChat(r, acc)
	if err != nil {
		return err
	}
	if !a.chatCanPost(acc, chat) {
		return &httpError{http.StatusForbidden, "forbidden: only deployment members and invited participants may post messages"}
	}
	var req PostChatMessageOptions
	if err := readJSON(r, &req); err != nil {
		return err
	}
	req.Sender = strings.ToLower(acc.Email)
	msg, taskMsg, dup, err := a.Store.PostChatMessage(ctx, chat.ID, req)
	if err != nil {
		return err
	}
	if dup {
		return writeJSON(w, http.StatusOK, msg)
	}
	a.publishChatEvent(chat.ID, ChatEvent{
		Type:    "message",
		Time:    msg.CreatedAt,
		ChatID:  chat.ID,
		TaskID:  chat.TaskID,
		Message: &msg,
	})
	if a.OnChatMessagePosted != nil {
		a.OnChatMessagePosted(chat, msg)
	}
	if taskMsg != nil && a.Dispatcher != nil {
		a.Dispatcher.Posted(ctx, chat.TaskID)
	}
	return writeJSON(w, http.StatusCreated, msg)
}

func (a *API) publishChatEvent(chatID string, ev ChatEvent) {
	a.chatMu.Lock()
	if a.chatSeq == nil {
		a.chatSeq = map[string]int64{}
	}
	a.chatSeq[chatID]++
	if ev.Seq == 0 {
		ev.Seq = a.chatSeq[chatID]
	}
	subs := a.chatSubs[chatID]
	for ch := range subs {
		select {
		case ch <- ev:
		default:
		}
	}
	a.chatMu.Unlock()
}

// OnTaskEvent forwards live events from a backing interactive task to any
// connected subscribers of its shared chat session.
func (a *API) OnTaskEvent(taskID string, e events.Event) {
	if a.Store == nil || taskID == "" {
		return
	}
	chat, ok, err := a.Store.GetChatByTask(context.Background(), taskID)
	if err != nil || !ok {
		return
	}
	ce := ChatEvent{
		Type:            e.Type,
		Time:            e.Time,
		ChatID:          chat.ID,
		TaskID:          taskID,
		Transcript:      e.Transcript,
		Status:          e.Status,
		AwaitingReply:   e.AwaitingReply,
		ApprovalRequest: e.ApprovalRequest,
		Result:          e.Result,
	}
	if e.Type == events.TypeTranscript && e.Transcript != nil && (e.Transcript.Kind == events.ItemText || e.Transcript.Kind == "") && strings.TrimSpace(e.Transcript.Text) != "" {
		ce.Message = &ChatMessage{
			Seq:       e.Seq,
			ID:        fmt.Sprintf("ev-%d", e.Seq),
			ChatID:    chat.ID,
			Sender:    chat.Deployment,
			Role:      "agent",
			Text:      e.Transcript.Text,
			CreatedAt: e.Time,
		}
	}
	a.publishChatEvent(chat.ID, ce)
	if a.OnChatTaskEvent != nil {
		a.OnChatTaskEvent(chat, ce)
	}
}

// OnTaskFiled notifies connected subscribers of a shared chat session when a
// child task is proposed/filed from the chat's backing session.
func (a *API) OnTaskFiled(t Task) {
	if a.Store == nil || t.OriginTask == "" {
		return
	}
	chat, ok, err := a.Store.GetChatByTask(context.Background(), t.OriginTask)
	if err != nil || !ok {
		return
	}
	cp := t
	ce := ChatEvent{
		Type:         "task_proposed",
		Time:         t.Created,
		ChatID:       chat.ID,
		TaskID:       chat.TaskID,
		ProposedTask: &cp,
	}
	a.publishChatEvent(chat.ID, ce)
	if a.OnChatTaskEvent != nil {
		a.OnChatTaskEvent(chat, ce)
	}
}

func (a *API) chatEvents(w http.ResponseWriter, r *http.Request, acc *directory.Access) error {
	ctx := r.Context()
	chat, err := a.loadChat(r, acc)
	if err != nil {
		return err
	}
	cursor := r.URL.Query().Get("from")
	if cursor == "" {
		cursor = r.Header.Get("Last-Event-ID")
	}
	var from int64
	if cursor != "" {
		n, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || n < 0 {
			return &httpError{http.StatusBadRequest, "from: want a seq, 0 or more"}
		}
		from = n
	}

	_ = a.enrichChat(ctx, &chat, true)

	// Build initial replay of messages, backing task state, and proposed tasks.
	var replay []ChatEvent
	var seq int64
	for _, m := range chat.Messages {
		seq++
		if seq <= from {
			continue
		}
		mc := m
		mc.Seq = seq
		replay = append(replay, ChatEvent{
			Seq:     seq,
			Type:    "message",
			Time:    mc.CreatedAt,
			ChatID:  chat.ID,
			TaskID:  chat.TaskID,
			Message: &mc,
		})
	}
	if chat.AwaitingReply {
		seq++
		if seq > from {
			replay = append(replay, ChatEvent{
				Seq:    seq,
				Type:   events.TypeAwaitingReply,
				Time:   chat.UpdatedAt,
				ChatID: chat.ID,
				TaskID: chat.TaskID,
				AwaitingReply: &events.AwaitingReply{
					State:    events.AwaitingWaiting,
					Question: chat.Question,
				},
			})
		}
	}
	for _, ar := range chat.PendingApprovals {
		seq++
		if seq <= from {
			continue
		}
		arc := ar
		replay = append(replay, ChatEvent{
			Seq:             seq,
			Type:            events.TypeApprovalRequest,
			Time:            chat.UpdatedAt,
			ChatID:          chat.ID,
			TaskID:          chat.TaskID,
			ApprovalRequest: &arc,
		})
	}
	for _, pt := range chat.ProposedTasks {
		seq++
		if seq <= from {
			continue
		}
		ptc := pt
		replay = append(replay, ChatEvent{
			Seq:          seq,
			Type:         "task_proposed",
			Time:         ptc.Created,
			ChatID:       chat.ID,
			TaskID:       chat.TaskID,
			ProposedTask: &ptc,
		})
	}

	ch := make(chan ChatEvent, 128)
	a.chatMu.Lock()
	if a.chatSubs == nil {
		a.chatSubs = map[string]map[chan ChatEvent]struct{}{}
	}
	if a.chatSeq == nil {
		a.chatSeq = map[string]int64{}
	}
	if seq > a.chatSeq[chat.ID] {
		a.chatSeq[chat.ID] = seq
	}
	if a.chatSubs[chat.ID] == nil {
		a.chatSubs[chat.ID] = map[chan ChatEvent]struct{}{}
	}
	a.chatSubs[chat.ID][ch] = struct{}{}
	a.chatMu.Unlock()
	defer func() {
		a.chatMu.Lock()
		delete(a.chatSubs[chat.ID], ch)
		a.chatMu.Unlock()
	}()

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()

	sendEvent := func(ev ChatEvent) error {
		b, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, b); err != nil {
			return err
		}
		return rc.Flush()
	}

	for _, ev := range replay {
		if err := sendEvent(ev); err != nil {
			return nil
		}
	}

	if chat.State.Terminal() {
		_, _ = io.WriteString(w, "event: end\ndata: {\"type\":\"end\"}\n\n")
		_ = rc.Flush()
		return nil
	}

	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case ev := <-ch:
			if ev.Seq <= seq {
				seq++
				ev.Seq = seq
			} else {
				seq = ev.Seq
			}
			if err := sendEvent(ev); err != nil {
				return nil
			}
			if ev.Type == events.TypeResult {
				_, _ = io.WriteString(w, "event: end\ndata: {\"type\":\"end\"}\n\n")
				_ = rc.Flush()
				return nil
			}
		case <-tick.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return nil
			}
			if err := rc.Flush(); err != nil {
				return nil
			}
		case <-ctx.Done():
			return nil
		}
	}
}
