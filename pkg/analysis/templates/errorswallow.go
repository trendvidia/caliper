package templates

import (
	"fmt"
	"go/ast"
	"go/types"
)

// ErrorSwallow detects explicitly discarded errors. These are assignments
// where an error return value is assigned to the blank identifier or where
// a function returning an error is called as a bare statement (return
// value silently dropped).
//
// Common patterns it catches:
//   - _ = f()              explicit discard of single error return
//   - _, _ = f()           explicit discard of (value, error) return
//   - f, _ := os.Open(p)   discarded error in multi-return
//   - f.Close()            error return silently dropped (configurable)
type ErrorSwallow struct {
	// IncludeDropped reports calls where the error return value is silently
	// dropped by not assigning it at all (e.g., bare `f.Close()`).
	// Default: false (these are common and often intentional).
	IncludeDropped bool
}

func (es *ErrorSwallow) Name() string { return "error_swallow" }

func (es *ErrorSwallow) Analyze(ctx *TemplateContext) []Finding {
	if ctx.Func.Func.Body == nil || ctx.Func.Info == nil {
		return nil
	}

	var findings []Finding

	ast.Inspect(ctx.Func.Func.Body, func(n ast.Node) bool {
		if n == nil {
			return false
		}

		switch stmt := n.(type) {
		case *ast.AssignStmt:
			findings = append(findings, es.checkAssign(ctx, stmt)...)
		case *ast.ExprStmt:
			if es.IncludeDropped {
				findings = append(findings, es.checkDropped(ctx, stmt)...)
			}
		}
		return true
	})

	return findings
}

// checkAssign detects error values assigned to blank identifiers.
func (es *ErrorSwallow) checkAssign(ctx *TemplateContext, assign *ast.AssignStmt) []Finding {
	if len(assign.Rhs) != 1 {
		return nil
	}

	// Get the type of the RHS call expression.
	call, ok := assign.Rhs[0].(*ast.CallExpr)
	if !ok {
		return nil
	}

	// Check if the call returns an error.
	callType := ctx.Func.Info.TypeOf(call)
	if callType == nil {
		return nil
	}

	// For single-return: _ = f() where f returns error.
	if len(assign.Lhs) == 1 {
		if isBlankIdent(assign.Lhs[0]) && isErrorType(callType) {
			sev := "warning"
			if isCleanupCall(call) {
				sev = "info"
			}
			return []Finding{{
				Kind:       "error_swallow",
				Severity:   sev,
				Message:    fmt.Sprintf("error from %s explicitly discarded", callDescription(call)),
				Location:   ctx.Engine.LocationOf(assign.Pos()),
				Suggestion: "Handle the error or document why it is safe to ignore",
			}}
		}
		return nil
	}

	// For multi-return: find which positions are errors and check if they're blank.
	tuple, ok := callType.(*types.Tuple)
	if !ok {
		return nil
	}

	var findings []Finding
	for i := 0; i < tuple.Len() && i < len(assign.Lhs); i++ {
		if !isBlankIdent(assign.Lhs[i]) {
			continue
		}
		elemType := tuple.At(i).Type()
		if isErrorType(elemType) {
			// Determine severity based on whether the non-error value
			// is used on subsequent lines (nil deref risk).
			sev := "warning"
			for j, lhs := range assign.Lhs {
				if j == i || isBlankIdent(lhs) {
					continue
				}
				if ident, ok := lhs.(*ast.Ident); ok {
					if valueUsedAfterAssign(ctx, assign, ident.Name) {
						sev = "error"
						break
					}
				}
			}
			findings = append(findings, Finding{
				Kind:       "error_swallow",
				Severity:   sev,
				Message:    fmt.Sprintf("error from %s explicitly discarded", callDescription(call)),
				Location:   ctx.Engine.LocationOf(assign.Pos()),
				Suggestion: "Handle the error or document why it is safe to ignore",
			})
		}
	}
	return findings
}

// checkDropped detects calls where the error return value is silently dropped.
func (es *ErrorSwallow) checkDropped(ctx *TemplateContext, expr *ast.ExprStmt) []Finding {
	call, ok := expr.X.(*ast.CallExpr)
	if !ok {
		return nil
	}

	// Check if the call returns an error that's being dropped.
	callType := ctx.Func.Info.TypeOf(call)
	if callType == nil {
		return nil
	}

	hasError := false
	if isErrorType(callType) {
		hasError = true
	} else if tuple, ok := callType.(*types.Tuple); ok {
		for i := 0; i < tuple.Len(); i++ {
			if isErrorType(tuple.At(i).Type()) {
				hasError = true
				break
			}
		}
	}

	if !hasError {
		return nil
	}

	return []Finding{{
		Kind:       "error_swallow",
		Severity:   "info",
		Message:    fmt.Sprintf("error return from %s silently dropped", callDescription(call)),
		Location:   ctx.Engine.LocationOf(expr.Pos()),
		Suggestion: "Assign the return value and check the error, or use _ = f() to explicitly acknowledge the discard",
	}}
}

// valueUsedAfterAssign checks if a variable assigned in a multi-return
// is used in any subsequent statement within the same block. This indicates
// the discarded error creates a nil deref risk — the value may be nil/zero
// and is used without checking the error that would have signaled the failure.
func valueUsedAfterAssign(ctx *TemplateContext, assign *ast.AssignStmt, varName string) bool {
	body := ctx.Func.Func.Body
	if body == nil {
		return false
	}
	return anySubsequentStmtUsesVar(body.List, assign, varName)
}

func anySubsequentStmtUsesVar(stmts []ast.Stmt, target *ast.AssignStmt, varName string) bool {
	found := false
	for _, stmt := range stmts {
		if found {
			// We're past the target — check if this statement uses the variable.
			if stmtReferencesVar(stmt, varName) {
				return true
			}
			continue
		}
		if stmt.Pos() == target.Pos() {
			found = true
			continue
		}
		// Search nested blocks for the target.
		switch s := stmt.(type) {
		case *ast.BlockStmt:
			if anySubsequentStmtUsesVar(s.List, target, varName) {
				return true
			}
		case *ast.IfStmt:
			if s.Body != nil && anySubsequentStmtUsesVar(s.Body.List, target, varName) {
				return true
			}
		}
	}
	return false
}

func stmtReferencesVar(stmt ast.Stmt, varName string) bool {
	found := false
	ast.Inspect(stmt, func(n ast.Node) bool {
		if found {
			return false
		}
		if ident, ok := n.(*ast.Ident); ok && ident.Name == varName {
			found = true
		}
		return !found
	})
	return found
}

// isCleanupCall checks if a call is a known cleanup/best-effort operation
// where discarding the error is typically intentional.
func isCleanupCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	switch sel.Sel.Name {
	case "Close", "Remove", "RemoveAll", "Hostname":
		return true
	}
	return false
}

func isBlankIdent(expr ast.Expr) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == "_"
}

func callDescription(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		if ident, ok := fun.X.(*ast.Ident); ok {
			return ident.Name + "." + fun.Sel.Name + "()"
		}
		return fun.Sel.Name + "()"
	case *ast.Ident:
		return fun.Name + "()"
	}
	return "call"
}
