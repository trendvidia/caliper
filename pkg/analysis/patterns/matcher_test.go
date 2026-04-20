package patterns

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
)

// helper to compile and match a pattern against a Go expression with type info.
func matchExprWithTypes(t *testing.T, pattern, code string, tc TypeConstraint) *Bindings {
	t.Helper()

	pat, err := Compile(pattern)
	if err != nil {
		t.Fatalf("compile pattern %q: %v", pattern, err)
	}
	if tc != nil {
		pat = pat.WithTypeConstraints(tc)
	}

	// Parse code as a Go source file with a function body.
	// Use only builtin types — no imports needed.
	src := `package p

func f() {
	var x int = 1
	var y string = "hello"
	var err error = nil
	_ = x; _ = y; _ = err
	_ = ` + code + `
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "test.go", src, 0)
	if err != nil {
		t.Fatalf("parse code %q: %v", code, err)
	}

	// Type check.
	conf := types.Config{
		Importer: nil,
		Error:    func(err error) {}, // ignore errors
	}
	info := &types.Info{
		Types: make(map[ast.Expr]types.TypeAndValue),
		Defs:  make(map[*ast.Ident]types.Object),
		Uses:  make(map[*ast.Ident]types.Object),
	}
	conf.Check("p", fset, []*ast.File{file}, info)

	// Walk all nodes to find a match.
	var result *Bindings
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil || result != nil {
			return result == nil
		}
		b := pat.MatchNode(n, info)
		if b != nil {
			result = b
			return false
		}
		return true
	})
	return result
}

func TestMatchNode_BasicIdent(t *testing.T) {
	pat, err := Compile("$x")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	node := &ast.Ident{Name: "foo"}
	b := pat.MatchNode(node, nil)
	if b == nil {
		t.Fatal("expected match")
	}
	if (*b)["x"] == nil || (*b)["x"].Node.(*ast.Ident).Name != "foo" {
		t.Error("expected x bound to foo")
	}
}

func TestMatchNode_Wildcard(t *testing.T) {
	pat, err := Compile("$_.Close()")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	node := &ast.CallExpr{
		Fun: &ast.SelectorExpr{
			X:   &ast.Ident{Name: "conn"},
			Sel: &ast.Ident{Name: "Close"},
		},
	}
	b := pat.MatchNode(node, nil)
	if b == nil {
		t.Fatal("expected match for wildcard")
	}
}

func TestMatchNode_ConsistentBinding(t *testing.T) {
	// $x + $x should only match when both sides are the same
	pat, err := Compile("$x + $x")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// a + a → should match
	match := &ast.BinaryExpr{
		X: &ast.Ident{Name: "a"}, Op: token.ADD, Y: &ast.Ident{Name: "a"},
	}
	if b := pat.MatchNode(match, nil); b == nil {
		t.Error("expected match for a + a")
	}

	// a + b → should not match
	noMatch := &ast.BinaryExpr{
		X: &ast.Ident{Name: "a"}, Op: token.ADD, Y: &ast.Ident{Name: "b"},
	}
	if b := pat.MatchNode(noMatch, nil); b != nil {
		t.Error("expected no match for a + b")
	}
}

func TestMatchNode_SelectorExpr(t *testing.T) {
	pat, err := Compile("$x.Close()")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	node := &ast.CallExpr{
		Fun: &ast.SelectorExpr{
			X:   &ast.Ident{Name: "f"},
			Sel: &ast.Ident{Name: "Close"},
		},
	}
	b := pat.MatchNode(node, nil)
	if b == nil {
		t.Fatal("expected match")
	}
	if (*b)["x"].Node.(*ast.Ident).Name != "f" {
		t.Error("expected x bound to f")
	}
}

func TestMatchNode_VariadicArgs(t *testing.T) {
	pat, err := Compile("foo($args...)")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// foo() → matches (variadic = 0 args)
	node0 := &ast.CallExpr{
		Fun: &ast.Ident{Name: "foo"},
	}
	if b := pat.MatchNode(node0, nil); b == nil {
		t.Error("expected match for foo() with variadic")
	}

	// foo(a, b) → matches
	node2 := &ast.CallExpr{
		Fun:  &ast.Ident{Name: "foo"},
		Args: []ast.Expr{&ast.Ident{Name: "a"}, &ast.Ident{Name: "b"}},
	}
	if b := pat.MatchNode(node2, nil); b == nil {
		t.Error("expected match for foo(a, b) with variadic")
	}
}

func TestMatchNode_NoMatch(t *testing.T) {
	pat, err := Compile("$x.Close()")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// Doesn't match a plain ident.
	node := &ast.Ident{Name: "foo"}
	if b := pat.MatchNode(node, nil); b != nil {
		t.Error("expected no match for plain ident")
	}

	// Doesn't match a call with different method name.
	node2 := &ast.CallExpr{
		Fun: &ast.SelectorExpr{
			X:   &ast.Ident{Name: "f"},
			Sel: &ast.Ident{Name: "Open"},
		},
	}
	if b := pat.MatchNode(node2, nil); b != nil {
		t.Error("expected no match for Open")
	}
}

func TestTypeConstraint_IntType(t *testing.T) {
	b := matchExprWithTypes(t, "$x + $x", "x + x", TypeConstraint{"x": "int"})
	if b == nil {
		t.Fatal("expected match for int + int with int constraint")
	}
}

func TestTypeConstraint_WrongType(t *testing.T) {
	// y is a string, constraint requires int → no match
	// Use y + y to make the match unambiguous (only y + y in the AST)
	b := matchExprWithTypes(t, "$x + $x", "y + y", TypeConstraint{"x": "int"})
	if b != nil {
		t.Error("expected no match: string does not satisfy int constraint")
	}
}

func TestTypeConstraint_ErrorType(t *testing.T) {
	b := matchExprWithTypes(t, "$x", "err", TypeConstraint{"x": "error"})
	if b == nil {
		t.Fatal("expected match for error variable with error constraint")
	}
}

func TestTypeConstraint_NoConstraint(t *testing.T) {
	// No type constraint → match anything
	b := matchExprWithTypes(t, "$x", "x", nil)
	if b == nil {
		t.Fatal("expected match without type constraint")
	}
}

// mockExpander is a test EquivalenceExpander that transforms
// x == nil ↔ !(x != nil)
type mockExpander struct{}

func (m *mockExpander) Expand(node ast.Node, info *types.Info) []ast.Node {
	bin, ok := node.(*ast.BinaryExpr)
	if !ok {
		return nil
	}
	nilIdent, isNil := bin.Y.(*ast.Ident)
	if !isNil || nilIdent.Name != "nil" {
		return nil
	}
	switch bin.Op {
	case token.EQL: // x == nil → !(x != nil)
		return []ast.Node{
			&ast.UnaryExpr{
				Op: token.NOT,
				X: &ast.ParenExpr{X: &ast.BinaryExpr{
					X: bin.X, Op: token.NEQ, Y: bin.Y,
				}},
			},
		}
	case token.NEQ: // x != nil → !(x == nil)
		return []ast.Node{
			&ast.UnaryExpr{
				Op: token.NOT,
				X: &ast.ParenExpr{X: &ast.BinaryExpr{
					X: bin.X, Op: token.EQL, Y: bin.Y,
				}},
			},
		}
	}
	return nil
}

func TestEquivalenceExpansion_NilCheck(t *testing.T) {
	// Pattern: !(x != nil)
	// Target: x == nil
	// With equivalence expansion, x == nil should match !(x != nil) pattern.
	pat, err := Compile("!($x != nil)")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	pat = pat.WithEquivalences(&mockExpander{})

	// Target is x == nil (direct form doesn't match !(x != nil),
	// but the expanded form !(x != nil) does).
	target := &ast.BinaryExpr{
		X:  &ast.Ident{Name: "x"},
		Op: token.EQL,
		Y:  &ast.Ident{Name: "nil"},
	}

	// Mock types.Info (minimal — just need non-nil for equivalence to kick in).
	info := &types.Info{
		Types: make(map[ast.Expr]types.TypeAndValue),
	}

	b := pat.MatchNode(target, info)
	if b == nil {
		t.Error("expected match via equivalence expansion")
	}
}

func TestEquivalenceExpansion_NoExpander(t *testing.T) {
	// Without equivalences, !(x != nil) should NOT match x == nil.
	pat, err := Compile("!($x != nil)")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	target := &ast.BinaryExpr{
		X:  &ast.Ident{Name: "x"},
		Op: token.EQL,
		Y:  &ast.Ident{Name: "nil"},
	}

	b := pat.MatchNode(target, nil)
	if b != nil {
		t.Error("expected no match without equivalence expander")
	}
}

func TestCompile_InvalidPattern(t *testing.T) {
	_, err := Compile("$")
	if err == nil {
		t.Error("expected error for empty metavariable")
	}
}

func TestStmtWildcard_IfBody(t *testing.T) {
	// Pattern: if $err != nil { ... }
	// Should match any if-err-nil block regardless of body.
	pat, err := Compile("if $err != nil { ... }")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// Target: if err != nil { return err }
	fset := token.NewFileSet()
	src := `package p; func f() { if err != nil { return } }`
	file, parseErr := parser.ParseFile(fset, "", src, 0)
	if parseErr != nil {
		t.Fatalf("parse: %v", parseErr)
	}

	var matched bool
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		b := pat.MatchNode(n, nil)
		if b != nil {
			matched = true
			return false
		}
		return true
	})
	if !matched {
		t.Error("expected match for if err != nil { ... }")
	}
}

func TestStmtWildcard_IfBodyMultipleStmts(t *testing.T) {
	// Pattern: if $cond { ... }
	// Should match if body with multiple statements.
	pat, err := Compile("if $cond { ... }")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	fset := token.NewFileSet()
	src := `package p; func f() { if x > 0 { a(); b(); c() } }`
	file, parseErr := parser.ParseFile(fset, "", src, 0)
	if parseErr != nil {
		t.Fatalf("parse: %v", parseErr)
	}

	var matched bool
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		if pat.MatchNode(n, nil) != nil {
			matched = true
			return false
		}
		return true
	})
	if !matched {
		t.Error("expected match for if with multiple statements in body")
	}
}

func TestMultiStatement_AssignAndIf(t *testing.T) {
	// Pattern: $x := $fn(); if $x != nil { ... }
	pat, err := Compile("$x := $fn(); if $x != nil { ... }")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !pat.IsMultiStatement() {
		t.Fatal("expected multi-statement pattern")
	}

	fset := token.NewFileSet()
	src := `package p; func f() {
		a := 1
		err := getVal()
		if err != nil { log() }
		b := 2
		_ = a; _ = b
	}`
	file, parseErr := parser.ParseFile(fset, "", src, 0)
	if parseErr != nil {
		t.Fatalf("parse: %v", parseErr)
	}

	// Walk to find the function body block.
	var matched bool
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		if pat.MatchNode(n, nil) != nil {
			matched = true
			return false
		}
		return true
	})
	if !matched {
		t.Error("expected match for multi-statement assign+if pattern")
	}
}

func TestMultiStatement_NoMatch(t *testing.T) {
	// Pattern: $x := $fn(); if $x != nil { ... }
	pat, err := Compile("$x := $fn(); if $x != nil { ... }")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	fset := token.NewFileSet()
	// No matching consecutive statements.
	src := `package p; func f() {
		err := getVal()
		doSomething()
		if err != nil { log() }
	}`
	file, parseErr := parser.ParseFile(fset, "", src, 0)
	if parseErr != nil {
		t.Fatalf("parse: %v", parseErr)
	}

	var matched bool
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		if pat.MatchNode(n, nil) != nil {
			matched = true
			return false
		}
		return true
	})
	if matched {
		t.Error("expected no match — statements are not consecutive")
	}
}

func TestMultiStatement_ConsistentBindingAcrossStmts(t *testing.T) {
	// $x in the assign must be the same $x in the if condition.
	pat, err := Compile("$x := $fn(); if $x != nil { ... }")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	fset := token.NewFileSet()
	// err in assign but different var in if → no match
	src := `package p; func f() {
		err := getVal()
		if other != nil { log() }
	}`
	file, parseErr := parser.ParseFile(fset, "", src, 0)
	if parseErr != nil {
		t.Fatalf("parse: %v", parseErr)
	}

	var matched bool
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		if pat.MatchNode(n, nil) != nil {
			matched = true
			return false
		}
		return true
	})
	if matched {
		t.Error("expected no match — different variable in assign vs if")
	}
}

func TestMatchNode_BinaryExpr(t *testing.T) {
	pat, err := Compile("$a + $b")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	node := &ast.BinaryExpr{
		X: &ast.Ident{Name: "x"}, Op: token.ADD, Y: &ast.Ident{Name: "y"},
	}
	b := pat.MatchNode(node, nil)
	if b == nil {
		t.Fatal("expected match for x + y")
	}
	if (*b)["a"].Node.(*ast.Ident).Name != "x" {
		t.Error("expected a bound to x")
	}
	if (*b)["b"].Node.(*ast.Ident).Name != "y" {
		t.Error("expected b bound to y")
	}
}

func TestMatchNode_BinaryExpr_WrongOp(t *testing.T) {
	pat, err := Compile("$a + $b")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	node := &ast.BinaryExpr{
		X: &ast.Ident{Name: "x"}, Op: token.MUL, Y: &ast.Ident{Name: "y"},
	}
	if b := pat.MatchNode(node, nil); b != nil {
		t.Error("expected no match for multiplication with addition pattern")
	}
}

func TestMatchNode_AssignStmt(t *testing.T) {
	pat, err := Compile("$x := $y")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	fset := token.NewFileSet()
	src := `package p; func f() { a := b }`
	file, parseErr := parser.ParseFile(fset, "", src, 0)
	if parseErr != nil {
		t.Fatalf("parse: %v", parseErr)
	}

	var matched bool
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		if pat.MatchNode(n, nil) != nil {
			matched = true
			return false
		}
		return true
	})
	if !matched {
		t.Error("expected match for $x := $y against a := b")
	}
}

func TestMatchNode_ReturnStmt(t *testing.T) {
	pat, err := Compile("return $x")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	fset := token.NewFileSet()
	src := `package p; func f() int { return 42 }`
	file, parseErr := parser.ParseFile(fset, "", src, 0)
	if parseErr != nil {
		t.Fatalf("parse: %v", parseErr)
	}

	var matched bool
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		if pat.MatchNode(n, nil) != nil {
			matched = true
			return false
		}
		return true
	})
	if !matched {
		t.Error("expected match for return $x against return 42")
	}
}

func TestMatchNode_IndexExpr(t *testing.T) {
	pat, err := Compile("$x[$i]")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	node := &ast.IndexExpr{
		X:     &ast.Ident{Name: "arr"},
		Index: &ast.Ident{Name: "idx"},
	}
	b := pat.MatchNode(node, nil)
	if b == nil {
		t.Fatal("expected match for arr[idx]")
	}
	if (*b)["x"].Node.(*ast.Ident).Name != "arr" {
		t.Error("expected x bound to arr")
	}
	if (*b)["i"].Node.(*ast.Ident).Name != "idx" {
		t.Error("expected i bound to idx")
	}
}

func TestMatchNode_StarExpr(t *testing.T) {
	pat, err := Compile("*$x")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	node := &ast.StarExpr{X: &ast.Ident{Name: "ptr"}}
	b := pat.MatchNode(node, nil)
	if b == nil {
		t.Fatal("expected match for *ptr")
	}
	if (*b)["x"].Node.(*ast.Ident).Name != "ptr" {
		t.Error("expected x bound to ptr")
	}
}

func TestMatchNode_VariadicWithPrefix(t *testing.T) {
	// foo(ctx, $args...) should match foo(ctx, a, b)
	pat, err := Compile("foo(ctx, $args...)")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	node := &ast.CallExpr{
		Fun: &ast.Ident{Name: "foo"},
		Args: []ast.Expr{
			&ast.Ident{Name: "ctx"},
			&ast.Ident{Name: "a"},
			&ast.Ident{Name: "b"},
		},
	}
	if b := pat.MatchNode(node, nil); b == nil {
		t.Error("expected match for foo(ctx, a, b) with variadic after ctx")
	}
}

func TestMatchNode_VariadicNoPrefix(t *testing.T) {
	// bar($args...) should match bar() with no args
	pat, err := Compile("bar($args...)")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	node := &ast.CallExpr{
		Fun: &ast.Ident{Name: "bar"},
	}
	if b := pat.MatchNode(node, nil); b == nil {
		t.Error("expected match for bar() with variadic pattern")
	}
}

func TestStmtWildcard_EmptyBody(t *testing.T) {
	// Pattern: if $cond { ... }
	// Should match even an empty body (... matches zero statements).
	pat, err := Compile("if $cond { ... }")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	fset := token.NewFileSet()
	src := `package p; func f() { if true { } }`
	file, parseErr := parser.ParseFile(fset, "", src, 0)
	if parseErr != nil {
		t.Fatalf("parse: %v", parseErr)
	}

	var matched bool
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		if pat.MatchNode(n, nil) != nil {
			matched = true
			return false
		}
		return true
	})
	if !matched {
		t.Error("expected match for if with empty body and ... wildcard")
	}
}

// --------------------------------------------------------------------------
// Name constraint tests
// --------------------------------------------------------------------------

func TestNameConstraint_MatchingIdent(t *testing.T) {
	pat, err := Compile("$x")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	pat, err = pat.WithNameConstraints(NameConstraint{"x": "^test"})
	if err != nil {
		t.Fatalf("name constraint: %v", err)
	}

	// testFoo matches ^test
	node := &ast.Ident{Name: "testFoo"}
	if b := pat.MatchNode(node, nil); b == nil {
		t.Error("expected match for testFoo with ^test constraint")
	}

	// prodBar does not match ^test
	node2 := &ast.Ident{Name: "prodBar"}
	if b := pat.MatchNode(node2, nil); b != nil {
		t.Error("expected no match for prodBar with ^test constraint")
	}
}

func TestNameConstraint_SelectorExpr(t *testing.T) {
	pat, err := Compile("$x.Close()")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	pat, err = pat.WithNameConstraints(NameConstraint{"x": "^(db|conn)$"})
	if err != nil {
		t.Fatalf("name constraint: %v", err)
	}

	// db.Close() matches
	match := &ast.CallExpr{
		Fun: &ast.SelectorExpr{
			X: &ast.Ident{Name: "db"}, Sel: &ast.Ident{Name: "Close"},
		},
	}
	if b := pat.MatchNode(match, nil); b == nil {
		t.Error("expected match for db.Close()")
	}

	// f.Close() does not match
	noMatch := &ast.CallExpr{
		Fun: &ast.SelectorExpr{
			X: &ast.Ident{Name: "f"}, Sel: &ast.Ident{Name: "Close"},
		},
	}
	if b := pat.MatchNode(noMatch, nil); b != nil {
		t.Error("expected no match for f.Close() with ^(db|conn)$ constraint")
	}
}

func TestNameConstraint_UnboundMetavar(t *testing.T) {
	// Constraint on a metavar that doesn't appear in the pattern.
	pat, err := Compile("$x")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	pat, err = pat.WithNameConstraints(NameConstraint{"y": "^foo"})
	if err != nil {
		t.Fatalf("name constraint: %v", err)
	}

	// $y is never bound, so constraint fails.
	node := &ast.Ident{Name: "anything"}
	if b := pat.MatchNode(node, nil); b != nil {
		t.Error("expected no match when constrained metavar is unbound")
	}
}

func TestNameConstraint_NonIdentBinding(t *testing.T) {
	// $x binds to a CallExpr (not an Ident) — name constraint should reject.
	pat, err := Compile("$x + 1")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	pat, err = pat.WithNameConstraints(NameConstraint{"x": "^foo"})
	if err != nil {
		t.Fatalf("name constraint: %v", err)
	}

	node := &ast.BinaryExpr{
		X:  &ast.CallExpr{Fun: &ast.Ident{Name: "foo"}},
		Op: token.ADD,
		Y:  &ast.BasicLit{Kind: token.INT, Value: "1"},
	}
	if b := pat.MatchNode(node, nil); b != nil {
		t.Error("expected no match when metavar binds to non-ident")
	}
}

func TestNameConstraint_InvalidRegex(t *testing.T) {
	pat, err := Compile("$x")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	_, err = pat.WithNameConstraints(NameConstraint{"x": "[invalid"})
	if err == nil {
		t.Error("expected error for invalid regex")
	}
}

func TestNameConstraint_CombinedWithTypeConstraint(t *testing.T) {
	// Both type and name constraints must pass.
	b := matchExprWithTypes(t, "$x + $x", "x + x", TypeConstraint{"x": "int"})
	if b == nil {
		t.Fatal("sanity check: expected match without name constraint")
	}

	// Now add a name constraint that also passes.
	pat, err := Compile("$x")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	pat = pat.WithTypeConstraints(TypeConstraint{"x": "int"})
	pat, err = pat.WithNameConstraints(NameConstraint{"x": "^x$"})
	if err != nil {
		t.Fatalf("name constraint: %v", err)
	}

	node := &ast.Ident{Name: "x"}
	if b := pat.MatchNode(node, nil); b == nil {
		// Without type info, type constraint is skipped (info is nil).
		// Name constraint still applies.
		t.Error("expected match for x with ^x$ name constraint (no type info)")
	}
}
