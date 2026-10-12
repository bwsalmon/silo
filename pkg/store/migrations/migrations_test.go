package migrations

import (
	"strings"
	"testing"
	"testing/fstest"
)

// TestBackendsAgree pins the versioning rule: both backends carry the
// same version numbers, so a version means the same schema on either.
func TestBackendsAgree(t *testing.T) {
	versions := map[string][]int{}
	for _, b := range Backends {
		ms, err := Load(b)
		if err != nil {
			t.Fatal(err)
		}
		if len(ms) == 0 {
			t.Fatalf("no migrations for %s", b)
		}
		for _, m := range ms {
			if strings.TrimSpace(m.SQL) == "" {
				t.Errorf("%s/%s is empty", b, m.Name)
			}
			versions[b] = append(versions[b], m.Version)
		}
	}
	if a, b := versions["sqlite"], versions["postgres"]; len(a) != len(b) {
		t.Errorf("sqlite has versions %v, postgres %v: every version needs a file in both", a, b)
	}
}

func TestLoadRejectsBadNumbering(t *testing.T) {
	for name, fsys := range map[string]fstest.MapFS{
		"gap":     {"x/0001_a.sql": {Data: []byte("x")}, "x/0003_c.sql": {Data: []byte("x")}},
		"repeat":  {"x/0001_a.sql": {Data: []byte("x")}, "x/0001_b.sql": {Data: []byte("x")}},
		"zero":    {"x/0000_a.sql": {Data: []byte("x")}},
		"no-num":  {"x/init.sql": {Data: []byte("x")}},
		"no-zero": {"x/0002_a.sql": {Data: []byte("x")}},
	} {
		if _, err := load(fsys, "x"); err == nil {
			t.Errorf("%s: load accepted it", name)
		}
	}
	ms, err := load(fstest.MapFS{
		"x/0002_b.sql":  {Data: []byte("two")},
		"x/0001_a.sql":  {Data: []byte("one")},
		"x/README.md":   {Data: []byte("ignored")},
		"x/0010_j.sql~": {Data: []byte("ignored")},
	}, "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].SQL != "one" || ms[1].Version != 2 {
		t.Errorf("load = %+v", ms)
	}
}

// TestExpandOnly: a migration that isn't marked as a contract step adds
// and never takes away (state.md "Expand, then contract", SV-53).
func TestExpandOnly(t *testing.T) {
	for _, b := range Backends {
		ms, err := Load(b)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range ms {
			if s := ContractStatement(m.SQL); s != "" && m.Requires == 0 {
				t.Errorf("%s/%s holds %q but is not marked -- silo:contract requires=<version>", b, m.Name, s)
			}
			if m.Requires >= m.Version {
				t.Errorf("%s/%s: a contract step can't require its own schema or a later one", b, m.Name)
			}
		}
	}
	for sql, want := range map[string]string{
		"ALTER TABLE t DROP COLUMN c":                        "DROP",
		"ALTER TABLE t RENAME TO u":                          "RENAME",
		"ALTER TABLE t ALTER COLUMN c SET NOT NULL":          "SET NOT NULL",
		"ALTER TABLE t ALTER COLUMN c TYPE BIGINT":           "ALTER COLUMN c TYPE",
		"-- we never DROP anything\nCREATE TABLE t (a TEXT)": "",
		"ALTER TABLE t ADD COLUMN c TEXT":                    "",
	} {
		if got := ContractStatement(sql); got != want {
			t.Errorf("ContractStatement(%q) = %q, want %q", sql, got, want)
		}
	}
	ms, err := load(fstest.MapFS{
		"x/0001_a.sql": {Data: []byte("CREATE TABLE t (a TEXT)")},
		"x/0002_b.sql": {Data: []byte("-- silo:contract requires=2\nALTER TABLE t DROP COLUMN a")},
	}, "x")
	if err != nil {
		t.Fatal(err)
	}
	if ms[0].Requires != 0 || ms[1].Requires != 2 {
		t.Errorf("requires = %d, %d; want 0, 2", ms[0].Requires, ms[1].Requires)
	}
}
