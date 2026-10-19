package tasks

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bwsalmon/silo/events"
	"github.com/bwsalmon/silo/hub/dbaccess"
)

// ChatsPath is the HTTP API path for shared multi-user and multi-agent chat sessions.
const ChatsPath = "/v1/chats"

// InboxChatMention is the Unified Inbox item kind for a shared chat @mention.
const InboxChatMention = "chat_mention"

// Reply modes for a shared chat session.
const (
	// ReplyModeMention wakes the backing agent only when a message mentions
	// @agent or @<deployment-id>, or sets trigger_agent: true.
	ReplyModeMention = "mention"
	// ReplyModeAll wakes the backing agent on every message posted.
	ReplyModeAll = "all"
)

// Participant roles in a shared chat session.
const (
	ChatRoleParticipant = "participant"
	ChatRoleViewer      = "viewer"
)

// ErrNoChat is a chat session that does not exist.
var ErrNoChat = errors.New("tasks: no such chat session")

var chatIDPattern = regexp.MustCompile(`^c-[0-9a-f]{16}$`)

func newChatID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "c-" + hex.EncodeToString(b)
}

// ChatParticipant is one human participant in a shared chat session.
// Members of the backing deployment are automatically included with Source "deployment";
// explicitly invited accepted Hub users have Source "invited" and Role "participant" or "viewer".
type ChatParticipant struct {
	Email   string    `json:"email"`
	Role    string    `json:"role"`
	Source  string    `json:"source,omitempty"`
	AddedBy string    `json:"added_by,omitempty"`
	AddedAt time.Time `json:"added_at,omitzero"`
}

