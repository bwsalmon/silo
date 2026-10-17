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
	// File files a task upward from the hub task running it, in the
	// repository its prompt names (Target): a file_task whose
	// origin_task is that task and whose node_task is the Grain's own
	// task waiting on the new one, and whose prompt is Prompt(Run).
	// It emits Started, then Filed with the filed task's ID, then waits
	// for that task's dependency_done: on done it emits Unblocked with
	// the ID and ends done; on any other state it ends failed.
	File = "file"
)

// Started is the status message a workload emits once for each run of
// it: two of them in one task is the Grain running it twice.
const Started = "silo-conformance: started"

// Filed and Unblocked begin the status messages of the File workload,
// each followed by a space and the filed task's hub ID: Filed once the
// hub has answered filed, Unblocked once its dependency_done done has
// come.
const (
	Filed     = "silo-conformance: filed"
	Unblocked = "silo-conformance: unblocked"
)

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

// FilePrompt is the File workload's prompt, filing in repo.
func FilePrompt(repo string) string { return Prompt(File + " " + repo) }

// Parse is the workload a prompt names, or "" when it is not a
// conformance task's.
func Parse(prompt string) string {
	work, _ := parse(prompt)
	return work
}

// Target is the repository a File workload's prompt names, or "".
func Target(prompt string) string {
	_, arg := parse(prompt)
	return arg
}

// parse splits a conformance prompt's first line, after the marker,
// into the workload and what follows it.
func parse(prompt string) (work, arg string) {
	line, _, _ := strings.Cut(prompt, "\n")
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), Marker+" ")
	if !ok {
		return "", ""
	}
	work, arg, _ = strings.Cut(strings.TrimSpace(rest), " ")
	return work, strings.TrimSpace(arg)
}
