package granule

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/bwsalmon/silo/v2/sandbox"
)

// Guest is what every granule guest shares however `granule exec`
// reaches it -- `granule vm exec` on the local Docker, or `kubectl exec
// ... granule exec` in a pod (sandbox/k8s): the script that loads the
// sandbox's environment, and the file tools, which run through Exec as
// the container kinds' do.
type Guest struct {
	// Exec runs one command in the guest.
	Exec func(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error)
	// Workdir is where a relative path is; empty means the guest
	// account's home directory.
	Workdir string
}

// execScript loads the sandbox's environment, records the command's
// pid so that a cancelled Exec can kill it in the guest (killing
// granule alone would leave it running there), and becomes the
// command.
const execScript = `echo $$ > "/tmp/.silo-exec-$0" 2>/dev/null
if [ -r ` + EnvFile + ` ]; then set -a; . ` + EnvFile + `; set +a; fi
exec "$@"`

// killScript kills a recorded command and its descendants.
const killScript = `f="/tmp/.silo-exec-$0"; [ -f "$f" ] || exit 0
kill_tree() { for c in $(cat /proc/$1/task/*/children 2>/dev/null); do kill_tree "$c"; done; kill -KILL "$1" 2>/dev/null; }
kill_tree "$(cat "$f")"; rm -f "$f"`

// ExecCommand returns the `granule exec` (or `granule vm exec`) flags
// and guest argv that run req as tag: in req.Cwd, or workdir, with
// req.Env on top of the sandbox's environment, which execScript loads.
func ExecCommand(req sandbox.ExecRequest, workdir, tag string) (flags, argv []string, err error) {
	keys, err := envKeys(req.Env)
	if err != nil {
		return nil, nil, err
	}
	cwd := req.Cwd
	if cwd == "" {
		cwd = workdir
	}
	if cwd != "" {
		if !strings.HasPrefix(cwd, "/") && workdir != "" {
			cwd = workdir + "/" + cwd
		}
		flags = append(flags, "-w", cwd)
	}
	argv = []string{"/bin/sh", "-c", execScript, tag}
	if len(keys) > 0 {
		// After the sandbox's environment, so that the request's wins.
		argv = append(argv, "env")
		for _, k := range keys {
			argv = append(argv, k+"="+req.Env[k])
		}
	}
	return flags, append(argv, req.Argv...), nil
}

// KillCommand is the guest argv that kills the command ExecCommand ran
// as tag, and its descendants.
func KillCommand(tag string) []string {
	return []string{"/bin/sh", "-c", killScript, tag}
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func envKeys(env map[string]string) ([]string, error) {
	keys := make([]string, 0, len(env))
	for k := range env {
		if !envName.MatchString(k) {
			return nil, fmt.Errorf("%q is not a valid environment variable name", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

// CheckEnv reports a variable name the guest's shell could not take.
func CheckEnv(env map[string]string) error {
	_, err := envKeys(env)
	return err
}

// envFile is env as sh assignments, each value single-quoted.
func envFile(env map[string]string, keys []string) []byte {
	var buf bytes.Buffer
	for _, k := range keys {
		fmt.Fprintf(&buf, "export %s=%s\n", k, shellQuote(env[k]))
	}
	return buf.Bytes()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Setup readies a booted guest: it makes workdir, and writes env to
// EnvFile (mode 0600), over stdin so that no value is on a command
// line.
func (g Guest) Setup(ctx context.Context, workdir string, env map[string]string) error {
	keys, err := envKeys(env)
	if err != nil {
		return err
	}
	setup := `set -e; if [ -n "$1" ]; then mkdir -p -- "$1"; fi
if [ "$2" = 1 ]; then umask 077; mkdir -p -- "$(dirname -- "$3")"; cat > "$3"; fi`
	hasEnv := "0"
	var envData []byte
	if len(keys) > 0 {
		hasEnv = "1"
		envData = envFile(env, keys)
	}
	_, err = g.Run(ctx, envData, setup, workdir, hasEnv, EnvFile)
	return err
}

// Run is Exec of a shell script with arguments, as a plain call: a
// non-zero exit is an error carrying the script's stderr.
func (g Guest) Run(ctx context.Context, stdin []byte, script string, args ...string) ([]byte, error) {
	argv := append([]string{"/bin/sh", "-c", script, "silo"}, args...)
	res, err := g.Exec(ctx, sandbox.ExecRequest{Argv: argv, Stdin: stdin, Cwd: "/"})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(string(res.Stderr))
		if msg == "" {
			msg = fmt.Sprintf("exit status %d", res.ExitCode)
		}
		return nil, errors.New(msg)
	}
	return res.Stdout, nil
}

// abs makes a relative path relative to the workdir, as the container
// kinds' file tools take it.
func (g Guest) abs(path string) string {
	if strings.HasPrefix(path, "/") || g.Workdir == "" {
		return path
	}
	return g.Workdir + "/" + path
}

// ReadFile returns at most limit bytes of the file at path; a relative
// path is relative to the sandbox's workdir.
func (g Guest) ReadFile(ctx context.Context, path string, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, nil
	}
	out, err := g.Run(ctx, nil, `if [ -d "$1" ]; then echo "$1: is a directory" >&2; exit 1; fi; exec head -c "$2" -- "$1"`,
		g.abs(path), strconv.FormatInt(limit, 10))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if int64(len(out)) > limit {
		out = out[:limit]
	}
	return out, nil
}

// WriteFile writes data to path, creating its parent directories, and
// sets its mode.
func (g Guest) WriteFile(ctx context.Context, path string, data []byte, mode fs.FileMode) error {
	_, err := g.Run(ctx, data, `mkdir -p -- "$(dirname -- "$1")" && cat > "$1" && chmod "$2" -- "$1"`,
		g.abs(path), fmt.Sprintf("%04o", mode.Perm()))
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// DeliverFile writes a credential file, mode 0400, by writing a
// temporary file beside it and renaming it into place, so that a reader
// sees the old file or the new one and never a partial one.
func (g Guest) DeliverFile(ctx context.Context, path string, data []byte) error {
	_, err := g.Run(ctx, data, `umask 077; d="$(dirname -- "$1")"; mkdir -p -- "$d" || exit 1
t="$d/.silo-deliver.$$"
if cat > "$t" && chmod 0400 "$t" && mv -f -- "$t" "$1"; then exit 0; fi
rm -f -- "$t"; exit 1`, g.abs(path))
	if err != nil {
		return fmt.Errorf("delivering %s: %w", path, err)
	}
	return nil
}
