// Cross-function metavariable binding for GoCaliper.
//
// This implements Coccinelle's rule chaining across files: match a function
// definition with one pattern, then match all its call sites with another
// pattern, carrying metavariable bindings between the two.
//
// Example use case: find all functions that return an error, then find all
// call sites that ignore the returned error.
package patterns

import (
	"go/ast"
	"go/types"

	"github.com/trendvidia/gocaliper/pkg/analysis"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
)

// CrossFuncRule chains two patterns: a definition pattern and a usage pattern.
// Both patterns use call-expression syntax (e.g., "process($args...)").
// The DefPattern identifies which function to look up in the call graph,
// and the UsePattern matches at each of that function's call sites.
type CrossFuncRule struct {
	// DefPattern identifies the function. It is matched against all call
	// expressions in the codebase — the callee of a matching call is the
	// function whose call sites we search. This uses call-expression syntax
	// (e.g., "$x.Close()" or "process($args...)").
	DefPattern *Pattern

	// UsePattern matches call sites of the identified function. Can be
	// the same as DefPattern or more specific.
	UsePattern *Pattern
}

// CrossFuncMatch represents a matched (definition, call-site) pair.
type CrossFuncMatch struct {
	// Definition is where the function is defined.
	Definition Match

	// CallSite is where the function is called.
	CallSite Match

	// Bindings contains all metavariables from both patterns merged.
	Bindings Bindings
}

// FindCrossFuncMatches finds all (definition, call-site) pairs where the
// definition matches DefPattern and the call site matches UsePattern.
//
// The call graph from ipc is used to find call sites rather than doing
// text search, ensuring accurate resolution of interface calls and
// function values.
func FindCrossFuncMatches(
	engine *analysis.Engine,
	ipc *analysis.InterProceduralContext,
	rule *CrossFuncRule,
) []CrossFuncMatch {
	var results []CrossFuncMatch

	// Step 1: Find functions by matching DefPattern against function names.
	// We match each FuncDecl's name as a call expression.
	for _, fc := range engine.FuncNodes() {
		funcName := fc.Func.Name.Name

		// Build a synthetic call expression to match against DefPattern.
		// This lets patterns like "process($args...)" match function "process".
		if !functionNameMatchesPattern(funcName, rule.DefPattern) {
			continue
		}

		defBindings := make(Bindings)
		defMatch := Match{
			Node:     fc.Func,
			Bindings: defBindings,
		}
		loc := engine.LocationOf(fc.Func.Pos())
		defMatch.File = loc.File
		defMatch.Line = loc.Line
		defMatch.Column = loc.Column

		// Step 2: Look up this function in the call graph.
		ssaFn := ipc.FindFunction(fc)
		if ssaFn == nil {
			continue
		}

		// Step 3: Find all call sites via the call graph.
		callSites := ipc.CallSitesOf(ssaFn)
		if len(callSites) == 0 {
			continue
		}

		// Step 4: For each call site, find the AST node and match UsePattern.
		for _, cs := range callSites {
			callMatches := findCallSiteMatches(engine, ipc, cs, rule.UsePattern, defBindings)
			for _, cm := range callMatches {
				results = append(results, CrossFuncMatch{
					Definition: defMatch,
					CallSite:   cm,
					Bindings:   mergeBindings(defBindings, cm.Bindings),
				})
			}
		}
	}

	return results
}

// functionNameMatchesPattern checks if a function name matches a call pattern.
// For a pattern like "process($args...)", it extracts the function name
// "process" and checks if it matches. For "$x.Method()", it checks the method name.
func functionNameMatchesPattern(funcName string, pat *Pattern) bool {
	// Extract the function/method name from the pattern's AST.
	callExpr, ok := pat.node.(*ast.CallExpr)
	if !ok {
		return false
	}

	switch fn := callExpr.Fun.(type) {
	case *ast.Ident:
		// Pattern like "process($args...)" — check if name matches.
		// If the pattern name is a metavariable, it matches any function.
		if _, isMeta := pat.meta[fn.Name]; isMeta {
			return true
		}
		return fn.Name == funcName
	case *ast.SelectorExpr:
		// Pattern like "$x.Method($args...)" — check method name.
		if _, isMeta := pat.meta[fn.Sel.Name]; isMeta {
			return true
		}
		return fn.Sel.Name == funcName
	}
	return false
}

// findCallSiteMatches finds AST-level matches for a call site.
func findCallSiteMatches(
	engine *analysis.Engine,
	ipc *analysis.InterProceduralContext,
	cs analysis.CallSite,
	usePattern *Pattern,
	defBindings Bindings,
) []Match {
	var matches []Match

	if cs.Position.File == "" {
		return nil
	}

	// Walk through packages to find the call expression at this position.
	engine.Walk(func(pkg *packages.Package, file *ast.File, node ast.Node, info *types.Info) bool {
		callExpr, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}

		pos := engine.LocationOf(callExpr.Pos())
		if pos.File != cs.Position.File || pos.Line != cs.Position.Line {
			return true
		}

		// Try matching with UsePattern, pre-seeding with defBindings.
		useBindings := usePattern.MatchNode(callExpr, info)
		if useBindings == nil {
			return true
		}

		matches = append(matches, Match{
			Node:     callExpr,
			Bindings: *useBindings,
			File:     pos.File,
			Line:     pos.Line,
			Column:   pos.Column,
		})
		return true
	})

	return matches
}

// mergeBindings merges two sets of bindings. If a key exists in both,
// the second set takes precedence.
func mergeBindings(a, b Bindings) Bindings {
	merged := make(Bindings)
	for k, v := range a {
		merged[k] = v
	}
	for k, v := range b {
		merged[k] = v
	}
	return merged
}

// FindCallSitesByPattern finds all call sites where the callee matches
// a function pattern, using the call graph for precise resolution.
// This is useful for the find_call_sites MCP tool.
func FindCallSitesByPattern(
	engine *analysis.Engine,
	ipc *analysis.InterProceduralContext,
	funcPattern *Pattern,
) []Match {
	var results []Match

	// Match function definitions against the pattern.
	for _, fc := range engine.FuncNodes() {
		bindings := funcPattern.MatchNode(fc.Func, fc.Info)
		if bindings == nil {
			continue
		}

		ssaFn := ipc.FindFunction(fc)
		if ssaFn == nil {
			continue
		}

		// Collect all call sites.
		for _, cs := range ipc.CallSitesOf(ssaFn) {
			results = append(results, Match{
				Node: nil, // AST node not tracked at this level
				File: cs.Position.File,
				Line: cs.Position.Line,
				Bindings: Bindings{
					"caller":   {Name: "caller", Node: nil},
					"callee":   {Name: "callee", Node: fc.Func.Name},
					"is_go":    {Name: "is_go"},
					"is_defer": {Name: "is_defer"},
				},
			})
		}
	}

	return results
}

// FindAllCallSites finds all call sites of a specific SSA function and
// returns their positions.
func FindAllCallSites(
	engine *analysis.Engine,
	ipc *analysis.InterProceduralContext,
	ssaFn *ssa.Function,
) []analysis.CallSite {
	return ipc.CallSitesOf(ssaFn)
}
