// Adapted from github.com/bwsalmon/grain pkg/agent/runfailure.go, pkg/agent/capacity.go, pkg/agent/usagelimit.go and pkg/agent/refusal.go at c950ca0858bc4b381e5ce7e2bd716997f63bd24d, Apache License 2.0.

package agentcli

import (
	"strings"

	"github.com/bwsalmon/silo/v2/events"
)

// Parser reads one CLI's headless output, a line at a time, into
// normalised events (grain.md §2.1). It is used by one goroutine.
type Parser interface {
	// Line returns the events one line of output says: transcript
	// items, usage and, the first time the CLI names it, resumable.
	// Only the payload and Type are set; the session stamps the rest.
	// A line the parser does not understand yields nothing.
	Line(line []byte) []events.Event
	// Outcome is what the output said, once the CLI has exited.
	Outcome() Outcome
}

// Outcome is what a CLI's output said about its run.
type Outcome struct {
	FinalText  string
	CLISession string
	// Finished reports whether the output reached its terminal event
	// (claude's result, codex's turn.completed or turn.failed).
	Finished bool
	// Turns counts the terminal events seen so far: one per turn, so a
	// rise says a turn has just ended.
	Turns int `json:"-"`
	// Failure is what the output said went wrong; nil for a run it
	// reported as a success.
	Failure *events.CLIError
}

// Transcripts is a Mapping whose CLI's headless output silo parses.
type Transcripts interface {
	NewParser(req Request) Parser
}

// NewParser returns m's parser for req, or nil when silo does not parse
// m's output.
func NewParser(m Mapping, req Request) Parser {
	if t, ok := m.(Transcripts); ok {
		return t.NewParser(req)
	}
	return nil
}

// sharedCapacityMarkers are the gateway statuses any provider returns
// when it has no capacity.
var sharedCapacityMarkers = []string{
	"503 service unavailable",
	"502 bad gateway",
	"504 gateway timeout",
}

// refusalHints are words a provider's refusal of a request uses: a
// failure that mentions none of them is the run's own (a test that
// failed, a tool that broke), not the provider's.
var refusalHints = []string{
	"overload", "capacity", "unavailable", "quota", "rate limit", "rate-limit",
	"too many requests", "try again", "retry", "temporarily", "upstream", "gateway",
	"server error", "api error", "unauthorized", "forbidden", "authentication",
	"429", "500", "502", "503", "504", "529",
}

// Mentions reports whether text contains any of markers, ignoring case.
func Mentions(text string, markers []string) bool {
	lower := strings.ToLower(text)
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// MentionsCapacity reports whether text says the provider had no
// capacity: one of markers, the CLI's own, or a shared gateway status.
func MentionsCapacity(text string, markers []string) bool {
	return Mentions(text, sharedCapacityMarkers) || Mentions(text, markers)
}

// ResemblesRefusal reports whether a failure the CLI reported reads as
// the provider refusing the request rather than the run failing on its
// own terms.
func ResemblesRefusal(text string) bool { return Mentions(text, refusalHints) }

// maxMessage is how much of a failure's text a CLIError keeps.
const maxMessage = 240

// TrimMessage folds text onto one line and cuts it to a length a run
// listing can show.
func TrimMessage(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxMessage {
		n := maxMessage
		for n > 0 && s[n]&0xC0 == 0x80 {
			n--
		}
		s = s[:n] + "..."
	}
	return s
}
