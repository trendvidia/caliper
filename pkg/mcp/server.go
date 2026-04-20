// Package mcp implements the MCP server for GoCaliper.
//
// It registers all tools and routes incoming requests to the appropriate
// analysis engine, template, or transformation. The server runs over
// stdio (standard MCP transport).
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"golang.org/x/tools/go/packages"

	"github.com/trendvidia/gocaliper/pkg/analysis"
	"github.com/trendvidia/gocaliper/pkg/analysis/cfg"
	"github.com/trendvidia/gocaliper/pkg/analysis/patterns"
	"github.com/trendvidia/gocaliper/pkg/analysis/templates"
	"github.com/trendvidia/gocaliper/pkg/iso"
	"github.com/trendvidia/gocaliper/pkg/transform"
)

// Server is the GoCaliper MCP server.
type Server struct {
	mcp *mcpserver.MCPServer
	iso *iso.Registry
}

// NewServer creates and configures the MCP server with all tools.
func NewServer() (*Server, error) {
	s := &Server{
		iso: iso.NewRegistry(),
	}

	s.mcp = mcpserver.NewMCPServer(
		"GoCaliper",
		"0.1.0",
		mcpserver.WithToolCapabilities(true),
	)

	// Register all tools with their handlers.
	s.registerTools()
	return s, nil
}

// Run starts the MCP server on stdio.
func (s *Server) Run(ctx context.Context) error {
	stdio := mcpserver.NewStdioServer(s.mcp)
	return stdio.Listen(ctx, nil, nil)
}

func (s *Server) registerTools() {
	s.mcp.AddTool(findPatternTool, s.handleFindPattern)
	s.mcp.AddTool(findTypeUsageTool, s.handleFindTypeUsage)
	s.mcp.AddTool(findResourceLeakTool, s.handleFindResourceLeak)
	s.mcp.AddTool(findUncheckedErrorTool, s.handleFindUncheckedError)
	s.mcp.AddTool(findLockViolationTool, s.handleFindLockViolation)
	s.mcp.AddTool(findNilDerefTool, s.handleFindNilDeref)
	s.mcp.AddTool(checkPathPropertyTool, s.handleCheckPathProperty)
	s.mcp.AddTool(applyTransformTool, s.handleApplyTransform)
	s.mcp.AddTool(migrateAPITool, s.handleMigrateAPI)
	s.mcp.AddTool(addMissingCleanupTool, s.handleAddMissingCleanup)
	s.mcp.AddTool(explainMatchTool, s.handleExplainMatch)
	s.mcp.AddTool(listEquivalencesTool, s.handleListEquivalences)
}

// --------------------------------------------------------------------------
// Handler implementations
// --------------------------------------------------------------------------

func (s *Server) handleFindPattern(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	pattern := stringParam(req, "pattern")
	path := stringParam(req, "path")
	if path == "" {
		path = "."
	}

	p, err := patterns.Compile(pattern)
	if err != nil {
		return errorResult("Invalid pattern: %v", err), nil
	}

	engine, err := analysis.NewEngine(path)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}

	var matches []map[string]any
	engine.Walk(func(pkg *packages.Package, file *ast.File, node ast.Node, info *types.Info) bool {
		bindings := p.MatchNode(node, info)
		if bindings == nil {
			return true
		}

		loc := engine.LocationOf(node.Pos())
		match := map[string]any{
			"file":   loc.File,
			"line":   loc.Line,
			"column": loc.Column,
		}

		// Include bindings.
		if len(*bindings) > 0 {
			bindMap := make(map[string]string)
			for name, b := range *bindings {
				bindMap[name] = fmt.Sprintf("%v", b.Node)
			}
			match["bindings"] = bindMap
		}

		matches = append(matches, match)
		return true
	})

	return jsonResult(map[string]any{
		"pattern":      pattern,
		"total_matches": len(matches),
		"matches":      matches,
	}), nil
}

