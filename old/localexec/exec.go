package localexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bwsalmon/silo/old/credentials"
	"github.com/bwsalmon/silo/old/gateway"
	"github.com/bwsalmon/silo/old/internal/headtail"
	"github.com/bwsalmon/silo/old/internal/procgroup"
	"github.com/bwsalmon/silo/old/mcp"
	"github.com/bwsalmon/silo/old/silofile"
)

// Config configures an Executor.
type Config struct {
	Scripts []Script
	// Workdir is where every script runs (local.workdir).
	Workdir string
	// Environ is every script's environment, already stripped of SILO_*
	// and of credentials (BaseEnviron).
	Environ []string
	// Env adds variables per script name: credentials delivered
	// {local: <script>, env: NAME}.
	Env map[string]map[string]string
	// OutputCap bounds stdout and stderr each; DefaultOutputCap when 0.
	OutputCap int
	// KillGrace replaces the KillGrace constant.
	KillGrace time.Duration
	// Approver answers approve: true scripts' calls. Without one they
	// are refused.
	Approver Approver
}

// Executor runs scripts for the gateway. Close ends any still running.
type Executor struct {
	cfg    Config
	ctx    context.Context
	cancel context.CancelFunc
	seq    atomic.Int64

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

// New returns an Executor.
func New(cfg Config) *Executor {
	if cfg.OutputCap <= 0 {
		cfg.OutputCap = DefaultOutputCap
	}
	if cfg.KillGrace <= 0 {
		cfg.KillGrace = KillGrace
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Executor{cfg: cfg, ctx: ctx, cancel: cancel}
}

// Close ends running scripts (SIGTERM to each process group, SIGKILL
// after the grace period) and waits for them.
func (e *Executor) Close() error {
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	e.cancel()
	e.wg.Wait()
	return nil
}

// Result is a call's result (design §12.3). The fields that fit in the
// audit log's head come first.
type Result struct {
	ExitCode int `json:"exit_code"`
	// Signal is set when the script was killed by a signal.
	Signal   string `json:"signal,omitempty"`
	TimedOut bool   `json:"timed_out,omitempty"`
	Duration string `json:"duration"`
	// Truncated says output passed output_cap; StdoutBytes and
	// StderrBytes are how much the script wrote, so the difference from
	// what is kept is what was dropped.
	Truncated   bool   `json:"truncated"`
	StdoutBytes int64  `json:"stdout_bytes"`
	StderrBytes int64  `json:"stderr_bytes"`
	Stdout      string `json:"stdout"`
	Stderr      string `json:"stderr"`
}

var inputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"properties": map[string]any{
		"args": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"description": "The script's arguments, passed exactly as given: no shell sees them.",
		},
		"stdin": map[string]any{"type": "string", "description": "Given to the script on standard input; it gets none otherwise."},
	},
	"required": []string{"args"},
}

// Tools returns one gateway tool per script.
func (e *Executor) Tools() []gateway.Tool {
	out := make([]gateway.Tool, 0, len(e.cfg.Scripts))
	for _, s := range e.cfg.Scripts {
		// The gateway's bound is a backstop above the script's own: an
		// approval may take its whole timeout, then the run its whole
		// timeout and the kill grace.
		backstop := s.Timeout + e.cfg.KillGrace + time.Minute
		if s.Approve {
			backstop += s.Timeout
		}
		out = append(out, gateway.Tool{
			Name:        s.Tool,
			Description: s.Description,
			InputSchema: inputSchema,
			Timeout:     backstop,
			Handler: func(ctx context.Context, args map[string]any) (mcp.Result, error) {
				return e.call(ctx, s, args)
			},
		})
	}
	return out
}

