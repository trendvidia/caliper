package templates

import (
	"fmt"
	"go/ast"
	"go/types"

	"github.com/trendvidia/gocaliper/pkg/analysis"
	"github.com/trendvidia/gocaliper/pkg/analysis/cfg"
	"github.com/trendvidia/gocaliper/pkg/analysis/patterns"
)

// NilDeref detects potential nil pointer dereferences by checking that
// guard values (error or ok bool) from two-value assignments are checked
// before the potentially-nil value is dereferenced.
//
// It catches:
//   - Using a pointer from (value, error) return without checking error first
//   - Using a value from (value, ok) map/typeassert without checking ok
//   - Dereferencing a pointer that was checked on one path but not another
type NilDeref struct {
	FuncPattern *patterns.Pattern // optional: restrict analysis to matching functions
}

func (nd *NilDeref) Name() string { return "nil_deref" }

func (nd *NilDeref) Analyze(ctx *TemplateContext) []Finding {
	if ctx.Func.Func.Body == nil {
		return nil
	}

	// If a function pattern is set, skip non-matching functions.
	if nd.FuncPattern != nil {
		if nd.FuncPattern.MatchNode(ctx.Func.Func, ctx.Func.Info) == nil {
			return nil
		}
	}

	var findings []Finding

	ast.Inspect(ctx.Func.Func.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 2 || len(assign.Rhs) != 1 {
			return true
		}

		valIdent, ok1 := assign.Lhs[0].(*ast.Ident)
		guardIdent, ok2 := assign.Lhs[1].(*ast.Ident)
		if !ok1 || !ok2 || valIdent.Name == "_" {
			return true
		}

		// Determine guard type: error or bool.
		var guardKind string
		if ctx.Func.Info != nil {
			if t := ctx.Func.Info.TypeOf(guardIdent); t != nil {
				if isErrorType(t) {
					guardKind = "error"
				} else if isBoolType(t) {
					guardKind = "bool"
				}
			}
		}
		if guardKind == "" {
			return true
		}

		// Check if value type is nilable (pointer, interface, slice, map, chan).
		if ctx.Func.Info != nil {
			if t := ctx.Func.Info.TypeOf(valIdent); t != nil {
				if !isNilableType(t) {
					return true
				}
			}
		}

		valName := valIdent.Name
		guardName := guardIdent.Name

		// Find the CFG block containing this assignment.
		assignBlock := findBlockContaining(ctx.CFG, assign)
		if assignBlock == nil {
			return true
		}

		guardPred := func(_ *cfg.Block, node ast.Node) bool {
			return condReferencesVar(node, guardName)
		}

		derefPred := func(_ *cfg.Block, node ast.Node) bool {
			return nodeDereferencesVar(node, valName)
		}

		violations := checkGuardBeforeDeref(ctx.CFG, assignBlock, guardPred, derefPred)

		for _, v := range violations {
			pos := ctx.Fset.Position(assign.Pos())

			derefLine := 0
			if v.RelevantNode != nil {
				derefLine = ctx.Fset.Position(v.RelevantNode.Pos()).Line
			}

			var msg string
			if guardKind == "error" {
				msg = fmt.Sprintf(
					"%s may be nil: %s not checked before dereference (line %d)",
					valName, guardName, derefLine,
				)
			} else {
				msg = fmt.Sprintf(
					"%s may be nil: %s not checked before use (line %d)",
					valName, guardName, derefLine,
				)
			}

			findings = append(findings, Finding{
				Kind:     "nil_deref",
				Severity: "error",
				Message:  msg,
				Location: analysis.FileLocation{
					File:   pos.Filename,
					Line:   pos.Line,
					Column: pos.Column,
				},
				Suggestion: fmt.Sprintf("Check %s before using %s", guardName, valName),
				Path:       v.Path,
			})
		}

		return true
	})

	return findings
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

func isErrorType(t types.Type) bool {
	iface := types.Universe.Lookup("error").Type().Underlying().(*types.Interface)
	return types.Implements(t, iface) ||
		types.Implements(types.NewPointer(t), iface)
}

func isBoolType(t types.Type) bool {
	b, ok := t.(*types.Basic)
	return ok && b.Kind() == types.Bool
}

