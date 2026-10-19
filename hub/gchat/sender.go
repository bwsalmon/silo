package gchat

import (
	"bytes"
	"cmp"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultAPIBaseURL is the Google Chat REST API base URL.
	DefaultAPIBaseURL = "https://chat.googleapis.com"
	// DefaultTokenURI is Google's OAuth2 token endpoint.
	DefaultTokenURI = "https://oauth2.googleapis.com/token"
	// ChatBotScope is the OAuth2 scope required by Google Chat bots.
	ChatBotScope = "https://www.googleapis.com/auth/chat.bot"
)

// Thread identifies a Google Chat thread within a space.
type Thread struct {
	Name      string `json:"name,omitempty"`
	ThreadKey string `json:"threadKey,omitempty"`
}

// ActionResponse instructs Google Chat how to render a synchronous webhook response
// (e.g. "NEW_MESSAGE" or "UPDATE_MESSAGE" when updating a card in-place on CARD_CLICKED).
type ActionResponse struct {
	Type string `json:"type,omitempty"`
}

// CardWithID wraps a Google Chat Card v2 with a stable cardId.
type CardWithID struct {
	CardID string `json:"cardId"`
	Card   Card   `json:"card"`
}

// Card is a Google Chat Card v2 payload.
type Card struct {
	Header   *CardHeader   `json:"header,omitempty"`
	Sections []CardSection `json:"sections,omitempty"`
}

// CardHeader is the header of a Google Chat Card v2.
type CardHeader struct {
	Title    string `json:"title"`
	Subtitle string `json:"subtitle,omitempty"`
}

// CardSection is one section of widgets inside a Card v2.
type CardSection struct {
	Header  string   `json:"header,omitempty"`
	Widgets []Widget `json:"widgets"`
}

// Widget is one widget inside a Card v2 section.
type Widget struct {
	TextParagraph *TextParagraph `json:"textParagraph,omitempty"`
	DecoratedText *DecoratedText `json:"decoratedText,omitempty"`
	ButtonList    *ButtonList    `json:"buttonList,omitempty"`
}

// TextParagraph renders formatted text inside a Card v2 section.
type TextParagraph struct {
	Text string `json:"text"`
}

// DecoratedText renders a labeled key/value line inside a Card v2 section.
type DecoratedText struct {
	TopLabel string   `json:"topLabel,omitempty"`
	Text     string   `json:"text"`
	Button   *Button  `json:"button,omitempty"`
	OnClick  *OnClick `json:"onClick,omitempty"`
}

// ButtonList renders a horizontal row of interactive buttons in a Card v2 section.
type ButtonList struct {
	Buttons []Button `json:"buttons"`
}

// Button is an interactive button in a Card v2 ButtonList.
type Button struct {
	Text    string   `json:"text"`
	OnClick *OnClick `json:"onClick,omitempty"`
}

// OnClick defines the action triggered when a Card v2 button is clicked.
type OnClick struct {
	Action   *Action   `json:"action,omitempty"`
	OpenLink *OpenLink `json:"openLink,omitempty"`
}

// Action invokes a CARD_CLICKED callback with key/value parameters.
type Action struct {
	Function   string            `json:"function"`
	Parameters []ActionParameter `json:"parameters,omitempty"`
}

// ActionParameter is one key/value pair passed in a Card v2 Action.
type ActionParameter struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// OpenLink opens an external URL when a Card v2 button is clicked.
type OpenLink struct {
	URL string `json:"url"`
}

// OutboundMessage is a message or interactive card sent or updated in Google Chat.
type OutboundMessage struct {
	Name           string          `json:"name,omitempty"`
	SpaceName      string          `json:"space_name,omitempty"`
	ThreadName     string          `json:"thread_name,omitempty"`
	Text           string          `json:"text,omitempty"`
	ActionResponse *ActionResponse `json:"actionResponse,omitempty"`
	CardsV2        []CardWithID    `json:"cardsV2,omitempty"`
	Thread         *Thread         `json:"thread,omitempty"`
}

