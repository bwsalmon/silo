package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bwsalmon/silo/internal/config"
	"github.com/bwsalmon/silo/internal/gitrepo"
	"github.com/bwsalmon/silo/internal/mcp"
	"github.com/bwsalmon/silo/internal/vendor"
)

// Vendorer is silo.vendor: it copies tool sets from another context
// repository into .silo/tools/vendor/<alias>/ as a repo-write proposal.
// Vendored code runs automatically once committed, so the person sees
// all of it, as a diff, before it lands.
type Vendorer struct {
	// Config says where repositories are (repos.root, git_base).
	Config *config.Config
	// Checkout is the session's checkout.
	Checkout string
	Writer   *Writer
}

func (v *Vendorer) addTo(s *set) {
	s.add("vendor",
		"Vendor tool sets from another context repository into this one: copies .silo/tools/<set>/ of each set from <repo> at <ref> "+
			"into .silo/tools/vendor/<alias>/<set>/ with a VENDOR.lock recording the upstream commit, as a proposed commit the person approves "+
			"after seeing all of the vendored code. Once committed the sets load as <alias>.<set>.<tool>. "+
			"With update, re-vendors an alias from its VENDOR.lock (at ref, if given, else the locked ref). "+
			"The upstream's own vendored sets are never included.",
		mcp.Schema(map[string]any{
			"repo":     str("Upstream repository, relative to the configured repos root (as for `silo run`). Not needed with update."),
			"ref":      str("Branch, tag or commit to vendor; default: the upstream's default branch, or with update the locked ref."),
			"toolsets": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Tool sets to vendor; default all of the upstream's (with update, the locked ones)."},
			"alias":    str("Namespace for the vendored sets: lowercase letters, digits, '-' and '_'; not one of this repository's own tool-set names."),
			"update":   flag("Re-vendor an existing alias."),
			"message":  str("Commit message; default one naming the upstream and its commit."),
		}, "alias"), v.vendor)
}

func (v *Vendorer) vendor(ctx context.Context, args json.RawMessage) *mcp.ToolResult {
	var a struct {
		Repo     string   `json:"repo"`
		Ref      string   `json:"ref"`
		Toolsets []string `json:"toolsets"`
		Alias    string   `json:"alias"`
		Update   bool     `json:"update"`
		Message  string   `json:"message"`
	}
	if err := decode(args, &a); err != nil {
		return mcp.Errorf("%v", err)
	}
	req, err := vendor.NewRequest(v.Checkout, a.Repo, a.Ref, a.Alias, a.Toolsets, a.Update)
	if err != nil {
		return mcp.Errorf("%v", err)
	}
	up, err := gitrepo.Open(ctx, v.Config, req.Repo)
	if err != nil {
		return mcp.Errorf("%v", err)
	}
	plan, err := vendor.Build(ctx, up, req)
	if err != nil {
		return mcp.Errorf("%v", err)
	}
	stale, err := vendor.Stale(v.Checkout, plan)
	if err != nil {
		return mcp.Errorf("%v", err)
	}
	var edits []Edit
	for _, f := range plan.Files {
		c := f.Content
		edits = append(edits, Edit{Path: f.Path, Content: &c, Mode: f.Mode})
	}
	for _, p := range stale {
		edits = append(edits, Edit{Path: p, Delete: true})
	}
	msg := a.Message
	if strings.TrimSpace(msg) == "" {
		verb := "Vendor"
		if a.Update {
			verb = "Update vendored"
		}
		msg = fmt.Sprintf("%s %s\n\nCopied from %s at %s (commit %s) into %s/, as recorded in its %s.",
			verb, plan.Summary(), plan.Lock.Repo, plan.Lock.Ref, plan.Lock.Commit, vendor.Dir(plan.Alias), config.VendorLock)
	}
	res := v.Writer.Propose(ctx, msg, edits, nil)
	if len(plan.Warnings) > 0 {
		res.Content = append(res.Content, mcp.Text("Warnings:\n"+strings.Join(plan.Warnings, "\n")).Content...)
	}
	return res
}
