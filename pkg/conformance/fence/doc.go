// Package fence holds the backend-neutral fence cases of the
// conformance suite (local-backend.md §13, call L24), written against a
// conformance.Target: the fence API and probes in silos (Target.Fence),
// and the registry's OCI API called as a silo, the way a probe in that
// silo calls it (Target.Registry). They are isolation, origin
// spoofing, never-looser, links, egress, and sealed and read silos
// (fence_test.go), and the registry and cache cases of
// local-registry.md §12 (registry_test.go).
//
// Every case runs against every target of targets.All. "local/fake"
// runs in process and so on every push, in the go job and in
// local-conformance; "gcp" reports the registry cases as not offered,
// and runs the fence cases with e2-micro probe VMs on the GCP harness's
// install when built with the silo_conformance tag (main_gcp_test.go),
// as silo-conformance does nightly, and reports its Fence as not
// runnable otherwise; "local/sandbox" and "local/vm" report themselves
// not runnable until the local backend's silod serves the registry and
// the Docker facade's host sockets.
package fence
