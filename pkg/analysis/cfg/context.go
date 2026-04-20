package cfg

// This file implements context.Context flow tracking for Go functions.
// It identifies how context.Context values are created, derived, and whether
// cancellation functions are properly called on all paths.
//
// This is particularly useful for detecting patterns like:
//   - context.TODO() left in production code
//   - context.WithCancel() without calling cancel()
//   - Functions that create background contexts instead of accepting them

import (
	"go/ast"
	"go/types"
)

// ContextFlowResult describes the flow of context.Context through a function.
type ContextFlowResult struct {
	// HasParentContext is true if the function receives a context.Context parameter.
	HasParentContext bool

	// CreatesBackground is true if the function calls context.Background().
	CreatesBackground bool

	// CreatesTodo is true if the function calls context.TODO().
	CreatesTodo bool

	// DerivesContext is true if the function calls WithCancel/WithTimeout/WithValue.
	DerivesContext bool

	// CancellationPaths holds paths where cancel() is not called after
	// WithCancel/WithTimeout/WithDeadline. Each entry is a violation.
	CancellationPaths []PathResult
}

// TraceContextFlow analyzes how context.Context flows through a function.
//
// The function inspects:
//  1. Function parameters for context.Context type
//  2. All blocks for context.Background(), context.TODO() calls
//  3. All blocks for context.WithCancel/WithTimeout/WithDeadline calls
//  4. For each derivation that returns a cancel func, whether cancel() is
//     called on all paths to exit
//
// When type info (info) is nil, matching is done by name pattern only.
func TraceContextFlow(g *Graph, info *types.Info) *ContextFlowResult {
	result := &ContextFlowResult{}

	// Step 1: Check function parameters for context.Context.
	if g.Func != nil && g.Func.Type != nil && g.Func.Type.Params != nil {
		result.HasParentContext = hasContextParam(g.Func.Type.Params, info)
	}

	// Step 2 & 3: Walk all blocks looking for context calls.
	var cancelNames []string
	var cancelBlocks []*Block

	for _, blk := range g.Blocks {
		for _, node := range blk.Nodes {
			ast.Inspect(node, func(n ast.Node) bool {
				if n == nil {
					return false
				}

				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}

				name := contextCallName(call, info)
				switch name {
				case "Background":
					result.CreatesBackground = true
				case "TODO":
					result.CreatesTodo = true
				case "WithCancel", "WithTimeout", "WithDeadline":
					result.DerivesContext = true
					// Find the cancel variable name from the assignment.
					cancelName := findCancelVarName(blk, call)
					if cancelName != "" {
						cancelNames = append(cancelNames, cancelName)
						cancelBlocks = append(cancelBlocks, blk)
					}
				}
				return true
			})
		}
	}

	// Step 4: For each cancel function, check it's called on all paths to exit.
	for i, cancelName := range cancelNames {
		cancelPred := func(block *Block, node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return false
			}
			if ident, ok := call.Fun.(*ast.Ident); ok {
				return ident.Name == cancelName
			}
			return false
		}

		violations := Reachable(g, cancelBlocks[i], cancelPred, AllPaths)
		result.CancellationPaths = append(result.CancellationPaths, violations...)
	}

	return result
}

// --------------------------------------------------------------------------
// Internal helpers
// --------------------------------------------------------------------------

// hasContextParam checks if the parameter list contains a context.Context type.
func hasContextParam(params *ast.FieldList, info *types.Info) bool {
	for _, field := range params.List {
		if isContextType(field.Type, info) {
			return true
		}
	}
	return false
}

// isContextType checks if an expression refers to context.Context.
// With type info, it checks the actual type. Without, it matches by name.
func isContextType(expr ast.Expr, info *types.Info) bool {
	// Try type info first.
	if info != nil {
		if tv, ok := info.Types[expr]; ok {
			return tv.Type.String() == "context.Context"
		}
	}

	// Fall back to name matching.
	switch e := expr.(type) {
	case *ast.SelectorExpr:
		if ident, ok := e.X.(*ast.Ident); ok {
			return ident.Name == "context" && e.Sel.Name == "Context"
		}
	case *ast.Ident:
		// Handles aliased imports like `type Context = context.Context`.
		return e.Name == "Context"
	}
	return false
}

// contextCallName returns the function name if the call is a context package
// call (e.g., "Background", "TODO", "WithCancel"), or "" if not.
func contextCallName(call *ast.CallExpr, info *types.Info) string {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}

	// Check by name pattern: context.XXX.
	if ident, ok := sel.X.(*ast.Ident); ok {
		if ident.Name == "context" {
			name := sel.Sel.Name
			switch name {
			case "Background", "TODO", "WithCancel", "WithTimeout",
				"WithDeadline", "WithValue":
				return name
			}
		}
	}

	// If we have type info, also check the package path.
	if info != nil {
		if use, ok := info.Uses[sel.Sel]; ok {
			if pkg := use.Pkg(); pkg != nil && pkg.Path() == "context" {
				return sel.Sel.Name
			}
		}
	}

	return ""
}

// findCancelVarName finds the variable name assigned to the cancel function
// from a context.WithCancel/WithTimeout/WithDeadline call.
// These calls return (ctx, cancel), so we look for an assignment statement
// in the same block that assigns the second return value.
func findCancelVarName(blk *Block, call *ast.CallExpr) string {
	for _, stmt := range blk.Stmts {
		switch s := stmt.(type) {
		case *ast.AssignStmt:
			// Look for: ctx, cancel := context.WithCancel(...)
			// or: ctx, cancel = context.WithCancel(...)
			for _, rhs := range s.Rhs {
				if rhs == call && len(s.Lhs) >= 2 {
					if ident, ok := s.Lhs[1].(*ast.Ident); ok {
						return ident.Name
					}
				}
			}
		}
	}

	// Also search within ast.Node entries (for nested expressions).
	for _, node := range blk.Nodes {
		var cancelName string
		ast.Inspect(node, func(n ast.Node) bool {
			if cancelName != "" || n == nil {
				return false
			}
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, rhs := range assign.Rhs {
				if rhs == call && len(assign.Lhs) >= 2 {
					if ident, ok := assign.Lhs[1].(*ast.Ident); ok {
						cancelName = ident.Name
					}
				}
			}
			return true
		})
		if cancelName != "" {
			return cancelName
		}
	}

	return ""
}
