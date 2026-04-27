package mcp

// All MCP tool handler implementations: read-only analyses (find_*,
// check_*), code-mutating transforms (apply_transform, migrate_api,
// add_missing_cleanup), and inspection helpers (explain_match,
// explain_finding, list_equivalences, run_patterns).
//
// Lifted from server.go to keep server.go focused on the server
// lifecycle (NewServer, Run, registerTools, transport shims) and to
// give handlers a clear home for new contributors.

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	"golang.org/x/tools/go/packages"

	"github.com/trendvidia/caliper/pkg/analysis"
	"github.com/trendvidia/caliper/pkg/analysis/cfg"
	"github.com/trendvidia/caliper/pkg/analysis/patterns"
	"github.com/trendvidia/caliper/pkg/analysis/taint"
	"github.com/trendvidia/caliper/pkg/analysis/templates"
	"github.com/trendvidia/caliper/pkg/gcl"
	"github.com/trendvidia/caliper/pkg/iso"
	"github.com/trendvidia/caliper/pkg/transform"
)

// --------------------------------------------------------------------------
// Handler implementations
// --------------------------------------------------------------------------

func (s *Server) handleFindPattern(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	pattern := stringParam(req, "pattern")
	path := stringParam(req, "path")
	path = s.defaultPath(path)

	p, err := patterns.Compile(pattern)
	if err != nil {
		return errorResult("Invalid pattern: %v", err), nil
	}

	// Apply name constraints if provided.
	if nc, ok := req.GetArguments()["name_constraints"]; ok {
		if ncMap, ok := nc.(map[string]any); ok && len(ncMap) > 0 {
			nameConst := make(patterns.NameConstraint, len(ncMap))
			for k, v := range ncMap {
				if vs, ok := v.(string); ok {
					nameConst[k] = vs
				}
			}
			p, err = p.WithNameConstraints(nameConst)
			if err != nil {
				return errorResult("Invalid name constraint: %v", err), nil
			}
		}
	}

	prog := newProgress(ctx, req)
	engine, release, err := s.getEngineWithProgress(path, prog)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}
	defer release()
	if prog.active() {
		prog.send(2, 3, "Matching patterns...")
	}

	// Negative mode: find functions where the pattern is absent.
	if boolParam(req, "negative") {
		neg := patterns.NewNegative(p)
		absences := neg.MatchAbsence(engine)

		var hits []map[string]any
		for _, m := range absences {
			hit := map[string]any{
				"loc": fmt.Sprintf("%s:%d", m.File, m.Line),
			}
			if len(m.Bindings) > 0 {
				bindMap := make(map[string]string)
				for name, b := range m.Bindings {
					bindMap[name] = printNode(engine.Fset(), b.Node)
				}
				hit["bindings"] = bindMap
			}
			hits = append(hits, hit)
		}

		return jsonResult(map[string]any{
			"pattern":  pattern,
			"negative": true,
			"total":    len(hits),
			"hits":     hits,
		}), nil
	}

	var hits []map[string]any
	engine.Walk(func(pkg *packages.Package, file *ast.File, node ast.Node, info *types.Info) bool {
		bindings := p.MatchNode(node, info)
		if bindings == nil {
			return true
		}

		loc := engine.LocationOf(node.Pos())
		hit := map[string]any{
			"loc": compactLoc(loc),
		}

		// Include bindings.
		if len(*bindings) > 0 {
			bindMap := make(map[string]string)
			for name, b := range *bindings {
				bindMap[name] = fmt.Sprintf("%v", b.Node)
			}
			hit["bindings"] = bindMap
		}

		hits = append(hits, hit)
		return true
	})

	return jsonResult(map[string]any{
		"pattern": pattern,
		"total":   len(hits),
		"hits":    hits,
	}), nil
}

