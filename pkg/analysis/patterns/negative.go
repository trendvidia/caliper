// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package patterns

import (
	"go/ast"
	"go/types"

	"github.com/trendvidia/caliper/pkg/analysis"
)

// NegativePattern wraps a positive pattern and checks for its absence
// within a given AST scope (typically a function body).
type NegativePattern struct {
	inner *Pattern
}

// NewNegative creates a negative pattern from a compiled pattern.
func NewNegative(p *Pattern) *NegativePattern {
	return &NegativePattern{inner: p}
}

// NegateInScope returns true if the inner pattern does NOT match
// anywhere within the given scope node (e.g., a *ast.FuncDecl.Body).
func (np *NegativePattern) NegateInScope(scope ast.Node, info *types.Info) bool {
	found := false
	ast.Inspect(scope, func(n ast.Node) bool {
		if n == nil || found {
			return !found
		}
		if np.inner.MatchNode(n, info) != nil {
			found = true
			return false
		}
		return true
	})
	return !found
}

// MatchAbsence returns all functions where the inner pattern is absent.
// Each returned Match has the function declaration as its Node and the
// function name in bindings under key "func".
func (np *NegativePattern) MatchAbsence(engine *analysis.Engine) []Match {
	var results []Match
	for _, fc := range engine.FuncNodes() {
		if fc.Func.Body == nil {
			continue
		}
		if np.NegateInScope(fc.Func.Body, fc.Info) {
			loc := engine.LocationOf(fc.Func.Pos())
			results = append(results, Match{
				Node: fc.Func,
				File: loc.File,
				Line: loc.Line,
				Bindings: Bindings{
					"func": {Name: "func", Node: fc.Func.Name},
				},
			})
		}
	}
	return results
}
