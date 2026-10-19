package security_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/hub/grainfiles"
)

// renderBound is how long a hostile template may take to be refused. The
// Hub renders .grain.md.jinja2 templates on GET /v1/grainfiles/content and
// mux fetch_grainfile with caller-supplied vars, so a template that runs
// away is a remote CPU/memory denial of service.
const renderBound = 5 * time.Second

func findingCodes(err error) []string {
	var errs grainfiles.Errors
	if !errors.As(err, &errs) {
		return nil
	}
	var codes []string
	for _, f := range errs {
		codes = append(codes, f.Code)
	}
	return codes
}

func wantFinding(t *testing.T, err error, code string) {
	t.Helper()
	for _, c := range findingCodes(err) {
		if c == code {
			return
		}
	}
	t.Fatalf("err = %v, want finding %s", err, code)
}

// TestTemplateRenderBombsAreBoundedWhileRendering: MaxFileBytes is
// enforced during evaluation, not only on the finished output, and a
// template whose output stays empty still cannot loop without bound.
func TestTemplateRenderBombsAreBoundedWhileRendering(t *testing.T) {
	const front = "---\ngrainfile: v1\ntitle: bomb\n---\n"
	many := strings.TrimSuffix(strings.Repeat("a,", 5000), ",")
	cases := []struct {
		name string
		tmpl string
		vars map[string]string
	}{
		{
			// Each replace multiplies the length by ten: 10^10 bytes if
			// allowed to run.
			name: "chained replace",
			tmpl: front + `{{ "aaaaaaaaaa"` + strings.Repeat(`|replace("a","aaaaaaaaaa")`, 9) + ` }}`,
		},
		{
			name: "replace with empty needle",
			tmpl: front + `{{ big|replace("","` + strings.Repeat("x", 1000) + `") }}`,
			vars: map[string]string{"big": strings.Repeat("y", 100000)},
		},
		{
			// 5000^3 iterations writing one byte each.
			name: "nested loop output",
			tmpl: front + `{% for a in v %}{% for b in v %}{% for c in v %}x{% endfor %}{% endfor %}{% endfor %}`,
			vars: map[string]string{"v": many},
		},
		{
			// 5000^3 iterations writing nothing: only a step budget stops it.
			name: "nested loop no output",
			tmpl: front + `{% for a in v %}{% for b in v %}{% for c in v %}{% endfor %}{% endfor %}{% endfor %}`,
			vars: map[string]string{"v": many},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				_, _, err := grainfiles.RenderTemplate(tc.tmpl, tc.vars)
				done <- err
			}()
			select {
			case err := <-done:
				wantFinding(t, err, grainfiles.CodeLimitExceeded)
			case <-time.After(renderBound):
				t.Fatalf("render still running after %v", renderBound)
			}
		})
	}
}

// TestTemplateFromTemplateVarCannotInjectFrontMatter: the
// `from_template` var a caller passes as ?var.from_template= (or mux
// fetch_grainfile vars) is written into the rendered front matter, so it
// may not carry a newline that adds keys of the caller's choosing.
func TestTemplateFromTemplateVarCannotInjectFrontMatter(t *testing.T) {
	tmpl := "---\ngrainfile: v1\ntitle: \"{{ title }}\"\n---\nbody\n"
	for _, ft := range []string{
		"https://github.com/acme/app/blob/main/x.task.grain.md.jinja2\nreferences:\n  - https://github.com/acme/secret",
		"/x.task.grain.md.jinja2\rwhen: {push: {}}",
		"/x.task.grain.md.jinja2\u2028draft: false",
		"!!python/object x.task.grain.md.jinja2",
	} {
		_, _, err := grainfiles.RenderTemplate(tmpl, map[string]string{"title": "t", "from_template": ft})
		wantFinding(t, err, grainfiles.CodeMalformedRef)
		rendered := "---\ngrainfile: v1\ntitle: t\n---\nbody\n"
		if got := grainfiles.EnsureFromTemplate(rendered, ft); got != rendered {
			t.Errorf("EnsureFromTemplate(%q) injected:\n%s", ft, got)
		}
	}
	// A well-formed URL is still injected.
	ok := "https://github.com/acme/app/blob/main/.grain/tasks/x.task.grain.md.jinja2"
	out, _, err := grainfiles.RenderTemplate(tmpl, map[string]string{"title": "t", "from_template": ok})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "from_template: "+ok+"\n") {
		t.Fatalf("rendered output lacks from_template:\n%s", out)
	}
}