func (s *Server) handleFindTypeUsage(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	typeName := stringParam(req, "type_name")
	path := stringParam(req, "path")
	path = s.defaultPath(path)

	engine, release, err := s.getEngine(path)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}
	defer release()

	// Parse desired usage kinds.
	wantAll := true
	wantKinds := make(map[string]bool)
	if kindsRaw, ok := req.GetArguments()["usage_kinds"]; ok {
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
						"kind": kind,
						"loc":  compactLoc(loc),
					})
				}

				return true
			})
		}
	}

	return jsonResult(map[string]any{
		"type_name": typeName,
		"total":     len(usages),
		"hits":      usages,
	}), nil
}

func (s *Server) handleFindResourceLeak(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	acquirePattern := stringParam(req, "acquire_pattern")
	releaseMethod := stringParam(req, "release_method")
	path := stringParam(req, "path")
	severity := stringParam(req, "severity")

	path = s.defaultPath(path)
	allPaths := severity != "any_path"

	tmpl, err := templates.NewResourceLeak(acquirePattern, releaseMethod, allPaths)
	if err != nil {
		return errorResult("Invalid acquire pattern: %v", err), nil
	}

	prog := newProgress(ctx, req)
	engine, release, err := s.getEngineWithProgress(path, prog)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}
	defer release()

	if prog.active() {
		prog.send(2, 3, "Analyzing resource leaks...")
	}
	findings := s.runTemplate(path, engine, tmpl)
	if prog.active() {
		prog.send(3, 3, "Done")
	}
	return s.findingsResult(findings), nil
}

func (s *Server) handleFindUncheckedError(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	path := stringParam(req, "path")
	path = s.defaultPath(path)

	includeDefers := boolParam(req, "include_defers")
	includeExplicit := boolParamDefault(req, "include_explicit_blank", true)

	tmpl := &templates.UncheckedError{
		IncludeDefers:   includeDefers,
		IncludeExplicit: includeExplicit,
	}

	prog := newProgress(ctx, req)
	engine, release, err := s.getEngineWithProgress(path, prog)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}
	defer release()

	if prog.active() {
		prog.send(2, 3, "Checking for unchecked errors...")
	}
	findings := s.runTemplate(path, engine, tmpl)
	if prog.active() {
		prog.send(3, 3, "Done")
	}
	return s.findingsResult(findings), nil
}

func (s *Server) handleFindLockViolation(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	path := stringParam(req, "path")
	path = s.defaultPath(path)

	checkBlocking := boolParamDefault(req, "check_blocking_under_lock", true)

	tmpl := &templates.LockViolation{
		CheckBlockingUnderLock: checkBlocking,
	}

	prog := newProgress(ctx, req)
	engine, release, err := s.getEngineWithProgress(path, prog)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}
	defer release()

	if prog.active() {
		prog.send(2, 3, "Checking lock discipline...")
	}
	findings := s.runTemplate(path, engine, tmpl)
	if prog.active() {
		prog.send(3, 3, "Done")
	}
	return s.findingsResult(findings), nil
}

func (s *Server) handleFindNilDeref(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	path := stringParam(req, "path")
	path = s.defaultPath(path)

	engine, release, err := s.getEngine(path)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}
	defer release()

	var funcPattern *patterns.Pattern
	if fp := stringParam(req, "func_pattern"); fp != "" {
		funcPattern, err = patterns.Compile(fp)
		if err != nil {
			return errorResult("Invalid func_pattern: %v", err), nil
		}
	}

	tmpl := &templates.NilDeref{FuncPattern: funcPattern}
	findings := s.runTemplate(path, engine, tmpl)

	return s.findingsResult(findings), nil
}

