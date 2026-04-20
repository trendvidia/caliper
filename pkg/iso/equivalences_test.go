package iso

import (
	"go/ast"
	"go/token"
	"testing"
)

func TestRegistry_BuiltinCount(t *testing.T) {
	r := NewRegistry()
	all := r.ByCategory("all")
	// Should have at least the original 6 + 3 new = 9 equivalences.
	if len(all) < 9 {
		t.Errorf("expected at least 9 equivalences, got %d", len(all))
	}
}

func TestRegistry_ByCategory(t *testing.T) {
	r := NewRegistry()
	tests := []struct {
		category Category
		minCount int
	}{
		{CatNilChecks, 1},
		{CatBooleanLogic, 1},
		{CatComparison, 2},
		{CatErrorPattern, 3}, // errors_is, error_wrapping, errors_as
		{CatContext, 1},
		{CatConcurrency, 1},
	}
	for _, tt := range tests {
		equivs := r.ByCategory(tt.category)
		if len(equivs) < tt.minCount {
			t.Errorf("category %q: expected at least %d, got %d",
				tt.category, tt.minCount, len(equivs))
		}
	}
}

func TestNilEqualityNegation(t *testing.T) {
	r := NewRegistry()

	// x == nil → should expand to !(x != nil)
	node := &ast.BinaryExpr{
		X:  &ast.Ident{Name: "x"},
		Op: token.EQL,
		Y:  &ast.Ident{Name: "nil"},
	}
	expanded := r.Expand(node, nil)
	if len(expanded) == 0 {
		t.Error("expected expansion for x == nil")
	}

	// Check that we get a UnaryExpr with NOT.
	found := false
	for _, alt := range expanded {
		if u, ok := alt.(*ast.UnaryExpr); ok && u.Op == token.NOT {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected !(x != nil) in expansion of x == nil")
	}
}

func TestComparisonFlip(t *testing.T) {
	r := NewRegistry()

	// a < b → should expand to b > a
	node := &ast.BinaryExpr{
		X:  &ast.Ident{Name: "a"},
		Op: token.LSS,
		Y:  &ast.Ident{Name: "b"},
	}
	expanded := r.Expand(node, nil)
	found := false
	for _, alt := range expanded {
		if bin, ok := alt.(*ast.BinaryExpr); ok && bin.Op == token.GTR {
			if id, ok := bin.X.(*ast.Ident); ok && id.Name == "b" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected b > a in expansion of a < b")
	}
}

func TestDeMorgan(t *testing.T) {
	r := NewRegistry()

	// !(a && b) → !a || !b
	node := &ast.UnaryExpr{
		Op: token.NOT,
		X: &ast.ParenExpr{X: &ast.BinaryExpr{
			X:  &ast.Ident{Name: "a"},
			Op: token.LAND,
			Y:  &ast.Ident{Name: "b"},
		}},
	}
	expanded := r.Expand(node, nil)
	found := false
	for _, alt := range expanded {
		if bin, ok := alt.(*ast.BinaryExpr); ok && bin.Op == token.LOR {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected !a || !b in expansion of !(a && b)")
	}
}

func TestSliceNilVsLen(t *testing.T) {
	r := NewRegistry()

	// len(s) == 0 → len(s) < 1
	node := &ast.BinaryExpr{
		X: &ast.CallExpr{
			Fun:  &ast.Ident{Name: "len"},
			Args: []ast.Expr{&ast.Ident{Name: "s"}},
		},
		Op: token.EQL,
		Y:  &ast.BasicLit{Kind: token.INT, Value: "0"},
	}
	expanded := r.Expand(node, nil)
	found := false
	for _, alt := range expanded {
		if bin, ok := alt.(*ast.BinaryExpr); ok && bin.Op == token.LSS {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected len(s) < 1 in expansion of len(s) == 0")
	}
}

// --- New equivalence tests ---

func TestErrorsAs_TypeAssertion(t *testing.T) {
	r := NewRegistry()

	// errors.As(err, &pathErr) → should expand to type assertion form
	node := &ast.CallExpr{
		Fun: &ast.SelectorExpr{
			X:   &ast.Ident{Name: "errors"},
			Sel: &ast.Ident{Name: "As"},
		},
		Args: []ast.Expr{
			&ast.Ident{Name: "err"},
			&ast.UnaryExpr{
				Op: token.AND,
				X:  &ast.Ident{Name: "pathErr"},
			},
		},
	}
	expanded := r.Expand(node, nil)
	if len(expanded) == 0 {
		t.Error("expected expansion for errors.As(err, &pathErr)")
	}

	// Check that we get an AssignStmt with := and TypeAssertExpr.
	found := false
	for _, alt := range expanded {
		if assign, ok := alt.(*ast.AssignStmt); ok && assign.Tok == token.DEFINE {
			if len(assign.Rhs) == 1 {
				if _, isTypeAssert := assign.Rhs[0].(*ast.TypeAssertExpr); isTypeAssert {
					found = true
					break
				}
			}
		}
	}
	if !found {
		t.Error("expected type assertion assignment in expansion of errors.As")
	}
}

func TestContextTodo_Background(t *testing.T) {
	r := NewRegistry()

	// context.TODO() → context.Background()
	todoNode := &ast.CallExpr{
		Fun: &ast.SelectorExpr{
			X:   &ast.Ident{Name: "context"},
			Sel: &ast.Ident{Name: "TODO"},
		},
	}
	expanded := r.Expand(todoNode, nil)
	found := false
	for _, alt := range expanded {
		if call, ok := alt.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if sel.Sel.Name == "Background" {
					found = true
					break
				}
			}
		}
	}
	if !found {
		t.Error("expected context.Background() in expansion of context.TODO()")
	}

	// And the reverse: context.Background() → context.TODO()
	bgNode := &ast.CallExpr{
		Fun: &ast.SelectorExpr{
			X:   &ast.Ident{Name: "context"},
			Sel: &ast.Ident{Name: "Background"},
		},
	}
	expanded = r.Expand(bgNode, nil)
	found = false
	for _, alt := range expanded {
		if call, ok := alt.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if sel.Sel.Name == "TODO" {
					found = true
					break
				}
			}
		}
	}
	if !found {
		t.Error("expected context.TODO() in expansion of context.Background()")
	}
}

