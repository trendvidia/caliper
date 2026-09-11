package analysis

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
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
			var root *packages.Package
			for _, p := range pkgs {
				if p.PkgPath == "exportdeps" {
					root = p
				}
			}
			if len(pkgs) != 3 || root == nil {
				t.Fatalf("loaded packages = %v, want exportdeps and its two sub-packages", pkgs)
			}
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
	return fn.Pkg != nil && (fn.Pkg.Pkg.Path() == "exportdeps" || strings.HasPrefix(fn.Pkg.Pkg.Path(), "exportdeps/"))
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
		"exportdeps/usesbodyerr.Use -> exportdeps/usesbodyerr.helper",
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
			"exportdeps/usesbodyerr.Use -> local.example/bodyerr.Good",
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

// TestIllTypedDependency_BodyErrorKeepsImporters pins #30 under both
// loads: a loaded package whose dependency has a type error only inside
// a function body is in the SSA program, calling the dependency's clean
// function, while the dependency itself is there from its types alone;
// a loaded package whose dependency has a type error in a declaration
// is not, as ssautil's IllTyped rule always had it.
func TestIllTypedDependency_BodyErrorKeepsImporters(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(fmt.Sprintf("DepsFromExportData=%v", split), func(t *testing.T) {
			engine := exportdepsFixture(t, EngineOptions{SkipCgo: true, DepsFromExportData: split})
			pkgs := engine.Packages()

			// The condition go/packages reports, so the test is measuring it.
			for _, path := range []string{"exportdeps/usesbodyerr", "exportdeps/usesdeclerr"} {
				p := findImport(pkgs, path)
				if p == nil {
					t.Fatalf("%s not loaded", path)
				}
				if len(p.Errors) != 0 || !p.IllTyped {
					t.Fatalf("%s: errors=%v illTyped=%v; want a clean package that go/packages marks IllTyped through its import", path, p.Errors, p.IllTyped)
				}
			}
			bodyerr := findImport(pkgs, "local.example/bodyerr")
			declerr := findImport(pkgs, "local.example/declerr")
			if bodyerr == nil || declerr == nil {
				t.Fatal("fixture dependencies not reachable")
			}
			if split {
				// Bodies are skipped: the body error is not seen, the
				// declaration error is, and both carry the driver's compile error.
				if hasNonDriverError(bodyerr) || !hasNonDriverError(declerr) {
					t.Fatalf("split: bodyerr errors=%v declerr errors=%v", bodyerr.Errors, declerr.Errors)
				}
			} else if !hasNonDriverError(bodyerr) || !hasNonDriverError(declerr) {
				t.Fatalf("default: bodyerr errors=%v declerr errors=%v; want type errors on both", bodyerr.Errors, declerr.Errors)
			}
			if hasDeclarationError(bodyerr) || !hasDeclarationError(declerr) {
				t.Errorf("hasDeclarationError: bodyerr=%v declerr=%v, want false and true", hasDeclarationError(bodyerr), hasDeclarationError(declerr))
			}

			ipc, err := engine.InterProcedural()
			if err != nil {
				t.Fatalf("InterProcedural: %v", err)
			}
			use := ipc.FindFunctionByName("exportdeps/usesbodyerr", "Use")
			if use == nil {
				t.Fatal("usesbodyerr.Use is not in the SSA program; its dependency's body error dropped it")
			}
			var callees []string
			for _, c := range ipc.CalleesOf(use) {
				callees = append(callees, c.String())
			}
			slices.Sort(callees)
			if want := []string{"exportdeps/usesbodyerr.helper", "local.example/bodyerr.Good"}; !slices.Equal(callees, want) {
				t.Errorf("callees of usesbodyerr.Use = %v, want %v", callees, want)
			}
			sp := ipc.Program.Package(bodyerr.Types)
			if sp == nil {
				t.Fatal("bodyerr is not in the SSA program")
			}
			for _, name := range []string{"Good", "Bad"} {
				if fn := sp.Func(name); fn == nil || len(fn.Blocks) != 0 {
					t.Errorf("bodyerr.%s: present=%v with body=%v; want present from types alone, no body", name, fn != nil, fn != nil && len(fn.Blocks) != 0)
				}
			}

			if fn := ipc.FindFunctionByName("exportdeps/usesdeclerr", "Use"); fn != nil {
				t.Errorf("usesdeclerr.Use is in the SSA program; a dependency with a declaration error must drop its importers")
			}
			if ipc.Program.Package(declerr.Types) != nil {
				t.Errorf("declerr is in the SSA program; want it skipped")
			}
		})
	}
}

