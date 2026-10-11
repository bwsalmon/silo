package frontdoor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/api/idtoken"
)

// Verifier checks a front-door ID token and returns the member
// identity it proves: user:<email> or serviceAccount:<email>, the form
// the member table keys on.
type Verifier interface {
	Verify(ctx context.Context, token string) (identity string, err error)
}

// Validator is the part of *idtoken.Validator a Google verifier uses.
type Validator interface {
	Validate(ctx context.Context, token, audience string) (*idtoken.Payload, error)
}

// Google verifies Google-signed ID tokens for one audience.
type Google struct {
	// Audience is the audience every token must carry exactly:
	// Audience(hostProject).
	Audience string
	// Validator checks the signature against Google's certificates,
	// the audience and the expiry. NewGoogle sets it; a test gives one
	// whose certificates are its own.
	Validator Validator
}

// NewGoogle returns a verifier for audience that fetches Google's
// certificates with opts.
func NewGoogle(ctx context.Context, audience string, opts ...idtoken.ClientOption) (*Google, error) {
	if audience == "" || audience == Audience("") {
		return nil, errors.New("frontdoor: no audience")
	}
	v, err := idtoken.NewValidator(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return &Google{Audience: audience, Validator: v}, nil
}

// The issuers Google signs ID tokens as.
var googleIssuers = map[string]bool{"accounts.google.com": true, "https://accounts.google.com": true}

// Verify checks token: its signature (by the Validator, against Google's
// certificates), that its audience is exactly g.Audience, that it hasn't
// expired, that Google issued it, and that it names a verified email.
func (g *Google) Verify(ctx context.Context, token string) (string, error) {
	if g.Audience == "" {
		// An empty audience would make the Validator skip the check.
		return "", errors.New("the front door has no audience configured")
	}
	p, err := g.Validator.Validate(ctx, token, g.Audience)
	if err != nil {
		return "", err
	}
	// The Validator checked these already; checking again costs nothing
	// and keeps the rule here, where it is read.
	if p.Audience != g.Audience {
		return "", fmt.Errorf("audience %q, want %q", p.Audience, g.Audience)
	}
	// The Validator also accepts ES256 tokens signed with IAP's keys,
	// and doesn't look at the issuer: an ID token here is Google's own.
	if !googleIssuers[p.Issuer] {
		return "", fmt.Errorf("issuer %q is not Google's", p.Issuer)
	}
	email, _ := p.Claims["email"].(string)
	if email == "" {
		return "", errors.New("the token carries no email; mint it with the email included")
	}
	if v, _ := p.Claims["email_verified"].(bool); !v {
		return "", fmt.Errorf("the token's email %s is not verified", email)
	}
	return Identity(email), nil
}

// Identity is the member identity of a token's email: serviceAccount:
// for a service account's, user: for anyone else's. The email is kept
// as the token has it; the member table matches it exactly.
func Identity(email string) string {
	if strings.HasSuffix(strings.ToLower(email), ".gserviceaccount.com") {
		return "serviceAccount:" + email
	}
	return "user:" + email
}
