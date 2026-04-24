// Package templates provides pre-built analysis templates that encapsulate
// complex control-flow properties behind simple, parameterized interfaces.
//
// This is how Caliper hides CTL complexity from the LLM. Each template
// knows which path quantifier to use, how to build predicates, and how to
// format results. The MCP tools call these directly.
package templates

import (
	"fmt"
	"go/token"
	"runtime"
	"strings"
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
		return filterSuppressed(engine, tmpl.Analyze(ctx))
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

			// Recover from panics in individual function analysis so a
			// single bad function doesn't crash the entire server.
			defer func() {
				if r := recover(); r != nil {
					mu.Lock()
					allFindings = append(allFindings, Finding{
						Kind:     tmpl.Name(),
						Severity: "error",
						Message:  fmt.Sprintf("internal error analyzing function: %v", r),
						Location: engine.LocationOf(fc.Func.Pos()),
					})
					mu.Unlock()
				}
			}()

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

	// Filter out suppressed findings (//caliper:ignore).
	allFindings = filterSuppressed(engine, allFindings)

	return allFindings
}

// filterSuppressed removes findings on lines that have a //caliper:ignore comment.
// Supports rule-specific suppression: //caliper:ignore nil_deref
func filterSuppressed(engine *analysis.Engine, findings []Finding) []Finding {
	if len(findings) == 0 {
		return findings
	}

	// Build a set of suppressed locations: file:line → set of suppressed rules (empty = all).
	type suppression struct {
		rules map[string]bool // empty means suppress all
	}
	suppressions := map[string]*suppression{}

	for _, pkg := range engine.Packages() {
		for _, file := range pkg.Syntax {
			for _, cg := range file.Comments {
				for _, c := range cg.List {
					text := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
					if !strings.HasPrefix(text, "caliper:ignore") {
						continue
					}

					pos := engine.Fset().Position(c.Pos())
					key := fmt.Sprintf("%s:%d", pos.Filename, pos.Line)

					sup := &suppression{rules: map[string]bool{}}
					rest := strings.TrimSpace(strings.TrimPrefix(text, "caliper:ignore"))
					if rest != "" {
						for _, rule := range strings.Fields(rest) {
							sup.rules[rule] = true
						}
					}
					suppressions[key] = sup
				}
			}
		}
	}

	if len(suppressions) == 0 {
		return findings
	}

	var filtered []Finding
	for _, f := range findings {
		key := fmt.Sprintf("%s:%d", f.Location.File, f.Location.Line)
		if sup, ok := suppressions[key]; ok {
			if len(sup.rules) == 0 || sup.rules[f.Kind] {
				continue // suppressed
			}
		}
		filtered = append(filtered, f)
	}
	return filtered
}
