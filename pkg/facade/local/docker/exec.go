package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/control"
	"github.com/bwsalmon/silo/pkg/store"
)

// execInstance is one exec: made by POST /containers/{id}/exec, run
// once by POST /exec/{id}/start (local-backend.md §4, "Exec, logs and
// attach"). Execs are silod's memory, as Docker's are the daemon's: a
// restart forgets them, and so does the container's removal, or five
// minutes after the process ends.
type execInstance struct {
	ID, Silo, Container string
	Process             runtime.Process
	Stdout, Stderr      bool
	DetachKeys          string
	// resize carries the TTY's sizes to the driver, the latest only;
	// it is closed when the process ends.
	resize chan [2]uint16

	// The rest is under Server.execMu.
	started, running bool
	exitCode         *int
	finished         time.Time
}

// execKeep is how long an exec is kept after its process ends.
const execKeep = 5 * time.Minute

// execBody is an exec create's body.
type execBody struct {
	AttachStdin, AttachStdout, AttachStderr bool
	DetachKeys                              string
	Tty                                     bool
	ConsoleSize                             *[2]uint16 // height, width
	Env                                     []string
	Cmd                                     strs
	User, WorkingDir                        string
}

// execCreate is POST /containers/{id}/exec: an exec in a running
// container, which its start runs.
func (s *Server) execCreate(q *request) error {
	b, err := decode[execBody](q)
	if err != nil {
		return s.refuse(q, err)
	}
	c, err := s.get(q)
	if err != nil {
		return err
	}
	switch {
	case len(b.Cmd) == 0:
		return s.refuse(q, invalid("No exec command specified"))
	case b.WorkingDir != "" && !path.IsAbs(b.WorkingDir):
		return s.refuse(q, invalid("the working directory '%s' is invalid, it needs to be an absolute path", b.WorkingDir))
	}
	if _, err := detachKeys(b.DetachKeys); err != nil {
		return s.refuse(q, err)
	}
	if err := running(s.state(q.ctx(), c), c); err != nil {
		return s.refuse(q, err)
	}
	e := &execInstance{ID: newID(), Silo: q.target.ID, Container: c.ID, Stdout: b.AttachStdout, Stderr: b.AttachStderr,
		DetachKeys: b.DetachKeys, resize: make(chan [2]uint16, 1),
		Process: runtime.Process{Args: b.Cmd, Env: b.Env, User: b.User, Workdir: b.WorkingDir, TTY: b.Tty, Stdin: b.AttachStdin}}
	if b.ConsoleSize != nil {
		e.resize <- [2]uint16{b.ConsoleSize[1], b.ConsoleSize[0]}
	}
	s.execMu.Lock()
	for id, o := range s.execs {
		if !o.finished.IsZero() && time.Since(o.finished) > execKeep {
			delete(s.execs, id)
		}
	}
	s.execs[e.ID] = e
	s.execMu.Unlock()
	s.audit(q, "OK", map[string]any{"id": c.ID, "exec": e.ID, "cmd": []string(b.Cmd)}, nil)
	writeJSON(q.w, http.StatusCreated, map[string]string{"Id": e.ID})
	return nil
}

// running refuses a container that isn't running, as Docker's exec
// does.
func running(rc runtime.Container, c *Container) error {
	switch rc.State {
	case runtime.Running:
		return nil
	case runtime.Paused:
		return conflict("Container %s is paused, unpause the container before exec", c.ID)
	}
	return conflict("Container %s is not running", c.ID)
}

// exec is the call's exec, which must be the target's, and its
// container.
func (s *Server) exec(q *request) (*execInstance, *Container, error) {
	s.execMu.Lock()
	e := s.execs[q.params["id"]]
	s.execMu.Unlock()
	if e == nil || e.Silo != q.target.ID {
		return nil, nil, notFound("No such exec instance: %s", q.params["id"])
	}
	var c *Container
	err := s.st.Tx(q.ctx(), func(tx store.Tx) error {
		var err error
		c, err = s.container(q.ctx(), tx, q.target.ID, e.Container)
		return err
	})
	if err == nil && q.class() == control.Mutation {
		err = s.refuseToolContainer(q, c)
	}
	return e, c, err
}

