package transform

import (
	"go/ast"
	"go/printer"
	"go/token"
	"strings"
	"testing"

	"github.com/trendvidia/gocaliper/pkg/analysis/patterns"
)

func nodeToString(node ast.Node) string {
	var buf strings.Builder
	fset := token.NewFileSet()
	printer.Fprint(&buf, fset, node)
	return buf.String()
}

func TestSubstituteBindings_SimpleIdent(t *testing.T) {
	// Pattern: $x.Close() → Replace: $x.Release()
	replace, err := patterns.Compile("$x.Release()")
	if err != nil {
		t.Fatalf("compile replace: %v", err)
	}

	bindings := patterns.Bindings{
		"x": &patterns.Binding{
			Name: "x",
			Node: &ast.Ident{Name: "conn"},
		},
	}

	result := substituteBindings(replace, &bindings)
	if result == nil {
		t.Fatal("substituteBindings returned nil")
	}

	got := nodeToString(result)
	if got != "conn.Release()" {
		t.Errorf("got %q, want %q", got, "conn.Release()")
	}
}

func TestSubstituteBindings_MultipleMetavars(t *testing.T) {
	// Replace: $obj.NewMethod($arg)
	replace, err := patterns.Compile("$obj.NewMethod($arg)")
	if err != nil {
		t.Fatalf("compile replace: %v", err)
	}

	bindings := patterns.Bindings{
		"obj": &patterns.Binding{
			Name: "obj",
			Node: &ast.Ident{Name: "client"},
		},
		"arg": &patterns.Binding{
			Name: "arg",
			Node: &ast.Ident{Name: "ctx"},
		},
	}

	result := substituteBindings(replace, &bindings)
	if result == nil {
		t.Fatal("substituteBindings returned nil")
	}

	got := nodeToString(result)
	if got != "client.NewMethod(ctx)" {
		t.Errorf("got %q, want %q", got, "client.NewMethod(ctx)")
	}
}

func TestSubstituteBindings_BinaryExpr(t *testing.T) {
	// Replace: $a + $b
	replace, err := patterns.Compile("$a + $b")
	if err != nil {
		t.Fatalf("compile replace: %v", err)
	}

	bindings := patterns.Bindings{
		"a": &patterns.Binding{
			Name: "a",
			Node: &ast.Ident{Name: "x"},
		},
		"b": &patterns.Binding{
			Name: "b",
			Node: &ast.BasicLit{Kind: token.INT, Value: "42"},
		},
	}

	result := substituteBindings(replace, &bindings)
	if result == nil {
		t.Fatal("substituteBindings returned nil")
	}

	got := nodeToString(result)
	if got != "x + 42" {
		t.Errorf("got %q, want %q", got, "x + 42")
	}
}

func TestSubstituteBindings_NestedCall(t *testing.T) {
	// Replace: errors.Is($err, $target)
	replace, err := patterns.Compile("errors.Is($err, $target)")
	if err != nil {
		t.Fatalf("compile replace: %v", err)
	}

	bindings := patterns.Bindings{
		"err": &patterns.Binding{
			Name: "err",
			Node: &ast.Ident{Name: "myErr"},
		},
		"target": &patterns.Binding{
			Name: "target",
			Node: &ast.SelectorExpr{
				X:   &ast.Ident{Name: "io"},
				Sel: &ast.Ident{Name: "EOF"},
			},
		},
	}

	result := substituteBindings(replace, &bindings)
	if result == nil {
		t.Fatal("substituteBindings returned nil")
	}

	got := nodeToString(result)
	if got != "errors.Is(myErr, io.EOF)" {
		t.Errorf("got %q, want %q", got, "errors.Is(myErr, io.EOF)")
	}
}

func TestSubstituteBindings_NilInputs(t *testing.T) {
	// nil replace pattern
	result := substituteBindings(nil, &patterns.Bindings{})
	if result != nil {
		t.Error("expected nil for nil replace pattern")
	}

	// nil bindings
	replace, _ := patterns.Compile("foo()")
	result = substituteBindings(replace, nil)
	if result != nil {
		t.Error("expected nil for nil bindings")
	}
}

func TestSubstituteBindings_NoMetavars(t *testing.T) {
	// Replace pattern with no metavariables — should clone as-is.
	replace, err := patterns.Compile("fmt.Println()")
	if err != nil {
		t.Fatalf("compile replace: %v", err)
	}

	bindings := patterns.Bindings{}
	result := substituteBindings(replace, &bindings)
	if result == nil {
		t.Fatal("substituteBindings returned nil")
	}

	got := nodeToString(result)
	if got != "fmt.Println()" {
		t.Errorf("got %q, want %q", got, "fmt.Println()")
	}
}

func TestSubstituteBindings_ComplexBoundNode(t *testing.T) {
	// Replace: $fn($x)
	// With $fn bound to a selector expression and $x bound to a call
	replace, err := patterns.Compile("$fn($x)")
	if err != nil {
		t.Fatalf("compile replace: %v", err)
	}

	bindings := patterns.Bindings{
		"fn": &patterns.Binding{
			Name: "fn",
			Node: &ast.SelectorExpr{
				X:   &ast.Ident{Name: "log"},
				Sel: &ast.Ident{Name: "Printf"},
			},
		},
		"x": &patterns.Binding{
			Name: "x",
			Node: &ast.CallExpr{
				Fun:  &ast.SelectorExpr{X: &ast.Ident{Name: "err"}, Sel: &ast.Ident{Name: "Error"}},
				Args: nil,
			},
		},
	}

	result := substituteBindings(replace, &bindings)
	if result == nil {
		t.Fatal("substituteBindings returned nil")
	}

	got := nodeToString(result)
	if got != "log.Printf(err.Error())" {
		t.Errorf("got %q, want %q", got, "log.Printf(err.Error())")
	}
}
