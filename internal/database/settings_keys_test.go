package database

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// DB1-1: report_delivery_min_interval_minutes had a constant, bounds, a
// default and a case in GetPlatformSettings' switch, and was in neither the
// key list the switch reads from nor the write loop — so the case was dead
// and the dial read 15 forever. Every case label in that switch must be a key
// platformSettingKeys asks for; the comment on that function already said so.
func TestEverySettingTheReaderSwitchesOnIsQueried(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "settings.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	queried := map[string]bool{}
	for _, k := range platformSettingKeys() {
		queried[k] = true
	}

	consts := map[string]string{}
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, s := range g.Specs {
			vs := s.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if i < len(vs.Values) {
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						consts[n.Name] = lit.Value[1 : len(lit.Value)-1]
					}
				}
			}
		}
	}

	var reader *ast.FuncDecl
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "GetPlatformSettings" {
			reader = fn
		}
	}
	if reader == nil {
		t.Fatal("GetPlatformSettings not found")
	}
	cases := 0
	ast.Inspect(reader, func(n ast.Node) bool {
		cc, ok := n.(*ast.CaseClause)
		if !ok {
			return true
		}
		for _, e := range cc.List {
			id, ok := e.(*ast.Ident)
			if !ok {
				continue
			}
			key, ok := consts[id.Name]
			if !ok {
				continue
			}
			cases++
			if !queried[key] {
				t.Errorf("%s (%q) is read by GetPlatformSettings' switch but not in platformSettingKeys(): it reads as its default forever", id.Name, key)
			}
		}
		return true
	})
	if cases < 20 {
		t.Fatalf("found only %d setting cases — the test no longer sees the switch", cases)
	}
}
