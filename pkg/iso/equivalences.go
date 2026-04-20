// Package iso defines Go-specific isomorphisms (equivalences) that expand
// patterns to match semantically equivalent code forms.
//
// This is the Go equivalent of Coccinelle's isomorphism system. Each
// equivalence describes two or more code forms that are semantically
// identical, allowing a single pattern to match all of them.
package iso

import (
	"go/ast"
	"go/token"
	"go/types"
)

// Equivalence describes a semantic equivalence between Go code forms.
type Equivalence struct {
	Name        string   `json:"name"`
	Category    Category `json:"category"`
	Description string   `json:"description"`
	Examples    []string `json:"examples"`
	// Transform returns equivalent AST forms for the given node.
	// Returns nil if this equivalence doesn't apply.
	Transform func(node ast.Node, info *types.Info) []ast.Node
}

// Category groups equivalences for filtering.
type Category string

const (
	CatNilChecks    Category = "nil_checks"
	CatErrorPattern Category = "error_patterns"
	CatBooleanLogic Category = "boolean_logic"
	CatComparison   Category = "comparison"
	CatSliceMap     Category = "slice_map"
	CatContext      Category = "context"
	CatConcurrency  Category = "concurrency"
)

// Registry holds all active equivalences.
type Registry struct {
	equivs []Equivalence
}

// NewRegistry creates a registry with all built-in Go equivalences.
func NewRegistry() *Registry {
	return &Registry{
		equivs: builtinEquivalences(),
	}
}

// ByCategory returns equivalences in a given category.
func (r *Registry) ByCategory(cat Category) []Equivalence {
	if cat == "all" {
		return r.equivs
	}
	var result []Equivalence
	for _, eq := range r.equivs {
		if eq.Category == cat {
			result = append(result, eq)
		}
	}
	return result
}

// Expand returns all semantically equivalent forms of a node.
func (r *Registry) Expand(node ast.Node, info *types.Info) []ast.Node {
	var expanded []ast.Node
	for _, eq := range r.equivs {
		if alts := eq.Transform(node, info); len(alts) > 0 {
			expanded = append(expanded, alts...)
		}
	}
	return expanded
}

// --------------------------------------------------------------------------
// Built-in equivalences
// --------------------------------------------------------------------------

