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
	"strconv"
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
func createProgram(engine *Engine, mode ssa.BuilderMode) (*ssa.Program, []*ssa.Package) {
	prog := ssa.NewProgram(engine.fset, mode)
	roots := make(map[*packages.Package]bool, len(engine.pkgs))
	for _, p := range engine.pkgs {
		roots[p] = true
	}
	broken := make(map[*packages.Package]bool)
	// Visit runs post-order, so a package's imports are classified
	// before it is.
	packages.Visit(engine.pkgs, nil, func(p *packages.Package) {
		b := hasDeclarationError(p)
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
		typesOnly := hasNonDriverError(p) || (engine.opts.DepsFromExportData && !roots[p])
		if typesOnly {
			prog.CreatePackage(p.Types, nil, nil, true)
		} else {
			prog.CreatePackage(p.Types, p.Syntax, p.TypesInfo, true)
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

// hasDeclarationError reports whether p has an error that can have
// reached its declarations: any error that is not the driver's and does
// not lie inside the body of a function declaration in p's syntax. A
// parse error counts whatever its position, since the AST after it is
// not the file's. An error whose position cannot be placed counts too.
func hasDeclarationError(p *packages.Package) bool {
	for _, e := range p.Errors {
		switch e.Kind {
		case packages.ListError:
			continue
		case packages.TypeError:
			if insideFuncBody(p, e.Pos) {
				continue
			}
		}
		return true
	}
	return false
}

// insideFuncBody reports whether the position string of a packages.Error
// ("file:line:col") lies within the body of a *ast.FuncDecl of p.
func insideFuncBody(p *packages.Package, pos string) bool {
	if p.Fset == nil {
		return false
	}
	file, line, col, ok := splitErrorPos(pos)
	if !ok {
		return false
	}
	for _, f := range p.Syntax {
		if p.Fset.Position(f.Pos()).Filename != file {
			continue
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			start, end := p.Fset.Position(fd.Body.Pos()), p.Fset.Position(fd.Body.End())
			if !before(line, col, start.Line, start.Column) && before(line, col, end.Line, end.Column) {
				return true
			}
		}
	}
	return false
}

// splitErrorPos parses "file:line:col". A file name may itself contain
// colons on some systems, so the last two fields are taken as the numbers.
func splitErrorPos(pos string) (file string, line, col int, ok bool) {
	i := strings.LastIndexByte(pos, ':')
	if i < 0 {
		return "", 0, 0, false
	}
	j := strings.LastIndexByte(pos[:i], ':')
	if j < 0 {
		return "", 0, 0, false
	}
	line, err1 := strconv.Atoi(pos[j+1 : i])
	col, err2 := strconv.Atoi(pos[i+1:])
	if err1 != nil || err2 != nil || line <= 0 {
		return "", 0, 0, false
	}
	return pos[:j], line, col, true
}

// before reports whether (l1, c1) is strictly before (l2, c2).
func before(l1, c1, l2, c2 int) bool {
	return l1 < l2 || l1 == l2 && c1 < c2
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
