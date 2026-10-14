// Adapted from github.com/bwsalmon/grain pkg/model/ref_push.go and pkg/gitproxy/refupdates.go (declaredUpdate) at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

package gitproxy

// Ref patterns: which refs a push may update, and how.
//
// A rule names refs by pattern, refs/heads/<branch> or refs/tags/<tag>,
// and says whether a matching ref may be force-moved or deleted:
//
//	refs/heads/silo/*            create and fast-forward silo/<x>
//	refs/tags/v*  (force)        also move an existing tag
//	refs/heads/scratch (delete)  also delete it
//
// Everything a rule does not name is refused, so a deletion and a tag are
// each refused unless a rule names them (design §9.1): a tag is only
// matched by a refs/tags/ pattern, a deletion only by a rule that allows
// one. This is grain's declared push set (model.RefPushSet) and its
// checker (declaredUpdate), with the policy that holds them left to the
// caller: silo's StaticPolicy has one set per repository, grain's adds
// the run's own branch to its template's.

import (
	"fmt"
	"path"
	"strings"
)

// RefRule is one ref pattern a push may update.
type RefRule struct {
	// Pattern is a full ref name, refs/heads/<branch> or
	// refs/tags/<tag>, where `*` and `?` match as path.Match matches
	// them: within one path segment, so refs/tags/v* is every tag
	// starting with v and refs/heads/release/* is one level of branches
	// under release/.
	Pattern string `json:"ref"`
	// Force allows an update the proxy cannot show is a fast-forward:
	// a branch moved to a commit that does not descend from where it
	// was, or a tag that already exists moved at all.
	Force bool `json:"force,omitempty"`
	// Delete allows deleting a matching ref.
	Delete bool `json:"delete,omitempty"`
}

// RefPrefixes is the two namespaces a rule may name refs in. Nothing
// else -- refs/pull/*, refs/notes/*, refs/meta/* -- is a ref an agent
// pushes.
var RefPrefixes = []string{"refs/heads/", "refs/tags/"}

// ValidateRefPattern says why pattern is not one a rule may hold, or
// nil. The rule is "explicit": a full ref name in one of RefPrefixes,
// globbing only with `*` and `?`, and naming at least one literal
// character after the prefix, so `refs/heads/*` -- every branch, the
// default one included -- has to be spelt out as the branches it means.
func ValidateRefPattern(pattern string) error {
	if pattern != strings.TrimSpace(pattern) || pattern == "" {
		return fmt.Errorf("%q is not a ref name", pattern)
	}
	var rest string
	prefixed := false
	for _, p := range RefPrefixes {
		if strings.HasPrefix(pattern, p) {
			rest, prefixed = strings.TrimPrefix(pattern, p), true
			break
		}
	}
	if !prefixed {
		return fmt.Errorf("%q: a push pattern names a full ref under %s", pattern, strings.Join(RefPrefixes, " or "))
	}
	if strings.ContainsAny(pattern, "[]\\ ~^:") || strings.Contains(pattern, "..") || strings.Contains(pattern, "//") ||
		strings.HasSuffix(pattern, "/") || strings.HasSuffix(pattern, ".lock") {
		return fmt.Errorf("%q is not a ref name this proxy accepts in a push pattern (only `*` and `?` glob)", pattern)
	}
	if strings.Trim(rest, "*?/") == "" {
		return fmt.Errorf("%q would match every ref under %s -- a push pattern names the refs it pushes, "+
			"so spell out the branches or tags it means", pattern, strings.TrimSuffix(pattern, rest))
	}
	if _, err := path.Match(pattern, pattern); err != nil {
		return fmt.Errorf("%q is not a valid pattern: %w", pattern, err)
	}
	return nil
}

// Matches reports whether ref is one of the refs r names.
func (r RefRule) Matches(ref string) bool {
	ok, err := path.Match(r.Pattern, ref)
	return err == nil && ok
}

// String is how one rule reads in a refusal or an audit line.
func (r RefRule) String() string {
	var flags []string
	if r.Force {
		flags = append(flags, "force")
	}
	if r.Delete {
		flags = append(flags, "delete")
	}
	if len(flags) == 0 {
		return r.Pattern
	}
	return r.Pattern + " (" + strings.Join(flags, ", ") + ")"
}

// RefRules is a whole set of rules.
type RefRules []RefRule

// Validate checks every rule's pattern.
func (s RefRules) Validate() error {
	for _, r := range s {
		if err := ValidateRefPattern(r.Pattern); err != nil {
			return err
		}
	}
	return nil
}

// Allows is what the set says about one ref: whether any rule names it
// at all, and whether any rule naming it allows a force-move or a
// deletion. Rules union: two that both match a ref give it the widest of
// what they say, which is what somebody who wrote both meant. matchedBy
// is the first matching rule's pattern.
func (s RefRules) Allows(ref string) (matched, force, del bool, matchedBy string) {
	for _, r := range s {
		if !r.Matches(ref) {
			continue
		}
		if !matched {
			matchedBy = r.Pattern
		}
		matched = true
		force = force || r.Force
		del = del || r.Delete
	}
	return matched, force, del, matchedBy
}

// Check checks one update against the set: the kind of update it is
// ("create", "update", "force" or "delete") and the pattern that allowed
// it when the set allows it, or why not. An update of an existing ref
// that no matching rule lets force-move must be shown a fast-forward by
// fastForward; a nil fastForward shows nothing, and an existing tag is
// never moved without force.
func (s RefRules) Check(u RefUpdate, fastForward FastForward) (kind, matchedBy, problem string) {
	matched, force, del, by := s.Allows(u.Ref)
	if !matched {
		return "", "", fmt.Sprintf("%s matches no push pattern", u.Ref)
	}
	kind = UpdateKind(u)
	switch kind {
	case "delete":
		if !del {
			return "", "", fmt.Sprintf("%s may be pushed but not deleted (no pattern naming it allows a deletion)", u.Ref)
		}
		return kind, by, ""
	case "create":
		return kind, by, ""
	}
	if force {
		return "force", by, ""
	}
	if strings.HasPrefix(u.Ref, "refs/tags/") {
		return "", "", fmt.Sprintf("%s already exists, and moving a tag is a force-push no pattern naming it "+
			"allows (`force: true`)", u.Ref)
	}
	var why string
	if fastForward != nil {
		var ok bool
		if ok, why = fastForward(u.New, u.Old); ok {
			return kind, by, ""
		}
	}
	if why != "" {
		why = " (asking the repository for the commits it already has: " + why + ")"
	}
	return "", "", fmt.Sprintf("%s: could not show %s is a fast-forward of %s from the commits this push carries "+
		"or the repository already has%s, and no pattern naming it allows a force-push (`force: true`)",
		u.Ref, shortCommit(u.New), shortCommit(u.Old), why)
}