func (s *Server) handleFindTypeUsage(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	typeName := stringParam(req, "type_name")
	path := stringParam(req, "path")
	if path == "" {
		path = "."
	}

	engine, err := analysis.NewEngine(path)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}

	// Parse desired usage kinds.
	wantAll := true
	wantKinds := make(map[string]bool)
	if kindsRaw, ok := req.Params.Arguments["usage_kinds"]; ok {
		if kindsSlice, ok := kindsRaw.([]any); ok && len(kindsSlice) > 0 {
			wantAll = false
			for _, k := range kindsSlice {
				if ks, ok := k.(string); ok {
					if ks == "all" {
						wantAll = true
						break
					}
					wantKinds[ks] = true
				}
			}
		}
	}

	baseType := strings.TrimPrefix(typeName, "*")

	var usages []map[string]any

	for _, pkg := range engine.Packages() {
		if pkg.TypesInfo == nil {
			continue
		}
		for _, file := range pkg.Syntax {
			ast.Inspect(file, func(n ast.Node) bool {
				if n == nil {
					return false
				}

				var kind string
				var pos token.Pos

				switch node := n.(type) {
				case *ast.CompositeLit:
					if matchesTypeName(node.Type, typeName, baseType) {
						kind = "instantiation"
						pos = node.Pos()
					}

				case *ast.TypeAssertExpr:
					if matchesTypeName(node.Type, typeName, baseType) {
						kind = "type_assertion"
						pos = node.Pos()
					}

				case *ast.ValueSpec:
					if matchesTypeName(node.Type, typeName, baseType) {
						kind = "field"
						pos = node.Pos()
					}

				case *ast.Field:
					if matchesTypeName(node.Type, typeName, baseType) {
						kind = "field"
						pos = node.Pos()
					}

				case *ast.AssignStmt:
					for _, rhs := range node.Rhs {
						if t := pkg.TypesInfo.TypeOf(rhs); t != nil {
							if typeStringMatches(t, typeName, baseType) {
								kind = "assignment"
								pos = node.Pos()
								break
							}
						}
					}

				case *ast.ReturnStmt:
					for _, result := range node.Results {
						if t := pkg.TypesInfo.TypeOf(result); t != nil {
							if typeStringMatches(t, typeName, baseType) {
								kind = "return"
								pos = result.Pos()
								break
							}
						}
					}

				case *ast.CallExpr:
					for _, arg := range node.Args {
						if t := pkg.TypesInfo.TypeOf(arg); t != nil {
							if typeStringMatches(t, typeName, baseType) {
								kind = "argument"
								pos = arg.Pos()
								break
							}
						}
					}
				}

				if kind != "" && pos.IsValid() && (wantAll || wantKinds[kind]) {
					loc := engine.LocationOf(pos)
					usages = append(usages, map[string]any{
						"kind":   kind,
						"file":   loc.File,
						"line":   loc.Line,
						"column": loc.Column,
					})
				}

				return true
			})
		}
	}

	return jsonResult(map[string]any{
		"type_name":    typeName,
		"total_usages": len(usages),
		"usages":       usages,
	}), nil
}

func (s *Server) handleFindResourceLeak(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	acquirePattern := stringParam(req, "acquire_pattern")
	releaseMethod := stringParam(req, "release_method")
	path := stringParam(req, "path")
	severity := stringParam(req, "severity")

	if path == "" {
		path = "."
	}
	allPaths := severity != "any_path"

	tmpl, err := templates.NewResourceLeak(acquirePattern, releaseMethod, allPaths)
	if err != nil {
		return errorResult("Invalid acquire pattern: %v", err), nil
	}

	engine, err := analysis.NewEngine(path)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}

	findings := templates.RunTemplate(engine, tmpl)
	return jsonResult(map[string]any{
		"total_findings": len(findings),
		"findings":       findings,
	}), nil
}

func (s *Server) handleFindUncheckedError(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	path := stringParam(req, "path")
	if path == "" {
		path = "."
	}

	includeDefers := boolParam(req, "include_defers")
	includeExplicit := boolParamDefault(req, "include_explicit_blank", true)

	tmpl := &templates.UncheckedError{
		IncludeDefers:   includeDefers,
		IncludeExplicit: includeExplicit,
	}

	engine, err := analysis.NewEngine(path)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}

	findings := templates.RunTemplate(engine, tmpl)
	return jsonResult(map[string]any{
		"total_findings": len(findings),
		"findings":       findings,
	}), nil
}

func (s *Server) handleFindLockViolation(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	path := stringParam(req, "path")
	if path == "" {
		path = "."
	}

	checkBlocking := boolParamDefault(req, "check_blocking_under_lock", true)

	tmpl := &templates.LockViolation{
		CheckBlockingUnderLock: checkBlocking,
	}

	engine, err := analysis.NewEngine(path)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}

	findings := templates.RunTemplate(engine, tmpl)
	return jsonResult(map[string]any{
		"total_findings": len(findings),
		"findings":       findings,
	}), nil
}

