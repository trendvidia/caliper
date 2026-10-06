// Inter-procedural analysis context for Caliper.
//
// This file provides the call graph infrastructure needed for cross-function
// analysis: taint tracking, call-site matching, and rule chaining. The
// InterProceduralContext is built once from an Engine and cached for reuse
// across multiple tool calls.
package analysis

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"slices"
	"strings"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/cha"
	"golang.org/x/tools/go/callgraph/rta"
	"golang.org/x/tools/go/callgraph/vta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// CallGraphAlgorithm selects the call graph construction algorithm.
type CallGraphAlgorithm int

const (
	// AlgoCHA uses Class Hierarchy Analysis — fastest, works on library code
	// (no main required), but least precise for interface calls.
	AlgoCHA CallGraphAlgorithm = iota

	// AlgoRTA uses Rapid Type Analysis — more precise than CHA but requires
	// a main function as a root. Falls back to CHA if no main is found.
	AlgoRTA

	// AlgoVTA uses Variable Type Analysis — most precise, handles interface
	// calls well, but slowest. Works on library code.
	AlgoVTA
)

// InterProceduralContext holds the SSA program and call graph for
// cross-function analyses. It is built once from an Engine and shared
// across all inter-procedural templates and tools.
type InterProceduralContext struct {
	Engine    *Engine
	Program   *ssa.Program
	SSAPkgs   []*ssa.Package
	CallGraph *callgraph.Graph
	Algorithm CallGraphAlgorithm

	// Skipped lists the packages of [Engine.Packages] that are not in
	// Program, in load order. Each type-checked cleanly, but imports,
	// possibly transitively, a package with an error outside a function
	// body, and SSA cannot be built on declarations the type checker did
	// not finish; see createProgram. Such a package has no functions in
	// Program and no edges in CallGraph.
	Skipped []SkippedPackage
}

// SkippedPackage is a loaded package left out of the SSA program, and why.
type SkippedPackage struct {
	Package *packages.Package
	// Cause is the package whose error put Package out of reach: the
	// first broken import on the path from Package, which has an error
	// outside a function body itself. It may be a package of
	// [Engine.Failed] or a dependency outside the load's patterns.
	Cause *packages.Package
}

// CallSite describes a location where a function is called.
type CallSite struct {
	Caller   *ssa.Function
	Callee   *ssa.Function
	Position FileLocation
	IsGo     bool // true if this is a `go` statement
	IsDefer  bool // true if this is a `defer` statement
}

// BuildInterProceduralContext constructs the SSA program and call graph
// from an Engine. The call graph algorithm can be selected; CHA is the
// default and works on library code without a main function.
func BuildInterProceduralContext(engine *Engine, algo CallGraphAlgorithm) (ipc *InterProceduralContext, err error) {
	// Build SSA program from all loaded packages.
	prog, ssaPkgs, skipped := createProgram(engine, ssa.SanityCheckFunctions)

	// prog.Build() panics on an inconsistent program — most often
	// "unsatisfied import: Program.CreatePackage(X) was not called" when a
	// dependency was never loaded. That panic fires inside a goroutine the
	// builder spawns per package, so a deferred recover() here CANNOT catch
	// it: the process dies, and an MCP server dies with it. The only way to
	// keep a malformed workspace from ending the session is to refuse to
	// build, so check the precondition first and report which import is
	// missing.
	if err := checkSSAImports(prog, ssaPkgs); err != nil {
		return nil, err
	}
	prog.Build()

	var nonNil []*ssa.Package
	for _, p := range ssaPkgs {
		if p != nil {
			nonNil = append(nonNil, p)
		}
	}
	if len(nonNil) == 0 {
		return nil, fmt.Errorf("no SSA packages built")
	}

	// Build call graph using the selected algorithm.
	cg, usedAlgo, err := buildCallGraph(prog, nonNil, algo)
	if err != nil {
		return nil, fmt.Errorf("building call graph: %w", err)
	}

	return &InterProceduralContext{
		Engine:    engine,
		Program:   prog,
		SSAPkgs:   nonNil,
		CallGraph: cg,
		Algorithm: usedAlgo,
		Skipped:   skipped,
	}, nil
}

