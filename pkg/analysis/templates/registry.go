// Package templates provides pre-built analysis templates that encapsulate
// complex control-flow properties behind simple, parameterized interfaces.
//
// This is how GoCaliper hides CTL complexity from the LLM. Each template
// knows which path quantifier to use, how to build predicates, and how to
// format results. The MCP tools call these directly.
package templates

import (
	"go/token"

	"github.com/trendvidia/gocaliper/pkg/analysis"
	"github.com/trendvidia/gocaliper/pkg/analysis/cfg"
	"github.com/trendvidia/gocaliper/pkg/analysis/patterns"
)

// Finding represents a single issue found by a template.
type Finding struct {
	Kind        string              `json:"kind"`
	Severity    string              `json:"severity"` // "error", "warning", "info"
	Message     string              `json:"message"`
	Location    analysis.FileLocation `json:"location"`
	Explanation string              `json:"explanation"`
	Suggestion  string              `json:"suggestion,omitempty"`
	Path        []int               `json:"path,omitempty"` // CFG block IDs for the violating path
}

// Template is the interface for all analysis templates.
type Template interface {
	// Name returns the template identifier.
	Name() string

	// Analyze runs the template against a function and returns findings.
	Analyze(ctx *TemplateContext) []Finding
}

// TemplateContext provides everything a template needs.
type TemplateContext struct {
	Engine  *analysis.Engine
	Func    *analysis.FuncContext
	CFG     *cfg.Graph
	Pattern *patterns.Pattern // optional, depends on the template
	Fset    *token.FileSet
}

// BuildContext creates a TemplateContext for a function.
func BuildContext(engine *analysis.Engine, fc *analysis.FuncContext) *TemplateContext {
	g := cfg.Build(engine.Fset(), fc.Func, fc.Info)
	return &TemplateContext{
		Engine: engine,
		Func:   fc,
		CFG:    g,
		Fset:   engine.Fset(),
	}
}

// RunTemplate runs a template across all functions in the engine.
func RunTemplate(engine *analysis.Engine, tmpl Template) []Finding {
	var allFindings []Finding
	for _, fc := range engine.FuncNodes() {
		ctx := BuildContext(engine, fc)
		findings := tmpl.Analyze(ctx)
		allFindings = append(allFindings, findings...)
	}
	return allFindings
}
