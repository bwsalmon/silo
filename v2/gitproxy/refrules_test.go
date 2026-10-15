package gitproxy

import "testing"

func TestParseRefRule(t *testing.T) {
	for in, want := range map[string]RefRule{
		"refs/heads/silo/*":             {Pattern: "refs/heads/silo/*"},
		"refs/heads/scratch (delete)":   {Pattern: "refs/heads/scratch", Delete: true},
		" refs/tags/v* (force, delete)": {Pattern: "refs/tags/v*", Force: true, Delete: true},
	} {
		got, err := ParseRefRule(in)
		if err != nil || got != want {
			t.Errorf("ParseRefRule(%q) = %+v, %v; want %+v", in, got, err, want)
		}
		if again, err := ParseRefRule(got.String()); err != nil || again != got {
			t.Errorf("ParseRefRule(%q) does not read back String's %q", in, got.String())
		}
	}
	for _, bad := range []string{"refs/heads/*", "main", "refs/heads/x delete", "refs/heads/x (merge)", "refs/heads/x (force"} {
		if _, err := ParseRefRule(bad); err == nil {
			t.Errorf("ParseRefRule(%q) succeeded", bad)
		}
	}
}