// TestHasDeclarationError_VersionSkewNotice pins that the notice
// go/packages appends to every package with errors when the program was
// built with an older Go than the `go` command on PATH does not make a
// body-only error a declaration error. No test run has the skew — the
// test binary is built by the go command that lists — so the notice is
// added by hand, worded as x/tools v0.44.0 words it. Unreadable export
// data, the other UnknownError at "-", still counts.
func TestHasDeclarationError_VersionSkewNotice(t *testing.T) {
	engine := exportdepsFixture(t, EngineOptions{SkipCgo: true})
	bodyerr := findImport(engine.Packages(), "local.example/bodyerr")
	if bodyerr == nil || len(bodyerr.TypeErrors) == 0 || hasDeclarationError(bodyerr) {
		t.Fatal("fixture: want local.example/bodyerr loaded with body-only type errors")
	}
	withError := func(e packages.Error) *packages.Package {
		p := *bodyerr
		p.Errors = append(slices.Clone(bodyerr.Errors), e)
		return &p
	}
	skewed := withError(packages.Error{
		Pos:  "-",
		Msg:  "This application uses version go1.26 of the source-processing packages but runs version go1.27 of 'go list'. It may fail to process source files that rely on newer language features. If so, rebuild the application using a newer version of Go.",
		Kind: packages.UnknownError,
	})
	if hasDeclarationError(skewed) {
		t.Errorf("the version-skew notice made bodyerr a declaration error: %v", skewed.Errors)
	}
	// The notice also rides on a driver-only error, which must stay driver-only.
	driverOnly := *bodyerr
	driverOnly.Errors = []packages.Error{{Pos: "-", Msg: "compile error", Kind: packages.ListError}, skewed.Errors[len(skewed.Errors)-1]}
	if hasNonDriverError(&driverOnly) {
		t.Errorf("the version-skew notice counts as a non-driver error: %v", driverOnly.Errors)
	}
	unreadable := withError(packages.Error{Pos: "-", Msg: "reading export data: unexpected EOF", Kind: packages.UnknownError})
	if !hasDeclarationError(unreadable) {
		t.Errorf("unreadable export data is not a declaration error: %v", unreadable.Errors)
	}
}

// typeCheckedFile type-checks src as the only file of a package and
// returns it shaped as go/packages would: Errors and TypeErrors, one
// entry each per type error, the Errors position remapped by //line.
func typeCheckedFile(t *testing.T, name, src string) *packages.Package {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	p := &packages.Package{PkgPath: "p", Fset: fset, Syntax: []*ast.File{f}}
	conf := types.Config{Error: func(err error) {
		te := err.(types.Error)
		p.TypeErrors = append(p.TypeErrors, te)
		p.Errors = append(p.Errors, packages.Error{Pos: te.Fset.Position(te.Pos).String(), Msg: te.Msg, Kind: packages.TypeError})
	}}
	p.Types, _ = conf.Check("p", fset, []*ast.File{f}, nil)
	return p
}

// TestHasDeclarationError_LineDirectives pins that an error is placed by
// its position in the source, not by the //line-remapped position
// go/packages reports: generated code (goyacc, cgo) remaps a body and a
// declaration to overlapping or different file:line ranges.
func TestHasDeclarationError_LineDirectives(t *testing.T) {
	for _, tc := range []struct {
		name, src, pos string
		decl           bool
	}{
		{
			// F's body maps to p.y:10–50, X's declaration to p.y:20.
			name: "declaration inside a body's remapped range",
			src:  "package p\n\n//line p.y:10:1\nfunc F() int {\n\treturn 1\n//line p.y:50:1\n}\n\n//line p.y:20:1\nvar X MissingType\n",
			pos:  "p.y:20:7",
			decl: true,
		},
		{
			// G's body statement maps to another file than the body's braces.
			name: "body error remapped to another file",
			src:  "package p\n\nfunc G() int {\n//line other.y:7:1\n\treturn undefinedName\n}\n",
			pos:  "other.y:7:9",
			decl: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := typeCheckedFile(t, "p.y", tc.src)
			if len(p.Errors) != 1 || p.Errors[0].Pos != tc.pos {
				t.Fatalf("fixture: errors = %v, want one at %s", p.Errors, tc.pos)
			}
			if got := hasDeclarationError(p); got != tc.decl {
				t.Errorf("hasDeclarationError = %v, want %v (error %v)", got, tc.decl, p.Errors[0])
			}
		})
	}
}
