// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package analysis

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"golang.org/x/tools/go/packages"
)

// failedpkgsFixture loads a copy of testdata/failedpkgs: a module where
// some of the packages the load matches fail — bodyerr in a function
// body, declerr in a declaration, syntaxerr in parsing — next to clean
// packages importing them, and a directory no configuration builds.
// syntaxerr is written here rather than kept in testdata, where it would
// stop gofmt.
func failedpkgsFixture(t *testing.T, opts EngineOptions) *Engine {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(filepath.Join("testdata", "failedpkgs"))); err != nil {
		t.Fatal(err)
	}
	const syntaxerr = "// Package syntaxerr does not parse.\npackage syntaxerr\n\nfunc Good() int { return 1 }\n\nfunc Bad() int {\n\treturn 1\n"
	if err := os.Mkdir(filepath.Join(dir, "syntaxerr"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "syntaxerr", "syntaxerr.go"), []byte(syntaxerr), 0o644); err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngineWithOptions(dir, opts)
	if err != nil {
		t.Fatalf("NewEngineWithOptions(DepsFromExportData=%v): %v", opts.DepsFromExportData, err)
	}
	return engine
}

func pkgPaths(pkgs []*packages.Package) []string {
	var paths []string
	for _, p := range pkgs {
		paths = append(paths, p.PkgPath)
	}
	slices.Sort(paths)
	return paths
}

// TestEngine_Failed pins that the packages a load matched and filtered out
// can be read from the Engine with their errors and what go/packages kept
// of their syntax and types, under both loads (#38).
func TestEngine_Failed(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(fmt.Sprintf("DepsFromExportData=%v", split), func(t *testing.T) {
			engine := failedpkgsFixture(t, EngineOptions{SkipCgo: true, DepsFromExportData: split})

			if got, want := pkgPaths(engine.Packages()), []string{
				"failedpkgs/clean", "failedpkgs/usesbody", "failedpkgs/usesdecl", "failedpkgs/usesusesdecl",
			}; !slices.Equal(got, want) {
				t.Errorf("Packages() = %v, want %v", got, want)
			}
			// excluded is in neither list: ./... does not match it.
			if got, want := pkgPaths(engine.Failed()), []string{
				"failedpkgs/bodyerr", "failedpkgs/declerr", "failedpkgs/syntaxerr",
			}; !slices.Equal(got, want) {
				t.Fatalf("Failed() = %v, want %v", got, want)
			}

			// What a caller describing the module gets from each: the error,
			// the syntax that parsed, and the declarations that resolved.
			wantScope := map[string][]string{
				"failedpkgs/bodyerr":   {"Bad", "Good"},
				"failedpkgs/declerr":   {"Good", "V"},
				"failedpkgs/syntaxerr": {"Bad", "Good"},
			}
			for _, p := range engine.Failed() {
				if len(p.Errors) == 0 {
					t.Errorf("%s: in Failed() with no Errors", p.PkgPath)
				}
				if len(p.Syntax) != 1 || p.TypesInfo == nil || p.Types == nil {
					t.Errorf("%s: syntax files=%d typesInfo=%v types=%v; want the partial results go/packages produces",
						p.PkgPath, len(p.Syntax), p.TypesInfo != nil, p.Types != nil)
					continue
				}
				if got := p.Types.Scope().Names(); !slices.Equal(got, wantScope[p.PkgPath]) {
					t.Errorf("%s: scope = %v, want %v", p.PkgPath, got, wantScope[p.PkgPath])
				}
			}
		})
	}
}

// TestInterProcedural_Skipped pins which loaded packages the SSA program
// leaves out, and the package whose error caused it: a package importing
// one with a declaration error, directly or through another loaded
// package. An importer of a body-only error stays, with the call into it.
func TestInterProcedural_Skipped(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(fmt.Sprintf("DepsFromExportData=%v", split), func(t *testing.T) {
			engine := failedpkgsFixture(t, EngineOptions{SkipCgo: true, DepsFromExportData: split})
			ipc, err := engine.InterProcedural()
			if err != nil {
				t.Fatalf("InterProcedural: %v", err)
			}

			got := map[string]string{}
			for _, s := range ipc.Skipped {
				got[s.Package.PkgPath] = s.Cause.PkgPath
				if ipc.Program.Package(s.Package.Types) != nil {
					t.Errorf("%s: in Skipped and in the SSA program", s.Package.PkgPath)
				}
			}
			want := map[string]string{
				"failedpkgs/usesdecl":     "failedpkgs/declerr",
				"failedpkgs/usesusesdecl": "failedpkgs/declerr",
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("Skipped = %v, want %v", got, want)
			}

			use := ipc.FindFunctionByName("failedpkgs/usesbody", "Use")
			if use == nil {
				t.Fatal("usesbody.Use is not in the SSA program")
			}
			var callees []string
			for _, c := range ipc.CalleesOf(use) {
				callees = append(callees, c.String())
				if len(c.Blocks) != 0 {
					t.Errorf("%s has a body; a package with a body error is created from its types alone", c)
				}
			}
			if want := []string{"failedpkgs/bodyerr.Good"}; !slices.Equal(callees, want) {
				t.Errorf("callees of usesbody.Use = %v, want %v", callees, want)
			}
		})
	}
}
