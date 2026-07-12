package router

import (
	"fmt"
	"strings"
)

// UnifiedDiff renders a unified diff (3 lines of context) between two texts.
// Returns "" when the texts are equal. Hand-rolled because the stdlib has no
// exported diff and the repo's dependency policy is stdlib-first; the inputs
// here are rendered configs of at most a few hundred lines, so the quadratic
// LCS table is irrelevant.
func UnifiedDiff(aName, bName, aText, bText string) string {
	if aText == bText {
		return ""
	}
	a := splitLines(aText)
	b := splitLines(bText)
	ops := diffOps(a, b)

	// Line number (1-based) each text is at when emission reaches ops[k].
	aAt := make([]int, len(ops)+1)
	bAt := make([]int, len(ops)+1)
	aLine, bLine := 1, 1
	for k, op := range ops {
		aAt[k], bAt[k] = aLine, bLine
		switch op.kind {
		case ' ':
			aLine++
			bLine++
		case '-':
			aLine++
		case '+':
			bLine++
		}
	}
	aAt[len(ops)], bAt[len(ops)] = aLine, bLine

	const ctx = 3
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", aName, bName)
	pos := 0
	for {
		i := pos
		for i < len(ops) && ops[i].kind == ' ' {
			i++
		}
		if i == len(ops) {
			break
		}
		start := max(i-ctx, pos)
		// Extend to the last change reachable without crossing a gap of
		// more than 2*ctx unchanged lines (two hunks' context would touch).
		lastChange := i
		j := i + 1
		for j < len(ops) {
			if ops[j].kind != ' ' {
				lastChange = j
				j++
				continue
			}
			k := j
			for k < len(ops) && ops[k].kind == ' ' {
				k++
			}
			if k == len(ops) || k-j > 2*ctx {
				break
			}
			j = k
		}
		end := min(lastChange+ctx+1, len(ops))

		aStart, bStart := aAt[start], bAt[start]
		aCount, bCount := aAt[end]-aAt[start], bAt[end]-bAt[start]
		// Unified format quirk: a zero-length range is anchored one line
		// earlier ("@@ -0,0 +1,3 @@" for an insertion into an empty file).
		if aCount == 0 {
			aStart--
		}
		if bCount == 0 {
			bStart--
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", aStart, aCount, bStart, bCount)
		for k := start; k < end; k++ {
			out.WriteByte(ops[k].kind)
			out.WriteString(ops[k].text)
			out.WriteByte('\n')
		}
		pos = end
	}
	return out.String()
}

type diffOp struct {
	kind byte // ' ' unchanged, '-' only in a, '+' only in b
	text string
}

// diffOps computes an LCS-based edit script from a to b.
func diffOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
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
	ops := make([]diffOp, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}