// ChatExternalBinding links a shared chat session to an external chat space/thread
// (e.g. Google Chat spaces/AAA...).
type ChatExternalBinding struct {
	Provider   string            `json:"provider"`
	ExternalID string            `json:"external_id"`
	ThreadID   string            `json:"thread_id,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// ChatMessage is one message in a shared chat session transcript.
type ChatMessage struct {
	Seq            int64               `json:"seq"`
	ID             string              `json:"id"`
	ChatID         string              `json:"chat_id,omitempty"`
	Sender         string              `json:"sender"`
	Role           string              `json:"role"`
	Text           string              `json:"text"`
	Attachments    []events.Attachment `json:"attachments,omitempty"`
	TriggeredAgent bool                `json:"triggered_agent,omitempty"`
	CreatedAt      time.Time           `json:"created_at"`
}

// ChatSession is a shared multi-user and multi-agent chat room in the Hub.
type ChatSession struct {
	ID                string                   `json:"id"`
	TaskID            string                   `json:"task_id"`
	Title             string                   `json:"title"`
	Deployment        string                   `json:"deployment"`
	LinkedDeployments []string                 `json:"linked_deployments"`
	ReplyMode         string                   `json:"reply_mode"`
	Participants      []ChatParticipant        `json:"participants"`
	Repos             []string                 `json:"repos"`
	References        []string                 `json:"references"`
	ExternalBindings  []ChatExternalBinding    `json:"external_bindings"`
	State             State                    `json:"state,omitempty"`
	Node              string                   `json:"node,omitempty"`
	AwaitingReply     bool                     `json:"awaiting_reply"`
	Question          string                   `json:"question,omitempty"`
	PendingApprovals  []events.ApprovalRequest `json:"pending_approvals,omitempty"`
	ProposedTasks     []Task                   `json:"proposed_tasks,omitempty"`
	Messages          []ChatMessage            `json:"messages,omitempty"`
	CreatedBy         string                   `json:"created_by"`
	CreatedAt         time.Time                `json:"created_at"`
	UpdatedAt         time.Time                `json:"updated_at"`
	LastAgentSeq      int64                    `json:"last_agent_seq,omitempty"`
}

// ChatEvent is a live event streamed over GET /v1/chats/{id}/events.
type ChatEvent struct {
	Seq             int64                   `json:"seq"`
	Type            string                  `json:"type"`
	Time            time.Time               `json:"time"`
	ChatID          string                  `json:"chat_id"`
	TaskID          string                  `json:"task_id,omitempty"`
	Message         *ChatMessage            `json:"message,omitempty"`
	Transcript      *events.Item            `json:"transcript,omitempty"`
	Status          *events.Status          `json:"status,omitempty"`
	AwaitingReply   *events.AwaitingReply   `json:"awaiting_reply,omitempty"`
	ApprovalRequest *events.ApprovalRequest `json:"approval_request,omitempty"`
	Result          *events.Result          `json:"result,omitempty"`
	ProposedTask    *Task                   `json:"proposed_task,omitempty"`
	Chat            *ChatSession            `json:"chat,omitempty"`
}

// CreateChatOptions are the parameters for creating a shared chat session.
type CreateChatOptions struct {
	Title             string                `json:"title"`
	Deployment        string                `json:"deployment"`
	LinkedDeployments []string              `json:"linked_deployments"`
	ReplyMode         string                `json:"reply_mode"`
	Participants      []ChatParticipant     `json:"participants"`
	Repos             []string              `json:"repos"`
	References        []string              `json:"references"`
	Prompt            string                `json:"prompt"`
	ExternalBindings  []ChatExternalBinding `json:"external_bindings"`
	CreatedBy         string                `json:"-"`
}

// UpdateChatOptions are the optional fields for PATCH /v1/chats/{id}.
type UpdateChatOptions struct {
	Title             *string                `json:"title"`
	ReplyMode         *string                `json:"reply_mode"`
	LinkedDeployments *[]string              `json:"linked_deployments"`
	Participants      *[]ChatParticipant     `json:"participants"`
	Repos             *[]string              `json:"repos"`
	References        *[]string              `json:"references"`
	ExternalBindings  *[]ChatExternalBinding `json:"external_bindings"`
	UpdatedBy         string                 `json:"-"`
}

// PostChatMessageOptions are the parameters for POST /v1/chats/{id}/messages.
type PostChatMessageOptions struct {
	ID           string              `json:"id"`
	Text         string              `json:"text"`
	Attachments  []events.Attachment `json:"attachments"`
	TriggerAgent bool                `json:"trigger_agent"`
	Sender       string              `json:"-"`
}

func normalizeReplyMode(mode string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(mode))
	if m == "" {
		return ReplyModeMention, nil
	}
	if m != ReplyModeMention && m != ReplyModeAll {
		return "", fmt.Errorf("%w reply_mode %q: want %q or %q", ErrInvalid, mode, ReplyModeMention, ReplyModeAll)
	}
	return m, nil
}

func normalizeLinkedDeployments(primary string, linked []string) ([]string, error) {
	out := []string{}
	for _, raw := range linked {
		d := strings.ToLower(strings.TrimSpace(raw))
		if d == "" {
			continue
		}
		if !teamID.MatchString(d) {
			return nil, fmt.Errorf("%w linked deployment %q", ErrInvalid, raw)
		}
		if d == primary || slices.Contains(out, d) {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

func normalizeChatRepos(repos []string) ([]string, error) {
	out := []string{}
	for _, raw := range repos {
		r := strings.ToLower(strings.TrimSpace(raw))
		if r == "" {
			continue
		}
		if !repoName.MatchString(r) {
			return nil, fmt.Errorf("%w repository %q: want owner/name", ErrInvalid, raw)
		}
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out, nil
}

func normalizeChatReferences(refs []string) ([]string, error) {
	out := []string{}
	for _, raw := range refs {
		r := strings.TrimSpace(raw)
		if r == "" {
			continue
		}
		if len(r) > maxAttachRef || strings.ContainsFunc(r, isControl) {
			return nil, fmt.Errorf("%w reference %q", ErrInvalid, raw)
		}
		if strings.Contains(r, "://") {
			u, err := url.Parse(r)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return nil, fmt.Errorf("%w reference %q: want http(s) URL", ErrInvalid, raw)
			}
		}
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out, nil
}

func normalizeExternalBindings(bindings []ChatExternalBinding) ([]ChatExternalBinding, error) {
	out := []ChatExternalBinding{}
	for _, b := range bindings {
		prov := strings.ToLower(strings.TrimSpace(b.Provider))
		extID := strings.TrimSpace(b.ExternalID)
		threadID := strings.TrimSpace(b.ThreadID)
		if prov == "" || extID == "" {
			return nil, fmt.Errorf("%w external binding: provider and external_id are required", ErrInvalid)
		}
		if strings.ContainsFunc(prov, isControl) || strings.ContainsFunc(extID, isControl) || strings.ContainsFunc(threadID, isControl) {
			return nil, fmt.Errorf("%w external binding", ErrInvalid)
		}
		out = append(out, ChatExternalBinding{
			Provider:   prov,
			ExternalID: extID,
			ThreadID:   threadID,
			Metadata:   b.Metadata,
		})
	}
	return out, nil
}

func normalizeInvitedParticipants(participants []ChatParticipant, addedBy string, now time.Time) ([]ChatParticipant, error) {
	out := []ChatParticipant{}
	seen := map[string]int{}
	for _, p := range participants {
		email := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(p.Email), "user:")))
		if email == "" {
			continue
		}
		if !strings.Contains(email, "@") || strings.ContainsFunc(email, isControl) {
			return nil, fmt.Errorf("%w participant email %q", ErrInvalid, p.Email)
		}
		role := strings.ToLower(strings.TrimSpace(p.Role))
		if role == "" {
			role = ChatRoleParticipant
		}
		if role != ChatRoleParticipant && role != ChatRoleViewer {
			return nil, fmt.Errorf("%w participant role %q for %s: want %q or %q", ErrInvalid, p.Role, email, ChatRoleParticipant, ChatRoleViewer)
		}
		item := ChatParticipant{
			Email:   email,
			Role:    role,
			Source:  "invited",
			AddedBy: strings.ToLower(strings.TrimSpace(addedBy)),
			AddedAt: now,
		}
		if idx, exists := seen[email]; exists {
			out[idx] = item
			continue
		}
		seen[email] = len(out)
		out = append(out, item)
	}
	return out, nil
}

// MentionsAgent reports whether text mentions @agent, @<deployment>, or any @<linked_deployment>
// as a standalone handle (not embedded inside an email address), returning the matched targets.
func MentionsAgent(text string, deployment string, linkedDeployments []string) (bool, []string) {
	targets := []string{"agent"}
	if d := strings.ToLower(strings.TrimSpace(deployment)); d != "" {
		targets = append(targets, d)
	}
	for _, ld := range linkedDeployments {
		if d := strings.ToLower(strings.TrimSpace(ld)); d != "" && !slices.Contains(targets, d) {
			targets = append(targets, d)
		}
	}
	lower := strings.ToLower(text)
	var matched []string
	for _, target := range targets {
		token := "@" + target
		idx := 0
		for {
			pos := strings.Index(lower[idx:], token)
			if pos < 0 {
				break
			}
			abs := idx + pos
			after := abs + len(token)
			idx = after
			if abs > 0 {
				prev, _ := utf8.DecodeLastRuneInString(lower[:abs])
				if unicode.IsLetter(prev) || unicode.IsDigit(prev) || prev == '.' || prev == '_' || prev == '-' || prev == '+' {
					continue
				}
			}
			if after < len(lower) {
				next, _ := utf8.DecodeRuneInString(lower[after:])
				if unicode.IsLetter(next) || unicode.IsDigit(next) || next == '_' || next == '-' {
					continue
				}
				if next == '.' && after+1 < len(lower) {
					next2, _ := utf8.DecodeRuneInString(lower[after+1:])
					if unicode.IsLetter(next2) || unicode.IsDigit(next2) {
						continue
					}
				}
			}
			if !slices.Contains(matched, target) {
				matched = append(matched, target)
			}
			break
		}
	}
	return len(matched) > 0, matched
}

// MentionsUser reports whether text mentions @<email> or @<localpart> for email.
func MentionsUser(text string, email string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return false
	}
	lower := strings.ToLower(text)
	local, _, _ := strings.Cut(email, "@")
	candidates := []string{"@" + email}
	if local != "" {
		candidates = append(candidates, "@"+local)
	}
	for _, token := range candidates {
		idx := 0
		for {
			pos := strings.Index(lower[idx:], token)
			if pos < 0 {
				break
			}
			abs := idx + pos
			after := abs + len(token)
			idx = after
			if abs > 0 {
				prev, _ := utf8.DecodeLastRuneInString(lower[:abs])
				if unicode.IsLetter(prev) || unicode.IsDigit(prev) || prev == '.' || prev == '_' || prev == '-' || prev == '+' {
					continue
				}
			}
			if after < len(lower) {
				next, _ := utf8.DecodeRuneInString(lower[after:])
				if unicode.IsLetter(next) || unicode.IsDigit(next) || next == '_' || next == '-' || next == '@' {
					continue
				}
			}
			return true
		}
	}
	return false
}

// FormatChatContextHeader formats the room context header for the backing interactive session.
func FormatChatContextHeader(title, deployment string, linkedDeployments, repos, refs []string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("[Shared Chat Session: %s (deployment: %s", title, deployment))
	if len(linkedDeployments) > 0 {
		b.WriteString(", linked_deployments: " + strings.Join(linkedDeployments, ", "))
	}
	b.WriteString(")]")
	if len(repos) > 0 {
		b.WriteString("\n[Attached Repositories: " + strings.Join(repos, ", ") + "]")
	}
	if len(refs) > 0 {
		b.WriteString("\n[Attached References: " + strings.Join(refs, ", ") + "]")
	}
	return b.String()
}

// FormatChatTranscriptSlice formats a slice of human ChatMessages with [sender]: text attribution.
func FormatChatTranscriptSlice(chat ChatSession, msgs []ChatMessage, mentionedTargets []string) string {
	var lines []string
	if len(chat.Repos) > 0 || len(chat.References) > 0 {
		var ctxParts []string
		if len(chat.Repos) > 0 {
			ctxParts = append(ctxParts, "repos="+strings.Join(chat.Repos, ","))
		}
		if len(chat.References) > 0 {
			ctxParts = append(ctxParts, "references="+strings.Join(chat.References, ","))
		}
		lines = append(lines, "[Chat Context: "+strings.Join(ctxParts, " ")+"]")
	}
	for _, target := range mentionedTargets {
		if target != "agent" && target != chat.Deployment {
			lines = append(lines, fmt.Sprintf("[Target Linked Deployment: @%s]", target))
		}
	}
	for _, m := range msgs {
		sender := m.Sender
		if sender == "" {
			sender = "user"
		}
		lines = append(lines, fmt.Sprintf("[%s]: %s", sender, m.Text))
	}
	return strings.Join(lines, "\n")
}

// CreateChat creates a new shared chat session and its backing interactive task.
func (s *Store) CreateChat(ctx context.Context, opt CreateChatOptions) (ChatSession, *TaskMessage, error) {
	deployment := strings.ToLower(strings.TrimSpace(opt.Deployment))
	if !teamID.MatchString(deployment) {
		return ChatSession{}, nil, fmt.Errorf("%w deployment %q", ErrInvalid, opt.Deployment)
	}
	creator := strings.ToLower(strings.TrimSpace(opt.CreatedBy))
	if creator == "" {
		return ChatSession{}, nil, fmt.Errorf("%w: no creator", ErrInvalid)
	}
	mode, err := normalizeReplyMode(opt.ReplyMode)
	if err != nil {
		return ChatSession{}, nil, err
	}
	linked, err := normalizeLinkedDeployments(deployment, opt.LinkedDeployments)
	if err != nil {
		return ChatSession{}, nil, err
	}
	repos, err := normalizeChatRepos(opt.Repos)
	if err != nil {
		return ChatSession{}, nil, err
	}
	refs, err := normalizeChatReferences(opt.References)
	if err != nil {
		return ChatSession{}, nil, err
	}
	bindings, err := normalizeExternalBindings(opt.ExternalBindings)
	if err != nil {
		return ChatSession{}, nil, err
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	invited, err := normalizeInvitedParticipants(opt.Participants, creator, now)
	if err != nil {
		return ChatSession{}, nil, err
	}

	title := strings.TrimSpace(opt.Title)
	prompt := strings.TrimSpace(opt.Prompt)
	if title == "" && prompt != "" {
		firstLine, _, _ := strings.Cut(prompt, "\n")
		title = truncate(strings.TrimSpace(firstLine), maxTitle)
	}
	if title == "" {
		title = "Shared Chat (" + deployment + ")"
	}
	if utf8.RuneCountInString(title) > maxTitle {
		return ChatSession{}, nil, fmt.Errorf("%w: the title is over %d characters", ErrInvalid, maxTitle)
	}

	var primaryRepo string
	if len(repos) > 0 {
		primaryRepo = repos[0]
	}

	header := FormatChatContextHeader(title, deployment, linked, repos, refs)
	taskPrompt := header
	if prompt != "" {
		taskPrompt = header + "\n[" + creator + "]: " + prompt
	}

	backingTask, err := s.Create(ctx, Spec{
		Team:      deployment,
		Repo:      primaryRepo,
		Kind:      KindInteractive,
		Title:     title,
		Prompt:    taskPrompt,
		Submitter: creator,
	})
	if err != nil {
		return ChatSession{}, nil, err
	}

	chatID := newChatID()
	linkedJSON, _ := json.Marshal(linked)
	reposJSON, _ := json.Marshal(repos)
	refsJSON, _ := json.Marshal(refs)
	bindingsJSON, _ := json.Marshal(bindings)

	var (
		lastAgentSeq int64
		initialMsg   *ChatMessage
		taskMsg      *TaskMessage
	)
	if prompt != "" {
		mentioned, targets := MentionsAgent(prompt, deployment, linked)
		triggered := mode == ReplyModeAll || mentioned
		if triggered {
			lastAgentSeq = 1
		}
		m := ChatMessage{
			Seq:            1,
			ID:             newMessageID(),
			ChatID:         chatID,
			Sender:         creator,
			Role:           "user",
			Text:           prompt,
			Attachments:    []events.Attachment{},
			TriggeredAgent: triggered,
			CreatedAt:      now,
		}
		initialMsg = &m
		if triggered {
			formatted := FormatChatTranscriptSlice(ChatSession{
				Deployment:        deployment,
				LinkedDeployments: linked,
				Repos:             repos,
				References:        refs,
			}, []ChatMessage{m}, targets)
			posted, _, perr := s.Post(ctx, TaskMessage{
				Task:   backingTask.ID,
				ID:     m.ID,
				Text:   formatted,
				Sender: creator,
			})
			if perr == nil {
				taskMsg = &posted
			}
		}
	}

	err = dbaccess.Retry(ctx, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO chat_sessions (
			id, task_id, title, deployment, linked_deployments, reply_mode,
			repos, refs, external_bindings, created_by, created_at, updated_at, last_agent_seq
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			chatID, backingTask.ID, title, deployment, string(linkedJSON), mode,
			string(reposJSON), string(refsJSON), string(bindingsJSON), creator,
			now.UnixMilli(), now.UnixMilli(), lastAgentSeq,
		); err != nil {
			return err
		}

		for _, p := range invited {
			if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO chat_participants (
				chat, email, role, added_by, added_at
			) VALUES (?, ?, ?, ?, ?) ON CONFLICT (chat, email) DO UPDATE SET role = excluded.role`),
				chatID, p.Email, p.Role, p.AddedBy, p.AddedAt.UnixMilli(),
			); err != nil {
				return err
			}
		}

		if initialMsg != nil {
			attJSON, _ := json.Marshal(initialMsg.Attachments)
			if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO chat_messages (
				chat, seq, id, sender, role, text, attachments, triggered_agent, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
				chatID, initialMsg.Seq, initialMsg.ID, initialMsg.Sender, initialMsg.Role,
				initialMsg.Text, string(attJSON), boolInt(initialMsg.TriggeredAgent), initialMsg.CreatedAt.UnixMilli(),
			); err != nil {
				return err
			}
		}

		return tx.Commit()
	})
	if err != nil {
		return ChatSession{}, nil, err
	}

	chat, err := s.GetChat(ctx, chatID)
	return chat, taskMsg, err
}

