//go:build !silo_conformance

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A default build of silod has neither test-only switch
// (testswitch_conformance.go): the hooks are nil, and the names aren't
// in the binary at all, while the conformance build's binary has them.
func TestReleaseBuildHasNoTestSwitches(t *testing.T) {
	if testSwitchesBuilt || keepControlPortSwitch() != nil || skipControlGateSwitch() != nil {
		t.Fatal("a default build has the test switches")
	}
	if testing.Short() {
		t.Skip("builds silod twice")
	}
	names := [][]byte{[]byte("SILO_TEST_" + "KEEP_CONTROL_PORT"), []byte("SILO_TEST_" + "SKIP_CONTROL_GATE")}
	for _, c := range []struct {
		tags string
		want bool
	}{{"", false}, {"silo_conformance", true}} {
		out := filepath.Join(t.TempDir(), "silod")
		args := []string{"build", "-trimpath", "-o", out}
		if c.tags != "" {
			args = append(args, "-tags", c.tags)
		}
		if b, err := exec.Command("go", append(args, ".")...).CombinedOutput(); err != nil {
			t.Fatalf("go build %v: %v\n%s", args, err, b)
		}
		bin, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			if got := bytes.Contains(bin, n); got != c.want {
				t.Errorf("tags %q: binary contains %s = %v, want %v", c.tags, n, got, c.want)
			}
		}
	}
}