func (s *Server) handleCheckPathProperty(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	fromPattern := stringParam(req, "from_pattern")
	targetPattern := stringParam(req, "target_pattern")
	property := stringParam(req, "property")
	excludePattern := stringParam(req, "exclude_pattern")
	path := stringParam(req, "path")

	path = s.defaultPath(path)

	engine, release, err := s.getEngine(path)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}
	defer release()

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

		case "invariant":
			// Check that from_pattern (safety predicate) holds at every block.
			violations := cfg.Invariant(g, g.Entry, fromPred)
			for _, v := range violations {
				findings = append(findings, templates.Finding{
					Kind:     "path_property",
					Severity: "warning",
					Message: fmt.Sprintf(
						"invariant '%s' violated in %s",
						fromPattern, fc.Func.Name.Name,
					),
					Location: engine.LocationOf(fc.Func.Pos()),
					Path:     v.Path,
				})
			}
		}
	}

	return jsonResult(map[string]any{
		"property":       property,
		"from_pattern":   fromPattern,
		"target_pattern": targetPattern,
		"total":          len(findings),
		"hits":           findings,
	}), nil
}

func (s *Server) handleApplyTransform(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	matchPat := stringParam(req, "match")
	replacePat := stringParam(req, "replace")
	path := stringParam(req, "path")
	preview := boolParamDefault(req, "preview", true)

	path = s.defaultPath(path)

	prog := newProgress(ctx, req)
	engine, release, err := s.getEngineWithProgress(path, prog)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}
	defer release()
	if prog.active() {
		prog.send(2, 3, "Applying transformation...")
	}

	rw, err := transform.NewRewriter(engine, matchPat, replacePat)
	if err != nil {
		return errorResult("Invalid patterns: %v", err), nil
	}

	var result *transform.Result
	if preview {
		result, err = rw.Preview()
	} else {
		s.invalidateEngine(path)
		result, err = rw.Apply()
	}
	if err != nil {
		return errorResult("Transform failed: %v", err), nil
	}

	return jsonResult(transformResultWithDiff(result)), nil
}

func (s *Server) handleMigrateAPI(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	path := stringParam(req, "path")
	preview := boolParamDefault(req, "preview", true)
	updateImports := boolParamDefault(req, "update_imports", true)

	path = s.defaultPath(path)

	// Parse rules from request.
	rulesRaw, ok := req.GetArguments()["rules"]
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

	engine, release, err := s.getEngine(path)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}
	defer release()

	migration := transform.NewAPIMigration(engine, rules, updateImports)

	var result *transform.Result
	if preview {
		result, err = migration.Preview()
	} else {
		s.invalidateEngine(path)
		result, err = migration.Apply()
	}
	if err != nil {
		return errorResult("Migration failed: %v", err), nil
	}

	return jsonResult(transformResultWithDiff(result)), nil
}

func (s *Server) handleAddMissingCleanup(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	acquirePattern := stringParam(req, "acquire_pattern")
	releaseMethod := stringParam(req, "release_method")
	style := stringParam(req, "cleanup_style")
	path := stringParam(req, "path")
	preview := boolParamDefault(req, "preview", true)

	path = s.defaultPath(path)

	engine, release, err := s.getEngine(path)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}
	defer release()

	inserter, err := transform.NewCleanupInserter(engine, acquirePattern, releaseMethod, style)
	if err != nil {
		return errorResult("Invalid parameters: %v", err), nil
	}

	var result *transform.Result
	if preview {
		result, err = inserter.Preview()
	} else {
		s.invalidateEngine(path)
		result, err = inserter.Apply()
	}
	if err != nil {
		return errorResult("Cleanup insertion failed: %v", err), nil
	}

	return jsonResult(transformResultWithDiff(result)), nil
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

	engine, err := analysis.NewEngineForFile(file)
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
		"hits":     result,
	}), nil
}

// --------------------------------------------------------------------------
// Phase 5: Pattern language handlers
// --------------------------------------------------------------------------

