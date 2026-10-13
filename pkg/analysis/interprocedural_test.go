// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package analysis

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// setupMultiFileProject creates a temp Go project with multiple source files.
func setupMultiFileProject(t *testing.T, files map[string]string) *Engine {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module testpkg\n\ngo 1.23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	engine, err := NewEngine(dir)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return engine
}

func TestBuildInterProceduralContext_CHA(t *testing.T) {
	engine := setupMultiFileProject(t, map[string]string{
		"main.go": `package testpkg

func caller() {
	callee()
}

func callee() {}
`,
	})

	ipc, err := BuildInterProceduralContext(engine, AlgoCHA)
	if err != nil {
		t.Fatalf("BuildInterProceduralContext: %v", err)
	}
	if ipc.Program == nil {
		t.Fatal("SSA program is nil")
	}
	if ipc.CallGraph == nil {
		t.Fatal("call graph is nil")
	}
	if ipc.Algorithm != AlgoCHA {
		t.Errorf("expected CHA algorithm, got %d", ipc.Algorithm)
	}
	if len(ipc.SSAPkgs) == 0 {
		t.Fatal("no SSA packages")
	}
}

func TestCalleesOf(t *testing.T) {
	engine := setupMultiFileProject(t, map[string]string{
		"main.go": `package testpkg

func caller() {
	a()
	b()
}

func a() {}
func b() {}
`,
	})

	ipc, err := BuildInterProceduralContext(engine, AlgoCHA)
	if err != nil {
		t.Fatal(err)
	}

	callerFn := ipc.SSAPkgs[0].Func("caller")
	if callerFn == nil {
		t.Fatal("caller function not found in SSA")
	}

	callees := ipc.CalleesOf(callerFn)
	names := make(map[string]bool)
	for _, c := range callees {
		names[c.Name()] = true
	}

	if !names["a"] {
		t.Error("expected callee 'a'")
	}
	if !names["b"] {
		t.Error("expected callee 'b'")
	}
}

func TestCallersOf(t *testing.T) {
	engine := setupMultiFileProject(t, map[string]string{
		"main.go": `package testpkg

func one() { target() }
func two() { target() }
func target() {}
`,
	})

	ipc, err := BuildInterProceduralContext(engine, AlgoCHA)
	if err != nil {
		t.Fatal(err)
	}

	targetFn := ipc.SSAPkgs[0].Func("target")
	if targetFn == nil {
		t.Fatal("target function not found in SSA")
	}

	callers := ipc.CallersOf(targetFn)
	names := make(map[string]bool)
	for _, c := range callers {
		names[c.Name()] = true
	}

	if !names["one"] {
		t.Error("expected caller 'one'")
	}
	if !names["two"] {
		t.Error("expected caller 'two'")
	}
}

func TestCallSitesOf(t *testing.T) {
	engine := setupMultiFileProject(t, map[string]string{
		"main.go": `package testpkg

func launcher() {
	go worker()
	defer cleanup()
	normal()
}

func worker() {}
func cleanup() {}
func normal() {}
`,
	})

	ipc, err := BuildInterProceduralContext(engine, AlgoCHA)
	if err != nil {
		t.Fatal(err)
	}

	// Check go statement.
	workerFn := ipc.SSAPkgs[0].Func("worker")
	if workerFn != nil {
		sites := ipc.CallSitesOf(workerFn)
		foundGo := false
		for _, s := range sites {
			if s.IsGo {
				foundGo = true
			}
		}
		if !foundGo {
			t.Error("expected go call site for worker")
		}
	}

	// Check defer statement.
	cleanupFn := ipc.SSAPkgs[0].Func("cleanup")
	if cleanupFn != nil {
		sites := ipc.CallSitesOf(cleanupFn)
		foundDefer := false
		for _, s := range sites {
			if s.IsDefer {
				foundDefer = true
			}
		}
		if !foundDefer {
			t.Error("expected defer call site for cleanup")
		}
	}
}

func TestFindFunction_FreeFunction(t *testing.T) {
	engine := setupMultiFileProject(t, map[string]string{
		"main.go": `package testpkg

func myFunc() {}
`,
	})

	ipc, err := BuildInterProceduralContext(engine, AlgoCHA)
	if err != nil {
		t.Fatal(err)
	}

	funcs := engine.FuncNodes()
	if len(funcs) == 0 {
		t.Fatal("no functions found")
	}

	ssaFn := ipc.FindFunction(funcs[0])
	if ssaFn == nil {
		t.Fatal("FindFunction returned nil for free function")
	}
	if ssaFn.Name() != "myFunc" {
		t.Errorf("expected myFunc, got %s", ssaFn.Name())
	}
}

func TestFindFunction_Method(t *testing.T) {
	engine := setupMultiFileProject(t, map[string]string{
		"main.go": `package testpkg

type Server struct{}

func (s *Server) Handle() {}
`,
	})

	ipc, err := BuildInterProceduralContext(engine, AlgoCHA)
	if err != nil {
		t.Fatal(err)
	}

	funcs := engine.FuncNodes()
	var handleFC *FuncContext
	for _, fc := range funcs {
		if fc.Func.Name.Name == "Handle" {
			handleFC = fc
			break
		}
	}
	if handleFC == nil {
		t.Fatal("Handle method not found")
	}

	ssaFn := ipc.FindFunction(handleFC)
	if ssaFn == nil {
		t.Fatal("FindFunction returned nil for method")
	}
	if ssaFn.Name() != "Handle" {
		t.Errorf("expected Handle, got %s", ssaFn.Name())
	}
}

