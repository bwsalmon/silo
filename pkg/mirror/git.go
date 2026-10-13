package mirror

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bwsalmon/silo/pkg/backend/local/gitstore"
)

// The files of keys/mirror/: the credential, and for SSH the pinned
// host keys.
const (
	credentialFile = "credential"
	knownHostsFile = "known_hosts"
)

// Kinds of credential, by the URL's scheme.
const (
	CredentialSSH   = "ssh"
	CredentialHTTPS = "https"
)

// credentialKind is the kind of credential url needs: ssh for ssh://
// and scp-style user@host:path URLs, https for http(s)://.
func credentialKind(u string) (string, error) {
	if strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
		if _, err := url.Parse(u); err != nil {
			return "", err
		}
		return CredentialHTTPS, nil
	}
	if strings.HasPrefix(u, "ssh://") {
		return CredentialSSH, nil
	}
	if host, _, ok := strings.Cut(u, ":"); ok && strings.Contains(host, "@") && !strings.Contains(host, "/") {
		return CredentialSSH, nil
	}
	return "", fmt.Errorf("mirror: %q is neither an https:// nor an ssh URL", u)
}

// sshHost is the host and port an ssh URL dials.
func sshHost(u string) (host, port string, err error) {
	if strings.HasPrefix(u, "ssh://") {
		p, err := url.Parse(u)
		if err != nil {
			return "", "", err
		}
		return p.Hostname(), p.Port(), nil
	}
	h, _, _ := strings.Cut(u, ":")
	_, h, _ = strings.Cut(h, "@")
	return h, "", nil
}

// gitError is a failed git command: what it printed, and its exit.
type gitError struct {
	args   []string
	stderr string
	err    error
}

func (e *gitError) Error() string {
	msg := strings.TrimSpace(e.stderr)
	if msg == "" {
		msg = e.err.Error()
	}
	return fmt.Sprintf("git %s: %s", e.args[0], msg)
}

func (e *gitError) Unwrap() error { return e.err }

// git runs git in dir with silod's scrubbed environment plus env. An
// HTTPS_PROXY in silod's own environment is kept, for hosts behind a
// corporate proxy.
func (m *Mirror) git(ctx context.Context, dir string, env []string, stdin []byte, args ...string) (string, error) {
	bin := m.opts.Git
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(gitstore.Env(), env...)
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"} {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("%w (%v)", ctx.Err(), err)
		}
		return out.String(), &gitError{args: args, stderr: stderr.String(), err: err}
	}
	return out.String(), nil
}

// remoteEnv is the environment and options git needs to reach the uber
// repo with the credential in keys, and nothing of the operator's: an
// ssh key through GIT_SSH_COMMAND, with only the pinned host keys; a
// token through a credential helper that reads the file.
func remoteEnv(kind, keys string) (env, opts []string) {
	cred := filepath.Join(keys, credentialFile)
	if kind == CredentialSSH {
		ssh := fmt.Sprintf("ssh -i %s -o IdentitiesOnly=yes -o IdentityAgent=none -o UserKnownHostsFile=%s -o StrictHostKeyChecking=yes -o BatchMode=yes",
			shellQuote(cred), shellQuote(filepath.Join(keys, knownHostsFile)))
		return []string{"GIT_SSH_COMMAND=" + ssh}, nil
	}
	helper := fmt.Sprintf(`!f() { test "$1" = get && printf 'username=x-access-token\npassword=%%s\n' "$(cat %s)"; }; f`, shellQuote(cred))
	return nil, []string{"-c", "credential.helper=", "-c", "credential.helper=" + helper}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// update is one ref a push changes: to Src (a SHA), or deleted if Src
// is "". Lease is the SHA it must have now, or "" for none at all.
type update struct {
	Dst   string
	Src   string
	Lease string
	// Local is the repo's own ref it mirrors, for mirror_refs.
	Local string
	// NoLease pushes without one: the staging ref of a chunked push,
	// which is silod's alone.
	NoLease bool
}

// pushResult is a push's outcome, ref by ref.
type pushResult struct {
	stale    []string          // the refs whose lease failed
	rejected map[string]string // refs the remote refused, with its reason
}

// errStale is a push that failed only because some leases did.
var errStale = errors.New("mirror: a lease failed")

// push makes one atomic push of us from the repo at dir to the uber
// repo, with the credential in keys.
func (m *Mirror) push(ctx context.Context, cfg *config, keys, dir string, us []update) (*pushResult, error) {
	env, opts := remoteEnv(cfg.Credential, keys)
	args := append(opts, "push", "--atomic", "--porcelain", "--no-verify")
	for _, u := range us {
		if !u.NoLease {
			args = append(args, "--force-with-lease="+u.Dst+":"+u.Lease)
		}
	}
	args = append(args, cfg.URL)
	for _, u := range us {
		spec := u.Src + ":" + u.Dst
		if u.NoLease && u.Src != "" {
			spec = "+" + spec
		}
		args = append(args, spec)
	}
	ctx, cancel := context.WithTimeout(ctx, m.opts.PushTimeout)
	defer cancel()
	out, err := m.git(ctx, dir, env, nil, args...)
	res := &pushResult{rejected: map[string]string{}}
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(line, "\t", 3)
		if len(f) < 3 || f[0] != "!" {
			continue
		}
		_, dst, _ := strings.Cut(f[1], ":")
		switch {
		case strings.Contains(f[2], "stale info"):
			res.stale = append(res.stale, dst)
		case strings.Contains(f[2], "atomic push failed"):
		default:
			res.rejected[dst] = f[2]
		}
	}
	if err == nil {
		return res, nil
	}
	if len(res.stale) > 0 && len(res.rejected) == 0 {
		return res, errStale
	}
	if len(res.rejected) > 0 {
		var reasons []string
		for ref, why := range res.rejected {
			reasons = append(reasons, ref+": "+why)
		}
		return res, fmt.Errorf("the uber repo refused the push: %s (%v)", strings.Join(reasons, "; "), err)
	}
	return res, err
}

// lsRemote reads the uber repo's refs matching patterns.
func (m *Mirror) lsRemote(ctx context.Context, cfg *config, keys string, patterns ...string) (map[string]string, error) {
	env, opts := remoteEnv(cfg.Credential, keys)
	args := append(opts, "ls-remote", cfg.URL)
	args = append(args, patterns...)
	ctx, cancel := context.WithTimeout(ctx, m.opts.PushTimeout)
	defer cancel()
	if err := m.ensureQuarantine(ctx); err != nil {
		return nil, err
	}
	out, err := m.git(ctx, m.quarantine(), env, nil, args...)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if sha, name, ok := strings.Cut(line, "\t"); ok {
			refs[name] = sha
		}
	}
	return refs, nil
}

// quarantine is mirror/quarantine.git: the one repo the mirror fetches
// into, and only external tips it is about to archive; the probe's
// commits are made there too (local-git.md, "When the uber repo
// disagrees").
func (m *Mirror) quarantine() string { return filepath.Join(m.opts.Dir, "quarantine.git") }

func (m *Mirror) ensureQuarantine(ctx context.Context) error {
	q := m.quarantine()
	if _, err := os.Stat(filepath.Join(q, "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(m.opts.Dir, 0o700); err != nil {
		return err
	}
	_, err := m.git(ctx, "", nil, nil, "init", "--quiet", "--bare", q)
	return err
}
