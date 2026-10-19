// Package gchat implements bidirectional Google Chat integration for Grain Hub
// Shared Chat Sessions (/v1/integrations/google-chat/events).
package gchat

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/events"
	"github.com/bwsalmon/silo/hub/directory"
	"github.com/bwsalmon/silo/hub/identity"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/route"
	"github.com/bwsalmon/silo/hub/tasks"
)

const (
	// EventsPath is the inbound HTTP webhook path for Google Chat events.
	EventsPath = "/v1/integrations/google-chat/events"
	// DefaultCertsURL is Google Chat's published JWK set URL for verifying inbound webhook JWTs.
	DefaultCertsURL = "https://www.googleapis.com/service_accounts/v1/metadata/jwk/chat@system.gserviceaccount.com"
	// DefaultIssuer is the default JWT issuer for Google Chat webhook tokens.
	DefaultIssuer = "chat@system.gserviceaccount.com"
	// AccessDeniedMessage is the refusal text returned when a Google Chat user is
	// not in the Hub's accepted_users list or lacks access to the bound chat.
	AccessDeniedMessage = "You are not in the Grain Hub accepted users list or do not have access to this chat"
)

var allowedIssuers = []string{
	DefaultIssuer,
	"https://accounts.google.com",
	"accounts.google.com",
	"https://chat.googleapis.com",
}

// User represents a Google Chat user or bot.
type User struct {
	Name        string `json:"name,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	Email       string `json:"email,omitempty"`
	Type        string `json:"type,omitempty"`
}

// Space represents a Google Chat space or room.
type Space struct {
	Name        string `json:"name,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	Type        string `json:"type,omitempty"`
}

// SlashCommand identifies a slash command invoked in Google Chat.
type SlashCommand struct {
	CommandID   string `json:"commandId,omitempty"`
	CommandName string `json:"commandName,omitempty"`
}

// UserMentionMetadata describes a user or bot @mention annotation in Google Chat.
type UserMentionMetadata struct {
	User User   `json:"user"`
	Type string `json:"type,omitempty"`
}

// SlashCommandMetadata describes a slash command annotation in Google Chat.
type SlashCommandMetadata struct {
	Bot         User   `json:"bot,omitempty"`
	Type        string `json:"type,omitempty"`
	CommandName string `json:"commandName,omitempty"`
	CommandID   string `json:"commandId,omitempty"`
}

// Annotation is one text annotation on a Google Chat message.
type Annotation struct {
	Type         string                `json:"type"`
	StartIndex   int                   `json:"startIndex,omitempty"`
	Length       int                   `json:"length,omitempty"`
	UserMention  *UserMentionMetadata  `json:"userMention,omitempty"`
	SlashCommand *SlashCommandMetadata `json:"slashCommand,omitempty"`
}

// Attachment represents a file or media attachment on a Google Chat message.
type Attachment struct {
	Name        string `json:"name,omitempty"`
	ContentName string `json:"contentName,omitempty"`
	ContentType string `json:"contentType,omitempty"`
	DownloadURI string `json:"downloadUri,omitempty"`
	Ref         string `json:"ref,omitempty"`
	MediaType   string `json:"media_type,omitempty"`
	Size        int64  `json:"size,omitempty"`
}

// Message is an inbound Google Chat message payload.
type Message struct {
	Name         string        `json:"name,omitempty"`
	Sender       *User         `json:"sender,omitempty"`
	Text         string        `json:"text,omitempty"`
	ArgumentText string        `json:"argumentText,omitempty"`
	Thread       *Thread       `json:"thread,omitempty"`
	Annotations  []Annotation  `json:"annotations,omitempty"`
	SlashCommand *SlashCommand `json:"slashCommand,omitempty"`
	Attachment   []Attachment  `json:"attachment,omitempty"`
	Attachments  []Attachment  `json:"attachments,omitempty"`
}

// FormAction describes a button click action in a CARD_CLICKED event.
type FormAction struct {
	ActionMethodName string            `json:"actionMethodName,omitempty"`
	Function         string            `json:"function,omitempty"`
	Parameters       []ActionParameter `json:"parameters,omitempty"`
}

// CommonEvent carries common Google Workspace Add-on card action metadata.
type CommonEvent struct {
	InvokedFunction string            `json:"invokedFunction,omitempty"`
	Parameters      map[string]string `json:"parameters,omitempty"`
}

// Event is an inbound Google Chat webhook event delivered to POST /v1/integrations/google-chat/events.
type Event struct {
	Type      string       `json:"type"`
	EventTime string       `json:"eventTime,omitempty"`
	Token     string       `json:"token,omitempty"`
	User      User         `json:"user"`
	Space     Space        `json:"space"`
	Message   *Message     `json:"message,omitempty"`
	Thread    *Thread      `json:"thread,omitempty"`
	Action    *FormAction  `json:"action,omitempty"`
	Common    *CommonEvent `json:"common,omitempty"`
}

// Handler manages inbound Google Chat webhook events and outbound delivery to bound spaces/threads.
type Handler struct {
	Store      *tasks.Store
	Dispatcher *tasks.Dispatcher
	Directory  *directory.API
	TasksAPI   *tasks.API
	Requests   tasks.ApprovalRequests
	Registry   *registry.Registry
	HubURL     string

	// Audience is the expected JWT audience (Config.GoogleChatAudience).
	// When empty, inbound webhook requests are refused as unauthenticated.
	Audience string
	// CertsURL overrides DefaultCertsURL when Keys is nil.
	CertsURL string
	// Keys, when set, is the JWK keyset used to verify Google Chat Bearer JWTs.
	Keys *identity.KeySet
	// Sender delivers outbound messages and card updates to Google Chat.
	Sender Sender
	// Now, when set, is the clock used for JWT verification.
	Now func() time.Time
	// Logf, when set, logs integration errors.
	Logf func(format string, args ...any)

	keysOnce     sync.Once
	resolvedKeys *identity.KeySet

	mu           sync.Mutex
	cardMessages map[string]string
}

