package mcp

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"

	"github.com/trendvidia/caliper/pkg/analysis"
	"github.com/trendvidia/caliper/pkg/analysis/templates"
	"github.com/trendvidia/caliper/pkg/transform"
)

// Finding filtering, severity overrides, and the four output formats
// (compact / summary / grouped / sarif). Plus the small result-building
// helpers (jsonResult, errorResult, compactLoc) used by every handler.

// jsonResult marshals v to JSON and wraps it in a TextContent result.
// On marshal error returns an errorResult so the failure surfaces to the
// MCP client.
func jsonResult(v any) *mcpsdk.CallToolResult {
	data, err := json.Marshal(v)
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

// compactLoc formats a FileLocation as a "file:line:col" string.
func compactLoc(loc analysis.FileLocation) string {
	return fmt.Sprintf("%s:%d:%d", loc.File, loc.Line, loc.Column)
}

// errorResult returns an MCP result flagged as an error, with a
// printf-style formatted message body.
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

// filterFindings applies max_findings and min_severity limits from the
// request's args.
func filterFindings(findings []templates.Finding, req mcpsdk.CallToolRequest) []templates.Finding {
	if minSev := stringParam(req, "min_severity"); minSev != "" {
		sevOrder := map[string]int{"info": 0, "warning": 1, "error": 2}
		minLevel := sevOrder[minSev]
		var filtered []templates.Finding
		for _, f := range findings {
			if sevOrder[f.Severity] >= minLevel {
				filtered = append(filtered, f)
			}
		}
		findings = filtered
	}
	if max := intParam(req, "max_findings"); max > 0 && len(findings) > max {
		findings = findings[:max]
	}
	return findings
}

// applySeverityOverrides re-stamps each finding's severity from the
// configured per-rule overrides in .caliper.pxf.
func (s *Server) applySeverityOverrides(findings []templates.Finding) []templates.Finding {
	if s.cfg == nil {
		return findings
	}
	for i := range findings {
		findings[i].Severity = s.cfg.SeverityFor(findings[i].Kind, findings[i].Severity)
	}
	return findings
}

// compactFinding is a token-efficient representation of a finding for MCP output.
type compactFinding struct {
	Loc   string `json:"loc"`
	Sev   string `json:"sev,omitempty"`
	Msg   string `json:"msg"`
	Fix   string `json:"fix,omitempty"`
	Count int    `json:"count,omitempty"` // >1 when consecutive identical findings are grouped
}

// findingsResult builds a JSON result from findings, applying config
// overrides. Format modes via the optional request:
//
//   - "compact" (default): short keys, relative paths, flat list
//   - "summary": counts by severity only, no individual findings
//   - "grouped": findings grouped by file for readability
//   - "sarif": SARIF v2.1.0 for GitHub Code Scanning
func (s *Server) findingsResult(findings []templates.Finding, req ...mcpsdk.CallToolRequest) *mcpsdk.CallToolResult {
	findings = s.applySeverityOverrides(findings)

	var format string
	if len(req) > 0 {
		findings = filterFindings(findings, req[0])
		format = stringParam(req[0], "format")
	}

	switch format {
	case "summary":
		return s.findingsSummary(findings)
	case "grouped":
		return s.findingsGrouped(findings)
	case "sarif":
		return s.findingsSARIF(findings)
	default:
		return s.findingsCompact(findings)
	}
}

func (s *Server) findingsSummary(findings []templates.Finding) *mcpsdk.CallToolResult {
	counts := map[string]int{}
	for _, f := range findings {
		counts[f.Severity]++
	}
	return jsonResult(map[string]any{
		"total":       len(findings),
		"by_severity": counts,
	})
}

func (s *Server) findingsGrouped(findings []templates.Finding) *mcpsdk.CallToolResult {
	root := findCommonRoot(findings)
	groups := map[string][]compactFinding{}
	for _, f := range findings {
		relFile := f.Location.File
		if root != "" {
			relFile = strings.TrimPrefix(f.Location.File, root+"/")
		}
		cf := compactFinding{
			Loc: fmt.Sprintf(":%d:%d", f.Location.Line, f.Location.Column),
			Sev: f.Severity,
			Msg: f.Message,
			Fix: f.Suggestion,
		}
		groups[relFile] = append(groups[relFile], cf)
	}
	result := map[string]any{
		"total": len(findings),
		"files": groups,
	}
	if root != "" {
		result["root"] = root
	}
	return jsonResult(result)
}

func (s *Server) findingsSARIF(findings []templates.Finding) *mcpsdk.CallToolResult {
	// Build SARIF v2.1.0 output for GitHub Code Scanning.
	// https://docs.oasis-open.org/sarif/sarif/v2.1.0/sarif-v2.1.0.html

	// Collect unique rule IDs.
	ruleMap := map[string]int{}
	var rules []map[string]any
	for _, f := range findings {
		if _, ok := ruleMap[f.Kind]; !ok {
			ruleMap[f.Kind] = len(rules)
			rule := map[string]any{
				"id": f.Kind,
				"shortDescription": map[string]any{
					"text": f.Kind,
				},
			}
			rules = append(rules, rule)
		}
	}

	sevMap := map[string]string{
		"error":   "error",
		"warning": "warning",
		"info":    "note",
	}

	var results []map[string]any
	for _, f := range findings {
		level := sevMap[f.Severity]
		if level == "" {
			level = "warning"
		}

		result := map[string]any{
			"ruleId":  f.Kind,
			"level":   level,
			"message": map[string]any{"text": f.Message},
			"locations": []map[string]any{{
				"physicalLocation": map[string]any{
					"artifactLocation": map[string]any{
						"uri": f.Location.File,
					},
					"region": map[string]any{
						"startLine":   f.Location.Line,
						"startColumn": f.Location.Column,
					},
				},
			}},
		}
		if f.Suggestion != "" {
			result["fixes"] = []map[string]any{{
				"description": map[string]any{"text": f.Suggestion},
			}}
		}
		results = append(results, result)
	}

	sarif := map[string]any{
		"version": "2.1.0",
		"$schema": "https://raw.githubusercontent.com/oasis-tcs/sarif-spec/main/sarif-2.1/schema/sarif-schema-2.1.0.json",
		"runs": []map[string]any{{
			"tool": map[string]any{
				"driver": map[string]any{
					"name":    "Caliper",
					"version": "1.0.6",
					"rules":   rules,
				},
			},
			"results": results,
		}},
	}

	return jsonResult(sarif)
}

func (s *Server) findingsCompact(findings []templates.Finding) *mcpsdk.CallToolResult {
	root := findCommonRoot(findings)

	var compact []compactFinding
	for _, f := range findings {
		relFile := f.Location.File
		if root != "" {
			relFile = strings.TrimPrefix(f.Location.File, root+"/")
		}
		cf := compactFinding{
			Loc: fmt.Sprintf("%s:%d:%d", relFile, f.Location.Line, f.Location.Column),
			Sev: f.Severity,
			Msg: f.Message,
			Fix: f.Suggestion,
		}

		// Group consecutive findings with the same message and file.
		if len(compact) > 0 {
			prev := &compact[len(compact)-1]
			if prev.Msg == cf.Msg && sameFile(prev.Loc, cf.Loc) {
				if prev.Count == 0 {
					prev.Count = 2
				} else {
					prev.Count++
				}
				// Extend loc to show range: "file.go:106:3-120"
				prev.Loc = extendLocRange(prev.Loc, f.Location.Line)
				continue
			}
		}
		compact = append(compact, cf)
	}

	result := map[string]any{
		"total": len(findings),
		"hits":  compact,
	}
	if root != "" {
		result["root"] = root
	}
	return jsonResult(result)
}

// sameFile checks if two compact loc strings refer to the same file.
func sameFile(loc1, loc2 string) bool {
	file1 := loc1[:strings.IndexByte(loc1, ':')]
	file2 := loc2[:strings.IndexByte(loc2, ':')]
	return file1 == file2
}

// extendLocRange updates a loc string to show a line range.
// "file.go:106:3" + line 120 → "file.go:106-120:3"
func extendLocRange(loc string, endLine int) string {
	parts := strings.SplitN(loc, ":", 3)
	if len(parts) < 2 {
		return loc
	}
	startLine := parts[1]
	if idx := strings.IndexByte(startLine, '-'); idx >= 0 {
		startLine = startLine[:idx]
	}
	if len(parts) == 3 {
		return fmt.Sprintf("%s:%s-%d:%s", parts[0], startLine, endLine, parts[2])
	}
	return fmt.Sprintf("%s:%s-%d", parts[0], startLine, endLine)
}

// findCommonRoot extracts the longest common directory prefix from finding locations.
func findCommonRoot(findings []templates.Finding) string {
	if len(findings) == 0 {
		return ""
	}
	root := filepath.Dir(findings[0].Location.File)
	for _, f := range findings[1:] {
		dir := filepath.Dir(f.Location.File)
		for !strings.HasPrefix(dir, root) {
			parent := filepath.Dir(root)
			if parent == root {
				return ""
			}
			root = parent
		}
	}
	return root
}

// transformResultWithDiff wraps a transform.Result, adding unified diffs
// so that LLMs can review changes before applying.
func transformResultWithDiff(r *transform.Result) map[string]any {
	m := map[string]any{
		"changes":        r.Changes,
		"files_affected": r.Files,
		"total_changes":  r.Total,
		"preview":        r.Preview,
	}
	if len(r.Changes) > 0 {
		m["diffs"] = r.UnifiedDiff(3)
	}
	return m
}