func isNilableType(t types.Type) bool {
	switch t.Underlying().(type) {
	case *types.Pointer, *types.Interface, *types.Slice, *types.Map, *types.Chan, *types.Signature:
		return true
	}
	return false
}

// findBlockContaining finds the CFG block that contains the given AST node.
func findBlockContaining(g *cfg.Graph, target ast.Node) *cfg.Block {
	for _, block := range g.Blocks {
		for _, node := range block.Nodes {
			if node.Pos() == target.Pos() {
				return block
			}
		}
	}
	return nil
}

// condReferencesVar checks if a node is a condition (if statement) that
// references the given variable name.
func condReferencesVar(node ast.Node, varName string) bool {
	ifStmt, ok := node.(*ast.IfStmt)
	if !ok {
		return false
	}
	return exprReferencesIdent(ifStmt.Cond, varName)
}

func exprReferencesIdent(expr ast.Expr, name string) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		if ident, ok := n.(*ast.Ident); ok && ident.Name == name {
			found = true
			return false
		}
		return true
	})
	return found
}

// nodeDereferencesVar checks if a node dereferences (uses as receiver,
// pointer deref, or index) the given variable.
func nodeDereferencesVar(node ast.Node, varName string) bool {
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		if found {
			return false
		}
		switch expr := n.(type) {
		case *ast.SelectorExpr:
			if ident, ok := expr.X.(*ast.Ident); ok && ident.Name == varName {
				found = true
			}
		case *ast.StarExpr:
			if ident, ok := expr.X.(*ast.Ident); ok && ident.Name == varName {
				found = true
			}
		case *ast.IndexExpr:
			if ident, ok := expr.X.(*ast.Ident); ok && ident.Name == varName {
				found = true
			}
		}
		return !found
	})
	return found
}

// checkGuardBeforeDeref does a BFS from start, tracking whether the guard
// predicate was seen. Reports violations where derefPred is reached
// without the guard.
func checkGuardBeforeDeref(g *cfg.Graph, start *cfg.Block, guardPred, derefPred cfg.NodePredicate) []cfg.PathResult {
	var violations []cfg.PathResult

	type state struct {
		block   *cfg.Block
		path    []int
		guarded bool
	}

	type visitKey struct {
		id      int
		guarded bool
	}
	visited := make(map[visitKey]bool)
	queue := []state{{block: start, path: []int{start.ID}}}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		guarded := cur.guarded
		if !guarded && blockSatisfies(cur.block, guardPred) {
			guarded = true
		}

		key := visitKey{cur.block.ID, guarded}
		if visited[key] {
			continue
		}
		visited[key] = true

		// Check for dereference without guard.
		if !guarded && cur.block != start {
			if n := blockFindMatch(cur.block, derefPred); n != nil {
				violations = append(violations, cfg.PathResult{
					Path:          cur.path,
					Satisfied:     false,
					RelevantNode:  n,
					RelevantBlock: cur.block,
				})
				continue // don't explore past violation
			}
		}

		for _, succ := range cur.block.Succs {
			if containsID(cur.path, succ.ID) || len(cur.path) >= 1000 {
				continue
			}
			queue = append(queue, state{
				block:   succ,
				path:    append(append([]int{}, cur.path...), succ.ID),
				guarded: guarded,
			})
		}
	}

	return violations
}

func blockSatisfies(block *cfg.Block, pred cfg.NodePredicate) bool {
	for _, n := range block.Nodes {
		matched := false
		ast.Inspect(n, func(child ast.Node) bool {
			if child == nil || matched {
				return false
			}
			if pred(block, child) {
				matched = true
				return false
			}
			return true
		})
		if matched {
			return true
		}
	}
	return false
}

func blockFindMatch(block *cfg.Block, pred cfg.NodePredicate) ast.Node {
	for _, n := range block.Nodes {
		var found ast.Node
		ast.Inspect(n, func(child ast.Node) bool {
			if child == nil || found != nil {
				return false
			}
			if pred(block, child) {
				found = child
				return false
			}
			return true
		})
		if found != nil {
			return found
		}
	}
	return nil
}

func containsID(path []int, id int) bool {
	for _, p := range path {
		if p == id {
			return true
		}
	}
	return false
}