// Handler returns an http.Handler serving POST /v1/integrations/google-chat/events.
func (h *Handler) Handler() http.Handler {
	m := route.NewMux()
	m.HandleFunc("POST "+EventsPath, h.serveEvents)
	return m
}

func (h *Handler) keySet() *identity.KeySet {
	h.keysOnce.Do(func() {
		if h.Keys != nil {
			h.resolvedKeys = h.Keys
			return
		}
		u := strings.TrimSpace(h.CertsURL)
		if u == "" {
			u = DefaultCertsURL
		}
		h.resolvedKeys = &identity.KeySet{URL: u}
	})
	return h.resolvedKeys
}

func (h *Handler) hubBaseURL() string {
	u := strings.TrimRight(strings.TrimSpace(h.HubURL), "/")
	if u == "" && h.Directory != nil {
		u = strings.TrimRight(strings.TrimSpace(h.Directory.HubURL), "/")
	}
	if u == "" {
		u = "https://hub.local"
	}
	return u
}

func (h *Handler) verifyJWT(ctx context.Context, r *http.Request) (*identity.Claims, error) {
	aud := strings.TrimSpace(h.Audience)
	if aud == "" {
		return nil, errors.New("gchat: google chat audience is not configured")
	}
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	tok, ok := strings.CutPrefix(authHeader, "Bearer ")
	if !ok {
		tok, ok = strings.CutPrefix(authHeader, "bearer ")
	}
	tok = strings.TrimSpace(tok)
	if !ok || tok == "" {
		return nil, errors.New("gchat: missing Authorization Bearer token")
	}

	issuer := DefaultIssuer
	if parts := strings.Split(tok, "."); len(parts) == 3 {
		if raw, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
			var peek struct {
				Iss string `json:"iss"`
			}
			if json.Unmarshal(raw, &peek) == nil && slices.Contains(allowedIssuers, peek.Iss) {
				issuer = peek.Iss
			}
		}
	}

	want := identity.Expect{
		Issuer:   issuer,
		Audience: aud,
	}
	if h.Now != nil {
		want.Now = h.Now()
	}
	return identity.Verify(ctx, h.keySet(), tok, want)
}

