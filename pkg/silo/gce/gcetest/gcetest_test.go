package gcetest_test

import (
	"testing"

	"github.com/bwsalmon/silo/pkg/silo/gce/gcetest"
)

// TestFake runs the behaviour tests against the fake in process. The
// real client runs them through the fake's HTTP server (package gcp).
func TestFake(t *testing.T) {
	gcetest.Run(t, func(t *testing.T) gcetest.Env {
		f := gcetest.NewFake("host-project")
		return gcetest.Env{Client: f, Fake: f, Project: "host-project", Region: "us-central1", Zone: "us-central1-a"}
	})
}