func TestMutex_RWMutex_NoTypeInfo(t *testing.T) {
	r := NewRegistry()

	// Without type info, the mutex equivalence shouldn't fire.
	node := &ast.CallExpr{
		Fun: &ast.SelectorExpr{
			X:   &ast.Ident{Name: "mu"},
			Sel: &ast.Ident{Name: "Lock"},
		},
	}
	expanded := r.Expand(node, nil)
	// Without type info, the mutex equivalence returns nil.
	for _, alt := range expanded {
		if call, ok := alt.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if sel.Sel.Name == "Lock" {
					t.Error("mutex equivalence should not fire without type info")
				}
			}
		}
	}
}

func TestExpand_NonMatchingNode(t *testing.T) {
	r := NewRegistry()

	// A plain ident should have no expansions.
	node := &ast.Ident{Name: "x"}
	expanded := r.Expand(node, nil)
	if len(expanded) != 0 {
		t.Errorf("expected no expansions for ident, got %d", len(expanded))
	}
}

func TestRegistry_Expand_ReturnsAlternatives(t *testing.T) {
	r := NewRegistry()

	// x != nil should expand to at least 2 forms:
	// 1. !(x == nil) from nil_equality_negation
	// 2. nil != x from equality_commutativity
	node := &ast.BinaryExpr{
		X:  &ast.Ident{Name: "x"},
		Op: token.NEQ,
		Y:  &ast.Ident{Name: "nil"},
	}
	expanded := r.Expand(node, nil)
	if len(expanded) < 2 {
		t.Errorf("expected at least 2 expansions for x != nil, got %d", len(expanded))
	}
}
