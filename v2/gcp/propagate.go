package gcp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// ExitPropagation is silo's exit code when IAM has not propagated in
// time (design §11.2 step 4, §3.3).
const ExitPropagation = 69

// PropagationTimeout is how long WaitReady waits.
const PropagationTimeout = 10 * time.Minute

// TokenLifetime is the lifetime of every token silo mints (call 20).
const TokenLifetime = 15 * time.Minute

// pollInterval is how often WaitReady asks again.
const pollInterval = 5 * time.Second

// ErrPropagation is returned, wrapped, when IAM has not propagated
// within the timeout. The account is left for the next run.
var ErrPropagation = errors.New("IAM changes have not propagated")

// MintToken mints an access token for email as the invoking user. It is
// the internal helper stage 2b turns into the refresh loop.
func (c *Client) MintToken(ctx context.Context, email string) (Token, error) {
	var t Token
	err := c.retry(ctx, false, func() (err error) {
		t, err = c.API.GenerateAccessToken(ctx, email, TokenLifetime)
		return err
	})
	return t, explain("minting a token for "+email, err)
}

// WaitReady is setup step 4 (design §11.2): it mints a token for the
// account, retrying while generateAccessToken returns 403 (a new
// token-creator grant not yet in effect) or 404 (a new account not yet
// visible), then tests samples (role to permissions) as the account
// until every one is granted, ticking each role off as it passes. It
// gives up after timeout (PropagationTimeout when zero) with an error
// wrapping ErrPropagation.
func (c *Client) WaitReady(ctx context.Context, project, email string, samples map[string][]string, timeout time.Duration) (Token, error) {
	if timeout == 0 {
		timeout = PropagationTimeout
	}
	deadline := c.now().Add(timeout)
	expired := func(what string) error {
		return fmt.Errorf("%s: %w after %s (%s); the account is kept, run again to carry on waiting",
			email, ErrPropagation, timeout, what)
	}

	var tok Token
	for {
		var err error
		tok, err = c.API.GenerateAccessToken(ctx, email, TokenLifetime)
		if err == nil {
			break
		}
		switch code := Code(err); {
		case code == 403 || code == 404 || retryable(err, false):
		default:
			return Token{}, explain("minting a token for "+email, err)
		}
		if !c.now().Before(deadline) {
			return Token{}, expired("the token-creator grant is still not in effect: " + err.Error())
		}
		if err := c.sleep(ctx, pollInterval); err != nil {
			return Token{}, err
		}
	}

	roles := make([]string, 0, len(samples))
	for r := range samples {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	c.printf("silo: waiting for %s's roles to take effect\n", email)
	pending := map[string]bool{}
	for _, r := range roles {
		pending[r] = true
	}
	for {
		for _, r := range roles {
			if !pending[r] {
				continue
			}
			ok := true
			if perms := samples[r]; len(perms) > 0 {
				held, err := c.API.TestPermissionsAs(ctx, tok.Value, project, perms)
				if err != nil && Code(err) != 403 && !retryable(err, false) {
					return Token{}, explain("testing "+email+"'s permissions", err)
				}
				ok = err == nil && len(minus(perms, held)) == 0
			}
			if ok {
				delete(pending, r)
				c.printf("  ✓ %s\n", r)
			}
		}
		if len(pending) == 0 {
			return tok, nil
		}
		if !c.now().Before(deadline) {
			return Token{}, expired("still waiting for " + list(keys(pending)))
		}
		if err := c.sleep(ctx, pollInterval); err != nil {
			return Token{}, err
		}
	}
}