// Sender delivers outbound messages and in-place card updates to Google Chat.
type Sender interface {
	SendMessage(ctx context.Context, spaceName, threadName string, msg OutboundMessage) (string, error)
	UpdateMessage(ctx context.Context, messageName string, msg OutboundMessage) error
}

// RecordedCall records one SendMessage ("POST") or UpdateMessage ("PATCH") call on MemorySender.
type RecordedCall struct {
	Method      string          `json:"method"`
	SpaceName   string          `json:"space_name,omitempty"`
	ThreadName  string          `json:"thread_name,omitempty"`
	MessageName string          `json:"message_name,omitempty"`
	Message     OutboundMessage `json:"message"`
}

// MemorySender is an in-memory Sender recorder for deterministic unit and E2E tests.
type MemorySender struct {
	mu      sync.Mutex
	seq     int64
	sent    []OutboundMessage
	updated []OutboundMessage
	calls   []RecordedCall
	byName  map[string]OutboundMessage
}

// NewMemorySender creates a new in-memory Google Chat Sender recorder.
func NewMemorySender() *MemorySender {
	return &MemorySender{
		byName: map[string]OutboundMessage{},
	}
}

// SendMessage records an outbound Google Chat message and returns a deterministic message name.
func (m *MemorySender) SendMessage(_ context.Context, spaceName, threadName string, msg OutboundMessage) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	spaceName = normalizeSpaceName(spaceName)
	threadName = strings.TrimSpace(threadName)
	m.seq++
	name := msg.Name
	if name == "" {
		name = fmt.Sprintf("%s/messages/msg-%d", spaceName, m.seq)
	}
	msg.Name = name
	msg.SpaceName = spaceName
	msg.ThreadName = threadName
	if threadName != "" && msg.Thread == nil {
		msg.Thread = &Thread{Name: threadName}
	}
	m.sent = append(m.sent, msg)
	m.byName[name] = msg
	m.calls = append(m.calls, RecordedCall{
		Method:      http.MethodPost,
		SpaceName:   spaceName,
		ThreadName:  threadName,
		MessageName: name,
		Message:     msg,
	})
	return name, nil
}

// UpdateMessage records an in-place PATCH update to an existing Google Chat message/card.
func (m *MemorySender) UpdateMessage(_ context.Context, messageName string, msg OutboundMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	messageName = strings.TrimSpace(messageName)
	if messageName == "" {
		return errors.New("gchat: message name is required for UpdateMessage")
	}
	msg.Name = messageName
	if prev, ok := m.byName[messageName]; ok {
		if msg.SpaceName == "" {
			msg.SpaceName = prev.SpaceName
		}
		if msg.ThreadName == "" {
			msg.ThreadName = prev.ThreadName
		}
	}
	m.updated = append(m.updated, msg)
	m.byName[messageName] = msg
	m.calls = append(m.calls, RecordedCall{
		Method:      http.MethodPatch,
		SpaceName:   msg.SpaceName,
		ThreadName:  msg.ThreadName,
		MessageName: messageName,
		Message:     msg,
	})
	return nil
}

// Messages returns a copy of all messages sent via SendMessage.
func (m *MemorySender) Messages() []OutboundMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]OutboundMessage, len(m.sent))
	copy(out, m.sent)
	return out
}

// UpdatedMessages returns a copy of all message updates sent via UpdateMessage.
func (m *MemorySender) UpdatedMessages() []OutboundMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]OutboundMessage, len(m.updated))
	copy(out, m.updated)
	return out
}

// Calls returns a copy of all recorded POST and PATCH calls in chronological order.
func (m *MemorySender) Calls() []RecordedCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RecordedCall, len(m.calls))
	copy(out, m.calls)
	return out
}

