package ssh

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// SessionFile is the file in each remote session directory that records
// which session made it, and from where: "id=<session> origin=<origin>".
const SessionFile = "session"

// Origin names the invoking machine and user in remote session
// directories, so that silo gc --host only removes what this user on
// this machine left behind, and never a live session another machine
// is running against the same remote account.
func Origin() string {
	h, _ := os.Hostname()
	return fmt.Sprintf("%s:%d", h, os.Getuid())
}

var validID = regexp.MustCompile(`^s-[a-z2-7]{12}$`)

// listScript lists the remote session directories this remote user
// owns, one per line: the directory, a tab, and its session file.
const listScript = `seen=
for d in "${XDG_RUNTIME_DIR:-/tmp}"/silo.* /tmp/silo.*; do
  [ -d "$d" ] && [ -O "$d" ] || continue
  case " $seen " in *" $d "*) continue ;; esac
  seen="$seen $d"
  printf '%s\t%s\n' "$d" "$(tr '\n' ' ' < "$d/session" 2>/dev/null)"
done
`

// GCOptions configure GCHost.
type GCOptions struct {
	// Live reports whether a session on this machine is still running.
	Live func(id string) bool
	// Origin is this machine's Origin(), when empty.
	Origin string
	// Keep is a session id never removed: the session running the gc.
	Keep string
}

// gcRemote removes the remote session directories whose session, made
// from this origin, is no longer live (design §5.3, §12.6), and returns
// what it removed.
func gcRemote(ctx context.Context, r runner, opts GCOptions) ([]string, error) {
	origin := opts.Origin
	if origin == "" {
		origin = Origin()
	}
	res, err := r(ctx, "sh -s", []byte(listScript))
	if err != nil {
		return nil, err
	}
	if res.Code != 0 {
		return nil, res.err("listing remote session directories")
	}
	var stale []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		dir, rec, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		fields := map[string]string{}
		for _, f := range strings.Fields(rec) {
			if k, v, ok := strings.Cut(f, "="); ok {
				fields[k] = v
			}
		}
		id := fields["id"]
		// A directory with no session file yet may be a session still
		// being set up: it is left alone.
		if !validID.MatchString(id) || fields["origin"] != origin || id == opts.Keep {
			continue
		}
		if opts.Live != nil && opts.Live(id) {
			continue
		}
		stale = append(stale, dir)
	}
	if len(stale) == 0 {
		return nil, nil
	}
	res, err = r(ctx, "rm -rf -- "+QuoteArgv(stale), nil)
	if err != nil {
		return nil, err
	}
	if res.Code != 0 {
		return nil, res.err("removing stale remote session directories")
	}
	return stale, nil
}

// GCHost is silo gc --host (design §3.1, §12.6): on its own connection
// to the host, it removes the stale remote session directories left by
// sessions from this machine that are gone.
func GCHost(ctx context.Context, t Transport, opts GCOptions) ([]string, error) {
	return gcRemote(ctx, direct(t, false), opts)
}