func (h *Handler) serveEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, err := h.verifyJWT(ctx, r); err != nil {
		http.Error(w, "unauthenticated: "+err.Error(), http.StatusUnauthorized)
		return
	}

	var ev Event
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&ev); err != nil {
		http.Error(w, "bad request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	email := strings.ToLower(strings.TrimSpace(ev.User.Email))
	if email == "" && ev.Message != nil && ev.Message.Sender != nil {
		email = strings.ToLower(strings.TrimSpace(ev.Message.Sender.Email))
	}
	if email == "" || h.Directory == nil {
		writeCardResponse(w, http.StatusForbidden, buildAccessDeniedResponse())
		return
	}

	acc, err := h.Directory.AccessForEmail(ctx, email)
	if err != nil || acc == nil {
		writeCardResponse(w, http.StatusForbidden, buildAccessDeniedResponse())
		return
	}

	spaceName := normalizeSpaceName(ev.Space.Name)
	threadName := ""
	if ev.Message != nil && ev.Message.Thread != nil {
		threadName = strings.TrimSpace(ev.Message.Thread.Name)
	} else if ev.Thread != nil {
		threadName = strings.TrimSpace(ev.Thread.Name)
	}

	evType := strings.ToUpper(strings.TrimSpace(ev.Type))

	// Check if the event invokes a /grain slash command.
	if subcmd, args, isCmd := parseGrainCommand(ev); isCmd {
		h.handleSlashCommand(ctx, w, acc, ev, spaceName, threadName, subcmd, args)
		return
	}

	switch evType {
	case "ADDED_TO_SPACE":
		h.handleAddedToSpace(ctx, w, acc, spaceName, threadName)
	case "MESSAGE":
		h.handleMessage(ctx, w, acc, ev, spaceName, threadName)
	case "CARD_CLICKED":
		h.handleCardClicked(ctx, w, acc, ev, spaceName, threadName)
	default:
		writeCardResponse(w, http.StatusOK, OutboundMessage{Text: "ok"})
	}
}

func (h *Handler) canSeeChat(acc *directory.Access, chat tasks.ChatSession) bool {
	if h.TasksAPI != nil {
		return h.TasksAPI.ChatSees(acc, chat)
	}
	if acc == nil {
		return false
	}
	if acc.HubAdmin {
		return true
	}
	if r := acc.Teams[chat.Deployment]; r == directory.Admin || r == directory.Member {
		return true
	}
	if strings.EqualFold(chat.CreatedBy, acc.Email) {
		return true
	}
	for _, p := range chat.Participants {
		if strings.EqualFold(p.Email, acc.Email) {
			return true
		}
	}
	return false
}

func (h *Handler) canPostChat(acc *directory.Access, chat tasks.ChatSession) bool {
	if h.TasksAPI != nil {
		return h.TasksAPI.ChatCanPost(acc, chat) || (acc != nil && acc.HubAdmin)
	}
	if acc == nil {
		return false
	}
	if acc.HubAdmin {
		return true
	}
	if r := acc.Teams[chat.Deployment]; r == directory.Admin || r == directory.Member {
		return true
	}
	if strings.EqualFold(chat.CreatedBy, acc.Email) {
		return true
	}
	for _, p := range chat.Participants {
		if strings.EqualFold(p.Email, acc.Email) && p.Role != tasks.ChatRoleViewer {
			return true
		}
	}
	return false
}

func (h *Handler) canUpdateChat(acc *directory.Access, chat tasks.ChatSession) bool {
	if h.TasksAPI != nil {
		return h.TasksAPI.ChatCanUpdate(acc, chat)
	}
	return h.canPostChat(acc, chat)
}

func (h *Handler) handleAddedToSpace(ctx context.Context, w http.ResponseWriter, acc *directory.Access, spaceName, threadName string) {
	if h.Store != nil && spaceName != "" {
		if chat, ok, err := h.Store.FindChatByGoogleChat(ctx, spaceName, threadName); err == nil && ok {
			if !h.canSeeChat(acc, chat) {
				writeCardResponse(w, http.StatusForbidden, buildAccessDeniedResponse())
				return
			}
			writeCardResponse(w, http.StatusOK, h.buildStatusResponse(chat, "Grain Hub Connected"))
			return
		}
	}
	writeCardResponse(w, http.StatusOK, OutboundMessage{
		Text: "Grain Hub bot added. Use `/grain link <chat-id>` to link an existing Shared Chat Session or `/grain new <deployment> <title>` to start a new one.",
		CardsV2: []CardWithID{
			{
				CardID: "grain-welcome",
				Card: Card{
					Header: &CardHeader{
						Title:    "Grain Hub",
						Subtitle: "Shared Chat Session Integration",
					},
					Sections: []CardSection{
						{
							Widgets: []Widget{
								{
									TextParagraph: &TextParagraph{
										Text: "Use `/grain link <chat-id>`, `/grain new <deployment> <title>`, `/grain mode mention|all`, or `/grain status`.",
									},
								},
							},
						},
					},
				},
			},
		},
	})
}

func (h *Handler) handleSlashCommand(ctx context.Context, w http.ResponseWriter, acc *directory.Access, ev Event, spaceName, threadName, subcmd string, args []string) {
	switch strings.ToLower(subcmd) {
	case "link":
		if len(args) < 1 || strings.TrimSpace(args[0]) == "" {
			writeCardResponse(w, http.StatusBadRequest, OutboundMessage{
				Text: "Usage: `/grain link <chat-id>`",
			})
			return
		}
		chatID := strings.TrimSpace(args[0])
		chat, err := h.Store.GetChat(ctx, chatID)
		if errors.Is(err, tasks.ErrNoChat) {
			writeCardResponse(w, http.StatusNotFound, OutboundMessage{
				Text: fmt.Sprintf("Chat session %q was not found.", chatID),
			})
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !h.canUpdateChat(acc, chat) {
			writeCardResponse(w, http.StatusForbidden, buildAccessDeniedResponse())
			return
		}
		meta := map[string]string{}
		if ev.Space.DisplayName != "" {
			meta["space_display_name"] = ev.Space.DisplayName
		}
		updated, err := h.Store.BindGoogleChat(ctx, chat.ID, spaceName, threadName, meta)
		if err != nil {
			writeCardResponse(w, http.StatusBadRequest, OutboundMessage{Text: err.Error()})
			return
		}
		if h.TasksAPI != nil {
			_ = h.TasksAPI.EnrichChat(ctx, &updated, true)
			h.TasksAPI.PublishChatEvent(updated.ID, tasks.ChatEvent{
				Type:   "chat_updated",
				Time:   updated.UpdatedAt,
				ChatID: updated.ID,
				TaskID: updated.TaskID,
				Chat:   &updated,
			})
		}
		writeCardResponse(w, http.StatusOK, h.buildStatusResponse(updated, fmt.Sprintf("Linked to Shared Chat %s", updated.ID)))

	case "new":
		if len(args) < 1 || strings.TrimSpace(args[0]) == "" {
			writeCardResponse(w, http.StatusBadRequest, OutboundMessage{
				Text: "Usage: `/grain new <deployment> <title>`",
			})
			return
		}
		dep := strings.ToLower(strings.TrimSpace(args[0]))
		title := strings.TrimSpace(strings.Join(args[1:], " "))
		if title == "" {
			title = cmp.Or(strings.TrimSpace(ev.Space.DisplayName), "Shared Chat ("+dep+")")
		}
		if h.Directory != nil && h.Directory.Store != nil {
			exists, err := h.Directory.Store.HasTeam(ctx, dep)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if !exists {
				writeCardResponse(w, http.StatusNotFound, OutboundMessage{
					Text: fmt.Sprintf("Deployment %q does not exist.", dep),
				})
				return
			}
			role := acc.Teams[dep]
			if role != directory.Admin && role != directory.Member && !acc.HubAdmin {
				writeCardResponse(w, http.StatusForbidden, buildAccessDeniedResponse())
				return
			}
		}
		created, _, err := h.Store.CreateChat(ctx, tasks.CreateChatOptions{
			Title:      title,
			Deployment: dep,
			ReplyMode:  tasks.ReplyModeMention,
			CreatedBy:  acc.Email,
		})
		if err != nil {
			writeCardResponse(w, http.StatusBadRequest, OutboundMessage{Text: err.Error()})
			return
		}
		meta := map[string]string{}
		if ev.Space.DisplayName != "" {
			meta["space_display_name"] = ev.Space.DisplayName
		}
		updated, err := h.Store.BindGoogleChat(ctx, created.ID, spaceName, threadName, meta)
		if err != nil {
			writeCardResponse(w, http.StatusBadRequest, OutboundMessage{Text: err.Error()})
			return
		}
		if h.Dispatcher != nil {
			h.Dispatcher.Wake()
		}
		if h.TasksAPI != nil {
			_ = h.TasksAPI.EnrichChat(ctx, &updated, true)
		}
		writeCardResponse(w, http.StatusOK, h.buildStatusResponse(updated, fmt.Sprintf("Created and linked Shared Chat %s", updated.ID)))

	case "mode":
		if len(args) < 1 {
			writeCardResponse(w, http.StatusBadRequest, OutboundMessage{
				Text: "Usage: `/grain mode mention|all`",
			})
			return
		}
		chat, ok, err := h.Store.FindChatByGoogleChat(ctx, spaceName, threadName)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			writeCardResponse(w, http.StatusNotFound, OutboundMessage{
				Text: "No Shared Chat Session is linked to this space. Use `/grain link <chat-id>` or `/grain new <deployment> <title>`.",
			})
			return
		}
		if !h.canUpdateChat(acc, chat) {
			writeCardResponse(w, http.StatusForbidden, buildAccessDeniedResponse())
			return
		}
		rawMode := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(args[0])), "@")
		if rawMode != tasks.ReplyModeMention && rawMode != tasks.ReplyModeAll {
			writeCardResponse(w, http.StatusBadRequest, OutboundMessage{
				Text: fmt.Sprintf("Invalid mode %q: want `mention` or `all`.", args[0]),
			})
			return
		}
		updated, err := h.Store.UpdateChat(ctx, chat.ID, tasks.UpdateChatOptions{
			ReplyMode: &rawMode,
			UpdatedBy: acc.Email,
		})
		if err != nil {
			writeCardResponse(w, http.StatusBadRequest, OutboundMessage{Text: err.Error()})
			return
		}
		if h.TasksAPI != nil {
			_ = h.TasksAPI.EnrichChat(ctx, &updated, true)
			h.TasksAPI.PublishChatEvent(updated.ID, tasks.ChatEvent{
				Type:   "chat_updated",
				Time:   updated.UpdatedAt,
				ChatID: updated.ID,
				TaskID: updated.TaskID,
				Chat:   &updated,
			})
		}
		writeCardResponse(w, http.StatusOK, h.buildStatusResponse(updated, fmt.Sprintf("Reply mode set to %s", updated.ReplyMode)))

	case "status":
		chat, ok, err := h.Store.FindChatByGoogleChat(ctx, spaceName, threadName)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			writeCardResponse(w, http.StatusNotFound, OutboundMessage{
				Text: "No Shared Chat Session is linked to this space. Use `/grain link <chat-id>` or `/grain new <deployment> <title>`.",
			})
			return
		}
		if !h.canSeeChat(acc, chat) {
			writeCardResponse(w, http.StatusForbidden, buildAccessDeniedResponse())
			return
		}
		if h.TasksAPI != nil {
			_ = h.TasksAPI.EnrichChat(ctx, &chat, false)
		}
		writeCardResponse(w, http.StatusOK, h.buildStatusResponse(chat, "Grain Shared Chat Status"))

	default:
		writeCardResponse(w, http.StatusBadRequest, OutboundMessage{
			Text: "Unknown `/grain` command. Supported commands: `/grain link <chat-id>`, `/grain new <deployment> <title>`, `/grain mode mention|all`, `/grain status`.",
		})
	}
}

