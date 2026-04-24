package templates

import (
	"fmt"
	"go/ast"
	"go/token"

	"github.com/trendvidia/caliper/pkg/analysis/cfg"
)

// GoroutineLeak detects goroutines that may never terminate because
// they lack a termination mechanism such as a context cancellation,
// channel read, or for-range over a channel.
type GoroutineLeak struct{}

func (gl *GoroutineLeak) Name() string { return "goroutine_leak" }

func (gl *GoroutineLeak) Analyze(ctx *TemplateContext) []Finding {
	if ctx.Func.Func.Body == nil {
		return nil
	}

	var findings []Finding

	for _, block := range ctx.CFG.Blocks {
		for _, node := range block.Nodes {
			findings = append(findings, gl.checkNode(ctx, node)...)
		}
	}

	return findings
}

func (gl *GoroutineLeak) checkNode(ctx *TemplateContext, node ast.Node) []Finding {
	var findings []Finding

	ast.Inspect(node, func(n ast.Node) bool {
		goStmt, ok := n.(*ast.GoStmt)
		if !ok {
			return true
		}

		funcLit, ok := goStmt.Call.Fun.(*ast.FuncLit)
		if !ok {
			return true
		}

		if funcLit.Body == nil {
			return true
		}

		// Only flag goroutines that contain an infinite loop without a
		// termination mechanism. One-shot goroutines that do finite work
		// and return naturally are not leaks.
		if !hasInfiniteLoop(funcLit.Body) {
			return true
		}

		// Check if the goroutine body has a termination mechanism.
		if hasTerminationMechanism(funcLit.Body) {
			return true
		}

		// Build a CFG for the goroutine body to check for deeper patterns.
		syntheticDecl := &ast.FuncDecl{
			Name: ast.NewIdent("_goroutine"),
			Type: funcLit.Type,
			Body: funcLit.Body,
		}
		goroutineCFG := cfg.Build(ctx.Fset, syntheticDecl, ctx.Func.Info)

		if hasTerminationInCFG(goroutineCFG) {
			return true
		}

		// Also check if the goroutine receives a context.Context parameter
		// and report if the caller's context might not be cancellable.
		contextWarning := ""
		if receivesContext(funcLit) {
			contextWarning = "; goroutine receives context.Context but body does not use ctx.Done()"
		}

		pos := ctx.Fset.Position(goStmt.Pos())
		findings = append(findings, Finding{
			Kind:     "goroutine_leak",
			Severity: "warning",
			Message: fmt.Sprintf(
				"goroutine at %s:%d may never terminate: infinite loop without channel receive, context.Done(), or range-over-channel%s",
				pos.Filename, pos.Line, contextWarning,
			),
			Location:   ctx.Engine.LocationOf(goStmt.Pos()),
			Suggestion: "Add a termination mechanism: select on ctx.Done(), read from a done/quit channel, or use a for-range over a channel",
		})

		return true
	})

	return findings
}

