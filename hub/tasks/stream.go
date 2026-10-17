package tasks

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/bwsalmon/silo/events"
)

// The task stream (docs/hub.md §6.2) is a stream the hub opens to a node
// (rendezvous.Server.Open) for one task, carrying JSON, one message per
// line. The hub writes first:
//
//	hub → node  {"type":"offer","task":{...},"requester_attestation":"<JWT>"}   run this task
//	            {"type":"resume","task":{...},"requester_attestation":"<JWT>"}  a task you ran when the hub lost you: still yours?
//	            {"type":"attest","task":{"id":...},"requester_attestation":"<JWT>"}  a fresh attestation for a task you accepted
//	            {"type":"cancel","task":{"id":...}}        stop it
//	            {"type":"approve","task":{"id":...},"request_id":"...","decision":"allow"|"deny","answered_by":"<email>","reason":"..."}
//	                                                       a human's answer to the task's approval_request event of that ID
//	            {"type":"message","task":{"id":...},"message":{"id":"...","text":"...","attachments":[...],"sender":"<email>","sent_at":"..."}}
//	                                                       a human's message to an interactive task's session
//	            {"type":"dependency_done","task":{"id":...},"state":"done"|"failed"|"cancelled"|"rejected","reason":"...","node_task":"<local id>"}
//	                                                       a hub task the node's own task waits on has ended
//	node → hub  {"type":"accept","node_task":"<local id>"} it is running
//	            {"type":"refuse","reason":"..."}           not here (to an offer), or not mine (to a resume)
//	            {"type":"result","state":"done"|"failed","reason":"..."}
//
// A stream that carries an offer or a resume stays open for the task's
// life: the node writes its result there when the task ends, and the hub
// writes cancel there if a human cancels it, and approve for each of
// the task's approval requests a human answers, and, for an interactive
// task, message for each message a human sends it. While the task is the
// node's, the hub also writes attest there, a newly signed attestation
// for the same task, before the last one it sent lapses: a node checks
// the attestation again before each dispatch of the task, which may be
// hours after it accepted it. A cancel for a task with no
// stream (its node was away when it was cancelled) is the first message
// of a stream of its own, which the hub closes once it has written it.
// The hub closes a stream after the task's result, or a refusal. A
// dependency_done, telling a node that filed a task (rendezvous
// TypeFileTask) and asked to be told of its end, is likewise the only
// message of a stream of its own, written once its task is over.
const (
	TypeOffer  = "offer"
	TypeResume = "resume"
	TypeAttest = "attest"
	TypeCancel = "cancel"
	// TypeApprove answers one of a running task's approval requests.
	TypeApprove = "approve"
	// TypeMessage carries a human's message to an interactive task.
	TypeMessage = "message"
	// TypeDependencyDone tells a node that a hub task its own task
	// NodeTask waits on has ended, in State.
	TypeDependencyDone = "dependency_done"
	TypeAccept         = "accept"
	TypeRefuse         = "refuse"
	TypeResult         = "result"
)

// Message is one line on a task stream.
type Message struct {
	Type string `json:"type"`
	// Task is the task, in offer and resume; in attest and cancel, only
	// its ID.
	Task *Spec `json:"task,omitempty"`
	// Attestation is the hub's signed claims about Task, for the node it
	// is written to (hub/claims, docs/hub.md §6.4), in offer, resume and
	// attest; absent when the hub has no signing key.
	Attestation string `json:"requester_attestation,omitempty"`
	// NodeTask is the node's own ID for the task, in accept; in
	// dependency_done, for its task waiting on Task.
	NodeTask string `json:"node_task,omitempty"`
	// State is the task's end, done or failed, in result; in
	// dependency_done, also cancelled or rejected.
	State State `json:"state,omitempty"`
	// Reason says why, in refuse, result, cancel and approve.
	Reason string `json:"reason,omitempty"`
	// Request is the approval_request event's ID an approve answers.
	Request string `json:"request_id,omitempty"`
	// Decision is an approve's answer: Allow or Deny.
	Decision string `json:"decision,omitempty"`
	// AnsweredBy is the member of the task's team who answered, in
	// approve: the hub's claim about who decided.
	AnsweredBy string `json:"answered_by,omitempty"`
	// Message is a message's message: its ID, which the node tells
	// copies apart by, what it says and who sent it, the hub's claim.
	Message *events.Message `json:"message,omitempty"`
}

// maxLine bounds one message; an offer carries the prompt.
const maxLine = 1 << 20

// Stream reads and writes the messages of one task stream. The hub's
// dispatcher uses it, and so may a node.
type Stream struct {
	c  net.Conn
	r  *bufio.Reader
	mu sync.Mutex // serialises writes
}

// NewStream wraps a task stream.
func NewStream(c net.Conn) *Stream {
	return &Stream{c: c, r: bufio.NewReaderSize(c, 4096)}
}

// Read reads the next message.
func (s *Stream) Read() (Message, error) {
	var line []byte
	for {
		chunk, isPrefix, err := s.r.ReadLine()
		if err != nil {
			return Message{}, err
		}
		line = append(line, chunk...)
		if len(line) > maxLine {
			return Message{}, errors.New("tasks: task stream message too long")
		}
		if !isPrefix {
			break
		}
	}
	var m Message
	if err := json.Unmarshal(line, &m); err != nil {
		return Message{}, fmt.Errorf("tasks: bad task stream message: %w", err)
	}
	return m, nil
}

// Write writes one message.
func (s *Stream) Write(m Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.c.Write(append(b, '\n'))
	return err
}

// Close closes the stream.
func (s *Stream) Close() error { return s.c.Close() }