func parseArgs(m map[string]any) (argv []string, stdin *string, err error) {
	for k := range m {
		if k != "args" && k != "stdin" {
			return nil, nil, gateway.Refusef("unknown argument %q: the arguments are args and stdin", k)
		}
	}
	raw, ok := m["args"]
	if !ok {
		return nil, nil, gateway.Refusef(`argument "args" is required (an empty list for none)`)
	}
	list, ok := raw.([]any)
	if raw != nil && !ok {
		return nil, nil, gateway.Refusef(`argument "args" must be a list of strings`)
	}
	for i, a := range list {
		s, ok := a.(string)
		if !ok {
			return nil, nil, gateway.Refusef("args[%d] must be a string", i)
		}
		if strings.IndexByte(s, 0) >= 0 {
			return nil, nil, gateway.Refusef("args[%d] contains a NUL byte, which no argument can hold", i)
		}
		argv = append(argv, s)
	}
	if v, ok := m["stdin"]; ok {
		s, ok := v.(string)
		if !ok {
			return nil, nil, gateway.Refusef(`argument "stdin" must be a string`)
		}
		stdin = &s
	}
	return argv, stdin, nil
}

func (e *Executor) call(ctx context.Context, s Script, m map[string]any) (mcp.Result, error) {
	args, stdin, err := parseArgs(m)
	if err != nil {
		return mcp.Result{}, err
	}
	if err := s.Verify(); err != nil {
		return mcp.Result{}, &gateway.Refusal{Reason: err.Error()}
	}
	if s.Approve {
		if err := e.approve(ctx, s, args, stdin); err != nil {
			return mcp.Result{}, err
		}
		// The approval may have waited a while.
		if err := s.Verify(); err != nil {
			return mcp.Result{}, &gateway.Refusal{Reason: err.Error()}
		}
	}
	res, err := e.run(ctx, s, args, stdin)
	b, merr := json.MarshalIndent(res, "", "  ")
	if merr != nil {
		return mcp.Result{}, merr
	}
	return mcp.Result{Text: string(b)}, err
}

func (e *Executor) approve(ctx context.Context, s Script, args []string, stdin *string) error {
	if e.cfg.Approver == nil {
		return fmt.Errorf("%s needs the user's approval, and this session has no way to ask for it", s.Tool)
	}
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	dl, _ := ctx.Deadline()
	r := Request{ID: strconv.FormatInt(e.seq.Add(1), 10), Tool: s.Tool, Script: s.Name, Args: args, Deadline: dl}
	if r.Args == nil {
		r.Args = []string{}
	}
	if stdin != nil {
		r.Stdin = len(*stdin)
	}
	ok, err := e.cfg.Approver.Approve(ctx, r)
	switch {
	case err != nil && ctx.Err() == context.DeadlineExceeded:
		// Not context.DeadlineExceeded itself: the script never ran, so
		// this is a failed call, not a timed-out one.
		return fmt.Errorf("%s was not approved within %s (call %s), so it did not run", s.Tool, s.Timeout, r.ID)
	case err != nil:
		return fmt.Errorf("%s was not approved (call %s): %w", s.Tool, r.ID, err)
	case !ok:
		return fmt.Errorf("%s did not run (call %s): %w", s.Tool, r.ID, ErrDenied)
	}
	return nil
}

// run executes the script with exactly argv, no shell (design §12.3).
func (e *Executor) run(ctx context.Context, s Script, args []string, stdin *string) (Result, error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return Result{}, errors.New("the session is ending")
	}
	e.wg.Add(1)
	e.mu.Unlock()
	defer e.wg.Done()
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()

	cmd := &exec.Cmd{
		Path: s.Path,
		Args: append([]string{s.Path}, args...),
		Env:  e.environ(s.Name),
		Dir:  e.cfg.Workdir,
	}
	if stdin != nil {
		cmd.Stdin = strings.NewReader(*stdin)
	}
	stdout, stderr := headtail.New(e.cfg.OutputCap), headtail.New(e.cfg.OutputCap)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	procgroup.DieWithParent(cmd)
	// Output still held open by something the script left running in
	// the background does not keep the call waiting.
	cmd.WaitDelay = time.Second

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("%s did not start: %w", s.Name, err)
	}
	pgid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var waitErr error
	ended := ""
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		ended = "cancelled"
		if ctx.Err() == context.DeadlineExceeded {
			ended = "timeout"
		}
	case <-e.ctx.Done():
		ended = "session"
	}
	if ended != "" {
		waitErr = e.kill(pgid, done)
	}
	res := Result{
		ExitCode:    -1,
		Duration:    time.Since(start).Round(time.Millisecond).String(),
		StdoutBytes: stdout.Total(),
		StderrBytes: stderr.Total(),
		Stdout:      string(stdout.Bytes()),
		Stderr:      string(stderr.Bytes()),
	}
	res.Truncated = stdout.Dropped() > 0 || stderr.Dropped() > 0
	if st := cmd.ProcessState; st != nil {
		res.ExitCode = st.ExitCode()
		if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			res.Signal = ws.Signal().String()
		}
	}
	switch ended {
	case "timeout":
		res.TimedOut = true
		return res, fmt.Errorf("%s timed out after %s; its process group was killed: %w", s.Name, s.Timeout, context.DeadlineExceeded)
	case "session":
		return res, errors.New("the session ended while the script ran; its process group was killed")
	case "cancelled":
		return res, fmt.Errorf("the call was cancelled while the script ran; its process group was killed: %w", ctx.Err())
	}
	if waitErr != nil && cmd.ProcessState == nil {
		return res, waitErr
	}
	// A non-zero exit is a normal result (design §12.3).
	return res, nil
}