// forgetExecs forgets a removed container's execs.
func (s *Server) forgetExecs(container string) {
	s.execMu.Lock()
	defer s.execMu.Unlock()
	for id, e := range s.execs {
		if e.Container == container {
			delete(s.execs, id)
		}
	}
}

// execStart is POST /exec/{id}/start: the exec's process, run through
// the driver with its stdin, TTY, resizes and exit code. Attached, its
// stdio is the hijacked connection; detached, it runs on its own and
// the call answers at once.
func (s *Server) execStart(q *request) error {
	b, err := decode[struct {
		Detach      bool
		ConsoleSize *[2]uint16
	}](q)
	if err != nil {
		return s.refuse(q, err)
	}
	e, c, err := s.exec(q)
	if err != nil {
		return err
	}
	if err := running(s.state(q.ctx(), c), c); err != nil {
		return s.refuse(q, err)
	}
	s.execMu.Lock()
	switch {
	case e.running:
		s.execMu.Unlock()
		return s.refuse(q, conflict("Error: Exec command %s is already running", e.ID))
	case e.started:
		s.execMu.Unlock()
		return s.refuse(q, conflict("silo: exec %s has already run; make another", e.ID))
	}
	e.started, e.running = true, true
	s.execMu.Unlock()
	if b.ConsoleSize != nil {
		s.resizeExec(e, b.ConsoleSize[1], b.ConsoleSize[0])
	}
	d := s.drivers[c.Class]
	ctx, cancel := context.WithCancel(s.bg)
	if b.Detach {
		s.audit(q, "OK", map[string]any{"id": c.ID, "exec": e.ID, "detach": true}, nil)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer cancel()
			s.runExec(ctx, d, e, runtime.ExecIO{Stdout: io.Discard, Stderr: io.Discard})
		}()
		q.w.WriteHeader(http.StatusOK)
		return nil
	}
	defer cancel()
	conn, in, err := hijack(q, e.Process.TTY)
	if err != nil {
		s.execMu.Lock()
		e.started, e.running = false, false
		s.execMu.Unlock()
		return err
	}
	defer conn.Close()
	s.audit(q, "OK", map[string]any{"id": c.ID, "exec": e.ID}, nil)
	stdio := runtime.ExecIO{Stdout: io.Discard, Stderr: io.Discard}
	stdout, stderr := outputs(conn, e.Process.TTY)
	if e.Stdout {
		stdio.Stdout = stdout
	}
	if e.Stderr {
		stdio.Stderr = stderr
	}
	if e.Process.Stdin {
		stdio.Stdin = in
	}
	s.runExec(ctx, d, e, stdio)
	return nil
}

// runExec runs e's process and records how it ended. A process the
// driver couldn't start ends 126, or 127 if the container has no such
// executable, as a shell reports them, with why on its stderr.
func (s *Server) runExec(ctx context.Context, d runtime.Driver, e *execInstance, stdio runtime.ExecIO) {
	stdio.Resize = e.resize
	code, err := d.Exec(ctx, e.Container, e.Process, stdio)
	if err != nil {
		code = 126
		if noExecutable(0, err) {
			code = 127
		}
		fmt.Fprintf(stdio.Stderr, "silo: exec: %v\n", err)
	}
	s.execMu.Lock()
	defer s.execMu.Unlock()
	e.running, e.exitCode, e.finished = false, &code, time.Now()
	close(e.resize)
}

// noExecutable is whether an exec ended because the container has no
// such executable: 127 from a shell, or containerd's refusal to start
// one.
func noExecutable(code int, err error) bool {
	if err != nil {
		m := err.Error()
		return strings.Contains(m, "executable file not found") || strings.Contains(m, "no such file or directory")
	}
	return code == 127 || code == 126
}

