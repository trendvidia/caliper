// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

// Enclosing-context predicates for pattern matches.
//
// Some rules are about the *context* of a call site rather than a path
// between two patterns — e.g. "widget mutation is fine on the main
// goroutine but must be wrapped in ui.Do(...) when the enclosing context
// is a go statement or a time.AfterFunc callback". These predicates
// classify a match by its enclosing AST: inside a go statement, inside a
// defer, inside a function literal passed to a given call (callback), or
// inside the arguments of a given call (wrapper).
//
// Context is lexical: a match inside a named function that happens to be
// called from a go statement elsewhere is NOT "inside go". That keeps the
// predicate cheap and its answer stable regardless of call graph
// precision.
package patterns

import (
	"fmt"
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
)

// ContextKind classifies an enclosing-context predicate.
type ContextKind int

const (
	// CtxGo — the match is lexically inside a `go` statement (including
	// inside a function literal launched by one).
	CtxGo ContextKind = iota
	// CtxDefer — the match is lexically inside a `defer` statement.
	CtxDefer
	// CtxCallbackOf — the match is inside a function literal passed as an
	// argument to a call matching the spec's pattern (e.g. time.AfterFunc).
	CtxCallbackOf
	// CtxCall — the match is nested anywhere inside the arguments of a
	// call matching the spec's pattern (e.g. ui.Do(func() { ... })).
	CtxCall
)

// ContextSpec is one enclosing-context predicate, parsed from the string
// forms accepted by ParseContextSpec.
type ContextSpec struct {
	Kind ContextKind
	Pat  *Pattern // callee pattern for CtxCallbackOf / CtxCall; nil otherwise
	raw  string
}

// String returns the spec in its original string form.
func (cs *ContextSpec) String() string { return cs.raw }

// ParseContextSpec parses a context spec string:
//
//	"go"                     — inside a go statement
//	"defer"                  — inside a defer statement
//	"callback_of:<pattern>"  — inside a func literal passed to a matching call
//	"call:<pattern>"         — inside the arguments of a matching call
//
// The <pattern> half uses find_pattern syntax and may be either a full
// call pattern ("time.AfterFunc($d, $f)") or just a callee ("ui.Do"),
// which matches a call to that callee with any arguments.
func ParseContextSpec(s string) (*ContextSpec, error) {
	trimmed := strings.TrimSpace(s)
	switch trimmed {
	case "go":
		return &ContextSpec{Kind: CtxGo, raw: trimmed}, nil
	case "defer":
		return &ContextSpec{Kind: CtxDefer, raw: trimmed}, nil
	}

	kindStr, patStr, ok := strings.Cut(trimmed, ":")
	if !ok {
		return nil, fmt.Errorf("unknown context spec %q (want \"go\", \"defer\", \"callback_of:<pattern>\", or \"call:<pattern>\")", s)
	}
	var kind ContextKind
	switch strings.TrimSpace(kindStr) {
	case "callback_of":
		kind = CtxCallbackOf
	case "call":
		kind = CtxCall
	default:
		return nil, fmt.Errorf("unknown context kind %q in spec %q (want \"callback_of\" or \"call\")", kindStr, s)
	}

	patStr = strings.TrimSpace(patStr)
	if patStr == "" {
		return nil, fmt.Errorf("context spec %q: missing pattern after %q", s, kindStr+":")
	}
	pat, err := Compile(patStr)
	if err != nil {
		return nil, fmt.Errorf("context spec %q: %w", s, err)
	}
	return &ContextSpec{Kind: kind, Pat: pat, raw: trimmed}, nil
}

// ParseContextSpecs parses a list of spec strings, failing on the first
// invalid one.
func ParseContextSpecs(specs []string) ([]*ContextSpec, error) {
	out := make([]*ContextSpec, 0, len(specs))
	for _, s := range specs {
		cs, err := ParseContextSpec(s)
		if err != nil {
			return nil, err
		}
		out = append(out, cs)
	}
	return out, nil
}

