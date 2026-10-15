// Adapted from github.com/bwsalmon/grain pkg/gitproxy/ambient.go at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

package credentials

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strings"

	"github.com/bwsalmon/silo/v2/internal/procgroup"
)

// ambient returns a token from the invoking user's own login, asking the
// source again once AmbientTTL has passed. A failure is not cached.
func (r *Resolver) ambient(ctx context.Context, source, host string) (string, error) {
	key := source + "\x00" + host
	r.mu.Lock()
	if c, ok := r.cache[key]; ok && r.now().Before(c.expires) {
		r.mu.Unlock()
		return c.value, nil
	}
	r.mu.Unlock()
	var token string
	var err error
	switch source {
	case "gh":
		token, err = r.ghToken(ctx, host)
	case "git-credential":
		token, err = r.gitCredentialToken(ctx, host)
	case "gcloud":
		token, err = r.gcloudToken(ctx)
	default:
		err = fmt.Errorf("unknown ambient source %q: gh, git-credential or gcloud", source)
	}
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	if r.cache == nil {
		r.cache = map[string]cached{}
	}
	r.cache[key] = cached{value: token, expires: r.now().Add(AmbientTTL)}
	r.mu.Unlock()
	return token, nil
}

func (r *Resolver) command(ctx context.Context, stdin, name string, args ...string) (string, string, error) {
	if r.Command != nil {
		return r.Command(ctx, stdin, name, args...)
	}
	return runCommand(ctx, stdin, name, args...)
}

// runCommand runs an ambient source's command with the user's own HOME,
// non-interactively, bounded by ambientCommandTimeout.
func runCommand(ctx context.Context, stdin, name string, args ...string) (string, string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", "", fmt.Errorf("%s is not installed for %s", name, account())
	}
	ctx, cancel := context.WithTimeout(ctx, ambientCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	procgroup.Prepare(cmd)
	cmd.Env = append(os.Environ(),
		"GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1",
		"GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never", "GIT_ASKPASS=", "SSH_ASKPASS=",
		"CLOUDSDK_CORE_DISABLE_PROMPTS=1")
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	return stdout.String(), stderr.String(), err
}

func (r *Resolver) ghToken(ctx context.Context, host string) (string, error) {
	stdout, stderr, err := r.command(ctx, "", "gh", "auth", "token", "--hostname", host)
	if err != nil {
		return "", fmt.Errorf("{ambient: gh}: gh is not logged in to %s (%s): run `gh auth login --hostname %s` as %s",
			host, short(strings.TrimSpace(stderr)+" "+err.Error()), host, account())
	}
	token := strings.TrimSpace(stdout)
	if token == "" || strings.ContainsAny(token, " \n\t") {
		return "", fmt.Errorf("{ambient: gh}: gh printed no token for %s: run `gh auth login --hostname %s` as %s", host, host, account())
	}
	return token, nil
}

func (r *Resolver) gitCredentialToken(ctx context.Context, host string) (string, error) {
	stdout, _, err := r.command(ctx, "protocol=https\nhost="+host+"\n\n", "git", "credential", "fill")
	password := ""
	for _, line := range strings.Split(stdout, "\n") {
		if v, ok := strings.CutPrefix(line, "password="); ok {
			password = strings.TrimSpace(v)
		}
	}
	if err != nil || password == "" {
		return "", fmt.Errorf("{ambient: git-credential}: git credential fill returned no password for https://%s: "+
			"configure a credential helper with a login for %s as %s (for example `gh auth setup-git`)", host, host, account())
	}
	return password, nil
}

func (r *Resolver) gcloudToken(ctx context.Context) (string, error) {
	stdout, stderr, err := r.command(ctx, "", "gcloud", "auth", "print-access-token")
	token := strings.TrimSpace(stdout)
	if err != nil || token == "" || strings.ContainsAny(token, " \n\t") {
		msg := strings.TrimSpace(stderr)
		if err != nil {
			msg += " " + err.Error()
		}
		return "", fmt.Errorf("{ambient: gcloud}: gcloud printed no access token (%s): run `gcloud auth login` as %s", short(msg), account())
	}
	return token, nil
}

// account names the invoking user, for messages saying whose login is
// missing.
func account() string {
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	if name == "" {
		name = "this user"
	}
	host, _ := os.Hostname()
	if host == "" {
		return name
	}
	return name + " on " + host
}

func short(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
