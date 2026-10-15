// Adapted from github.com/bwsalmon/grain pkg/gitproxy/tokens_test.go at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

package gitproxy

import (
	"encoding/base64"
	"testing"
)

func basicAuth(username, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}

func TestExtractBasicAuthTokenIgnoresUsername(t *testing.T) {
	if tok, ok := ExtractBasicAuthToken(basicAuth("whatever", "the-token")); !ok || tok != "the-token" {
		t.Errorf("got %q, %v", tok, ok)
	}
	if tok, ok := ExtractBasicAuthToken(basicAuth("", "the-token")); !ok || tok != "the-token" {
		t.Errorf("got %q, %v", tok, ok)
	}
}

func TestExtractBasicAuthTokenRejectsMalformedHeader(t *testing.T) {
	cases := []string{
		"",
		"Bearer sometoken",
		"Basic not-valid-base64!!!",
		"Basic " + base64.StdEncoding.EncodeToString([]byte("no-colon-here")),
	}
	for _, header := range cases {
		if _, ok := ExtractBasicAuthToken(header); ok {
			t.Errorf("ExtractBasicAuthToken(%q) should fail", header)
		}
	}
}

// --- SandboxTokenStore -------------------------------------------------

func TestSessionTokenAuthenticatesOnlyItsOwnToken(t *testing.T) {
	s, err := NewSessionToken(Caller{ID: "session-1", Audit: map[string]string{"session": "session-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Token()) != 64 {
		t.Errorf("token %q is not 32 random bytes in hex", s.Token())
	}
	if c, ok := s.Authenticate(s.Token()); !ok || c.ID != "session-1" || c.Audit["session"] != "session-1" {
		t.Errorf("own token: %+v, %v", c, ok)
	}
	for _, bad := range []string{"", "x", s.Token() + "0", s.Token()[1:]} {
		if _, ok := s.Authenticate(bad); ok {
			t.Errorf("%q authenticated", bad)
		}
	}
	other, _ := NewSessionToken(Caller{ID: "session-2"})
	if other.Token() == s.Token() {
		t.Error("two sessions minted the same token")
	}
	if _, ok := (*SessionToken)(nil).Authenticate(""); ok {
		t.Error("a nil SessionToken authenticated")
	}
}

func TestTokenSetNamesEachCaller(t *testing.T) {
	var s TokenSet
	if _, ok := s.Authenticate(""); ok {
		t.Fatal("an empty set authenticated the empty token")
	}
	a, err := s.Mint(Caller{ID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.Mint(Caller{ID: "b"})
	if a == b {
		t.Fatal("two callers were given one token")
	}
	for token, want := range map[string]string{a: "a", b: "b"} {
		if c, ok := s.Authenticate(token); !ok || c.ID != want {
			t.Errorf("Authenticate = %+v, %v; want %s", c, ok, want)
		}
	}
	for _, bad := range []string{"", a[:len(a)-1], a + "0"} {
		if _, ok := s.Authenticate(bad); ok {
			t.Errorf("Authenticate(%q) succeeded", bad)
		}
	}
}
