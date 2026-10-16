package logsafe

import (
	"strings"
	"testing"
)

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{
		"":                               `""`,
		"t-1":                            `"t-1"`,
		"a\nb":                           `"a\nb"`,
		"x\r\n2026 forged":               `"x\r\n2026 forged"`,
		"\x1b[31mred":                    `"\x1b[31mred"`,
		strings.Repeat("a", Max):         `"` + strings.Repeat("a", Max) + `"`,
		strings.Repeat("a", Max+1):       `"` + strings.Repeat("a", Max) + `"…(201 bytes)`,
		strings.Repeat("a", Max-1) + "é": `"` + strings.Repeat("a", Max-1) + `"…(201 bytes)`,
	} {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%.40q) = %.60s, want %.60s", in, got, want)
		}
	}
	if got := Quote(strings.Repeat("\n", 1<<20)); len(got) > 4*Max+40 || strings.Contains(got, "\n") {
		t.Errorf("Quote of 1 MiB of newlines is %d bytes", len(got))
	}
}
