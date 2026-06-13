package index

import (
	"regexp/syntax"
	"strings"
)

// ExtractLiteral returns a substring that every match of pattern must
// contain, lowercased for trigram lookup. It returns "" when no usable
// (>=3 byte) required literal exists, in which case the caller scans all
// files. Correctness does not depend on this: it is purely a pruning hint.
func ExtractLiteral(pattern string, fixed bool) string {
	var lit string
	if fixed {
		lit = pattern
	} else {
		re, err := syntax.Parse(pattern, syntax.Perl)
		if err != nil {
			return ""
		}
		lit = requiredLiteral(re.Simplify())
	}
	if len(lit) < 3 {
		return ""
	}
	return strings.ToLower(lit)
}

// requiredLiteral walks the regex AST for the longest literal that must
// appear in every match. Alternations and optional parts contribute nothing.
func requiredLiteral(re *syntax.Regexp) string {
	switch re.Op {
	case syntax.OpLiteral:
		if re.Flags&syntax.FoldCase != 0 {
			// case-folded literal still matches lowercased trigrams
			return strings.ToLower(string(re.Rune))
		}
		return string(re.Rune)
	case syntax.OpCapture:
		return requiredLiteral(re.Sub[0])
	case syntax.OpPlus:
		// child occurs at least once
		return requiredLiteral(re.Sub[0])
	case syntax.OpConcat:
		best := ""
		var run strings.Builder
		flush := func() {
			if run.Len() > len(best) {
				best = run.String()
			}
			run.Reset()
		}
		for _, sub := range re.Sub {
			if sub.Op == syntax.OpLiteral && sub.Flags&syntax.FoldCase == 0 {
				run.WriteString(string(sub.Rune))
				continue
			}
			flush()
			if s := requiredLiteral(sub); len(s) > len(best) {
				best = s
			}
		}
		flush()
		return best
	default:
		return ""
	}
}
