// Package symbol extracts code symbol definitions (classes, functions,
// methods) and call-site references, IDE-style but syntax-level.
//
// Approach: pure Go, no parser dependencies. Go uses the stdlib AST for
// exact results; Java/Python/TypeScript use comment/string stripping plus
// lexical heuristics — the same fidelity class as universal-ctags, which
// established that this is good enough for code navigation. References
// are CANDIDATES by name (like ctags/zoekt), not type-resolved: telling
// a.getUser() from b.getUser() apart needs a compiler, not an indexer.
package symbol

import (
	"path/filepath"
	"strings"
)

type Kind string

const (
	KindClass     Kind = "class"
	KindInterface Kind = "interface"
	KindEnum      Kind = "enum"
	KindStruct    Kind = "struct"
	KindType      Kind = "type"
	KindFunc      Kind = "func"
	KindMethod    Kind = "method"
)

type Def struct {
	Name      string
	Kind      Kind
	Container string // enclosing class/type, "" at top level
	Line      int    // 1-based
}

// Language returns the language key for a file path, or "" if the file
// has no symbol support (it still gets full-text search).
func Language(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".java":
		return "java"
	case ".py":
		return "python"
	case ".ts", ".tsx", ".js", ".jsx", ".mjs":
		return "typescript"
	}
	return ""
}

// Extract returns the definitions in src. Unknown languages return nil.
// Extractors never fail: on malformed input they return what they can.
func Extract(path string, src []byte) []Def {
	switch Language(path) {
	case "go":
		return extractGo(src)
	case "java":
		return extractJava(src)
	case "python":
		return extractPython(src)
	case "typescript":
		return extractTS(src)
	}
	return nil
}

func isIdentByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// lineOf returns the 1-based line number of byte offset pos.
func lineOf(src []byte, pos int) int {
	line := 1
	for i := 0; i < pos && i < len(src); i++ {
		if src[i] == '\n' {
			line++
		}
	}
	return line
}

// lineIndex precomputes offsets -> line numbers for repeated lookups.
type lineIndex []int // starts[i] = offset of line i+1

func newLineIndex(src []byte) lineIndex {
	starts := []int{0}
	for i, c := range src {
		if c == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

func (li lineIndex) line(pos int) int {
	lo, hi := 0, len(li)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if li[mid] <= pos {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo + 1
}