func (s *Server) handleRunPatterns(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	gclFile := stringParam(req, "gcl_file")
	path := stringParam(req, "path")
	ruleName := stringParam(req, "rule")
	path = s.defaultPath(path)

	engine, release, err := s.getEngine(path)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}
	defer release()

	// Determine which .gcl files to load.
	var gclPaths []string
	if gclFile != "" {
		gclPaths = []string{gclFile}
	} else {
		absPath, err := filepath.Abs(path)
		if err != nil {
			absPath = path
		}
		gclPaths = s.cfg.GCLPatterns(absPath)
	}

	if len(gclPaths) == 0 {
		return errorResult("No .gcl files specified. Provide gcl_file parameter or configure patterns in .caliper.yaml."), nil
	}

	var allFindings []map[string]any
	for _, gp := range gclPaths {
		loaded, err := gcl.Load(gp)
		if err != nil {
			return errorResult("Loading %s: %v", gp, err), nil
		}
		results := gcl.Run(loaded, engine)
		for _, r := range results {
			if ruleName != "" && r.RuleName != ruleName {
				continue
			}
			for _, f := range r.Findings {
				allFindings = append(allFindings, map[string]any{
					"rule":     r.RuleName,
					"gcl_file": gp,
					"severity": f.Severity,
					"message":  f.Message,
					"location": f.Location,
				})
			}
		}
	}

	return jsonResult(map[string]any{
		"total": len(allFindings),
		"hits":  allFindings,
	}), nil
}

// --------------------------------------------------------------------------
// Phase 4: Advanced analysis handlers
// --------------------------------------------------------------------------

func (s *Server) handleFindGoroutineLeak(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	path := stringParam(req, "path")
	path = s.defaultPath(path)

	engine, release, err := s.getEngine(path)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}
	defer release()

	tmpl := &templates.GoroutineLeak{}
	findings := s.runTemplate(path, engine, tmpl)
	return s.findingsResult(findings), nil
}

func (s *Server) handleFindSQLInjection(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	path := stringParam(req, "path")
	path = s.defaultPath(path)

	engine, release, err := s.getEngine(path)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}
	defer release()

	tmpl := &templates.SQLInjection{}
	findings := s.runTemplate(path, engine, tmpl)
	return s.findingsResult(findings), nil
}

func (s *Server) handleFindRaceCondition(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	path := stringParam(req, "path")
	path = s.defaultPath(path)

	engine, release, err := s.getEngine(path)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}
	defer release()

	tmpl := &templates.RaceCondition{}
	findings := s.runTemplate(path, engine, tmpl)
	return s.findingsResult(findings), nil
}

func (s *Server) handleFindContextMisuse(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	path := stringParam(req, "path")
	path = s.defaultPath(path)

	flagTodo := boolParamDefault(req, "flag_todo", true)

	engine, release, err := s.getEngine(path)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}
	defer release()

	tmpl := &templates.ContextMisuse{FlagTodo: flagTodo}
	findings := s.runTemplate(path, engine, tmpl)
	return s.findingsResult(findings), nil
}

func (s *Server) handleFindTaintFlow(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	sourceType := stringParam(req, "source_type")
	sinkFunction := stringParam(req, "sink_function")
	path := stringParam(req, "path")
	path = s.defaultPath(path)

	prog := newProgress(ctx, req)
	engine, release, err := s.getEngineWithProgress(path, prog)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}
	defer release()

	if prog.active() {
		prog.send(2, 4, "Building call graph...")
	}
	ipc, err := engine.InterProcedural()
	if err != nil {
		return errorResult("Failed to build call graph: %v", err), nil
	}
	if prog.active() {
		prog.send(3, 4, "Tracing taint flows...")
	}

	// Parse source type: "*net/http.Request" → package="net/http", type="Request"
	sourcePkg, sourceTypeName := parseQualifiedType(sourceType)

	// Parse sink function: "database/sql.Query" → package="database/sql", function="Query"
	sinkPkg, sinkFuncName := parseQualifiedName(sinkFunction)

	config := taint.TaintConfig{
		Sources: []taint.Source{{
			Package: sourcePkg,
			Type:    sourceTypeName,
		}},
		Sinks: []taint.Sink{{
			Package:  sinkPkg,
			Function: sinkFuncName,
			ArgIndex: -1, // any argument
		}},
	}

	tracker := taint.NewTracker(engine, ipc, config)
	flows := tracker.Analyze()

	var results []map[string]any
	for _, flow := range flows {
		if flow.Sanitized {
			continue
		}
		result := map[string]any{
			"source": flow.Source,
			"sink":   flow.Sink,
		}
		if len(flow.Path) > 0 {
			var steps []map[string]any
			for _, step := range flow.Path {
				steps = append(steps, map[string]any{
					"location":    step.Location,
					"kind":        step.Kind,
					"description": step.Description,
				})
			}
			result["path"] = steps
		}
		results = append(results, result)
	}

	return jsonResult(map[string]any{
		"source_type":   sourceType,
		"sink_function": sinkFunction,
		"total":         len(results),
		"hits":          results,
	}), nil
}

