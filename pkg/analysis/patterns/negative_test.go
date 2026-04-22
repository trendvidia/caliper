package patterns

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"github.com/trendvidia/caliper/pkg/analysis"
)

func TestNegateInScope_PatternPresent(t *testing.T) {
	pat, err := Compile("$x.Close()")
	if err != nil {
		t.Fatal(err)
	}
	neg := NewNegative(pat)

	// Function body that contains f.Close().
	fset := token.NewFileSet()
	src := `package p; func f() { var f int; _ = f; close() }
func g() { f.Close() }`
	file, err := parser.ParseFile(fset, "", src, 0)
	if err != nil {
		t.Fatal(err)
	}

	// g() has f.Close() — NegateInScope should return false.
	fn := file.Decls[1].(*ast.FuncDecl)
	if neg.NegateInScope(fn.Body, nil) {
		t.Error("expected false: f.Close() is present in scope")
	}
}

func TestNegateInScope_PatternAbsent(t *testing.T) {
	pat, err := Compile("$x.Close()")
	if err != nil {
		t.Fatal(err)
	}
	neg := NewNegative(pat)

	fset := token.NewFileSet()
	src := `package p; func noClose() { x := 1; _ = x }`
	file, err := parser.ParseFile(fset, "", src, 0)
	if err != nil {
		t.Fatal(err)
	}

	fn := file.Decls[0].(*ast.FuncDecl)
	if !neg.NegateInScope(fn.Body, nil) {
		t.Error("expected true: Close() is absent from scope")
	}
}

func TestNegateInScope_EmptyBody(t *testing.T) {
	pat, err := Compile("$x.Close()")
	if err != nil {
		t.Fatal(err)
	}
	neg := NewNegative(pat)

	fset := token.NewFileSet()
	src := `package p; func empty() {}`
	file, err := parser.ParseFile(fset, "", src, 0)
	if err != nil {
		t.Fatal(err)
	}

	fn := file.Decls[0].(*ast.FuncDecl)
	if !neg.NegateInScope(fn.Body, nil) {
		t.Error("expected true: empty body has no matches")
	}
}

func setupNegativeProject(t *testing.T, files map[string]string) *analysis.Engine {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module testpkg\n\ngo 1.23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	engine, err := analysis.NewEngine(dir)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return engine
}

func TestMatchAbsence_MultipleFunctions(t *testing.T) {
	engine := setupNegativeProject(t, map[string]string{
		"code.go": `package testpkg

func hasClose() {
	var f struct{ Close func() }
	f.Close()
}

func noClose() {
	x := 1
	_ = x
}

func alsoNoClose() {
	y := "hello"
	_ = y
}
`,
	})

	pat, err := Compile("$x.Close()")
	if err != nil {
		t.Fatal(err)
	}
	neg := NewNegative(pat)

	absences := neg.MatchAbsence(engine)

	// hasClose has Close() so should NOT be in absences.
	// noClose and alsoNoClose lack Close() so should be in absences.
	foundNames := make(map[string]bool)
	for _, m := range absences {
		if b, ok := m.Bindings["func"]; ok {
			if id, ok := b.Node.(*ast.Ident); ok {
				foundNames[id.Name] = true
			}
		}
	}

	if foundNames["hasClose"] {
		t.Error("hasClose should NOT appear in absences — it calls Close()")
	}
	if !foundNames["noClose"] {
		t.Error("noClose should appear in absences")
	}
	if !foundNames["alsoNoClose"] {
		t.Error("alsoNoClose should appear in absences")
	}
}