// createProgram creates the SSA program and a package for everything the
// load reached, and decides which of them are analysed.
//
// ssautil.AllPackages skips every package go/packages marks IllTyped,
// and IllTyped propagates: one dependency with an error removes every
// loaded package that imports it, transitively, from the program —
// silently, since the loaded package's own type-check was clean and it
// is still in Engine.Packages(). On steward one grpc package with a type
// error inside a function body took 36 of 117 loaded packages with it
// (#30). A function body cannot change a package's declarations, and a
// dependency's declarations are all its importers need, so this does not
// read IllTyped:
//
//   - a package is BROKEN when it has an error outside a function body —
//     a parse error, a type error in a declaration, export data that
//     could not be read — or imports a broken package, transitively.
//     Broken packages are skipped, and so is everything importing them,
//     which is what IllTyped does for every error;
//   - a package with errors only inside function bodies is created from
//     its types alone, so its declarations are there for its importers
//     and its bodies, which the type checker did not finish, never reach
//     the SSA builder (which has no guard against an AST the checker did
//     not visit);
//   - under EngineOptions.DepsFromExportData every dependency is created
//     from types alone, whatever its origin: one read from export data
//     has no syntax, and one go/packages fell back to type-checking from
//     source because `go list -export` could not compile it had its
//     bodies skipped. The driver's compile error on such a package is a
//     ListError and says nothing about its types, so ListError never
//     counts; a listing failure that matters surfaces as a type error in
//     the importer.
//   - everything else is created from its syntax: the loaded packages,
//     and under the default load the dependencies too.
//
// It also returns the loaded packages it skipped, each with the package
// whose error caused it.
func createProgram(engine *Engine, mode ssa.BuilderMode) (*ssa.Program, []*ssa.Package, []SkippedPackage) {
	prog := ssa.NewProgram(engine.fset, mode)
	roots := make(map[*packages.Package]bool, len(engine.pkgs))
	for _, p := range engine.pkgs {
		roots[p] = true
	}
	// broken maps a broken package to the package with a declaration
	// error that broke it, itself if it has one.
	broken := make(map[*packages.Package]*packages.Package)
	// Visit runs post-order, so a package's imports are classified
	// before it is. Imports is a map; its keys are sorted so the cause
	// reported for a package with several broken imports is stable.
	packages.Visit(engine.pkgs, nil, func(p *packages.Package) {
		var cause *packages.Package
		if hasDeclarationError(p) {
			cause = p
		} else {
			for _, path := range slices.Sorted(maps.Keys(p.Imports)) {
				if c := broken[p.Imports[path]]; c != nil {
					cause = c
					break
				}
			}
		}
		if cause != nil {
			broken[p] = cause
			return
		}
		if p.Types == nil {
			return
		}
		typesOnly := hasNonDriverError(p) || (engine.opts.DepsFromExportData && !roots[p])
		if typesOnly {
			prog.CreatePackage(p.Types, nil, nil, true)
		} else {
			prog.CreatePackage(p.Types, p.Syntax, p.TypesInfo, true)
		}
	})
	ssaPkgs := make([]*ssa.Package, len(engine.pkgs))
	var skipped []SkippedPackage
	for i, p := range engine.pkgs {
		if p.Types != nil {
			ssaPkgs[i] = prog.Package(p.Types) // nil when skipped
		}
		if cause := broken[p]; cause != nil {
			skipped = append(skipped, SkippedPackage{Package: p, Cause: cause})
		}
	}
	return prog, ssaPkgs, skipped
}

// hasNonDriverError reports whether p carries an error other than one the
// go list driver reported. Under a load without NeedDeps the driver's
// error on a dependency is that `go list -export` could not compile it,
// which says nothing about whether its types are usable.
func hasNonDriverError(p *packages.Package) bool {
	for _, e := range p.Errors {
		if e.Kind != packages.ListError && !isVersionSkewNotice(e) {
			return true
		}
	}
	return false
}

// isVersionSkewNotice reports whether e is the notice go/packages appends
// to every package that already has an error when this program was built
// with an older Go than the `go` command it runs (x/tools
// go/packages/packages.go, "This application uses version go1.N of the
// source-processing packages..."). It says nothing about the package, so
// it must not decide whether the package's declarations are usable. It is
// matched by its wording because unreadable export data is also an
// UnknownError at "-"; should x/tools reword it, it counts again, the
// conservative way.
//
// This is insurance, not a fix for an observed regression. Measured on
// steward with a caliper built by go1.26.7 against go1.27.1's `go list`:
// 12 of the 1,293 packages carry errors and every one of them is a real
// type error the older checker reports ("file requires newer Go version
// go1.27", a changed math/rand/v2 signature) — no package carries the
// notice alone, so classification is the same with and without this, and
// the load collapses to 15 of steward's 117 packages either way. The
// notice can only mislead where a skew is otherwise harmless, which the
// standard library's own use of new language features makes rare.
func isVersionSkewNotice(e packages.Error) bool {
	return e.Kind == packages.UnknownError && e.Pos == "-" &&
		strings.HasPrefix(e.Msg, "This application uses version go1.")
}