const chatColumns = `id, task_id, title, deployment, linked_deployments, reply_mode, repos, refs, external_bindings, created_by, created_at, updated_at, last_agent_seq`

func scanChatSession(r scanner) (ChatSession, error) {
	var (
		c                            ChatSession
		linked, repos, refs, binding string
		created, updated             int64
	)
	if err := r.Scan(
		&c.ID, &c.TaskID, &c.Title, &c.Deployment, &linked, &c.ReplyMode,
		&repos, &refs, &binding, &c.CreatedBy, &created, &updated, &c.LastAgentSeq,
	); err != nil {
		return ChatSession{}, err
	}
	c.CreatedAt = time.UnixMilli(created).UTC()
	c.UpdatedAt = time.UnixMilli(updated).UTC()
	_ = json.Unmarshal([]byte(linked), &c.LinkedDeployments)
	_ = json.Unmarshal([]byte(repos), &c.Repos)
	_ = json.Unmarshal([]byte(refs), &c.References)
	_ = json.Unmarshal([]byte(binding), &c.ExternalBindings)
	if c.LinkedDeployments == nil {
		c.LinkedDeployments = []string{}
	}
	if c.Repos == nil {
		c.Repos = []string{}
	}
	if c.References == nil {
		c.References = []string{}
	}
	if c.ExternalBindings == nil {
		c.ExternalBindings = []ChatExternalBinding{}
	}
	c.Participants = []ChatParticipant{}
	return c, nil
}

