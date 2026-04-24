package templates

import (
	"fmt"
	"go/ast"
	"strings"

	"github.com/trendvidia/caliper/pkg/analysis/cfg"
	"github.com/trendvidia/caliper/pkg/analysis/patterns"
)

// ResourceLeak detects resources that are acquired but not released on
// all paths to function exit.
//
// Internally, this compiles to: for each block where acquire_pattern
// matches, check AF(release_method) — i.e., on ALL paths from the
// acquisition to function exit, the release method must be called.
//
// The MCP tool hides this: the user just says "find os.Open without Close".
type ResourceLeak struct {
	AcquirePattern *patterns.Pattern
	ReleaseMethod  string // e.g., "Close", "Unlock"
	Severity       cfg.Quantifier
}

func NewResourceLeak(acquirePattern string, releaseMethod string, allPaths bool) (*ResourceLeak, error) {
	p, err := patterns.Compile(acquirePattern)
	if err != nil {
		return nil, fmt.Errorf("compiling acquire pattern: %w", err)
	}
	if releaseMethod == "" {
		releaseMethod = "Close"
	}
	q := cfg.AllPaths
	if !allPaths {
		q = cfg.AnyPath
	}
	return &ResourceLeak{
		AcquirePattern: p,
		ReleaseMethod:  releaseMethod,
		Severity:       q,
	}, nil
}

func (rl *ResourceLeak) Name() string { return "resource_leak" }

func (rl *ResourceLeak) Analyze(ctx *TemplateContext) []Finding {
	var findings []Finding

	// Step 1: Find all blocks where the acquire pattern matches.
	for _, block := range ctx.CFG.Blocks {
		for _, node := range block.Nodes {
			bindings := rl.AcquirePattern.MatchNode(node, ctx.Func.Info)
			if bindings == nil {
				continue
			}

			// Step 2: Determine the variable that holds the resource.
			resourceVar := findResourceVar(node, bindings)
			if resourceVar == "" {
				continue
			}

			// Step 3: Build a predicate that checks for $resource.Release().
			releasePred := rl.buildReleasePredicate(resourceVar)

			// Step 4: Check if release is reachable on all paths from here.
			// This is the CTL query: AF(release) from this block.
			violations := cfg.Reachable(ctx.CFG, block, releasePred, rl.Severity)

			if len(violations) > 0 {
				findings = append(findings, Finding{
					Kind:     "resource_leak",
					Severity: "warning",
					Message: fmt.Sprintf(
						"%s acquired but %s.%s() not called on %d path(s)",
						rl.AcquirePattern.String(),
						resourceVar, rl.ReleaseMethod, len(violations),
					),
					Location:    ctx.Engine.LocationOf(node.Pos()),
					Explanation: formatPathExplanation(violations),
					Suggestion:  fmt.Sprintf("Add 'defer %s.%s()' after error check", resourceVar, rl.ReleaseMethod),
					Path:        violations[0].Path,
				})
			}
		}
	}

	return findings
}

func (rl *ResourceLeak) buildReleasePredicate(resourceVar string) cfg.NodePredicate {
	return func(_ *cfg.Block, node ast.Node) bool {
		// Look for $resource.Release() call — either direct or deferred.
		var callExpr *ast.CallExpr
		switch n := node.(type) {
		case *ast.ExprStmt:
			c, ok := n.X.(*ast.CallExpr)
			if !ok {
				return false
			}
			callExpr = c
		case *ast.DeferStmt:
			callExpr = n.Call
		default:
			return false
		}
		sel, ok := callExpr.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		if sel.Sel.Name != rl.ReleaseMethod {
			return false
		}
		// Check receiver matches the resource variable.
		if ident, ok := sel.X.(*ast.Ident); ok {
			return ident.Name == resourceVar
		}
		return false
	}
}

// Also check for defer $resource.Release().
func (rl *ResourceLeak) buildDeferReleasePredicate(resourceVar string) cfg.NodePredicate {
	return func(_ *cfg.Block, node ast.Node) bool {
		deferStmt, ok := node.(*ast.DeferStmt)
		if !ok {
			return false
		}
		sel, ok := deferStmt.Call.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		if sel.Sel.Name != rl.ReleaseMethod {
			return false
		}
		if ident, ok := sel.X.(*ast.Ident); ok {
			return ident.Name == resourceVar
		}
		return false
	}
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// findResourceVar extracts the variable name holding the acquired resource.
// For "f, err := os.Open(path)", this returns "f".
// For "$x.Lock()", this returns the bound value of $x.
func findResourceVar(node ast.Node, bindings *patterns.Bindings) string {
	if bindings != nil {
		// Check if there's a metavar that looks like the receiver.
		for _, b := range *bindings {
			if ident, ok := b.Node.(*ast.Ident); ok {
				return ident.Name
			}
		}
	}

	// Try to extract from assignment context.
	if assign, ok := node.(*ast.AssignStmt); ok {
		if len(assign.Lhs) > 0 {
			if ident, ok := assign.Lhs[0].(*ast.Ident); ok {
				return ident.Name
			}
		}
	}

	return ""
}

func formatPathExplanation(violations []cfg.PathResult) string {
	var parts []string
	for i, v := range violations {
		if i >= 3 {
			parts = append(parts, fmt.Sprintf("... and %d more paths", len(violations)-3))
			break
		}
		parts = append(parts, fmt.Sprintf("Path %d: blocks %v → exits without release", i+1, v.Path))
	}
	return strings.Join(parts, "\n")
}