// Message returns the latest state of messageName (including any in-place UpdateMessage edits).
func (m *MemorySender) Message(messageName string) (OutboundMessage, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	msg, ok := m.byName[messageName]
	return msg, ok
}

// Reset clears all recorded messages and calls.
func (m *MemorySender) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq = 0
	m.sent = nil
	m.updated = nil
	m.calls = nil
	m.byName = map[string]OutboundMessage{}
}

// HTTPSender calls the Google Chat REST API (POST /v1/{space}/messages and
// PATCH /v1/{message}) authenticated with OAuth2 service-account credentials.
type HTTPSender struct {
	// BaseURL overrides DefaultAPIBaseURL ("https://chat.googleapis.com").
	BaseURL string
	// Client is the HTTP client used for API and token requests; defaults to http.DefaultClient.
	Client *http.Client
	// ServiceAccountJSON is the raw Google Cloud service account key JSON, or a file path to it.
	ServiceAccountJSON []byte
	// BearerToken is an optional static bearer token (used when ServiceAccountJSON is empty).
	BearerToken string

	mu          sync.Mutex
	parsedKey   *serviceAccountCreds
	cachedToken string
	tokenExpiry time.Time
}

type serviceAccountCreds struct {
	ClientEmail  string `json:"client_email"`
	PrivateKey   string `json:"private_key"`
	PrivateKeyID string `json:"private_key_id"`
	TokenURI     string `json:"token_uri"`
	rsaKey       *rsa.PrivateKey
}

// NewHTTPSender constructs an HTTPSender from a service account JSON file path or inline JSON string.
func NewHTTPSender(serviceAccount string) (*HTTPSender, error) {
	s := &HTTPSender{
		BaseURL: DefaultAPIBaseURL,
	}
	trimmed := strings.TrimSpace(serviceAccount)
	if trimmed == "" {
		return s, nil
	}
	var raw []byte
	if strings.HasPrefix(trimmed, "{") {
		raw = []byte(trimmed)
	} else {
		b, err := os.ReadFile(trimmed)
		if err != nil {
			return nil, fmt.Errorf("gchat: reading service account %q: %w", trimmed, err)
		}
		raw = b
	}
	s.ServiceAccountJSON = raw
	if _, err := s.loadCreds(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *HTTPSender) httpClient() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return http.DefaultClient
}

func (s *HTTPSender) baseURL() string {
	b := strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
	if b == "" {
		return DefaultAPIBaseURL
	}
	return b
}

func (s *HTTPSender) loadCreds() (*serviceAccountCreds, error) {
	if s.parsedKey != nil {
		return s.parsedKey, nil
	}
	if len(s.ServiceAccountJSON) == 0 {
		return nil, nil
	}
	var creds serviceAccountCreds
	if err := json.Unmarshal(s.ServiceAccountJSON, &creds); err != nil {
		return nil, fmt.Errorf("gchat: parsing service account JSON: %w", err)
	}
	if creds.ClientEmail == "" || creds.PrivateKey == "" {
		return nil, errors.New("gchat: service account JSON missing client_email or private_key")
	}
	if creds.TokenURI == "" {
		creds.TokenURI = DefaultTokenURI
	}
	block, _ := pem.Decode([]byte(creds.PrivateKey))
	if block == nil {
		return nil, errors.New("gchat: service account private_key is not valid PEM")
	}
	var rsaKey *rsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		var ok bool
		rsaKey, ok = k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("gchat: service account private_key is not RSA")
		}
	} else if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		rsaKey = k
	} else {
		return nil, fmt.Errorf("gchat: parsing service account RSA key: %w", err)
	}
	creds.rsaKey = rsaKey
	s.parsedKey = &creds
	return s.parsedKey, nil
}

