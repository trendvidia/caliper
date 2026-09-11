package analysis

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
)

// exportdepsFixture loads testdata/exportdeps — a module with a
// third-party dependency (github.com/google/uuid) and a standard-library
// one — under the given options, alidade's shape (generated code kept,
// cgo skipped) plus whatever the test sets.
func exportdepsFixture(t *testing.T, opts EngineOptions) *Engine {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("testdata", "exportdeps"))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngineWithOptions(dir, opts)
	if err != nil {
		t.Fatalf("NewEngineWithOptions(DepsFromExportData=%v): %v", opts.DepsFromExportData, err)
	}
	return engine
}

// findImport walks the import graph from the loaded packages and returns
// the package with the given path, or nil.
func findImport(pkgs []*packages.Package, path string) *packages.Package {
	var found *packages.Package
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		if p.PkgPath == path {
			found = p
		}
	})
	return found
}

// TestDepsFromExportData_LoadShape pins what each mode hands a consumer:
// the module's packages have syntax and type info under both; a
// dependency has syntax under the default and only types under the split.
//
// The exception is a dependency that does not compile — the fixture's
// local.example/nocompile, whose function has no body — so `go list
// -export` gives it no export data and go/packages type-checks it from
// source, attaching the driver's compile error. go/packages then marks
// the loaded package IllTyped through it; the engine does not act on that
// (see createProgram), and the pin here is that the condition still
// exists to be handled.
func TestDepsFromExportData_LoadShape(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(fmt.Sprintf("DepsFromExportData=%v", split), func(t *testing.T) {
			engine := exportdepsFixture(t, EngineOptions{SkipCgo: true, DepsFromExportData: split})
			pkgs := engine.Packages()
			if len(pkgs) != 1 || pkgs[0].PkgPath != "exportdeps" {
				t.Fatalf("loaded packages = %v, want [exportdeps]", pkgs)
			}
			root := pkgs[0]
			if len(root.Syntax) == 0 || root.TypesInfo == nil || root.Types == nil || !root.Types.Complete() {
				t.Fatalf("module package lacks syntax or types: Syntax=%d TypesInfo=%v", len(root.Syntax), root.TypesInfo != nil)
			}
			for _, dep := range []string{"github.com/google/uuid", "fmt"} {
				p := findImport(pkgs, dep)
				if p == nil {
					t.Fatalf("dependency %q not reachable through Imports", dep)
				}
				if p.Types == nil || !p.Types.Complete() {
					t.Errorf("%s: Types is nil or incomplete; a direct import needs complete types under both modes", dep)
				}
				if p.Types.Scope().Lookup("New") == nil && dep == "github.com/google/uuid" {
					t.Errorf("%s: uuid.New missing from the dependency's scope", dep)
				}
				hasSrc := len(p.Syntax) > 0 && p.TypesInfo != nil
				if hasSrc == split {
					t.Errorf("%s: Syntax=%d TypesInfo=%v under DepsFromExportData=%v; want source only under the default",
						dep, len(p.Syntax), p.TypesInfo != nil, split)
				}
			}
			nc := findImport(pkgs, "local.example/nocompile")
			if nc == nil {
				t.Fatal("local.example/nocompile not reachable through Imports")
			}
			if len(nc.Syntax) == 0 || nc.Types == nil || !nc.Types.Complete() {
				t.Errorf("nocompile: Syntax=%d complete=%v; it has no export data, so both modes type-check it from source",
					len(nc.Syntax), nc.Types != nil && nc.Types.Complete())
			}
			driverErr := slices.ContainsFunc(nc.Errors, func(e packages.Error) bool { return e.Kind == packages.ListError })
			if driverErr != split || root.IllTyped != split {
				t.Errorf("nocompile: driver error=%v, root IllTyped=%v under DepsFromExportData=%v; want both only under the split",
					driverErr, root.IllTyped, split)
			}
		})
	}
}

// edgeKey names a call-graph edge by its caller, callee and call site,
// so two graphs can be compared edge for edge.
func edgeKey(e *Engine, caller, callee *ssa.Function, site ssa.CallInstruction) string {
	pos := "-"
	if site != nil && site.Pos().IsValid() {
		p := e.Fset().Position(site.Pos())
		pos = fmt.Sprintf("%s:%d:%d", filepath.Base(p.Filename), p.Line, p.Column)
	}
	return caller.String() + " -> " + callee.String() + " @ " + pos
}

// inModule reports whether fn is a function of the fixture module. A
// synthetic function with no package (a wrapper, a bound method) is not.
func inModule(fn *ssa.Function) bool {
	return fn.Pkg != nil && fn.Pkg.Pkg.Path() == "exportdeps"
}

// callGraphEdges splits the CHA call graph into the edges whose caller and
// callee are both in the module, those from the module into a dependency,
// and those whose caller is a dependency function.
func callGraphEdges(t *testing.T, e *Engine) (within, out, fromDep []string) {
	t.Helper()
	ipc, err := BuildInterProceduralContext(e, AlgoCHA)
	if err != nil {
		t.Fatalf("BuildInterProceduralContext: %v", err)
	}
	for fn, node := range ipc.CallGraph.Nodes {
		for _, edge := range node.Out {
			key := edgeKey(e, fn, edge.Callee.Func, edge.Site)
			switch {
			case inModule(fn) && inModule(edge.Callee.Func):
				within = append(within, key)
			case inModule(fn):
				out = append(out, key)
			case edge.Caller.Func.Pkg != nil:
				fromDep = append(fromDep, key)
			}
		}
	}
	slices.Sort(within)
	slices.Sort(out)
	slices.Sort(fromDep)
	return within, out, fromDep
}

