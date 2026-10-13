package gitserve

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/gitstore"
	"github.com/bwsalmon/silo/pkg/repos"
)

// HookArg is the first argument silod's binary is given when it runs
// as the pre-receive hook: its main hands the rest to HookMain.
const HookArg = "git-hook"

// hookScript is the pre-receive hook in silod's hooks directory, which
// every repo's core.hooksPath names. It runs silod's own binary, which
// the git service names in the environment it runs receive-pack in;
// run any other way, it refuses the push.
const hookScript = `#!/bin/sh
# silod's pre-receive hook (package gitserve): silod's own binary.
if [ -z "$` + envHook + `" ]; then
	echo "silo: pushes to this repo are served only by silod" >&2
	exit 1
fi
exec "$` + envHook + `" ` + HookArg + ` pre-receive
`

// WriteHooks writes silod's hooks directory at dir, as
// gitstore.Host.HooksDir names it: a read-only script in a directory
// only silod's uid may write.
func WriteHooks(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return err
	}
	p := filepath.Join(dir, "pre-receive")
	_ = os.Chmod(p, 0o755)
	if err := os.WriteFile(p, []byte(hookScript), 0o555); err != nil {
		return err
	}
	return os.Chmod(p, 0o555)
}

// HookMain is the pre-receive hook (local-git.md, "How a request is
// served", step 6), run by receive-pack in the repo with the updates
// on stdin, one "<old> <new> <ref>" a line. It refuses the push, with
// a message on stderr and a non-zero result, if any update is
// outside refs/heads/ and refs/tags/, if the push adds a blob over
// the size limit, or if the silo started the push over its disk_gb
// grant (a push that only deletes is let through). Otherwise it writes
// local archive refs for the tips the push would orphan, and lets it
// in.
func HookMain(args []string, stdin io.Reader, stderr io.Writer) int {
	if len(args) != 1 || args[0] != "pre-receive" {
		fmt.Fprintf(stderr, "silo: unknown git hook %q\n", strings.Join(args, " "))
		return 2
	}
	var ups []update
	sc := bufio.NewScanner(stdin)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 3 {
			continue
		}
		ups = append(ups, update{old: f[0], new: f[1], ref: f[2]})
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(stderr, "silo: reading the push's updates: %v\n", err)
		return 1
	}
	var refused []string
	onlyDeletes := true
	for _, u := range ups {
		if !strings.HasPrefix(u.ref, "refs/heads/") && !strings.HasPrefix(u.ref, "refs/tags/") {
			refused = append(refused, fmt.Sprintf("silo: refusing %s: only refs/heads/ and refs/tags/ are kept, the namespaces the uber repo mirrors", u.ref))
		}
		onlyDeletes = onlyDeletes && zero(u.new)
	}
	if over := os.Getenv(envOver); over != "" && !onlyDeletes {
		refused = append(refused, over)
	}
	if len(refused) == 0 {
		big, err := bigBlobs(ups)
		if err != nil {
			fmt.Fprintf(stderr, "silo: checking the push's blobs: %v\n", err)
			return 1
		}
		refused = append(refused, big...)
	}
	if len(refused) > 0 {
		for _, m := range refused {
			fmt.Fprintln(stderr, m)
		}
		return 1
	}
	if err := archive(ups); err != nil {
		fmt.Fprintf(stderr, "silo: writing archive refs: %v\n", err)
		return 1
	}
	return 0
}

type update struct{ old, new, ref string }

func zero(sha string) bool { return strings.Trim(sha, "0") == "" }

// hookGit runs git in the hook's own environment (receive-pack's, with
// the push's quarantine), or, with outside, without the quarantine:
// git refuses ref updates inside one.
func hookGit(stdin []byte, outside bool, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	env := os.Environ()
	if outside {
		env = env[:0:0]
		for _, kv := range os.Environ() {
			k, _, _ := strings.Cut(kv, "=")
			if k != "GIT_QUARANTINE_PATH" && k != "GIT_OBJECT_DIRECTORY" && k != "GIT_ALTERNATE_OBJECT_DIRECTORIES" {
				env = append(env, kv)
			}
		}
	}
	cmd.Env = env
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// bigBlobs is a refusal for each blob the push adds over the limit:
// one the uber repo would refuse should fail now, with a message,
// rather than stall the mirror.
func bigBlobs(ups []update) ([]string, error) {
	limit, err := strconv.ParseInt(os.Getenv(envMaxBlob), 10, 64)
	if err != nil || limit <= 0 {
		return nil, nil
	}
	args := []string{"rev-list", "--objects"}
	for _, u := range ups {
		if !zero(u.new) {
			args = append(args, u.new)
		}
	}
	if len(args) == 2 {
		return nil, nil
	}
	objs, err := hookGit(nil, false, append(args, "--not", "--all")...)
	if err != nil {
		return nil, err
	}
	out, err := hookGit([]byte(objs), false, "cat-file", "--batch-check=%(objecttype) %(objectsize) %(objectname) %(rest)")
	if err != nil {
		return nil, err
	}
	var refused []string
	mb := func(n int64) string { return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB" }
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.SplitN(line, " ", 4)
		if len(f) < 3 || f[0] != "blob" {
			continue
		}
		n, _ := strconv.ParseInt(f[1], 10, 64)
		if n > limit {
			name := f[2][:12]
			if len(f) == 4 && f[3] != "" {
				name = f[3] + " (" + name + ")"
			}
			refused = append(refused, fmt.Sprintf("silo: %s is %s, over the %s limit on a file that the uber repo takes", name, mb(n), mb(limit)))
		}
	}
	return refused, nil
}

// archive writes refs/silo-archive/<UTC ts>-<seq>/heads/<branch> (or
// /tags/<tag>) at the old tip of each update that isn't a
// fast-forward, or deletes the ref, when no other ref reaches that tip
// (local-git.md, "Local archive refs", call L48). Refs the push also
// changes don't count as reaching it.
func archive(ups []update) error {
	pushed := map[string]bool{}
	for _, u := range ups {
		pushed[u.ref] = true
	}
	var orphans []update
	for _, u := range ups {
		if zero(u.old) {
			continue
		}
		if !zero(u.new) && strings.HasPrefix(u.ref, "refs/heads/") {
			if _, err := hookGit(nil, false, "merge-base", "--is-ancestor", u.old, u.new); err == nil {
				continue // a fast-forward
			}
		}
		reached := false
		if commit, err := hookGit(nil, false, "rev-parse", "--verify", "--quiet", u.old+"^{commit}"); err == nil {
			out, err := hookGit(nil, false, "for-each-ref", "--format=%(refname)", "--contains", strings.TrimSpace(commit))
			if err != nil {
				return err
			}
			for _, r := range strings.Fields(out) {
				reached = reached || !pushed[r]
			}
		}
		if !reached {
			orphans = append(orphans, u)
		}
	}
	if len(orphans) == 0 {
		return nil
	}
	ts := time.Now().UTC().Format(repos.ArchiveTime)
	dir := ""
	for seq := 1; ; seq++ {
		dir = fmt.Sprintf("%s%s-%d/", gitstore.HideRefs, ts, seq)
		out, err := hookGit(nil, true, "for-each-ref", "--count=1", dir)
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) == "" {
			break
		}
	}
	var in strings.Builder
	for _, u := range orphans {
		fmt.Fprintf(&in, "create %s%s %s\n", dir, strings.TrimPrefix(u.ref, "refs/"), u.old)
	}
	_, err := hookGit([]byte(in.String()), true, "update-ref", "--stdin")
	return err
}
