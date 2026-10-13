// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package patterns

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/trendvidia/caliper/pkg/analysis"
)

func setupCrossFuncProject(t *testing.T, files map[string]string) (*analysis.Engine, *analysis.InterProceduralContext) {
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

	engine, err := analysis.NewEngine(dir)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	ipc, err := engine.InterProcedural()
	if err != nil {
		t.Fatalf("InterProcedural: %v", err)
	}

	return engine, ipc
}

func TestFindCrossFuncMatches_Basic(t *testing.T) {
	engine, ipc := setupCrossFuncProject(t, map[string]string{
		"main.go": `package testpkg

func process(data string) {
	_ = data
}

func caller1() {
	process("hello")
}

func caller2() {
	process("world")
}
`,
	})

	defPat, err := Compile("process($args...)")
	if err != nil {
		t.Fatal(err)
	}

	usePat, err := Compile("process($args...)")
	if err != nil {
		t.Fatal(err)
	}

	matches := FindCrossFuncMatches(engine, ipc, &CrossFuncRule{
		DefPattern: defPat,
		UsePattern: usePat,
	})

	if len(matches) < 2 {
		t.Errorf("expected at least 2 cross-function matches (caller1 and caller2), got %d", len(matches))
	}

	// All matches should have a definition.
	for _, m := range matches {
		if m.Definition.File == "" {
			t.Error("match has empty definition file")
		}
		if m.CallSite.File == "" {
			t.Error("match has empty call site file")
		}
	}
}

func TestFindCrossFuncMatches_NoCallSites(t *testing.T) {
	engine, ipc := setupCrossFuncProject(t, map[string]string{
		"main.go": `package testpkg

func unused() {}
func other() {}
`,
	})

	defPat, err := Compile("unused()")
	if err != nil {
		t.Fatal(err)
	}
	usePat, err := Compile("unused()")
	if err != nil {
		t.Fatal(err)
	}

	matches := FindCrossFuncMatches(engine, ipc, &CrossFuncRule{
		DefPattern: defPat,
		UsePattern: usePat,
	})

	if len(matches) != 0 {
		t.Errorf("expected 0 matches for function with no call sites, got %d", len(matches))
	}
}

func TestFindCrossFuncMatches_NoDefMatch(t *testing.T) {
	engine, ipc := setupCrossFuncProject(t, map[string]string{
		"main.go": `package testpkg

func helper() {}
func caller() { helper() }
`,
	})

	defPat, err := Compile("nonexistent()")
	if err != nil {
		t.Fatal(err)
	}
	usePat, err := Compile("nonexistent()")
	if err != nil {
		t.Fatal(err)
	}

	matches := FindCrossFuncMatches(engine, ipc, &CrossFuncRule{
		DefPattern: defPat,
		UsePattern: usePat,
	})

	if len(matches) != 0 {
		t.Errorf("expected 0 matches when def pattern doesn't match, got %d", len(matches))
	}
}

func TestMergeBindings(t *testing.T) {
	a := Bindings{
		"x": {Name: "x"},
		"y": {Name: "y"},
	}
	b := Bindings{
		"y": {Name: "y_override"},
		"z": {Name: "z"},
	}

	merged := mergeBindings(a, b)

	if len(merged) != 3 {
		t.Errorf("expected 3 bindings, got %d", len(merged))
	}
	if merged["x"].Name != "x" {
		t.Error("expected 'x' from first set")
	}
	if merged["y"].Name != "y_override" {
		t.Error("expected 'y_override' from second set (override)")
	}
	if merged["z"].Name != "z" {
		t.Error("expected 'z' from second set")
	}
}