func (s *Server) handleFindCallSites(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	functionPattern := stringParam(req, "function_pattern")
	path := stringParam(req, "path")
	algoStr := stringParam(req, "algorithm")
	path = s.defaultPath(path)

	pat, err := patterns.Compile(functionPattern)
	if err != nil {
		return errorResult("Invalid function_pattern: %v", err), nil
	}

	engine, release, err := s.getEngine(path)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}
	defer release()

	algo := analysis.AlgoCHA
	switch algoStr {
	case "rta":
		algo = analysis.AlgoRTA
	case "vta":
		algo = analysis.AlgoVTA
	}

	ipc, err := engine.InterProceduralWith(algo)
	if err != nil {
		return errorResult("Failed to build call graph: %v", err), nil
	}

	matches := patterns.FindCallSitesByPattern(engine, ipc, pat)

	var hits []map[string]any
	for _, m := range matches {
		hits = append(hits, map[string]any{
			"loc": fmt.Sprintf("%s:%d", m.File, m.Line),
		})
	}

	return jsonResult(map[string]any{
		"function_pattern": functionPattern,
		"algorithm":        algoStr,
		"total":            len(hits),
		"hits":             hits,
	}), nil
}

func (s *Server) handleExplainFinding(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	file := stringParam(req, "file")
	line := intParam(req, "line")
	kind := stringParam(req, "kind")
	path := stringParam(req, "path")
	path = s.defaultPath(path)

	engine, release, err := s.getEngine(path)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}
	defer release()

	// Select the template based on kind.
	tmpl := s.templateForKind(kind)
	if tmpl == nil {
		return errorResult("Unknown finding kind: %s", kind), nil
	}

	// Run the template and find findings matching the location.
	allFindings := s.runTemplate(path, engine, tmpl)

	var matched *templates.Finding
	for i, f := range allFindings {
		if f.Location.Line == line && strings.HasSuffix(f.Location.File, file) {
			matched = &allFindings[i]
			break
		}
	}

	if matched == nil {
		return jsonResult(map[string]any{
			"found":   false,
			"file":    file,
			"line":    line,
			"kind":    kind,
			"message": fmt.Sprintf("No %s finding at %s:%d", kind, file, line),
		}), nil
	}

	// Translate CFG block path to source lines.
	var trace []map[string]any
	if len(matched.Path) > 0 {
		// Find the function containing the finding.
		for _, fc := range engine.FuncNodes() {
			funcPos := engine.Fset().Position(fc.Func.Pos())
			funcEnd := engine.Fset().Position(fc.Func.End())
			if !strings.HasSuffix(funcPos.Filename, file) {
				continue
			}
			if line < funcPos.Line || line > funcEnd.Line {
				continue
			}

			g := cfg.Build(engine.Fset(), fc.Func, fc.Info)
			blockMap := map[int]*cfg.Block{}
			for _, b := range g.Blocks {
				blockMap[b.ID] = b
			}

			for _, blockID := range matched.Path {
				block, ok := blockMap[blockID]
				if !ok {
					continue
				}
				step := map[string]any{
					"block": blockID,
				}
				if block.Pos.IsValid() {
					pos := engine.Fset().Position(block.Pos)
					step["loc"] = fmt.Sprintf("%s:%d", pos.Filename, pos.Line)
				}
				// Summarize what happens in this block.
				var stmtDescs []string
				for _, node := range block.Nodes {
					desc := printNode(engine.Fset(), node)
					if len(desc) > 80 {
						desc = desc[:77] + "..."
					}
					stmtDescs = append(stmtDescs, desc)
				}
				if len(stmtDescs) > 0 {
					step["code"] = stmtDescs
				}
				if block.Kind == cfg.BlockBranch {
					step["type"] = "branch"
				}
				trace = append(trace, step)
			}
			break
		}
	}

	result := map[string]any{
		"found": true,
		"kind":  matched.Kind,
		"sev":   matched.Severity,
		"msg":   matched.Message,
		"loc":   fmt.Sprintf("%s:%d:%d", matched.Location.File, matched.Location.Line, matched.Location.Column),
	}
	if matched.Suggestion != "" {
		result["fix"] = matched.Suggestion
	}
	if len(trace) > 0 {
		result["trace"] = trace
	}

	return jsonResult(result), nil
}

