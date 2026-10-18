// Package rendezvous is how a Grain node reaches the Silo hub
// (docs/hub.md §4): the node dials out, over HTTPS, to a WebSocket at
// ConnectPath, and that one connection becomes a reverse tunnel. The
// hub never dials a node; a workstation behind NAT or a corporate
// firewall needs nothing open inbound.
//
// The WebSocket carries a mux (internal/tunnel/mux, grain's pkg/tunnel
// model). The node opens the first stream, the control stream, and
// says Hello on it: its ID, repositories and slots. The hub
// authenticates the node from the WebSocket handshake's credential,
// registers it (package registry) under that identity and answers
// Welcome; from then on the node heartbeats its free slots on the
// control stream, and either side opens further streams: the hub to
// hand a node work (Server.Open), the node to stream events back
// (Server.NodeStream). When the connection drops, the node is offline.
//
// The control stream speaks JSON, one message per line. Besides its
// heartbeats, a node may file a task at the hub on it (TypeFileTask),
// which the hub answers there (TypeFiled).
package rendezvous

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/bwsalmon/silo/hub/grainfiles"
	"github.com/bwsalmon/silo/hub/registry"
)

// ConnectPath is the hub's WebSocket endpoint for nodes.
const ConnectPath = "/v1/nodes/connect"

// DirectoryPath is the hub's node directory, as JSON.
const DirectoryPath = "/v1/nodes"

// Protocol is the WebSocket subprotocol both sides name, so that a
// version change is refused at the handshake rather than misread.
const Protocol = "silo-node.v1"

// Identity is what a node's credential proves (Server.Authenticate).
type Identity struct {
	// Owner is the principal the node is registered under.
	Owner string
	// Node, when set, is the one node ID the credential was minted for:
	// a hello naming any other is refused. Empty is a credential not
	// bound to a node.
	Node string
}

// Message types on the control stream.
const (
	TypeHello     = "hello"     // node → hub, first
	TypeWelcome   = "welcome"   // hub → node, the answer to hello
	TypeError     = "error"     // hub → node, a refusal; the hub then closes
	TypeHeartbeat = "heartbeat" // node → hub
	// TypeGoodbye is the hub ending a welcomed node's connection, Error
	// saying why (it is shutting down): the node reconnects, to this
	// hub's replacement, as it would after any drop.
	TypeGoodbye = "goodbye" // hub → node
	// TypeFileTask is the node filing a task at the hub (docs/hub.md
	// §6.1, Server.FileTask): Task is the filing, RequestID the node's
	// name for it. The hub answers each with TypeFiled.
	TypeFileTask = "file_task" // node → hub
	// TypeFiled is the hub's answer to a file_task of the same
	// RequestID: the task it filed, in Task, or why it filed none, in
	// Error. Unlike error, it does not end the connection.
	TypeFiled = "filed" // hub → node

	// TypeSubscribe updates a node's grainfile subscriptions (recipient
	// deployment URLs, repositories, folders, and grainfile URLs) on an
	// active control stream. The hub answers with TypeSubscribed.
	TypeSubscribe = "subscribe" // node → hub
	// TypeSubscribed acknowledges a node's TypeSubscribe request with its
	// canonicalized subscriptions, revision, and cursor (or Error).
	TypeSubscribed = "subscribed" // hub → node
	// TypeGrainfileEvent delivers one discoverable grainfile notification
	// (task_ready, process_updated, process_triggered,
	// dependency_completed) from the hub to a subscribed node.
	TypeGrainfileEvent = "grainfile_event" // hub → node
	// TypeCatchup asks the hub for missed grainfile_event notifications
	// since Cursor (or the node's stored cursor).
	TypeCatchup = "catchup" // node → hub
	// TypeCatchupResult answers a TypeCatchup request with the matching
	// GrainfileEvents and updated Cursor.
	TypeCatchupResult = "catchup_result" // hub → node
	// TypeGrainfileCompleted reports completion of a task or process
	// referencing a source Grainfile URL so the hub can emit
	// dependency_completed notices to waiting deployments.
	TypeGrainfileCompleted = "grainfile_completed" // node → hub
	// TypeGrainfileCompletedAck answers a TypeGrainfileCompleted message
	// with any emitted dependency_completed events (or Error).
	TypeGrainfileCompletedAck = "grainfile_completed_ack" // hub → node

	// TypeRunStarted and TypeRegisterRun announce a node-originated or
	// Grainfile-triggered run on the control stream so the hub registers it
	// in Running state without imperative offer/accept.
	TypeRunStarted  = "run_started"  // node → hub
	TypeRegisterRun = "register_run" // node → hub
	// TypeRunRegistered answers a TypeRunStarted or TypeRegisterRun message
	// that carried a RequestID.
	TypeRunRegistered = "run_registered" // hub → node
)

