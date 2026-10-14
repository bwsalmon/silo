package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"

	"github.com/bwsalmon/silo/v2/gateway"
	"github.com/bwsalmon/silo/v2/silofile"
)

// CheckResult is one line of silo run --dry-run's checklist
// (design §2.4).
type CheckResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Check asks the world about one part of a valid Silofile, changing
// nothing.
type Check func(ctx context.Context, f *silofile.Silofile, environ []string) []CheckResult

var checks = map[string]Check{}

// RegisterCheck adds a --dry-run check. Stage 3a's fuller check
// registry (silo_check) replaces this minimal one.
func RegisterCheck(name string, c Check) {
	regMu.Lock()
	defer regMu.Unlock()
	checks[name] = c
}

// DryRun runs every registered check, in name order.
func DryRun(ctx context.Context, f *silofile.Silofile, environ []string) []CheckResult {
	if environ == nil {
		environ = os.Environ()
	}
	regMu.Lock()
	names := make([]string, 0, len(checks))
	for n := range checks {
		names = append(names, n)
	}
	regMu.Unlock()
	sort.Strings(names)
	var out []CheckResult
	for _, n := range names {
		regMu.Lock()
		c := checks[n]
		regMu.Unlock()
		out = append(out, c(ctx, f, environ)...)
	}
	return out
}

// LockFile is where a silo folder records what its last run resolved.
const LockFile = ".silo/lock.json"

// writeLock sets one top-level key of the folder's .silo/lock.json,
// keeping the others.
func writeLock(folder, key string, v any) error {
	if folder == "" {
		return nil
	}
	path := filepath.Join(folder, LockFile)
	doc := map[string]json.RawMessage{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &doc); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	doc[key] = b
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeJSON(path, doc)
}

// ReadLock reads one key of a folder's .silo/lock.json into v.
func ReadLock(folder, key string, v any) error {
	b, err := os.ReadFile(filepath.Join(folder, LockFile))
	if err != nil {
		return err
	}
	doc := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	raw, ok := doc[key]
	if !ok {
		return os.ErrNotExist
	}
	return json.Unmarshal(raw, v)
}

func (s *Session) environ() []string { return s.opts.Environ }

// onGateway runs fn once the gateway exists: a tool provider whose tool
// set changes during the session (the MCP proxy) publishes to it.
func (s *Session) onGateway(fn func(*gateway.Gateway)) {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	s.gatewayHooks = append(s.gatewayHooks, fn)
}

func (s *Session) runGatewayHooks(g *gateway.Gateway) {
	s.hookMu.Lock()
	hooks := s.gatewayHooks
	s.hookMu.Unlock()
	for _, fn := range hooks {
		fn(g)
	}
}