func (s *Server) handleFindNilDeref(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	path := stringParam(req, "path")
	if path == "" {
		path = "."
	}

	engine, err := analysis.NewEngine(path)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}

	var funcPattern *patterns.Pattern
	if fp := stringParam(req, "func_pattern"); fp != "" {
		funcPattern, err = patterns.Compile(fp)
		if err != nil {
			return errorResult("Invalid func_pattern: %v", err), nil
		}
	}

	tmpl := &templates.NilDeref{FuncPattern: funcPattern}
	findings := templates.RunTemplate(engine, tmpl)

	return jsonResult(map[string]any{
		"total_findings": len(findings),
		"findings":       findings,
	}), nil
}

func (s *Server) handleCheckPathProperty(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	fromPattern := stringParam(req, "from_pattern")
	targetPattern := stringParam(req, "target_pattern")
	property := stringParam(req, "property")
	excludePattern := stringParam(req, "exclude_pattern")
	path := stringParam(req, "path")

	if path == "" {
		path = "."
	}

	engine, err := analysis.NewEngine(path)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}

	fromPat, err := patterns.Compile(fromPattern)
	if err != nil {
		return errorResult("Invalid from_pattern: %v", err), nil
	}
	targetPat, err := patterns.Compile(targetPattern)
	if err != nil {
		return errorResult("Invalid target_pattern: %v", err), nil
	}

	var excludePat *patterns.Pattern
	if excludePattern != "" {
		excludePat, err = patterns.Compile(excludePattern)
		if err != nil {
			return errorResult("Invalid exclude_pattern: %v", err), nil
		}
	}

	var findings []templates.Finding

	for _, fc := range engine.FuncNodes() {
		tctx := templates.BuildContext(engine, fc)
		g := tctx.CFG

		fromPred := patternPredicate(fromPat, fc.Info)
		targetPred := patternPredicate(targetPat, fc.Info)

		switch property {
		case "always_before":
			// target_pattern must always precede from_pattern on all paths.
			violations := cfg.AlwaysBefore(g, targetPred, fromPred)
			for _, v := range violations {
				findings = append(findings, templates.Finding{
					Kind:     "path_property",
					Severity: "warning",
					Message: fmt.Sprintf(
						"'%s' reached without '%s' preceding it in %s",
						fromPattern, targetPattern, fc.Func.Name.Name,
					),
					Location: engine.LocationOf(fc.Func.Pos()),
					Path:     v.Path,
				})
			}

		case "always_after":
			// from_pattern must always be followed by target_pattern on all paths.
			for _, block := range g.Blocks {
				if !blockMatchesPattern(block, fromPred) {
					continue
				}
				violations := cfg.Reachable(g, block, targetPred, cfg.AllPaths)
				for _, v := range violations {
					findings = append(findings, templates.Finding{
						Kind:     "path_property",
						Severity: "warning",
						Message: fmt.Sprintf(
							"'%s' not always followed by '%s' in %s",
							fromPattern, targetPattern, fc.Func.Name.Name,
						),
						Location: engine.LocationOf(block.Pos),
						Path:     v.Path,
					})
				}
			}

		case "never_between":
			if excludePat == nil {
				return errorResult("never_between requires exclude_pattern"), nil
			}
			excludePred := patternPredicate(excludePat, fc.Info)
			violations := cfg.NeverBetween(g, fromPred, targetPred, excludePred)
			for _, v := range violations {
				findings = append(findings, templates.Finding{
					Kind:     "path_property",
					Severity: "warning",
					Message: fmt.Sprintf(
						"'%s' appears between '%s' and '%s' in %s",
						excludePattern, fromPattern, targetPattern, fc.Func.Name.Name,
					),
					Location: engine.LocationOf(fc.Func.Pos()),
					Path:     v.Path,
				})
			}

		case "exists_between":
			if excludePat == nil {
				return errorResult("exists_between requires exclude_pattern"), nil
			}
			excludePred := patternPredicate(excludePat, fc.Info)
			// If NeverBetween returns empty, exclude never appears → violation for exists_between.
			violations := cfg.NeverBetween(g, fromPred, targetPred, excludePred)
			if len(violations) == 0 {
				hasFrom, hasTarget := false, false
				for _, block := range g.Blocks {
					if blockMatchesPattern(block, fromPred) {
						hasFrom = true
					}
					if blockMatchesPattern(block, targetPred) {
						hasTarget = true
					}
				}
				if hasFrom && hasTarget {
					findings = append(findings, templates.Finding{
						Kind:     "path_property",
						Severity: "warning",
						Message: fmt.Sprintf(
							"'%s' never appears between '%s' and '%s' in %s",
							excludePattern, fromPattern, targetPattern, fc.Func.Name.Name,
						),
						Location: engine.LocationOf(fc.Func.Pos()),
					})
				}
			}

		case "dominates":
			if !cfg.Dominates(g, fromPred, targetPred) {
				hasFrom, hasTarget := false, false
				for _, block := range g.Blocks {
					if blockMatchesPattern(block, fromPred) {
						hasFrom = true
					}
					if blockMatchesPattern(block, targetPred) {
						hasTarget = true
					}
				}
				if hasFrom && hasTarget {
					findings = append(findings, templates.Finding{
						Kind:     "path_property",
						Severity: "warning",
						Message: fmt.Sprintf(
							"'%s' does not dominate '%s' in %s",
							fromPattern, targetPattern, fc.Func.Name.Name,
						),
						Location: engine.LocationOf(fc.Func.Pos()),
					})
				}
			}
		}
	}

	return jsonResult(map[string]any{
		"property":       property,
		"from_pattern":   fromPattern,
		"target_pattern": targetPattern,
		"total_findings": len(findings),
		"findings":       findings,
	}), nil
}

