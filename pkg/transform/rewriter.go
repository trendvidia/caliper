// Package transform implements AST-level code transformations.
//
// Unlike text-based search-and-replace, these transformations understand
// Go syntax, preserve comments and formatting, and produce gofmt-clean output.
package transform

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/printer"
	"go/token"
	"go/types"
	"os"
	"sort"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/imports"

	"github.com/trendvidia/caliper/pkg/analysis"
	"github.com/trendvidia/caliper/pkg/analysis/patterns"
	"github.com/trendvidia/caliper/pkg/analysis/templates"
)

// Change describes a single transformation applied to a file.
type Change struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Before   string `json:"before"`   // original code fragment
	After    string `json:"after"`    // transformed code fragment
	Applied  bool   `json:"applied"`  // true if actually written (false in preview)
}

// Result is the outcome of a transformation.
type Result struct {
	Changes []Change `json:"changes"`
	Files   int      `json:"files_affected"`
	Total   int      `json:"total_changes"`
	Preview bool     `json:"preview"` // true if no changes were written
}

// Rewriter applies pattern-based transformations.
type Rewriter struct {
	match   *patterns.Pattern
	replace *patterns.Pattern
	engine  *analysis.Engine
}

// NewRewriter creates a rewriter that replaces `match` with `replace`.
func NewRewriter(engine *analysis.Engine, matchPat, replacePat string) (*Rewriter, error) {
	m, err := patterns.Compile(matchPat)
	if err != nil {
		return nil, fmt.Errorf("compiling match pattern: %w", err)
	}
	r, err := patterns.Compile(replacePat)
	if err != nil {
		return nil, fmt.Errorf("compiling replace pattern: %w", err)
	}
	return &Rewriter{
		match:   m,
		replace: r,
		engine:  engine,
	}, nil
}

// Preview shows what would change without applying.
func (rw *Rewriter) Preview() (*Result, error) {
	return rw.run(true)
}

// Apply executes the transformation and writes files.
func (rw *Rewriter) Apply() (*Result, error) {
	return rw.run(false)
}

func (rw *Rewriter) run(preview bool) (*Result, error) {
	result := &Result{Preview: preview}
	filesAffected := make(map[string]bool)

	for _, pkg := range rw.engine.Packages() {
		for _, file := range pkg.Syntax {
			filename := rw.engine.Fset().Position(file.Pos()).Filename
			changes := rw.transformFile(file, pkg.TypesInfo, filename, !preview)

			if len(changes) > 0 {
				filesAffected[filename] = true
				for i := range changes {
					changes[i].Applied = !preview
				}
				result.Changes = append(result.Changes, changes...)

				if !preview {
					if err := rw.writeFile(filename, file); err != nil {
						return nil, fmt.Errorf("writing %s: %w", filename, err)
					}
				}
			}
		}
	}

	result.Files = len(filesAffected)
	result.Total = len(result.Changes)
	return result, nil
}

func (rw *Rewriter) transformFile(file *ast.File, info *types.Info, filename string, apply bool) []Change {
	var changes []Change

	astutil.Apply(file, func(c *astutil.Cursor) bool {
		n := c.Node()
		if n == nil {
			return true
		}

		bindings := rw.match.MatchNode(n, info)
		if bindings == nil {
			return true
		}

		// Capture the "before" text.
		before := nodeText(rw.engine.Fset(), n)

		// Build the replacement node using bindings.
		replacement := rw.buildReplacement(bindings)
		if replacement == nil {
			return true
		}

		after := nodeText(rw.engine.Fset(), replacement)

		pos := rw.engine.Fset().Position(n.Pos())
		changes = append(changes, Change{
			File:   filename,
			Line:   pos.Line,
			Before: before,
			After:  after,
		})

		// Replace the node in the AST when applying.
		if apply {
			c.Replace(replacement)
		}

		return false // don't recurse into matched/replaced node
	}, nil)

	return changes
}

func (rw *Rewriter) buildReplacement(bindings *patterns.Bindings) ast.Node {
	// Clone the replace pattern's AST and substitute metavariables
	// with their bound values from the match.
	if bindings == nil {
		return nil
	}
	return substituteBindings(rw.replace, bindings)
}

// substituteBindings creates a new AST from the replace pattern with
// metavariables replaced by their bound values.
func substituteBindings(replace *patterns.Pattern, bindings *patterns.Bindings) ast.Node {
	if replace == nil || bindings == nil {
		return nil
	}
	return cloneAndSubstitute(replace.Node(), *bindings)
}

