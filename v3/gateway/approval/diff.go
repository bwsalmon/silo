package approval

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// maxDiffCells bounds the LCS table; larger inputs get a summary instead.
const maxDiffCells = 4 << 20

// isText reports whether data looks like text worth diffing.
func isText(data []byte) bool {
	return utf8.Valid(data) && !strings.ContainsRune(string(data), 0)
}

// UnifiedDiff returns a unified diff (3 lines of context) turning a into b,
// labelled with the two names. It returns "" when they are equal.
func UnifiedDiff(nameA, nameB string, a, b []byte) string {
	if string(a) == string(b) {
		return ""
	}
	if !isText(a) || !isText(b) {
		return fmt.Sprintf("Binary content differs: %d bytes (sha256 %s) -> %d bytes (sha256 %s)\n", len(a), sha(a)[:16], len(b), sha(b)[:16])
	}
	la, lb := splitLines(string(a)), splitLines(string(b))
	if (len(la)+1)*(len(lb)+1) > maxDiffCells {
		return fmt.Sprintf("Too large to diff: %d lines -> %d lines (sha256 %s)\n", len(la), len(lb), sha(b)[:16])
	}
	ops := diffOps(la, lb)
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", nameA, nameB)
	const ctx = 3
	// Group the edit script into hunks separated by more than 2*ctx
	// unchanged lines.
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := max(i-ctx, 0)
		end := i
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			run := end
			for run < len(ops) && ops[run].kind == ' ' {
				run++
			}
			if run == len(ops) || run-end > 2*ctx {
				end = min(end+ctx, len(ops))
				break
			}
			end = run
		}
		h := ops[start:end]
		aStart, bStart := h[0].ai, h[0].bi
		var aN, bN int
		for _, o := range h {
			if o.kind != '+' {
				aN++
			}
			if o.kind != '-' {
				bN++
			}
		}
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", hunkRange(aStart, aN), hunkRange(bStart, bN))
		for _, o := range h {
			out.WriteByte(o.kind)
			out.WriteString(o.text)
			if !strings.HasSuffix(o.text, "\n") {
				out.WriteString("\n\\ No newline at end of file\n")
			}
		}
		i = end
	}
	return out.String()
}

func hunkRange(start, n int) string {
	if n == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	if n == 1 {
		return fmt.Sprintf("%d", start+1)
	}
	return fmt.Sprintf("%d,%d", start+1, n)
}

type diffOp struct {
	kind   byte // ' ', '-', '+'
	text   string
	ai, bi int // line index in a and b where this op sits
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// diffOps is a longest-common-subsequence edit script.
func diffOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	// lcs[i][j] = LCS length of a[i:], b[j:].
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i], i, j})
			i++
			j++
		case i < n && (j == m || lcs[i+1][j] >= lcs[i][j+1]):
			ops = append(ops, diffOp{'-', a[i], i, j})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j], i, j})
			j++
		}
	}
	return ops
}
