package templates

// PairedCalls: type-scoped paired-lifecycle rule (issue #8).
//
// The property "Animation.Start() in one method requires a reachable
// Stop() in a sibling method or the renderer's Destroy" is not a
// within-one-function path property — the two halves live in different
// methods. This template scopes the rule to a type's whole method set:
// if the First pattern matches in any method of type T, the Second
// pattern must match somewhere in T's methods, or (by default) in the
// methods of a companion type that T's methods construct (e.g. the
// renderer a widget returns from CreateRenderer).
//
// When both patterns share a metavariable and the First match binds it
// to a field of the receiver (w.anim in "$x.Start()"), pairing prefers
// field identity: a Second match on the same field pairs it. To keep
// signal high, a Second match whose binding cannot be resolved to a
// field (a local alias, a parameter) still counts as pairing — only a
// Second that provably targets a *different* field does not.

import (
	"fmt"
	"go/ast"
	"go/types"
	"sort"

	"github.com/trendvidia/caliper/pkg/analysis"
	"github.com/trendvidia/caliper/pkg/analysis/patterns"
)

// PairedCalls checks that a match of First in any method of a type is
// paired with a match of Second somewhere in the type's method set.
type PairedCalls struct {
	First  *patterns.Pattern
	Second *patterns.Pattern

	// IncludeConstructed extends the search scope for Second to types
	// constructed via composite literals inside the receiver type's
	// methods (the widget→renderer link). NewPairedCalls defaults it
	// to true.
	IncludeConstructed bool

	// KindName overrides the finding kind (default "unpaired_call").
	KindName string
}

// NewPairedCalls compiles the two pattern strings into a PairedCalls
// template with companion-type search enabled.
func NewPairedCalls(first, second string) (*PairedCalls, error) {
	fp, err := patterns.Compile(first)
	if err != nil {
		return nil, fmt.Errorf("first pattern: %w", err)
	}
	sp, err := patterns.Compile(second)
	if err != nil {
		return nil, fmt.Errorf("second pattern: %w", err)
	}
	return &PairedCalls{First: fp, Second: sp, IncludeConstructed: true}, nil
}

// Name returns the finding kind.
func (p *PairedCalls) Name() string {
	if p.KindName != "" {
		return p.KindName
	}
	return "unpaired_call"
}

// AnalyzeProgram runs the pairing rule over every type's method set.
func (p *PairedCalls) AnalyzeProgram(engine *analysis.Engine) []Finding {
	sets := engine.MethodSets()
	var findings []Finding
	for tn, methods := range sets {
		findings = append(findings, p.analyzeType(engine, sets, tn, methods)...)
	}
	sortFindings(findings)
	return findings
}

func (p *PairedCalls) analyzeType(
	engine *analysis.Engine,
	sets map[*types.TypeName][]*analysis.FuncContext,
	tn *types.TypeName,
	methods []*analysis.FuncContext,
) []Finding {
	var firstMatches []methodMatch
	for _, fc := range methods {
		firstMatches = append(firstMatches, matchesInMethod(fc, p.First)...)
	}
	if len(firstMatches) == 0 {
		return nil
	}

	// The Second search scope: T's own methods, plus the method sets of
	// companion types constructed inside T's methods.
	scope := methods
	scopeDesc := tn.Name()
	if p.IncludeConstructed {
		seen := map[*types.TypeName]bool{tn: true}
		for _, fc := range methods {
			for _, ct := range constructedTypes(fc) {
				if seen[ct] || sets[ct] == nil {
					continue
				}
				seen[ct] = true
				scope = append(scope, sets[ct]...)
				scopeDesc += ", " + ct.Name()
			}
		}
	}

	shared := sharedMetavars(p.First, p.Second)

	var secondAnchors []types.Object // one entry per Second match; nil = unresolved
	for _, fc := range scope {
		for _, m := range matchesInMethod(fc, p.Second) {
			secondAnchors = append(secondAnchors, fieldAnchor(m, shared))
		}
	}

	var findings []Finding
	for _, fm := range firstMatches {
		anchor := fieldAnchor(fm, shared)
		paired := false
		for _, sa := range secondAnchors {
			// Field-identity pairing when both sides resolve; lenient
			// when either side is unresolvable.
			if anchor == nil || sa == nil || anchor == sa {
				paired = true
				break
			}
		}
		if paired {
			continue
		}

		target := fmt.Sprintf("%q", p.Second.String())
		if anchor != nil {
			target = fmt.Sprintf("%q on field %s", p.Second.String(), anchor.Name())
		}
		findings = append(findings, Finding{
			Kind:     p.Name(),
			Severity: "warning",
			Message: fmt.Sprintf("%s in %s.%s has no paired match of %s in the method set of %s",
				nodeText(engine, fm.node), tn.Name(), fm.fc.Func.Name.Name, target, scopeDesc),
			Location: engine.LocationOf(fm.node.Pos()),
			Explanation: fmt.Sprintf(
				"Pattern %q matched in a method of %s, but pattern %q does not match in any method of %s. "+
					"For lifecycle pairs (Start/Stop, Open/Close, Subscribe/Unsubscribe) this usually means "+
					"the started resource outlives the type because no teardown method releases it.",
				p.First.String(), tn.Name(), p.Second.String(), scopeDesc),
			Suggestion: fmt.Sprintf("add a call matching %q in a teardown method of %s (or one of its companion types)",
				p.Second.String(), tn.Name()),
		})
	}
	return findings
}

// sharedMetavars returns the metavariable names declared in both
// patterns (excluding the wildcard), sorted for determinism.
func sharedMetavars(a, b *patterns.Pattern) []string {
	var shared []string
	bMeta := b.Meta()
	for name := range a.Meta() {
		if name == "_" {
			continue
		}
		if _, ok := bMeta[name]; ok {
			shared = append(shared, name)
		}
	}
	sort.Strings(shared)
	return shared
}

// fieldAnchor resolves a match to the receiver-field object one of the
// shared metavariables is bound to, or nil when no binding is a field
// selector. Field identity lets "$x.Start()" in Widget.Show pair with
// "$x.Stop()" in Widget.Hide only when both x's are the same field.
func fieldAnchor(m methodMatch, shared []string) types.Object {
	for _, name := range shared {
		b, ok := m.bindings[name]
		if !ok || b == nil {
			continue
		}
		sel, ok := b.Node.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		if v, ok := m.fc.Info.Uses[sel.Sel].(*types.Var); ok && v.IsField() {
			return v
		}
	}
	return nil
}
