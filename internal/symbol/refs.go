package symbol

import (
	"bytes"
)

// Ref is one candidate reference to a symbol name.
type Ref struct {
	Line   int
	IsCall bool // name is followed by '(' or preceded by `new`
}

// Refs returns candidate references to name in src: identifier-boundary
// occurrences outside comments and strings, excluding the symbol's own
// definition lines. These are syntax-level candidates — overloads and
// same-named members of unrelated types are NOT distinguished (that needs
// type resolution; IDE territory).
func Refs(path string, src []byte, name string) []Ref {
	lang := Language(path)
	s := Strip(lang, src)
	defLines := make(map[int]bool)
	for _, d := range Extract(path, src) {
		if d.Name == name {
			defLines[d.Line] = true
		}
	}
	li := newLineIndex(s)
	needle := []byte(name)
	var refs []Ref
	seenLine := -1
	for off := 0; ; {
		i := bytes.Index(s[off:], needle)
		if i < 0 {
			break
		}
		pos := off + i
		off = pos + len(needle)
		// identifier boundaries
		if pos > 0 && isIdentByte(s[pos-1]) || off < len(s) && isIdentByte(s[off]) {
			continue
		}
		line := li.line(pos)
		if defLines[line] || line == seenLine {
			continue
		}
		seenLine = line
		j := skipSpace(s, off)
		isCall := j < len(s) && s[j] == '('
		if !isCall {
			// `new Name`, `Name{...}` (Go composite literal), `&Name{`
			p := pos - 1
			for p >= 0 && (s[p] == ' ' || s[p] == '\t') {
				p--
			}
			if p >= 2 && string(s[p-2:p+1]) == "new" {
				isCall = true
			}
		}
		refs = append(refs, Ref{Line: line, IsCall: isCall})
	}
	return refs
}
