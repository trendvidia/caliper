// Package templates provides pre-built analysis templates that encapsulate
// complex control-flow properties behind simple, parameterized interfaces.
//
// This is how Caliper hides CTL complexity from the LLM. Each template
// knows which path quantifier to use, how to build predicates, and how to
// format results. The MCP tools call these directly.
package templates

import (
	"go/token"
	"runtime"
	"sync"

	"github.com/trendvidia/caliper/pkg/analysis"
	"github.com/trendvidia/caliper/pkg/analysis/cfg"
	"github.com/trendvidia/caliper/pkg/analysis/patterns"
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
	Pattern *patterns.Pattern                // optional, depends on the template
	Fset    *token.FileSet
	IPC     *analysis.InterProceduralContext // optional, nil for intra-procedural
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
// Analysis of each function is parallelized for performance on large codebases.
func RunTemplate(engine *analysis.Engine, tmpl Template) []Finding {
	funcs := engine.FuncNodes()
	if len(funcs) <= 1 {
		if len(funcs) == 0 {
			return nil
		}
		ctx := BuildContext(engine, funcs[0])
		return tmpl.Analyze(ctx)
	}

	var (
		mu          sync.Mutex
		allFindings []Finding
		wg          sync.WaitGroup
	)

	sem := make(chan struct{}, runtime.GOMAXPROCS(0))

	for _, fc := range funcs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			ctx := BuildContext(engine, fc)
			findings := tmpl.Analyze(ctx)

			if len(findings) > 0 {
				mu.Lock()
				allFindings = append(allFindings, findings...)
				mu.Unlock()
			}
		}()
	}

	wg.Wait()
	return allFindings
}
