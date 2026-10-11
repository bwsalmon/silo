// Package install is silo install and silo uninstall
// (docs/silo/install.md): Go, not terraform (call 18, SV-91).
//
// Every step is a small reconciler (Step): Observe reads the cloud and
// changes nothing, Plan is pure, and Apply is safe to repeat. Steps
// find their objects by fixed name (or, for the database instance, by
// label), and label what they create silo-install=<InstallID>, or give
// it the description prefix silo-install: where GCE has no labels.
// Running install again with the same flags therefore finishes an
// interrupted install, with no state of its own to keep.
//
// --dry-run is PlanAll; an install prints the same plan (RenderTable,
// or RenderJSON, SV-95), asks for confirmation unless --yes, and runs
// Apply, which observes and plans each step afresh just before applying
// it. The prereqs step (SV-94) runs every prerequisite check and
// reports every failure at once; a check whose API isn't enabled yet is
// deferred, and Apply runs it again once the apis step has enabled it.
//
// Every GCP API install calls is behind a small interface in Cloud;
// package install/gcp has the real ones, over the installer's own
// credentials, and package install/fakecloud an in-memory project. The
// state harness in states_test.go plans every step against recorded
// observations of an install interrupted after each step (SV-97).
package install