// resizeExec gives e's TTY a new size, replacing one the driver hasn't
// taken yet.
func (s *Server) resizeExec(e *execInstance, cols, rows uint16) {
	s.execMu.Lock()
	defer s.execMu.Unlock()
	if !e.finished.IsZero() {
		return
	}
	select {
	case <-e.resize:
	default:
	}
	select {
	case e.resize <- [2]uint16{cols, rows}:
	default:
	}
}

// execResize is POST /exec/{id}/resize?h=&w=. A size given before
// start is the TTY's first.
func (s *Server) execResize(q *request) error {
	e, _, err := s.exec(q)
	if err != nil {
		return err
	}
	var size [2]uint16
	for i, k := range []string{"w", "h"} {
		n, err := strconv.ParseUint(q.query(k), 10, 16)
		if err != nil {
			return invalid("invalid %s: %q", k, q.query(k))
		}
		size[i] = uint16(n)
	}
	s.execMu.Lock()
	done := !e.finished.IsZero()
	s.execMu.Unlock()
	if done {
		return conflict("silo: exec %s has ended", e.ID)
	}
	s.resizeExec(e, size[0], size[1])
	q.w.WriteHeader(http.StatusOK)
	return nil
}

// execInspect is GET /exec/{id}/json.
func (s *Server) execInspect(q *request) error {
	e, _, err := s.exec(q)
	if err != nil {
		return err
	}
	s.execMu.Lock()
	run, code := e.running, e.exitCode
	s.execMu.Unlock()
	return ok(q, map[string]any{
		"ID": e.ID, "Running": run, "ExitCode": code, "CanRemove": false, "ContainerID": e.Container,
		"DetachKeys": e.DetachKeys, "OpenStdin": e.Process.Stdin, "OpenStdout": e.Stdout, "OpenStderr": e.Stderr, "Pid": 0,
		"ProcessConfig": map[string]any{"tty": e.Process.TTY, "entrypoint": first(e.Process.Args),
			"arguments": rest(e.Process.Args), "privileged": false, "user": e.Process.User},
	})
}

// containerTop is GET /containers/{id}/top: an exec of ps in the
// container, with ps_args (-ef if none), its output parsed into
// Docker's titles and rows. An image with no ps is 501.
func (s *Server) containerTop(q *request) error {
	c, err := s.get(q)
	if err != nil {
		return err
	}
	if err := running(s.state(q.ctx(), c), c); err != nil {
		return err
	}
	args := strings.Fields(q.query("ps_args"))
	if len(args) == 0 {
		args = []string{"-ef"}
	}
	var outb, errb bytes.Buffer
	out, errs := &limitedBuffer{b: &outb, max: 4 << 20}, &limitedBuffer{b: &errb, max: 64 << 10}
	code, err := s.drivers[c.Class].Exec(q.ctx(), c.ID, runtime.Process{Args: append([]string{"ps"}, args...)},
		runtime.ExecIO{Stdout: out, Stderr: errs})
	switch {
	case noExecutable(code, err):
		return unsupported("silo: top runs ps in the container, and its image has none")
	case err != nil:
		return runtimeErr(err, c)
	case code != 0:
		return errorf(http.StatusInternalServerError, "ps: %s", strings.TrimSpace(errb.String()))
	}
	lines := strings.Split(strings.TrimSpace(outb.String()), "\n")
	titles := strings.Fields(lines[0])
	if len(titles) == 0 {
		return errorf(http.StatusInternalServerError, "silo: ps gave no titles")
	}
	procs := [][]string{}
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		// The last column, the command, keeps its spaces.
		if len(f) > len(titles) {
			f = append(f[:len(titles)-1], strings.Join(f[len(titles)-1:], " "))
		}
		procs = append(procs, f)
	}
	return ok(q, map[string]any{"Titles": titles, "Processes": procs})
}
