package ssh

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwsalmon/silo/v2/location"
)

// CheckResult is one line of silo run --dry-run's checklist.
type CheckResult struct {
	Name   string
	OK     bool
	Detail string
}

// Check is silo run --dry-run's part for an ssh location (design §2.4):
// the host is reachable without prompting, its host key is accepted, and
// the CLI's binary and login are there. It changes nothing on the host.
func Check(ctx context.Context, t Transport, s *location.Session) []CheckResult {
	host := t.Host()
	r := direct(t, true)
	res, err := r(ctx, "true", nil)
	reach := CheckResult{Name: "ssh " + host + ": reachable", OK: err == nil && res.Code == 0}
	keyOK := true
	switch {
	case reach.OK:
		reach.Detail = "ssh -o BatchMode=yes " + host + " true succeeded"
	case err != nil:
		reach.Detail = err.Error()
	case unknownKey.MatchString(res.Stderr) || strings.Contains(res.Stderr, "REMOTE HOST IDENTIFICATION HAS CHANGED"):
		keyOK = false
		reach.Detail = "the host key is not accepted"
	default:
		reach.Detail = strings.TrimSpace(res.Stderr) + fmt.Sprintf(" (ssh -o BatchMode=yes %s true exited %d; silo run may still prompt for a passphrase)", host, res.Code)
	}
	key := CheckResult{Name: "ssh " + host + ": host key accepted", OK: keyOK, Detail: "known_hosts has the host's key"}
	if !keyOK {
		key.Detail = strings.TrimSpace(res.Stderr) + "; " + AcceptKeyFix(ctx, t)
	}
	out := []CheckResult{reach, key}
	if !reach.OK {
		return out
	}
	info, perr := probe(ctx, r, host, s)
	bin := CheckResult{Name: "ssh " + host + ": agent CLI " + s.CLI, OK: info.Binary != ""}
	if bin.OK {
		bin.Detail = strings.TrimSpace(info.Binary + " " + info.Version)
	} else if perr != nil {
		bin.Detail = perr.Error()
	}
	out = append(out, bin)
	if bin.OK && s.AmbientAuth {
		if _, ok := logins[s.CLI]; ok {
			login := CheckResult{Name: "ssh " + host + ": " + s.CLI + " login", OK: true, Detail: "logged in"}
			if perr != nil {
				login.OK, login.Detail = false, perr.Error()
			}
			out = append(out, login)
		}
	} else if bin.OK && perr != nil {
		bin.OK, bin.Detail = false, perr.Error()
		out[len(out)-1] = bin
	}
	return out
}
