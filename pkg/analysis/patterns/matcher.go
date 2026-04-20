// Package patterns implements AST pattern matching with metavariables
// for Go source code.
//
// Pattern syntax:
//
//	$x       — captures any expression/identifier as metavariable "x"
//	$_       — matches any expression without capturing
//	$x...    — matches zero or more arguments/elements (variadic)
//	...      — matches zero or more statements in a block
//
// Patterns are parsed as Go code fragments, with $ prefixed identifiers
// treated as metavariables.
package patterns

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
)

// Binding is a captured metavariable with its matched AST node and type.
type Binding struct {
	Name string
	Node ast.Node
	Type types.Type // may be nil if type info unavailable
}

// Bindings maps metavariable names to their matched values.
type Bindings map[string]*Binding

// Match represents a single match of a pattern in source code.
type Match struct {
	Node     ast.Node // the matched AST node
	Bindings Bindings // captured metavariables
	File     string   // file path
	Line     int
	Column   int
}

// TypeConstraint maps metavariable names to required type strings.
// e.g. {"x": "io.Reader", "err": "error"}
type TypeConstraint map[string]string

// EquivalenceExpander can expand a node into semantically equivalent forms.
// This is implemented by iso.Registry.
type EquivalenceExpander interface {
	Expand(node ast.Node, info *types.Info) []ast.Node
}

// Pattern is a compiled pattern ready for matching.
type Pattern struct {
	raw             string   // original pattern string
	node            ast.Node // parsed pattern AST
	meta            map[string]metaKind
	typeConstraints TypeConstraint
	equivalences    EquivalenceExpander // optional isomorphism expander
}

type metaKind int

const (
	metaSingle   metaKind = iota // $x — single match
	metaWildcard                 // $_ — discard
	metaVariadic                 // $x... — variadic
)

// Compile parses a pattern string into a Pattern.
func Compile(pattern string) (*Pattern, error) {
	// Replace metavariable syntax with valid Go identifiers for parsing.
	// $x      → __meta_x
	// $_      → __meta_wildcard
	// $x...   → __meta_variadic_x
	meta := make(map[string]metaKind)
	normalized := pattern

	// Handle variadic metavars first ($x...)
	for {
		idx := strings.Index(normalized, "$")
		if idx < 0 {
			break
		}

		end := idx + 1
		for end < len(normalized) && (normalized[end] == '_' || isAlphaNum(normalized[end])) {
			end++
		}

		name := normalized[idx+1 : end]
		if name == "" {
			return nil, fmt.Errorf("empty metavariable at position %d", idx)
		}

		var kind metaKind
		var replacement string

		if end+3 <= len(normalized) && normalized[end:end+3] == "..." {
			kind = metaVariadic
			replacement = "__meta_variadic_" + name
			meta[name] = kind
			normalized = normalized[:idx] + replacement + normalized[end+3:]
		} else if name == "_" {
			kind = metaWildcard
			replacement = "__meta_wildcard"
			meta["_"] = kind
			normalized = normalized[:idx] + replacement + normalized[end:]
		} else {
			kind = metaSingle
			replacement = "__meta_" + name
			meta[name] = kind
			normalized = normalized[:idx] + replacement + normalized[end:]
		}
	}

	// Replace standalone ... (statement wildcard) with a placeholder.
	// Must run after metavar processing to avoid confusing $x... with ...
	normalized = replaceStmtWildcard(normalized)

	// Parse the normalized pattern as a Go expression or statement.
	node, err := parseFragment(normalized)
	if err != nil {
		return nil, fmt.Errorf("parsing pattern %q: %w", pattern, err)
	}

	return &Pattern{
		raw:  pattern,
		node: node,
		meta: meta,
	}, nil
}

// WithTypeConstraints returns a copy of the pattern with type constraints set.
func (p *Pattern) WithTypeConstraints(tc TypeConstraint) *Pattern {
	cp := *p
	cp.typeConstraints = tc
	return &cp
}