// TestDepsFromExportData_CallGraphWithinModuleIdentical pins the promise
// #28 makes: the call graph among the module's own functions is the same
// edge for edge whether dependencies were type-checked from source or
// read from export data, and calls from the module into a dependency are
// still edges to the dependency's function.
func TestDepsFromExportData_CallGraphWithinModuleIdentical(t *testing.T) {
	full := exportdepsFixture(t, EngineOptions{SkipCgo: true})
	split := exportdepsFixture(t, EngineOptions{SkipCgo: true, DepsFromExportData: true})

	fullWithin, fullOut, _ := callGraphEdges(t, full)
	splitWithin, splitOut, _ := callGraphEdges(t, split)
	t.Logf("default: %d within-module edges, %d into dependencies; DepsFromExportData: %d and %d",
		len(fullWithin), len(fullOut), len(splitWithin), len(splitOut))

	// The set must be worth comparing: a static call and a dynamic
	// dispatch with two candidates, all within the module.
	for _, want := range []string{
		"exportdeps.Tag -> exportdeps.describe",
		"exportdeps.Total -> (exportdeps.Square).Area",
		"exportdeps.Total -> (exportdeps.Circle).Area",
	} {
		if !slices.ContainsFunc(fullWithin, func(k string) bool { return strings.HasPrefix(k, want+" @") }) {
			t.Errorf("default mode: within-module edge %q missing from\n%s", want, strings.Join(fullWithin, "\n"))
		}
	}
	if !slices.Equal(fullWithin, splitWithin) {
		t.Errorf("within-module edges differ\n-- default --\n%s\n-- DepsFromExportData --\n%s",
			strings.Join(fullWithin, "\n"), strings.Join(splitWithin, "\n"))
	}

	// Calls from the module into a dependency resolve to the dependency's
	// function under both modes: uuid.New is a member of a package created
	// from export data, and the static call from Tag lands on it.
	for _, edges := range [][]string{fullOut, splitOut} {
		for _, want := range []string{
			"exportdeps.Tag -> github.com/google/uuid.New",
			"exportdeps.Boundary -> local.example/nocompile.Double",
		} {
			if !slices.ContainsFunc(edges, func(k string) bool { return strings.HasPrefix(k, want+" @") }) {
				t.Errorf("edge %q missing from\n%s", want, strings.Join(edges, "\n"))
			}
		}
	}
	if !slices.Equal(fullOut, splitOut) {
		t.Errorf("module-to-dependency edges differ\n-- default --\n%s\n-- DepsFromExportData --\n%s",
			strings.Join(fullOut, "\n"), strings.Join(splitOut, "\n"))
	}
}

// TestDepsFromExportData_NoDependencyBodies pins the documented limitation:
// under the split load a dependency's functions have no bodies, so no
// call-graph edge leaves one — the call from inside fmt back into
// (Square).String, present by default, is gone.
func TestDepsFromExportData_NoDependencyBodies(t *testing.T) {
	full := exportdepsFixture(t, EngineOptions{SkipCgo: true})
	split := exportdepsFixture(t, EngineOptions{SkipCgo: true, DepsFromExportData: true})

	_, _, fullFromDep := callGraphEdges(t, full)
	_, _, splitFromDep := callGraphEdges(t, split)
	t.Logf("edges leaving dependency functions: default %d, DepsFromExportData %d", len(fullFromDep), len(splitFromDep))

	callback := func(k string) bool { return strings.Contains(k, " -> (exportdeps.Square).String @") }
	if !slices.ContainsFunc(fullFromDep, callback) {
		t.Errorf("default mode: no edge from a dependency function into (Square).String; fmt should call the Stringer\n%s", strings.Join(fullFromDep, "\n"))
	}
	if len(splitFromDep) != 0 {
		t.Errorf("DepsFromExportData: %d edges leave dependency functions, want none:\n%s", len(splitFromDep), strings.Join(splitFromDep, "\n"))
	}

	ipc, err := split.InterProcedural()
	if err != nil {
		t.Fatal(err)
	}
	uuidNew := ipc.FindFunctionByName("github.com/google/uuid", "New")
	if uuidNew == nil {
		// Not in SSAPkgs (those are the loaded packages); look it up
		// through the program's package for uuid.
		if p := findImport(split.Packages(), "github.com/google/uuid"); p != nil {
			if sp := ipc.Program.Package(p.Types); sp != nil {
				uuidNew = sp.Func("New")
			}
		}
	}
	if uuidNew == nil {
		t.Fatal("uuid.New not found in the SSA program under DepsFromExportData")
	}
	if len(uuidNew.Blocks) != 0 {
		t.Errorf("uuid.New has %d blocks under DepsFromExportData; a dependency function should have no body", len(uuidNew.Blocks))
	}

	// A dependency that fell back to source type-checking is still
	// bodiless: the type checker skipped its bodies, and handing that
	// AST to the SSA builder would be a panic in a builder goroutine.
	if p := findImport(split.Packages(), "local.example/nocompile"); p != nil {
		sp := ipc.Program.Package(p.Types)
		if sp == nil {
			t.Fatal("nocompile is not in the SSA program under DepsFromExportData")
		}
		if fn := sp.Func("Double"); fn == nil || len(fn.Blocks) != 0 {
			t.Errorf("nocompile.Double: blocks=%v under DepsFromExportData; want the function with no body", fn != nil && len(fn.Blocks) != 0)
		}
	}

	fullIPC, err := full.InterProcedural()
	if err != nil {
		t.Fatal(err)
	}
	if p := findImport(full.Packages(), "github.com/google/uuid"); p != nil {
		if fn := fullIPC.Program.Package(p.Types).Func("New"); fn == nil || len(fn.Blocks) == 0 {
			t.Errorf("default mode: uuid.New has no body; the comparison above is not measuring the split")
		}
	}
}
