// Binary fakeacp is a test ACP coding-agent CLI speaking JSON-RPC 2.0 over stdio.
// It supports initialize, session/new, session/load, session/prompt (including
// sending session/request_permission back to the bridge before completing a turn
// and streaming session/update notifications), and session/cancel.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   any             `json:"error,omitempty"`
}

func main() {
	var writeMu sync.Mutex
	send := func(v any) {
		writeMu.Lock()
		defer writeMu.Unlock()
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		b = append(b, '\n')
		_, _ = os.Stdout.Write(b)
	}

	var nextID atomic.Int64
	pending := map[string]chan rpcMsg{}
	var pendingMu sync.Mutex

	sessionID := "fake-session-1"
	cancelled := make(chan struct{}, 8)

	handlePrompt := func(req rpcMsg) {
		var p struct {
			SessionID string `json:"sessionId"`
			Text      string `json:"text"`
			Prompt    []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"prompt"`
		}
		_ = json.Unmarshal(req.Params, &p)
		text := p.Text
		if text == "" {
			for _, part := range p.Prompt {
				if part.Text != "" {
					text = part.Text
					break
				}
			}
		}
		sid := p.SessionID
		if sid == "" {
			sid = sessionID
		}

		// Emit initial session/update notification.
		send(map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": sid,
				"update": map[string]any{
					"sessionUpdate": "agent_message_chunk",
					"content": map[string]any{
						"type": "text",
						"text": "starting: " + text,
					},
				},
			},
		})

		if strings.Contains(text, "hang") {
			<-cancelled
			send(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  map[string]any{"stopReason": "cancelled"},
			})
			return
		}

		// Unless explicitly told to skip permissions, request permission via ACP
		// `session/request_permission` before running the tool/command.
		if !strings.Contains(text, "no-perm") {
			callID := fmt.Sprintf(`"perm-%d"`, nextID.Add(1))
			ch := make(chan rpcMsg, 1)
			pendingMu.Lock()
			pending[callID] = ch
			pendingMu.Unlock()

			send(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(callID),
				"method":  "session/request_permission",
				"params": map[string]any{
					"sessionId": sid,
					"toolCall": map[string]any{
						"toolCallId": "tc-1",
						"title":      "Execute shell command for: " + text,
						"kind":       "execute",
					},
					"options": []map[string]any{
						{"optionId": "allow_once", "name": "Allow once", "kind": "allow_once"},
						{"optionId": "reject_once", "name": "Reject", "kind": "reject_once"},
					},
				},
			})

			select {
			case resp := <-ch:
				var out struct {
					Outcome struct {
						Outcome  string `json:"outcome"`
						OptionID string `json:"optionId"`
					} `json:"outcome"`
				}
				_ = json.Unmarshal(resp.Result, &out)
				if out.Outcome.Outcome != "selected" || strings.HasPrefix(out.Outcome.OptionID, "reject") {
					send(map[string]any{
						"jsonrpc": "2.0",
						"method":  "session/update",
						"params": map[string]any{
							"sessionId": sid,
							"update": map[string]any{
								"sessionUpdate": "agent_message_chunk",
								"content": map[string]any{
									"type": "text",
									"text": "permission denied for: " + text,
								},
							},
						},
					})
					send(map[string]any{
						"jsonrpc": "2.0",
						"id":      req.ID,
						"result":  map[string]any{"stopReason": "cancelled"},
					})
					return
				}
			case <-cancelled:
				send(map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result":  map[string]any{"stopReason": "cancelled"},
				})
				return
			}
		}

		// Emit completion update and finish the turn.
		send(map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": sid,
				"update": map[string]any{
					"sessionUpdate": "agent_message_chunk",
					"content": map[string]any{
						"type": "text",
						"text": "completed: " + text,
					},
				},
			},
		})
		send(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  map[string]any{"stopReason": "end_turn"},
		})
	}

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var msg rpcMsg
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}

		// Response to a request sent by fakeacp (e.g. session/request_permission).
		if msg.Method == "" && len(msg.ID) > 0 {
			key := string(msg.ID)
			pendingMu.Lock()
			ch, ok := pending[key]
			if ok {
				delete(pending, key)
			}
			pendingMu.Unlock()
			if ok {
				ch <- msg
			}
			continue
		}

		switch msg.Method {
		case "initialize":
			send(map[string]any{
				"jsonrpc": "2.0",
				"id":      msg.ID,
				"result": map[string]any{
					"protocolVersion": 1,
					"agentInfo":       map[string]any{"name": "fakeacp", "version": "0.1.0"},
				},
			})
		case "session/new", "session/load":
			send(map[string]any{
				"jsonrpc": "2.0",
				"id":      msg.ID,
				"result": map[string]any{
					"sessionId": sessionID,
				},
			})
		case "session/prompt":
			go handlePrompt(msg)
		case "session/cancel":
			select {
			case cancelled <- struct{}{}:
			default:
			}
			if len(msg.ID) > 0 {
				send(map[string]any{
					"jsonrpc": "2.0",
					"id":      msg.ID,
					"result":  map[string]any{"cancelled": true},
				})
			}
		}
	}
}