// cloneAndSubstitute deep-clones an AST node, replacing metavariable
// references with their bound values from the match.
func cloneAndSubstitute(node ast.Node, bindings patterns.Bindings) ast.Node {
	if node == nil {
		return nil
	}

	// Check if this node is a metavariable reference.
	if ident, ok := node.(*ast.Ident); ok {
		if name, isMeta := metavarName(ident.Name); isMeta {
			if b, exists := bindings[name]; exists && b.Node != nil {
				return cloneNode(b.Node)
			}
		}
	}

	switch n := node.(type) {
	case *ast.Ident:
		return &ast.Ident{Name: n.Name}

	case *ast.BasicLit:
		return &ast.BasicLit{Kind: n.Kind, Value: n.Value}

	case *ast.CallExpr:
		fun := cloneAndSubstitute(n.Fun, bindings)
		args := substituteExprList(n.Args, bindings)
		return &ast.CallExpr{
			Fun:      fun.(ast.Expr),
			Args:     args,
			Ellipsis: n.Ellipsis,
		}

	case *ast.SelectorExpr:
		x := cloneAndSubstitute(n.X, bindings).(ast.Expr)
		sel := cloneAndSubstitute(n.Sel, bindings).(*ast.Ident)
		return &ast.SelectorExpr{X: x, Sel: sel}

	case *ast.BinaryExpr:
		x := cloneAndSubstitute(n.X, bindings).(ast.Expr)
		y := cloneAndSubstitute(n.Y, bindings).(ast.Expr)
		return &ast.BinaryExpr{X: x, Op: n.Op, Y: y}

	case *ast.UnaryExpr:
		x := cloneAndSubstitute(n.X, bindings).(ast.Expr)
		return &ast.UnaryExpr{Op: n.Op, X: x}

	case *ast.ParenExpr:
		x := cloneAndSubstitute(n.X, bindings).(ast.Expr)
		return &ast.ParenExpr{X: x}

	case *ast.StarExpr:
		x := cloneAndSubstitute(n.X, bindings).(ast.Expr)
		return &ast.StarExpr{X: x}

	case *ast.IndexExpr:
		x := cloneAndSubstitute(n.X, bindings).(ast.Expr)
		index := cloneAndSubstitute(n.Index, bindings).(ast.Expr)
		return &ast.IndexExpr{X: x, Index: index}

	case *ast.CompositeLit:
		var typ ast.Expr
		if n.Type != nil {
			typ = cloneAndSubstitute(n.Type, bindings).(ast.Expr)
		}
		elts := substituteExprList(n.Elts, bindings)
		return &ast.CompositeLit{Type: typ, Elts: elts}

	case *ast.KeyValueExpr:
		key := cloneAndSubstitute(n.Key, bindings).(ast.Expr)
		value := cloneAndSubstitute(n.Value, bindings).(ast.Expr)
		return &ast.KeyValueExpr{Key: key, Value: value}

	case *ast.AssignStmt:
		lhs := substituteExprList(n.Lhs, bindings)
		rhs := substituteExprList(n.Rhs, bindings)
		return &ast.AssignStmt{Lhs: lhs, Tok: n.Tok, Rhs: rhs}

	case *ast.ExprStmt:
		x := cloneAndSubstitute(n.X, bindings).(ast.Expr)
		return &ast.ExprStmt{X: x}

	case *ast.ReturnStmt:
		results := substituteExprList(n.Results, bindings)
		return &ast.ReturnStmt{Results: results}

	case *ast.IfStmt:
		var init ast.Stmt
		if n.Init != nil {
			init = cloneAndSubstitute(n.Init, bindings).(ast.Stmt)
		}
		cond := cloneAndSubstitute(n.Cond, bindings).(ast.Expr)
		body := cloneAndSubstitute(n.Body, bindings).(*ast.BlockStmt)
		var els ast.Stmt
		if n.Else != nil {
			els = cloneAndSubstitute(n.Else, bindings).(ast.Stmt)
		}
		return &ast.IfStmt{Init: init, Cond: cond, Body: body, Else: els}

	case *ast.BlockStmt:
		var list []ast.Stmt
		for _, s := range n.List {
			list = append(list, cloneAndSubstitute(s, bindings).(ast.Stmt))
		}
		return &ast.BlockStmt{List: list}

	default:
		// For unhandled node types, return as-is (no metavars expected).
		return node
	}
}

