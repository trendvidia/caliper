// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package mcp

import (
	"bytes"
	"go/ast"
	"go/printer"
	"go/token"
	"go/types"
	"strings"

	"github.com/trendvidia/caliper/pkg/analysis/cfg"
	"github.com/trendvidia/caliper/pkg/analysis/patterns"
)

// AST/CFG predicates and the AST-printing helper used by the various
// find_* and check_* handlers. Lifted out of server.go so handlers can
// stay focused on parameter parsing and result shaping.

// patternPredicate adapts a compiled pattern into a CFG NodePredicate.
func patternPredicate(pat *patterns.Pattern, info *types.Info) cfg.NodePredicate {
	return func(_ *cfg.Block, node ast.Node) bool {
		return pat.MatchNode(node, info) != nil
	}
}

// blockMatchesPattern reports whether any node in block satisfies pred.
// Walks each top-level Node with ast.Inspect so the predicate can fire
// on nested expressions.
func blockMatchesPattern(block *cfg.Block, pred cfg.NodePredicate) bool {
	for _, n := range block.Nodes {
		matched := false
		ast.Inspect(n, func(child ast.Node) bool {
			if child == nil || matched {
				return false
			}
			if pred(block, child) {
				matched = true
				return false
			}
			return true
		})
		if matched {
			return true
		}
	}
	return false
}

// matchesTypeName reports whether an AST type expression refers to a type
// whose qualified name is fullName or whose unqualified name is baseName.
// Handles selector (`pkg.T`), pointer (`*T`), and slice (`[]T`) shapes.
func matchesTypeName(expr ast.Expr, fullName, baseName string) bool {
	if expr == nil {
		return false
	}
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name == fullName || e.Name == baseName
	case *ast.SelectorExpr:
		if ident, ok := e.X.(*ast.Ident); ok {
			qualified := ident.Name + "." + e.Sel.Name
			return qualified == fullName || qualified == baseName
		}
	case *ast.StarExpr:
		return matchesTypeName(e.X, strings.TrimPrefix(fullName, "*"), baseName)
	case *ast.ArrayType:
		return matchesTypeName(e.Elt, fullName, baseName)
	}
	return false
}

// typeStringMatches reports whether a types.Type matches a type name
// string by stringification, with pointer-element fallback.
func typeStringMatches(t types.Type, fullName, baseName string) bool {
	if t == nil {
		return false
	}
	ts := t.String()
	if ts == fullName || ts == baseName {
		return true
	}
	if strings.HasSuffix(ts, "."+baseName) {
		return true
	}
	if ptr, ok := t.(*types.Pointer); ok {
		return typeStringMatches(ptr.Elem(), strings.TrimPrefix(fullName, "*"), baseName)
	}
	return false
}

// printNode renders an AST node as Go source code using the standard
// printer config.
func printNode(fset *token.FileSet, node ast.Node) string {
	var buf bytes.Buffer
	printer.Fprint(&buf, fset, node)
	return buf.String()
}
