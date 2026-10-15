package session

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/bwsalmon/silo/v2/agentcli"
	"github.com/bwsalmon/silo/v2/location"
)

// Delivery says what Inject did with a message (grain.md §2.1).
type Delivery string

const (
	// Delivered: written to the running CLI, which reads it now. A
	// message injected before the CLI starts goes with its prompt.
	Delivered Delivery = "delivered"
	// Queued: kept until the CLI's turn ends, and then sent as its next
	// turn (codex exec resume, claude --resume).
	Queued Delivery = "queued"
)

// injector delivers injected messages to the CLI: live on the stdin of
// a CLI that reads it mid-run (claude), and otherwise queued until the
// turn ends and sent as a resumed session's next turn (codex, and a
// claude whose turn has ended).
type injector struct {
	mu sync.Mutex
	// refused is why the session takes no messages; nil when it does.
	refused error
	inj     agentcli.Injection
	// started is set once the first CLI process is started.
	started bool
	// w is the live CLI's stdin while it is open; wmu orders writes to
	// it, the first being the prompt.
	w   *os.File
	wmu sync.Mutex
	// pending are messages not yet written.
	pending []string
	// ended is set once no further turn will start.
	ended bool
}

func newInjector(mode string, cli string, m agentcli.Mapping) *injector {
	j := &injector{}
	in, ok := m.(agentcli.Injector)
	switch {
	case mode != agentcli.Headless:
		j.refused = errors.New("only a headless session takes injected messages: in an interactive one, type into the CLI")
	case !ok:
		j.refused = fmt.Errorf("cli %s takes no injected messages: silo has no way to give it one mid-run", cli)
	default:
		j.inj = in.Injection()
	}
	return j
}

// Inject gives the CLI a user message mid-run (grain.md §2.1) and says
// whether it was delivered now or queued for the CLI's next turn.
func (s *Session) Inject(text string) (Delivery, error) {
	if text == "" {
		return "", errors.New("inject: the message is empty")
	}
	d, err := s.inj.inject(text)
	if err == nil {
		s.status("injected a message (%s)", d)
	}
	return d, err
}

func (j *injector) inject(text string) (Delivery, error) {
	j.mu.Lock()
	switch {
	case j.refused != nil:
		j.mu.Unlock()
		return "", j.refused
	case j.ended:
		j.mu.Unlock()
		return "", errors.New("the session is ending: it starts no further turn")
	case j.w == nil && !j.started && j.inj.Live:
		j.pending = append(j.pending, text)
		j.mu.Unlock()
		return Delivered, nil
	case j.w == nil:
		j.pending = append(j.pending, text)
		j.mu.Unlock()
		return Queued, nil
	}
	w := j.w
	j.mu.Unlock()
	// Not under mu: a CLI slow to read must not hold up its turn ending.
	j.wmu.Lock()
	_, err := w.Write(j.inj.Message(text))
	j.wmu.Unlock()
	if err == nil {
		return Delivered, nil
	}
	// The CLI stopped reading; the message waits for its next turn.
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.w == w {
		j.closeLocked()
	}
	if j.ended {
		return "", errors.New("the session is ending: it starts no further turn")
	}
	j.pending = append(j.pending, text)
	return Queued, nil
}

// begin readies cmd's stdin before a CLI process starts, carrying msgs
// (a resumed turn's) or the messages injected during setup. It returns
// what to call once Start has returned.
func (j *injector) begin(cmd *location.Command, msgs []string) (func(), error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.started = true
	if j.refused != nil {
		return func() {}, nil
	}
	if !j.inj.Live {
		if len(msgs) > 0 {
			cmd.Stdin = j.inj.Message(msgs...)
		}
		return func() {}, nil
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	first := append(append([]byte(nil), cmd.Stdin...), j.inj.Message(append(msgs, j.pending...)...)...)
	j.pending = nil
	cmd.Stdin, cmd.Input = nil, r
	j.w = w
	// Written while the CLI starts reading: a prompt larger than the
	// pipe holds would otherwise block here. Later writes wait for it.
	j.wmu.Lock()
	go func() {
		defer j.wmu.Unlock()
		_, _ = w.Write(first)
	}()
	return func() { r.Close() }, nil
}

// turnEnded closes a live CLI's stdin when its turn ends, so that it
// exits once it has answered what it has read; anything injected later
// waits for a resumed turn.
func (j *injector) turnEnded() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.closeLocked()
}

func (j *injector) closeLocked() {
	if j.w == nil {
		return
	}
	// A write still blocked on the pipe fails rather than holding this
	// up.
	j.w.Close()
	j.w = nil
}

// next is called when the CLI exits: it returns the messages for a
// resumed turn, or none when the session should end, with a warning
// when messages are dropped. clean is whether the CLI's turn ended well
// enough to resume.
func (j *injector) next(o agentcli.Outcome, clean bool) (msgs []string, dropped string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.closeLocked()
	if j.ended || len(j.pending) == 0 {
		j.ended = true
		return nil, ""
	}
	msgs, j.pending = j.pending, nil
	switch {
	case !clean:
		dropped = "the agent CLI did not end its turn cleanly"
	case o.CLISession == "":
		dropped = "the agent CLI named no session to resume"
	default:
		return msgs, ""
	}
	j.ended = true
	return nil, fmt.Sprintf("%d injected message(s) were not delivered: %s", len(msgs), dropped)
}

// end stops further injection: the session is ending.
func (j *injector) end() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.ended = true
	j.closeLocked()
}
