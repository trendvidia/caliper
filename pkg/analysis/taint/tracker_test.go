// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package taint

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/trendvidia/caliper/pkg/analysis"
)

// setupTaintProject creates a temp Go project and returns an Engine and
// InterProceduralContext ready for taint analysis.
func setupTaintProject(t *testing.T, files map[string]string) (*analysis.Engine, *analysis.InterProceduralContext) {
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

// --------------------------------------------------------------------------
// Test: direct flow (source parameter -> sink call)
// --------------------------------------------------------------------------

func TestDirectFlow(t *testing.T) {
	engine, ipc := setupTaintProject(t, map[string]string{
		"main.go": `package testpkg

type Request struct{ Input string }

type DB struct{}

func (db *DB) Query(q string) {}

func handler(r *Request) {
	db := &DB{}
	db.Query(r.Input)
}
`,
	})

	config := TaintConfig{
		Sources: []Source{{Package: "testpkg", Type: "Request"}},
		Sinks:   []Sink{{Package: "testpkg", Function: "Query", ArgIndex: -1}},
	}

	tracker := NewTracker(engine, ipc, config)
	flows := tracker.Analyze()

	if len(flows) == 0 {
		t.Fatal("expected at least one taint flow, got none")
	}

	// Verify the flow is not marked as sanitized.
	for _, f := range flows {
		if f.Sanitized {
			t.Error("expected unsanitized flow")
		}
	}
}

// --------------------------------------------------------------------------
// Test: sanitized flow (source -> sanitizer -> sink = no report)
// --------------------------------------------------------------------------

func TestSanitizedFlow(t *testing.T) {
	engine, ipc := setupTaintProject(t, map[string]string{
		"main.go": `package testpkg

type Request struct{ Input string }

type DB struct{}

func (db *DB) Query(q string) {}

func sanitize(s string) string { return s }

func handler(r *Request) {
	db := &DB{}
	clean := sanitize(r.Input)
	db.Query(clean)
}
`,
	})

	config := TaintConfig{
		Sources:    []Source{{Package: "testpkg", Type: "Request"}},
		Sinks:      []Sink{{Package: "testpkg", Function: "Query", ArgIndex: -1}},
		Sanitizers: []Sanitizer{{Package: "testpkg", Function: "sanitize"}},
	}

	tracker := NewTracker(engine, ipc, config)
	flows := tracker.Analyze()

	// All flows should be filtered out (sanitized).
	if len(flows) != 0 {
		t.Errorf("expected no unsanitized flows, got %d", len(flows))
		for i, f := range flows {
			t.Logf("  flow %d: source=%+v sink=%+v sanitized=%v steps=%d",
				i, f.Source, f.Sink, f.Sanitized, len(f.Path))
		}
	}
}

// --------------------------------------------------------------------------
// Test: cross-function flow (source -> helper -> sink)
// --------------------------------------------------------------------------

func TestCrossFunctionFlow(t *testing.T) {
	engine, ipc := setupTaintProject(t, map[string]string{
		"main.go": `package testpkg

type Request struct{ Input string }

type DB struct{}

func (db *DB) Query(q string) {}

func runQuery(db *DB, q string) {
	db.Query(q)
}

func handler(r *Request) {
	db := &DB{}
	runQuery(db, r.Input)
}
`,
	})

	config := TaintConfig{
		Sources: []Source{{Package: "testpkg", Type: "Request"}},
		Sinks:   []Sink{{Package: "testpkg", Function: "Query", ArgIndex: -1}},
	}

	tracker := NewTracker(engine, ipc, config)
	flows := tracker.Analyze()

	if len(flows) == 0 {
		t.Fatal("expected at least one cross-function taint flow, got none")
	}

	// The path should have multiple steps showing the cross-function hop.
	for _, f := range flows {
		if len(f.Path) < 2 {
			t.Errorf("expected multi-step path, got %d steps", len(f.Path))
		}
	}
}

// --------------------------------------------------------------------------
// Test: no sources means no flows
// --------------------------------------------------------------------------

func TestNoSources(t *testing.T) {
	engine, ipc := setupTaintProject(t, map[string]string{
		"main.go": `package testpkg

type DB struct{}

func (db *DB) Query(q string) {}

func handler(q string) {
	db := &DB{}
	db.Query(q)
}
`,
	})

	config := TaintConfig{
		Sources: []Source{{Package: "testpkg", Type: "Request"}},
		Sinks:   []Sink{{Package: "testpkg", Function: "Query", ArgIndex: -1}},
	}

	tracker := NewTracker(engine, ipc, config)
	flows := tracker.Analyze()

	if len(flows) != 0 {
		t.Errorf("expected no flows when source type is absent, got %d", len(flows))
	}
}

// --------------------------------------------------------------------------
// Test: multiple flows in the same program
// --------------------------------------------------------------------------

func TestMultipleFlows(t *testing.T) {
	engine, ipc := setupTaintProject(t, map[string]string{
		"main.go": `package testpkg

type Request struct{ Input string }

type DB struct{}

func (db *DB) Query(q string) {}
func (db *DB) Exec(q string)  {}

func handler(r *Request) {
	db := &DB{}
	db.Query(r.Input)
	db.Exec(r.Input)
}
`,
	})

	config := TaintConfig{
		Sources: []Source{{Package: "testpkg", Type: "Request"}},
		Sinks: []Sink{
			{Package: "testpkg", Function: "Query", ArgIndex: -1},
			{Package: "testpkg", Function: "Exec", ArgIndex: -1},
		},
	}

	tracker := NewTracker(engine, ipc, config)
	flows := tracker.Analyze()

	if len(flows) < 2 {
		t.Errorf("expected at least 2 taint flows, got %d", len(flows))
	}

	// Check that we have flows to both sinks.
	sinkFuncs := make(map[string]bool)
	for _, f := range flows {
		for _, step := range f.Path {
			if step.Kind == "call_arg" {
				sinkFuncs[step.Description] = true
			}
		}
	}
	// We expect descriptions mentioning both Query and Exec.
	if len(sinkFuncs) < 2 {
		t.Errorf("expected flows to multiple sinks, got descriptions: %v", sinkFuncs)
	}
}

// --------------------------------------------------------------------------
// Test: pointer source type matching
// --------------------------------------------------------------------------

func TestPointerSourceType(t *testing.T) {
	engine, ipc := setupTaintProject(t, map[string]string{
		"main.go": `package testpkg

type Request struct{ Input string }

type DB struct{}

func (db *DB) Query(q string) {}

func handler(r *Request) {
	db := &DB{}
	db.Query(r.Input)
}
`,
	})

	// Use the pointer form in the source config.
	config := TaintConfig{
		Sources: []Source{{Package: "testpkg", Type: "*Request"}},
		Sinks:   []Sink{{Package: "testpkg", Function: "Query", ArgIndex: -1}},
	}

	tracker := NewTracker(engine, ipc, config)
	flows := tracker.Analyze()

	if len(flows) == 0 {
		t.Fatal("expected taint flow with pointer source type, got none")
	}
}
