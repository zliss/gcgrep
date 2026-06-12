package symbol

import (
	"regexp"
)

var (
	tsFuncRe  = regexp.MustCompile(`\bfunction[ \t]*\*?[ \t]+([A-Za-z_$]\w*)`)
	tsTypeRe  = regexp.MustCompile(`\b(class|interface|enum)[ \t]+([A-Za-z_$]\w*)`)
	tsAliasRe = regexp.MustCompile(`\btype[ \t]+([A-Za-z_$]\w*)[ \t]*[=<]`)
	// const f = (...) => / const f = async (...) => / const f = x =>
	tsArrowRe = regexp.MustCompile(`\b(?:const|let|var)[ \t]+([A-Za-z_$]\w*)(?:[ \t]*:[^=\n]*)?[ \t]*=[ \t]*(?:async[ \t]+)?(?:\([^)\n]*\)|[A-Za-z_$]\w*)[ \t]*=>`)
	// class methods: optional modifiers then name(...) {  — constructor included
	tsMethodRe = regexp.MustCompile(`(?m)^[ \t]*(?:public[ \t]+|private[ \t]+|protected[ \t]+|static[ \t]+|readonly[ \t]+|async[ \t]+|override[ \t]+|get[ \t]+|set[ \t]+|\*[ \t]*)*([A-Za-z_$]\w*)[ \t]*(?:<[^>\n]*>)?[ \t]*\([^;{}]*\)[ \t]*(?::[^;{}\n]*)?\{`)
)

var tsNotMethods = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "catch": true,
	"function": true, "return": true, "with": true, "do": true, "else": true,
	"new": true, "typeof": true, "await": true, "yield": true, "case": true,
}

// extractTS covers TypeScript/JavaScript: functions, classes, interfaces,
// enums, type aliases, arrow-function consts, and class methods. Method
// detection is scoped to class bodies to avoid counting function calls
// followed by object-literal blocks.
func extractTS(src []byte) []Def {
	s := stripCLike(src)
	li := newLineIndex(s)
	var defs []Def
	var classes []typeRange

	for _, m := range tsTypeRe.FindAllSubmatchIndex(s, -1) {
		kind := Kind(s[m[2]:m[3]])
		name := string(s[m[4]:m[5]])
		open, close := braceRange(s, m[1])
		classes = append(classes, typeRange{name: name, kind: kind, open: open, close: close})
		defs = append(defs, Def{Name: name, Kind: kind, Line: li.line(m[0])})
	}
	for _, m := range tsFuncRe.FindAllSubmatchIndex(s, -1) {
		defs = append(defs, Def{Name: string(s[m[2]:m[3]]), Kind: KindFunc, Line: li.line(m[0])})
	}
	for _, m := range tsAliasRe.FindAllSubmatchIndex(s, -1) {
		defs = append(defs, Def{Name: string(s[m[2]:m[3]]), Kind: KindType, Line: li.line(m[0])})
	}
	for _, m := range tsArrowRe.FindAllSubmatchIndex(s, -1) {
		defs = append(defs, Def{Name: string(s[m[2]:m[3]]), Kind: KindFunc, Line: li.line(m[0])})
	}
	for _, m := range tsMethodRe.FindAllSubmatchIndex(s, -1) {
		name := string(s[m[2]:m[3]])
		if tsNotMethods[name] {
			continue
		}
		tr := innermost(classes, m[2])
		if tr == nil || tr.kind != KindClass {
			continue
		}
		defs = append(defs, Def{Name: name, Kind: KindMethod, Container: tr.name, Line: li.line(m[2])})
	}
	return defs
}
