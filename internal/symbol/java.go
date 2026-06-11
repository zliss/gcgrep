package symbol

import (
	"regexp"
)

var javaTypeRe = regexp.MustCompile(`\b(class|interface|enum|record)\s+([A-Za-z_]\w*)`)

// javaKeywordsBeforeParen are tokens that, when directly preceding
// `name(`, mean it is control flow or an expression — never a definition.
var notDefPreceders = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "catch": true,
	"return": true, "new": true, "else": true, "do": true, "throw": true,
	"case": true, "assert": true, "yield": true, "await": true, "typeof": true,
	"in": true, "of": true, "delete": true, "synchronized": true, "try": true,
	"finally": true, "record": true,
}

type typeRange struct {
	name  string
	kind  Kind
	open  int // offset of '{'
	close int // offset past matching '}'
}

// extractJava finds type declarations via regex and methods via the
// "identifier( ... ) followed by { or ; -inside-interface" heuristic on
// comment/string-stripped source — ctags-class fidelity.
func extractJava(src []byte) []Def {
	s := stripCLike(src)
	li := newLineIndex(s)
	var defs []Def
	var types []typeRange

	for _, m := range javaTypeRe.FindAllSubmatchIndex(s, -1) {
		kind := Kind(s[m[2]:m[3]])
		name := string(s[m[4]:m[5]])
		open, close := braceRange(s, m[1])
		types = append(types, typeRange{name: name, kind: kind, open: open, close: close})
		defs = append(defs, Def{Name: name, Kind: kind, Container: containerAt(types[:len(types)-1], m[0]), Line: li.line(m[0])})
	}

	for _, c := range methodCandidates(s) {
		tr := innermost(types, c.namePos)
		if tr == nil {
			continue // Java methods only exist inside a type body
		}
		if !c.hasBody && tr.kind != KindInterface {
			continue // body-less signature outside an interface: not a def
		}
		defs = append(defs, Def{Name: c.name, Kind: KindMethod, Container: tr.name, Line: li.line(c.namePos)})
	}
	return defs
}

// braceRange locates the '{' opening a declaration starting after pos and
// its matching '}' (offset past it). Returns (-1,-1) if none found.
func braceRange(s []byte, pos int) (int, int) {
	open := -1
	for i := pos; i < len(s); i++ {
		if s[i] == '{' {
			open = i
			break
		}
		if s[i] == ';' {
			return -1, -1
		}
	}
	if open < 0 {
		return -1, -1
	}
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return open, i + 1
			}
		}
	}
	return open, len(s)
}

func innermost(types []typeRange, pos int) *typeRange {
	var best *typeRange
	for i := range types {
		t := &types[i]
		if t.open >= 0 && t.open < pos && pos < t.close {
			if best == nil || t.open > best.open {
				best = t
			}
		}
	}
	return best
}

func containerAt(types []typeRange, pos int) string {
	if t := innermost(types, pos); t != nil {
		return t.name
	}
	return ""
}

type methodCandidate struct {
	name    string
	namePos int
	hasBody bool
}

// methodCandidates finds `identifier ( ... )` sequences that look like
// definitions: not preceded by '.', not preceded by a control keyword,
// preceded by another word/'>'/']' (the return type) or an annotation,
// and followed (after optional `throws X`) by '{' or ';'.
func methodCandidates(s []byte) []methodCandidate {
	var out []methodCandidate
	for i := 0; i < len(s); i++ {
		if !isIdentByte(s[i]) || i > 0 && isIdentByte(s[i-1]) {
			continue
		}
		start := i
		for i < len(s) && isIdentByte(s[i]) {
			i++
		}
		name := string(s[start:i])
		j := skipSpace(s, i)
		if j >= len(s) || s[j] != '(' {
			continue
		}
		if notDefPreceders[name] {
			continue
		}
		// preceding context
		p := start - 1
		for p >= 0 && (s[p] == ' ' || s[p] == '\t' || s[p] == '\n' || s[p] == '\r') {
			p--
		}
		if p < 0 {
			continue
		}
		switch s[p] {
		case '.', '(', ',', '=', '+', '-', '!', '&', '|', ':', '?', '{', ';', '<', '*', '/':
			continue // expression or statement context: a call, not a definition
		}
		var prevWord string
		if isIdentByte(s[p]) {
			e := p
			for p >= 0 && isIdentByte(s[p]) {
				p--
			}
			prevWord = string(s[p+1 : e+1])
		} else if s[p] != '>' && s[p] != ']' && s[p] != '}' && s[p] != ';' && s[p] != '@' {
			continue
		}
		if notDefPreceders[prevWord] {
			continue
		}
		// skip balanced parameter list
		depth := 0
		k := j
		for ; k < len(s); k++ {
			if s[k] == '(' {
				depth++
			} else if s[k] == ')' {
				depth--
				if depth == 0 {
					k++
					break
				}
			}
		}
		k = skipSpace(s, k)
		// optional `throws A, B`
		if k+6 <= len(s) && string(s[k:k+6]) == "throws" {
			for k < len(s) && s[k] != '{' && s[k] != ';' {
				k++
			}
		}
		if k < len(s) && s[k] == '{' {
			out = append(out, methodCandidate{name: name, namePos: start, hasBody: true})
		} else if k < len(s) && s[k] == ';' && prevWord != "" {
			out = append(out, methodCandidate{name: name, namePos: start, hasBody: false})
		}
	}
	return out
}

func skipSpace(s []byte, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	return i
}
