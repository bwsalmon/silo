// Package chats provides shared multi-user and multi-agent chat session models,
// mention parsing, transcript attribution, and store bindings for the Grain Hub.
package chats

import "github.com/bwsalmon/silo/hub/tasks"

const (
	ChatsPath           = tasks.ChatsPath
	InboxChatMention    = tasks.InboxChatMention
	ReplyModeMention    = tasks.ReplyModeMention
	ReplyModeAll        = tasks.ReplyModeAll
	ChatRoleParticipant = tasks.ChatRoleParticipant
	ChatRoleViewer      = tasks.ChatRoleViewer
)

type (
	Store                  = tasks.Store
	ChatParticipant        = tasks.ChatParticipant
	ChatExternalBinding    = tasks.ChatExternalBinding
	ChatMessage            = tasks.ChatMessage
	ChatSession            = tasks.ChatSession
	ChatEvent              = tasks.ChatEvent
	CreateChatOptions      = tasks.CreateChatOptions
	UpdateChatOptions      = tasks.UpdateChatOptions
	PostChatMessageOptions = tasks.PostChatMessageOptions
)

var (
	ErrNoChat                 = tasks.ErrNoChat
	Open                      = tasks.Open
	OpenExisting              = tasks.OpenExisting
	MentionsAgent             = tasks.MentionsAgent
	MentionsUser              = tasks.MentionsUser
	FormatChatContextHeader   = tasks.FormatChatContextHeader
	FormatChatTranscriptSlice = tasks.FormatChatTranscriptSlice
)
