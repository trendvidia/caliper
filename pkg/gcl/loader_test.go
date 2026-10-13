// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package gcl

import (
	"path/filepath"
	"testing"
)

func TestLoad_SimpleGCL(t *testing.T) {
	path := filepath.Join("testdata", "simple.gcl")
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.Proto.Name != "simple" {
		t.Errorf("name = %q, want simple", loaded.Proto.Name)
	}
	if loaded.Proto.Version != 1 {
		t.Errorf("version = %d, want 1", loaded.Proto.Version)
	}
	if len(loaded.Rules) != 1 {
		t.Fatalf("rules = %d, want 1", len(loaded.Rules))
	}
	if loaded.Rules[0].Name != "find-println" {
		t.Errorf("rule name = %q", loaded.Rules[0].Name)
	}
	if loaded.Rules[0].MatchPattern == nil {
		t.Error("match pattern should be compiled")
	}
}

func TestLoad_ResourceSafetyGCL(t *testing.T) {
	path := filepath.Join("testdata", "resource_safety.gcl")
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.Proto.Name != "resource-safety" {
		t.Errorf("name = %q", loaded.Proto.Name)
	}
	if loaded.Patterns.Len() != 2 {
		t.Errorf("patterns = %d, want 2", loaded.Patterns.Len())
	}
	if len(loaded.Rules) != 1 {
		t.Fatalf("rules = %d, want 1", len(loaded.Rules))
	}

	rule := loaded.Rules[0]
	if rule.Name != "unclosed-file" {
		t.Errorf("rule name = %q", rule.Name)
	}
	if rule.MatchPattern == nil {
		t.Error("match pattern should be resolved from @open_call")
	}
	if rule.AbsentPattern == nil {
		t.Error("absent pattern should be resolved from @close_call")
	}
	if rule.Scope != "function" {
		t.Errorf("scope = %q, want function", rule.Scope)
	}
}

func TestLoadBytes_InvalidPXF(t *testing.T) {
	_, err := LoadBytes([]byte("{{invalid"), "test.gcl")
	if err == nil {
		t.Error("expected error for invalid PXF")
	}
}

func TestLoadBytes_UnsupportedVersion(t *testing.T) {
	data := []byte(`
version = 99
name = "test"
rules = [{ name = "r1", match = "$x" }]
`)
	_, err := LoadBytes(data, "test.gcl")
	if err == nil {
		t.Error("expected error for unsupported version")
	}
}

func TestLoadBytes_MissingName(t *testing.T) {
	data := []byte(`
version = 1
rules = [{ name = "r1", match = "$x" }]
`)
	_, err := LoadBytes(data, "test.gcl")
	if err == nil {
		t.Error("expected error for missing name")
	}
}

func TestLoadBytes_NoRules(t *testing.T) {
	data := []byte(`
version = 1
name = "empty"
`)
	_, err := LoadBytes(data, "test.gcl")
	if err == nil {
		t.Error("expected error for empty rules")
	}
}

func TestLoadBytes_UnknownPatternRef(t *testing.T) {
	data := []byte(`
version = 1
name = "bad-ref"
rules = [{ name = "r1", match = "@nonexistent" }]
`)
	_, err := LoadBytes(data, "test.gcl")
	if err == nil {
		t.Error("expected error for unknown @reference")
	}
}

func TestLoadBytes_InvalidInlinePattern(t *testing.T) {
	data := []byte(`
version = 1
name = "bad-pattern"
rules = [{ name = "r1", match = "$" }]
`)
	_, err := LoadBytes(data, "test.gcl")
	if err == nil {
		t.Error("expected error for invalid inline pattern")
	}
}

func TestLoadBytes_DefaultSeverityAndScope(t *testing.T) {
	data := []byte(`
version = 1
name = "defaults"
rules = [{ name = "r1"  match = "$x" }]
`)
	loaded, err := LoadBytes(data, "test.gcl")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Rules[0].Severity != "warning" {
		t.Errorf("severity = %q, want warning", loaded.Rules[0].Severity)
	}
	if loaded.Rules[0].Scope != "function" {
		t.Errorf("scope = %q, want function", loaded.Rules[0].Scope)
	}
}

func TestLoadBytes_WithNameConstraints(t *testing.T) {
	data := []byte(`
version = 1
name = "name-constrained"
patterns = {
  test_call: {
    pattern = "$fn($args...)"
    name_constraints = { fn: "^Test" }
  }
}
rules = [{ name = "find-tests"  match = "@test_call" }]
`)
	loaded, err := LoadBytes(data, "test.gcl")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Patterns.Len() != 1 {
		t.Errorf("patterns = %d, want 1", loaded.Patterns.Len())
	}
	entry, ok := loaded.Patterns.Get("test_call")
	if !ok {
		t.Fatal("expected test_call pattern")
	}
	if entry.Pattern == nil {
		t.Error("pattern should be compiled")
	}
}

func TestLoadBytes_ContainsRule(t *testing.T) {
	data := []byte(`
version = 1
name = "contains-test"
rules = [{
  name = "test-uses-background"
  match = "$x.Open()"
  contains = "$x.Background()"
  message = "Don't use context.Background()"
}]
`)
	loaded, err := LoadBytes(data, "test.gcl")
	if err != nil {
		t.Fatal(err)
	}
	rule := loaded.Rules[0]
	if rule.ContainsPattern == nil {
		t.Error("contains pattern should be compiled")
	}
	if rule.MatchPattern == nil {
		t.Error("match pattern should be compiled")
	}
}

func TestLoadBytes_InvalidNamedPatternRegex(t *testing.T) {
	data := []byte(`
version = 1
name = "bad-regex"
patterns = {
  bad: {
    pattern = "$x"
    name_constraints = { x: "[invalid" }
  }
}
rules = [{ name = "r1", match = "@bad" }]
`)
	_, err := LoadBytes(data, "test.gcl")
	if err == nil {
		t.Error("expected error for invalid regex in named pattern")
	}
}
