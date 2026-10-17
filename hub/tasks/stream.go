package tasks

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
)

// The task stream (docs/hub.md §6.2) is a stream the hub opens to a node
// (rendezvous.Server.Open) for one task, carrying JSON, one message per
// line. The hub writes first:
//
//	hub → node  {"type":"offer","task":{...},"requester_attestation":"<JWT>"}   run this task
//	            {"type":"resume","task":{...},"requester_attestation":"<JWT>"}  a task you ran when the hub lost you: still yours?
//	            {"type":"cancel","task":{"id":...}}        stop it
//	            {"type":"approve","task":{"id":...},"request_id":"...","decision":"allow"|"deny","answered_by":"<email>","reason":"..."}
//	                                                       a human's answer to the task's approval_request event of that ID
//	node → hub  {"type":"accept","node_task":"<local id>"} it is running
//	            {"type":"refuse","reason":"..."}           not here (to an offer), or not mine (to a resume)
//	            {"type":"result","state":"done"|"failed","reason":"..."}
//
// A stream that carries an offer or a resume stays open for the task's
// life: the node writes its result there when the task ends, and the hub
// writes cancel there if a human cancels it, and approve for each of
// the task's approval requests a human answers. A cancel for a task with no
// stream (its node was away when it was cancelled) is the first message
// of a stream of its own, which the hub closes once it has written it.
// The hub closes a stream after the task's result, or a refusal.
const (
	TypeOffer  = "offer"
	TypeResume = "resume"
	TypeCancel = "cancel"
	// TypeApprove answers one of a running task's approval requests.
	TypeApprove = "approve"
	TypeAccept  = "accept"
	TypeRefuse  = "refuse"
	TypeResult  = "result"
)

// Message is one line on a task stream.
type Message struct {
	Type string `json:"type"`
	// Task is the task, in offer and resume; in cancel, only its ID.
	Task *Spec `json:"task,omitempty"`
	// Attestation is the hub's signed claims about Task, for the node it
	// is written to (hub/claims, docs/hub.md §6.4), in offer and resume;
	// absent when the hub has no signing key.
	Attestation string `json:"requester_attestation,omitempty"`
	// NodeTask is the node's own ID for the task, in accept.
	NodeTask string `json:"node_task,omitempty"`
	// State is the task's end, done or failed, in result.
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