// hasDeclarationError reports whether p has an error that can have
// reached its declarations: any error that is not the driver's or the
// version-skew notice and does not lie inside the body of a function
// declaration in p's syntax. A parse error counts whatever its position,
// since the AST after it is not the file's. An error whose position
// cannot be placed counts too.
//
// A type error is placed by its types.Error in p.TypeErrors, whose
// token.Pos is the position in the source, not by the position string in
// p.Errors, which //line directives have remapped: in generated code a
// declaration can map into the line range of a body in the same file, and
// a body statement into another file.
func hasDeclarationError(p *packages.Package) bool {
	typeErrors := 0
	for _, e := range p.Errors {
		switch {
		case e.Kind == packages.ListError, isVersionSkewNotice(e):
		case e.Kind == packages.TypeError:
			typeErrors++
		default:
			return true
		}
	}
	// go/packages records each types.Error in both lists; a mismatch is a
	// type error this cannot place.
	if typeErrors != len(p.TypeErrors) {
		return true
	}
	for _, e := range p.TypeErrors {
		if !insideFuncBody(p, e.Pos) {
			return true
		}
	}
	return false
}

// insideFuncBody reports whether pos lies within the body of a
// *ast.FuncDecl in p's syntax.
func insideFuncBody(p *packages.Package, pos token.Pos) bool {
	if !pos.IsValid() {
		return false
	}
	for _, f := range p.Syntax {
		if pos < f.FileStart || pos > f.FileEnd {
			continue
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil && fd.Body.Pos() <= pos && pos < fd.Body.End() {
				return true
			}
		}
	}
	return false
}

// checkSSAImports verifies every created package's imports were also created,
// which is prog.Build()'s unstated precondition. Violating it panics inside a
// builder goroutine and takes the process down, so this returns an error
// instead — naming the importer, the missing import, and the usual cause.
func checkSSAImports(prog *ssa.Program, ssaPkgs []*ssa.Package) error {
	for _, p := range ssaPkgs {
		if p == nil || p.Pkg == nil {
			continue
		}
		for _, imp := range p.Pkg.Imports() {
			if prog.Package(imp) == nil {
				return fmt.Errorf(
					"package %q imports %q, which was not created in the SSA program: "+
						"the packages.Config load mode is missing packages.NeedImports "+
						"(NeedDeps alone does not populate Imports)",
					p.Pkg.Path(), imp.Path())
			}
		}
	}
	return nil
}

// IsGeneratedFunc reports whether fn was declared in a generated file.
//
// The SSA program and call graph are built from every loaded package, so
// EngineOptions.SkipGenerated does not filter them. A consumer walking the
// call graph — building a code graph, for instance — uses this to decide
// whether generated code belongs in its output. For protobuf- or
// sqlc-heavy repositories it usually does: the generated types are the
// bridge between a schema and the code that uses it.
func (ipc *InterProceduralContext) IsGeneratedFunc(fn *ssa.Function) bool {
	if ipc == nil || ipc.Engine == nil || fn == nil {
		return false
	}
	pos := fn.Pos()
	if !pos.IsValid() {
		return false
	}
	return ipc.Engine.IsGeneratedFile(ipc.Engine.Fset().Position(pos).Filename)
}

func buildCallGraph(prog *ssa.Program, pkgs []*ssa.Package, algo CallGraphAlgorithm) (*callgraph.Graph, CallGraphAlgorithm, error) {
	switch algo {
	case AlgoRTA:
		// RTA needs at least one root function (main or init).
		var roots []*ssa.Function
		for _, pkg := range pkgs {
			if fn := pkg.Func("main"); fn != nil {
				roots = append(roots, fn)
			}
			if fn := pkg.Func("init"); fn != nil {
				roots = append(roots, fn)
			}
		}
		if len(roots) == 0 {
			// Fall back to CHA — no main/init found.
			cg := cha.CallGraph(prog)
			return cg, AlgoCHA, nil
		}
		result := rta.Analyze(roots, true)
		return result.CallGraph, AlgoRTA, nil

	case AlgoVTA:
		cg := vta.CallGraph(ssautil.AllFunctions(prog), nil)
		return cg, AlgoVTA, nil

	default: // AlgoCHA
		cg := cha.CallGraph(prog)
		return cg, AlgoCHA, nil
	}
}

