package templates

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strconv"
	"strings"
)

// RaceCondition detects potential data races caused by goroutine closures
// that capture variables from the enclosing scope which are also accessed
// after the go statement.
type RaceCondition struct{}

func (rc *RaceCondition) Name() string { return "race_condition" }

func (rc *RaceCondition) Analyze(ctx *TemplateContext) []Finding {
	if ctx.Func.Func.Body == nil {
		return nil
	}

	var findings []Finding

	// Walk the function's AST looking for go statements.
	ast.Inspect(ctx.Func.Func.Body, func(n ast.Node) bool {
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

		findings = append(findings, rc.checkClosure(ctx, goStmt, funcLit)...)
		return true
	})

	return findings
}

func (rc *RaceCondition) checkClosure(ctx *TemplateContext, goStmt *ast.GoStmt, funcLit *ast.FuncLit) []Finding {
	var findings []Finding

	// Go 1.22 changed for-loop semantics so each iteration gets a fresh
	// copy of the loop variable. Before 1.22, `for i := ... { go func()
	// { use(i) }() }` was a classic race; from 1.22 on it's safe unless
	// the closure itself writes the variable. The check honors the
	// declared `go` version in go.mod (conservative when unknown).
	loopVarPerIter := false
	if ctx.Func.Pkg != nil && ctx.Func.Pkg.Module != nil {
		loopVarPerIter = goVersionAtLeast(ctx.Func.Pkg.Module.GoVersion, 22)
	}

	// Step 1: Find all parameters of the closure (these are not free variables).
	params := closureParams(funcLit)

	// Step 2: Find all identifiers used inside the closure body.
	closureIdents := collectIdents(funcLit.Body)

	// Step 3: Determine free variables — identifiers that reference the
	// enclosing scope, not the closure's own parameters or local declarations.
	localDecls := collectLocalDecls(funcLit.Body)
	freeVars := make(map[string]bool)
	for name := range closureIdents {
		if params[name] || localDecls[name] {
			continue
		}
		// Verify it's actually defined in the enclosing function scope.
		if isDefinedInFunc(ctx, name) {
			freeVars[name] = true
		}
	}

	if len(freeVars) == 0 {
		return nil
	}

	// Step 4: Find statements after the go statement in the enclosing function.
	// Also find the enclosing for loop (if any) to detect loop variable races.
	stmtsAfterGo := findStmtsAfterGoStmt(ctx.Func.Func.Body, goStmt)
	enclosingLoop := findEnclosingForLoop(ctx.Func.Func.Body, goStmt)

	// Step 5: For each free variable, check if it is also accessed after
	// the go statement, and if so, check for mutex protection.
	for varName := range freeVars {
		// Skip channel-type variables — channels are designed for concurrent use.
		if ctx.Func.Info != nil {
			if obj := lookupIdent(ctx, varName); obj != nil {
				if isChanType(obj.Type()) {
					continue
				}
			}
		}

		// Check if the variable is used after the go statement at the same scope level.
		usedAfter := identUsedInStmts(stmtsAfterGo, varName)

		// Also check if the variable is a loop variable that is mutated by the loop itself.
		// Under Go 1.22+ loop scoping, each iteration has its own copy, so
		// the loop's iteration update no longer races with the closure.
		if !usedAfter && enclosingLoop != nil && !loopVarPerIter {
			usedAfter = isLoopVariable(enclosingLoop, varName)
		}

		if !usedAfter {
			continue
		}

		// Skip if the variable is not written on either side (goroutine
		// closure or statements after go). Read-only captures are safe
		// regardless of type — but loop variables under pre-1.22 semantics
		// are always mutated by the loop, so don't skip those.
		isLoopVar := enclosingLoop != nil && isLoopVariable(enclosingLoop, varName)
		if !isLoopVar || loopVarPerIter {
			writtenAfterGo := identWrittenInStmts(stmtsAfterGo, varName)
			writtenInClosure := identWrittenInNode(funcLit.Body, varName)
			if !writtenAfterGo && !writtenInClosure {
				continue
			}
		}

		// Check if there is channel-based synchronization between the go
		// statement and the variable use (e.g., <-done or <-errc before read).
		if hasChannelSyncBefore(stmtsAfterGo, varName) {
			continue
		}

		// Check if there is a WaitGroup.Wait() before the variable use.
		if hasWaitGroupSync(stmtsAfterGo, varName) {
			continue
		}

		// Check if the variable is only accessed inside a sync.Once.Do() call
		// in the closure (safe single-execution pattern).
		if isAccessedInsideSyncOnce(funcLit.Body, varName) {
			continue
		}

		// Check if there is mutex protection around both accesses.
		if hasMutexProtection(ctx.Func.Func.Body, goStmt, varName) {
			continue
		}

		severity := "warning"
		typeDesc := ""

		// Special case: map types are never safe for concurrent access.
		if ctx.Func.Info != nil {
			if obj := lookupIdent(ctx, varName); obj != nil {
				t := obj.Type()
				if isMapType(t) {
					severity = "error"
					typeDesc = " (map type — never safe for concurrent access)"
				} else {
					typeDesc = fmt.Sprintf(" (type: %s)", t.String())
				}
			}
		}

		findings = append(findings, Finding{
			Kind:     "race_condition",
			Severity: severity,
			Message: fmt.Sprintf(
				"potential data race on variable %q%s: shared between goroutine and enclosing function",
				varName, typeDesc,
			),
			Location:   ctx.Engine.LocationOf(goStmt.Pos()),
			Suggestion: fmt.Sprintf("Pass %q as a parameter to the goroutine closure, or protect access with a sync.Mutex", varName),
		})
	}

	return findings
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// closureParams returns the set of parameter names for a function literal.
func closureParams(fn *ast.FuncLit) map[string]bool {
	params := make(map[string]bool)
	if fn.Type == nil || fn.Type.Params == nil {
		return params
	}
	for _, field := range fn.Type.Params.List {
		for _, name := range field.Names {
			params[name.Name] = true
		}
	}
	return params
}

// collectIdents collects all identifier names referenced in an AST subtree.
func collectIdents(node ast.Node) map[string]bool {
	idents := make(map[string]bool)
	ast.Inspect(node, func(n ast.Node) bool {
		if ident, ok := n.(*ast.Ident); ok {
			idents[ident.Name] = true
		}
		return true
	})
	return idents
}

// collectLocalDecls collects all variable names declared inside a block
// (short var declarations and var statements).
func collectLocalDecls(body *ast.BlockStmt) map[string]bool {
	decls := make(map[string]bool)
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			if node.Tok.String() == ":=" {
				for _, lhs := range node.Lhs {
					if ident, ok := lhs.(*ast.Ident); ok {
						decls[ident.Name] = true
					}
				}
			}
		case *ast.GenDecl:
			for _, spec := range node.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok {
					for _, name := range vs.Names {
						decls[name.Name] = true
					}
				}
			}
		}
		return true
	})
	return decls
}

