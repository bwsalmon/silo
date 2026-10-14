package session

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/v2/agentcli"
	"github.com/bwsalmon/silo/v2/events"
	"github.com/bwsalmon/silo/v2/gateway/audit"
)

// Files the state directory keeps beside audit.jsonl.
const (
	// RawTranscriptFile is the CLI's headless output as it wrote it,
	// credentials redacted.
	RawTranscriptFile = "transcript.raw.jsonl"
	// TranscriptFile is the normalised transcript: one transcript event
	// per line.
	TranscriptFile = "transcript.jsonl"
)

// Sink receives a session's events (docs/events.md), in order, from
// whichever goroutine emitted them; calls never overlap.
type Sink interface{ Event(events.Event) }

// SinkFunc is a function as a Sink.
type SinkFunc func(events.Event)

func (f SinkFunc) Event(e events.Event) { f(e) }

// TextSink writes silo run's progress lines: status messages and the
// session's end.
type TextSink struct{ W io.Writer }

func (t TextSink) Event(e events.Event) {
	switch {
	case e.ApprovalRequest != nil:
		fmt.Fprintf(t.W, "\nsilo: %s\n", e.ApprovalRequest.Message)
	case e.Status != nil && e.Status.Message != "":
		if e.Status.State == events.StateWarning {
			fmt.Fprintf(t.W, "silo: warning: %s\n", e.Status.Message)
		} else {
			fmt.Fprintf(t.W, "silo: %s\n", e.Status.Message)
		}
	case e.Result != nil:
		x := e.Result.Exit
		why := x.Reason
		if x.Detail != "" {
			why += ": " + x.Detail
		}
		if x.CLI != nil {
			why += fmt.Sprintf(" (%s: %s)", x.CLI.Kind, x.CLI.Message)
		}
		fmt.Fprintf(t.W, "silo: session %s ended: %s (exit %d)\n", e.Session, why, x.Code)
	}
}

// JSONSink writes each event as a JSON line: silo run --json.
type JSONSink struct{ W io.Writer }

func (j JSONSink) Event(e events.Event) {
	if b, err := events.Marshal(e); err == nil {
		j.W.Write(b)
	}
}

// emitter stamps, redacts and delivers a session's events.
type emitter struct {
	mu      sync.Mutex
	sink    Sink
	session string
	phase   string
	seq     int64
	secrets []string // longest first
	// usage sums usage events per model, for the result.
	usage map[string]*events.Usage
	order []string
}

// setSecrets has every later event's strings redacted of the values.
func (m *emitter) setSecrets(named map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.secrets = m.secrets[:0]
	for name, v := range named {
		if len(v) >= audit.MinSecretLength {
			m.secrets = append(m.secrets, name+"\x00"+v)
		}
	}
	sort.Slice(m.secrets, func(i, j int) bool { return len(m.secrets[i]) > len(m.secrets[j]) })
}

func (m *emitter) redactLocked(s string) string {
	for _, nv := range m.secrets {
		name, v, _ := strings.Cut(nv, "\x00")
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, "[credential:"+name+"]")
		}
	}
	return s
}

func (m *emitter) redact(s string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.redactLocked(s)
}

func (m *emitter) setPhase(p string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.phase = p
}

// emit delivers e and returns it as delivered.
func (m *emitter) emit(e events.Event) events.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	e.V, e.Seq, e.Session = events.Version, m.seq, m.session
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	if e.Status != nil && e.Status.Phase == "" {
		e.Status.Phase = m.phase
	}
	e.Redact(m.redactLocked)
	if u := e.Usage; u != nil {
		if m.usage == nil {
			m.usage = map[string]*events.Usage{}
		}
		t := m.usage[u.Model]
		if t == nil {
			t = &events.Usage{Source: u.Source, Model: u.Model}
			m.usage[u.Model] = t
			m.order = append(m.order, u.Model)
		}
		t.Add(*u)
	}
	m.sink.Event(e)
	return e
}

func (m *emitter) totals() []events.Usage {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []events.Usage
	for _, k := range m.order {
		out = append(out, *m.usage[k])
	}
	return out
}

// toolCallEvent is an audit record as a tool_call event.
func toolCallEvent(r audit.Record) events.Event {
	c := &events.ToolCall{Phase: r.Event, Call: r.Call, Tool: r.Tool, Outcome: r.Outcome, Reason: r.Reason, Size: r.Size, Head: r.Head, DurationMS: r.DurationMS}
	if a, ok := r.Args.(map[string]any); ok {
		c.Args = a
	}
	return events.Event{Time: r.Time, Type: events.TypeToolCall, ToolCall: c}
}

// transcript is the writer the CLI's headless stdout is teed into: it
// keeps the raw output, parses it a line at a time and emits what the
// parser finds, keeping the transcript events in the normalised file.
type transcript struct {
	em     *emitter
	parser agentcli.Parser // nil when silo does not parse the CLI's output
	raw    *os.File
	norm   *os.File
	buf    []byte
	mu     sync.Mutex
	closed bool
}

func newTranscript(em *emitter, p agentcli.Parser, rawPath, normPath string) (*transcript, error) {
	raw, err := os.OpenFile(rawPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	t := &transcript{em: em, parser: p, raw: raw}
	if p != nil {
		if t.norm, err = os.OpenFile(normPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600); err != nil {
			raw.Close()
			return nil, err
		}
	}
	return t, nil
}

func (t *transcript) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return len(p), nil
	}
	t.buf = append(t.buf, p...)
	for {
		i := bytes.IndexByte(t.buf, '\n')
		if i < 0 {
			break
		}
		t.line(t.buf[:i])
		t.buf = t.buf[i+1:]
	}
	return len(p), nil
}

func (t *transcript) line(l []byte) {
	t.raw.WriteString(t.em.redact(string(l)) + "\n")
	l = bytes.TrimSpace(l)
	if t.parser == nil || len(l) == 0 {
		return
	}
	for _, e := range t.parser.Line(l) {
		e = t.em.emit(e)
		if e.Type == events.TypeTranscript {
			if b, err := events.Marshal(e); err == nil {
				t.norm.Write(b)
			}
		}
	}
}

// close parses a last line without its newline and closes the files.
func (t *transcript) close() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	if len(t.buf) > 0 {
		t.line(t.buf)
		t.buf = nil
	}
	t.closed = true
	t.raw.Close()
	if t.norm != nil {
		t.norm.Close()
	}
}

// outcome is what the parser made of the output; zero when there is no
// parser.
func (t *transcript) outcome() agentcli.Outcome {
	if t == nil || t.parser == nil {
		return agentcli.Outcome{}
	}
	return t.parser.Outcome()
}
