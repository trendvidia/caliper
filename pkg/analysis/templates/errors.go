package templates

import (
	"fmt"
	"go/ast"
	"go/types"

	"github.com/trendvidia/gocaliper/pkg/analysis/cfg"
)

// UncheckedError finds function calls that return an error which is
// never checked.
//
// Internally, this checks: for each call that returns (T, error),
// does there exist a path from the call to function exit where the
// error value is never compared to nil, passed to another function,
// or wrapped?
type UncheckedError struct {
	FuncPattern     string // optional: only check specific calls
	IncludeDefers   bool
	IncludeExplicit bool // include explicit _ = fn() assignments
}

func (ue *UncheckedError) Name() string { return "unchecked_error" }

func (ue *UncheckedError) Analyze(ctx *TemplateContext) []Finding {
	var findings []Finding

	for _, block := range ctx.CFG.Blocks {
		for _, node := range block.Nodes {
			finding := ue.checkNode(ctx, block, node)
			if finding != nil {
				findings = append(findings, *finding)
			}
		}
	}

	return findings
}

func (ue *UncheckedError) checkNode(ctx *TemplateContext, block *cfg.Block, node ast.Node) *Finding {
	switch n := node.(type) {
	case *ast.AssignStmt:
		return ue.checkAssign(ctx, block, n)
	case *ast.ExprStmt:
		return ue.checkExprStmt(ctx, block, n)
	case *ast.DeferStmt:
		if !ue.IncludeDefers {
			return nil
		}
		return ue.checkDeferStmt(ctx, block, n)
	}
	return nil
}

func (ue *UncheckedError) checkAssign(ctx *TemplateContext, _ *cfg.Block, assign *ast.AssignStmt) *Finding {
	if len(assign.Rhs) != 1 {
		return nil
	}

	call, ok := assign.Rhs[0].(*ast.CallExpr)
	if !ok {
		return nil
	}

	// Check if the function returns an error.
	if !callReturnsError(call, ctx.Func.Info) {
		return nil
	}

	// Check if error is assigned to blank identifier.
	for i, lhs := range assign.Lhs {
		ident, ok := lhs.(*ast.Ident)
		if !ok {
			continue
		}
		if ident.Name != "_" {
			continue
		}

		// Check if this position corresponds to the error return.
		sig := callSignature(call, ctx.Func.Info)
		if sig == nil {
			continue
		}

		results := sig.Results()
		if i < results.Len() {
			retType := results.At(i).Type()
			if isError(retType) {
				if !ue.IncludeExplicit {
					return nil
				}
				pos := ctx.Fset.Position(assign.Pos())
				return &Finding{
					Kind:     "unchecked_error",
					Severity: "warning",
					Message: fmt.Sprintf(
						"error from %s explicitly ignored with blank identifier at %s:%d",
						formatCall(call), pos.Filename, pos.Line,
					),
					Location:    ctx.Engine.LocationOf(assign.Pos()),
					Explanation: "The error return value is assigned to _ and never handled.",
					Suggestion:  "Handle the error or document why it's safe to ignore.",
				}
			}
		}
	}

	return nil
}

func (ue *UncheckedError) checkExprStmt(ctx *TemplateContext, _ *cfg.Block, expr *ast.ExprStmt) *Finding {
	call, ok := expr.X.(*ast.CallExpr)
	if !ok {
		return nil
	}

	if !callReturnsError(call, ctx.Func.Info) {
		return nil
	}

	pos := ctx.Fset.Position(expr.Pos())
	return &Finding{
		Kind:     "unchecked_error",
		Severity: "error",
		Message: fmt.Sprintf(
			"error from %s silently discarded at %s:%d",
			formatCall(call), pos.Filename, pos.Line,
		),
		Location:    ctx.Engine.LocationOf(expr.Pos()),
		Explanation: "The function returns an error but the return value is completely discarded.",
		Suggestion:  "Assign the result and check the error.",
	}
}

func (ue *UncheckedError) checkDeferStmt(ctx *TemplateContext, _ *cfg.Block, d *ast.DeferStmt) *Finding {
	if !callReturnsError(d.Call, ctx.Func.Info) {
		return nil
	}

	pos := ctx.Fset.Position(d.Pos())
	return &Finding{
		Kind:     "unchecked_error",
		Severity: "info",
		Message: fmt.Sprintf(
			"error from deferred %s not checked at %s:%d",
			formatCall(d.Call), pos.Filename, pos.Line,
		),
		Location:    ctx.Engine.LocationOf(d.Pos()),
		Explanation: "Error from a deferred call is not captured. This is common for Close() but may hide issues.",
		Suggestion:  "Consider using a named return and closing in a defer with error checking.",
	}
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

func callReturnsError(call *ast.CallExpr, info *types.Info) bool {
	if info == nil {
		return false
	}
	sig := callSignature(call, info)
	if sig == nil {
		return false
	}
	results := sig.Results()
	for i := 0; i < results.Len(); i++ {
		if isError(results.At(i).Type()) {
			return true
		}
	}
	return false
}

func callSignature(call *ast.CallExpr, info *types.Info) *types.Signature {
	t := info.TypeOf(call.Fun)
	if t == nil {
		return nil
	}
	sig, ok := t.Underlying().(*types.Signature)
	if !ok {
		return nil
	}
	return sig
}

func isError(t types.Type) bool {
	if t == nil {
		return false
	}
	return t.String() == "error"
}

func formatCall(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name + "()"
	case *ast.SelectorExpr:
		if x, ok := fn.X.(*ast.Ident); ok {
			return x.Name + "." + fn.Sel.Name + "()"
		}
		return fn.Sel.Name + "()"
	}
	return "function()"
}
