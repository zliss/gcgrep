package symbol

import (
	"bytes"
	"regexp"
)

var pyDefRe = regexp.MustCompile(`^([ \t]*)(?:async[ \t]+)?(def|class)[ \t]+([A-Za-z_]\w*)`)

// extractPython scans comment/string-stripped lines for def/class
// declarations, deriving containers from indentation.
func extractPython(src []byte) []Def {
	s := stripPython(src)
	var defs []Def
	type frame struct {
		name   string
		indent int
		class  bool
	}
	var stack []frame
	for lineNo, line := range bytes.Split(s, []byte("\n")) {
		m := pyDefRe.FindSubmatch(line)
		if m == nil {
			continue
		}
		indent := len(m[1])
		kw, name := string(m[2]), string(m[3])
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		d := Def{Name: name, Line: lineNo + 1}
		if kw == "class" {
			d.Kind = KindClass
		} else if len(stack) > 0 && stack[len(stack)-1].class {
			d.Kind = KindMethod
		} else {
			d.Kind = KindFunc
		}
		if len(stack) > 0 {
			d.Container = stack[len(stack)-1].name
		}
		defs = append(defs, d)
		stack = append(stack, frame{name: name, indent: indent, class: kw == "class"})
	}
	return defs
}