// CalleesOf returns all functions that fn may call, according to the
// call graph.
func (ipc *InterProceduralContext) CalleesOf(fn *ssa.Function) []*ssa.Function {
	node := ipc.CallGraph.Nodes[fn]
	if node == nil {
		return nil
	}

	seen := make(map[*ssa.Function]bool)
	var callees []*ssa.Function
	for _, edge := range node.Out {
		callee := edge.Callee.Func
		if !seen[callee] {
			seen[callee] = true
			callees = append(callees, callee)
		}
	}
	return callees
}

// CallersOf returns all functions that may call fn, according to the
// call graph.
func (ipc *InterProceduralContext) CallersOf(fn *ssa.Function) []*ssa.Function {
	node := ipc.CallGraph.Nodes[fn]
	if node == nil {
		return nil
	}

	seen := make(map[*ssa.Function]bool)
	var callers []*ssa.Function
	for _, edge := range node.In {
		caller := edge.Caller.Func
		if !seen[caller] {
			seen[caller] = true
			callers = append(callers, caller)
		}
	}
	return callers
}

// CallSitesOf returns all call sites where fn is called, with position
// and call kind (normal, go, defer) information.
func (ipc *InterProceduralContext) CallSitesOf(fn *ssa.Function) []CallSite {
	node := ipc.CallGraph.Nodes[fn]
	if node == nil {
		return nil
	}

	var sites []CallSite
	for _, edge := range node.In {
		site := edge.Site
		if site == nil {
			continue
		}

		cs := CallSite{
			Caller: edge.Caller.Func,
			Callee: fn,
		}

		if pos := site.Pos(); pos.IsValid() {
			cs.Position = ipc.Engine.LocationOf(pos)
		}

		// Determine call kind from the SSA instruction.
		switch site.(type) {
		case *ssa.Go:
			cs.IsGo = true
		case *ssa.Defer:
			cs.IsDefer = true
		}

		sites = append(sites, cs)
	}
	return sites
}

// FindFunction looks up the SSA function corresponding to an AST
// function declaration. Returns nil if not found.
func (ipc *InterProceduralContext) FindFunction(fc *FuncContext) *ssa.Function {
	// Find the SSA package for this function's package.
	var ssaPkg *ssa.Package
	for _, sp := range ipc.SSAPkgs {
		if sp.Pkg == fc.Pkg.Types {
			ssaPkg = sp
			break
		}
	}
	if ssaPkg == nil {
		return nil
	}

	// Free function.
	if fc.Func.Recv == nil {
		return ssaPkg.Func(fc.Func.Name.Name)
	}

	// Method — search through type members.
	return ipc.findMethod(ssaPkg, fc.Func)
}

func (ipc *InterProceduralContext) findMethod(ssaPkg *ssa.Package, fn *ast.FuncDecl) *ssa.Function {
	methodName := fn.Name.Name

	for _, member := range ssaPkg.Members {
		typ, ok := member.(*ssa.Type)
		if !ok {
			continue
		}

		named := typ.Type()

		// Check value receiver method set.
		if ssaFn := ipc.findMethodInSet(named, methodName); ssaFn != nil {
			return ssaFn
		}

		// Check pointer receiver method set.
		if ssaFn := ipc.findMethodInSet(types.NewPointer(named), methodName); ssaFn != nil {
			return ssaFn
		}
	}
	return nil
}

func (ipc *InterProceduralContext) findMethodInSet(typ types.Type, name string) *ssa.Function {
	mset := ipc.Program.MethodSets.MethodSet(typ)
	for i := 0; i < mset.Len(); i++ {
		sel := mset.At(i)
		if sel.Obj().Name() == name {
			return ipc.Program.MethodValue(sel)
		}
	}
	return nil
}

// FindFunctionByName looks up an SSA function by package path and name.
// For methods, use the form "(*Type).Method" or "Type.Method".
func (ipc *InterProceduralContext) FindFunctionByName(pkgPath, name string) *ssa.Function {
	for _, pkg := range ipc.SSAPkgs {
		if pkg.Pkg.Path() == pkgPath {
			return pkg.Func(name)
		}
	}
	return nil
}

// AllFunctions returns all functions in the SSA program.
func (ipc *InterProceduralContext) AllFunctions() map[*ssa.Function]bool {
	return ssautil.AllFunctions(ipc.Program)
}
