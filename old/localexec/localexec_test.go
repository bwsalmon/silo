package localexec

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bwsalmon/silo/old/credentials"
	"github.com/bwsalmon/silo/old/gateway"
	"github.com/bwsalmon/silo/old/silofile"
)

// folder writes a silo folder: the Silofile and files, each written
// with mode 0755 when its content starts with #!, 0644 otherwise.
func folder(t *testing.T, silo string, files map[string]string) *silofile.Silofile {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Silofile"), []byte(silo), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasPrefix(content, "#!") {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	f, err := silofile.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

const header = `silo: v2
name: t
agent: {cli: claude}
`

func snapshot(t *testing.T, f *silofile.Silofile) []Script {
	t.Helper()
	listed, errs := f.LocalScripts()
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	s, err := Snapshot(f, listed)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSnapshotWhichScripts(t *testing.T) {
	f := folder(t, header+`local:
  scripts:
    list:
      - path: tools/deploy
        description: Deploy a preview.
        approve: true
        timeout: 1m
`, map[string]string{
		"tools/deploy":        "#!/bin/sh\n# silo-args: <branch>\necho deploy\n",
		"scripts/Logs.Tail":   "#!/bin/sh\n# silo: Tail the logs\n# silo:   of a service.\n# silo-args: <service> [-n lines]\necho logs\n",
		"scripts/plain":       "#!/bin/sh\necho plain\n",
		"scripts/.hidden":     "#!/bin/sh\necho hidden\n",
		"scripts/notexec":     "echo not executable\n",
		"scripts/sub/nested":  "#!/bin/sh\necho nested\n",
		"scripts/late-silo":   "#!/bin/sh\n" + strings.Repeat("true\n", 25) + "# silo: too late\n",
		"scripts/sub/.hidden": "#!/bin/sh\n",
	})
	scripts := snapshot(t, f)
	var names []string
	byName := map[string]Script{}
	for _, s := range scripts {
		names = append(names, s.Name)
		byName[s.Name] = s
	}
	if got := strings.Join(names, " "); got != "Logs.Tail deploy late-silo plain" {
		t.Fatalf("scripts %q: want listed ones and executables directly in scripts/, without dotfiles or subdirectories", got)
	}
	d := byName["deploy"]
	if d.Tool != "local_deploy" || !d.Approve || d.Timeout != time.Minute {
		t.Errorf("deploy: %+v", d)
	}
	if d.Description != "Deploy a preview.\nUsage: deploy <branch>\nEach call waits for the user to approve it." {
		t.Errorf("deploy description %q", d.Description)
	}
	l := byName["Logs.Tail"]
	if l.Tool != "local_logs_tail" || l.Description != "Tail the logs of a service.\nUsage: Logs.Tail <service> [-n lines]" {
		t.Errorf("Logs.Tail: tool %s description %q", l.Tool, l.Description)
	}
	if p := byName["plain"]; p.Description != "Run plain on the user's machine." || p.Timeout != DefaultTimeout || p.Approve {
		t.Errorf("plain: %+v", p)
	}
	if p := byName["late-silo"]; p.Description != "Run late-silo on the user's machine." {
		t.Errorf("a # silo: line past line 20 was read: %q", p.Description)
	}
	if p := byName["plain"]; p.Size != int64(len("#!/bin/sh\necho plain\n")) || p.Mode.Perm() != 0o755 || len(p.SHA256) != 64 {
		t.Errorf("plain snapshot: %+v", p)
	}
}

func TestSnapshotRefusals(t *testing.T) {
	base := header + "local: {}\n"
	files := func() map[string]string { return map[string]string{"scripts/a": "#!/bin/sh\necho a\n"} }

	t.Run("symlink out of the folder", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "evil")
		os.WriteFile(outside, []byte("#!/bin/sh\n"), 0o755)
		f := folder(t, base, files())
		if err := os.Symlink(outside, filepath.Join(f.Folder, "scripts", "evil")); err != nil {
			t.Fatal(err)
		}
		if _, errs := f.LocalScripts(); len(errs) == 0 || !strings.Contains(errs.Error(), "leaves the silo folder") {
			t.Errorf("listing: %v", errs)
		}
		_, err := Snapshot(f, []silofile.LocalScript{{Name: "evil", Tool: "local_evil", Path: filepath.Join(f.Folder, "scripts", "evil")}})
		if err == nil || !strings.Contains(err.Error(), "outside the silo folder") {
			t.Errorf("snapshot: %v", err)
		}
	})
	t.Run("symlink inside the folder", func(t *testing.T) {
		f := folder(t, base, files())
		os.MkdirAll(filepath.Join(f.Folder, "lib"), 0o755)
		os.WriteFile(filepath.Join(f.Folder, "lib", "real"), []byte("#!/bin/sh\n"), 0o755)
		if err := os.Symlink("../lib/real", filepath.Join(f.Folder, "scripts", "linked")); err != nil {
			t.Fatal(err)
		}
		if s := snapshot(t, f); len(s) != 2 {
			t.Errorf("scripts %+v", s)
		}
	})
	t.Run("group-writable parent", func(t *testing.T) {
		f := folder(t, base, files())
		os.Chmod(filepath.Join(f.Folder, "scripts"), 0o775)
		listed, _ := f.LocalScripts()
		_, err := Snapshot(f, listed)
		if err == nil || !strings.Contains(err.Error(), "its directory") || !strings.Contains(err.Error(), "writable by group or others") {
			t.Errorf("snapshot: %v", err)
		}
	})
	t.Run("world-writable folder", func(t *testing.T) {
		f := folder(t, base, files())
		os.Chmod(f.Folder, 0o757)
		listed, _ := f.LocalScripts()
		if _, err := Snapshot(f, listed); err == nil || !strings.Contains(err.Error(), "writable by group or others") {
			t.Errorf("snapshot: %v", err)
		}
	})
	t.Run("group-writable file", func(t *testing.T) {
		f := folder(t, base, files())
		os.Chmod(filepath.Join(f.Folder, "scripts", "a"), 0o775)
		listed, _ := f.LocalScripts()
		_, err := Snapshot(f, listed)
		if err == nil || !strings.Contains(err.Error(), "script a: it is writable") {
			t.Errorf("snapshot: %v", err)
		}
		// silo validate reports the same, as an error.
		errs, _ := Check(f, listed)
		if len(errs) != 1 {
			t.Errorf("check: %v", errs)
		}
	})
	t.Run("foreign owner", func(t *testing.T) {
		f := folder(t, base, files())
		defer func(old func() int) { getuid = old }(getuid)
		getuid = func() int { return os.Getuid() + 1 }
		listed, _ := f.LocalScripts()
		if _, err := Snapshot(f, listed); err == nil || !strings.Contains(err.Error(), "not the invoking user") {
			t.Errorf("snapshot: %v", err)
		}
	})
}