// callMatches reports whether an enclosing call expression matches the
// spec's pattern. A call-shaped pattern ("time.AfterFunc($d, $f)") is
// matched against the whole call; a bare callee pattern ("ui.Do") is
// matched against the call's Fun so any argument list matches.
func (cs *ContextSpec) callMatches(call *ast.CallExpr, info *types.Info) bool {
	if cs.Pat == nil {
		return false
	}
	if _, ok := cs.Pat.node.(*ast.CallExpr); ok {
		return cs.Pat.MatchNode(call, info) != nil
	}
	return cs.Pat.MatchNode(call.Fun, info) != nil
}

// satisfied reports whether the predicate holds for a match with the
// given ancestor chain. ancestors is ordered innermost-first and excludes
// the match node itself; node is the match.
func (cs *ContextSpec) satisfied(node ast.Node, ancestors []ast.Node, info *types.Info) bool {
	switch cs.Kind {
	case CtxGo:
		for _, n := range ancestors {
			if _, ok := n.(*ast.GoStmt); ok {
				return true
			}
		}
	case CtxDefer:
		for _, n := range ancestors {
			if _, ok := n.(*ast.DeferStmt); ok {
				return true
			}
		}
	case CtxCall:
		// The match must sit inside the *arguments* of a matching call,
		// not inside its Fun (a chained call's callee expression).
		for i, n := range ancestors {
			call, ok := n.(*ast.CallExpr)
			if !ok || !cs.callMatches(call, info) {
				continue
			}
			child := node
			if i > 0 {
				child = ancestors[i-1]
			}
			if nodeWithinArgs(call, child) {
				return true
			}
		}
	case CtxCallbackOf:
		// The match must sit inside a function literal that is directly
		// an argument of a matching call. Function values passed through
		// variables are not tracked (lexical semantics).
		for i, n := range ancestors {
			fl, ok := n.(*ast.FuncLit)
			if !ok || i+1 >= len(ancestors) {
				continue
			}
			call, ok := ancestors[i+1].(*ast.CallExpr)
			if !ok || !cs.callMatches(call, info) {
				continue
			}
			if nodeWithinArgs(call, fl) {
				return true
			}
		}
	}
	return false
}

// nodeWithinArgs reports whether child lies within one of call's
// argument expressions.
func nodeWithinArgs(call *ast.CallExpr, child ast.Node) bool {
	for _, arg := range call.Args {
		if arg.Pos() <= child.Pos() && child.End() <= arg.End() {
			return true
		}
	}
	return false
}

// ContextFilter restricts matches by their enclosing context. A match
// passes when it is inside at least one of the Inside specs (OR
// semantics; an empty list imposes no requirement) and inside none of
// the NotInside specs.
type ContextFilter struct {
	Inside    []*ContextSpec
	NotInside []*ContextSpec
}

// Empty reports whether the filter imposes no constraints.
func (cf *ContextFilter) Empty() bool {
	return cf == nil || (len(cf.Inside) == 0 && len(cf.NotInside) == 0)
}

// Allows reports whether the enclosing context of node within file
// passes the filter.
func (cf *ContextFilter) Allows(file *ast.File, node ast.Node, info *types.Info) bool {
	if cf.Empty() {
		return true
	}
	path, _ := astutil.PathEnclosingInterval(file, node.Pos(), node.End())
	ancestors := ancestorsOf(path, node)

	if len(cf.Inside) > 0 {
		ok := false
		for _, spec := range cf.Inside {
			if spec.satisfied(node, ancestors, info) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	for _, spec := range cf.NotInside {
		if spec.satisfied(node, ancestors, info) {
			return false
		}
	}
	return true
}

// ancestorsOf strips the match node (and any same-extent descendants
// that PathEnclosingInterval reports as innermost) from an enclosing
// path, returning strict ancestors ordered innermost-first.
func ancestorsOf(path []ast.Node, node ast.Node) []ast.Node {
	for i, n := range path {
		if n == node {
			return path[i+1:]
		}
	}
	if len(path) > 0 {
		return path[1:]
	}
	return nil
}