// hasInfiniteLoop checks if a function body contains an infinite loop:
// `for { }`, `for true { }`, or `for ;; { }`. These are the patterns that
// indicate a goroutine is intended to run indefinitely and thus needs an
// explicit termination mechanism.
func hasInfiniteLoop(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		forStmt, ok := n.(*ast.ForStmt)
		if !ok {
			return true
		}
		// `for { }` — no init, condition, or post.
		if forStmt.Init == nil && forStmt.Cond == nil && forStmt.Post == nil {
			found = true
			return false
		}
		// `for true { }` — condition is the literal `true`.
		if forStmt.Cond != nil && forStmt.Init == nil && forStmt.Post == nil {
			if ident, ok := forStmt.Cond.(*ast.Ident); ok && ident.Name == "true" {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// hasTerminationMechanism walks the function body AST and checks for
// patterns that indicate the goroutine can terminate:
//   - <-ctx.Done() or <-someChan
//   - select with a ctx.Done() case
//   - for-range over a channel
func hasTerminationMechanism(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		switch node := n.(type) {
		case *ast.UnaryExpr:
			// <-ch channel receive
			if node.Op == token.ARROW {
				found = true
				return false
			}
		case *ast.SelectStmt:
			// select with ctx.Done() case
			if selectHasContextDone(node) {
				found = true
				return false
			}
		case *ast.RangeStmt:
			// for-range over a channel
			if isChannelRange(node) {
				found = true
				return false
			}
		case *ast.CommClause:
			// case <-ctx.Done(): or case <-ch:
			if node.Comm != nil {
				if isContextDoneRecv(node.Comm) || isChanRecv(node.Comm) {
					found = true
					return false
				}
			}
		}
		return true
	})
	return found
}

// hasTerminationInCFG checks the goroutine's CFG blocks for termination patterns.
func hasTerminationInCFG(g *cfg.Graph) bool {
	for _, block := range g.Blocks {
		for _, node := range block.Nodes {
			terminated := false
			ast.Inspect(node, func(n ast.Node) bool {
				if terminated {
					return false
				}
				switch expr := n.(type) {
				case *ast.UnaryExpr:
					if expr.Op == token.ARROW {
						terminated = true
						return false
					}
				case *ast.RangeStmt:
					if isChannelRange(expr) {
						terminated = true
						return false
					}
				}
				return true
			})
			if terminated {
				return true
			}
		}
	}
	return false
}

// selectHasContextDone checks if a select statement has a case <-ctx.Done().
func selectHasContextDone(sel *ast.SelectStmt) bool {
	for _, clause := range sel.Body.List {
		cc, ok := clause.(*ast.CommClause)
		if !ok || cc.Comm == nil {
			continue
		}
		if isContextDoneRecv(cc.Comm) {
			return true
		}
	}
	return false
}

// isContextDoneRecv checks if a statement is of the form <-ctx.Done().
func isContextDoneRecv(stmt ast.Stmt) bool {
	switch s := stmt.(type) {
	case *ast.ExprStmt:
		return isContextDoneExpr(s.X)
	case *ast.AssignStmt:
		if len(s.Rhs) == 1 {
			return isContextDoneExpr(s.Rhs[0])
		}
	}
	return false
}

// isContextDoneExpr checks if an expression is <-ctx.Done() or <-something.Done().
func isContextDoneExpr(expr ast.Expr) bool {
	unary, ok := expr.(*ast.UnaryExpr)
	if !ok || unary.Op != token.ARROW {
		return false
	}

	call, ok := unary.X.(*ast.CallExpr)
	if !ok {
		return false
	}

	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}

	return sel.Sel.Name == "Done"
}

// isChanRecv checks if a statement is a channel receive.
func isChanRecv(stmt ast.Stmt) bool {
	switch s := stmt.(type) {
	case *ast.ExprStmt:
		if unary, ok := s.X.(*ast.UnaryExpr); ok {
			return unary.Op == token.ARROW
		}
	case *ast.AssignStmt:
		if len(s.Rhs) == 1 {
			if unary, ok := s.Rhs[0].(*ast.UnaryExpr); ok {
				return unary.Op == token.ARROW
			}
		}
	}
	return false
}

// isChannelRange checks if a range statement iterates over a channel.
// Since we may not have type info for the goroutine's synthetic scope,
// we use a heuristic: the range has no Key/Value (channel range syntax)
// or only a Key (the received value).
func isChannelRange(r *ast.RangeStmt) bool {
	// In Go, `for v := range ch` has Key set but Value is nil for channels.
	// `for range ch` has both nil. Either pattern suggests a channel range.
	return r.Value == nil
}

// receivesContext checks if a FuncLit has a context.Context parameter.
func receivesContext(fn *ast.FuncLit) bool {
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

// isContextType checks if a type expression looks like context.Context.
func isContextType(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if ident, ok := sel.X.(*ast.Ident); ok {
		return ident.Name == "context" && sel.Sel.Name == "Context"
	}
	return false
}
