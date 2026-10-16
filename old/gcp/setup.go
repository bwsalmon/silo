package gcp

import (
	"context"
	"fmt"
	"time"
)

// SetupInput is one projects[] entry to set up.
type SetupInput struct {
	CheckInput
	TTL     time.Duration
	Session *SessionRef
	// Timeout bounds the propagation wait; PropagationTimeout when zero.
	Timeout time.Duration
}

// Setup runs the four setup steps of design §11.2 for one entry: the
// checks, create or reuse, and the propagation wait. It returns the
// account's record and a first token.
func (c *Client) Setup(ctx context.Context, in SetupInput) (Record, Token, error) {
	rep, err := c.Check(ctx, in.CheckInput)
	if err != nil {
		return Record{}, Token{}, err
	}
	for _, w := range rep.Warnings {
		c.printf("silo: warning: %s\n", w)
	}
	if err := rep.Err(); err != nil {
		return Record{}, Token{}, err
	}
	rec, err := c.Ensure(ctx, EnsureInput{Spec: rep.Spec, TTL: in.TTL, Session: in.Session})
	if err != nil {
		return rec, Token{}, err
	}
	tok, err := c.WaitReady(ctx, rep.Spec.Project, rec.Email, rep.Samples(), in.Timeout)
	if err != nil {
		return rec, Token{}, fmt.Errorf("setting up %s: %w", rec.Email, err)
	}
	return rec, tok, nil
}