// WithEquivalences returns a copy of the pattern that will try matching
// against all equivalent forms of target nodes during matching.
func (p *Pattern) WithEquivalences(eq EquivalenceExpander) *Pattern {
	cp := *p
	cp.equivalences = eq
	return &cp
}

// IsMultiStatement returns true if the pattern spans multiple statements.
func (p *Pattern) IsMultiStatement() bool {
	block, ok := p.node.(*ast.BlockStmt)
	return ok && len(block.List) > 1
}

// MatchNode checks if the pattern matches an AST node.
// Returns bindings if matched, nil if not.
func (p *Pattern) MatchNode(node ast.Node, info *types.Info) *Bindings {
	// For multi-statement patterns, try matching against consecutive
	// statements within a block.
	if p.IsMultiStatement() {
		block, ok := node.(*ast.BlockStmt)
		if !ok {
			return nil
		}
		return p.matchMultiStmt(block, info)
	}

	bindings := make(Bindings)
	if matchNodes(p.node, node, info, bindings) {
		return p.enforceTypeConstraints(bindings, info)
	}

	// If equivalences are enabled, try matching against equivalent forms
	// of the target node.
	if p.equivalences != nil && info != nil {
		for _, alt := range p.equivalences.Expand(node, info) {
			altBindings := make(Bindings)
			if matchNodes(p.node, alt, info, altBindings) {
				return p.enforceTypeConstraints(altBindings, info)
			}
		}
	}

	return nil
}

// MatchMultiStmtAt tries matching the multi-statement pattern at every
// position in a block and returns all matches.
func (p *Pattern) MatchMultiStmtAt(block *ast.BlockStmt, info *types.Info) []Bindings {
	if !p.IsMultiStatement() {
		return nil
	}
	patBlock := p.node.(*ast.BlockStmt)
	patLen := len(patBlock.List)
	var results []Bindings

	for i := 0; i <= len(block.List)-patLen; i++ {
		bindings := make(Bindings)
		matched := true
		for j, ps := range patBlock.List {
			if !matchNodes(ps, block.List[i+j], info, bindings) {
				matched = false
				break
			}
		}
		if matched {
			if b := p.enforceTypeConstraints(bindings, info); b != nil {
				results = append(results, *b)
			}
		}
	}
	return results
}

func (p *Pattern) matchMultiStmt(block *ast.BlockStmt, info *types.Info) *Bindings {
	matches := p.MatchMultiStmtAt(block, info)
	if len(matches) > 0 {
		return &matches[0]
	}
	return nil
}

func (p *Pattern) enforceTypeConstraints(bindings Bindings, info *types.Info) *Bindings {
	if p.typeConstraints != nil && info != nil {
		for varName, requiredType := range p.typeConstraints {
			b, ok := bindings[varName]
			if !ok {
				return nil
			}
			if b.Type == nil {
				return nil
			}
			if !typeMatches(b.Type, requiredType) {
				return nil
			}
		}
	}
	return &bindings
}

// String returns the original pattern string.
func (p *Pattern) String() string { return p.raw }

// Node returns the parsed AST node of the pattern.
func (p *Pattern) Node() ast.Node { return p.node }

// Meta returns the metavariable map.
func (p *Pattern) Meta() map[string]metaKind { return p.meta }

// MetaSingle is exported for use by substituteBindings.
const MetaSingle = metaSingle

// MetaWildcard is exported for use by substituteBindings.
const MetaWildcard = metaWildcard

// MetaVariadic is exported for use by substituteBindings.
const MetaVariadic = metaVariadic

// --------------------------------------------------------------------------
// Core matching logic
// --------------------------------------------------------------------------