// call runs a tool through its handler as the gateway would.
func call(t *testing.T, ex *Executor, tool string, args map[string]any) (Result, string, error) {
	t.Helper()
	for _, tl := range ex.Tools() {
		if tl.Name != tool {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), tl.Timeout)
		defer cancel()
		res, err := tl.Handler(ctx, args)
		var r Result
		if res.Text != "" {
			if jerr := json.Unmarshal([]byte(res.Text), &r); jerr != nil {
				t.Fatalf("result %q: %v", res.Text, jerr)
			}
		}
		return r, res.Text, err
	}
	t.Fatalf("no tool %s", tool)
	return Result{}, "", nil
}

func executor(t *testing.T, f *silofile.Silofile, cfg Config) *Executor {
	t.Helper()
	cfg.Scripts = snapshot(t, f)
	if cfg.Workdir == "" {
		cfg.Workdir = Workdir(f)
	}
	ex := New(cfg)
	t.Cleanup(func() { ex.Close() })
	return ex
}

func TestChangedScriptIsRefused(t *testing.T) {
	f := folder(t, header+"local: {}\n", map[string]string{"scripts/a": "#!/bin/sh\necho one\n"})
	ex := executor(t, f, Config{})
	if r, _, err := call(t, ex, "local_a", map[string]any{"args": []any{}}); err != nil || r.Stdout != "one\n" {
		t.Fatalf("first call: %+v %v", r, err)
	}
	os.WriteFile(filepath.Join(f.Folder, "scripts", "a"), []byte("#!/bin/sh\necho two\n"), 0o755)
	_, _, err := call(t, ex, "local_a", map[string]any{"args": []any{}})
	var ref *gateway.Refusal
	if !errors.As(err, &ref) || !strings.Contains(err.Error(), "changed since this session started (content)") {
		t.Fatalf("changed script: %v", err)
	}
}

