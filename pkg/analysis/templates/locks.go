// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package templates

import (
	"fmt"
	"go/ast"

	"github.com/trendvidia/caliper/pkg/analysis/cfg"
)

// LockViolation detects mutex discipline violations:
//   - Lock() without Unlock() on all paths
//   - Double Lock() (deadlock)
//   - Lock held across blocking operations
type LockViolation struct {
	CheckBlockingUnderLock bool
}

func (lv *LockViolation) Name() string { return "lock_violation" }

func (lv *LockViolation) Analyze(ctx *TemplateContext) []Finding {
	var findings []Finding

	findings = append(findings, lv.checkMissingUnlock(ctx)...)
	if lv.CheckBlockingUnderLock {
		findings = append(findings, lv.checkBlockingUnderLock(ctx)...)
	}

	return findings
}

// checkMissingUnlock finds Lock() calls where Unlock() is missing on some path.
func (lv *LockViolation) checkMissingUnlock(ctx *TemplateContext) []Finding {
	var findings []Finding

	for _, block := range ctx.CFG.Blocks {
		for _, node := range block.Nodes {
			mutexName, lockKind := extractLockCall(node)
			if mutexName == "" {
				continue
			}

			// Only process lock acquisitions, not unlocks.
			if lockKind != "Lock" && lockKind != "RLock" {
				continue
			}

			// Build predicate for matching unlock.
			unlockMethod := "Unlock"
			if lockKind == "RLock" {
				unlockMethod = "RUnlock"
			}

			unlockPred := func(_ *cfg.Block, n ast.Node) bool {
				name, kind := extractLockCall(n)
				return name == mutexName && kind == unlockMethod
			}

			// Check AF(unlock) — unlock must happen on ALL paths.
			violations := cfg.Reachable(ctx.CFG, block, unlockPred, cfg.AllPaths)
			if len(violations) > 0 {
				// Skip when the unlock obligation is transferred out
				// of the function — i.e., the method value
				// `mutex.Unlock` is returned, passed as an argument,
				// or assigned. The caller is responsible for calling
				// it, and analyzing that is out of scope here.
				if isLockTransferred(ctx.Func.Func.Body, mutexName, unlockMethod) {
					continue
				}
				findings = append(findings, Finding{
					Kind:     "lock_violation",
					Severity: "error",
					Message: fmt.Sprintf(
						"%s.%s() without matching %s() on %d path(s)",
						mutexName, lockKind,
						unlockMethod, len(violations),
					),
					Location:    ctx.Engine.LocationOf(node.Pos()),
					Explanation: formatPathExplanation(violations),
					Suggestion: fmt.Sprintf(
						"Add 'defer %s.%s()' immediately after %s(), or ensure %s() on all error paths",
						mutexName, unlockMethod, lockKind, unlockMethod,
					),
					Path: violations[0].Path,
				})
			}
		}
	}

	return findings
}

// checkBlockingUnderLock finds cases where a lock is held while performing
// potentially blocking operations (channel ops, I/O).
func (lv *LockViolation) checkBlockingUnderLock(ctx *TemplateContext) []Finding {
	var findings []Finding

	for _, block := range ctx.CFG.Blocks {
		for _, node := range block.Nodes {
			mutexName, lockKind := extractLockCall(node)
			if mutexName == "" {
				continue
			}

			// Only process lock acquisitions, not unlocks.
			if lockKind != "Lock" && lockKind != "RLock" {
				continue
			}

			unlockMethod := "Unlock"
			if lockKind == "RLock" {
				unlockMethod = "RUnlock"
			}

			// Check NeverBetween(lock, unlock, blocking_op).
			lockPred := func(_ *cfg.Block, n ast.Node) bool {
				name, kind := extractLockCall(n)
				return name == mutexName && kind == lockKind
			}
			unlockPred := func(_ *cfg.Block, n ast.Node) bool {
				name, kind := extractLockCall(n)
				return name == mutexName && kind == unlockMethod
			}
			blockingPred := func(_ *cfg.Block, n ast.Node) bool {
				return isBlockingOp(n)
			}

			violations := cfg.NeverBetween(ctx.CFG, lockPred, unlockPred, blockingPred)
			for _, v := range violations {
				findings = append(findings, Finding{
					Kind:     "blocking_under_lock",
					Severity: "warning",
					Message: fmt.Sprintf(
						"potentially blocking operation while %s.%s() is held",
						mutexName, lockKind,
					),
					Location:    ctx.Engine.LocationOf(node.Pos()),
					Explanation: "Holding a lock across a blocking operation can cause deadlocks or high contention.",
					Suggestion:  "Consider releasing the lock before the blocking operation and re-acquiring after.",
					Path:        v.Path,
				})
			}
		}
	}

	return findings
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// isLockTransferred reports whether `mutexName.methodName` appears in the
// function body as a method-value reference (not a direct call) — i.e.,
// returned, passed as an argument, or assigned. That pattern delegates
// the unlock obligation to the caller, so the in-function "missing
// unlock" check should not fire.
func isLockTransferred(body *ast.BlockStmt, mutexName, methodName string) bool {
	if body == nil {
		return false
	}

	// Pass 1: collect every SelectorExpr that's the Fun of a CallExpr —
	// these are calls, not method-value references.
	calledSelectors := make(map[*ast.SelectorExpr]bool)
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			calledSelectors[sel] = true
		}
		return true
	})

	// Pass 2: any mutex.method SelectorExpr not in calledSelectors is a
	// method-value reference.
	transferred := false
	ast.Inspect(body, func(n ast.Node) bool {
		if transferred {
			return false
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if calledSelectors[sel] {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if ident.Name == mutexName && sel.Sel.Name == methodName {
			transferred = true
		}
		return !transferred
	})
	return transferred
}

// extractLockCall returns (mutexName, methodName) if the node is a Lock/Unlock call.
func extractLockCall(node ast.Node) (string, string) {
	var call *ast.CallExpr

	switch n := node.(type) {
	case *ast.ExprStmt:
		c, ok := n.X.(*ast.CallExpr)
		if !ok {
			return "", ""
		}
		call = c
	case *ast.DeferStmt:
		call = n.Call
	default:
		return "", ""
	}

	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", ""
	}

	method := sel.Sel.Name
	switch method {
	case "Lock", "Unlock", "RLock", "RUnlock":
		if ident, ok := sel.X.(*ast.Ident); ok {
			return ident.Name, method
		}
	}

	return "", ""
}

// isBlockingOp checks if a node is a potentially blocking operation.
func isBlockingOp(node ast.Node) bool {
	switch n := node.(type) {
	case *ast.SendStmt:
		return true // channel send
	case *ast.UnaryExpr:
		// <-ch channel receive
		if n.Op.String() == "<-" {
			return true
		}
	case *ast.ExprStmt:
		// Check for known blocking calls.
		if call, ok := n.X.(*ast.CallExpr); ok {
			return isBlockingCall(call)
		}
	}
	return false
}

func isBlockingCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}

	// Common blocking methods.
	switch sel.Sel.Name {
	case "Read", "Write", "Accept", "Dial", "Wait", "Recv", "Send":
		return true
	}
	return false
}
