package symbol

import (
	"go/ast"
	"go/parser"
	"go/token"
)

// extractGo uses the stdlib parser: exact results, zero heuristics.
// Files that fail to parse fall back to whatever declarations were
// recovered before the error.
func extractGo(src []byte) []Def {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "src.go", src, parser.SkipObjectResolution)
	if f == nil {
		_ = err
		return nil
	}
	var defs []Def
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			def := Def{Name: d.Name.Name, Kind: KindFunc, Line: fset.Position(d.Pos()).Line}
			if d.Recv != nil && len(d.Recv.List) > 0 {
				def.Kind = KindMethod
				def.Container = recvTypeName(d.Recv.List[0].Type)
			}
			defs = append(defs, def)
		case *ast.GenDecl:
			if d.Tok != token.TYPE {
				continue
			}
			for _, spec := range d.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				kind := KindType
				switch ts.Type.(type) {
				case *ast.StructType:
					kind = KindStruct
				case *ast.InterfaceType:
					kind = KindInterface
				}
				defs = append(defs, Def{Name: ts.Name.Name, Kind: kind, Line: fset.Position(ts.Pos()).Line})
			}
		}
	}
	return defs
}

func recvTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return recvTypeName(t.X)
	case *ast.IndexExpr: // generic receiver T[P]
		return recvTypeName(t.X)
	case *ast.IndexListExpr:
		return recvTypeName(t.X)
	}
	return ""
}