// substituteExprList clones and substitutes a list of expressions,
// expanding variadic bindings inline.
func substituteExprList(exprs []ast.Expr, bindings patterns.Bindings) []ast.Expr {
	var result []ast.Expr
	for _, expr := range exprs {
		if ident, ok := expr.(*ast.Ident); ok {
			if name, isMeta := metavarName(ident.Name); isMeta {
				if b, exists := bindings[name]; exists && b.Node != nil {
					// For variadic bindings, the bound node might be
					// a wrapper — but single nodes just get appended.
					result = append(result, cloneNode(b.Node).(ast.Expr))
					continue
				}
			}
		}
		result = append(result, cloneAndSubstitute(expr, bindings).(ast.Expr))
	}
	return result
}

// metavarName extracts the user-visible metavariable name from an internal
// identifier. Returns ("", false) if it's not a metavariable.
func metavarName(name string) (string, bool) {
	if strings.HasPrefix(name, "__meta_variadic_") {
		return strings.TrimPrefix(name, "__meta_variadic_"), true
	}
	if strings.HasPrefix(name, "__meta_") {
		n := strings.TrimPrefix(name, "__meta_")
		if n == "wildcard" {
			return "_", true
		}
		return n, true
	}
	return "", false
}

// cloneNode deep-clones an AST node without substitution.
func cloneNode(node ast.Node) ast.Node {
	if node == nil {
		return nil
	}
	switch n := node.(type) {
	case *ast.Ident:
		return &ast.Ident{Name: n.Name}
	case *ast.BasicLit:
		return &ast.BasicLit{Kind: n.Kind, Value: n.Value}
	case *ast.CallExpr:
		args := make([]ast.Expr, len(n.Args))
		for i, a := range n.Args {
			args[i] = cloneNode(a).(ast.Expr)
		}
		return &ast.CallExpr{
			Fun:      cloneNode(n.Fun).(ast.Expr),
			Args:     args,
			Ellipsis: n.Ellipsis,
		}
	case *ast.SelectorExpr:
		return &ast.SelectorExpr{
			X:   cloneNode(n.X).(ast.Expr),
			Sel: cloneNode(n.Sel).(*ast.Ident),
		}
	case *ast.BinaryExpr:
		return &ast.BinaryExpr{
			X: cloneNode(n.X).(ast.Expr), Op: n.Op, Y: cloneNode(n.Y).(ast.Expr),
		}
	case *ast.UnaryExpr:
		return &ast.UnaryExpr{Op: n.Op, X: cloneNode(n.X).(ast.Expr)}
	case *ast.ParenExpr:
		return &ast.ParenExpr{X: cloneNode(n.X).(ast.Expr)}
	case *ast.StarExpr:
		return &ast.StarExpr{X: cloneNode(n.X).(ast.Expr)}
	case *ast.IndexExpr:
		return &ast.IndexExpr{
			X: cloneNode(n.X).(ast.Expr), Index: cloneNode(n.Index).(ast.Expr),
		}
	case *ast.CompositeLit:
		var typ ast.Expr
		if n.Type != nil {
			typ = cloneNode(n.Type).(ast.Expr)
		}
		elts := make([]ast.Expr, len(n.Elts))
		for i, e := range n.Elts {
			elts[i] = cloneNode(e).(ast.Expr)
		}
		return &ast.CompositeLit{Type: typ, Elts: elts}
	case *ast.KeyValueExpr:
		return &ast.KeyValueExpr{
			Key: cloneNode(n.Key).(ast.Expr), Value: cloneNode(n.Value).(ast.Expr),
		}
	case *ast.ExprStmt:
		return &ast.ExprStmt{X: cloneNode(n.X).(ast.Expr)}
	case *ast.AssignStmt:
		lhs := make([]ast.Expr, len(n.Lhs))
		for i, e := range n.Lhs {
			lhs[i] = cloneNode(e).(ast.Expr)
		}
		rhs := make([]ast.Expr, len(n.Rhs))
		for i, e := range n.Rhs {
			rhs[i] = cloneNode(e).(ast.Expr)
		}
		return &ast.AssignStmt{Lhs: lhs, Tok: n.Tok, Rhs: rhs}
	case *ast.ReturnStmt:
		results := make([]ast.Expr, len(n.Results))
		for i, e := range n.Results {
			results[i] = cloneNode(e).(ast.Expr)
		}
		return &ast.ReturnStmt{Results: results}
	default:
		return node
	}
}

