// Package deploy holds silo's install and uninstall scripts. These tests
// keep them parseable and the pinned versions exact in CI; the scripts
// themselves are exercised by the end-to-end test (v3/e2e,
// SILO_E2E_INSTALL=1).
package deploy

import (
	"bufio"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

var scripts = []string{"install.sh", "uninstall.sh"}

func TestScriptsParse(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	for _, s := range scripts {
		if out, err := exec.Command(bash, "-n", s).CombinedOutput(); err != nil {
			t.Errorf("bash -n %s: %v\n%s", s, err, out)
		}
		fi, err := os.Stat(s)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode()&0o111 == 0 {
			t.Errorf("%s is not executable", s)
		}
	}
	if sc, err := exec.LookPath("shellcheck"); err == nil {
		args := append([]string{"-x", "-S", "warning"}, scripts...)
		if out, err := exec.Command(sc, args...).CombinedOutput(); err != nil {
			t.Errorf("shellcheck: %v\n%s", err, out)
		}
	}
}

var (
	lineRe = regexp.MustCompile(`^([A-Z][A-Z0-9_]*)=(\S+)$`)
	// An npm package pinned to an exact version: no ranges, tags or "latest".
	pkgRe = regexp.MustCompile(`^(@[a-z0-9-]+/)?[a-z0-9.-]+@[0-9]+\.[0-9]+\.[0-9]+$`)
)

func TestVersionsPinned(t *testing.T) {
	f, err := os.Open("versions.env")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	install, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := lineRe.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("versions.env: %q is not a plain KEY=value line", line)
			continue
		}
		key, val := m[1], m[2]
		seen[key] = true
		if strings.HasSuffix(key, "_PKG") && !pkgRe.MatchString(val) {
			t.Errorf("versions.env: %s=%s is not an exact package@x.y.z", key, val)
		}
		if !strings.Contains(string(install), "$"+key) {
			t.Errorf("versions.env: install.sh never uses %s", key)
		}
	}
	for _, k := range []string{"SILO_VERSION", "NODE_MAJOR", "CLAUDE_CODE_PKG", "CLAUDE_ACP_PKG", "CODEX_PKG", "CODEX_ACP_PKG", "GEMINI_CLI_PKG"} {
		if !seen[k] {
			t.Errorf("versions.env: no %s", k)
		}
	}
}
