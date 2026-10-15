package gcp_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bwsalmon/silo/v2/gcp"
)

// TestLive sets up, reuses and garbage-collects a real account. It runs
// only with SILO_V2_LIVE_GCP=1, as the ambient gcloud login (ADC or the
// gcloud CLI), in GOOGLE_CLOUD_PROJECT (default bsalmon-fun-sandbox),
// and deletes what it creates. It is not part of the CI gate.
func TestLive(t *testing.T) {
	if os.Getenv("SILO_V2_LIVE_GCP") != "1" {
		t.Skip("set SILO_V2_LIVE_GCP=1 to run against a real project")
	}
	project := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if project == "" {
		project = "bsalmon-fun-sandbox"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	api, user, err := gcp.Ambient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("as %s in %s", user, project)
	c := &gcp.Client{API: api, Store: &gcp.Store{Path: filepath.Join(t.TempDir(), "accounts.json")}, Out: testWriter{t}}
	in := gcp.CheckInput{Project: project, User: user, Roles: []string{"roles/logging.viewer"}}

	rep, err := c.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range rep.Warnings {
		t.Logf("warning: %s", w)
	}
	if err := rep.Err(); err != nil {
		t.Fatalf("setup checks refused (the roles must be granted to the invoking principal): %v", err)
	}

	email := rep.Spec.Email()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := c.GC(ctx, gcp.GCOptions{User: user, Projects: []string{project}, Account: email}); err != nil {
			t.Errorf("cleaning up %s: %v", email, err)
		}
	})
	rec, tok, err := c.Setup(ctx, gcp.SetupInput{CheckInput: in, TTL: 0})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Email != email || tok.Value == "" {
		t.Fatalf("record %+v", rec)
	}
	// Reuse verifies and writes nothing.
	if _, err := c.Ensure(ctx, gcp.EnsureInput{Spec: rep.Spec}); err != nil {
		t.Fatal(err)
	}
	// ttl 0 and no live session: gc deletes it.
	gcr, err := c.GC(ctx, gcp.GCOptions{User: user, Alive: func(gcp.SessionRef) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	if len(gcr.Deleted) != 1 || gcr.Deleted[0] != email {
		t.Fatalf("gc deleted %v", gcr.Deleted)
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}
