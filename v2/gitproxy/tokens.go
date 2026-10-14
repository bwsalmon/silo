// Adapted from github.com/bwsalmon/grain pkg/gitproxy/tokens.go at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

package gitproxy

// The token a caller's git sends. grain keeps one per sandbox in a
// sealed file, because its daemon outlives every sandbox and restarts
// under them; a silo session's proxy lives exactly as long as the
// session, so it holds one token, in memory, and nothing is written down.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// SessionToken is an Authenticator with one token, for one caller.
type SessionToken struct {
	token  string
	caller Caller
}

// NewSessionToken mints a random token for caller.
func NewSessionToken(caller Caller) (*SessionToken, error) {
	token, err := randomHex(32)
	if err != nil {
		return nil, err
	}
	return &SessionToken{token: token, caller: caller}, nil
}

// Token is the token, to hand to the caller's git (as the password of
// its credential, or in the remote URL).
func (s *SessionToken) Token() string { return s.token }

// Authenticate implements Authenticator, comparing in constant time.
func (s *SessionToken) Authenticate(token string) (Caller, bool) {
	if s == nil || s.token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
		return Caller{}, false
	}
	return s.caller, true
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("gitproxy: minting a token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// ExtractBasicAuthToken pulls the token out of an
// "Authorization: Basic base64(user:token)" header. The username is
// ignored -- a credential helper is configured with an arbitrary username
// and the token as the password, so the token is what identifies the
// caller.
func ExtractBasicAuthToken(header string) (string, bool) {
	const prefix = "Basic "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(header[len(prefix):])
	if err != nil {
		return "", false
	}
	_, token, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return "", false
	}
	return token, true
}
