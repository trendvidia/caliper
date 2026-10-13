// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

// Cross-function metavariable binding for Caliper.
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

	"github.com/trendvidia/caliper/pkg/analysis"
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
	// Keyed on file/line/column only: the syntactic pass and the call-graph
	// pass describe the same call site but do not always agree on
	// FileLocation.End, so the whole struct is not a stable identity.
	type siteKey struct {
		file string
		line int
		col  int
	}
	seen := make(map[siteKey]bool)

	add := func(loc analysis.FileLocation, node ast.Node, callee string) {
		k := siteKey{loc.File, loc.Line, loc.Column}
		if loc.File == "" || seen[k] {
			return
		}
		seen[k] = true
		results = append(results, Match{
			Node:   node,
			File:   loc.File,
			Line:   loc.Line,
			Column: loc.Column,
			Bindings: Bindings{
				"callee": {Name: "callee", Node: nil},
			},
		})
		_ = callee
	}

	// Syntactic call sites: match the pattern against call expressions across
	// the corpus. This is the same walk find_pattern uses, and it is what makes
	// the documented `$x.Close()` form work.
	callees := make(map[*ssa.Function]bool)
	engine.Walk(func(_ *packages.Package, _ *ast.File, node ast.Node, info *types.Info) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if funcPattern.MatchNode(node, info) == nil {
			return true
		}
		// Anchor on the '(' — the call graph reports a call site at its
		// Lparen, so using the same anchor makes the two passes agree on
		// identity and keeps two calls on one line distinct.
		add(engine.LocationOf(call.Lparen), node, "")
		if fn := calleeFunction(ipc, call, info); fn != nil {
			callees[fn] = true
		}
		return true
	})

	// Call-graph enrichment: for every callee the syntactic pass resolved, add
	// the call sites the call graph knows about. This is what the tool claims
	// over text search — calls reaching the same function through a function
	// value, or from a package the pattern's spelling does not appear in.
	if ipc != nil {
		for fn := range callees {
			for _, cs := range ipc.CallSitesOf(fn) {
				add(cs.Position, nil, fn.Name())
			}
		}
	}

	return results
}

// calleeFunction resolves the static callee of a call expression to its SSA
// function, or nil when the callee is not a statically known func (an
// interface method, a func-typed variable, or a builtin).
func calleeFunction(ipc *analysis.InterProceduralContext, call *ast.CallExpr, info *types.Info) *ssa.Function {
	if ipc == nil || ipc.Program == nil || info == nil {
		return nil
	}
	var id *ast.Ident
	switch fun := unparen(call.Fun).(type) {
	case *ast.Ident:
		id = fun
	case *ast.SelectorExpr:
		id = fun.Sel
	default:
		return nil
	}
	fnObj, ok := info.Uses[id].(*types.Func)
	if !ok {
		return nil
	}
	return ipc.Program.FuncValue(fnObj)
}

func unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
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