func (s *Server) handleApplyTransform(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	matchPat := stringParam(req, "match")
	replacePat := stringParam(req, "replace")
	path := stringParam(req, "path")
	preview := boolParamDefault(req, "preview", true)

	if path == "" {
		path = "."
	}

	engine, err := analysis.NewEngine(path)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}

	rw, err := transform.NewRewriter(engine, matchPat, replacePat)
	if err != nil {
		return errorResult("Invalid patterns: %v", err), nil
	}

	var result *transform.Result
	if preview {
		result, err = rw.Preview()
	} else {
		result, err = rw.Apply()
	}
	if err != nil {
		return errorResult("Transform failed: %v", err), nil
	}

	return jsonResult(result), nil
}

func (s *Server) handleMigrateAPI(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	path := stringParam(req, "path")
	preview := boolParamDefault(req, "preview", true)
	updateImports := boolParamDefault(req, "update_imports", true)

	if path == "" {
		path = "."
	}

	// Parse rules from request.
	rulesRaw, ok := req.Params.Arguments["rules"]
	if !ok {
		return errorResult("Missing required parameter: rules"), nil
	}

	rulesJSON, err := json.Marshal(rulesRaw)
	if err != nil {
		return errorResult("Invalid rules format: %v", err), nil
	}

	var rules []transform.MigrationRule
	if err := json.Unmarshal(rulesJSON, &rules); err != nil {
		return errorResult("Invalid rules format: %v", err), nil
	}

	engine, err := analysis.NewEngine(path)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}

	migration := transform.NewAPIMigration(engine, rules, updateImports)

	var result *transform.Result
	if preview {
		result, err = migration.Preview()
	} else {
		result, err = migration.Apply()
	}
	if err != nil {
		return errorResult("Migration failed: %v", err), nil
	}

	return jsonResult(result), nil
}

func (s *Server) handleAddMissingCleanup(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	acquirePattern := stringParam(req, "acquire_pattern")
	releaseMethod := stringParam(req, "release_method")
	style := stringParam(req, "cleanup_style")
	path := stringParam(req, "path")
	preview := boolParamDefault(req, "preview", true)

	if path == "" {
		path = "."
	}

	engine, err := analysis.NewEngine(path)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}

	inserter, err := transform.NewCleanupInserter(engine, acquirePattern, releaseMethod, style)
	if err != nil {
		return errorResult("Invalid parameters: %v", err), nil
	}

	var result *transform.Result
	if preview {
		result, err = inserter.Preview()
	} else {
		result, err = inserter.Apply()
	}
	if err != nil {
		return errorResult("Cleanup insertion failed: %v", err), nil
	}

	return jsonResult(result), nil
}

