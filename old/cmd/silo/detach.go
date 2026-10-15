package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/bwsalmon/silo/old/events"
	"github.com/bwsalmon/silo/old/session"
)

// superviseCmd is the hidden command silo run --detach starts its
// supervisor with: silo __supervise <silo run's arguments>.
const superviseCmd = "__supervise"

// supervisorLog is the supervisor's own log, beside the audit log.
const supervisorLog = "supervisor.log"

// handshakeFD is the pipe the supervisor reports on: "ok <id>" once the
// session exists, or "fail <code>" and what it printed.
const handshakeFD = 3

// startDetached is silo run --detach (grain.md §2.7): it starts the
// session's supervisor, a silo process in a session of its own that is
// no child of the caller once this one exits, waits until the session
// exists, prints its id and exits. The supervisor owns the CLI, the
// gateway and teardown, and writes the session's events to its
// events.jsonl, which silo attach follows.
func startDetached(args []string, environ []string, stdout, stderr io.Writer) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "silo run --detach: %v\n", err)
		return session.ExitPrerequisite
	}
	r, w, err := os.Pipe()
	if err != nil {
		fmt.Fprintf(stderr, "silo run --detach: %v\n", err)
		return session.ExitPrerequisite
	}
	defer r.Close()
	cmd := exec.Command(exe, append([]string{superviseCmd}, args...)...)
	if environ == nil {
		environ = os.Environ()
	}
	// Stdin, stdout and stderr are /dev/null: the supervisor must not
	// hold the caller's pipes open, nor die writing to them once the
	// caller has gone.
	cmd.Env, cmd.ExtraFiles = environ, []*os.File{w}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		w.Close()
		fmt.Fprintf(stderr, "silo run --detach: starting the supervisor: %v\n", err)
		return session.ExitPrerequisite
	}
	w.Close()
	report, _ := io.ReadAll(r)
	line, rest, _ := strings.Cut(string(report), "\n")
	verb, arg, _ := strings.Cut(line, " ")
	switch verb {
	case "ok":
		// The supervisor is not waited for: it outlives this process.
		_ = cmd.Process.Release()
		fmt.Fprintln(stdout, arg)
		return exitOK
	case "fail":
		_ = cmd.Wait()
		fmt.Fprint(stderr, rest)
		code, err := strconv.Atoi(arg)
		if err != nil || code == 0 {
			code = session.ExitPrerequisite
		}
		return code
	}
	err = cmd.Wait()
	fmt.Fprintf(stderr, "silo run --detach: the supervisor exited before the session started (%v)\n", err)
	return session.ExitPrerequisite
}

// supervise is silo __supervise: the detached session's own process.
func supervise(args []string) int {
	syscall.CloseOnExec(handshakeFD)
	// Nothing reaches it from a terminal; SIGTERM still ends the session
	// as it ends silo run.
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
	sup := &supervisor{hs: os.NewFile(handshakeFD, "handshake")}
	code := runSessionWith(args, nil, io.Discard, sup.log(), session.Options{}, sup)
	sup.finish(code)
	return code
}

// supervisor is the state of silo __supervise.
type supervisor struct {
	hs *os.File
	l  supLog
}

func (s *supervisor) log() io.Writer { return &s.l }

// started is session.Options.Started: the session exists, so its log
// moves to the state directory and the caller is told its id.
func (s *supervisor) started(ss *session.Session) {
	s.l.open(filepath.Join(ss.StateDir, supervisorLog))
	if s.hs != nil {
		fmt.Fprintf(s.hs, "ok %s\n", ss.ID)
		s.hs.Close()
		s.hs = nil
	}
}

// finish tells a caller still waiting that the session never started.
func (s *supervisor) finish(code int) {
	if s.hs == nil {
		return
	}
	if code == 0 {
		code = session.ExitPrerequisite
	}
	fmt.Fprintf(s.hs, "fail %d\n%s", code, s.l.buffered())
	s.hs.Close()
	s.hs = nil
}

// supLog is the supervisor's stderr: held in memory until the session
// has a state directory, then supervisor.log there.
type supLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
	f   *os.File
}

func (l *supLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		return l.f.Write(p)
	}
	return l.buf.Write(p)
}

func (l *supLog) open(path string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	f.Write(l.buf.Bytes())
	l.buf.Reset()
	l.f = f
}

func (l *supLog) buffered() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// labelFlags collects --label key=value.
type labelFlags map[string]string

func (l labelFlags) set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("want key=value, not %q", v)
	}
	l[k] = val
	return nil
}

func (l labelFlags) String() string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		keys[i] = k + "=" + l[k]
	}
	return strings.Join(keys, ",")
}

// parseInterleaved parses fs's flags wherever they are among the
// arguments (silo attach <id> --from 5), and returns the rest.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// attach is silo attach (grain.md §2.7): it replays the session's events
// after --from and follows them to the result, then exits with the
// session's exit code. Interrupting it leaves the session running.
func attach(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("silo attach", flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.Int64("from", 0, "replay the events after this seq: the last one the caller has (0 replays all)")
	jsonOut := fs.Bool("json", false, "write the events as JSON lines (docs/events.md)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: silo attach [--from seq] [--json] <session>")
		fs.PrintDefaults()
	}
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(pos) != 1 || *from < 0 {
		fs.Usage()
		return exitUsage
	}
	id := pos[0]
	if _, err := os.Stat(session.EventsPath(getenv, id)); !session.ValidID(id) || err != nil {
		// A unique prefix of a running session's id.
		if id, err = findSession(getenv, pos[0]); err != nil {
			fmt.Fprintf(stderr, "silo attach: %v\n", err)
			return 1
		}
	}
	var sink session.Sink = session.TextSink{W: stdout}
	if *jsonOut {
		sink = session.JSONSink{W: stdout}
	}
	code := -1
	err = session.Follow(context.Background(), getenv, id, *from, func(e events.Event) error {
		sink.Event(e)
		if e.Result != nil {
			code = e.Result.Exit.Code
		}
		return nil
	})
	if err != nil || code < 0 {
		if err == nil {
			err = session.ErrNoResult
		}
		fmt.Fprintf(stderr, "silo attach: session %s: %v\n", id, err)
		if errors.Is(err, session.ErrNoResult) {
			fmt.Fprintln(stderr, "silo attach: silo gc removes what it left behind")
		}
		return 1
	}
	return code
}

// ls is silo ls (grain.md §2.7): the sessions running under this user,
// from their session directories, filtered by label.
func ls(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("silo ls", flag.ContinueOnError)
	fs.SetOutput(stderr)
	labels := labelFlags{}
	fs.Func("label", "only sessions with this label, `key=value` (repeatable)", labels.set)
	jsonOut := fs.Bool("json", false, "write each session's session.json as a JSON line")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: silo ls [--label key=value ...] [--json]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		if err == nil {
			fs.Usage()
		}
		return exitUsage
	}
	recs, err := session.List(getenv, labels)
	if err != nil {
		fmt.Fprintf(stderr, "silo ls: %v\n", err)
		return 1
	}
	if *jsonOut {
		for _, r := range recs {
			b, _ := json.Marshal(r)
			fmt.Fprintln(stdout, string(b))
		}
		return exitOK
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tPID\tSTARTED\tNAME\tCLI\tDETACHED\tLABELS")
	for _, r := range recs {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%t\t%s\n", r.ID, r.PID, r.Started.Local().Format(time.DateTime), r.Name, r.CLI, r.Detached, labelFlags(r.Labels))
	}
	tw.Flush()
	return exitOK
}
