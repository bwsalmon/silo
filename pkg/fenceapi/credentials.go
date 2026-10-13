package fenceapi

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/agents"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// Agent credentials (agents.md, §6, SV-108): model provider logins, in
// agent_credentials, so that every control VM can use them. The secret
// (an API key, or a subscription's login files) is sealed with
// AES-256-GCM under Options.CredentialKey, silo-credential-key from the
// control VMs' metadata, which only silod can read: the database and its
// backups hold it only encrypted, without the key. Root members manage
// them through the front door, and nobody reads one back: only silod's
// model proxy (the API key) and its session runner (a subscription's
// files) open them.

// The audit kinds of credentials.
const (
	KindCredentialAdd    = "credential.add"
	KindCredentialRemove = "credential.remove"
)

// CredentialSchema is the first schema with agent_credentials and the
// agents' lease_warned.
const CredentialSchema = 18

// DefaultSubscriptionSessions is a subscription's max_sessions when the
// request leaves it out: its rate limits are shared by every session on
// it.
const DefaultSubscriptionSessions = 2

// CredentialInfo is a credential as the API shows it: never its secret.
type CredentialInfo struct {
	Name        string    `json:"name"`
	Provider    string    `json:"provider"`
	Kind        string    `json:"kind"`
	MaxSessions int       `json:"max_sessions"`
	CreateTime  time.Time `json:"create_time"`
	UpdateTime  time.Time `json:"update_time"`
}

// CredentialRequest is POST /silo/v1/credentials's body. Secret is an
// api_key's key; Files are a subscription's login files, by path under
// the harness's home, base64.
type CredentialRequest struct {
	Name        string            `json:"name"`
	Provider    string            `json:"provider"`
	Kind        string            `json:"kind"`
	MaxSessions *int              `json:"max_sessions"`
	Secret      string            `json:"secret,omitempty"`
	Files       map[string]string `json:"files,omitempty"`
}