// Message is one line on the control stream.
type Message struct {
	Type string `json:"type"`
	// Node is the node's registration, in hello.
	Node *registry.Node `json:"node,omitempty"`
	// Owner is the identity the hub registered the node under, in
	// welcome: the one its credential proves, which the node may log
	// to show whom it is serving as.
	Owner string `json:"owner,omitempty"`
	// HeartbeatSeconds is how often the hub wants heartbeats, in welcome.
	HeartbeatSeconds int `json:"heartbeat_seconds,omitempty"`
	// Free is the node's free slots, in heartbeat.
	Free *int `json:"free,omitempty"`
	// Error is the reason, in error, and in a filed that filed nothing.
	Error string `json:"error,omitempty"`
	// RequestID names a file_task, subscribe, catchup, or
	// grainfile_completed request, and the hub message answering it.
	RequestID string `json:"request_id,omitempty"`
	// Task is the filing, in file_task, and the task filed, in filed:
	// package tasks' to read and write (tasks.Filing, tasks.Task).
	Task json.RawMessage `json:"task,omitempty"`

	// Subscriptions, or the shorthand fields Recipients/Repos/Folders/Grainfiles/Cursor,
	// declare a node's grainfile subscriptions in hello or subscribe, and
	// return the stored subscriptions in welcome or subscribed.
	Subscriptions *grainfiles.Subscriptions `json:"subscriptions,omitempty"`
	Recipients    []string                  `json:"recipients,omitempty"`
	Repos         []string                  `json:"repos,omitempty"`
	Folders       []string                  `json:"folders,omitempty"`
	Grainfiles    []string                  `json:"grainfiles,omitempty"`
	Cursor        int64                     `json:"cursor,omitempty"`
	Revision      int64                     `json:"revision,omitempty"`

	// GrainfileEvent carries a single grainfile notification in
	// TypeGrainfileEvent.
	GrainfileEvent *grainfiles.Event `json:"grainfile_event,omitempty"`
	// GrainfileEvents carries a batch of grainfile notifications in
	// TypeCatchupResult or TypeGrainfileCompletedAck.
	GrainfileEvents []grainfiles.Event `json:"grainfile_events,omitempty"`

	// GrainfileURL, CommitSHA, State, and Reason report completion of a
	// source Grainfile in TypeGrainfileCompleted.
	GrainfileURL string `json:"grainfile_url,omitempty"`
	CommitSHA    string `json:"commit_sha,omitempty"`
	State        string `json:"state,omitempty"`
	Reason       string `json:"reason,omitempty"`

	// Run carries the run registration payload in TypeRunStarted or
	// TypeRegisterRun, and the registered task in TypeRunRegistered.
	Run json.RawMessage `json:"run,omitempty"`
	// Optional top-level run fields if a node sends TypeRunStarted or
	// TypeRegisterRun flat instead of nested under Run.
	ID            string `json:"id,omitempty"`
	NodeTask      string `json:"node_task,omitempty"`
	Repo          string `json:"repo,omitempty"`
	Kind          string `json:"kind,omitempty"`
	Title         string `json:"title,omitempty"`
	Prompt        string `json:"prompt,omitempty"`
	TriggerSource string `json:"trigger_source,omitempty"`
}

// maxLine bounds one control message.
const maxLine = 1 << 20

// lines reads and writes control messages on one stream. Writes may
// come from more than one goroutine (the hub's goodbye); reads may not.
type lines struct {
	c  net.Conn
	r  *bufio.Reader
	mu sync.Mutex
}

func newLines(c net.Conn) *lines {
	return &lines{c: c, r: bufio.NewReaderSize(c, 4096)}
}

func (l *lines) read() (Message, error) {
	var line []byte
	for {
		chunk, isPrefix, err := l.r.ReadLine()
		if err != nil {
			return Message{}, err
		}
		line = append(line, chunk...)
		if len(line) > maxLine {
			return Message{}, errors.New("rendezvous: control message too long")
		}
		if !isPrefix {
			break
		}
	}
	var m Message
	if err := json.Unmarshal(line, &m); err != nil {
		return Message{}, fmt.Errorf("rendezvous: bad control message: %w", err)
	}
	return m, nil
}

func (l *lines) write(m Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err = l.c.Write(append(b, '\n'))
	return err
}
