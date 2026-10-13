// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package templates

import (
	"fmt"
	"go/ast"
)

// DeferInLoop detects defer statements inside for/range loops.
// A defer does not execute at the end of the loop iteration — it executes
// when the enclosing function returns. This causes resource accumulation:
// every iteration adds a deferred call, and none execute until the function
// exits.
//
// Common bugs this catches:
//   - defer f.Close() inside a loop opening files
//   - defer mu.Unlock() inside a loop acquiring locks
//   - defer rows.Close() inside a loop querying a database
type DeferInLoop struct{}

func (d *DeferInLoop) Name() string { return "defer_in_loop" }

func (d *DeferInLoop) Analyze(ctx *TemplateContext) []Finding {
	if ctx.Func.Func.Body == nil {
		return nil
	}

	var findings []Finding
	findDefersInLoops(ctx, ctx.Func.Func.Body, false, &findings)
	return findings
}

// findDefersInLoops walks the AST tracking whether we're inside a loop.
// When a defer is found inside a loop, it's reported.
func findDefersInLoops(ctx *TemplateContext, node ast.Node, inLoop bool, findings *[]Finding) {
	ast.Inspect(node, func(n ast.Node) bool {
		if n == nil {
			return false
		}

		switch stmt := n.(type) {
		case *ast.ForStmt, *ast.RangeStmt:
			// Descend into loop body with inLoop=true.
			var body *ast.BlockStmt
			switch s := stmt.(type) {
			case *ast.ForStmt:
				body = s.Body
			case *ast.RangeStmt:
				body = s.Body
			}
			if body != nil {
				findDefersInLoopBody(ctx, body, findings)
			}
			return false // we handle descent manually

		case *ast.FuncLit:
			// Function literals create a new defer scope — defers inside
			// a closure run when the closure returns, not the outer function.
			// So don't report defers inside closures even if the closure
			// is inside a loop.
			return false

		case *ast.DeferStmt:
			if inLoop {
				callDesc := deferCallDescription(stmt)
				*findings = append(*findings, Finding{
					Kind:     "defer_in_loop",
					Severity: "warning",
					Message: fmt.Sprintf(
						"defer %s inside loop — deferred call accumulates until function returns, not loop iteration",
						callDesc,
					),
					Location:   ctx.Engine.LocationOf(stmt.Pos()),
					Suggestion: "Move the deferred resource into a helper function, or call the cleanup explicitly at the end of each iteration",
				})
			}
		}

		return true
	})
}

// findDefersInLoopBody walks a loop body looking for defer statements,
// respecting function literal boundaries.
func findDefersInLoopBody(ctx *TemplateContext, body *ast.BlockStmt, findings *[]Finding) {
	findDefersInLoops(ctx, body, true, findings)
}

// deferCallDescription returns a short description of the deferred call.
func deferCallDescription(d *ast.DeferStmt) string {
	switch fun := d.Call.Fun.(type) {
	case *ast.SelectorExpr:
		if ident, ok := fun.X.(*ast.Ident); ok {
			return ident.Name + "." + fun.Sel.Name + "()"
		}
		return fun.Sel.Name + "()"
	case *ast.Ident:
		return fun.Name + "()"
	}
	return "func()"
}
