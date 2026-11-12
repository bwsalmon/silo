// Package session runs one silo session: it prepares the context
// repository's checkout, starts the MCP router with the built-in and
// repository tool sets behind it, and launches the agent CLI pointed at
// the router.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bwsalmon/silo/internal/adapter"
	"github.com/bwsalmon/silo/internal/approval"
	"github.com/bwsalmon/silo/internal/audit"
	"github.com/bwsalmon/silo/internal/builtin"
	"github.com/bwsalmon/silo/internal/config"
	"github.com/bwsalmon/silo/internal/gitrepo"
	"github.com/bwsalmon/silo/internal/router"
)

// Options configure a session.
type Options struct {
	Config *config.Config
	// Repo is the repository name, relative to the configured root.
	Repo string
	// Branch is the base branch; "" means the repository's default.
	Branch string
	// Approver answers approvals. Run uses the terminal console when nil.
	Approver approval.Approver
	// SelfPath is the silo executable the CLI starts as its MCP server
	// (`silo mcp-bridge`); default os.Executable().
	SelfPath string
	// Status receives silo's own messages (startup summary, load
	// failures); default os.Stderr.
	Status io.Writer
}

// Session is a running session.
type Session struct {
	ID       string
	Dir      string
	Branch   string
	Checkout *gitrepo.Checkout
	Router   *router.Router
	Host     *router.Host
	Scratch  *builtin.Scratch
	Adapter  adapter.Adapter
	Socket   string
	Prompt   string

	opts     Options
	audit    *audit.File
	listener net.Listener
	// socketDir is the private temporary directory holding Socket when
	// the session directory's path is too long for one.
	socketDir string
	cancel    context.CancelFunc
}

// NewID returns a session id: a UTC timestamp and a random suffix.
func NewID() string {
	b := make([]byte, 3)
	rand.Read(b)
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

// Start prepares everything up to launching the CLI.
func Start(ctx context.Context, opts Options) (*Session, error) {
	cfg := opts.Config
	if opts.Status == nil {
		opts.Status = os.Stderr
	}
	if opts.SelfPath == "" {
		self, err := os.Executable()
		if err != nil {
			return nil, err
		}
		opts.SelfPath = self
	}
	ad, err := adapter.Get(cfg.CLI.Name)
	if err != nil {
		return nil, err
	}
	co, err := gitrepo.Prepare(ctx, cfg, opts.Repo, opts.Branch)
	if err != nil {
		return nil, err
	}
	repoCfg, err := config.LoadRepoConfig(co.Dir)
	if err != nil {
		return nil, err
	}

	s := &Session{ID: NewID(), Checkout: co, Adapter: ad, opts: opts}
	s.Dir = filepath.Join(cfg.SessionsDir, s.ID)
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	if s.audit, err = audit.Open(filepath.Join(s.Dir, "audit.jsonl")); err != nil {
		return nil, err
	}
	s.Branch = firstNonEmpty(cfg.Push.Branch, repoCfg.PushBranch, "silo/"+s.ID)
	if err := co.StartBranch(ctx, s.Branch); err != nil {
		s.audit.Close()
		return nil, err
	}
	head, _ := co.Head(ctx)
	s.audit.Log("session_start", map[string]any{
		"session": s.ID, "repo": opts.Repo, "checkout": co.Dir, "base": co.Base,
		"branch": s.Branch, "head": head, "cli": cfg.CLI.Name,
	})

	approver := opts.Approver
	if approver == nil {
		approver = approval.Deny{Reason: "no approver configured"}
	}
	approver = approval.WithAudit(approver, s.audit)

	listChanged := ad.ListChanged()
	switch cfg.CLI.ListChanged {
	case "yes":
		listChanged = true
	case "no":
		listChanged = false
	}

	disabled := map[string]bool{}
	for _, n := range append(append([]string{}, cfg.Toolsets.Disabled...), repoCfg.Disabled...) {
		disabled[n] = true
	}
	s.Router = router.New(approver, s.audit)
	s.Host = &router.Host{Router: s.Router, Checkout: co.Dir, SessionDir: s.Dir, Disabled: disabled, Log: s.audit}
	remote := co.Remote
	if cfg.Push.Disabled {
		remote = ""
	}
	writer := &builtin.Writer{Repo: &co.Repo, Branch: s.Branch, Remote: remote, Identity: cfg.Identity, Approver: approver, Host: s.Host, Log: s.audit}
	s.Scratch = &builtin.Scratch{Dir: filepath.Join(s.Dir, "tools"), Approver: approver, Host: s.Host, Writer: writer, Log: s.audit}
	refresher := &builtin.Refresher{Checkout: co, Branch: s.Branch, Host: s.Host, Log: s.audit}

	for _, b := range []struct {
		name string
		set  router.Toolset
	}{
		{"repo-read", builtin.NewRepoRead(&co.Repo)},
		{"repo-write", builtin.NewRepoWrite(writer)},
		{"scratch", builtin.NewScratch(s.Scratch)},
		{"silo", builtin.NewSilo(refresher, s.Router, !listChanged, &builtin.Vendorer{Config: cfg, Checkout: co.Dir, Writer: writer})},
	} {
		if err := s.Router.Add(ctx, b.name, b.set, router.Options{Origin: router.Builtin}); err != nil {
			s.Close()
			return nil, err
		}
	}
	report, ok := s.Host.LoadAll(ctx)
	if report != "" {
		fmt.Fprintf(opts.Status, "silo: repository tool sets:\n%s\n", indent(report))
	}
	if !ok {
		fmt.Fprintf(opts.Status, "silo: some tool sets failed to load; the session goes on without them\n")
	}

	s.Prompt, err = ComposePrompt(co, opts.Repo, s.Branch, repoCfg, !listChanged)
	if err != nil {
		s.Close()
		return nil, err
	}
	s.Router.Instructions = Instructions(!listChanged)

	if err := s.listen(); err != nil {
		s.Close()
		return nil, err
	}
	fmt.Fprintf(opts.Status, "silo: session %s on %s (branch %s), %s\nsilo: audit log %s\n",
		s.ID, co.Dir, s.Branch, cfg.CLI.Name, filepath.Join(s.Dir, "audit.jsonl"))
	return s, nil
}

// listen opens the router's Unix socket, which `silo mcp-bridge` connects
// the CLI's stdio to.
func (s *Session) listen() error {
	s.Socket = filepath.Join(s.Dir, "router.sock")
	if len(s.Socket) > 100 { // sun_path is 104-108 bytes
		// A directory only this user can enter, so nobody else can
		// connect in the moment before the chmod below.
		dir, err := os.MkdirTemp("", "silo-")
		if err != nil {
			return err
		}
		s.socketDir = dir
		s.Socket = filepath.Join(dir, "router.sock")
	}
	l, err := net.Listen("unix", s.Socket)
	if err != nil {
		return err
	}
	os.Chmod(s.Socket, 0o600)
	s.listener = l
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				s.Router.Serve(ctx, conn, conn)
			}()
		}
	}()
	return nil
}

