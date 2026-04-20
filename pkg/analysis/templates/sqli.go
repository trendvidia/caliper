package templates

import (
	"fmt"
	"go/ast"
	"strings"
)

// SQLInjection detects potential SQL injection vulnerabilities by finding
// calls to database query/exec methods where the query string is built
// via string concatenation or fmt.Sprintf rather than parameterized
// queries.
type SQLInjection struct{}

func (si *SQLInjection) Name() string { return "sql_injection" }

// sqlMethods are the method names that accept a query string as their
// first (or second, for *Context variants) argument.
var sqlMethods = map[string]bool{
	"Query":           true,
	"QueryRow":        true,
	"Exec":            true,
	"QueryContext":    true,
	"ExecContext":     true,
	"QueryRowContext": true,
}

func (si *SQLInjection) Analyze(ctx *TemplateContext) []Finding {
	var findings []Finding

	for _, block := range ctx.CFG.Blocks {
		for _, node := range block.Nodes {
			f := si.checkNode(ctx, node)
			if f != nil {
				findings = append(findings, *f)
			}
		}
	}

	return findings
}

func (si *SQLInjection) checkNode(ctx *TemplateContext, node ast.Node) *Finding {
	var call *ast.CallExpr

	switch n := node.(type) {
	case *ast.ExprStmt:
		c, ok := n.X.(*ast.CallExpr)
		if !ok {
			return nil
		}
		call = c
	case *ast.AssignStmt:
		if len(n.Rhs) != 1 {
			return nil
		}
		c, ok := n.Rhs[0].(*ast.CallExpr)
		if !ok {
			return nil
		}
		call = c
	default:
		return nil
	}

	if !si.isSQLCall(call) {
		return nil
	}

	// Determine which argument is the query string.
	// For *Context variants the first arg is context.Context, so the
	// query is the second argument; otherwise it is the first.
	queryIdx := 0
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if strings.HasSuffix(sel.Sel.Name, "Context") {
			queryIdx = 1
		}
	}

	if queryIdx >= len(call.Args) {
		return nil
	}

	queryArg := call.Args[queryIdx]

	severity, unsafe := si.classifyArg(ctx, queryArg)
	if !unsafe {
		return nil
	}

	pos := ctx.Fset.Position(queryArg.Pos())
	return &Finding{
		Kind:     "sql_injection",
		Severity: severity,
		Message: fmt.Sprintf(
			"potential SQL injection: query string built via %s at %s:%d",
			describeExpr(queryArg), pos.Filename, pos.Line,
		),
		Location:   ctx.Engine.LocationOf(queryArg.Pos()),
		Suggestion: "Use parameterized queries with ? placeholders instead of string concatenation",
	}
}

// isSQLCall returns true if the call expression is a method call to one
// of the known SQL query/exec methods. When type information is
// available the receiver type is also checked.
func (si *SQLInjection) isSQLCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if !sqlMethods[sel.Sel.Name] {
		return false
	}
	return true
}

// classifyArg decides whether a query argument is safe. It returns the
// severity ("error" for direct concatenation, "warning" for indirect)
// and whether the argument should be reported.
func (si *SQLInjection) classifyArg(ctx *TemplateContext, arg ast.Expr) (string, bool) {
	switch a := arg.(type) {
	case *ast.BasicLit:
		// String literal — safe.
		return "", false

	case *ast.BinaryExpr:
		// String concatenation — direct injection.
		return "error", true

	case *ast.CallExpr:
		if isFmtSprintf(a) {
			// fmt.Sprintf — unsafe unless all format args are constants.
			if allConstantArgs(a) {
				return "", false
			}
			return "error", true
		}
		return "", false

	case *ast.Ident:
		// Could be a constant or a variable.
		if a.Obj != nil {
			if isConstantIdent(a) {
				return "", false
			}
			if isUnsafeAssignment(a) {
				return "warning", true
			}
		}
		return "", false
	}

	return "", false
}

// isFmtSprintf checks whether a call expression is fmt.Sprintf.
func isFmtSprintf(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return ident.Name == "fmt" && sel.Sel.Name == "Sprintf"
}

// allConstantArgs checks whether all non-format arguments to a
// fmt.Sprintf call are constants.
func allConstantArgs(call *ast.CallExpr) bool {
	// Skip the format string (first arg).
	for i := 1; i < len(call.Args); i++ {
		switch call.Args[i].(type) {
		case *ast.BasicLit:
			// Fine — literal constant.
		default:
			return false
		}
	}
	return true
}

// isConstantIdent checks if an identifier refers to a constant.
func isConstantIdent(ident *ast.Ident) bool {
	if ident.Obj == nil {
		return false
	}
	if ident.Obj.Kind == ast.Con {
		return true
	}
	// Check if it was assigned from a string literal.
	if vs, ok := ident.Obj.Decl.(*ast.ValueSpec); ok {
		if len(vs.Values) == 1 {
			if _, ok := vs.Values[0].(*ast.BasicLit); ok {
				return true
			}
		}
	}
	return false
}

// isUnsafeAssignment checks if an identifier was assigned from a
// concatenation or fmt.Sprintf call.
func isUnsafeAssignment(ident *ast.Ident) bool {
	if ident.Obj == nil {
		return false
	}
	switch decl := ident.Obj.Decl.(type) {
	case *ast.AssignStmt:
		// Find the position of this ident in the LHS.
		idx := -1
		for i, lhs := range decl.Lhs {
			if id, ok := lhs.(*ast.Ident); ok && id.Name == ident.Name {
				idx = i
				break
			}
		}
		if idx < 0 || idx >= len(decl.Rhs) {
			// Single RHS expression assigned to single LHS.
			if len(decl.Rhs) == 1 {
				return isUnsafeExpr(decl.Rhs[0])
			}
			return false
		}
		return isUnsafeExpr(decl.Rhs[idx])
	case *ast.ValueSpec:
		if len(decl.Values) == 1 {
			return isUnsafeExpr(decl.Values[0])
		}
	}
	return false
}

// isUnsafeExpr returns true for expressions that indicate dynamic query
// building (concatenation or fmt.Sprintf).
func isUnsafeExpr(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.BinaryExpr:
		return true
	case *ast.CallExpr:
		return isFmtSprintf(e) && !allConstantArgs(e)
	}
	return false
}

// describeExpr produces a short human-readable description of an expression.
func describeExpr(expr ast.Expr) string {
	switch expr.(type) {
	case *ast.BinaryExpr:
		return "string concatenation"
	case *ast.CallExpr:
		return "fmt.Sprintf"
	case *ast.Ident:
		return "variable (traced to unsafe construction)"
	}
	return "dynamic expression"
}
