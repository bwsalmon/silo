// Package credentials resolves a Silofile's credentials from their
// sources and works out where each is delivered (design §8).
//
// Sources here are {env}, {file} and {ambient: gh | git-credential |
// gcloud}; {store} (the OS keyring) comes in a later stage. Every
// resolved value is registered with a Registrar, the audit log, so that
// it is redacted wherever it would be written (§8.4).
package credentials

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/v2/silofile"
)

// DefaultHost is the host the ambient gh and git-credential sources ask
// for a token for.
const DefaultHost = "github.com"

// Forget drops a cached ambient token for credential c on host, after
// the upstream refused it, so the next request asks its source again.
func (r *Resolver) Forget(c silofile.Credential, host string) {
	if c.From.Ambient == "" {
		return
	}
	if host == "" {
		host = r.host()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, c.From.Ambient+"\x00"+host)
}

// AmbientTTL is how long an ambient token is reused before its source
// is asked again (grain's ambient.go).
const AmbientTTL = 5 * time.Minute

// ambientCommandTimeout bounds one gh, git or gcloud invocation, so a
// helper that wants to prompt cannot hang a session.
const ambientCommandTimeout = 15 * time.Second

// ErrStoreUnsupported is returned for {store:} sources.
var ErrStoreUnsupported = errors.New("the {store:} credential source (the OS keyring, design §8.1) is not yet supported: use {env:} or {file:} for now")

// Registrar is told every resolved value, for redaction. The audit
// log's *audit.Log is one.
type Registrar interface {
	Register(name, value string)
}

// Value is one resolved credential.
type Value struct {
	Name   string
	Secret string
	// Source says where it came from, for messages: "env FOO",
	// "file ~/x", "ambient gh".
	Source string
	// SilosOwn is set for {ambient: gcloud}, which silo uses itself and
	// never delivers anywhere.
	SilosOwn bool
}

// Resolver resolves credential sources. The zero value reads the real
// environment, files and commands.
type Resolver struct {
	// Folder is the silo folder, against which a relative {file:} path
	// is read.
	Folder string
	// Host is the host the ambient gh and git-credential sources ask
	// about; DefaultHost when empty.
	Host string
	// Getenv reads the invoking environment; os.Getenv when nil.
	Getenv func(string) string
	// Command runs an ambient source's command; runCommand when nil.
	Command func(ctx context.Context, stdin string, name string, args ...string) (stdout, stderr string, err error)
	// Now is the clock; time.Now when nil.
	Now func() time.Time

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	value   string
	expires time.Time
}

func (r *Resolver) getenv(k string) string {
	if r.Getenv != nil {
		return r.Getenv(k)
	}
	return os.Getenv(k)
}

func (r *Resolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Resolver) host() string {
	if r.Host != "" {
		return r.Host
	}
	return DefaultHost
}

// ResolveAll resolves every credential, registering each value with reg
// (when not nil), and fails on the first source that is missing, before
// anything is created (design §3, step 2).
func (r *Resolver) ResolveAll(ctx context.Context, creds []silofile.Credential, reg Registrar) (map[string]Value, error) {
	out := make(map[string]Value, len(creds))
	for _, c := range creds {
		v, err := r.Resolve(ctx, c)
		if err != nil {
			return nil, err
		}
		if reg != nil {
			reg.Register(v.Name, v.Secret)
		}
		out[c.Name] = v
	}
	return out, nil
}

// Resolve resolves one credential, asking an ambient source about
// Host.
func (r *Resolver) Resolve(ctx context.Context, c silofile.Credential) (Value, error) {
	return r.ResolveHost(ctx, c, "")
}

// ResolveHost resolves one credential for a git host: an ambient source
// asks for a login to host (Host when empty), and the other sources do
// not depend on it. The git proxy asks once per request, for the host
// of the repository it is for (design §9.1).
func (r *Resolver) ResolveHost(ctx context.Context, c silofile.Credential, host string) (Value, error) {
	if host == "" {
		host = r.host()
	}
	v := Value{Name: c.Name}
	var err error
	switch s := c.From; {
	case s.Env != "":
		v.Source = "env " + s.Env
		v.Secret = r.getenv(s.Env)
		if v.Secret == "" {
			err = fmt.Errorf("the environment variable %s is not set", s.Env)
		}
	case s.File != "":
		v.Source = "file " + s.File
		v.Secret, err = r.readFile(s.File)
	case s.Store != "":
		v.Source = "store " + s.Store
		err = ErrStoreUnsupported
	case s.Ambient != "":
		v.Source = "ambient " + s.Ambient
		v.SilosOwn = s.Ambient == "gcloud"
		v.Secret, err = r.ambient(ctx, s.Ambient, host)
	default:
		err = errors.New("it has no source")
	}
	if err != nil {
		return Value{}, fmt.Errorf("credential %q: %w", c.Name, err)
	}
	return v, nil
}

// readFile reads a {file:} source, refusing one that other users can
// read, and drops a trailing newline.
func (r *Resolver) readFile(p string) (string, error) {
	path := p
	switch {
	case path == "~" || strings.HasPrefix(path, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	case !filepath.IsAbs(path):
		path = filepath.Join(r.Folder, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", p)
	}
	if perm := fi.Mode().Perm(); perm&0o044 != 0 {
		return "", fmt.Errorf("%s is readable by other users (mode %04o): run chmod go-rwx %s", p, perm, p)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if s == "" {
		return "", fmt.Errorf("%s is empty", p)
	}
	return s, nil
}