// Command builds the CLI's command line.
func (s *Session) Command() (*exec.Cmd, error) {
	cfg := s.opts.Config
	l, err := s.Adapter.Prepare(adapter.Spec{
		Binary:       cfg.CLI.Binary,
		Args:         cfg.CLI.Args,
		Env:          cfg.CLI.Env,
		Checkout:     s.Checkout.Dir,
		SessionDir:   s.Dir,
		MCPCommand:   s.opts.SelfPath,
		MCPArgs:      []string{"mcp-bridge", s.Socket},
		SystemPrompt: s.Prompt,
	})
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(l.Path, l.Args...)
	cmd.Dir = l.Dir
	cmd.Env = append(os.Environ(), l.Env...)
	cmd.Env = append(cmd.Env, "SILO_SESSION_ID="+s.ID)
	return cmd, nil
}

// Close stops every tool set, deletes the one-off tools and closes the
// audit log. The checkout and its commits stay.
func (s *Session) Close() {
	if s.listener != nil {
		s.listener.Close()
		os.Remove(s.Socket)
	}
	if s.socketDir != "" {
		os.RemoveAll(s.socketDir)
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.Router != nil {
		s.Router.Close()
	}
	if s.Scratch != nil {
		s.Scratch.Cleanup()
	}
	os.RemoveAll(filepath.Join(s.Dir, "state"))
	if s.audit != nil {
		s.audit.Log("session_end", map[string]any{"session": s.ID})
		s.audit.Close()
	}
}

// Run runs a whole interactive session: Start, the CLI on the person's
// terminal, Close.
func Run(ctx context.Context, opts Options) error {
	console := approval.NewConsole()
	if opts.Approver == nil {
		if !console.IsTerminal() {
			return errors.New("silo run needs a terminal: approvals are read from it")
		}
		opts.Approver = console
	}
	s, err := Start(ctx, opts)
	if err != nil {
		return err
	}
	defer s.Close()
	cmd, err := s.Command()
	if err != nil {
		return err
	}
	if console.IsTerminal() {
		err = console.Run(cmd)
	} else {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		err = cmd.Run()
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return nil // the CLI's own exit status is its business
	}
	return err
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}
