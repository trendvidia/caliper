package gcl

import (
	"fmt"
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/packages"

	"github.com/trendvidia/gocaliper/pkg/analysis"
	"github.com/trendvidia/gocaliper/pkg/analysis/patterns"
	"github.com/trendvidia/gocaliper/pkg/analysis/templates"
)

// RuleResult holds findings from running a single rule.
type RuleResult struct {
	RuleName string
	Findings []templates.Finding
}

// Run executes all rules in a loaded .gcl file against the engine.
func Run(loaded *LoadedGCL, engine *analysis.Engine) []RuleResult {
	var results []RuleResult
	for _, rule := range loaded.Rules {
		findings := runRule(rule, engine)
		if len(findings) > 0 {
			results = append(results, RuleResult{
				RuleName: rule.Name,
				Findings: findings,
			})
		}
	}
	return results
}

func runRule(rule ResolvedRule, engine *analysis.Engine) []templates.Finding {
	switch {
	case rule.AbsentPattern != nil:
		return runAbsenceRule(rule, engine)
	case rule.ContainsPattern != nil:
		return runContainsRule(rule, engine)
	default:
		return runSimpleRule(rule, engine)
	}
}

// runSimpleRule finds all matches of the match pattern.
func runSimpleRule(rule ResolvedRule, engine *analysis.Engine) []templates.Finding {
	var findings []templates.Finding
	engine.Walk(func(pkg *packages.Package, file *ast.File, node ast.Node, info *types.Info) bool {
		bindings := rule.MatchPattern.MatchNode(node, info)
		if bindings == nil {
			return true
		}

		loc := engine.LocationOf(node.Pos())
		findings = append(findings, templates.Finding{
			Kind:       rule.Name,
			Severity:   rule.Severity,
			Message:    formatMessage(rule.Message, rule.Description, *bindings),
			Location:   loc,
			Suggestion: rule.Description,
		})
		return true
	})
	return findings
}

// runAbsenceRule finds functions where the match pattern succeeds
// but the absent pattern does NOT appear in the same scope.
func runAbsenceRule(rule ResolvedRule, engine *analysis.Engine) []templates.Finding {
	neg := patterns.NewNegative(rule.AbsentPattern)
	var findings []templates.Finding

	for _, fc := range engine.FuncNodes() {
		if fc.Func.Body == nil {
			continue
		}

		// Check if the match pattern matches anywhere in this function.
		var matchFound bool
		ast.Inspect(fc.Func.Body, func(n ast.Node) bool {
			if n == nil || matchFound {
				return !matchFound
			}
			if rule.MatchPattern.MatchNode(n, fc.Info) != nil {
				matchFound = true
				return false
			}
			return true
		})

		if !matchFound {
			continue
		}

		// Check that the absent pattern is truly absent.
		if neg.NegateInScope(fc.Func.Body, fc.Info) {
			loc := engine.LocationOf(fc.Func.Pos())
			findings = append(findings, templates.Finding{
				Kind:     rule.Name,
				Severity: rule.Severity,
				Message:  formatMessage(rule.Message, rule.Description, nil),
				Location: loc,
			})
		}
	}
	return findings
}

// runContainsRule finds functions where the match pattern succeeds
// and the contains pattern also appears in the same scope.
func runContainsRule(rule ResolvedRule, engine *analysis.Engine) []templates.Finding {
	var findings []templates.Finding

	for _, fc := range engine.FuncNodes() {
		if fc.Func.Body == nil {
			continue
		}

		// Check if the match pattern matches anywhere.
		var matchFound bool
		ast.Inspect(fc.Func.Body, func(n ast.Node) bool {
			if n == nil || matchFound {
				return !matchFound
			}
			if rule.MatchPattern.MatchNode(n, fc.Info) != nil {
				matchFound = true
				return false
			}
			return true
		})

		if !matchFound {
			continue
		}

		// Check if the contains pattern also matches.
		var containsMatch bool
		ast.Inspect(fc.Func.Body, func(n ast.Node) bool {
			if n == nil || containsMatch {
				return !containsMatch
			}
			if rule.ContainsPattern.MatchNode(n, fc.Info) != nil {
				containsMatch = true
				return false
			}
			return true
		})

		if containsMatch {
			loc := engine.LocationOf(fc.Func.Pos())
			findings = append(findings, templates.Finding{
				Kind:     rule.Name,
				Severity: rule.Severity,
				Message:  formatMessage(rule.Message, rule.Description, nil),
				Location: loc,
			})
		}
	}
	return findings
}

// formatMessage returns the rule message, falling back to description.
func formatMessage(message, description string, bindings patterns.Bindings) string {
	if message != "" {
		result := message
		// Simple template substitution for $varname in messages.
		for name, b := range bindings {
			if ident, ok := b.Node.(*ast.Ident); ok {
				result = replaceAll(result, "$"+name, ident.Name)
			}
		}
		return result
	}
	if description != "" {
		return description
	}
	return "pattern match"
}

func replaceAll(s, old, new string) string {
	for {
		i := indexOf(s, old)
		if i < 0 {
			return s
		}
		// Don't replace if old is a prefix of a longer $identifier.
		end := i + len(old)
		if end < len(s) && (isAlpha(s[end]) || s[end] == '_') {
			// Part of a longer name — skip past it.
			s = s[:end] + s[end:]
			continue
		}
		s = s[:i] + new + s[end:]
	}
}

func indexOf(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func isAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// FormatFinding creates a human-readable string from a finding.
func FormatFinding(f templates.Finding) string {
	return fmt.Sprintf("%s:%d: [%s] %s (%s)",
		f.Location.File, f.Location.Line, f.Severity, f.Message, f.Kind)
}
