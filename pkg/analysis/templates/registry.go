package templates

import (
	"fmt"
	"go/token"
	"go/types"
	"runtime"
	"strings"
	"sync"

	"github.com/trendvidia/caliper/pkg/analysis"
	"github.com/trendvidia/caliper/pkg/analysis/cfg"
	"github.com/trendvidia/caliper/pkg/analysis/patterns"
)

// Finding represents a single issue found by a template.
type Finding struct {
	Kind        string                `json:"kind"`
	Severity    string                `json:"severity"` // "error", "warning", "info"
	Message     string                `json:"message"`
	Location    analysis.FileLocation `json:"location"`
	Explanation string                `json:"explanation"`
	Suggestion  string                `json:"suggestion,omitempty"`
	Path        []int                 `json:"path,omitempty"` // CFG block IDs for the violating path
}

// Template is the interface for all analysis templates.
type Template interface {
	// Name returns the template identifier.
	Name() string

	// Analyze runs the template against a function and returns findings.
	Analyze(ctx *TemplateContext) []Finding
}

// WholeProgramTemplate is the interface for analyses that need the whole
// program at once — cross-method rules that reason over a type's method
// set rather than a single function body (paired lifecycle calls,
// capture-vs-re-read). Distinct from Template because there is no single
// Func/CFG context to build.
type WholeProgramTemplate interface {
	// Name returns the template identifier.
	Name() string

	// AnalyzeProgram runs the template across all loaded packages.
	AnalyzeProgram(engine *analysis.Engine) []Finding
}

// RunWholeProgram runs a whole-program template and applies
// //caliper:ignore suppressions, mirroring RunTemplate's contract for
// per-function templates.
func RunWholeProgram(engine *analysis.Engine, tmpl WholeProgramTemplate) []Finding {
	return filterSuppressed(engine, tmpl.AnalyzeProgram(engine))
}

// TemplateContext provides everything a template needs.
type TemplateContext struct {
	Engine     *analysis.Engine
	Func       *analysis.FuncContext
	CFG        *cfg.Graph
	Pattern    *patterns.Pattern // optional, depends on the template
	Fset       *token.FileSet
	IPC        *analysis.InterProceduralContext // optional, nil for intra-procedural
	Infallible *InfallibleState                 // project-local infallibility config; nil = none

	// defsByName memoizes a name → *types.Var lookup over Func.Info.Defs.
	// Built lazily on first lookupIdent call so templates that don't use
	// it (the majority) don't pay for it. Populated/read only by helpers
	// running within a single TemplateContext, which is single-goroutine.
	defsByName map[string]types.Object
}

// BuildContext creates a TemplateContext for a function.
func BuildContext(engine *analysis.Engine, fc *analysis.FuncContext) *TemplateContext {
	return BuildContextWithConfig(engine, fc, nil)
}

// BuildContextWithConfig is like BuildContext but attaches project-local
// infallibility state for use by find_unchecked_error and related templates.
func BuildContextWithConfig(engine *analysis.Engine, fc *analysis.FuncContext, state *InfallibleState) *TemplateContext {
	g := cfg.Build(engine.Fset(), fc.Func, fc.Info)
	return &TemplateContext{
		Engine:     engine,
		Func:       fc,
		CFG:        g,
		Fset:       engine.Fset(),
		Infallible: state,
	}
}

// RunTemplate runs a template across all functions in the engine.
// Analysis of each function is parallelized for performance on large codebases.
func RunTemplate(engine *analysis.Engine, tmpl Template) []Finding {
	return RunTemplateWithConfig(engine, tmpl, nil)
}

// RunTemplateWithConfig is like RunTemplate but attaches project-local
// infallibility state to every function's TemplateContext. Pass state=nil
// for "no project-local config" (equivalent to RunTemplate).
func RunTemplateWithConfig(engine *analysis.Engine, tmpl Template, state *InfallibleState) []Finding {
	funcs := engine.FuncNodes()
	if len(funcs) <= 1 {
		if len(funcs) == 0 {
			return nil
		}
		ctx := BuildContextWithConfig(engine, funcs[0], state)
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

			ctx := BuildContextWithConfig(engine, fc, state)
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