func (h *Handler) handleMessage(ctx context.Context, w http.ResponseWriter, acc *directory.Access, ev Event, spaceName, threadName string) {
	chat, ok, err := h.Store.FindChatByGoogleChat(ctx, spaceName, threadName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		writeCardResponse(w, http.StatusNotFound, OutboundMessage{
			Text: "This Google Chat space is not linked to a Grain Hub Shared Chat Session. Use `/grain link <chat-id>` or `/grain new <deployment> <title>`.",
		})
		return
	}
	if !h.canPostChat(acc, chat) {
		writeCardResponse(w, http.StatusForbidden, buildAccessDeniedResponse())
		return
	}
	if ev.Message == nil {
		writeCardResponse(w, http.StatusBadRequest, OutboundMessage{Text: "missing message payload"})
		return
	}

	rawText := ev.Message.Text
	if strings.TrimSpace(rawText) == "" && strings.TrimSpace(ev.Message.ArgumentText) != "" {
		rawText = ev.Message.ArgumentText
	}
	cleanedText, hadLeadingBotMention := stripLeadingBotMention(rawText)
	botAnnotation := hasBotMentionAnnotation(ev.Message)
	mentionedAgent, _ := tasks.MentionsAgent(cleanedText, chat.Deployment, chat.LinkedDeployments)
	wakeAgent := chat.ReplyMode == tasks.ReplyModeAll || hadLeadingBotMention || botAnnotation || mentionedAgent

	attachments := extractAttachments(ev.Message)
	if strings.TrimSpace(cleanedText) == "" && len(attachments) == 0 {
		if wakeAgent {
			cleanedText = "@agent"
		} else {
			writeCardResponse(w, http.StatusBadRequest, OutboundMessage{Text: "empty message"})
			return
		}
	}

	msgID := strings.TrimSpace(ev.Message.Name)
	createdMsg, taskMsg, dup, err := h.Store.PostChatMessage(ctx, chat.ID, tasks.PostChatMessageOptions{
		ID:           msgID,
		Text:         cleanedText,
		Attachments:  attachments,
		TriggerAgent: wakeAgent,
		Sender:       acc.Email,
	})
	if err != nil {
		writeCardResponse(w, http.StatusBadRequest, OutboundMessage{Text: err.Error()})
		return
	}
	if !dup {
		if h.TasksAPI != nil {
			h.TasksAPI.PublishChatEvent(chat.ID, tasks.ChatEvent{
				Type:    "message",
				Time:    createdMsg.CreatedAt,
				ChatID:  chat.ID,
				TaskID:  chat.TaskID,
				Message: &createdMsg,
			})
		}
		if taskMsg != nil && h.Dispatcher != nil {
			h.Dispatcher.Posted(ctx, chat.TaskID)
		}
	}
	writeCardResponse(w, http.StatusOK, OutboundMessage{})
}

