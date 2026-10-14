package silo_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	silo "github.com/bwsalmon/silo/v2"
	"github.com/bwsalmon/silo/v2/events"
	"github.com/bwsalmon/silo/v2/internal/fakecli"
	"github.com/bwsalmon/silo/v2/silofile"
)

func TestMain(m *testing.M) {
	if fakecli.Is() {
		os.Exit(fakecli.Main())
	}
	os.Exit(m.Run())
}

// host is one session's private machine: its own HOME (the fake's
// channel), runtime and state directories.
type host struct {
	root, home string
	environ    []string
}

func newHost(t *testing.T) host {
	t.Helper()
	root := t.TempDir()
	h := host{root: root, home: filepath.Join(root, "home")}
	bin := filepath.Join(root, "bin")
	for _, d := range []string{bin, h.home, filepath.Join(root, "run"), filepath.Join(root, "state")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(bin, "claude")); err != nil {
		t.Fatal(err)
	}
	h.environ = []string{
		"PATH=" + bin, "HOME=" + h.home, "LANG=C.UTF-8",
		"XDG_RUNTIME_DIR=" + filepath.Join(root, "run"),
		"XDG_STATE_HOME=" + filepath.Join(root, "state"),
	}
	return h
}

func (h host) fake(t *testing.T, cfg fakecli.Config) {
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(h.home, "fake.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (h host) report(t *testing.T) fakecli.Report {
	var r fakecli.Report
	b, err := os.ReadFile(filepath.Join(h.home, "report.json"))
	if err != nil {
		t.Fatalf("no report: %v", err)
	}
	json.Unmarshal(b, &r)
	return r
}

// stream is a recorded stream-json run that answers with text and
// names session cliSession.
func stream(cliSession, text string) []string {
	return []string{
		fmt.Sprintf(`{"type":"system","subtype":"init","session_id":%q,"model":"claude-opus-5-5"}`, cliSession),
		fmt.Sprintf(`{"type":"assistant","session_id":%q,"message":{"id":"m1","model":"claude-opus-5-5","content":[{"type":"text","text":%q}],"usage":{"input_tokens":10,"output_tokens":5}}}`, cliSession, text),
		fmt.Sprintf(`{"type":"result","subtype":"success","is_error":false,"result":%q,"session_id":%q}`, text, cliSession),
	}
}

func file(deadline time.Duration) silofile.Silofile {
	f := silofile.Silofile{Silo: "v2", Name: "lib", Agent: silofile.Agent{CLI: "claude", Mode: "headless", Prompt: "do the thing"}}
	f.Credentials = []silofile.Credential{{Name: "token", From: silofile.Source{Env: "UNSET_IN_THE_ENVIRONMENT"}, To: []silofile.Destination{{Agent: "env", Name: "SILO_TOKEN"}}}}
	if deadline > 0 {
		d := silofile.Duration(deadline)
		f.Agent.Limits = &silofile.Limits{Deadline: &d}
	}
	return f
}

// TestConcurrentSessions runs four sessions at once in one process: two
// that complete, one cancelled and one that hits its deadline. Each has
// its own credential value and gateway call; neither may show up in
// another's events, audit log or transcript.
func TestConcurrentSessions(t *testing.T) {
	type tc struct {
		name     string
		deadline time.Duration
		cfg      fakecli.Config
		cancel   bool
		reason   string
		code     int
	}
	cases := []tc{
		{name: "one", cfg: fakecli.Config{}, reason: events.ReasonCompleted},
		{name: "two", cfg: fakecli.Config{}, reason: events.ReasonCompleted},
		{name: "cancelled", cfg: fakecli.Config{Wait: true}, cancel: true, reason: events.ReasonCancelled, code: 143},
		{name: "deadline", deadline: 2 * time.Second, cfg: fakecli.Config{Wait: true}, reason: events.ReasonDeadline, code: 124},
	}
	type run struct {
		tc
		h      host
		secret string
		s      *silo.Session
		evs    []events.Event
	}
	runs := make([]*run, len(cases))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for i, c := range cases {
		r := &run{tc: c, h: newHost(t), secret: "secret-value-" + c.name}
		r.cfg.Call = "probe"
		r.cfg.Args = map[string]any{"marker": "marker-" + c.name, "leak": r.secret}
		r.cfg.Stream = stream("cli-"+c.name, "answer from "+c.name+" "+r.secret)
		r.h.fake(t, r.cfg)
		s, err := silo.Start(ctx, file(c.deadline), silo.Options{
			Environ: r.h.environ, Credentials: map[string]string{"token": r.secret}, Grace: 2 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		r.s = s
		runs[i] = r
	}
	var wg sync.WaitGroup
	for _, r := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for e := range r.s.Events() {
				r.evs = append(r.evs, e)
				if r.cancel && e.Type == events.TypeResumable {
					r.s.Cancel("the caller is done")
				}
			}
		}()
	}
	wg.Wait()

	for _, r := range runs {
		t.Run(r.name, func(t *testing.T) {
			res, err := r.s.Wait()
			if err != nil {
				t.Errorf("Wait: %v", err)
			}
			if res.Exit.Reason != r.reason || res.Exit.Code != r.code {
				t.Errorf("exit %+v, want %s %d", res.Exit, r.reason, r.code)
			}
			last := r.evs[len(r.evs)-1]
			if last.Type != events.TypeResult || last.Result.Exit != res.Exit {
				t.Errorf("last event %+v, want the result", last)
			}
			if res.CLISession != "cli-"+r.name {
				t.Errorf("cli session %q", res.CLISession)
			}
			if r.reason == events.ReasonCompleted {
				if want := "answer from " + r.name + " [credential:token]"; res.FinalText != want {
					t.Errorf("final text %q, want %q", res.FinalText, want)
				}
				if len(res.UsageTotals) != 1 || res.UsageTotals[0].InputTokens != 10 || res.UsageTotals[0].Source != events.SourceAgent {
					t.Errorf("usage totals %+v", res.UsageTotals)
				}
			}
			// The CLI was given this session's credential and gateway
			// token, and the token appears nowhere in another session.
			rep := r.h.report(t)
			if rep.Token == "" || !strings.Contains(rep.Stdin, "do the thing") {
				t.Errorf("report %+v", rep)
			}
			var seq int64
			sawCall := false
			for _, e := range r.evs {
				seq++
				if e.Seq != seq || e.V != events.Version || e.Session == "" || e.Session != r.evs[0].Session {
					t.Errorf("event %d: %+v", seq, e)
				}
				if e.ToolCall != nil && e.ToolCall.Tool == "probe" {
					sawCall = true
				}
			}
			if !sawCall {
				t.Error("no tool_call event for the gateway call")
			}
			b, _ := json.Marshal(r.evs)
			for _, o := range runs {
				own := o == r
				if strings.Contains(string(b), o.secret) {
					t.Errorf("events carry %s's credential unredacted", o.name)
				}
				if !own && strings.Contains(string(b), "marker-"+o.name) {
					t.Errorf("events carry %s's gateway call", o.name)
				}
				if !own && strings.Contains(string(b), o.h.report(t).Token) {
					t.Errorf("events carry %s's gateway token", o.name)
				}
			}
			audit, err := os.ReadFile(res.AuditPath)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(audit), "marker-"+r.name) {
				t.Errorf("audit log lacks this session's call: %s", audit)
			}
			for _, o := range runs {
				if o != r && strings.Contains(string(audit), "marker-"+o.name) {
					t.Errorf("audit log has %s's call", o.name)
				}
			}
			raw, err := os.ReadFile(res.TranscriptPath)
			if err != nil || !strings.Contains(string(raw), `"session_id":"cli-`+r.name+`"`) || strings.Contains(string(raw), r.secret) {
				t.Errorf("raw transcript %s: %v %s", res.TranscriptPath, err, raw)
			}
			norm, err := os.Open(res.NormalizedTranscriptPath)
			if err != nil {
				t.Fatal(err)
			}
			defer norm.Close()
			sc := bufio.NewScanner(norm)
			n := 0
			for sc.Scan() {
				var e events.Event
				if err := events.Unmarshal(sc.Bytes(), &e); err != nil || e.Type != events.TypeTranscript {
					t.Errorf("normalised line %q: %v", sc.Text(), err)
				}
				n++
			}
			if n == 0 && r.reason == events.ReasonCompleted {
				t.Error("empty normalised transcript")
			}
		})
	}
}

