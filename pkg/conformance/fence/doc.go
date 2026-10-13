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
// and its Fence as not runnable here, since the GCP harness in
// pkg/conformance, behind the silo_conformance tag, still runs its own
// forms of the fence cases nightly; "local/sandbox" and "local/vm"
// report themselves not runnable until the local backend's silod
// serves the registry and the Docker facade's host sockets.
package fence
