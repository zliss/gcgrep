package index

import (
	"bytes"
	"regexp"
)

// Matcher bundles the compiled pattern with the literal fast-path
// decision. Daemon (index search) and client (stream-file scan) must
// match identically, so both build it through MatcherFor.
type Matcher struct {
	Re *regexp.Regexp
	// PlainLit, when non-empty, replaces the regex with a direct byte
	// search; Fold makes that search ASCII-case-insensitive.
	PlainLit string
	Fold     bool
}

// MatcherFor compiles pattern with grep-style fixed/nocase semantics and
// selects the literal fast path when safe (ASCII-only needle under -i).
func MatcherFor(pattern string, fixed, nocase bool) (Matcher, error) {
	pat := pattern
	if fixed {
		pat = regexp.QuoteMeta(pat)
	}
	if nocase {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return Matcher{}, err
	}
	m := Matcher{Re: re}
	if lit, ok := PlainLiteral(pattern, fixed); ok {
		if !nocase {
			m.PlainLit = lit
		} else if !HasNonASCII(lit) {
			m.PlainLit = lit
			m.Fold = true
		}
	}
	return m, nil
}

// FindFirst returns the [start,end) of the first match in line, or nil.
// Used for line-at-a-time scanning of stream-set files on the client.
func (m Matcher) FindFirst(line []byte) []int {
	if m.PlainLit != "" {
		lit := []byte(m.PlainLit)
		var i int
		if m.Fold {
			i = foldIndex(line, lit)
		} else {
			i = bytes.Index(line, lit)
		}
		if i < 0 {
			return nil
		}
		return []int{i, i + len(lit)}
	}
	return m.Re.FindIndex(line)
}