func (rw *Rewriter) writeFile(filename string, file *ast.File) error {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, rw.engine.Fset(), file); err != nil {
		return fmt.Errorf("printing AST: %w", err)
	}

	// Apply gofmt.
	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		// If gofmt fails, write unformatted (shouldn't happen).
		formatted = buf.Bytes()
	}

	return os.WriteFile(filename, formatted, 0o644)
}

func nodeText(fset *token.FileSet, node ast.Node) string {
	var buf bytes.Buffer
	printer.Fprint(&buf, fset, node)
	return buf.String()
}

// --------------------------------------------------------------------------
// API Migration
// --------------------------------------------------------------------------

// MigrationRule is a single old→new mapping in an API migration.
type MigrationRule struct {
	Old             string            `json:"old"`
	New             string            `json:"new"`
	TypeConstraints map[string]string `json:"type_constraints,omitempty"`
}

// APIMigration applies a sequence of transformation rules and updates imports.
type APIMigration struct {
	Rules         []MigrationRule
	UpdateImports bool
	engine        *analysis.Engine
	lastChanges   []Change // populated by run() for fixImports
}

// NewAPIMigration creates an API migration from a set of rules.
func NewAPIMigration(engine *analysis.Engine, rules []MigrationRule, updateImports bool) *APIMigration {
	return &APIMigration{
		Rules:         rules,
		UpdateImports: updateImports,
		engine:        engine,
	}
}

// Preview shows what the migration would change.
func (am *APIMigration) Preview() (*Result, error) {
	return am.run(true)
}

// Apply executes the migration.
func (am *APIMigration) Apply() (*Result, error) {
	return am.run(false)
}

func (am *APIMigration) run(preview bool) (*Result, error) {
	combined := &Result{Preview: preview}

	for _, rule := range am.Rules {
		rw, err := NewRewriter(am.engine, rule.Old, rule.New)
		if err != nil {
			return nil, fmt.Errorf("rule %q → %q: %w", rule.Old, rule.New, err)
		}

		var result *Result
		if preview {
			result, err = rw.Preview()
		} else {
			result, err = rw.Apply()
		}
		if err != nil {
			return nil, err
		}

		combined.Changes = append(combined.Changes, result.Changes...)
		combined.Total += result.Total
	}

	// Count unique files.
	files := make(map[string]bool)
	for _, c := range combined.Changes {
		files[c.File] = true
	}
	combined.Files = len(files)

	am.lastChanges = combined.Changes

	if am.UpdateImports && !preview {
		am.fixImports()
	}

	return combined, nil
}

func (am *APIMigration) fixImports() {
	// Collect unique files that were modified.
	files := make(map[string]bool)
	for _, c := range am.lastChanges {
		files[c.File] = true
	}

	// Run goimports on each modified file to add missing imports
	// and remove unused ones.
	for file := range files {
		src, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		formatted, err := imports.Process(file, src, &imports.Options{
			Comments:  true,
			TabIndent: true,
			TabWidth:  8,
		})
		if err != nil {
			continue
		}
		os.WriteFile(file, formatted, 0o644)
	}
}

// --------------------------------------------------------------------------
// Cleanup insertion
// --------------------------------------------------------------------------

// CleanupInserter adds missing defer/cleanup statements for resource leaks.
type CleanupInserter struct {
	AcquirePattern string
	ReleaseMethod  string
	Style          string // "defer" or "explicit"
	engine         *analysis.Engine
}

// NewCleanupInserter creates a cleanup inserter.
func NewCleanupInserter(engine *analysis.Engine, acquire, release, style string) (*CleanupInserter, error) {
	if release == "" {
		release = "Close"
	}
	if style == "" {
		style = "defer"
	}
	return &CleanupInserter{
		AcquirePattern: acquire,
		ReleaseMethod:  release,
		Style:          style,
		engine:         engine,
	}, nil
}

// Preview shows what cleanup statements would be inserted.
func (ci *CleanupInserter) Preview() (*Result, error) {
	return ci.run(true)
}

// Apply inserts cleanup statements.
func (ci *CleanupInserter) Apply() (*Result, error) {
	return ci.run(false)
}

