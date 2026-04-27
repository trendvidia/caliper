package templates

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"github.com/trendvidia/caliper/pkg/analysis/cfg"
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

	if isInfallibleErrorReturn(call, ctx.Func.Info, ctx.Infallible) {
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
				return &Finding{
					Kind:     "unchecked_error",
					Severity: "warning",
					Message: fmt.Sprintf(
						"error from %s explicitly ignored with blank identifier",
						formatCall(call),
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

	if isInfallibleErrorReturn(call, ctx.Func.Info, ctx.Infallible) {
		return nil
	}

	// Cleanup-path suppression: a Close-family call is intentional
	// best-effort when any of these shapes hold:
	//   - the call's CFG block is reached only via the "err != nil"
	//     successor of some err-check branch (subsumes the simpler
	//     AST "inside if-err body" rule and handles deeper nesting,
	//     helper-block inlining, and goto-cleanup patterns)
	//   - inside a function whose name suggests cleanup
	//     (Close/Stop/Cancel/finalClose/cleanupX/etc.)
	//   - sandwiched between an err-bearing assignment and an err-check
	//     of the same variable: `_, err := X; r.Close(); if err != nil {...}`
	if isCleanupShapedCall(call) {
		if dominatedByErrTrueBranch(ctx.CFG, expr.Pos()) ||
			inCleanupFunction(ctx.Func.Func) ||
			bracketedByErrCheck(ctx.Func.Func.Body, expr.Pos()) {
			return nil
		}
	}

	return &Finding{
		Kind:     "unchecked_error",
		Severity: "error",
		Message: fmt.Sprintf(
			"error from %s silently discarded",
			formatCall(call),
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

	return &Finding{
		Kind:     "unchecked_error",
		Severity: "info",
		Message: fmt.Sprintf(
			"error from deferred %s not checked",
			formatCall(d.Call),
		),
		Location:    ctx.Engine.LocationOf(d.Pos()),
		Explanation: "Error from a deferred call is not captured. This is common for Close() but may hide issues.",
		Suggestion:  "Consider using a named return and closing in a defer with error checking.",
	}
}

// --------------------------------------------------------------------------
// Cleanup-path detection
// --------------------------------------------------------------------------

// cleanupMethodNames are the method names whose discarded error in an
// already-errored cleanup branch is conventional. The list is small on
// purpose — broader matches risk silencing real bugs.
var cleanupMethodNames = map[string]bool{
	"Close":       true,
	"closeBody":   true, // net/http internal
	"CloseSend":   true, // gRPC streams
	"Stop":        true,
	"Cancel":      true,
	"Discard":     true,
	"Abort":       true,
	"Rollback":    true, // sql.Tx — common in error paths
	"Reset":       true,
	"Cleanup":     true,
	"closeLocked": true, // crypto/tls internal
}

// isCleanupShapedCall reports whether the call's method name suggests
// it's a resource-cleanup operation. Used to gate the cleanup-path
// suppression so it only applies to known cleanup shapes — a Read/Write
// inside an error block still fires.
func isCleanupShapedCall(call *ast.CallExpr) bool {
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		return cleanupMethodNames[fn.Sel.Name]
	case *ast.Ident:
		// Bare-identifier call (intra-package). Typical cleanup helpers
		// would be method calls, but allow plain function names too.
		return cleanupMethodNames[fn.Name]
	}
	return false
}

// inCleanupFunction reports whether the enclosing function's name
// suggests it's doing resource cleanup. Uses substring matching against
// a small keyword set so non-canonical names like finalClose,
// doClose, cleanupResources, teardown all qualify.
//
// Risk of over-recognition is bounded because this gate only fires
// alongside isCleanupShapedCall — a Read/Write inside a finalClose
// still fires.
func inCleanupFunction(fn *ast.FuncDecl) bool {
	if fn == nil || fn.Name == nil {
		return false
	}
	name := strings.ToLower(fn.Name.Name)
	for _, kw := range cleanupFuncKeywords {
		if strings.Contains(name, kw) {
			return true
		}
	}
	return false
}

// cleanupFuncKeywords are case-insensitive substrings; if any appears
// in the enclosing function's name, the function is treated as a
// cleanup context for the purposes of suppressing Close-family error
// discards.
var cleanupFuncKeywords = []string{
	"close",
	"clean", // covers cleanup, cleaner, cleanResources, etc.
	"dispose",
	"finalize",
	"teardown",
	"shutdown",
	"destroy",
	"release",
}

// dominatedByErrTrueBranch reports whether the CFG block containing pos
// is reachable only via the "err != nil" successor of some err-check
// BlockBranch. Stronger than the AST `inside if-err body` check because
// it works at the control-flow level: a Close inside a helper block
// reachable only from an err-true edge counts, even if the AST nesting
// is non-obvious.
//
// Algorithm:
//
//  1. Locate the CFG block containing pos.
//  2. For each err-check BlockBranch B with two successors (then/else):
//     a. Confirm target is reachable from B.Succs[0] (then branch).
//     b. Confirm target is NOT reachable from B.Succs[1] (else/join).
//     c. Confirm B dominates target (every path from entry to target
//     goes through B).
//     If all three hold, target is err-true-dominated.
func dominatedByErrTrueBranch(g *cfg.Graph, pos token.Pos) bool {
	if g == nil {
		return false
	}
	target := findCFGBlock(g, pos)
	if target == nil {
		return false
	}

	for _, branch := range g.Blocks {
		if branch.Kind != cfg.BlockBranch || len(branch.Succs) < 2 {
			continue
		}
		if !blockHasErrCheckCond(branch) {
			continue
		}
		if !cfgReaches(branch.Succs[0], target) {
			continue
		}
		if cfgReaches(branch.Succs[1], target) {
			continue // both branches reach target → ambiguous, skip
		}
		if cfgDominates(g, branch, target) {
			return true
		}
	}
	return false
}

// findCFGBlock returns the CFG block whose Nodes lexically contain pos,
// or nil. Walks all blocks; for typical function-sized CFGs this is
// O(blocks * stmts-per-block) which is small.
func findCFGBlock(g *cfg.Graph, pos token.Pos) *cfg.Block {
	for _, blk := range g.Blocks {
		for _, n := range blk.Nodes {
			if n.Pos() <= pos && pos <= n.End() {
				return blk
			}
		}
		for _, s := range blk.Stmts {
			if s.Pos() <= pos && pos <= s.End() {
				return blk
			}
		}
	}
	return nil
}

// blockHasErrCheckCond reports whether branch's condition node is an
// err-shape comparison.
func blockHasErrCheckCond(branch *cfg.Block) bool {
	for _, n := range branch.Nodes {
		if expr, ok := n.(ast.Expr); ok && isErrCheckCond(expr) {
			return true
		}
	}
	return false
}

// cfgReaches reports whether target is reachable from start via Succs.
func cfgReaches(start, target *cfg.Block) bool {
	if start == nil || target == nil {
		return false
	}
	visited := map[int]bool{}
	queue := []*cfg.Block{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur.ID] {
			continue
		}
		visited[cur.ID] = true
		if cur == target {
			return true
		}
		queue = append(queue, cur.Succs...)
	}
	return false
}

// cfgDominates reports whether every path from g.Entry to target passes
// through dominator. BFS from entry skipping the dominator: if we can
// reach target without crossing dominator, dominance fails.
func cfgDominates(g *cfg.Graph, dominator, target *cfg.Block) bool {
	if dominator == target {
		return true
	}
	visited := map[int]bool{}
	queue := []*cfg.Block{g.Entry}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur.ID] {
			continue
		}
		visited[cur.ID] = true
		if cur == target {
			return false // reached target without going through dominator
		}
		if cur == dominator {
			continue // don't traverse past dominator
		}
		queue = append(queue, cur.Succs...)
	}
	return true
}