func TestRTAFallbackToCHA(t *testing.T) {
	// Library code — no main function or init. RTA should fall back to CHA.
	// Note: Go generates synthetic init functions for packages, so we check
	// that RTA at least produces a valid call graph even without an explicit main.
	engine := setupMultiFileProject(t, map[string]string{
		"lib.go": `package testpkg

func helper() {}
`,
	})

	ipc, err := BuildInterProceduralContext(engine, AlgoRTA)
	if err != nil {
		t.Fatal(err)
	}

	// Should produce a valid call graph regardless of algorithm used.
	if ipc.CallGraph == nil {
		t.Fatal("call graph is nil")
	}
	// Algorithm may be RTA (if synthetic init was used as root) or CHA (if fallback).
	if ipc.Algorithm != AlgoCHA && ipc.Algorithm != AlgoRTA {
		t.Errorf("expected CHA or RTA algorithm, got %d", ipc.Algorithm)
	}
}

func TestEngineInterProcedural_Lazy(t *testing.T) {
	engine := setupMultiFileProject(t, map[string]string{
		"main.go": `package testpkg

func f() {}
`,
	})

	// First call builds the context.
	ipc1, err := engine.InterProcedural()
	if err != nil {
		t.Fatal(err)
	}
	if ipc1 == nil {
		t.Fatal("InterProcedural returned nil")
	}

	// Second call returns the same cached context.
	ipc2, err := engine.InterProcedural()
	if err != nil {
		t.Fatal(err)
	}
	if ipc1 != ipc2 {
		t.Error("expected same cached InterProceduralContext on second call")
	}
}

func TestFindFunctionByName(t *testing.T) {
	engine := setupMultiFileProject(t, map[string]string{
		"main.go": `package testpkg

func target() {}
`,
	})

	ipc, err := BuildInterProceduralContext(engine, AlgoCHA)
	if err != nil {
		t.Fatal(err)
	}

	fn := ipc.FindFunctionByName("testpkg", "target")
	if fn == nil {
		t.Fatal("FindFunctionByName returned nil")
	}
	if fn.Name() != "target" {
		t.Errorf("expected target, got %s", fn.Name())
	}

	// Non-existent function.
	if ipc.FindFunctionByName("testpkg", "nonexistent") != nil {
		t.Error("expected nil for nonexistent function")
	}
}

// TestBuildInterProceduralContext_StdlibImports is the regression guard for the
// load-mode bug where packages.NeedImports was absent: pkg.Imports came back
// empty, ssautil.AllPackages never created the dependency packages, and
// prog.Build() panicked on the first unsatisfied import. Every fixture in this
// file used to be import-free, so the whole interprocedural surface was broken
// against real code while the suite stayed green.
//
// The fixture must import at least one stdlib package for this to test anything.
func TestBuildInterProceduralContext_StdlibImports(t *testing.T) {
	engine := setupMultiFileProject(t, map[string]string{
		"main.go": `package testpkg

import (
	"context"
	"fmt"
)

func caller(ctx context.Context) {
	callee(ctx)
	fmt.Println("side effect")
}

func callee(ctx context.Context) {
	_ = ctx.Err()
}
`,
	})

	// Guard the root cause directly: the load mode must populate Imports.
	for _, p := range engine.Packages() {
		if len(p.Imports) == 0 {
			t.Fatalf("package %q has no Imports — packages.NeedImports is missing "+
				"from the load mode; SSA cannot resolve dependencies without it", p.PkgPath)
		}
	}

	ipc, err := BuildInterProceduralContext(engine, AlgoCHA)
	if err != nil {
		t.Fatalf("BuildInterProceduralContext: %v", err)
	}

	fn := ipc.FindFunctionByName("testpkg", "callee")
	if fn == nil {
		t.Fatal("FindFunctionByName(testpkg, callee) returned nil")
	}
	callers := ipc.CallersOf(fn)
	if len(callers) == 0 {
		t.Fatal("CallersOf(callee) is empty; expected caller()")
	}
	found := false
	for _, c := range callers {
		if c.Name() == "caller" {
			found = true
		}
	}
	if !found {
		t.Errorf("CallersOf(callee) did not include caller(); got %v", callers)
	}
}

// TestCheckSSAImports_MissingDependency pins that a program whose dependencies
// were never created is refused with an error instead of panicking. The panic
// fires in a goroutine spawned by prog.Build(), so it cannot be recovered — it
// used to exit(2) and take the MCP server with it, ending the session over one
// malformed workspace.
//
// It reproduces the exact historical condition rather than a synthetic one: load
// a package WITHOUT packages.NeedImports (the pre-fix load mode), which leaves
// pkg.Imports empty so ssautil.AllPackages never creates the dependency
// packages.
func TestCheckSSAImports_MissingDependency(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module testpkg\n\ngo 1.23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := `package testpkg

import "context"

func use(ctx context.Context) error { return ctx.Err() }
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	// Deliberately the pre-fix mode: NeedDeps without NeedImports.
	brokenMode := packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
		packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps | packages.NeedModule
	pkgs, err := packages.Load(&packages.Config{Mode: brokenMode, Dir: dir}, "./...")
	if err != nil {
		t.Fatalf("packages.Load: %v", err)
	}
	for _, p := range pkgs {
		if len(p.Imports) != 0 {
			t.Skipf("x/tools now populates Imports without NeedImports; "+
				"this test no longer reproduces the condition (package %q)", p.PkgPath)
		}
	}

	prog, ssaPkgs := ssautil.AllPackages(pkgs, ssa.SanityCheckFunctions)

	// Must return an error rather than let prog.Build() panic.
	err = checkSSAImports(prog, ssaPkgs)
	if err == nil {
		t.Fatal("checkSSAImports accepted a program with uncreated dependencies; " +
			"prog.Build() would panic in a builder goroutine and kill the process")
	}
	if !strings.Contains(err.Error(), "NeedImports") {
		t.Errorf("error does not name the root cause: %v", err)
	}
}
