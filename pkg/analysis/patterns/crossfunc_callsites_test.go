// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package patterns

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/trendvidia/caliper/pkg/analysis"
)

func setupCallSiteProject(t *testing.T, src string) *analysis.Engine {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module callsites\n\ngo 1.23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	engine, err := analysis.NewEngine(dir)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return engine
}

const callSiteSrc = `package callsites

import "io"

type res struct{}

func (r *res) Close() error { return nil }

func direct(r *res) {
	r.Close()
}

func twice(r *res) {
	r.Close()
	r.Close()
}

func viaInterface(c io.Closer) {
	c.Close()
}

func notACall(r *res) func() error {
	return r.Close
}
`

// TestFindCallSitesByPattern_MethodCall is the regression guard for #18:
// the function matched the pattern against *ast.FuncDecl while only
// expressions compile, so it could never return a match and every
// find_call_sites invocation reported zero.
func TestFindCallSitesByPattern_MethodCall(t *testing.T) {
	engine := setupCallSiteProject(t, callSiteSrc)
	ipc, err := engine.InterProcedural()
	if err != nil {
		t.Fatalf("InterProcedural: %v", err)
	}

	pat, err := Compile("$x.Close()")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	matches := FindCallSitesByPattern(engine, ipc, pat)
	if len(matches) == 0 {
		t.Fatal("no call sites found for $x.Close(); the documented example must work")
	}

	// direct() 1 + twice() 2 + viaInterface() 1 = at least 4 call sites.
	if len(matches) < 4 {
		t.Errorf("expected at least 4 call sites, got %d: %+v", len(matches), matches)
	}

	// Every match must carry a real location.
	locs := make(map[int]bool)
	for _, m := range matches {
		if m.File == "" || m.Line == 0 {
			t.Errorf("match with empty location: %+v", m)
		}
		if locs[m.Line] {
			t.Errorf("duplicate call site reported at line %d", m.Line)
		}
		locs[m.Line] = true
	}

	// `return r.Close` is a method value, not a call — it must not be reported.
	for _, m := range matches {
		if m.Line == 24 {
			t.Errorf("method value at line 24 reported as a call site")
		}
	}
}

// TestFindCallSitesByPattern_NoMatch pins that a pattern matching nothing
// returns nothing, rather than everything.
func TestFindCallSitesByPattern_NoMatch(t *testing.T) {
	engine := setupCallSiteProject(t, callSiteSrc)
	ipc, err := engine.InterProcedural()
	if err != nil {
		t.Fatalf("InterProcedural: %v", err)
	}
	pat, err := Compile("$x.Flush()")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if got := FindCallSitesByPattern(engine, ipc, pat); len(got) != 0 {
		t.Errorf("expected no matches for $x.Flush(), got %d: %+v", len(got), got)
	}
}
