package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bwsalmon/silo/pkg/client"
)

// The agent credentials (agents.md, §6, SV-108), root members only:
// silo credential add|login|list|remove. A secret goes up once and is
// never read back.

// loginFlows are each provider's harness login: the command that logs
// in, and the files under its home that hold the login.
var loginFlows = map[string]struct {
	argv  []string
	files []string
}{
	"anthropic": {argv: []string{"claude", "setup-token"}, files: []string{".claude/.credentials.json", ".claude.json"}},
	"openai":    {argv: []string{"codex", "login"}, files: []string{".codex/auth.json"}},
}

// loginCommand runs a login flow; tests replace it.
var loginCommand = func(ctx context.Context, home string, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = home, stdin, stdout, stderr
	return cmd.Run()
}

func maxSessionsFlag(fs interface {
	Int(string, int, string) *int
}) *int {
	return fs.Int("max-sessions", -1, "how many sessions may use it at once; 0 is no cap (default: none for api_key, 2 for subscription)")
}

func (c *cli) credentialAdd(ctx context.Context, args []string) error {
	fs := c.flags("credential add")
	provider := fs.String("provider", "", "the model provider: anthropic or openai")
	kind := fs.String("kind", "api_key", "api_key (the key is read from stdin)")
	max := maxSessionsFlag(fs)
	m := mutation(fs)
	pos, err := c.parse(fs, args, 1, 1, "<name> --provider anthropic|openai --kind api_key < key")
	if err != nil {
		return err
	}
	if *kind != "api_key" {
		return usagef("--kind %s: add takes an api_key; a subscription is silo credential login", *kind)
	}
	if c.stdin == nil {
		return usagef("give the key on stdin")
	}
	b, err := io.ReadAll(io.LimitReader(c.stdin, 64<<10))
	if err != nil {
		return err
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		return usagef("give the key on stdin")
	}
	req := client.CredentialRequest{Name: pos[0], Provider: *provider, Kind: *kind, Secret: key}
	if *max >= 0 {
		req.MaxSessions = max
	}
	return c.addCredential(ctx, req, *m)
}

func (c *cli) addCredential(ctx context.Context, req client.CredentialRequest, m client.Mutation) error {
	out, err := c.api.AddCredential(ctx, req, m)
	if err != nil {
		return err
	}
	return c.print(out, func(w *tw) {
		fmt.Fprintf(w, "credential %s: %s %s, max_sessions %d\n", out.Name, out.Provider, out.Kind, out.MaxSessions)
	})
}

// credentialLogin runs the provider's harness login in a fresh home,
// or takes the login files from --from, and stores them as a
// subscription. The home is removed afterwards: the files are kept only
// in silod's database, sealed.
func (c *cli) credentialLogin(ctx context.Context, args []string) error {
	fs := c.flags("credential login")
	provider := fs.String("provider", "", "the model provider: anthropic or openai")
	from := fs.String("from", "", "take the login files from this home directory instead of running the login")
	max := maxSessionsFlag(fs)
	m := mutation(fs)
	pos, err := c.parse(fs, args, 1, 1, "<name> --provider anthropic|openai [--from <home>]")
	if err != nil {
		return err
	}
	flow, ok := loginFlows[*provider]
	if !ok {
		return usagef("--provider %q: want anthropic or openai", *provider)
	}
	home := *from
	if home == "" {
		if home, err = os.MkdirTemp("", "silo-login-"); err != nil {
			return err
		}
		defer os.RemoveAll(home)
		if err := loginCommand(ctx, home, flow.argv, c.stdin, c.stderr, c.stderr); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(flow.argv, " "), err)
		}
	}
	files := map[string]string{}
	for _, p := range flow.files {
		b, err := os.ReadFile(filepath.Join(home, filepath.FromSlash(p)))
		if errors.Is(err, iofs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		files[p] = base64.StdEncoding.EncodeToString(b)
	}
	if len(files) == 0 {
		return fmt.Errorf("the login wrote none of %s under %s", strings.Join(flow.files, ", "), home)
	}
	req := client.CredentialRequest{Name: pos[0], Provider: *provider, Kind: "subscription", Files: files}
	if *max >= 0 {
		req.MaxSessions = max
	}
	return c.addCredential(ctx, req, *m)
}

func (c *cli) credentialList(ctx context.Context, args []string) error {
	fs := c.flags("credential list")
	if _, err := c.parse(fs, args, 0, 0, ""); err != nil {
		return err
	}
	l, err := c.api.Credentials(ctx)
	if err != nil {
		return err
	}
	return c.print(map[string]any{"credentials": l}, func(w *tw) {
		w.row("NAME", "PROVIDER", "KIND", "MAX SESSIONS", "UPDATED")
		for _, cr := range l {
			w.row(cr.Name, cr.Provider, cr.Kind, fmt.Sprint(cr.MaxSessions), cr.UpdateTime.Format("2006-01-02T15:04:05Z07:00"))
		}
	})
}

func (c *cli) credentialRemove(ctx context.Context, args []string) error {
	fs := c.flags("credential remove")
	m := mutation(fs)
	pos, err := c.parse(fs, args, 1, 1, "<name>")
	if err != nil {
		return err
	}
	if err := c.api.RemoveCredential(ctx, pos[0], *m); err != nil {
		return err
	}
	return c.print(map[string]any{"name": pos[0], "deleted": !m.ValidateOnly}, func(w *tw) {
		fmt.Fprintf(w, "removed credential %s\n", pos[0])
	})
}
