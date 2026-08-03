package templates

// StaleCapture: capture-at-create vs re-read-at-refresh (issue #8).
//
// A renderer that copies a widget's exported field in CreateRenderer
// ("r := &renderer{segments: w.Segments}") must re-read that field in
// Refresh — otherwise user mutations of the field after creation are
// invisible until the renderer is rebuilt. The general form: a field of
// type T read in T's capture method (into an assignment or composite
// literal) must be read again in the re-read method of T or of a
// companion type constructed by the capture method.

import (
	"fmt"
	"go/ast"
	"go/types"
	"strings"

	"github.com/trendvidia/caliper/pkg/analysis"
)

// StaleCapture finds receiver fields captured in one method and never
// re-read in another, over a type's method set and its companions.
type StaleCapture struct {
	// CaptureMethod is the method where fields are copied out
	// (e.g. "CreateRenderer").
	CaptureMethod string

	// RereadMethod is the method that must re-read captured fields
	// (e.g. "Refresh"). Looked up on the receiver type and on companion
	// types constructed inside CaptureMethod. Types where no such
	// method exists are skipped entirely.
	RereadMethod string

	// ExportedOnly restricts the check to exported fields — the ones
	// users of the type can mutate after creation. NewStaleCapture
	// defaults it to true.
	ExportedOnly bool

	// KindName overrides the finding kind (default "stale_capture").
	KindName string
}

// NewStaleCapture builds a StaleCapture with the exported-fields-only
// default.
func NewStaleCapture(captureMethod, rereadMethod string) (*StaleCapture, error) {
	if captureMethod == "" || rereadMethod == "" {
		return nil, fmt.Errorf("both capture_method and reread_method are required")
	}
	return &StaleCapture{
		CaptureMethod: captureMethod,
		RereadMethod:  rereadMethod,
		ExportedOnly:  true,
	}, nil
}

// Name returns the finding kind.
func (s *StaleCapture) Name() string {
	if s.KindName != "" {
		return s.KindName
	}
	return "stale_capture"
}

// capturedField pairs a field with its first capture site.
type capturedField struct {
	field *types.Var
	site  ast.Node
}

// AnalyzeProgram runs the capture-vs-re-read rule over every type that
// declares the capture method.
func (s *StaleCapture) AnalyzeProgram(engine *analysis.Engine) []Finding {
	sets := engine.MethodSets()
	var findings []Finding
	for tn, methods := range sets {
		for _, fc := range methods {
			if fc.Func.Name.Name != s.CaptureMethod || fc.Func.Body == nil {
				continue
			}
			findings = append(findings, s.analyzeCapture(engine, sets, tn, methods, fc)...)
		}
	}
	sortFindings(findings)
	return findings
}

func (s *StaleCapture) analyzeCapture(
	engine *analysis.Engine,
	sets map[*types.TypeName][]*analysis.FuncContext,
	tn *types.TypeName,
	methods []*analysis.FuncContext,
	captureFC *analysis.FuncContext,
) []Finding {
	captures := s.capturedFields(captureFC, tn)
	if len(captures) == 0 {
		return nil
	}

	// Re-read scope: RereadMethod on the receiver type itself and on
	// companion types constructed inside the capture method.
	var rereads []*analysis.FuncContext
	var rereadNames []string
	addRereads := func(owner *types.TypeName, ms []*analysis.FuncContext) {
		for _, m := range ms {
			if m.Func.Name.Name == s.RereadMethod && m.Func.Body != nil {
				rereads = append(rereads, m)
				rereadNames = append(rereadNames, owner.Name()+"."+s.RereadMethod)
			}
		}
	}
	addRereads(tn, methods)
	for _, ct := range constructedTypes(captureFC) {
		if ct != tn {
			addRereads(ct, sets[ct])
		}
	}
	// No re-read method in scope means the lifecycle shape this rule
	// checks doesn't apply to this type — skip rather than flag every
	// capture.
	if len(rereads) == 0 {
		return nil
	}

	var findings []Finding
	for _, c := range captures {
		if fieldReadInAny(rereads, c.field) {
			continue
		}
		findings = append(findings, Finding{
			Kind:     s.Name(),
			Severity: "warning",
			Message: fmt.Sprintf("field %s.%s captured in %s.%s is never re-read in %s",
				tn.Name(), c.field.Name(), tn.Name(), s.CaptureMethod,
				strings.Join(rereadNames, " or ")),
			Location: engine.LocationOf(c.site.Pos()),
			Explanation: fmt.Sprintf(
				"%s.%s copies the value of the field %s when it runs. Because %s never reads "+
					"the field again, mutations of %s.%s after creation are invisible until %s runs again.",
				tn.Name(), s.CaptureMethod, c.field.Name(),
				strings.Join(rereadNames, "/"), tn.Name(), c.field.Name(), s.CaptureMethod),
			Suggestion: fmt.Sprintf("re-read %s.%s inside %s instead of relying on the captured copy",
				tn.Name(), c.field.Name(), strings.Join(rereadNames, "/")),
		})
	}
	return findings
}

// capturedFields collects fields of tn that the capture method copies
// into an assignment or composite literal. Deduped by field, keeping the
// first capture site, in source order.
func (s *StaleCapture) capturedFields(fc *analysis.FuncContext, tn *types.TypeName) []capturedField {
	var out []capturedField
	seen := map[*types.Var]bool{}

	collect := func(root ast.Expr) {
		ast.Inspect(root, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			field := selectsFieldOf(sel, fc.Info, tn)
			if field == nil || seen[field] {
				return true
			}
			if s.ExportedOnly && !field.Exported() {
				return true
			}
			seen[field] = true
			out = append(out, capturedField{field: field, site: sel})
			return true
		})
	}

	ast.Inspect(fc.Func.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			for _, rhs := range node.Rhs {
				collect(rhs)
			}
		case *ast.ValueSpec:
			for _, v := range node.Values {
				collect(v)
			}
		case *ast.CompositeLit:
			for _, elt := range node.Elts {
				collect(elt)
			}
		}
		return true
	})
	return out
}

// fieldReadInAny reports whether any of the methods reads the given
// field.
func fieldReadInAny(methods []*analysis.FuncContext, field *types.Var) bool {
	for _, fc := range methods {
		found := false
		ast.Inspect(fc.Func.Body, func(n ast.Node) bool {
			if found {
				return false
			}
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if fc.Info.Uses[sel.Sel] == field {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}