func (s *Server) templateForKind(kind string) templates.Template {
	switch kind {
	case "nil_deref":
		return &templates.NilDeref{}
	case "resource_leak":
		return nil // needs acquire/release params, handled separately
	case "unchecked_error":
		return &templates.UncheckedError{}
	case "lock_violation":
		return &templates.LockViolation{CheckBlockingUnderLock: true}
	case "blocking_under_lock":
		return &templates.LockViolation{CheckBlockingUnderLock: true}
	case "goroutine_leak":
		return &templates.GoroutineLeak{}
	case "race_condition":
		return &templates.RaceCondition{}
	case "context_misuse":
		return &templates.ContextMisuse{FlagTodo: true}
	case "defer_in_loop":
		return &templates.DeferInLoop{}
	case "error_swallow":
		return &templates.UncheckedError{Strict: true, KindName: "error_swallow"}
	case "slice_append_race":
		return &templates.SliceAppendRace{}
	case "sql_injection":
		return &templates.SQLInjection{}
	}
	return nil
}

func (s *Server) handleCheckInterface(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	typeName := stringParam(req, "type_name")
	ifaceName := stringParam(req, "interface_name")
	path := stringParam(req, "path")
	path = s.defaultPath(path)

	engine, release, err := s.getEngine(path)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}
	defer release()

	result := templates.CheckInterfaceSatisfaction(engine, typeName, ifaceName)
	return jsonResult(result), nil
}

func (s *Server) handleFindSliceAppendRace(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	path := stringParam(req, "path")
	path = s.defaultPath(path)

	engine, release, err := s.getEngine(path)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}
	defer release()

	tmpl := &templates.SliceAppendRace{}
	findings := s.runTemplate(path, engine, tmpl)
	return s.findingsResult(findings, req), nil
}

func (s *Server) handleFindErrorSwallow(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	path := stringParam(req, "path")
	path = s.defaultPath(path)

	engine, release, err := s.getEngine(path)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}
	defer release()

	// find_error_swallow is now an alias for find_unchecked_error in
	// strict mode (blank-id discards always flagged + value-use
	// severity escalation), with the kind renamed back to
	// "error_swallow" so clients that route by kind still work.
	tmpl := &templates.UncheckedError{
		Strict:        true,
		KindName:      "error_swallow",
		IncludeDefers: boolParam(req, "include_dropped"),
	}
	findings := s.runTemplate(path, engine, tmpl)
	return s.findingsResult(findings, req), nil
}

func (s *Server) handleFindDeferInLoop(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	path := stringParam(req, "path")
	path = s.defaultPath(path)

	engine, release, err := s.getEngine(path)
	if err != nil {
		return errorResult("Failed to load packages: %v", err), nil
	}
	defer release()

	tmpl := &templates.DeferInLoop{}
	findings := s.runTemplate(path, engine, tmpl)
	return s.findingsResult(findings, req), nil
}