func (h *Handler) handleCardClicked(ctx context.Context, w http.ResponseWriter, acc *directory.Access, ev Event, spaceName, threadName string) {
	fn, params := extractCardAction(ev)
	decision := strings.ToLower(strings.TrimSpace(cmp.Or(params["decision"], params["action"])))
	switch strings.ToLower(fn) {
	case "allow", "allow_approval":
		if decision == "" {
			decision = tasks.Allow
		}
	case "deny", "deny_approval":
		if decision == "" {
			decision = tasks.Deny
		}
	case "approve", "approve_task", "approve_proposal":
		if decision == "" {
			decision = "approve"
		}
	case "reject", "reject_task", "reject_proposal":
		if decision == "" {
			decision = "reject"
		}
	}

	requestID := strings.TrimSpace(cmp.Or(params["request_id"], params["request"]))
	taskID := strings.TrimSpace(cmp.Or(params["task_id"], params["task"]))
	chatID := strings.TrimSpace(params["chat_id"])
	reason := strings.TrimSpace(params["reason"])

	isApproval := requestID != "" || decision == tasks.Allow || decision == tasks.Deny ||
		strings.Contains(strings.ToLower(fn), "approval")
	if isApproval {
		if taskID == "" {
			if chatID != "" {
				if c, err := h.Store.GetChat(ctx, chatID); err == nil {
					taskID = c.TaskID
				}
			} else if c, ok, err := h.Store.FindChatByGoogleChat(ctx, spaceName, threadName); err == nil && ok {
				taskID = c.TaskID
				chatID = c.ID
			}
		}
		if taskID == "" || requestID == "" || (decision != tasks.Allow && decision != tasks.Deny) {
			writeCardResponse(w, http.StatusBadRequest, OutboundMessage{
				Text: "Invalid approval card action parameters.",
			})
			return
		}
		t, err := h.Store.Get(ctx, taskID)
		if err != nil {
			writeCardResponse(w, http.StatusNotFound, OutboundMessage{Text: "Task not found."})
			return
		}
		chat, hasChat, _ := h.Store.GetChatByTask(ctx, t.ID)
		if hasChat && chatID == "" {
			chatID = chat.ID
		}
		canAnswer := acc.MemberOf(t.Team) || acc.HubAdmin || (hasChat && h.canPostChat(acc, chat))
		if !canAnswer {
			writeCardResponse(w, http.StatusForbidden, buildAccessDeniedResponse())
			return
		}
		var toolName string
		if h.Requests != nil {
			ar, ok, err := h.Requests.ApprovalRequest(ctx, t.ID, requestID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if !ok {
				writeCardResponse(w, http.StatusNotFound, OutboundMessage{Text: "Approval request not found."})
				return
			}
			if ar.Deadline != nil && time.Now().After(*ar.Deadline) {
				writeCardResponse(w, http.StatusConflict, OutboundMessage{Text: "Approval request deadline has passed."})
				return
			}
			toolName = ar.Tool
		}
		ans, err := h.Store.Answer(ctx, tasks.Answer{
			Task:       t.ID,
			Request:    requestID,
			Decision:   decision,
			AnsweredBy: acc.Email,
			Reason:     reason,
		})
		if err != nil {
			writeCardResponse(w, http.StatusConflict, OutboundMessage{Text: err.Error()})
			return
		}
		if h.Dispatcher != nil {
			h.Dispatcher.Answered(ctx, t.ID)
		}
		if hasChat && h.TasksAPI != nil {
			updatedChat, cerr := h.Store.GetChat(ctx, chat.ID)
			if cerr == nil {
				_ = h.TasksAPI.EnrichChat(ctx, &updatedChat, true)
				h.TasksAPI.PublishChatEvent(chat.ID, tasks.ChatEvent{
					Type:   "chat_updated",
					Time:   ans.AnsweredAt,
					ChatID: chat.ID,
					TaskID: t.ID,
					Chat:   &updatedChat,
				})
			}
		}
		updatedCard := h.buildApprovalResolvedCard(chatID, t.ID, requestID, toolName, decision, acc.Email)
		h.updateTrackedCard(ctx, ev, "approval:"+t.ID+":"+requestID, updatedCard)
		writeCardResponse(w, http.StatusOK, updatedCard)
		return
	}

	// Proposed task Approve / Reject
	if taskID == "" {
		taskID = strings.TrimSpace(params["id"])
	}
	if taskID == "" || (decision != "approve" && decision != "reject") {
		writeCardResponse(w, http.StatusBadRequest, OutboundMessage{
			Text: "Invalid proposal card action parameters.",
		})
		return
	}
	t, err := h.Store.Get(ctx, taskID)
	if err != nil {
		writeCardResponse(w, http.StatusNotFound, OutboundMessage{Text: "Proposed task not found."})
		return
	}
	chat, hasChat, _ := h.Store.GetChatByTask(ctx, t.OriginTask)
	if hasChat && chatID == "" {
		chatID = chat.ID
	}
	canReview := acc.MemberOf(t.Team) || acc.HubAdmin || (hasChat && h.canPostChat(acc, chat))
	if !hasChat && strings.EqualFold(t.Submitter, acc.Email) {
		canReview = false
	}
	if !canReview {
		writeCardResponse(w, http.StatusForbidden, buildAccessDeniedResponse())
		return
	}

	var updatedTask tasks.Task
	if decision == "approve" {
		updatedTask, err = h.Store.Approve(ctx, t.ID, acc.Email)
		if err != nil {
			writeCardResponse(w, http.StatusConflict, OutboundMessage{Text: err.Error()})
			return
		}
		if h.Dispatcher != nil {
			h.Dispatcher.Wake()
		}
	} else {
		rejReason := "rejected by " + acc.Email
		if reason != "" {
			rejReason += ": " + reason
		}
		updatedTask, err = h.Store.Reject(ctx, t.ID, rejReason)
		if err != nil {
			writeCardResponse(w, http.StatusConflict, OutboundMessage{Text: err.Error()})
			return
		}
	}

	if hasChat && h.TasksAPI != nil {
		cp := updatedTask
		h.TasksAPI.PublishChatEvent(chat.ID, tasks.ChatEvent{
			Type:         "task_proposed",
			Time:         updatedTask.Updated,
			ChatID:       chat.ID,
			TaskID:       chat.TaskID,
			ProposedTask: &cp,
		})
	}

	updatedCard := h.buildProposalResolvedCard(chatID, updatedTask, decision, acc.Email)
	h.updateTrackedCard(ctx, ev, "proposal:"+t.ID, updatedCard)
	writeCardResponse(w, http.StatusOK, updatedCard)
}

func (h *Handler) trackCardMessage(key, msgName string) {
	if key == "" || msgName == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cardMessages == nil {
		h.cardMessages = map[string]string{}
	}
	h.cardMessages[key] = msgName
}

func (h *Handler) updateTrackedCard(ctx context.Context, ev Event, key string, updated OutboundMessage) {
	if h.Sender == nil {
		return
	}
	msgName := ""
	if ev.Message != nil {
		msgName = strings.TrimSpace(ev.Message.Name)
	}
	if msgName == "" && key != "" {
		h.mu.Lock()
		msgName = h.cardMessages[key]
		h.mu.Unlock()
	}
	if msgName != "" {
		_ = h.Sender.UpdateMessage(ctx, msgName, updated)
	}
}

// OnWebMessage mirrors a human's message posted in the Hub web UI (/chats/{id})
// to all bound Google Chat spaces/threads attributed to [<sender-email>].
func (h *Handler) OnWebMessage(chat tasks.ChatSession, msg tasks.ChatMessage) {
	if h == nil || h.Sender == nil {
		return
	}
	if h.Store != nil {
		if fresh, err := h.Store.GetChat(context.Background(), chat.ID); err == nil {
			chat = fresh
		}
	}
	for _, b := range chat.ExternalBindings {
		if b.Provider != tasks.ProviderGoogleChat || strings.TrimSpace(b.ExternalID) == "" {
			continue
		}
		out := OutboundMessage{
			Text: fmt.Sprintf("[%s]: %s", msg.Sender, msg.Text),
		}
		_, _ = h.Sender.SendMessage(context.Background(), b.ExternalID, b.ThreadID, out)
	}
}

// OnChatTaskEvent delivers agent replies, clarifying questions, approval_request
// cards, and proposed task cards to all bound Google Chat spaces/threads.
func (h *Handler) OnChatTaskEvent(chat tasks.ChatSession, ev tasks.ChatEvent) {
	if h == nil || h.Sender == nil {
		return
	}
	if h.Store != nil {
		if fresh, err := h.Store.GetChat(context.Background(), chat.ID); err == nil {
			chat = fresh
		}
	}
	var bindings []tasks.ChatExternalBinding
	for _, b := range chat.ExternalBindings {
		if b.Provider == tasks.ProviderGoogleChat && strings.TrimSpace(b.ExternalID) != "" {
			bindings = append(bindings, b)
		}
	}
	if len(bindings) == 0 {
		return
	}

	var (
		out      *OutboundMessage
		trackKey string
	)
	switch ev.Type {
	case events.TypeTranscript:
		if ev.Message != nil && strings.TrimSpace(ev.Message.Text) != "" {
			out = &OutboundMessage{
				Text: fmt.Sprintf("[@%s]: %s", chat.Deployment, ev.Message.Text),
			}
		}
	case events.TypeResult:
		if ev.Result != nil && strings.TrimSpace(ev.Result.FinalText) != "" {
			out = &OutboundMessage{
				Text: fmt.Sprintf("[@%s]: %s", chat.Deployment, ev.Result.FinalText),
			}
		}
	case events.TypeAwaitingReply:
		if ev.AwaitingReply != nil && ev.AwaitingReply.State == events.AwaitingWaiting && strings.TrimSpace(ev.AwaitingReply.Question) != "" {
			msg := h.buildAwaitingReplyCard(chat, ev.AwaitingReply.Question)
			out = &msg
		}
	case events.TypeApprovalRequest:
		if ev.ApprovalRequest != nil {
			msg := h.buildApprovalRequestCard(chat, *ev.ApprovalRequest)
			out = &msg
			trackKey = "approval:" + chat.TaskID + ":" + ev.ApprovalRequest.ID
		}
	case "task_proposed":
		if ev.ProposedTask != nil {
			msg := h.buildProposedTaskCard(chat, *ev.ProposedTask)
			out = &msg
			trackKey = "proposal:" + ev.ProposedTask.ID
		}
	}
	if out == nil {
		return
	}
	for _, b := range bindings {
		msgName, err := h.Sender.SendMessage(context.Background(), b.ExternalID, b.ThreadID, *out)
		if err == nil && trackKey != "" && msgName != "" {
			h.trackCardMessage(trackKey, msgName)
		}
	}
}

func buildAccessDeniedResponse() OutboundMessage {
	return OutboundMessage{
		Text: AccessDeniedMessage,
		CardsV2: []CardWithID{
			{
				CardID: "grain-access-denied",
				Card: Card{
					Header: &CardHeader{
						Title:    "Grain Hub — Access Denied",
						Subtitle: "Authorization Required",
					},
					Sections: []CardSection{
						{
							Widgets: []Widget{
								{
									TextParagraph: &TextParagraph{
										Text: AccessDeniedMessage,
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func (h *Handler) buildStatusResponse(chat tasks.ChatSession, headerTitle string) OutboundMessage {
	chatPath := "/chats/" + chat.ID
	chatURL := h.hubBaseURL() + chatPath
	depStatus := "offline"
	if h.Registry != nil {
		if _, ok := h.Registry.Get(chat.Deployment); ok {
			depStatus = "online"
		}
	}
	stateStr := string(chat.State)
	if stateStr == "" {
		stateStr = "queued"
	}
	summary := fmt.Sprintf("%s — Chat `%s` (%s) | Backing deployment: `@%s` (%s, session %s) | Reply mode: `%s`",
		headerTitle, chat.ID, chatPath, chat.Deployment, depStatus, stateStr, chat.ReplyMode)
	return OutboundMessage{
		Text: summary,
		CardsV2: []CardWithID{
			{
				CardID: "grain-chat-status-" + chat.ID,
				Card: Card{
					Header: &CardHeader{
						Title:    headerTitle,
						Subtitle: fmt.Sprintf("%s (%s)", chat.Title, chat.ID),
					},
					Sections: []CardSection{
						{
							Widgets: []Widget{
								{
									DecoratedText: &DecoratedText{
										TopLabel: "Hub Shared Chat",
										Text:     chatPath,
									},
								},
								{
									DecoratedText: &DecoratedText{
										TopLabel: "Backing Deployment",
										Text:     fmt.Sprintf("@%s (%s · %s)", chat.Deployment, depStatus, stateStr),
									},
								},
								{
									DecoratedText: &DecoratedText{
										TopLabel: "Reply Mode",
										Text:     chat.ReplyMode,
									},
								},
								{
									ButtonList: &ButtonList{
										Buttons: []Button{
											{
												Text: "Open in Grain Hub",
												OnClick: &OnClick{
													OpenLink: &OpenLink{URL: chatURL},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func (h *Handler) buildAwaitingReplyCard(chat tasks.ChatSession, question string) OutboundMessage {
	chatURL := h.hubBaseURL() + "/chats/" + chat.ID
	return OutboundMessage{
		Text: fmt.Sprintf("[@%s] Question: %s", chat.Deployment, question),
		CardsV2: []CardWithID{
			{
				CardID: "grain-question-" + chat.ID,
				Card: Card{
					Header: &CardHeader{
						Title:    fmt.Sprintf("@%s is awaiting a reply", chat.Deployment),
						Subtitle: chat.Title,
					},
					Sections: []CardSection{
						{
							Widgets: []Widget{
								{
									TextParagraph: &TextParagraph{Text: question},
								},
								{
									ButtonList: &ButtonList{
										Buttons: []Button{
											{
												Text: "Open in Grain Hub",
												OnClick: &OnClick{
													OpenLink: &OpenLink{URL: chatURL},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func (h *Handler) buildApprovalRequestCard(chat tasks.ChatSession, req events.ApprovalRequest) OutboundMessage {
	chatURL := h.hubBaseURL() + "/chats/" + chat.ID
	desc := fmt.Sprintf("Tool: `%s`", req.Tool)
	if req.Message != "" {
		desc += " — " + req.Message
	}
	params := func(decision string) []ActionParameter {
		return []ActionParameter{
			{Key: "task_id", Value: chat.TaskID},
			{Key: "request_id", Value: req.ID},
			{Key: "decision", Value: decision},
			{Key: "chat_id", Value: chat.ID},
		}
	}
	return OutboundMessage{
		Text: fmt.Sprintf("[@%s] Approval requested (%s): %s", chat.Deployment, req.ID, desc),
		CardsV2: []CardWithID{
			{
				CardID: "grain-approval-" + req.ID,
				Card: Card{
					Header: &CardHeader{
						Title:    fmt.Sprintf("Approval Requested by @%s", chat.Deployment),
						Subtitle: fmt.Sprintf("Request %s · %s", req.ID, req.Tool),
					},
					Sections: []CardSection{
						{
							Widgets: []Widget{
								{
									TextParagraph: &TextParagraph{Text: desc},
								},
								{
									ButtonList: &ButtonList{
										Buttons: []Button{
											{
												Text: "Allow",
												OnClick: &OnClick{
													Action: &Action{
														Function:   "answer_approval",
														Parameters: params(tasks.Allow),
													},
												},
											},
											{
												Text: "Deny",
												OnClick: &OnClick{
													Action: &Action{
														Function:   "answer_approval",
														Parameters: params(tasks.Deny),
													},
												},
											},
											{
												Text: "Open in Grain Hub",
												OnClick: &OnClick{
													OpenLink: &OpenLink{URL: chatURL},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func (h *Handler) buildApprovalResolvedCard(chatID, taskID, requestID, toolName, decision, clickerEmail string) OutboundMessage {
	hubLink := h.hubBaseURL() + "/tasks/" + taskID
	if chatID != "" {
		hubLink = h.hubBaseURL() + "/chats/" + chatID
	}
	label := "Allowed"
	if decision == tasks.Deny {
		label = "Denied"
	}
	summary := fmt.Sprintf("Approval request %s %s by %s", requestID, strings.ToLower(label), clickerEmail)
	return OutboundMessage{
		Text:           summary,
		ActionResponse: &ActionResponse{Type: "UPDATE_MESSAGE"},
		CardsV2: []CardWithID{
			{
				CardID: "grain-approval-" + requestID,
				Card: Card{
					Header: &CardHeader{
						Title:    fmt.Sprintf("Approval %s", label),
						Subtitle: fmt.Sprintf("Request %s · Answered by %s", requestID, clickerEmail),
					},
					Sections: []CardSection{
						{
							Widgets: []Widget{
								{
									TextParagraph: &TextParagraph{
										Text: cmp.Or(toolName, summary),
									},
								},
								{
									DecoratedText: &DecoratedText{
										TopLabel: "Decision",
										Text:     fmt.Sprintf("%s by %s", label, clickerEmail),
									},
								},
								{
									ButtonList: &ButtonList{
										Buttons: []Button{
											{
												Text: "Open in Grain Hub",
												OnClick: &OnClick{
													OpenLink: &OpenLink{URL: hubLink},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func (h *Handler) buildProposedTaskCard(chat tasks.ChatSession, pt tasks.Task) OutboundMessage {
	chatURL := h.hubBaseURL() + "/chats/" + chat.ID
	params := func(decision string) []ActionParameter {
		return []ActionParameter{
			{Key: "task_id", Value: pt.ID},
			{Key: "decision", Value: decision},
			{Key: "chat_id", Value: chat.ID},
		}
	}
	return OutboundMessage{
		Text: fmt.Sprintf("[@%s] Proposed task %s (@%s): %s", chat.Deployment, pt.ID, pt.Team, cmp.Or(pt.Title, pt.Prompt)),
		CardsV2: []CardWithID{
			{
				CardID: "grain-proposal-" + pt.ID,
				Card: Card{
					Header: &CardHeader{
						Title:    fmt.Sprintf("Task Proposed: %s", cmp.Or(pt.Title, pt.ID)),
						Subtitle: fmt.Sprintf("%s · Target @%s", pt.ID, pt.Team),
					},
					Sections: []CardSection{
						{
							Widgets: []Widget{
								{
									TextParagraph: &TextParagraph{
										Text: cmp.Or(pt.Prompt, pt.Title),
									},
								},
								{
									ButtonList: &ButtonList{
										Buttons: []Button{
											{
												Text: "Approve",
												OnClick: &OnClick{
													Action: &Action{
														Function:   "review_proposal",
														Parameters: params("approve"),
													},
												},
											},
											{
												Text: "Reject",
												OnClick: &OnClick{
													Action: &Action{
														Function:   "review_proposal",
														Parameters: params("reject"),
													},
												},
											},
											{
												Text: "Open in Grain Hub",
												OnClick: &OnClick{
													OpenLink: &OpenLink{URL: chatURL},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func (h *Handler) buildProposalResolvedCard(chatID string, pt tasks.Task, decision, clickerEmail string) OutboundMessage {
	hubLink := h.hubBaseURL() + "/tasks/" + pt.ID
	if chatID != "" {
		hubLink = h.hubBaseURL() + "/chats/" + chatID
	}
	label := "Approved"
	if decision == "reject" {
		label = "Rejected"
	}
	summary := fmt.Sprintf("Task %s %s by %s", pt.ID, strings.ToLower(label), clickerEmail)
	return OutboundMessage{
		Text:           summary,
		ActionResponse: &ActionResponse{Type: "UPDATE_MESSAGE"},
		CardsV2: []CardWithID{
			{
				CardID: "grain-proposal-" + pt.ID,
				Card: Card{
					Header: &CardHeader{
						Title:    fmt.Sprintf("Task %s: %s", label, cmp.Or(pt.Title, pt.ID)),
						Subtitle: fmt.Sprintf("%s · %s by %s", pt.ID, label, clickerEmail),
					},
					Sections: []CardSection{
						{
							Widgets: []Widget{
								{
									DecoratedText: &DecoratedText{
										TopLabel: "Status",
										Text:     fmt.Sprintf("%s (%s by %s)", pt.State, label, clickerEmail),
									},
								},
								{
									ButtonList: &ButtonList{
										Buttons: []Button{
											{
												Text: "Open in Grain Hub",
												OnClick: &OnClick{
													OpenLink: &OpenLink{URL: hubLink},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func parseGrainCommand(ev Event) (subcmd string, args []string, isCmd bool) {
	if ev.Message == nil {
		return "", nil, false
	}
	text := strings.TrimSpace(ev.Message.Text)
	if cleaned, stripped := stripLeadingBotMention(text); stripped {
		text = strings.TrimSpace(cleaned)
	}
	lower := strings.ToLower(text)
	if lower == "/grain" || strings.HasPrefix(lower, "/grain ") || strings.HasPrefix(lower, "/grain\t") {
		rest := strings.TrimSpace(text[len("/grain"):])
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return "status", nil, true
		}
		return fields[0], fields[1:], true
	}
	hasSlashMeta := ev.Message.SlashCommand != nil
	if !hasSlashMeta {
		for _, a := range ev.Message.Annotations {
			if strings.EqualFold(a.Type, "SLASH_COMMAND") {
				hasSlashMeta = true
				break
			}
		}
	}
	if hasSlashMeta {
		argText := strings.TrimSpace(cmp.Or(ev.Message.ArgumentText, text))
		if strings.HasPrefix(strings.ToLower(argText), "/grain") {
			argText = strings.TrimSpace(argText[len("/grain"):])
		}
		fields := strings.Fields(argText)
		if len(fields) == 0 {
			return "status", nil, true
		}
		return fields[0], fields[1:], true
	}
	return "", nil, false
}

func stripLeadingBotMention(text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	for _, prefix := range []string{"@grain", "@grain-bot", "@grainhub"} {
		if strings.HasPrefix(strings.ToLower(trimmed), prefix) {
			rest := trimmed[len(prefix):]
			if rest == "" {
				return "", true
			}
			first := rest[0]
			if first == ' ' || first == '\t' || first == '\n' || first == ':' || first == ',' {
				rest = strings.TrimLeft(rest, " \t\n:,")
				return rest, true
			}
		}
	}
	return trimmed, false
}

func hasBotMentionAnnotation(m *Message) bool {
	if m == nil {
		return false
	}
	for _, a := range m.Annotations {
		if !strings.EqualFold(a.Type, "USER_MENTION") {
			continue
		}
		if a.UserMention == nil {
			return true
		}
		u := a.UserMention.User
		if strings.EqualFold(u.Type, "BOT") || strings.EqualFold(u.DisplayName, "Grain") || u.Email == "" {
			return true
		}
	}
	return false
}

func extractAttachments(m *Message) []events.Attachment {
	if m == nil {
		return nil
	}
	raw := append([]Attachment{}, m.Attachment...)
	raw = append(raw, m.Attachments...)
	if len(raw) == 0 {
		return nil
	}
	out := make([]events.Attachment, 0, len(raw))
	for _, a := range raw {
		name := strings.TrimSpace(cmp.Or(a.ContentName, a.Name, "attachment"))
		ref := strings.TrimSpace(cmp.Or(a.Ref, a.DownloadURI, a.Name))
		if ref == "" {
			continue
		}
		out = append(out, events.Attachment{
			Name:      name,
			Ref:       ref,
			MediaType: strings.TrimSpace(cmp.Or(a.MediaType, a.ContentType)),
			Size:      a.Size,
		})
	}
	return out
}

func extractCardAction(ev Event) (string, map[string]string) {
	params := map[string]string{}
	fn := ""
	if ev.Common != nil {
		fn = strings.TrimSpace(ev.Common.InvokedFunction)
		for k, v := range ev.Common.Parameters {
			params[k] = v
		}
	}
	if ev.Action != nil {
		if fn == "" {
			fn = strings.TrimSpace(cmp.Or(ev.Action.ActionMethodName, ev.Action.Function))
		}
		for _, p := range ev.Action.Parameters {
			if p.Key != "" {
				params[p.Key] = p.Value
			}
		}
	}
	return fn, params
}

func writeCardResponse(w http.ResponseWriter, code int, msg OutboundMessage) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(msg)
}