func matchNodes(pattern, target ast.Node, info *types.Info, bindings Bindings) bool {
	if pattern == nil && target == nil {
		return true
	}
	if pattern == nil || target == nil {
		return false
	}

	// Check if pattern node is a metavariable reference.
	if ident, ok := pattern.(*ast.Ident); ok {
		if strings.HasPrefix(ident.Name, "__meta_") {
			return matchMetavar(ident.Name, target, info, bindings)
		}
	}

	// Match by node type.
	switch p := pattern.(type) {
	case *ast.Ident:
		t, ok := target.(*ast.Ident)
		if !ok {
			return false
		}
		return p.Name == t.Name

	case *ast.BasicLit:
		t, ok := target.(*ast.BasicLit)
		if !ok {
			return false
		}
		return p.Kind == t.Kind && p.Value == t.Value

	case *ast.CallExpr:
		t, ok := target.(*ast.CallExpr)
		if !ok {
			return false
		}
		if !matchNodes(p.Fun, t.Fun, info, bindings) {
			return false
		}
		return matchArgList(p.Args, t.Args, info, bindings)

	case *ast.SelectorExpr:
		t, ok := target.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		return matchNodes(p.X, t.X, info, bindings) &&
			matchNodes(p.Sel, t.Sel, info, bindings)

	case *ast.BinaryExpr:
		t, ok := target.(*ast.BinaryExpr)
		if !ok {
			return false
		}
		return p.Op == t.Op &&
			matchNodes(p.X, t.X, info, bindings) &&
			matchNodes(p.Y, t.Y, info, bindings)

	case *ast.UnaryExpr:
		t, ok := target.(*ast.UnaryExpr)
		if !ok {
			return false
		}
		return p.Op == t.Op && matchNodes(p.X, t.X, info, bindings)

	case *ast.AssignStmt:
		t, ok := target.(*ast.AssignStmt)
		if !ok {
			return false
		}
		if p.Tok != t.Tok {
			return false
		}
		return matchExprList(p.Lhs, t.Lhs, info, bindings) &&
			matchExprList(p.Rhs, t.Rhs, info, bindings)

	case *ast.ExprStmt:
		t, ok := target.(*ast.ExprStmt)
		if !ok {
			return false
		}
		return matchNodes(p.X, t.X, info, bindings)

	case *ast.ReturnStmt:
		t, ok := target.(*ast.ReturnStmt)
		if !ok {
			return false
		}
		return matchExprList(p.Results, t.Results, info, bindings)

	case *ast.IfStmt:
		t, ok := target.(*ast.IfStmt)
		if !ok {
			return false
		}
		if !matchNodes(p.Cond, t.Cond, info, bindings) {
			return false
		}
		// Match body if pattern has a body.
		if p.Body != nil && t.Body != nil {
			if !matchStmtList(p.Body.List, t.Body.List, info, bindings) {
				return false
			}
		}
		// Match else clause if pattern has one.
		if p.Else != nil {
			if t.Else == nil {
				return false
			}
			if !matchNodes(p.Else, t.Else, info, bindings) {
				return false
			}
		}
		return true

	case *ast.BlockStmt:
		t, ok := target.(*ast.BlockStmt)
		if !ok {
			return false
		}
		return matchStmtList(p.List, t.List, info, bindings)

	case *ast.ParenExpr:
		t, ok := target.(*ast.ParenExpr)
		if !ok {
			return false
		}
		return matchNodes(p.X, t.X, info, bindings)

	case *ast.IndexExpr:
		t, ok := target.(*ast.IndexExpr)
		if !ok {
			return false
		}
		return matchNodes(p.X, t.X, info, bindings) &&
			matchNodes(p.Index, t.Index, info, bindings)

	case *ast.StarExpr:
		t, ok := target.(*ast.StarExpr)
		if !ok {
			return false
		}
		return matchNodes(p.X, t.X, info, bindings)

	case *ast.CompositeLit:
		t, ok := target.(*ast.CompositeLit)
		if !ok {
			return false
		}
		if !matchNodes(p.Type, t.Type, info, bindings) {
			return false
		}
		return matchExprList(p.Elts, t.Elts, info, bindings)
	}

	// Fallback: types don't match.
	return false
}

