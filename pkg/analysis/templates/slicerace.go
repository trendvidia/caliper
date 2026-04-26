package templates

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
)

// SliceAppendRace detects slices shared across goroutines via append.
//
// When multiple goroutines append to the same slice, the backing array can
// be aliased: if one goroutine's append triggers a reallocation while another
// is writing, the result is data corruption or lost elements. Unlike maps,
// concurrent slice append does NOT panic — it silently corrupts data.
//
// Common patterns:
//   - var results []T; for _, v := range items { go func() { results = append(results, ...) }() }
//   - Slice declared outside goroutine, appended inside closure
type SliceAppendRace struct{}

func (sa *SliceAppendRace) Name() string { return "slice_append_race" }

func (sa *SliceAppendRace) Analyze(ctx *TemplateContext) []Finding {
	if ctx.Func.Func.Body == nil || ctx.Func.Info == nil {
		return nil
	}

	var findings []Finding

	ast.Inspect(ctx.Func.Func.Body, func(n ast.Node) bool {
		goStmt, ok := n.(*ast.GoStmt)
		if !ok {
			return true
		}

		funcLit, ok := goStmt.Call.Fun.(*ast.FuncLit)
		if !ok || funcLit.Body == nil {
			return true
		}

		// Find append calls inside the goroutine closure.
		appendTargets := findAppendTargets(funcLit.Body)
		if len(appendTargets) == 0 {
			return true
		}

		// Check which append targets are free variables (defined in enclosing scope).
		params := closureParams(funcLit)
		localDecls := collectLocalDecls(funcLit.Body)

		for _, target := range appendTargets {
			if params[target.name] || localDecls[target.name] {
				continue
			}
			if !isDefinedInFunc(ctx, target.name) {
				continue
			}

			// Verify it's actually a slice type.
			if obj := lookupIdent(ctx, target.name); obj != nil {
				if !isSliceType(obj.Type()) {
					continue
				}
			}

			// Skip if the append is in a block protected by Lock()/
			// Unlock() bracketing (or Lock() + deferred Unlock()) in
			// the goroutine body. Same heuristic shape as race.go's
			// hasMutexProtection — a held lock around the access is
			// the user's signal that they know about the sharing.
			if appendInLockedBlock(funcLit.Body, target.pos) {
				continue
			}

			findings = append(findings, Finding{
				Kind:     "slice_append_race",
				Severity: "error",
				Message: fmt.Sprintf(
					"concurrent append to shared slice %q — append is not goroutine-safe and can silently corrupt data",
					target.name,
				),
				Location:   ctx.Engine.LocationOf(goStmt.Pos()),
				Suggestion: "Use a mutex to protect the append, or collect results via a channel",
			})
		}

		return true
	})

	return findings
}

type appendTarget struct {
	name string
	pos  token.Pos
}

// findAppendTargets finds all `x = append(x, ...)` statements in a block
// and returns the target variable names.
func findAppendTargets(body *ast.BlockStmt) []appendTarget {
	var targets []appendTarget
	ast.Inspect(body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}

		// Check RHS is append(...).
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		fnIdent, ok := call.Fun.(*ast.Ident)
		if !ok || fnIdent.Name != "append" {
			return true
		}

		// Check LHS is an identifier.
		lhsIdent, ok := assign.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}

		targets = append(targets, appendTarget{
			name: lhsIdent.Name,
			pos:  assign.Pos(),
		})
		return true
	})
	return targets
}

func isSliceType(t types.Type) bool {
	if t == nil {
		return false
	}
	_, ok := t.Underlying().(*types.Slice)
	return ok
}

// appendInLockedBlock reports whether the append at pos is in a block
// where a Lock() (or RLock()) call lexically precedes it AND either an
// Unlock() (or RUnlock()) call follows it in the same block, or a
// deferred Unlock has been scheduled in the closure body. This is the
// same heuristic shape as race.go's hasMutexProtection — a held lock at
// the access point is treated as the user's intentional protection.
//
// Misses: locks acquired in an outer block, locks released conditionally,
// or Lock-equivalent functions not named Lock/RLock. False negatives are
// preferable to false positives here — the existing call sites that
// depend on this kind of protection (e.g., caliper's own RunTemplate)
// follow the standard pattern this matches.
func appendInLockedBlock(body *ast.BlockStmt, pos token.Pos) bool {
	block := findInnermostBlock(body, pos)
	if block == nil {
		return false
	}

	lockBefore := false
	for _, stmt := range block.List {
		if stmt.End() < pos && stmtHasLockCall(stmt) {
			lockBefore = true
			break
		}
	}
	if !lockBefore {
		return false
	}

	for _, stmt := range block.List {
		if stmt.Pos() > pos && stmtHasUnlockCall(stmt) {
			return true
		}
	}

	deferredUnlock := false
	ast.Inspect(body, func(n ast.Node) bool {
		if deferredUnlock {
			return false
		}
		d, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		ast.Inspect(d, func(m ast.Node) bool {
			if deferredUnlock {
				return false
			}
			if _, kind := extractLockCall(m); kind == "Unlock" || kind == "RUnlock" {
				deferredUnlock = true
			}
			return !deferredUnlock
		})
		return !deferredUnlock
	})
	return deferredUnlock
}

// findInnermostBlock returns the deepest *ast.BlockStmt under root that
// lexically contains pos.
func findInnermostBlock(root ast.Node, pos token.Pos) *ast.BlockStmt {
	var found *ast.BlockStmt
	ast.Inspect(root, func(n ast.Node) bool {
		block, ok := n.(*ast.BlockStmt)
		if !ok {
			return true
		}
		if block.Pos() <= pos && pos <= block.End() {
			found = block
		}
		return true
	})
	return found
}
