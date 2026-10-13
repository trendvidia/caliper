// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package templates

// Shared helpers for the whole-program, type-scoped templates
// (PairedCalls, StaleCapture): companion-type discovery, match
// collection over method bodies, and deterministic finding ordering.

import (
	"bytes"
	"go/ast"
	"go/printer"
	"go/types"
	"sort"

	"github.com/trendvidia/caliper/pkg/analysis"
	"github.com/trendvidia/caliper/pkg/analysis/patterns"
)

// methodMatch is one pattern match inside a method body, retaining the
// method context so bindings can be resolved against the right
// types.Info.
type methodMatch struct {
	fc       *analysis.FuncContext
	node     ast.Node
	bindings patterns.Bindings
}

// matchesInMethod collects all matches of pat within fc's body.
func matchesInMethod(fc *analysis.FuncContext, pat *patterns.Pattern) []methodMatch {
	if fc.Func.Body == nil {
		return nil
	}
	var out []methodMatch
	ast.Inspect(fc.Func.Body, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		if b := pat.MatchNode(n, fc.Info); b != nil {
			out = append(out, methodMatch{fc: fc, node: n, bindings: *b})
		}
		return true
	})
	return out
}

// constructedTypes returns the named types instantiated via composite
// literals in fc's body — e.g. the renderer type a widget's
// CreateRenderer returns. Deduped, in source order.
func constructedTypes(fc *analysis.FuncContext) []*types.TypeName {
	if fc.Func.Body == nil {
		return nil
	}
	var out []*types.TypeName
	seen := map[*types.TypeName]bool{}
	ast.Inspect(fc.Func.Body, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if tn := analysis.NamedTypeOf(fc.Info.TypeOf(cl)); tn != nil && !seen[tn] {
			seen[tn] = true
			out = append(out, tn)
		}
		return true
	})
	return out
}

// selectsFieldOf reports whether sel reads a struct field through a value
// of the named type tn (pointer or value receiver alike), returning the
// field object or nil.
func selectsFieldOf(sel *ast.SelectorExpr, info *types.Info, tn *types.TypeName) *types.Var {
	v, ok := info.Uses[sel.Sel].(*types.Var)
	if !ok || !v.IsField() {
		return nil
	}
	if analysis.NamedTypeOf(info.TypeOf(sel.X)) != tn {
		return nil
	}
	return v
}

// nodeText renders a node as source text for finding messages,
// truncated to keep output compact.
func nodeText(engine *analysis.Engine, n ast.Node) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, engine.Fset(), n); err != nil {
		return ""
	}
	s := buf.String()
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}

// sortFindings orders findings by location so whole-program templates
// (which iterate map-keyed method sets) produce deterministic output.
func sortFindings(fs []Finding) {
	sort.Slice(fs, func(i, j int) bool {
		a, b := fs[i].Location, fs[j].Location
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Column < b.Column
	})
}
