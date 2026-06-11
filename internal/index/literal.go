package index

import (
	"bytes"
	"regexp/syntax"
)

// PlainLiteral reports whether the pattern is a pure literal (no regex
// operators), returning it. Pure literals take a bytes.Index fast path:
// Go's regexp is markedly slower than direct search, especially with (?i).
func PlainLiteral(pattern string, fixed bool) (string, bool) {
	if fixed {
		return pattern, true
	}
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return "", false
	}
	re = re.Simplify()
	if re.Op == syntax.OpLiteral && re.Flags&syntax.FoldCase == 0 {
		return string(re.Rune), true
	}
	return "", false
}

// asciiLower lowercases ASCII letters into a fresh buffer, preserving
// length (multi-byte runes are untouched, so offsets stay valid).
func asciiLower(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		out[i] = c
	}
	return out
}

// HasNonASCII reports whether s contains bytes outside the ASCII range.
func HasNonASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return true
		}
	}
	return false
}

// literalFindAll returns match index pairs of lit in content, optionally
// ASCII-case-folded, mirroring regexp.FindAllIndex's shape.
func literalFindAll(content, lit []byte, fold bool) [][]int {
	if fold {
		content = asciiLower(content)
		lit = asciiLower(lit)
	}
	var locs [][]int
	for off := 0; ; {
		i := bytes.Index(content[off:], lit)
		if i < 0 {
			return locs
		}
		start := off + i
		locs = append(locs, []int{start, start + len(lit)})
		off = start + len(lit)
	}
}
