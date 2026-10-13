// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package analysis

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"
)

// TestBuildInterProceduralContext_GenericMethods pins that a program
// reaching methods with type parameters of their own — Go 1.27's
// math/rand/v2 (*Rand).N, and one in the module — builds a call graph,
// with the calls to them as edges. x/tools v0.44.0 panicked in
// Program.RuntimeTypes on such a method's signature (#40).
func TestBuildInterProceduralContext_GenericMethods(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("testdata", "genericmethod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, split := range []bool{false, true} {
		for _, algo := range []CallGraphAlgorithm{AlgoCHA, AlgoVTA} {
			t.Run(fmt.Sprintf("DepsFromExportData=%v/algo=%d", split, algo), func(t *testing.T) {
				engine, err := NewEngineWithOptions(dir, EngineOptions{SkipCgo: true, DepsFromExportData: split})
				if err != nil {
					t.Fatalf("NewEngineWithOptions: %v", err)
				}
				ipc, err := BuildInterProceduralContext(engine, algo)
				if err != nil {
					t.Fatalf("BuildInterProceduralContext: %v", err)
				}

				found := false
				for _, p := range ipc.SSAPkgs {
					if p.Pkg.Path() != "genericmethod" {
						continue
					}
					fn := p.Func("Roll")
					if fn == nil {
						t.Fatal("genericmethod.Roll not in the SSA program")
					}
					found = true
					var callees []string
					for _, c := range ipc.CalleesOf(fn) {
						origin := c
						if o := c.Origin(); o != nil {
							origin = o
						}
						callees = append(callees, origin.String())
					}
					for _, want := range []string{
						"(*math/rand/v2.Rand).N",
						"(*genericmethod.Box).Map",
					} {
						if !slices.Contains(callees, want) {
							t.Errorf("Roll callees = %v, want %s among them", callees, want)
						}
					}
				}
				if !found {
					t.Fatal("package genericmethod not in the SSA program")
				}
			})
		}
	}
}

// TestBuildCallGraph_PanicIsError pins that a panic inside call graph
// construction comes back as an error rather than ending the process: a
// nil program stands in for the next input x/tools cannot handle.
func TestBuildCallGraph_PanicIsError(t *testing.T) {
	for _, algo := range []CallGraphAlgorithm{AlgoCHA, AlgoRTA, AlgoVTA} {
		cg, _, err := buildCallGraph(nil, nil, algo)
		if err == nil || cg != nil {
			t.Errorf("algo=%d: buildCallGraph(nil) = %v, %v; want nil graph and an error", algo, cg, err)
		} else {
			t.Logf("algo=%d: %v", algo, err)
		}
	}
}
