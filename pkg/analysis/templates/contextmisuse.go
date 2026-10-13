// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package templates

import (
	"go/ast"
	"strings"

	"github.com/trendvidia/caliper/pkg/analysis/cfg"
)

// ContextMisuse detects misuse of context.Context:
//   - Using context.Background() when a parent context is available
//   - Using context.TODO() in non-TODO situations (optional)
//
// These patterns often indicate that a function is ignoring cancellation
// signals from its caller, which can lead to goroutine leaks and
// unresponsive shutdown behavior.
type ContextMisuse struct {
	FlagTodo bool // also flag context.TODO() usage. Default: true
}

func (cm *ContextMisuse) Name() string { return "context_misuse" }

func (cm *ContextMisuse) Analyze(ctx *TemplateContext) []Finding {
	fn := ctx.Func.Func

	// Don't flag test functions.
	if fn.Name != nil && strings.HasPrefix(fn.Name.Name, "Test") {
		return nil
	}

	if fn.Body == nil {
		return nil
	}

	// Check whether this function has a context.Context parameter.
	hasCtxParam := hasContextParam(fn)

	var findings []Finding

	sourceNodes(ctx.CFG, func(_ *cfg.Block, node ast.Node) {
		if f := cm.checkNode(ctx, node, hasCtxParam); f != nil {
			findings = append(findings, *f)
		}
	})

	return findings
}

// hasContextParam checks whether the function has a context.Context parameter.
// It looks both at the type name (via type info if available) and at the
// syntactic name ("context.Context" or "ctx context.Context").
func hasContextParam(fn *ast.FuncDecl) bool {
	if fn.Type == nil || fn.Type.Params == nil {
		return false
	}

	for _, field := range fn.Type.Params.List {
		if isContextType(field.Type) {
			return true
		}
	}
	return false
}

func (cm *ContextMisuse) checkNode(ctx *TemplateContext, node ast.Node, hasCtxParam bool) *Finding {
	// We are looking for calls to context.Background() and context.TODO().
	var call *ast.CallExpr

	switch n := node.(type) {
	case *ast.ExprStmt:
		c, ok := n.X.(*ast.CallExpr)
		if !ok {
			return nil
		}
		call = c
	case *ast.AssignStmt:
		// Check all RHS expressions for context calls.
		for _, rhs := range n.Rhs {
			if f := cm.checkCallExpr(ctx, rhs, hasCtxParam); f != nil {
				return f
			}
		}
		return nil
	case *ast.CallExpr:
		call = n
	case *ast.DeferStmt:
		// A deferred cleanup runs when the function returns — including
		// because ctx was cancelled or timed out — so passing ctx to it
		// skips the cleanup exactly when it is needed most (pgx's
		// Rollback returns at once on a done context and leaves the
		// connection in a transaction). context.Background(), or a
		// WithTimeout off it, inside a defer is the correct idiom, not
		// a misuse, whether the call is deferred directly or inside a
		// deferred closure (#10). context.TODO() is still a placeholder
		// wherever it sits, so the walk keeps reporting that.
		return cm.checkNested(ctx, n, false)
	default:
		// Walk composite expressions — e.g., context.WithCancel(context.Background())
		return cm.checkNested(ctx, node, hasCtxParam)
	}

	if call == nil {
		return nil
	}

	return cm.checkCallExpr(ctx, call, hasCtxParam)
}

func (cm *ContextMisuse) checkCallExpr(ctx *TemplateContext, expr ast.Expr, hasCtxParam bool) *Finding {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return nil
	}

	// Check nested arguments first — e.g., context.WithCancel(context.Background()).
	for _, arg := range call.Args {
		if f := cm.checkCallExpr(ctx, arg, hasCtxParam); f != nil {
			return f
		}
	}

	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}

	ident, ok := sel.X.(*ast.Ident)
	if !ok {
		return nil
	}

	if ident.Name != "context" {
		return nil
	}

	switch sel.Sel.Name {
	case "Background":
		if !hasCtxParam {
			return nil
		}
		return &Finding{
			Kind:        "context_misuse",
			Severity:    "warning",
			Message:     "context.Background() used when parent context is available",
			Location:    ctx.Engine.LocationOf(call.Pos()),
			Explanation: "This function accepts a context.Context parameter but creates a new background context, ignoring cancellation signals from the caller.",
			Suggestion:  "Use the parent context parameter instead of context.Background()",
		}

	case "TODO":
		if !cm.FlagTodo {
			return nil
		}
		return &Finding{
			Kind:        "context_misuse",
			Severity:    "warning",
			Message:     "context.TODO() found",
			Location:    ctx.Engine.LocationOf(call.Pos()),
			Explanation: "context.TODO() is intended as a placeholder during development and should be replaced before production.",
			Suggestion:  "Replace context.TODO() with a proper context or context.Background()",
		}
	}

	return nil
}

// checkNested walks an arbitrary node looking for context.Background()
// or context.TODO() calls nested inside other expressions.
func (cm *ContextMisuse) checkNested(ctx *TemplateContext, node ast.Node, hasCtxParam bool) *Finding {
	var found *Finding
	ast.Inspect(node, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		if call, ok := n.(*ast.CallExpr); ok {
			f := cm.checkCallExpr(ctx, call, hasCtxParam)
			if f != nil {
				found = f
				return false
			}
		}
		return true
	})
	return found
}