func (s *Server) handleExplainMatch(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	pattern := stringParam(req, "pattern")
	file := stringParam(req, "file")
	line := intParam(req, "line")

	pat, err := patterns.Compile(pattern)
	if err != nil {
		return errorResult("Invalid pattern: %v", err), nil
	}

	// Add equivalences for richer matching.
	pat = pat.WithEquivalences(s.iso)

	engine, err := analysis.NewEngine(filepath.Dir(file))
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}

	type attempt struct {
		NodeType string            `json:"node_type"`
		Code     string            `json:"code"`
		Line     int               `json:"line"`
		Matched  bool              `json:"matched"`
		Bindings map[string]string `json:"bindings,omitempty"`
		Types    map[string]string `json:"types,omitempty"`
	}

	var attempts []attempt
	var bestMatch *attempt

	engine.Walk(func(pkg *packages.Package, f *ast.File, node ast.Node, info *types.Info) bool {
		pos := engine.Fset().Position(node.Pos())
		if pos.Filename != file || pos.Line != line {
			return true
		}

		bindings := pat.MatchNode(node, info)
		a := attempt{
			NodeType: fmt.Sprintf("%T", node),
			Code:     printNode(engine.Fset(), node),
			Line:     pos.Line,
			Matched:  bindings != nil,
		}

		if bindings != nil {
			a.Bindings = make(map[string]string)
			a.Types = make(map[string]string)
			for name, b := range *bindings {
				a.Bindings[name] = printNode(engine.Fset(), b.Node)
				if b.Type != nil {
					a.Types[name] = b.Type.String()
				}
			}
			bestMatch = &a
		}

		attempts = append(attempts, a)
		return true
	})

	result := map[string]any{
		"pattern":  pattern,
		"file":     file,
		"line":     line,
		"matched":  bestMatch != nil,
		"attempts": attempts,
	}
	if bestMatch != nil {
		result["match"] = bestMatch
	}

	return jsonResult(result), nil
}

func (s *Server) handleListEquivalences(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	category := stringParam(req, "category")
	if category == "" {
		category = "all"
	}

	equivs := s.iso.ByCategory(iso.Category(category))

	var result []map[string]any
	for _, eq := range equivs {
		result = append(result, map[string]any{
			"name":        eq.Name,
			"category":    eq.Category,
			"description": eq.Description,
			"examples":    eq.Examples,
		})
	}

	return jsonResult(map[string]any{
		"category": category,
		"total":    len(result),
		"equivalences": result,
	}), nil
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

func stringParam(req mcpsdk.CallToolRequest, key string) string {
	v, ok := req.Params.Arguments[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

func boolParam(req mcpsdk.CallToolRequest, key string) bool {
	v, ok := req.Params.Arguments[key]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	if !ok {
		return false
	}
	return b
}

func boolParamDefault(req mcpsdk.CallToolRequest, key string, def bool) bool {
	v, ok := req.Params.Arguments[key]
	if !ok {
		return def
	}
	b, ok := v.(bool)
	if !ok {
		return def
	}
	return b
}

func jsonResult(v any) *mcpsdk.CallToolResult {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errorResult("Failed to marshal result: %v", err)
	}
	return &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{
			mcpsdk.TextContent{
				Type: "text",
				Text: string(data),
			},
		},
	}
}

func errorResult(format string, args ...any) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{
		IsError: true,
		Content: []mcpsdk.Content{
			mcpsdk.TextContent{
				Type: "text",
				Text: fmt.Sprintf(format, args...),
			},
		},
	}
}

func intParam(req mcpsdk.CallToolRequest, key string) int {
	v, ok := req.Params.Arguments[key]
	if !ok {
		return 0
	}
	// JSON numbers come as float64.
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

// patternPredicate builds a cfg.NodePredicate from a compiled pattern.
func patternPredicate(pat *patterns.Pattern, info *types.Info) cfg.NodePredicate {
	return func(_ *cfg.Block, node ast.Node) bool {
		return pat.MatchNode(node, info) != nil
	}
}

// blockMatchesPattern checks if any node in a block matches the predicate.
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

// matchesTypeName checks if an AST type expression matches a type name.
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

// typeStringMatches checks if a types.Type matches a type name string.
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

// printNode renders an AST node as Go source code.
func printNode(fset *token.FileSet, node ast.Node) string {
	var buf bytes.Buffer
	printer.Fprint(&buf, fset, node)
	return buf.String()
}
