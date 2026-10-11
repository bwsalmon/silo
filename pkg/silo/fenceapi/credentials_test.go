package fenceapi_test

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/silo/agents"
	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/store"
)

// testCredentialKey is the agent tests' silo-credential-key.
var testCredentialKey = bytes.Repeat([]byte{7}, 32)

const testSecret = "sk-ant-api03-PLAINTEXT-SECRET"

// storedSecrets are agent_credentials's secret column, as the database
// (and so its backups) holds it.
func (e *agentEnv) storedSecrets() []string {
	e.t.Helper()
	var out []string
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		rows, err := tx.Query(ctx, `SELECT name, provider, kind, secret FROM agent_credentials`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n, p, k, s string
			if err := rows.Scan(&n, &p, &k, &s); err != nil {
				return err
			}
			out = append(out, n+" "+p+" "+k+" "+s)
		}
		return rows.Err()
	}); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func TestCredentials(t *testing.T) {
	e := newAgentEnv(t)
	key := obj{"name": "anthropic-api", "provider": "anthropic", "kind": "api_key", "secret": testSecret}

	// Root members only, and never an agent's session, even the root's.
	e.fail(fenceapi.PermissionDenied, e.team, "POST", "/silo/v1/credentials", key)
	e.fail(fenceapi.PermissionDenied, e.team, "GET", "/silo/v1/credentials", nil)
	e.fail(fenceapi.PermissionDenied, e.root+"/agent:m3x8re", "POST", "/silo/v1/credentials", key)
	e.fail(fenceapi.PermissionDenied, e.team, "DELETE", "/silo/v1/credentials", nil)

	got := e.ok(e.root, "POST", "/silo/v1/credentials", key)
	if got["max_sessions"] != float64(0) || got["kind"] != "api_key" || got["secret"] != nil {
		t.Errorf("add: %v", got)
	}
	login := base64.StdEncoding.EncodeToString([]byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat01-PLAINTEXT-LOGIN"}}`))
	sub := e.ok(e.root, "POST", "/silo/v1/credentials", obj{"name": "claude-max-bob", "provider": "anthropic", "kind": "subscription",
		"files": obj{".claude/.credentials.json": login}})
	if sub["max_sessions"] != float64(2) {
		t.Errorf("a subscription's cap is %v, want 2", sub["max_sessions"])
	}
	e.fail(fenceapi.InvalidArgument, e.root, "POST", "/silo/v1/credentials", obj{"name": "x", "provider": "anthropic", "kind": "subscription",
		"files": obj{"../etc/passwd": login}})
	e.fail(fenceapi.InvalidArgument, e.root, "POST", "/silo/v1/credentials", obj{"name": "x", "provider": "nobody", "kind": "api_key", "secret": "k"})
	e.fail(fenceapi.AlreadyExists, e.root, "POST", "/silo/v1/credentials", obj{"name": "claude-max-bob", "provider": "anthropic", "kind": "api_key", "secret": "k"})

	// Listed, never read back.
	l := e.ok(e.root, "GET", "/silo/v1/credentials", nil)["credentials"].([]any)
	if len(l) != 2 || strings.Contains(str(l), "PLAINTEXT") {
		t.Errorf("list: %v", l)
	}
	// At rest, sealed: no plaintext in the database.
	for _, row := range e.storedSecrets() {
		if strings.Contains(row, "PLAINTEXT") || strings.Contains(row, "claudeAiOauth") {
			t.Errorf("a secret in the clear: %s", row)
		}
	}
	// silod opens them with the key...
	cred, secret, err := e.api.CredentialSecret(ctx, "anthropic-api")
	if err != nil || string(secret) != testSecret || cred.Kind != agents.CredentialAPIKey {
		t.Errorf("CredentialSecret: %v %q %v", cred, secret, err)
	}
	files, err := e.api.CredentialFiles(ctx, "claude-max-bob")
	if err != nil || !strings.Contains(string(files[".claude/.credentials.json"]), "PLAINTEXT-LOGIN") {
		t.Errorf("CredentialFiles: %v %v", files, err)
	}
	// ...and a refreshed login is written back, sealed.
	if err := e.api.StoreCredentialFiles(ctx, "claude-max-bob", map[string][]byte{".claude/.credentials.json": []byte("REFRESHED")}); err != nil {
		t.Fatal(err)
	}
	if files, _ := e.api.CredentialFiles(ctx, "claude-max-bob"); string(files[".claude/.credentials.json"]) != "REFRESHED" {
		t.Errorf("after the refresh: %q", files)
	}
	if _, err := e.api.CredentialFiles(ctx, "anthropic-api"); err == nil {
		t.Error("an API key's secret came out as session files")
	}
	// Without the key, nothing opens: a database (or a backup) alone
	// holds no credential.
	other := fenceapi.New(e.db, fenceapi.Options{CredentialKey: bytes.Repeat([]byte{8}, 32)})
	defer other.Close()
	if _, _, err := other.CredentialSecret(ctx, "anthropic-api"); err == nil {
		t.Error("a credential opened with another key")
	}
	none := fenceapi.New(e.db, fenceapi.Options{})
	defer none.Close()
	if _, _, err := none.CredentialSecret(ctx, "anthropic-api"); err == nil {
		t.Error("a credential opened with no key")
	}

	e.ok(e.root, "DELETE", "/silo/v1/credentials/claude-max-bob", nil)
	e.fail(fenceapi.NotFound, e.root, "DELETE", "/silo/v1/credentials/claude-max-bob", nil)
	if recs := e.records(e.root, "credential.add"); len(recs) != 2 || get(recs[0], "fields", "credential") == nil {
		t.Errorf("credential.add audit: %v", recs)
	}
	if recs := e.records(e.root, "credential.remove"); len(recs) != 1 || strings.Contains(str(recs), "PLAINTEXT") {
		t.Errorf("credential.remove audit: %v", recs)
	}

	// Uninstall's step: every credential goes.
	all := e.ok(e.root, "DELETE", "/silo/v1/credentials", nil)
	if str(all["deleted"]) != "[anthropic-api]" {
		t.Errorf("delete all: %v", all)
	}
	if rows := e.storedSecrets(); len(rows) != 0 {
		t.Errorf("left after delete all: %v", rows)
	}
}

func TestCredentialMaxSessions(t *testing.T) {
	e := newAgentEnv(t)
	login := base64.StdEncoding.EncodeToString([]byte(`{}`))
	e.ok(e.root, "POST", "/silo/v1/credentials", obj{"name": "anthropic-api", "provider": "anthropic", "kind": "subscription",
		"max_sessions": 1, "files": obj{".claude/.credentials.json": login}})
	e.create(e.root, "team", "a", obj{"budget": obj{"usd": "1"}})
	er := e.fail(fenceapi.ResourceExhausted, e.root, "POST", "/silo/v1/silos/team/agents", agentBody("b", obj{"budget": obj{"usd": "1"}}))
	if get(er, "details", "credential") != "anthropic-api" {
		t.Errorf("refusal: %v", er)
	}
	// A stopped agent holds none.
	e.ok(e.root, "POST", "/silo/v1/silos/team/agents/a:stop", nil)
	e.create(e.root, "team", "b", obj{"budget": obj{"usd": "1"}})
	e.fail(fenceapi.ResourceExhausted, e.root, "POST", "/silo/v1/silos/team/agents/a:resume", nil)
}

// The uninstall step's endpoint, through silod's handler, answers as
// install expects.
func TestDeleteAllCredentialsShape(t *testing.T) {
	e := newAgentEnv(t)
	if out := e.ok(e.root, "DELETE", "/silo/v1/credentials", nil); str(out["deleted"]) != "[]" {
		t.Errorf("with none: %v", out)
	}
}

// silo uninstall's step 3 lists the credentials at /silod/credentials
// and deletes each there (install's Silod client), root members only.
func TestUninstallCredentialEndpoint(t *testing.T) {
	e := newAgentEnv(t)
	e.ok(e.root, "POST", "/silo/v1/credentials", obj{"name": "anthropic-api", "provider": "anthropic", "kind": "api_key", "secret": testSecret})
	e.fail(fenceapi.PermissionDenied, e.team, "GET", "/silod/credentials", nil)
	e.fail(fenceapi.PermissionDenied, e.team, "DELETE", "/silod/credentials/anthropic-api", nil)
	l := e.ok(e.root, "GET", "/silod/credentials", nil)["credentials"].([]any)
	if len(l) != 1 || l[0].(obj)["name"] != "anthropic-api" {
		t.Fatalf("list: %v", l)
	}
	e.ok(e.root, "DELETE", "/silod/credentials/anthropic-api", nil)
	if rows := e.storedSecrets(); len(rows) != 0 {
		t.Errorf("left: %v", rows)
	}
	if recs := e.records(e.root, "credential.remove"); len(recs) != 1 {
		t.Errorf("audit: %v", recs)
	}
}