// kill ends a script's process group: SIGTERM, then SIGKILL once the
// grace period passes with anything in the group still alive, and
// returns the script's wait error.
func (e *Executor) kill(pgid int, done <-chan error) error {
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	grace := time.NewTimer(e.cfg.KillGrace)
	defer grace.Stop()
	var waitErr error
	exited := false
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case waitErr = <-done:
			exited = true
		case <-tick.C:
		case <-grace.C:
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			if !exited {
				waitErr = <-done
			}
			return waitErr
		}
		// The script itself may have gone at SIGTERM while a child that
		// ignores it lives on in the group.
		if exited && syscall.Kill(-pgid, 0) == syscall.ESRCH {
			return waitErr
		}
	}
}

func (e *Executor) environ(script string) []string {
	env := append([]string(nil), e.cfg.Environ...)
	add := e.cfg.Env[script]
	if len(add) == 0 {
		return env
	}
	keys := make([]string, 0, len(add))
	for k := range add {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := env[:0]
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := add[k]; !ok {
			out = append(out, kv)
		}
	}
	for _, k := range keys {
		out = append(out, k+"="+add[k])
	}
	return out
}

// BaseEnviron is the scripts' environment (design §12.3): the invoking
// environment minus SILO_* and minus every credential silo resolved,
// whether named by its {env:} source or found by its value.
func BaseEnviron(environ []string, creds []silofile.Credential, values map[string]credentials.Value) []string {
	names := map[string]bool{}
	for _, c := range creds {
		if c.From.Env != "" {
			names[c.From.Env] = true
		}
	}
	secrets := map[string]bool{}
	for _, v := range values {
		if v.Secret != "" {
			secrets[v.Secret] = true
		}
	}
	var out []string
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "SILO_") || names[k] || secrets[v] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Delivered is the credentials delivered {local: <script>, env: NAME},
// by script and variable.
func Delivered(creds []silofile.Credential, values map[string]credentials.Value) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	for _, c := range creds {
		for _, d := range c.To {
			if d.Local == "" {
				continue
			}
			v, ok := values[c.Name]
			if !ok {
				return nil, fmt.Errorf("credential %q was not resolved", c.Name)
			}
			if v.SilosOwn {
				return nil, fmt.Errorf("credential %q is silo's own (%s) and is never delivered", c.Name, v.Source)
			}
			if out[d.Local] == nil {
				out[d.Local] = map[string]string{}
			}
			out[d.Local][d.Env] = v.Secret
		}
	}
	return out, nil
}

// Workdir is local.workdir resolved against the silo folder; the folder
// itself by default.
func Workdir(f *silofile.Silofile) string {
	if f.Local == nil || f.Local.Workdir == "" {
		return f.Folder
	}
	if filepath.IsAbs(f.Local.Workdir) {
		return f.Local.Workdir
	}
	return filepath.Join(f.Folder, f.Local.Workdir)
}

// OutputCap is local.output_cap, or DefaultOutputCap.
func OutputCap(f *silofile.Silofile) int {
	if f.Local == nil || f.Local.OutputCap == nil || *f.Local.OutputCap <= 0 {
		return DefaultOutputCap
	}
	return int(*f.Local.OutputCap)
}