func matchMetavar(name string, target ast.Node, info *types.Info, bindings Bindings) bool {
	if name == "__meta_wildcard" {
		return true // $_ matches anything
	}

	// Strip prefix to get user-visible name.
	var varName string
	if strings.HasPrefix(name, "__meta_variadic_") {
		varName = strings.TrimPrefix(name, "__meta_variadic_")
	} else {
		varName = strings.TrimPrefix(name, "__meta_")
	}

	// Check if already bound — must match same structure.
	if existing, ok := bindings[varName]; ok {
		return nodesEqual(existing.Node, target)
	}

	// New binding.
	b := &Binding{
		Name: varName,
		Node: target,
	}

	// Resolve type if possible.
	if info != nil {
		if expr, ok := target.(ast.Expr); ok {
			b.Type = info.TypeOf(expr)
		}
	}

	bindings[varName] = b
	return true
}

func matchArgList(pattern, target []ast.Expr, info *types.Info, bindings Bindings) bool {
	// Check for variadic metavar in pattern.
	for i, p := range pattern {
		if ident, ok := p.(*ast.Ident); ok && strings.HasPrefix(ident.Name, "__meta_variadic_") {
			// Match everything before the variadic.
			if !matchExprList(pattern[:i], target[:min(i, len(target))], info, bindings) {
				return false
			}
			// Match everything after the variadic.
			remaining := pattern[i+1:]
			if len(remaining) == 0 {
				// Variadic consumes all remaining args.
				return i <= len(target)
			}
			// There are args after the variadic — match from the end.
			if len(target) < i+len(remaining) {
				return false
			}
			return matchExprList(remaining, target[len(target)-len(remaining):], info, bindings)
		}
	}

	// No variadic — exact length match.
	if len(pattern) != len(target) {
		return false
	}
	return matchExprList(pattern, target, info, bindings)
}

// matchStmtList matches a pattern statement list against a target statement list.
// Supports the ... wildcard which matches zero or more statements.
func matchStmtList(pattern, target []ast.Stmt, info *types.Info, bindings Bindings) bool {
	// Find ... wildcard positions.
	for i, ps := range pattern {
		if isStmtWildcard(ps) {
			// Match statements before the wildcard.
			before := pattern[:i]
			after := pattern[i+1:]

			if len(before) > len(target) {
				return false
			}
			// Match prefix.
			for j, bs := range before {
				if !matchNodes(bs, target[j], info, bindings) {
					return false
				}
			}
			// Match suffix (statements after the wildcard).
			if len(after) == 0 {
				return true // ... consumes all remaining
			}
			if len(target)-len(before) < len(after) {
				return false
			}
			for j, as := range after {
				targetIdx := len(target) - len(after) + j
				if !matchNodes(as, target[targetIdx], info, bindings) {
					return false
				}
			}
			return true
		}
	}

	// No wildcard — exact length match.
	if len(pattern) != len(target) {
		return false
	}
	for i := range pattern {
		if !matchNodes(pattern[i], target[i], info, bindings) {
			return false
		}
	}
	return true
}

func matchExprList(pattern, target []ast.Expr, info *types.Info, bindings Bindings) bool {
	if len(pattern) != len(target) {
		return false
	}
	for i := range pattern {
		if !matchNodes(pattern[i], target[i], info, bindings) {
			return false
		}
	}
	return true
}

// nodesEqual checks structural equality of two AST nodes.
func nodesEqual(a, b ast.Node) bool {
	// Simple structural comparison for consistency checking.
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}

	switch av := a.(type) {
	case *ast.Ident:
		bv, ok := b.(*ast.Ident)
		return ok && av.Name == bv.Name
	case *ast.BasicLit:
		bv, ok := b.(*ast.BasicLit)
		return ok && av.Kind == bv.Kind && av.Value == bv.Value
	case *ast.SelectorExpr:
		bv, ok := b.(*ast.SelectorExpr)
		return ok && nodesEqual(av.X, bv.X) && nodesEqual(av.Sel, bv.Sel)
	}

	// For complex nodes, fall back to position comparison.
	return a.Pos() == b.Pos() && a.End() == b.End()
}

