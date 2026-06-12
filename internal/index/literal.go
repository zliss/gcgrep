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
	if len(lit) == 0 {
		return nil
	}
	var locs [][]int
	if !fold {
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
	for off := 0; ; {
		i := foldIndex(content[off:], lit)
		if i < 0 {
			return locs
		}
		start := off + i
		locs = append(locs, []int{start, start + len(lit)})
		off = start + len(lit)
	}
}

// foldIndex is an allocation-free ASCII-case-insensitive bytes.Index:
// SIMD-accelerated IndexByte jumps on both case variants of the first
// byte, then a folded compare. Copying the haystack to lowercase (the
// obvious approach) costs a full content-sized allocation per file per
// query, which dominated -i searches.
func foldIndex(s, lit []byte) int {
	c1 := lower(lit[0])
	c2 := c1
	if c1 >= 'a' && c1 <= 'z' {
		c2 = c1 - 32
	}
	for off := 0; off+len(lit) <= len(s); {
		i1 := bytes.IndexByte(s[off:], c1)
		if c2 != c1 {
			if i2 := bytes.IndexByte(s[off:], c2); i2 >= 0 && (i1 < 0 || i2 < i1) {
				i1 = i2
			}
		}
		if i1 < 0 {
			return -1
		}
		pos := off + i1
		if pos+len(lit) > len(s) {
			return -1
		}
		if foldEqual(s[pos:pos+len(lit)], lit) {
			return pos
		}
		off = pos + 1
	}
	return -1
}

func foldEqual(a, b []byte) bool {
	for i := range b {
		if lower(a[i]) != lower(b[i]) {
			return false
		}
	}
	return true
}
