// Package fence holds the backend-neutral fence cases of the
// conformance suite (local-backend.md §13, call L24), written against a
// conformance.Target: the fence API, and the registry's OCI API called
// as a silo, the way a probe in that silo calls it. Today these are the
// registry and cache cases of local-registry.md §12; the GCP harness's
// fence cases in pkg/conformance move here with local-backend.md's item
// 12.
//
// Every case runs against every target of targets.All. "local/fake"
// runs in process and so on every push, in the go job; "gcp" reports
// the registry cases as not offered; "local/sandbox" and "local/vm"
// report themselves not runnable until the local backend's silod
// serves the registry and a CI job installs it.
package fence