// GetChat loads a shared chat session by ID, including its invited participants,
// messages, backing task state, and proposed tasks.
func (s *Store) GetChat(ctx context.Context, id string) (ChatSession, error) {
	var c ChatSession
	err := dbaccess.Retry(ctx, func() error {
		var err error
		c, err = scanChatSession(s.db.QueryRowContext(ctx, s.q(`SELECT `+chatColumns+` FROM chat_sessions WHERE id = ?`), id))
		return err
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ChatSession{}, ErrNoChat
		}
		return ChatSession{}, err
	}
	invited, err := s.chatInvitedParticipants(ctx, c.ID)
	if err != nil {
		return ChatSession{}, err
	}
	c.Participants = invited
	msgs, err := s.ChatMessages(ctx, c.ID, 0)
	if err != nil {
		return ChatSession{}, err
	}
	c.Messages = msgs
	if t, err := s.Get(ctx, c.TaskID); err == nil {
		c.State = t.State
		c.Node = t.Node
	}
	proposed, err := s.ProposedTasksForChat(ctx, c.TaskID)
	if err == nil {
		c.ProposedTasks = proposed
	}
	return c, nil
}

// GetChatByTask finds the shared chat session backed by taskID, if one exists.
func (s *Store) GetChatByTask(ctx context.Context, taskID string) (ChatSession, bool, error) {
	var c ChatSession
	err := dbaccess.Retry(ctx, func() error {
		var err error
		c, err = scanChatSession(s.db.QueryRowContext(ctx, s.q(`SELECT `+chatColumns+` FROM chat_sessions WHERE task_id = ?`), taskID))
		return err
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ChatSession{}, false, nil
		}
		return ChatSession{}, false, err
	}
	invited, err := s.chatInvitedParticipants(ctx, c.ID)
	if err != nil {
		return ChatSession{}, false, err
	}
	c.Participants = invited
	return c, true, nil
}