// bracketedByErrCheck reports whether the call at pos is sandwiched
// between an err-bearing assignment and a same-variable err-check in
// the same block:
//
//	names, err := f.Readdirnames(-1)
//	f.Close()                 // ← suppressed
//	if err != nil { return err }
//
// Both the preceding statement and the following statement must be
// present and shaped as expected; otherwise no suppression.
func bracketedByErrCheck(funcBody *ast.BlockStmt, pos token.Pos) bool {
	block := findInnermostBlock(funcBody, pos)
	if block == nil {
		return false
	}
	idx := -1
	for i, stmt := range block.List {
		if stmt.Pos() <= pos && pos <= stmt.End() {
			idx = i
			break
		}
	}
	if idx <= 0 || idx >= len(block.List)-1 {
		return false
	}

	errName := errAssignedBy(block.List[idx-1])
	if errName == "" {
		return false
	}
	return ifChecksErrName(block.List[idx+1], errName)
}

// errAssignedBy returns the err-shaped LHS name of an assignment
// statement, or "" if the statement isn't an err-bearing assignment.
// Recognizes both `:=` and `=` forms.
func errAssignedBy(stmt ast.Stmt) string {
	assign, ok := stmt.(*ast.AssignStmt)
	if !ok {
		return ""
	}
	for _, lhs := range assign.Lhs {
		if id, ok := lhs.(*ast.Ident); ok && looksLikeErrIdent(id) {
			return id.Name
		}
	}
	return ""
}

// ifChecksErrName reports whether stmt is `if <name> != nil { ... }` or
// any condition recognized by isErrCheckCond that references the given
// err name.
func ifChecksErrName(stmt ast.Stmt, name string) bool {
	ifs, ok := stmt.(*ast.IfStmt)
	if !ok {
		return false
	}
	if !isErrCheckCond(ifs.Cond) {
		return false
	}
	// Confirm the condition references the same err name we saw.
	found := false
	ast.Inspect(ifs.Cond, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			found = true
			return false
		}
		return !found
	})
	return found
}

// isErrCheckCond reports whether expr is a recognized "this branch is
// the error-handling branch" condition.
func isErrCheckCond(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.BinaryExpr:
		// `<x> != nil` or `<x> == nil` — either can gate cleanup.
		if e.Op != token.NEQ && e.Op != token.EQL {
			return false
		}
		if isNilLiteral(e.X) && looksLikeErrIdent(e.Y) {
			return true
		}
		if isNilLiteral(e.Y) && looksLikeErrIdent(e.X) {
			return true
		}
	case *ast.CallExpr:
		// errors.Is(err, ...) / err.As(...) / fn(err) — anything that
		// references an error-named identifier in its args.
		for _, arg := range e.Args {
			if looksLikeErrIdent(arg) {
				return true
			}
		}
		// Also: `err.As(&dst)` style — sel.X is an err-named ident.
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok {
			if looksLikeErrIdent(sel.X) {
				return true
			}
		}
	}
	return false
}

func isNilLiteral(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}

// looksLikeErrIdent reports whether e is an identifier whose name
// suggests it holds an error value.
func looksLikeErrIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	if !ok {
		return false
	}
	name := id.Name
	if name == "err" || name == "e" {
		return true
	}
	return strings.HasSuffix(name, "Err") || strings.HasSuffix(name, "err")
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
