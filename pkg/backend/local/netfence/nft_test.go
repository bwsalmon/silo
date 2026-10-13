package netfence

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// TestExec runs Exec against a stand-in for nft that keeps what it is
// loaded with in a file, and lists it back.
func TestExec(t *testing.T) {
	dir := t.TempDir()
	script := `case "$1" in
-f) cat > "$D/rules" ;;
list) if [ "$2" = tables ]; then if [ -f "$D/rules" ]; then printf 'table inet silo\ntable bridge silo\ntable ip other\n'; fi; else echo "listed $3 $4"; cat "$D/rules"; fi ;;
*) echo "bad args $*" >&2; exit 1 ;;
esac`
	var calls [][]string
	e := Exec{Command: func(ctx context.Context, args ...string) (*exec.Cmd, error) {
		calls = append(calls, args)
		cmd := exec.CommandContext(ctx, "sh", append([]string{"-c", script, "nft"}, args...)...)
		cmd.Env = []string{"D=" + dir}
		return cmd, nil
	}}
	ctx := context.Background()
	if got, err := e.List(ctx); err != nil || got != "" {
		t.Fatalf("List with no tables: %q, %v", got, err)
	}
	in, topo := example()
	rs, err := Compile(in, topo, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Load(ctx, rs); err != nil {
		t.Fatal(err)
	}
	got, err := e.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "listed inet silo") || !strings.Contains(got, "listed bridge silo") || strings.Contains(got, "other") {
		t.Errorf("List = %q", got)
	}
	if !strings.Contains(got, "delete table inet silo") {
		t.Errorf("the stand-in was not loaded with the rendered ruleset: %q", got)
	}

	gone := errors.New("no holder")
	n := Nsenter(func() (int, error) { return 0, gone }, "")
	if err := n.Load(ctx, rs); !errors.Is(err, gone) {
		t.Errorf("Load with no holder: %v", err)
	}
	n = Nsenter(func() (int, error) { return 42, nil }, "/usr/sbin/nft")
	cmd, _ := n.Command(ctx, "list", "tables")
	if want := "nsenter -t 42 -U -n -- /usr/sbin/nft list tables"; strings.Join(cmd.Args, " ") != want {
		t.Errorf("nsenter command %q, want %q", cmd.Args, want)
	}
}