// --------------------------------------------------------------------------
// Parsing helpers
// --------------------------------------------------------------------------

func parseFragment(src string) (ast.Node, error) {
	// Try as expression first.
	fset := token.NewFileSet()
	expr, err := parser.ParseExpr(src)
	if err == nil {
		return expr, nil
	}

	// Try as statement.
	wrapped := fmt.Sprintf("package p; func _() { %s }", src)
	f, err2 := parser.ParseFile(fset, "", wrapped, 0)
	if err2 != nil {
		return nil, fmt.Errorf("not a valid expression (%v) or statement (%v)", err, err2)
	}

	// Extract the statement from the wrapper function.
	fn := f.Decls[0].(*ast.FuncDecl)
	if len(fn.Body.List) == 1 {
		return fn.Body.List[0], nil
	}
	// Multiple statements — return the block.
	return fn.Body, nil
}

// replaceStmtWildcard replaces standalone ... (not preceded by identifier)
// with a parseable placeholder function call.
func replaceStmtWildcard(s string) string {
	const placeholder = "__meta_stmt_wildcard()"
	const ellipsis = "..."

	result := s
	for {
		idx := strings.Index(result, ellipsis)
		if idx < 0 {
			break
		}
		// Check it's not part of __meta_variadic_ (already processed).
		// Standalone ... should not be preceded by an alphanumeric char or _.
		if idx > 0 && (isAlphaNum(result[idx-1]) || result[idx-1] == '_') {
			// Part of a variadic metavar — skip past it.
			result = result[:idx] + "___SKIP___" + result[idx+3:]
			continue
		}
		result = result[:idx] + placeholder + result[idx+3:]
	}
	result = strings.ReplaceAll(result, "___SKIP___", "...")
	return result
}

// isStmtWildcard checks if a statement is our statement wildcard placeholder.
func isStmtWildcard(stmt ast.Stmt) bool {
	es, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	ident, ok := call.Fun.(*ast.Ident)
	return ok && ident.Name == "__meta_stmt_wildcard"
}

func isAlphaNum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// typeMatches checks whether a type satisfies a type constraint string.
// The constraint can be:
//   - A simple name: "error", "int", "string"
//   - A qualified name: "io.Reader", "context.Context"
//   - An interface check: types implementing the interface are accepted
func typeMatches(t types.Type, constraint string) bool {
	if t == nil {
		return false
	}

	// Get the string representation of the type.
	typeStr := t.String()

	// Direct string match (handles "int", "string", "error", "io.Reader", etc.)
	if typeStr == constraint {
		return true
	}

	// Check unqualified name: constraint "Reader" matches "io.Reader"
	if idx := strings.LastIndex(typeStr, "."); idx >= 0 {
		if typeStr[idx+1:] == constraint {
			return true
		}
	}

	// Check if constraint ends with the type's short name
	// e.g., constraint "io.Reader" matches type "*io.Reader" (pointer to interface)
	if strings.HasSuffix(typeStr, "."+constraint) || strings.HasSuffix(typeStr, "/"+constraint) {
		return true
	}

	// Handle pointer types: *T should match constraint "T"
	if ptr, ok := t.(*types.Pointer); ok {
		return typeMatches(ptr.Elem(), constraint)
	}

	// Handle named types: check if constraint matches the underlying type name
	if named, ok := t.(*types.Named); ok {
		obj := named.Obj()
		if obj.Name() == constraint {
			return true
		}
		// Check with package path: "pkg.Type"
		if obj.Pkg() != nil {
			qualified := obj.Pkg().Name() + "." + obj.Name()
			if qualified == constraint {
				return true
			}
		}
	}

	// Special case: "error" interface check
	if constraint == "error" {
		errorIface := types.Universe.Lookup("error").Type().Underlying().(*types.Interface)
		return types.Implements(t, errorIface) || types.Implements(types.NewPointer(t), errorIface)
	}

	return false
}