func (s *HTTPSender) accessToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.BearerToken != "" && len(s.ServiceAccountJSON) == 0 {
		return s.BearerToken, nil
	}
	if s.cachedToken != "" && time.Now().Add(time.Minute).Before(s.tokenExpiry) {
		return s.cachedToken, nil
	}
	creds, err := s.loadCreds()
	if err != nil {
		return "", err
	}
	if creds == nil {
		return "", nil
	}
	now := time.Now().UTC()
	exp := now.Add(time.Hour)
	hdr := map[string]string{
		"alg": "RS256",
		"typ": "JWT",
	}
	if creds.PrivateKeyID != "" {
		hdr["kid"] = creds.PrivateKeyID
	}
	claims := map[string]any{
		"iss":   creds.ClientEmail,
		"scope": ChatBotScope,
		"aud":   creds.TokenURI,
		"iat":   now.Unix(),
		"exp":   exp.Unix(),
	}
	hdrJSON, _ := json.Marshal(hdr)
	claimsJSON, _ := json.Marshal(claims)
	unsigned := base64.RawURLEncoding.EncodeToString(hdrJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, creds.rsaKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("gchat: signing service account JWT: %w", err)
	}
	assertion := unsigned + "." + base64.RawURLEncoding.EncodeToString(sig)

	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", assertion)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, creds.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("gchat: exchanging service account token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("gchat: token endpoint %s returned %d: %s", creds.TokenURI, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tokResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tokResp); err != nil {
		return "", fmt.Errorf("gchat: decoding token response: %w", err)
	}
	s.cachedToken = tokResp.AccessToken
	ttl := time.Duration(cmp.Or(tokResp.ExpiresIn, int64(3600))) * time.Second
	s.tokenExpiry = now.Add(ttl)
	return s.cachedToken, nil
}

func normalizeSpaceName(space string) string {
	space = strings.TrimSpace(space)
	if space == "" {
		return ""
	}
	if !strings.HasPrefix(space, "spaces/") {
		return "spaces/" + space
	}
	return space
}

// SendMessage posts a message or card to Google Chat via POST /v1/{space}/messages.
func (s *HTTPSender) SendMessage(ctx context.Context, spaceName, threadName string, msg OutboundMessage) (string, error) {
	spaceName = normalizeSpaceName(spaceName)
	threadName = strings.TrimSpace(threadName)
	if spaceName == "" {
		return "", errors.New("gchat: space_name is required")
	}
	if threadName != "" && msg.Thread == nil {
		msg.Thread = &Thread{Name: threadName}
	}
	endpoint := s.baseURL() + "/v1/" + spaceName + "/messages"
	if threadName != "" {
		endpoint += "?messageReplyOption=REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD"
	}
	payload := struct {
		Text    string       `json:"text,omitempty"`
		CardsV2 []CardWithID `json:"cardsV2,omitempty"`
		Thread  *Thread      `json:"thread,omitempty"`
	}{
		Text:    msg.Text,
		CardsV2: msg.CardsV2,
		Thread:  msg.Thread,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	tok, err := s.accessToken(ctx)
	if err != nil {
		return "", err
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("gchat: POST %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("gchat: POST %s returned %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	var created struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&created)
	return created.Name, nil
}

// UpdateMessage updates an existing Google Chat message/card in-place via PATCH /v1/{message}.
func (s *HTTPSender) UpdateMessage(ctx context.Context, messageName string, msg OutboundMessage) error {
	messageName = strings.TrimPrefix(strings.TrimSpace(messageName), "/")
	if messageName == "" {
		return errors.New("gchat: messageName is required")
	}
	endpoint := s.baseURL() + "/v1/" + messageName + "?updateMask=text,cardsV2"
	payload := struct {
		Text    string       `json:"text,omitempty"`
		CardsV2 []CardWithID `json:"cardsV2,omitempty"`
	}{
		Text:    msg.Text,
		CardsV2: msg.CardsV2,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	tok, err := s.accessToken(ctx)
	if err != nil {
		return err
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("gchat: PATCH %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("gchat: PATCH %s returned %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	return nil
}
