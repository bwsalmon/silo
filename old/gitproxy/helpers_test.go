// Adapted from github.com/bwsalmon/grain pkg/gitproxy/core_test.go, body_limit_test.go and hardening_live_test.go (their shared helpers) at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

package gitproxy

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"testing"
)

var gitHeaders = map[string]string{"User-Agent": "git/2.39.2", "Accept": "*/*"}

func basicAuthHeader(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("x:"+token))
}

func merge(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// fixedToken authenticates exactly one token, as sandbox-0.
type fixedToken string

func (f fixedToken) Authenticate(token string) (Caller, bool) {
	if token != string(f) {
		return Caller{}, false
	}
	return Caller{ID: "sandbox-0", Audit: map[string]string{"session": "sandbox-0"}}, true
}

// staticToken is a credential source that always answers token.
func staticToken(name, token string) *CredentialSource {
	return &CredentialSource{Name: name, Token: func(context.Context) (string, error) { return token, nil }}
}

// testPolicy is the policy newTestProxy's proxy runs under:
// github.com/owner/repo, pushable under refs/heads/feature/*, and
// github.com/owner/readonly, readable, both with the bot credential.
func testPolicy(t *testing.T, repos ...StaticRepo) *StaticPolicy {
	t.Helper()
	if repos == nil {
		repos = []StaticRepo{
			{Repo: "github.com/owner/repo", Access: AccessPush, Push: RefRules{{Pattern: "refs/heads/feature/*"}}},
			{Repo: "github.com/owner/readonly", Access: AccessRead},
		}
	}
	p, err := NewStaticPolicy(StaticConfig{
		Repos:       repos,
		Credentials: map[string]*CredentialSource{"github.com": staticToken("bot", "bot-token")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newTestProxy(t *testing.T) (*GitProxy, *FakeForwarder, *RecordingAuditLog) {
	t.Helper()
	forwarder := &FakeForwarder{Response: UpstreamResponse{Status: 200, Headers: map[string]string{"Content-Type": "x"}, Body: []byte("ok")}}
	audit := &RecordingAuditLog{}
	proxy := &GitProxy{
		Policy:       testPolicy(t),
		Tokens:       fixedToken("tok0"),
		ForwarderFor: func(string) (Forwarder, error) { return forwarder, nil },
		Audit:        audit,
	}
	return proxy, forwarder, audit
}

// receivePackBody is a receive-pack request moving ref from old to new,
// carrying pack.
func receivePackBody(old, new, ref string, pack []byte) []byte {
	line := fmt.Sprintf("%s %s %s\x00report-status side-band-64k\n", old, new, ref)
	var body bytes.Buffer
	fmt.Fprintf(&body, "%04x%s0000", len(line)+4, line)
	body.Write(pack)
	return body.Bytes()
}

// packOf frames entries (each an object header and its zlib stream) as a
// version 2 packfile.
func packOf(entries ...[]byte) []byte {
	var pack bytes.Buffer
	pack.WriteString("PACK")
	_ = binary.Write(&pack, binary.BigEndian, uint32(2))
	_ = binary.Write(&pack, binary.BigEndian, uint32(len(entries)))
	for _, e := range entries {
		pack.Write(e)
	}
	pack.Write(make([]byte, 20))
	return pack.Bytes()
}

// packEntry is one object: its type and size header, any prefix (a
// delta's base offset), and data deflated -- or, with zeros > 0, that
// many zero bytes deflated after it.
func packEntry(typ int, prefix, data []byte, zeros int) []byte {
	var e bytes.Buffer
	e.WriteByte(byte(typ << 4)) // the size is advisory; the walk reads the stream
	e.Write(prefix)
	zw := zlib.NewWriter(&e)
	_, _ = zw.Write(data)
	chunk := make([]byte, 1<<20)
	for zeros > 0 {
		n := min(zeros, len(chunk))
		_, _ = zw.Write(chunk[:n])
		zeros -= n
	}
	_ = zw.Close()
	return e.Bytes()
}