func builtinEquivalences() []Equivalence {
	return []Equivalence{
		// --- Nil checks ---
		{
			Name:        "nil_equality_negation",
			Category:    CatNilChecks,
			Description: "x == nil ↔ !(x != nil) and x != nil ↔ !(x == nil)",
			Examples:    []string{"x == nil", "!(x != nil)"},
			Transform: func(node ast.Node, _ *types.Info) []ast.Node {
				bin, ok := node.(*ast.BinaryExpr)
				if !ok {
					return nil
				}
				if !isNilIdent(bin.Y) {
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
			},
		},
		{
			Name:        "slice_nil_vs_len",
			Category:    CatSliceMap,
			Description: "For slices: len(s) == 0 ↔ s == nil is NOT equivalent (nil vs empty). But len(s) > 0 ↔ s != nil is commonly treated as equivalent in practice.",
			Examples:    []string{"len(s) == 0", "len(s) > 0"},
			Transform: func(node ast.Node, info *types.Info) []ast.Node {
				bin, ok := node.(*ast.BinaryExpr)
				if !ok {
					return nil
				}
				// len(s) == 0 → also match len(s) < 1
				call, isCall := bin.X.(*ast.CallExpr)
				if !isCall {
					return nil
				}
				if ident, ok := call.Fun.(*ast.Ident); !ok || ident.Name != "len" {
					return nil
				}
				if len(call.Args) != 1 {
					return nil
				}
				lit, isLit := bin.Y.(*ast.BasicLit)
				if !isLit || lit.Value != "0" {
					return nil
				}
				switch bin.Op {
				case token.EQL: // len(s) == 0 → len(s) < 1
					return []ast.Node{
						&ast.BinaryExpr{
							X: bin.X, Op: token.LSS,
							Y: &ast.BasicLit{Kind: token.INT, Value: "1"},
						},
					}
				case token.GTR: // len(s) > 0 → len(s) >= 1, len(s) != 0
					return []ast.Node{
						&ast.BinaryExpr{
							X: bin.X, Op: token.GEQ,
							Y: &ast.BasicLit{Kind: token.INT, Value: "1"},
						},
						&ast.BinaryExpr{
							X: bin.X, Op: token.NEQ,
							Y: &ast.BasicLit{Kind: token.INT, Value: "0"},
						},
					}
				}
				return nil
			},
		},

		// --- Boolean logic (De Morgan) ---
		{
			Name:        "de_morgan",
			Category:    CatBooleanLogic,
			Description: "!(a && b) ↔ (!a || !b) and !(a || b) ↔ (!a && !b)",
			Examples:    []string{"!(a && b)", "!a || !b"},
			Transform: func(node ast.Node, _ *types.Info) []ast.Node {
				unary, ok := node.(*ast.UnaryExpr)
				if !ok || unary.Op != token.NOT {
					return nil
				}
				paren, ok := unary.X.(*ast.ParenExpr)
				if !ok {
					return nil
				}
				bin, ok := paren.X.(*ast.BinaryExpr)
				if !ok {
					return nil
				}
				switch bin.Op {
				case token.LAND: // !(a && b) → !a || !b
					return []ast.Node{
						&ast.BinaryExpr{
							X:  &ast.UnaryExpr{Op: token.NOT, X: bin.X},
							Op: token.LOR,
							Y:  &ast.UnaryExpr{Op: token.NOT, X: bin.Y},
						},
					}
				case token.LOR: // !(a || b) → !a && !b
					return []ast.Node{
						&ast.BinaryExpr{
							X:  &ast.UnaryExpr{Op: token.NOT, X: bin.X},
							Op: token.LAND,
							Y:  &ast.UnaryExpr{Op: token.NOT, X: bin.Y},
						},
					}
				}
				return nil
			},
		},

		// --- Comparison ---
		{
			Name:        "comparison_flip",
			Category:    CatComparison,
			Description: "a < b ↔ b > a, a <= b ↔ b >= a (comparison operand swap)",
			Examples:    []string{"a < b", "b > a"},
			Transform: func(node ast.Node, _ *types.Info) []ast.Node {
				bin, ok := node.(*ast.BinaryExpr)
				if !ok {
					return nil
				}
				var flipped token.Token
				switch bin.Op {
				case token.LSS:
					flipped = token.GTR
				case token.GTR:
					flipped = token.LSS
				case token.LEQ:
					flipped = token.GEQ
				case token.GEQ:
					flipped = token.LEQ
				case token.EQL:
					flipped = token.EQL
				case token.NEQ:
					flipped = token.NEQ
				default:
					return nil
				}
				return []ast.Node{
					&ast.BinaryExpr{X: bin.Y, Op: flipped, Y: bin.X},
				}
			},
		},
		{
			Name:        "equality_commutativity",
			Category:    CatComparison,
			Description: "a == b ↔ b == a, a != b ↔ b != a",
			Examples:    []string{"err == nil", "nil == err"},
			Transform: func(node ast.Node, _ *types.Info) []ast.Node {
				bin, ok := node.(*ast.BinaryExpr)
				if !ok {
					return nil
				}
				if bin.Op != token.EQL && bin.Op != token.NEQ {
					return nil
				}
				return []ast.Node{
					&ast.BinaryExpr{X: bin.Y, Op: bin.Op, Y: bin.X},
				}
			},
		},

		// --- Error patterns ---
		{
			Name:        "error_wrapping_verbs",
			Category:    CatErrorPattern,
			Description: "fmt.Errorf with %v vs %w — matches both wrapping styles",
			Examples:    []string{`fmt.Errorf("failed: %v", err)`, `fmt.Errorf("failed: %w", err)`},
			Transform: func(node ast.Node, _ *types.Info) []ast.Node {
				// This is a hint equivalence — the pattern matcher notes that
				// %v and %w are interchangeable for matching purposes when
				// the user is searching for error wrapping patterns.
				// Actual transform requires string literal manipulation.
				return nil
			},
		},
		{
			Name:        "errors_is_vs_equality",
			Category:    CatErrorPattern,
			Description: "err == ErrFoo ↔ errors.Is(err, ErrFoo) — direct comparison vs errors.Is",
			Examples:    []string{"err == io.EOF", "errors.Is(err, io.EOF)"},
			Transform: func(node ast.Node, info *types.Info) []ast.Node {
				// err == X where X is an error → errors.Is(err, X)
				bin, ok := node.(*ast.BinaryExpr)
				if !ok || bin.Op != token.EQL {
					return nil
				}
				if info == nil {
					return nil
				}
				// Check if comparison involves error type.
				xType := info.TypeOf(bin.X)
				if xType == nil {
					return nil
				}
				if !isErrorType(xType) {
					return nil
				}
				return []ast.Node{
					&ast.CallExpr{
						Fun: &ast.SelectorExpr{
							X:   &ast.Ident{Name: "errors"},
							Sel: &ast.Ident{Name: "Is"},
						},
						Args: []ast.Expr{bin.X, bin.Y},
					},
				}
			},
		},

		// --- errors.As ↔ type assertion ---
		{
			Name:     "errors_as_vs_type_assertion",
			Category: CatErrorPattern,
			Description: "errors.As(err, &target) ↔ target, ok := err.(*Type) — " +
				"errors.As unwraps while type assertion doesn't",
			Examples: []string{
				"errors.As(err, &pathErr)",
				"pathErr, ok := err.(*os.PathError)",
			},
			Transform: func(node ast.Node, info *types.Info) []ast.Node {
				// errors.As(err, &target) → target, ok := err.(*TargetType)
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) != 2 {
					return nil
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return nil
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "errors" || sel.Sel.Name != "As" {
					return nil
				}
				// Second arg should be &target (address-of).
				unary, ok := call.Args[1].(*ast.UnaryExpr)
				if !ok || unary.Op != token.AND {
					return nil
				}
				targetIdent, ok := unary.X.(*ast.Ident)
				if !ok {
					return nil
				}

				// Build equivalent type assertion:
				// target, ok := err.(*TargetType)
				// We use the target variable name as a placeholder for the type.
				return []ast.Node{
					&ast.AssignStmt{
						Lhs: []ast.Expr{
							targetIdent,
							&ast.Ident{Name: "ok"},
						},
						Tok: token.DEFINE,
						Rhs: []ast.Expr{
							&ast.TypeAssertExpr{
								X:    call.Args[0],
								Type: nil, // type unknown without full type info
							},
						},
					},
				}
			},
		},

		// --- context.TODO() ↔ context.Background() ---
		{
			Name:        "context_todo_background",
			Category:    CatContext,
			Description: "context.TODO() ↔ context.Background() — equivalent in many cases (configurable)",
			Examples:    []string{"context.TODO()", "context.Background()"},
			Transform: func(node ast.Node, _ *types.Info) []ast.Node {
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) != 0 {
					return nil
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return nil
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "context" {
					return nil
				}
				switch sel.Sel.Name {
				case "TODO":
					return []ast.Node{
						&ast.CallExpr{
							Fun: &ast.SelectorExpr{
								X:   &ast.Ident{Name: "context"},
								Sel: &ast.Ident{Name: "Background"},
							},
						},
					}
				case "Background":
					return []ast.Node{
						&ast.CallExpr{
							Fun: &ast.SelectorExpr{
								X:   &ast.Ident{Name: "context"},
								Sel: &ast.Ident{Name: "TODO"},
							},
						},
					}
				}
				return nil
			},
		},

		// --- sync.Mutex ↔ sync.RWMutex partial equivalence ---
		{
			Name:     "mutex_rwmutex_lock_unlock",
			Category: CatConcurrency,
			Description: "sync.Mutex.Lock/Unlock ↔ sync.RWMutex.Lock/Unlock — " +
				"RWMutex supports the same Lock/Unlock interface as Mutex",
			Examples: []string{"mu.Lock()", "rw.Lock()", "mu.Unlock()", "rw.Unlock()"},
			Transform: func(node ast.Node, info *types.Info) []ast.Node {
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) != 0 {
					return nil
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return nil
				}
				method := sel.Sel.Name
				if method != "Lock" && method != "Unlock" &&
					method != "RLock" && method != "RUnlock" {
					return nil
				}

				if info == nil {
					return nil
				}

				// Check if the receiver is a Mutex or RWMutex.
				recvType := info.TypeOf(sel.X)
				if recvType == nil {
					return nil
				}
				typeStr := recvType.String()

				isMutex := typeStr == "sync.Mutex" || typeStr == "*sync.Mutex"
				isRWMutex := typeStr == "sync.RWMutex" || typeStr == "*sync.RWMutex"

				if !isMutex && !isRWMutex {
					return nil
				}

				// Lock/Unlock on Mutex ↔ Lock/Unlock on RWMutex.
				// (RLock/RUnlock are RWMutex-only, no equivalent on Mutex.)
				if method == "Lock" || method == "Unlock" {
					// Both Mutex and RWMutex support Lock/Unlock.
					// The equivalence is that a pattern matching mu.Lock()
					// should also match rw.Lock() and vice versa.
					// We return the same call — the key point is that
					// the pattern matcher will accept both types.
					return []ast.Node{
						&ast.CallExpr{
							Fun: &ast.SelectorExpr{
								X:   sel.X,
								Sel: &ast.Ident{Name: method},
							},
						},
					}
				}

				return nil
			},
		},
	}
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

func isNilIdent(expr ast.Expr) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == "nil"
}

func isErrorType(t types.Type) bool {
	if t == nil {
		return false
	}
	// Check if the type implements the error interface.
	iface := types.Universe.Lookup("error").Type().Underlying().(*types.Interface)
	return types.Implements(t, iface) || types.Implements(types.NewPointer(t), iface)
}
