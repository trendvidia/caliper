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
	"go/types"

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
	prog, ssaPkgs := createProgram(engine, ssa.SanityCheckFunctions)

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
	}, nil
}

// createProgram creates the SSA program and a package for everything the
// load reached. Under the default load that is ssautil.AllPackages: every
// package from its syntax. Under EngineOptions.DepsFromExportData the
// loaded packages are created from syntax and every dependency from its
// types alone, so no dependency function has a body — including one
// go/packages fell back to type-checking from source because `go list
// -export` produced no export data for it (it does not compile on this
// toolchain). That fallback skips function bodies, and the SSA builder
// has no guard against an AST the type checker did not visit.
//
// The fallback also decides which loaded packages are analysed. The
// driver attaches its compile error to such a dependency, which makes
// go/packages mark it and every importer IllTyped, and ssautil would then
// drop the importing loaded packages from the program — packages whose
// own type-check was clean, since go/types does not need a dependency to
// compile, only to type-check its declarations. So this does not read
// IllTyped: a package is skipped when it, or a package it imports
// transitively, has an error that is not the driver's (a parse or type
// error, or export data that could not be read). That is the rule the
// default load applies through IllTyped minus the compile errors, and
// since a dependency's bodies are never type-checked here, minus type
// errors inside them too — which the default load does see, and drops
// importers for (#30).
func createProgram(engine *Engine, mode ssa.BuilderMode) (*ssa.Program, []*ssa.Package) {
	if !engine.opts.DepsFromExportData {
		return ssautil.AllPackages(engine.Packages(), mode)
	}
	prog := ssa.NewProgram(engine.fset, mode)
	roots := make(map[*packages.Package]bool, len(engine.pkgs))
	for _, p := range engine.pkgs {
		roots[p] = true
	}
	broken := make(map[*packages.Package]bool)
	// Visit runs post-order, so a package's imports are classified
	// before it is.
	packages.Visit(engine.pkgs, nil, func(p *packages.Package) {
		b := hasNonDriverError(p)
		for _, imp := range p.Imports {
			if broken[imp] {
				b = true
				break
			}
		}
		broken[p] = b
		if b || p.Types == nil {
			return
		}
		if roots[p] {
			prog.CreatePackage(p.Types, p.Syntax, p.TypesInfo, true)
		} else {
			prog.CreatePackage(p.Types, nil, nil, true)
		}
	})
	ssaPkgs := make([]*ssa.Package, len(engine.pkgs))
	for i, p := range engine.pkgs {
		if p.Types != nil {
			ssaPkgs[i] = prog.Package(p.Types) // nil when skipped
		}
	}
	return prog, ssaPkgs
}

// hasNonDriverError reports whether p carries an error other than one the
// go list driver reported. Under a load without NeedDeps the driver's
// error on a dependency is that `go list -export` could not compile it,
// which says nothing about whether its types are usable.
func hasNonDriverError(p *packages.Package) bool {
	for _, e := range p.Errors {
		if e.Kind != packages.ListError {
			return true
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
