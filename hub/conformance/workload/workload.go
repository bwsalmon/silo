// Package workload is the conformance workloads: the tasks the
// conformance kit (package conformance) files, which a Grain may run
// without a model because what each must do is fixed
// (docs/hub-interface.md, "Conformance workloads").
//
// A conformance task's prompt starts with the line
//
//	silo-conformance/v1 <workload>
//
// and a Grain that recognises it may short-circuit the task: do exactly
// what the workload says, and nothing else.
package workload

import "strings"

// Marker begins the first line of every conformance task's prompt.
const Marker = "silo-conformance/v1"

// The workloads.
const (
	// Run emits a status event, an artifact event for the fixed change
	// (Diff, by a ref of the Grain's choosing, served on the view
	// stream), a result event, and the result done.
	Run = "run"
	// Hold emits Started, then a status event every second or so, and
	// never ends on its own: only a cancel ends it.
	Hold = "hold"
	// Approve emits Started and an approval_request with ID ApprovalID,
	// then waits for its approve: allow ends it done, deny failed.
	Approve = "approve"
	// Interactive is a session (kind interactive): it emits Started and
	// awaiting_reply waiting, then, taking one message in, a message
	// event naming it, awaiting_reply resumed with its ID, and the
	// result done.
	Interactive = "interactive"
)

// Started is the status message a workload emits once for each run of
// it: two of them in one task is the Grain running it twice.
const Started = "silo-conformance: started"

// ApprovalID is the ID of the Approve workload's approval request.
const ApprovalID = "silo-conformance"

// Diff is the fixed tiny change the Run workload makes, exactly the
// bytes its artifact's view serves.
const Diff = `--- /dev/null
+++ b/SILO-CONFORMANCE
@@ -0,0 +1 @@
+This change was made by a Grain running the silo conformance kit.
`

// Prompt is a workload's prompt.
func Prompt(work string) string {
	return Marker + " " + work + "\n\nThis is a silo conformance task (docs/conformance.md). A Grain that recognises the first line does exactly what its workload says in docs/hub-interface.md, and nothing else.\n"
}

// Parse is the workload a prompt names, or "" when it is not a
// conformance task's.
func Parse(prompt string) string {
	line, _, _ := strings.Cut(prompt, "\n")
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), Marker+" ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(rest)
}