func (ci *CleanupInserter) run(preview bool) (*Result, error) {
	tmpl, err := templates.NewResourceLeak(ci.AcquirePattern, ci.ReleaseMethod, true)
	if err != nil {
		return nil, fmt.Errorf("compiling acquire pattern: %w", err)
	}

	leaks := templates.RunTemplate(ci.engine, tmpl)

	result := &Result{Preview: preview}
	filesAffected := make(map[string]bool)

	// For each leak, find the acquisition site and generate a cleanup insertion.
	for _, leak := range leaks {
		resourceVar := extractResourceVar(leak.Suggestion, ci.ReleaseMethod)
		if resourceVar == "" {
			continue
		}

		var cleanup string
		if ci.Style == "defer" {
			cleanup = fmt.Sprintf("defer %s.%s()", resourceVar, ci.ReleaseMethod)
		} else {
			cleanup = fmt.Sprintf("%s.%s()", resourceVar, ci.ReleaseMethod)
		}

		// Determine insertion line: after the error check following the acquisition.
		insertLine := ci.findInsertionLine(leak.Location)

		change := Change{
			File:    leak.Location.File,
			Line:    insertLine,
			Before:  "",
			After:   cleanup,
			Applied: !preview,
		}

		result.Changes = append(result.Changes, change)
		filesAffected[leak.Location.File] = true
	}

	result.Files = len(filesAffected)
	result.Total = len(result.Changes)

	if !preview && len(result.Changes) > 0 {
		if err := ci.applyTextInsertions(result.Changes); err != nil {
			return nil, err
		}
	}

	return result, nil
}

// findInsertionLine determines where to insert the cleanup statement.
// It looks for an error check pattern after the acquisition and inserts
// after it. Falls back to inserting on the line after the acquisition.
func (ci *CleanupInserter) findInsertionLine(loc analysis.FileLocation) int {
	src, err := os.ReadFile(loc.File)
	if err != nil {
		return loc.Line + 1
	}

	lines := strings.Split(string(src), "\n")
	acquireLine := loc.Line - 1 // 0-indexed

	// Look for "if err != nil {" pattern in the lines following the acquisition.
	for i := acquireLine + 1; i < len(lines) && i <= acquireLine+3; i++ {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "if ") && strings.Contains(trimmed, "!= nil") {
			// Find the closing brace of the if block.
			braceDepth := 0
			for j := i; j < len(lines); j++ {
				braceDepth += strings.Count(lines[j], "{") - strings.Count(lines[j], "}")
				if braceDepth <= 0 {
					return j + 2 // 1-indexed, after the closing brace
				}
			}
		}
	}

	return loc.Line + 1
}

// applyTextInsertions inserts cleanup lines into source files.
func (ci *CleanupInserter) applyTextInsertions(changes []Change) error {
	// Group by file.
	byFile := make(map[string][]Change)
	for _, c := range changes {
		byFile[c.File] = append(byFile[c.File], c)
	}

	for file, fileChanges := range byFile {
		src, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("reading %s: %w", file, err)
		}

		lines := strings.Split(string(src), "\n")

		// Sort by line descending so insertions don't shift subsequent line numbers.
		sort.Slice(fileChanges, func(i, j int) bool {
			return fileChanges[i].Line > fileChanges[j].Line
		})

		for _, c := range fileChanges {
			idx := c.Line - 1 // 0-indexed
			if idx < 0 {
				idx = 0
			}
			if idx > len(lines) {
				idx = len(lines)
			}

			// Determine indentation from surrounding code.
			indent := "\t"
			if idx > 0 && idx <= len(lines) {
				indent = extractIndent(lines[idx-1])
			}

			newLine := indent + c.After
			tail := append([]string{newLine}, lines[idx:]...)
			lines = append(lines[:idx], tail...)
		}

		output := []byte(strings.Join(lines, "\n"))

		// Format with goimports.
		formatted, fmtErr := imports.Process(file, output, nil)
		if fmtErr != nil {
			formatted = output
		}

		if err := os.WriteFile(file, formatted, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", file, err)
		}
	}

	return nil
}

// extractResourceVar extracts the resource variable name from a suggestion
// string like "Add 'defer f.Close()' after error check".
func extractResourceVar(suggestion, releaseMethod string) string {
	prefix := "defer "
	idx := strings.Index(suggestion, prefix)
	if idx < 0 {
		prefix = "Add 'defer "
		idx = strings.Index(suggestion, prefix)
		if idx < 0 {
			return ""
		}
		idx += len("Add '")
	}

	rest := suggestion[idx+len(prefix):]
	dotIdx := strings.Index(rest, "."+releaseMethod)
	if dotIdx < 0 {
		return ""
	}
	return rest[:dotIdx]
}

// extractIndent returns the leading whitespace of a line.
func extractIndent(line string) string {
	trimmed := strings.TrimLeft(line, " \t")
	return line[:len(line)-len(trimmed)]
}