func TestArgumentsPassExactly(t *testing.T) {
	f := folder(t, header+"local: {}\n", map[string]string{
		"scripts/args": "#!/bin/sh\nfor a in \"$@\"; do printf '[%s]\\n' \"$a\"; done\ncat\n",
	})
	ex := executor(t, f, Config{})
	args := []string{"a b", `"double"`, `'single'`, "$(touch pwned)", "`touch pwned2`", "*", "", "x;touch pwned3", "$HOME", "line\nbreak", "\\"}
	in := make([]any, len(args))
	want := ""
	for i, a := range args {
		in[i] = a
		want += "[" + a + "]\n"
	}
	r, _, err := call(t, ex, "local_args", map[string]any{"args": in, "stdin": "from stdin $(x)"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Stdout != want+"from stdin $(x)" || r.ExitCode != 0 {
		t.Fatalf("stdout:\n%s\nwant:\n%s", r.Stdout, want)
	}
	for _, p := range []string{"pwned", "pwned2", "pwned3"} {
		if _, err := os.Stat(filepath.Join(f.Folder, p)); err == nil {
			t.Errorf("an argument was interpreted: %s exists", p)
		}
	}

	_, _, err = call(t, ex, "local_args", map[string]any{"args": []any{"ok", "nul\x00byte"}})
	var ref *gateway.Refusal
	if !errors.As(err, &ref) || !strings.Contains(err.Error(), "NUL") {
		t.Errorf("NUL: %v", err)
	}
	for _, bad := range []map[string]any{{}, {"args": "a b"}, {"args": []any{1}}, {"args": []any{}, "cwd": "/"}, {"args": []any{}, "stdin": 3}} {
		if _, _, err := call(t, ex, "local_args", bad); !errors.As(err, &ref) {
			t.Errorf("%v: %v, want a refusal", bad, err)
		}
	}
}

func TestNonZeroExitIsAResult(t *testing.T) {
	f := folder(t, header+"local: {}\n", map[string]string{"scripts/fail": "#!/bin/sh\necho oops >&2\nexit 3\n"})
	ex := executor(t, f, Config{})
	r, text, err := call(t, ex, "local_fail", map[string]any{"args": []any{}})
	if err != nil || r.ExitCode != 3 || r.Stderr != "oops\n" || r.Truncated || r.Duration == "" {
		t.Fatalf("%s %v", text, err)
	}
	if !strings.HasPrefix(text, "{\n  \"exit_code\": 3,") {
		t.Errorf("exit_code is not first, where the audit head keeps it: %s", text)
	}
}

func TestWorkdirAndEnvironment(t *testing.T) {
	f := folder(t, header+`local:
  workdir: wd
credentials:
  - name: tok
    from: {env: TOKEN_SOURCE}
    to: [{local: a, env: DEPLOY_TOKEN}]
  - name: other
    from: {env: OTHER_SOURCE}
    to: [{local: a, env: OTHER}]
`, map[string]string{
		"scripts/a": "#!/bin/sh\npwd\nenv\n",
		"scripts/b": "#!/bin/sh\nenv\n",
		"wd/.keep":  "",
	})
	creds := map[string]credentials.Value{
		"tok":   {Name: "tok", Secret: "tok-secret-value"},
		"other": {Name: "other", Secret: "other-secret"},
	}
	environ := []string{
		"PATH=" + os.Getenv("PATH"), "PLAIN=kept",
		"SILO_SESSION=x", "SILO_ANYTHING=y",
		"TOKEN_SOURCE=tok-secret-value", "OTHER_SOURCE=other-secret",
		"COPY_OF_TOKEN=tok-secret-value",
	}
	delivered, err := Delivered(f.Credentials, creds)
	if err != nil {
		t.Fatal(err)
	}
	ex := executor(t, f, Config{Environ: BaseEnviron(environ, f.Credentials, creds), Env: delivered})
	ra, _, err := call(t, ex, "local_a", map[string]any{"args": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	rb, _, err := call(t, ex, "local_b", map[string]any{"args": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	if wd, _ := filepath.EvalSymlinks(filepath.Join(f.Folder, "wd")); !strings.HasPrefix(ra.Stdout, wd+"\n") {
		t.Errorf("workdir: %s, want %s", ra.Stdout, wd)
	}
	for _, r := range []Result{ra, rb} {
		for _, gone := range []string{"SILO_", "TOKEN_SOURCE", "OTHER_SOURCE", "COPY_OF_TOKEN"} {
			if strings.Contains(r.Stdout, gone) {
				t.Errorf("%s reached a script:\n%s", gone, r.Stdout)
			}
		}
		if !strings.Contains(r.Stdout, "PLAIN=kept") {
			t.Errorf("the invoking environment is missing:\n%s", r.Stdout)
		}
	}
	if !strings.Contains(ra.Stdout, "DEPLOY_TOKEN=tok-secret-value") || !strings.Contains(ra.Stdout, "OTHER=other-secret") {
		t.Errorf("a's delivered credentials are missing:\n%s", ra.Stdout)
	}
	if strings.Contains(rb.Stdout, "secret") {
		t.Errorf("b got a credential delivered only to a:\n%s", rb.Stdout)
	}

	if _, err := Delivered(f.Credentials, map[string]credentials.Value{"tok": {SilosOwn: true}, "other": {}}); err == nil {
		t.Error("silo's own credential was delivered")
	}
}

func alive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return syscall.Kill(pid, 0) == nil
	}
	// A zombie is dead; it is only waiting for init to reap it.
	fields := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
	return len(fields) > 0 && fields[0] != "Z"
}

func TestTimeoutKillsTheProcessGroup(t *testing.T) {
	f := folder(t, header+`local:
  timeout: 1s
`, map[string]string{
		// A grandchild that ignores SIGTERM, so only the SIGKILL after
		// the grace period ends it.
		"scripts/hang": "#!/bin/sh\nsh -c 'trap \"\" TERM; echo $$ > grandchild; while :; do sleep 1; done' &\necho started\nwhile :; do sleep 1; done\n",
	})
	ex := executor(t, f, Config{KillGrace: 500 * time.Millisecond})
	start := time.Now()
	r, _, err := call(t, ex, "local_hang", map[string]any{"args": []any{}})
	if !errors.Is(err, context.DeadlineExceeded) || !r.TimedOut || r.Stdout != "started\n" {
		t.Fatalf("result %+v, err %v", r, err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %s", d)
	}
	b, err := os.ReadFile(filepath.Join(f.Folder, "grandchild"))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("grandchild %d outlived the timeout", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestCloseEndsRunningScripts(t *testing.T) {
	f := folder(t, header+"local: {}\n", map[string]string{"scripts/hang": "#!/bin/sh\nwhile :; do sleep 1; done\n"})
	ex := executor(t, f, Config{KillGrace: 200 * time.Millisecond})
	errc := make(chan error, 1)
	go func() {
		_, _, err := call(t, ex, "local_hang", map[string]any{"args": []any{}})
		errc <- err
	}()
	time.Sleep(300 * time.Millisecond)
	ex.Close()
	select {
	case err := <-errc:
		if err == nil || !strings.Contains(err.Error(), "session ended") {
			t.Errorf("err %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close left the script running")
	}
}

func TestOutputCap(t *testing.T) {
	f := folder(t, header+`local:
  output_cap: 100
`, map[string]string{"scripts/loud": "#!/bin/sh\nhead -c 10000 /dev/zero | tr '\\0' x\necho small >&2\n"})
	ex := executor(t, f, Config{OutputCap: OutputCap(f)})
	r, _, err := call(t, ex, "local_loud", map[string]any{"args": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Truncated || r.StdoutBytes != 10000 || r.StderrBytes != 6 || r.Stderr != "small\n" {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(r.Stdout, "9900 bytes of output dropped") || strings.Count(r.Stdout, "x") != 100 {
		t.Errorf("stdout %q", r.Stdout)
	}
}

func TestWarnings(t *testing.T) {
	cases := map[string]string{
		"eval":      "#!/bin/sh\neval \"$@\"\n",
		"shc":       "#!/bin/sh\nset -e\nbash -c \"$1\"\n",
		"shc2":      "#!/bin/sh\nexec /bin/sh -ec \"${2}\"\n",
		"unquoted":  "#!/bin/sh\nls $@\n",
		"unquoted2": "#!/bin/sh\necho ${*}\n",
		"xargs":     "#!/bin/sh\nprintf '%s\\n' \"$@\" | xargs rm\n",
		"wrapper":   "#!/bin/sh\nexec python3 \"$@\"\n",
	}
	clean := map[string]string{
		"quoted":   "#!/bin/sh\n# eval is mentioned only in a comment; so is xargs\nls -- \"$@\"\necho 'eval $@'\ngit log \"$1\"\n",
		"evaluate": "#!/bin/sh\nevaluate \"$1\"\nsh -c 'echo fixed'\n",
	}
	files := map[string]string{}
	for k, v := range cases {
		files["scripts/"+k] = v
	}
	for k, v := range clean {
		files["scripts/"+k] = v
	}
	f := folder(t, header+"local: {}\n", files)
	// A copy of a shell, which is itself an interpreter.
	sh, err := os.ReadFile("/bin/sh")
	if err != nil {
		t.Skip(err)
	}
	os.WriteFile(filepath.Join(f.Folder, "scripts", "dash"), sh, 0o755)
	cases["dash"] = ""

	for name := range cases {
		w := Warnings(filepath.Join(f.Folder, "scripts", name))
		if len(w) == 0 {
			t.Errorf("%s: no warning", name)
		}
		t.Logf("%s: %v", name, w)
	}
	for name := range clean {
		if w := Warnings(filepath.Join(f.Folder, "scripts", name)); len(w) != 0 {
			t.Errorf("%s: %v", name, w)
		}
	}
	listed, errs := f.LocalScripts()
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	res := f.Validate(silofile.Options{CheckScripts: Check})
	if len(res.Errors) != 0 || len(res.Warnings) < len(cases) {
		t.Errorf("validate: errors %v, warnings %v (%d scripts)", res.Errors, res.Warnings, len(listed))
	}
}
