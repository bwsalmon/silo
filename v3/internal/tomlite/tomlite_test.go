package tomlite

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	doc := `
# a comment
name = "claude"   # trailing comment
command = [
  "/opt/silo/cli/claude",
  '--print',  # literal string
  "{brief}",
]
n = 1_000
neg = -3
ratio = 0.5
on = true
dotted.key = "x"
inline = { a = 1, b = "two" }
"quoted key" = "é\t\"q\""

[env]
FOO = "bar"

[root_agent.limits]
memory_max = "4G"
`
	got, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"name":       "claude",
		"command":    []any{"/opt/silo/cli/claude", "--print", "{brief}"},
		"n":          int64(1000),
		"neg":        int64(-3),
		"ratio":      0.5,
		"on":         true,
		"dotted":     map[string]any{"key": "x"},
		"inline":     map[string]any{"a": int64(1), "b": "two"},
		"quoted key": "é\t\"q\"",
		"env":        map[string]any{"FOO": "bar"},
		"root_agent": map[string]any{"limits": map[string]any{"memory_max": "4G"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %#v\nwant %#v", got, want)
	}
}

func TestParseErrors(t *testing.T) {
	for _, doc := range []string{
		`a = `,
		`a = "unterminated`,
		`a = 1 b = 2`,
		"a = 1\na = 2",
		"[t]\n[t]",
		"[[arr]]",
		`a = """x"""`,
		`a = 1979-05-27`,
		`a = [1, 2`,
		`= 1`,
		"a = 1\n[a]",
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", doc)
		}
	}
}

func TestUnmarshal(t *testing.T) {
	var v struct {
		Name  string            `json:"name"`
		UIDs  []int             `json:"uids"`
		Env   map[string]string `json:"env"`
		Inner struct {
			On bool `json:"on"`
		} `json:"inner"`
	}
	err := Unmarshal([]byte("name = \"x\"\nuids = [1, 2]\n[env]\nA = \"b\"\n[inner]\non = true\n"), &v)
	if err != nil {
		t.Fatal(err)
	}
	if v.Name != "x" || !reflect.DeepEqual(v.UIDs, []int{1, 2}) || v.Env["A"] != "b" || !v.Inner.On {
		t.Fatalf("got %+v", v)
	}
	err = Unmarshal([]byte(`nmae = "typo"`), &v)
	if err == nil || !strings.Contains(err.Error(), "nmae") {
		t.Fatalf("unknown key: got %v", err)
	}
}
