package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bwsalmon/silo/internal/audit"
	"github.com/bwsalmon/silo/internal/gitrepo"
	"github.com/bwsalmon/silo/internal/mcp"
	"github.com/bwsalmon/silo/internal/router"
)

// Refresher pulls commits made outside the session into the checkout.
type Refresher struct {
	Checkout *gitrepo.Checkout
	// Branch is the session's work branch.
	Branch string
	Host   Host
	Log    audit.Logger
}

// NewSilo returns the silo tool set: refresh, plus (when fallback is set,
// for CLIs that ignore tools/list_changed) list_tools and call.
func NewSilo(rf *Refresher, rt *router.Router, fallback bool) *set {
	s := &set{}
	s.add("refresh",
		"Fetch the repository and fast-forward this session's checkout to commits made outside the session "+
			"(on the session branch, or on the base branch if the session has not diverged from it), "+
			"then reload every tool set those commits changed. Reports what changed.",
		mcp.Schema(map[string]any{}), rf.refresh)
	if fallback {
		s.add("list_tools",
			"List every tool this session offers now, with its input schema. Tool sets can be added mid-session; "+
				"if one you expect is not among your tools, find it here and call it with silo.call.",
			mcp.Schema(map[string]any{}),
			func(ctx context.Context, _ json.RawMessage) *mcp.ToolResult {
				b, _ := json.MarshalIndent(rt.Tools(), "", "  ")
				return mcp.Text(string(b))
			})
		s.add("call", "Call any tool listed by silo.list_tools by its full name.",
			mcp.Schema(map[string]any{
				"tool":      str("Full tool name, e.g. tickets.search."),
				"arguments": map[string]any{"type": "object", "description": "The tool's arguments."},
			}, "tool"),
			func(ctx context.Context, args json.RawMessage) *mcp.ToolResult {
				var a struct {
					Tool      string          `json:"tool"`
					Arguments json.RawMessage `json:"arguments"`
				}
				if err := decode(args, &a); err != nil {
					return mcp.Errorf("%v", err)
				}
				if strings.HasPrefix(a.Tool, "silo"+router.Sep) {
					return mcp.Errorf("call silo's own tools directly")
				}
				return rt.Call(ctx, a.Tool, a.Arguments)
			})
	}
	return s
}

func (rf *Refresher) refresh(ctx context.Context, _ json.RawMessage) *mcp.ToolResult {
	c := rf.Checkout
	if c.Remote == "" {
		return mcp.Text("This repository has no remote; there is nothing to fetch.")
	}
	before, err := c.Head(ctx)
	if err != nil {
		return mcp.Errorf("%v", err)
	}
	if _, err := c.Git(ctx, "fetch", "--prune", c.Remote); err != nil {
		return mcp.Errorf("%v", err)
	}
	var notes []string
	for _, ref := range []string{c.Remote + "/" + rf.Branch, c.StartPoint()} {
		if _, err := c.Git(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err != nil {
			continue
		}
		if _, err := c.Git(ctx, "merge-base", "--is-ancestor", ref, "HEAD"); err == nil {
			continue // nothing new there
		}
		if _, err := c.Git(ctx, "merge", "--ff-only", "-q", ref); err != nil {
			notes = append(notes, fmt.Sprintf("%s has new commits, but the session branch has diverged from it, so they were not pulled in.", ref))
			continue
		}
		notes = append(notes, "Fast-forwarded to "+ref+".")
	}
	after, _ := c.Head(ctx)
	rf.Log.Log("refresh", map[string]any{"from": before, "to": after})
	if after == before {
		if len(notes) == 0 {
			notes = append(notes, "Already up to date.")
		}
		return mcp.Text(strings.Join(notes, "\n"))
	}
	paths, err := c.ChangedPaths(ctx, before, after)
	if err != nil {
		return mcp.Errorf("%v", err)
	}
	log, _ := c.Git(ctx, "log", "--oneline", "--no-decorate", before+".."+after)
	out := append(notes, "New commits:\n"+strings.TrimRight(log, "\n"), "Changed files:\n"+clip(strings.Join(paths, "\n"), 16*1024))
	ok := true
	if names := router.ChangedToolsets(paths); len(names) > 0 {
		var report string
		report, ok = rf.Host.ReloadRepoSets(ctx, names)
		out = append(out, "Tool sets:\n"+report)
	}
	res := mcp.Text(strings.Join(out, "\n"))
	res.IsError = !ok
	return res
}
