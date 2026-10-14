package session_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/v2/control"
	"github.com/bwsalmon/silo/v2/session"
)

const localScripts = `silo: v2
name: t
agent:
  cli: claude
  mode: headless
  prompt: "deploy it"
local:
  scripts:
    list:
      - path: scripts/deploy
        approve: true
        timeout: 30s
credentials:
  - name: shared
    from: {env: SILO_TEST_DELIVERED}
    to: [{local: deploy, env: TOKEN}]
`

func (e *env) script(name, content string) {
	e.t.Helper()
	dir := filepath.Join(e.root, "silo", "scripts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755); err != nil {
		e.t.Fatal(err)
	}
}

// answer approves (or denies) the first call to wait on the running
// session's control socket, as silo approve does.
func (e *env) answer(deny bool) chan session.ApproveResult {
	out := make(chan session.ApproveResult, 1)
	go func() {
		deadline := time.Now().Add(time.Minute)
		for time.Now().Before(deadline) {
			socks, _ := filepath.Glob(filepath.Join(e.root, "run", "silo", "s-*", control.FileName))
			for _, sock := range socks {
				var res session.ApproveResult
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := control.Call(ctx, sock, "approve", session.ApproveParams{Deny: deny}, &res)
				cancel()
				if err == nil {
					out <- res
					return
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		close(out)
	}()
	return out
}

func TestLocalScriptsEndToEnd(t *testing.T) {
	for _, deny := range []bool{false, true} {
		name := "approved"
		if deny {
			name = "denied"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.script("deploy", "#!/bin/sh\necho \"deploying $1 with $TOKEN\"\ncase \"$(export -p)\" in *SILO_*) echo leaked;; *) echo clean;; esac\nexit 4\n")
			e.script("status", "#!/bin/sh\necho ok\n")
			e.fake(fakeConfig{Call: "local_deploy", Args: map[string]any{"args": []any{"feature $(x)"}}})
			f := e.silofile(localScripts)
			answered := e.answer(deny)
			res := e.run(f, nil)
			if res.code != 0 {
				t.Fatalf("exit %d (%v)\n%s", res.code, res.err, res.stderr)
			}
			r := e.report()
			if !strings.Contains(strings.Join(r.Tools, ","), "local_deploy,local_status") {
				t.Errorf("tools %v", r.Tools)
			}
			a, ok := <-answered
			if !ok || a.Tool != "local_deploy" || a.Approved == deny || len(a.Args) != 1 || a.Args[0] != "feature $(x)" {
				t.Fatalf("answer %+v", a)
			}
			if !strings.Contains(res.stderr, "waits for your approval") || !strings.Contains(res.stderr, "silo approve s-") {
				t.Errorf("no approval request on stderr:\n%s", res.stderr)
			}
			rec := e.sessionGone()
			audit, _ := os.ReadFile(filepath.Join(rec.StateDir, "audit.jsonl"))
			if deny {
				if !strings.Contains(r.Result, "denied") {
					t.Errorf("denied call: result %q error %q", r.Result, r.Error)
				}
				if !strings.Contains(string(audit), `"outcome":"error"`) {
					t.Errorf("audit:\n%s", audit)
				}
				return
			}
			// A non-zero exit is a result, with the credential delivered to
			// the script and SILO_* stripped.
			if !strings.Contains(r.Result, `"exit_code": 4`) || !strings.Contains(r.Result, `deploying feature $(x) with delivered-value\nclean\n`) {
				t.Errorf("result %q error %q", r.Result, r.Error)
			}
			if !strings.Contains(string(audit), `"tool":"local_deploy","args":{"args":["feature $(x)"]}`) ||
				!strings.Contains(string(audit), `\"exit_code\": 4`) {
				t.Errorf("audit:\n%s", audit)
			}
			if strings.Contains(string(audit), "delivered-value") || !strings.Contains(string(audit), "[credential:shared]") {
				t.Errorf("the credential is not redacted in the audit log:\n%s", audit)
			}
		})
	}
}

func TestRefusedScriptFailsTheSession(t *testing.T) {
	e := newEnv(t)
	e.script("deploy", "#!/bin/sh\n")
	f := e.silofile(localScripts)
	os.Chmod(filepath.Join(e.root, "silo", "scripts"), 0o777)
	res := e.run(f, nil)
	if res.code != session.ExitInvalid || res.err == nil || !strings.Contains(res.err.Error(), "writable by group or others") {
		t.Fatalf("exit %d (%v)", res.code, res.err)
	}
}
