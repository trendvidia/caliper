// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package gcl

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/trendvidia/caliper/pkg/analysis"
)

func setupProject(t *testing.T, files map[string]string) *analysis.Engine {
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
	return engine
}

func TestRun_SimpleRule(t *testing.T) {
	engine := setupProject(t, map[string]string{
		"code.go": `package testpkg

import "fmt"

func example() {
	fmt.Println("hello")
	fmt.Println("world")
}

func noprint() {
	x := 1
	_ = x
}
`,
	})

	data := []byte(`
version = 1
name = "simple"
rules = [{
  name = "find-println"
  severity = "info"
  match = "fmt.Println($args...)"
  message = "Found Println"
}]
`)
	loaded, err := LoadBytes(data, "test.gcl")
	if err != nil {
		t.Fatal(err)
	}

	results := Run(loaded, engine)
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].RuleName != "find-println" {
		t.Errorf("rule = %q", results[0].RuleName)
	}
	if len(results[0].Findings) != 2 {
		t.Errorf("findings = %d, want 2", len(results[0].Findings))
	}
}

func TestRun_AbsenceRule(t *testing.T) {
	engine := setupProject(t, map[string]string{
		"code.go": `package testpkg

func hasClose() {
	var x struct{ Open func(); Close func() }
	x.Open()
	x.Close()
}

func noClose() {
	var x struct{ Open func(); Close func() }
	x.Open()
}
`,
	})

	data := []byte(`
version = 1
name = "absence"
patterns = {
  open: { pattern = "$x.Open()" }
  close: { pattern = "$x.Close()" }
}
rules = [{
  name = "missing-close"
  severity = "warning"
  match = "@open"
  absent = "@close"
  message = "Open without Close"
}]
`)
	loaded, err := LoadBytes(data, "test.gcl")
	if err != nil {
		t.Fatal(err)
	}

	results := Run(loaded, engine)
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if len(results[0].Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(results[0].Findings))
	}

	finding := results[0].Findings[0]
	if finding.Severity != "warning" {
		t.Errorf("severity = %q", finding.Severity)
	}
}

func TestRun_AbsenceRule_NoFindings(t *testing.T) {
	engine := setupProject(t, map[string]string{
		"code.go": `package testpkg

func properlyClosing() {
	var x struct{ Open func(); Close func() }
	x.Open()
	x.Close()
}
`,
	})

	data := []byte(`
version = 1
name = "absence"
patterns = {
  open: { pattern = "$x.Open()" }
  close: { pattern = "$x.Close()" }
}
rules = [{
  name = "missing-close"
  match = "@open"
  absent = "@close"
}]
`)
	loaded, err := LoadBytes(data, "test.gcl")
	if err != nil {
		t.Fatal(err)
	}

	results := Run(loaded, engine)
	if len(results) != 0 {
		t.Errorf("expected no findings, got %d", len(results))
	}
}

func TestRun_ContainsRule(t *testing.T) {
	engine := setupProject(t, map[string]string{
		"code.go": `package testpkg

func usesBackground() {
	var x struct{ Open func(); Background func() }
	x.Open()
	x.Background()
}

func doesNotUseBackground() {
	var x struct{ Open func() }
	x.Open()
}
`,
	})

	data := []byte(`
version = 1
name = "contains"
rules = [{
  name = "open-with-background"
  match = "$x.Open()"
  contains = "$x.Background()"
  message = "Open with Background in same function"
}]
`)
	loaded, err := LoadBytes(data, "test.gcl")
	if err != nil {
		t.Fatal(err)
	}

	results := Run(loaded, engine)
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if len(results[0].Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(results[0].Findings))
	}
}

func TestRun_RuleFilterByName(t *testing.T) {
	engine := setupProject(t, map[string]string{
		"code.go": `package testpkg

import "fmt"

func example() {
	fmt.Println("hello")
	x := 1
	_ = x
}
`,
	})

	data := []byte(`
version = 1
name = "multi"
rules = [
  { name = "rule-a"  match = "fmt.Println($args...)" }
  { name = "rule-b"  match = "$x := 1" }
]
`)
	loaded, err := LoadBytes(data, "test.gcl")
	if err != nil {
		t.Fatal(err)
	}

	results := Run(loaded, engine)
	totalFindings := 0
	for _, r := range results {
		totalFindings += len(r.Findings)
	}
	if totalFindings < 2 {
		t.Errorf("expected at least 2 findings from 2 rules, got %d", totalFindings)
	}
}

func TestRun_SeverityPropagation(t *testing.T) {
	engine := setupProject(t, map[string]string{
		"code.go": `package testpkg

import "fmt"

func example() { fmt.Println("x") }
`,
	})

	data := []byte(`
version = 1
name = "severity"
rules = [{
  name = "error-level"
  severity = "error"
  match = "fmt.Println($args...)"
}]
`)
	loaded, err := LoadBytes(data, "test.gcl")
	if err != nil {
		t.Fatal(err)
	}

	results := Run(loaded, engine)
	if len(results) != 1 || len(results[0].Findings) != 1 {
		t.Fatal("expected 1 finding")
	}
	if results[0].Findings[0].Severity != "error" {
		t.Errorf("severity = %q, want error", results[0].Findings[0].Severity)
	}
}
