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
	// rareByte is the least common byte in PlainLit, used as a
	// pre-filter: if this byte is absent from a line, the line cannot
	// match. bytes.IndexByte is SIMD-accelerated on amd64.
	rareByte byte
	hasRare  bool
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
	if m.PlainLit != "" {
		m.rareByte, m.hasRare = pickRareByte(m.PlainLit)
	}
	return m, nil
}

// byteFreqRank approximates byte frequency in source code. Lower = rarer.
var byteFreqRank [256]byte

func init() {
	// common bytes in source code get high rank (= frequent)
	for i := range byteFreqRank {
		byteFreqRank[i] = 50 // default: moderately rare
	}
	// very common: space, newline, letters, digits
	for _, b := range []byte(" \t\n\r") {
		byteFreqRank[b] = 255
	}
	for b := byte('a'); b <= 'z'; b++ {
		byteFreqRank[b] = 200
	}
	for b := byte('A'); b <= 'Z'; b++ {
		byteFreqRank[b] = 180
	}
	for b := byte('0'); b <= '9'; b++ {
		byteFreqRank[b] = 160
	}
	// common punctuation in code
	for _, b := range []byte("(){}[];,._=-+*/<>&|!:\"'\\#") {
		byteFreqRank[b] = 140
	}
}

func pickRareByte(lit string) (byte, bool) {
	if len(lit) == 0 {
		return 0, false
	}
	best := lit[0]
	for i := 1; i < len(lit); i++ {
		if byteFreqRank[lit[i]] < byteFreqRank[best] {
			best = lit[i]
		}
	}
	return best, true
}

// FindFirst returns the [start,end) of the first match in line, or nil.
// Used for line-at-a-time scanning of stream-set files on the client.
func (m Matcher) FindFirst(line []byte) []int {
	if m.PlainLit != "" {
		// rare-byte pre-filter: skip lines missing the rarest byte
		if m.hasRare && bytes.IndexByte(line, m.rareByte) < 0 {
			if !m.Fold {
				return nil
			}
			// case-insensitive: also check the opposite case
			alt := m.rareByte ^ 0x20
			if alt >= 'a' && alt <= 'z' || alt >= 'A' && alt <= 'Z' {
				if bytes.IndexByte(line, alt) < 0 {
					return nil
				}
			}
		}
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