// isDefinedInFunc checks if a variable name is defined in the enclosing function
// (parameters, named results, or local declarations).
func isDefinedInFunc(ctx *TemplateContext, name string) bool {
	fn := ctx.Func.Func

	// Check function parameters.
	if fn.Type != nil && fn.Type.Params != nil {
		for _, field := range fn.Type.Params.List {
			for _, n := range field.Names {
				if n.Name == name {
					return true
				}
			}
		}
	}

	// Check named results.
	if fn.Type != nil && fn.Type.Results != nil {
		for _, field := range fn.Type.Results.List {
			for _, n := range field.Names {
				if n.Name == name {
					return true
				}
			}
		}
	}

	// Check receiver.
	if fn.Recv != nil {
		for _, field := range fn.Recv.List {
			for _, n := range field.Names {
				if n.Name == name {
					return true
				}
			}
		}
	}

	// Check local declarations in the function body.
	if fn.Body != nil {
		found := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if found {
				return false
			}
			switch node := n.(type) {
			case *ast.FuncLit:
				// Don't descend into nested function literals.
				return false
			case *ast.AssignStmt:
				if node.Tok.String() == ":=" {
					for _, lhs := range node.Lhs {
						if ident, ok := lhs.(*ast.Ident); ok && ident.Name == name {
							found = true
							return false
						}
					}
				}
			case *ast.GenDecl:
				for _, spec := range node.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for _, n := range vs.Names {
							if n.Name == name {
								found = true
								return false
							}
						}
					}
				}
			case *ast.RangeStmt:
				if ident, ok := node.Key.(*ast.Ident); ok && ident.Name == name {
					found = true
					return false
				}
				if node.Value != nil {
					if ident, ok := node.Value.(*ast.Ident); ok && ident.Name == name {
						found = true
						return false
					}
				}
			}
			return true
		})
		if found {
			return true
		}
	}

	return false
}