func TestStartRefusesWhatALibrarySessionCannotRun(t *testing.T) {
	f := file(0)
	f.Agent.Mode = "interactive"
	if _, err := silo.Start(context.Background(), f, silo.Options{}); err == nil {
		t.Error("interactive mode was accepted")
	}
	f = file(0)
	f.Agent.CLI = ""
	if _, err := silo.Start(context.Background(), f, silo.Options{}); err == nil {
		t.Error("an invalid Silofile was accepted")
	}
}

func TestExtendDeadline(t *testing.T) {
	h := newHost(t)
	h.fake(t, fakecli.Config{Wait: true, Stream: stream("x", "hi")})
	start := time.Now()
	s, err := silo.Start(context.Background(), file(time.Second), silo.Options{Environ: h.environ, Credentials: map[string]string{"token": "tok-1234"}, Grace: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for e := range s.Events() {
		if e.Status != nil && e.Status.Step == "agent" && e.Status.State == events.StateStarted {
			s.ExtendDeadline(time.Now().Add(3 * time.Second))
		}
	}
	res, _ := s.Wait()
	if res.Exit.Reason != events.ReasonDeadline {
		t.Fatalf("exit %+v", res.Exit)
	}
	if d := time.Since(start); d < 3*time.Second {
		t.Errorf("ended after %s: the deadline was not extended", d)
	}
	if s.Inject("more") != silo.ErrNotYet {
		t.Error("Inject is stage 6b's")
	}
}
