// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package templates

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"

	"github.com/trendvidia/caliper/pkg/analysis"
	"github.com/trendvidia/caliper/pkg/analysis/cfg"
	"github.com/trendvidia/caliper/pkg/analysis/patterns"
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
			// Check if the guard variable (err/ok) is referenced in a condition.
			if condReferencesVar(node, guardName) {
				return true
			}
			// Also check if the value variable is compared to nil,
			// e.g., `if x != nil { x.Method() }` or `x != nil && x.Field`.
			if isNilCheckOnVar(node, valName) {
				return true
			}
			return false
		}

		derefPred := func(_ *cfg.Block, node ast.Node) bool {
			return nodeDereferencesVar(node, valName)
		}

		violations := checkGuardBeforeDeref(ctx.CFG, assignBlock, guardName, valName, guardPred, derefPred)

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

// condReferencesVar checks if a node is a condition that references the
// given variable name. Handles full IfStmt nodes, bare binary expressions,
// and unary expressions. Note: bare identifiers used as conditions (e.g.,
// `if ok { }`) are handled separately in branchBlockReferencesVar since they
// appear as top-level nodes in BlockBranch blocks and would cause false
// matches when found via ast.Inspect inside assignment statements.
func condReferencesVar(node ast.Node, varName string) bool {
	switch n := node.(type) {
	case *ast.IfStmt:
		return exprReferencesIdent(n.Cond, varName)
	case *ast.BinaryExpr:
		return exprReferencesIdent(n, varName)
	case *ast.UnaryExpr:
		return exprReferencesIdent(n, varName)
	}
	return false
}

// branchBlockReferencesVar checks if a BlockBranch block's condition
// references the guard variable. This handles the case where the condition
// is a bare identifier (e.g., `if ok { ... }`) which produces a branch
// block containing just *ast.Ident as its top-level node.
func branchBlockReferencesVar(block *cfg.Block, varName string) bool {
	if block.Kind != cfg.BlockBranch {
		return false
	}
	for _, node := range block.Nodes {
		if expr, ok := node.(ast.Expr); ok {
			if exprReferencesIdent(expr, varName) {
				return true
			}
		}
	}
	return false
}

// branchBlockHasNilCheck checks if a BlockBranch block's condition
// compares the given variable against nil. This handles patterns like
// `if x != nil { x.Method() }` where the value itself is the guard.
func branchBlockHasNilCheck(block *cfg.Block, varName string) bool {
	if block.Kind != cfg.BlockBranch {
		return false
	}
	for _, node := range block.Nodes {
		if isNilCheckOnVar(node, varName) {
			return true
		}
	}
	return false
}

// isNilCheckOnVar checks if a node is a comparison of varName against nil,
// including inside short-circuit && expressions like `x != nil && x.Field`.
func isNilCheckOnVar(node ast.Node, varName string) bool {
	binExpr, ok := node.(*ast.BinaryExpr)
	if !ok {
		return false
	}
	// x != nil, x == nil, nil != x, nil == x
	if binExpr.Op == token.NEQ || binExpr.Op == token.EQL {
		if isIdentNamed(binExpr.X, varName) && isNilLit(binExpr.Y) {
			return true
		}
		if isIdentNamed(binExpr.Y, varName) && isNilLit(binExpr.X) {
			return true
		}
	}
	// x != nil && ... (left side of short-circuit AND)
	if binExpr.Op == token.LAND {
		if isNilCheckOnVar(binExpr.X, varName) || isNilCheckOnVar(binExpr.Y, varName) {
			return true
		}
	}
	return false
}

func isIdentNamed(expr ast.Expr, name string) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == name
}

func isNilLit(expr ast.Expr) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == "nil"
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
func checkGuardBeforeDeref(g *cfg.Graph, start *cfg.Block, guardVarName, valVarName string, guardPred, derefPred cfg.NodePredicate) []cfg.PathResult {
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
		// Also check if this is a branch block whose condition directly
		// references the guard variable (handles bare `if ok { }` where
		// the CFG branch block contains just the *ast.Ident), or checks
		// the value variable against nil (e.g., `if x != nil { }`).
		if !guarded && (branchBlockReferencesVar(cur.block, guardVarName) || branchBlockHasNilCheck(cur.block, valVarName)) {
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