// findStmtsAfterGoStmt walks the function body to find the go statement at
// any nesting depth and returns the remaining statements in that block.
func findStmtsAfterGoStmt(body *ast.BlockStmt, goStmt *ast.GoStmt) []ast.Stmt {
	var result []ast.Stmt
	findStmtsAfterInBlock(body.List, goStmt, &result)
	return result
}

func findStmtsAfterInBlock(stmts []ast.Stmt, target ast.Stmt, result *[]ast.Stmt) bool {
	for i, stmt := range stmts {
		if stmt.Pos() == target.Pos() {
			*result = stmts[i+1:]
			return true
		}
		// Search inside nested blocks.
		switch s := stmt.(type) {
		case *ast.BlockStmt:
			if findStmtsAfterInBlock(s.List, target, result) {
				return true
			}
		case *ast.IfStmt:
			if s.Body != nil && findStmtsAfterInBlock(s.Body.List, target, result) {
				return true
			}
			if s.Else != nil {
				if block, ok := s.Else.(*ast.BlockStmt); ok {
					if findStmtsAfterInBlock(block.List, target, result) {
						return true
					}
				}
			}
		case *ast.ForStmt:
			if s.Body != nil && findStmtsAfterInBlock(s.Body.List, target, result) {
				return true
			}
		case *ast.RangeStmt:
			if s.Body != nil && findStmtsAfterInBlock(s.Body.List, target, result) {
				return true
			}
		case *ast.SelectStmt:
			if s.Body != nil {
				for _, clause := range s.Body.List {
					if cc, ok := clause.(*ast.CommClause); ok {
						if findStmtsAfterInBlock(cc.Body, target, result) {
							return true
						}
					}
				}
			}
		case *ast.SwitchStmt:
			if s.Body != nil {
				for _, clause := range s.Body.List {
					if cc, ok := clause.(*ast.CaseClause); ok {
						if findStmtsAfterInBlock(cc.Body, target, result) {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

// findEnclosingForLoop returns the ForStmt or RangeStmt that directly
// encloses the go statement, or nil if the go statement is not in a loop.
func findEnclosingForLoop(body *ast.BlockStmt, goStmt *ast.GoStmt) ast.Stmt {
	var enclosing ast.Stmt
	ast.Inspect(body, func(n ast.Node) bool {
		if enclosing != nil {
			return false
		}
		switch s := n.(type) {
		case *ast.ForStmt:
			if containsPos(s, goStmt.Pos()) {
				enclosing = s
			}
		case *ast.RangeStmt:
			if containsPos(s, goStmt.Pos()) {
				enclosing = s
			}
		}
		return enclosing == nil
	})
	return enclosing
}

// containsPos checks if a node's range contains a position.
func containsPos(node ast.Node, pos token.Pos) bool {
	return node.Pos() <= pos && pos <= node.End()
}

// isLoopVariable checks if a variable is the loop variable of a for statement
// (either the init variable or the range key/value).
func isLoopVariable(loop ast.Stmt, varName string) bool {
	switch s := loop.(type) {
	case *ast.ForStmt:
		// Check init statement (e.g., i := 0).
		if s.Init != nil {
			if assign, ok := s.Init.(*ast.AssignStmt); ok {
				for _, lhs := range assign.Lhs {
					if ident, ok := lhs.(*ast.Ident); ok && ident.Name == varName {
						return true
					}
				}
			}
		}
		// Check post statement (e.g., i++).
		if s.Post != nil {
			if identUsedInStmts([]ast.Stmt{s.Post}, varName) {
				return true
			}
		}
	case *ast.RangeStmt:
		if ident, ok := s.Key.(*ast.Ident); ok && ident.Name == varName {
			return true
		}
		if s.Value != nil {
			if ident, ok := s.Value.(*ast.Ident); ok && ident.Name == varName {
				return true
			}
		}
	}
	return false
}

// identUsedInStmts checks if a variable is referenced in any of the given statements.
func identUsedInStmts(stmts []ast.Stmt, name string) bool {
	for _, stmt := range stmts {
		found := false
		ast.Inspect(stmt, func(n ast.Node) bool {
			if found {
				return false
			}
			if ident, ok := n.(*ast.Ident); ok && ident.Name == name {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

// hasMutexProtection checks if accesses to varName are protected by a
// Lock/Unlock pair in both the goroutine and the enclosing function around
// the go statement.
func hasMutexProtection(body *ast.BlockStmt, goStmt *ast.GoStmt, varName string) bool {
	// Simple heuristic: check if there's a Lock() call before the go statement
	// and the variable access, and an Unlock() after, within the same scope.
	// This is a simplified check — full lock analysis is done by LockViolation.

	stmts := body.List
	goIdx := -1
	for i, stmt := range stmts {
		if stmt.Pos() == goStmt.Pos() {
			goIdx = i
			break
		}
	}
	if goIdx < 0 {
		return false
	}

	// Look for Lock() before the go statement.
	hasLockBefore := false
	for i := 0; i < goIdx; i++ {
		if stmtHasLockCall(stmts[i]) {
			hasLockBefore = true
		}
	}

	// Look for Unlock() after the go statement.
	hasUnlockAfter := false
	for i := goIdx + 1; i < len(stmts); i++ {
		if stmtHasUnlockCall(stmts[i]) {
			hasUnlockAfter = true
		}
	}

	return hasLockBefore && hasUnlockAfter
}

func stmtHasLockCall(stmt ast.Stmt) bool {
	found := false
	ast.Inspect(stmt, func(n ast.Node) bool {
		if found {
			return false
		}
		name, kind := extractLockCall(n)
		if name != "" && (kind == "Lock" || kind == "RLock") {
			found = true
		}
		return !found
	})
	return found
}

func stmtHasUnlockCall(stmt ast.Stmt) bool {
	found := false
	ast.Inspect(stmt, func(n ast.Node) bool {
		if found {
			return false
		}
		name, kind := extractLockCall(n)
		if name != "" && (kind == "Unlock" || kind == "RUnlock") {
			found = true
		}
		return !found
	})
	return found
}

// lookupIdent finds the types.Object for a variable name in the function scope.
func lookupIdent(ctx *TemplateContext, name string) types.Object {
	if ctx.Func.Info == nil || ctx.Func.Info.Defs == nil {
		return nil
	}

	// Search Defs for the variable.
	for ident, obj := range ctx.Func.Info.Defs {
		if ident.Name == name && obj != nil {
			if _, ok := obj.(*types.Var); ok {
				return obj
			}
		}
	}
	return nil
}

// isMapType checks if a type is a map type.
func isMapType(t types.Type) bool {
	if t == nil {
		return false
	}
	_, ok := t.Underlying().(*types.Map)
	return ok
}

// isChanType checks if a type is a channel type.
func isChanType(t types.Type) bool {
	if t == nil {
		return false
	}
	_, ok := t.Underlying().(*types.Chan)
	return ok
}

// identWrittenInNode checks if a variable is assigned to (written) in any
// part of an AST subtree. Covers direct assignment (x = ...), field
// assignment (x.field = ...), and index assignment (x[key] = ...).
func identWrittenInNode(node ast.Node, name string) bool {
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		if found {
			return false
		}
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range assign.Lhs {
			if lhsReferencesVar(lhs, name) {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// lhsReferencesVar checks if an LHS expression refers to a variable,
// including through selectors and index expressions (e.g., x.field, x[key]).
func lhsReferencesVar(expr ast.Expr, name string) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name == name
	case *ast.SelectorExpr:
		return lhsReferencesVar(e.X, name)
	case *ast.IndexExpr:
		return lhsReferencesVar(e.X, name)
	}
	return false
}

// identWrittenInStmts checks if a variable is assigned to (written) in any
// of the given statements.
func identWrittenInStmts(stmts []ast.Stmt, name string) bool {
	for _, stmt := range stmts {
		found := false
		ast.Inspect(stmt, func(n ast.Node) bool {
			if found {
				return false
			}
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range assign.Lhs {
				if ident, ok := lhs.(*ast.Ident); ok && ident.Name == name {
					found = true
					return false
				}
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

// hasChannelSyncBefore checks if there is a channel receive operation
// before the variable is used in the given statements. A channel receive
// acts as a synchronization point.
func hasChannelSyncBefore(stmts []ast.Stmt, varName string) bool {
	for _, stmt := range stmts {
		// Check if this statement is a channel receive (synchronization point).
		if stmtHasChanRecv(stmt) {
			return true
		}
		// If we encounter the variable use before any sync, not synchronized.
		if identUsedInStmts([]ast.Stmt{stmt}, varName) {
			return false
		}
	}
	return false
}

// hasWaitGroupSync checks if there is a .Wait() call before the variable
// is used in the given statements. WaitGroup.Wait() is a synchronization point.
func hasWaitGroupSync(stmts []ast.Stmt, varName string) bool {
	for _, stmt := range stmts {
		if stmtHasWaitCall(stmt) {
			return true
		}
		if identUsedInStmts([]ast.Stmt{stmt}, varName) {
			return false
		}
	}
	return false
}

// stmtHasWaitCall checks if a statement contains a .Wait() method call.
func stmtHasWaitCall(stmt ast.Stmt) bool {
	found := false
	ast.Inspect(stmt, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name == "Wait" {
			found = true
		}
		return !found
	})
	return found
}

// isAccessedInsideSyncOnce checks if all accesses to a variable inside a
// closure body are within a sync.Once.Do() call. This is a safe pattern
// for one-time initialization.
func isAccessedInsideSyncOnce(body *ast.BlockStmt, varName string) bool {
	// Check if any statement in the body is a .Do() call containing
	// the variable, and the variable is NOT used outside of .Do() calls.
	hasDoAccess := false
	hasNonDoAccess := false

	for _, stmt := range body.List {
		if isOnceDoStmt(stmt) {
			if identUsedInStmts([]ast.Stmt{stmt}, varName) {
				hasDoAccess = true
			}
		} else {
			if identUsedInStmts([]ast.Stmt{stmt}, varName) {
				hasNonDoAccess = true
			}
		}
	}

	return hasDoAccess && !hasNonDoAccess
}

// isOnceDoStmt checks if a statement is a call to something.Do(func() { ... }).
func isOnceDoStmt(stmt ast.Stmt) bool {
	exprStmt, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := exprStmt.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return sel.Sel.Name == "Do"
}

// goVersionAtLeast reports whether v ("1.22", "1.22.1", "go1.22") names a
// Go release whose minor version is >= the given threshold. Empty or
// unparseable inputs return false (conservative default).
func goVersionAtLeast(v string, minor int) bool {
	v = strings.TrimPrefix(v, "go")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 || parts[0] != "1" {
		return false
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	return n >= minor
}

// stmtHasChanRecv checks if a statement contains a channel receive operation.
func stmtHasChanRecv(stmt ast.Stmt) bool {
	found := false
	ast.Inspect(stmt, func(n ast.Node) bool {
		if found {
			return false
		}
		switch expr := n.(type) {
		case *ast.UnaryExpr:
			if expr.Op == token.ARROW {
				found = true
			}
		}
		return !found
	})
	return found
}