// ListChats returns all shared chat sessions, newest updated first, with their
// invited participants and backing task state populated.
func (s *Store) ListChats(ctx context.Context) ([]ChatSession, error) {
	var chats []ChatSession
	err := dbaccess.Retry(ctx, func() error {
		chats = []ChatSession{}
		rows, err := s.db.QueryContext(ctx, `SELECT `+chatColumns+` FROM chat_sessions ORDER BY updated_at DESC, id DESC`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanChatSession(rows)
			if err != nil {
				return err
			}
			chats = append(chats, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	for i := range chats {
		invited, err := s.chatInvitedParticipants(ctx, chats[i].ID)
		if err != nil {
			return nil, err
		}
		chats[i].Participants = invited
		if t, err := s.Get(ctx, chats[i].TaskID); err == nil {
			chats[i].State = t.State
			chats[i].Node = t.Node
		}
	}
	return chats, nil
}

func (s *Store) chatInvitedParticipants(ctx context.Context, chatID string) ([]ChatParticipant, error) {
	var out []ChatParticipant
	err := dbaccess.Retry(ctx, func() error {
		out = []ChatParticipant{}
		rows, err := s.db.QueryContext(ctx, s.q(`SELECT email, role, added_by, added_at FROM chat_participants WHERE chat = ? ORDER BY email`), chatID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				p       ChatParticipant
				addedAt int64
			)
			if err := rows.Scan(&p.Email, &p.Role, &p.AddedBy, &addedAt); err != nil {
				return err
			}
			p.Source = "invited"
			if addedAt > 0 {
				p.AddedAt = time.UnixMilli(addedAt).UTC()
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// UpdateChat updates a shared chat session's mutable metadata and participants.
func (s *Store) UpdateChat(ctx context.Context, id string, opt UpdateChatOptions) (ChatSession, error) {
	cur, err := s.GetChat(ctx, id)
	if err != nil {
		return ChatSession{}, err
	}

	title := cur.Title
	if opt.Title != nil {
		title = strings.TrimSpace(*opt.Title)
		if title == "" {
			return ChatSession{}, fmt.Errorf("%w: empty title", ErrInvalid)
		}
		if utf8.RuneCountInString(title) > maxTitle {
			return ChatSession{}, fmt.Errorf("%w: the title is over %d characters", ErrInvalid, maxTitle)
		}
	}

	mode := cur.ReplyMode
	if opt.ReplyMode != nil {
		mode, err = normalizeReplyMode(*opt.ReplyMode)
		if err != nil {
			return ChatSession{}, err
		}
	}

	linked := cur.LinkedDeployments
	if opt.LinkedDeployments != nil {
		linked, err = normalizeLinkedDeployments(cur.Deployment, *opt.LinkedDeployments)
		if err != nil {
			return ChatSession{}, err
		}
	}

	repos := cur.Repos
	if opt.Repos != nil {
		repos, err = normalizeChatRepos(*opt.Repos)
		if err != nil {
			return ChatSession{}, err
		}
	}

	refs := cur.References
	if opt.References != nil {
		refs, err = normalizeChatReferences(*opt.References)
		if err != nil {
			return ChatSession{}, err
		}
	}

	bindings := cur.ExternalBindings
	if opt.ExternalBindings != nil {
		bindings, err = normalizeExternalBindings(*opt.ExternalBindings)
		if err != nil {
			return ChatSession{}, err
		}
	}

	now := s.now().UTC().Truncate(time.Millisecond)
	var invited []ChatParticipant
	if opt.Participants != nil {
		invited, err = normalizeInvitedParticipants(*opt.Participants, opt.UpdatedBy, now)
		if err != nil {
			return ChatSession{}, err
		}
	}

	linkedJSON, _ := json.Marshal(linked)
	reposJSON, _ := json.Marshal(repos)
	refsJSON, _ := json.Marshal(refs)
	bindingsJSON, _ := json.Marshal(bindings)

	err = dbaccess.Retry(ctx, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		if _, err := tx.ExecContext(ctx, s.q(`UPDATE chat_sessions
			SET title = ?, reply_mode = ?, linked_deployments = ?, repos = ?, refs = ?, external_bindings = ?, updated_at = ?
			WHERE id = ?`),
			title, mode, string(linkedJSON), string(reposJSON), string(refsJSON), string(bindingsJSON), now.UnixMilli(), id,
		); err != nil {
			return err
		}

		if opt.Participants != nil {
			if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM chat_participants WHERE chat = ?`), id); err != nil {
				return err
			}
			for _, p := range invited {
				if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO chat_participants (
					chat, email, role, added_by, added_at
				) VALUES (?, ?, ?, ?, ?)`),
					id, p.Email, p.Role, p.AddedBy, p.AddedAt.UnixMilli(),
				); err != nil {
					return err
				}
			}
		}

		return tx.Commit()
	})
	if err != nil {
		return ChatSession{}, err
	}
	return s.GetChat(ctx, id)
}

const chatMessageColumns = `chat, seq, id, sender, role, text, attachments, triggered_agent, created_at`

func scanChatMessage(r scanner) (ChatMessage, error) {
	var (
		m         ChatMessage
		att       string
		triggered int
		createdAt int64
	)
	if err := r.Scan(&m.ChatID, &m.Seq, &m.ID, &m.Sender, &m.Role, &m.Text, &att, &triggered, &createdAt); err != nil {
		return ChatMessage{}, err
	}
	m.TriggeredAgent = triggered != 0
	m.CreatedAt = time.UnixMilli(createdAt).UTC()
	if err := json.Unmarshal([]byte(att), &m.Attachments); err != nil {
		m.Attachments = []events.Attachment{}
	}
	return m, nil
}

// ChatMessages returns the messages in chatID with seq > afterSeq, ordered by seq.
func (s *Store) ChatMessages(ctx context.Context, chatID string, afterSeq int64) ([]ChatMessage, error) {
	var out []ChatMessage
	err := dbaccess.Retry(ctx, func() error {
		out = []ChatMessage{}
		rows, err := s.db.QueryContext(ctx, s.q(`SELECT `+chatMessageColumns+` FROM chat_messages WHERE chat = ? AND seq > ? ORDER BY seq ASC`), chatID, afterSeq)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanChatMessage(rows)
			if err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// PostChatMessage records a human's message in a shared chat session and, when
// reply_mode == "all", opt.TriggerAgent is true, or the message @mentions the agent
// or a deployment, batches all pending human messages since the agent's last turn
// (with [sender]: text attribution) and posts them to the backing interactive task.
func (s *Store) PostChatMessage(ctx context.Context, id string, opt PostChatMessageOptions) (_ ChatMessage, _ *TaskMessage, dup bool, err error) {
	chat, err := s.GetChat(ctx, id)
	if err != nil {
		return ChatMessage{}, nil, false, err
	}
	backingTask, err := s.Get(ctx, chat.TaskID)
	if err != nil {
		return ChatMessage{}, nil, false, err
	}
	if backingTask.State.Terminal() {
		return ChatMessage{}, nil, false, fmt.Errorf("%w: backing session %s is %s", ErrState, backingTask.ID, backingTask.State)
	}

	sender := strings.ToLower(strings.TrimSpace(opt.Sender))
	if sender == "" {
		return ChatMessage{}, nil, false, fmt.Errorf("%w: no sender", ErrInvalid)
	}
	msgID := strings.TrimSpace(opt.ID)
	if msgID == "" {
		msgID = newMessageID()
	}
	text := opt.Text
	if strings.TrimSpace(text) == "" && len(opt.Attachments) == 0 && opt.TriggerAgent {
		// Allow "Ask @<deployment>" with empty composer text to summon the agent
		// on the pending human conversation in the room.
		pending, perr := s.ChatMessages(ctx, id, chat.LastAgentSeq)
		if perr != nil {
			return ChatMessage{}, nil, false, perr
		}
		if len(pending) == 0 && len(chat.Messages) > 0 {
			pending = chat.Messages[len(chat.Messages)-1:]
		}
		if len(pending) > 0 {
			last := pending[len(pending)-1]
			last.TriggeredAgent = true
			formatted := FormatChatTranscriptSlice(chat, pending, []string{chat.Deployment})
			var combinedAtt []events.Attachment
			for _, pm := range pending {
				combinedAtt = append(combinedAtt, pm.Attachments...)
			}
			if len(combinedAtt) > maxAttachments {
				combinedAtt = combinedAtt[:maxAttachments]
			}
			tm, _, err := s.Post(ctx, TaskMessage{
				Task:        chat.TaskID,
				ID:          msgID,
				Text:        formatted,
				Attachments: combinedAtt,
				Sender:      sender,
			})
			if err != nil {
				return ChatMessage{}, nil, false, err
			}
			_ = dbaccess.Retry(ctx, func() error {
				_, uerr := s.db.ExecContext(ctx, s.q(`UPDATE chat_sessions SET last_agent_seq = ?, updated_at = ? WHERE id = ?`),
					last.Seq, s.now().UTC().Truncate(time.Millisecond).UnixMilli(), id)
				if uerr != nil {
					return uerr
				}
				_, uerr = s.db.ExecContext(ctx, s.q(`UPDATE chat_messages SET triggered_agent = 1 WHERE chat = ? AND seq = ?`), id, last.Seq)
				return uerr
			})
			return last, &tm, false, nil
		}
	}

	if err := checkMessage(TaskMessage{
		Task:        chat.TaskID,
		ID:          msgID,
		Text:        text,
		Attachments: opt.Attachments,
		Sender:      sender,
	}); err != nil {
		return ChatMessage{}, nil, false, err
	}

	// Check for idempotent retry by message ID.
	for _, existing := range chat.Messages {
		if existing.ID == msgID {
			if existing.Sender != sender || existing.Text != text {
				return ChatMessage{}, nil, false, fmt.Errorf("%w: chat %s already has a different message %s", ErrState, id, msgID)
			}
			return existing, nil, true, nil
		}
	}

	mentioned, targets := MentionsAgent(text, chat.Deployment, chat.LinkedDeployments)
	wakeAgent := chat.ReplyMode == ReplyModeAll || opt.TriggerAgent || mentioned

	now := s.now().UTC().Truncate(time.Millisecond)
	attachments := opt.Attachments
	if attachments == nil {
		attachments = []events.Attachment{}
	}
	attJSON, _ := json.Marshal(attachments)

	var createdMsg ChatMessage
	err = dbaccess.Retry(ctx, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		var maxSeq sql.NullInt64
		if err := tx.QueryRowContext(ctx, s.q(`SELECT MAX(seq) FROM chat_messages WHERE chat = ?`), id).Scan(&maxSeq); err != nil {
			return err
		}
		nextSeq := int64(1)
		if maxSeq.Valid {
			nextSeq = maxSeq.Int64 + 1
		}

		createdMsg = ChatMessage{
			Seq:            nextSeq,
			ID:             msgID,
			ChatID:         id,
			Sender:         sender,
			Role:           "user",
			Text:           text,
			Attachments:    attachments,
			TriggeredAgent: wakeAgent,
			CreatedAt:      now,
		}

		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO chat_messages (
			chat, seq, id, sender, role, text, attachments, triggered_agent, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			id, createdMsg.Seq, createdMsg.ID, createdMsg.Sender, createdMsg.Role,
			createdMsg.Text, string(attJSON), boolInt(createdMsg.TriggeredAgent), now.UnixMilli(),
		); err != nil {
			return err
		}

		if wakeAgent {
			if _, err := tx.ExecContext(ctx, s.q(`UPDATE chat_sessions SET last_agent_seq = ?, updated_at = ? WHERE id = ?`),
				createdMsg.Seq, now.UnixMilli(), id,
			); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, s.q(`UPDATE chat_sessions SET updated_at = ? WHERE id = ?`),
				now.UnixMilli(), id,
			); err != nil {
				return err
			}
		}

		return tx.Commit()
	})
	if err != nil {
		return ChatMessage{}, nil, false, err
	}

	var taskMsg *TaskMessage
	if wakeAgent {
		pending, perr := s.ChatMessages(ctx, id, chat.LastAgentSeq)
		if perr != nil || len(pending) == 0 {
			pending = []ChatMessage{createdMsg}
		}
		var userPending []ChatMessage
		var combinedAtt []events.Attachment
		for _, pm := range pending {
			if pm.Role == "user" {
				userPending = append(userPending, pm)
				combinedAtt = append(combinedAtt, pm.Attachments...)
			}
		}
		if len(userPending) == 0 {
			userPending = []ChatMessage{createdMsg}
			combinedAtt = createdMsg.Attachments
		}
		if len(combinedAtt) > maxAttachments {
			combinedAtt = combinedAtt[:maxAttachments]
		}
		formatted := FormatChatTranscriptSlice(chat, userPending, targets)
		tm, _, err := s.Post(ctx, TaskMessage{
			Task:        chat.TaskID,
			ID:          createdMsg.ID,
			Text:        formatted,
			Attachments: combinedAtt,
			Sender:      sender,
		})
		if err != nil {
			return ChatMessage{}, nil, false, err
		}
		taskMsg = &tm
	}

	return createdMsg, taskMsg, false, nil
}

// ProposedTasksForChat returns all tasks filed from the chat's backing task
// (OriginTask == taskID), oldest first.
func (s *Store) ProposedTasksForChat(ctx context.Context, taskID string) ([]Task, error) {
	if taskID == "" {
		return []Task{}, nil
	}
	var out []Task
	err := dbaccess.Retry(ctx, func() error {
		out = []Task{}
		rows, err := s.db.QueryContext(ctx, s.q(`SELECT `+columns+` FROM tasks WHERE origin_task = ? ORDER BY created_at ASC, id ASC`), taskID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t, err := scanTask(rows)
			if err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}