// The providers credentials may be for: those of the shipped adapters.
func credentialProviders() []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range agents.Adapters {
		for _, p := range a.Providers {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// sealer is the credential key's AEAD.
func (s *Server) sealer() (cipher.AEAD, error) {
	if len(s.opts.CredentialKey) == 0 {
		return nil, errorf(FailedPrecondition, "silod has no credential key: install with --agent-sessions puts silo-credential-key in the control VMs' metadata")
	}
	block, err := aes.NewCipher(s.opts.CredentialKey)
	if err != nil {
		return nil, fmt.Errorf("the credential key: %w", err)
	}
	return cipher.NewGCM(block)
}

// seal encrypts plain for credential name, which is bound in as
// additional data so a sealed secret can't be moved to another name.
func (s *Server) seal(name string, plain []byte) (string, error) {
	g, err := s.sealer()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(g.Seal(nonce, nonce, plain, []byte(name))), nil
}

func (s *Server) open(name, sealed string) ([]byte, error) {
	g, err := s.sealer()
	if err != nil {
		return nil, err
	}
	b, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(b) < g.NonceSize() {
		return nil, fmt.Errorf("credential %s: a malformed secret", name)
	}
	plain, err := g.Open(nil, b[:g.NonceSize()], b[g.NonceSize():], []byte(name))
	if err != nil {
		return nil, fmt.Errorf("credential %s: its secret doesn't open with this credential key", name)
	}
	return plain, nil
}

type credentialRow struct {
	CredentialInfo
	secret string
}

func readCredentials(ctx context.Context, tx store.Tx, where string, args ...any) ([]credentialRow, error) {
	q := `SELECT name, provider, kind, max_sessions, secret, created_at, updated_at FROM agent_credentials`
	if where != "" {
		q += ` WHERE ` + where
	}
	rows, err := tx.Query(ctx, q+` ORDER BY name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []credentialRow
	for rows.Next() {
		var c credentialRow
		var created, updated int64
		if err := rows.Scan(&c.Name, &c.Provider, &c.Kind, &c.MaxSessions, &c.secret, &created, &updated); err != nil {
			return nil, err
		}
		c.CreateTime, c.UpdateTime = store.Time(created), store.Time(updated)
		out = append(out, c)
	}
	return out, rows.Err()
}

// credential is a credential by name: a stored one, or else one of
// Options.Credentials (development's --agent-credentials), which are
// API keys with no cap.
func (s *Server) credential(ctx context.Context, tx store.Tx, name string) (agents.Credential, bool, error) {
	if name == "" {
		return agents.Credential{}, false, nil
	}
	if c, ok := s.ambient(name); ok {
		return c, true, nil
	}
	l, err := readCredentials(ctx, tx, `name = ?`, name)
	if err != nil {
		return agents.Credential{}, false, err
	}
	if len(l) > 0 {
		return agents.Credential{Name: name, Provider: l[0].Provider, Kind: l[0].Kind, MaxSessions: l[0].MaxSessions}, true, nil
	}
	if s.opts.Credentials != nil {
		c, ok, err := s.opts.Credentials.Credential(ctx, name)
		if ok && c.Kind == "" {
			c.Kind = agents.CredentialAPIKey
		}
		return c, ok, err
	}
	return agents.Credential{}, false, nil
}

// ambient is the ambient credential called name, if silod has
// ambient credentials and name is one: ambient-<provider>, for each
// provider an adapter takes. It has no cap of its own; the root's
// agent_sessions is the cap.
func (s *Server) ambient(name string) (agents.Credential, bool) {
	provider, ok := strings.CutPrefix(name, agents.AmbientPrefix)
	if !s.opts.Ambient || !ok || !contains(credentialProviders(), provider) {
		return agents.Credential{}, false
	}
	return agents.Credential{Name: name, Provider: provider, Kind: agents.CredentialAmbient}, true
}

// ambientInfos are the ambient credentials, as the list shows them.
func (s *Server) ambientInfos() []CredentialInfo {
	if !s.opts.Ambient {
		return nil
	}
	var out []CredentialInfo
	for _, p := range credentialProviders() {
		out = append(out, CredentialInfo{Name: agents.AmbientName(p), Provider: p, Kind: agents.CredentialAmbient})
	}
	return out
}

// mustRootMember refuses a caller that isn't a root member at the front
// door: credentials are managed by people, never by an agent's session
// or a VM, even the root's.
func mustRootMember(ctx context.Context, tx store.Tx, c Caller) (*tree.Silo, error) {
	root, err := tree.Root(ctx, tx)
	if err != nil {
		return nil, err
	}
	if c.Silo != root.ID || (c.Via != ViaFrontDoor && c.Via != ViaInternal) {
		return nil, errorf(PermissionDenied, "only the root's members, through the front door, may manage credentials")
	}
	return root, nil
}

// listCredentials is GET /silo/v1/credentials: names, providers, kinds
// and caps, never secrets.
func (s *Server) listCredentials(q *request) {
	ctx := q.r.Context()
	out := []CredentialInfo{}
	err := s.tx(ctx, func(tx store.Tx) error {
		if _, err := mustRootMember(ctx, tx, q.caller); err != nil {
			return err
		}
		l, err := readCredentials(ctx, tx, "")
		if err != nil {
			return err
		}
		for _, c := range l {
			out = append(out, c.CredentialInfo)
		}
		out = append(out, s.ambientInfos()...)
		return nil
	})
	if err != nil {
		writeError(q.w, err)
		return
	}
	writeJSON(q.w, http.StatusOK, map[string]any{"credentials": out})
}

// validLoginPath is a login file's path: relative, under the home, with
// no way out of it.
func validLoginPath(p string) bool {
	return p != "" && !strings.HasPrefix(p, "/") && path.Clean(p) == p && !strings.HasPrefix(p, "../") && p != ".."
}

// addCredential is POST /silo/v1/credentials: a new credential, or a
// new secret for one of the same provider and kind (a fresh login).
func (s *Server) addCredential(q *request) (any, error) {
	ctx := q.r.Context()
	var req CredentialRequest
	if err := decode(q, &req); err != nil {
		return nil, err
	}
	if err := agents.ValidName(req.Name); err != nil {
		return nil, errorf(InvalidArgument, "name: %v", err).with("field", "name")
	}
	if strings.HasPrefix(req.Name, agents.AmbientPrefix) {
		return nil, errorf(InvalidArgument, "name %q: names starting %s are the ambient credentials', which nobody adds", req.Name, agents.AmbientPrefix).with("field", "name")
	}
	if !contains(credentialProviders(), req.Provider) {
		return nil, errorf(InvalidArgument, "provider %q: want one of %s", req.Provider, strings.Join(credentialProviders(), ", ")).with("field", "provider")
	}
	var plain []byte
	switch req.Kind {
	case agents.CredentialAPIKey:
		if strings.TrimSpace(req.Secret) == "" || len(req.Files) > 0 {
			return nil, errorf(InvalidArgument, "an api_key credential needs its key as secret, and no files").with("field", "secret")
		}
		plain = []byte(strings.TrimSpace(req.Secret))
	case agents.CredentialSubscription:
		if len(req.Files) == 0 || req.Secret != "" {
			return nil, errorf(InvalidArgument, "a subscription credential needs its login files, and no secret").with("field", "files")
		}
		files := map[string][]byte{}
		for p, v := range req.Files {
			if !validLoginPath(p) {
				return nil, errorf(InvalidArgument, "login file %q: want a relative path under the harness's home", p).with("field", "files")
			}
			b, err := base64.StdEncoding.DecodeString(v)
			if err != nil {
				return nil, errorf(InvalidArgument, "login file %q: not base64", p).with("field", "files")
			}
			files[p] = b
		}
		plain, _ = json.Marshal(files)
	case agents.CredentialAmbient:
		return nil, errorf(InvalidArgument, "kind ambient: an ambient credential is the agent user's own login on a local backend's host, which silod has without being given it (ambient-<provider>)").with("field", "kind")
	default:
		return nil, errorf(InvalidArgument, "kind %q: want api_key or subscription", req.Kind).with("field", "kind")
	}
	max := 0
	if req.Kind == agents.CredentialSubscription {
		max = DefaultSubscriptionSessions
	}
	if req.MaxSessions != nil {
		max = *req.MaxSessions
	}
	if max < 0 {
		return nil, errorf(InvalidArgument, "max_sessions %d: want 0 (no cap) or more", max).with("field", "max_sessions")
	}
	var out CredentialInfo
	err := s.tx(ctx, func(tx store.Tx) error {
		root, err := mustRootMember(ctx, tx, q.caller)
		if err != nil {
			return err
		}
		q.note(root, KindCredentialAdd)
		sealed, err := s.seal(req.Name, plain)
		if err != nil {
			return err
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		old, err := readCredentials(ctx, tx, `name = ?`, req.Name)
		if err != nil {
			return err
		}
		out = CredentialInfo{Name: req.Name, Provider: req.Provider, Kind: req.Kind, MaxSessions: max, CreateTime: now, UpdateTime: now}
		if len(old) > 0 {
			if old[0].Provider != req.Provider || old[0].Kind != req.Kind {
				return errorf(AlreadyExists, "credential %s is a %s %s; remove it first to make it another", req.Name, old[0].Provider, old[0].Kind)
			}
			out.CreateTime = old[0].CreateTime
		}
		if q.validateOnly {
			return errValidateOnly
		}
		if _, err := tx.Exec(ctx, `INSERT INTO agent_credentials (name, provider, kind, max_sessions, secret, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (name) DO UPDATE SET max_sessions = excluded.max_sessions, secret = excluded.secret, updated_at = excluded.updated_at`,
			req.Name, req.Provider, req.Kind, max, sealed, store.Millis(out.CreateTime), store.Millis(now)); err != nil {
			return err
		}
		return Audit(ctx, tx, root, q.caller, KindCredentialAdd, q.requestID, "OK", map[string]any{
			"credential": req.Name, "provider": req.Provider, "credential_kind": req.Kind, "max_sessions": max, "replaced": len(old) > 0,
		})
	})
	if err != nil && !errors.Is(err, errValidateOnly) {
		return nil, err
	}
	return out, nil
}

// removeCredential is DELETE /silo/v1/credentials/{name}. Agents naming
// it keep their name for it, and fail to start until one is added back.
func (s *Server) removeCredential(q *request, name string) (any, error) {
	ctx := q.r.Context()
	out := map[string]any{"name": name, "deleted": !q.validateOnly}
	err := s.tx(ctx, func(tx store.Tx) error {
		root, err := mustRootMember(ctx, tx, q.caller)
		if err != nil {
			return err
		}
		q.note(root, KindCredentialRemove)
		l, err := readCredentials(ctx, tx, `name = ?`, name)
		if err != nil {
			return err
		}
		if len(l) == 0 {
			return errorf(NotFound, "no credential %s", name)
		}
		if q.validateOnly {
			return errValidateOnly
		}
		if _, err := tx.Exec(ctx, `DELETE FROM agent_credentials WHERE name = ?`, name); err != nil {
			return err
		}
		return Audit(ctx, tx, root, q.caller, KindCredentialRemove, q.requestID, "OK", map[string]any{
			"credential": name, "provider": l[0].Provider, "credential_kind": l[0].Kind,
		})
	})
	if err != nil && !errors.Is(err, errValidateOnly) {
		return nil, err
	}
	return out, nil
}

// removeAllCredentials is DELETE /silo/v1/credentials: every credential
// goes, each audited credential.remove. silo uninstall calls it before
// it removes silo-credential-key from the control VMs' metadata
// (install.md, "silo uninstall", step 3), so that a kept database, or
// its final backup, holds none.
func (s *Server) removeAllCredentials(q *request) (any, error) {
	ctx := q.r.Context()
	var names []string
	err := s.tx(ctx, func(tx store.Tx) error {
		root, err := mustRootMember(ctx, tx, q.caller)
		if err != nil {
			return err
		}
		q.note(root, KindCredentialRemove)
		l, err := readCredentials(ctx, tx, "")
		if err != nil {
			return err
		}
		names = []string{}
		for _, c := range l {
			names = append(names, c.Name)
		}
		if q.validateOnly {
			return errValidateOnly
		}
		for _, c := range l {
			if err := Audit(ctx, tx, root, q.caller, KindCredentialRemove, q.requestID, "OK", map[string]any{
				"credential": c.Name, "provider": c.Provider, "credential_kind": c.Kind,
			}); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `DELETE FROM agent_credentials`)
		return err
	})
	if err != nil && !errors.Is(err, errValidateOnly) {
		return nil, err
	}
	return map[string]any{"deleted": names}, nil
}

// CredentialSecret is an api_key credential's key, for the model proxy,
// which adds it to a call on the way out. It is never returned through
// the API.
func (s *Server) CredentialSecret(ctx context.Context, name string) (agents.Credential, []byte, error) {
	var cred agents.Credential
	var key []byte
	if a, ok := s.ambient(name); ok {
		return a, nil, errorf(FailedPrecondition, "credential %s is ambient: the harness uses its own login, and the model proxy carries nothing for it", name)
	}
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		l, err := readCredentials(ctx, tx, `name = ?`, name)
		if err != nil {
			return err
		}
		if len(l) == 0 {
			return errorf(NotFound, "no credential %s", name)
		}
		c := l[0]
		cred = agents.Credential{Name: c.Name, Provider: c.Provider, Kind: c.Kind, MaxSessions: c.MaxSessions}
		if c.Kind != agents.CredentialAPIKey {
			return errorf(FailedPrecondition, "credential %s is a subscription, which the model proxy doesn't carry", name)
		}
		key, err = s.open(c.Name, c.secret)
		return err
	})
	return cred, key, err
}

// SessionCredential is the credential a session runs on, by name, for
// the harness Runner: its provider and kind, never its secret.
func (s *Server) SessionCredential(ctx context.Context, name string) (agents.Credential, error) {
	var cred agents.Credential
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		c, ok, err := s.credential(ctx, tx, name)
		if err == nil && !ok {
			err = errorf(NotFound, "no credential %s", name)
		}
		cred = c
		return err
	})
	return cred, err
}

// CredentialFiles are a subscription credential's login files, by path
// under the harness's home: what the session runner decrypts into a
// session's private directory when it starts.
func (s *Server) CredentialFiles(ctx context.Context, name string) (map[string][]byte, error) {
	var files map[string][]byte
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		l, err := readCredentials(ctx, tx, `name = ?`, name)
		if err != nil {
			return err
		}
		if len(l) == 0 {
			return errorf(NotFound, "no credential %s", name)
		}
		if l[0].Kind != agents.CredentialSubscription {
			return errorf(FailedPrecondition, "credential %s is an API key, which never reaches a session", name)
		}
		plain, err := s.open(name, l[0].secret)
		if err != nil {
			return err
		}
		return json.Unmarshal(plain, &files)
	})
	return files, err
}

// StoreCredentialFiles writes a subscription's login files back, when a
// harness has refreshed its login, so the next session on it, on any
// control VM, gets them.
func (s *Server) StoreCredentialFiles(ctx context.Context, name string, files map[string][]byte) error {
	plain, err := json.Marshal(files)
	if err != nil {
		return err
	}
	sealed, err := s.seal(name, plain)
	if err != nil {
		return err
	}
	return s.st.Tx(ctx, func(tx store.Tx) error {
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		n, err := tx.Exec(ctx, `UPDATE agent_credentials SET secret = ?, updated_at = ? WHERE name = ? AND kind = ?`,
			sealed, store.Millis(now), name, agents.CredentialSubscription)
		if err != nil {
			return err
		}
		if n == 0 {
			return errorf(NotFound, "no subscription credential %s", name)
		}
		return nil
	})
}

// credentialSessions refuses a session for an agent on a credential at
// its max_sessions: RESOURCE_EXHAUSTED, naming the credential.
func (s *Server) credentialSessions(ctx context.Context, tx store.Tx, credential, agent string) error {
	cred, ok, err := s.credential(ctx, tx, credential)
	if err != nil || !ok || cred.MaxSessions == 0 {
		return err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM agents WHERE credential = ? AND id <> ? AND state IN (?, ?, ?, ?)`,
		credential, agent, agents.StateProvisioning, agents.StateRunning, agents.StateIdle, agents.StateWaiting).Scan(&n); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if n >= cred.MaxSessions {
		return errorf(ResourceExhausted, "credential %s already has its max_sessions of %d sessions", credential, cred.MaxSessions).
			with("credential", credential)
	}
	return nil
}
