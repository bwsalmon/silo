package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bwsalmon/silo/v3/gateway"
	"github.com/bwsalmon/silo/v3/gateway/approval"
)

const rootUsage = `usage: silo root propose [-socket PATH] [-token TOKEN] [-timeout 10m] [-json] BATCH.json

Submits a run_as_root batch (design §4.3) as the root agent and waits until
the human has decided and it has run, or the timeout passes. The root agent
is identified by -token (default $SILO_AGENT_TOKEN) or, with no token, by
its Unix user. In BATCH.json a file write or attachment may give "source"
(a path, relative to BATCH.json) or "content" (text) instead of content_b64.
`

const approvalsUsage = `usage: silo approvals <command> [-socket PATH] ...

  list [-all] [-json]                       pending batches (or all of them)
  show [-json] ID                           what the batch does: files, diffs, commands, flags
  approve [-comment TEXT] [-confirm HASH] ID   approve it; it runs at once, as root
  reject -comment TEXT ID                   reject it, telling the agent why

Deciding needs an admin on the box (root, or a uid in admin_uids).
`

// runRoot is `silo root`, the root agent's side of run_as_root.
func runRoot(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "propose" {
		fmt.Fprint(stderr, rootUsage)
		return errors.New("usage: silo root propose BATCH.json")
	}
	fs := flag.NewFlagSet("root propose", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sock := fs.String("socket", gateway.DefaultSocketPath, "gateway socket")
	tok := fs.String("token", os.Getenv("SILO_AGENT_TOKEN"), "the root agent's session token")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait for the decision and the run")
	asJSON := fs.Bool("json", false, "print the final status as JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: silo root propose [flags] BATCH.json")
	}
	b, err := approval.LoadBatchFile(fs.Arg(0))
	if err != nil {
		return err
	}
	c, err := gateway.Dial(context.Background(), *sock)
	if err != nil {
		return err
	}
	defer c.Close()
	var auth map[string]any
	if *tok != "" {
		auth = map[string]any{"agent_token": *tok}
	}
	sub, st, err := approval.Propose(context.Background(), c.Call, auth, *b, *timeout)
	if err != nil {
		return err
	}
	if *asJSON {
		if err := printJSON(stdout, st); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(stdout, "submitted %s (hash %s), %s\n%s\n", sub.BatchID, sub.ShortHash, sub.Snapshot, approval.Describe(st))
	}
	switch st.Status {
	case approval.StatusFailed, approval.StatusRefused, approval.StatusRejected, approval.StatusCancelled, approval.StatusInterrupted:
		return fmt.Errorf("batch %s %s", st.BatchID, st.Status)
	}
	return nil
}

// runApprovals is `silo approvals`, the human's queue on the box.
func runApprovals(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, approvalsUsage)
		return errors.New("no approvals command")
	}
	fs := flag.NewFlagSet("approvals "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	sock := fs.String("socket", gateway.DefaultSocketPath, "gateway socket")
	var all, asJSON *bool
	var comment, confirm *string
	switch args[0] {
	case "list":
		all = fs.Bool("all", false, "include decided batches")
		asJSON = fs.Bool("json", false, "print JSON")
	case "show":
		asJSON = fs.Bool("json", false, "print the whole record as JSON")
	case "approve", "reject":
		comment = fs.String("comment", "", "a note for the agent")
		confirm = fs.String("confirm", "", "the batch's short hash, required for batches that edit root's instructions")
	case "help", "-h", "--help":
		fmt.Fprint(stdout, approvalsUsage)
		return nil
	default:
		fmt.Fprint(stderr, approvalsUsage)
		return fmt.Errorf("unknown approvals command %q", args[0])
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	c, err := gateway.Dial(context.Background(), *sock)
	if err != nil {
		return err
	}
	defer c.Close()

	switch args[0] {
	case "list":
		var list []approval.Summary
		if err := c.Call("approval.list", map[string]any{"all": *all}, &list); err != nil {
			return err
		}
		if *asJSON {
			return printJSON(stdout, list)
		}
		if len(list) == 0 {
			fmt.Fprintln(stdout, "no batches")
			return nil
		}
		tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tHASH\tSTATUS\tFROM\tSUBMITTED\tFILES\tSTEPS\tFLAGS\tTITLE")
		for _, s := range list {
			flags := strings.Join(s.Flags, ",")
			if s.AutoRule != "" {
				flags = strings.TrimPrefix(flags+",auto:"+s.AutoRule, ",")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\n", s.BatchID, s.ShortHash, s.Status, s.Submitter,
				s.SubmittedAt.Format(time.RFC3339), s.Files, s.Steps, flags, approval.Escape(s.Title))
		}
		return tw.Flush()
	case "show":
		if fs.NArg() != 1 {
			return errors.New("usage: silo approvals show [-json] ID")
		}
		var r approval.Record
		if err := c.Call("approval.show", map[string]any{"batch_id": fs.Arg(0)}, &r); err != nil {
			return err
		}
		if *asJSON {
			return printJSON(stdout, r)
		}
		fmt.Fprintf(stdout, "batch %s: %s\n\n%s", r.ID, r.Status, r.Presentation.Text())
		if r.Status != approval.StatusPending {
			st := approval.Status{BatchID: r.ID, Hash: r.Hash, Title: r.Bound.Title, Status: r.Status, Decision: r.Decision,
				SnapshotRef: r.Snapshot, Files: r.Files, Steps: r.Steps, Error: r.Error}
			fmt.Fprintf(stdout, "\nOutcome: %s\n", approval.Describe(&st))
		}
		return nil
	default: // approve, reject
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: silo approvals %s [flags] ID", args[0])
		}
		if args[0] == "reject" && strings.TrimSpace(*comment) == "" {
			return errors.New("reject needs -comment, so the agent knows what to change")
		}
		var st approval.Status
		params := map[string]any{"batch_id": fs.Arg(0), "decision": args[0], "comment": *comment, "confirm": *confirm}
		if err := c.Call("approval.decide", params, &st); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "batch %s is %s\n", st.BatchID, st.Status)
		return nil
	}
}

func printJSON(w io.Writer, v any) error {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", out)
	return err
}
