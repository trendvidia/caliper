// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_NoFile(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if cfg.DefaultPath() != "." {
		t.Errorf("expected default path '.', got %q", cfg.DefaultPath())
	}
	if !cfg.SkipGenerated() {
		t.Error("expected SkipGenerated true by default")
	}
}

func TestLoad_WithFile(t *testing.T) {
	dir := t.TempDir()
	pxfData := `
severity = {
  resource_leak: "error"
  unchecked_error: "warning"
}
rules {
  enable = ["resource_leak", "unchecked_error"]
  disable = ["lock_violation"]
}
engine {
  skip_generated = false
  build_tags = ["integration"]
  max_cache_entries = 16
}
defaults {
  path = "./cmd/..."
  preview = false
}
`
	if err := os.WriteFile(filepath.Join(dir, ".caliper.pxf"), []byte(pxfData), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.SeverityFor("resource_leak", "info") != "error" {
		t.Error("expected resource_leak severity override to 'error'")
	}
	if cfg.SeverityFor("unknown", "info") != "info" {
		t.Error("expected fallback to default severity")
	}

	if !cfg.RuleEnabled("resource_leak") {
		t.Error("expected resource_leak to be enabled")
	}
	if cfg.RuleEnabled("lock_violation") {
		t.Error("expected lock_violation to be disabled")
	}
	if cfg.RuleEnabled("goroutine_leak") {
		t.Error("expected goroutine_leak to be disabled (not in enable list)")
	}

	if cfg.SkipGenerated() {
		t.Error("expected SkipGenerated false")
	}

	if tags := cfg.BuildTags(); len(tags) != 1 || tags[0] != "integration" {
		t.Errorf("unexpected build tags: %v", tags)
	}

	if cfg.MaxCacheEntries() != 16 {
		t.Errorf("expected max_cache_entries 16, got %d", cfg.MaxCacheEntries())
	}

	if cfg.DefaultPath() != "./cmd/..." {
		t.Errorf("expected default path './cmd/...', got %q", cfg.DefaultPath())
	}
}

func TestLoad_ParentDirectory(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "sub", "dir")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}

	pxfData := `severity = { test_rule: "error" }`
	if err := os.WriteFile(filepath.Join(parent, ".caliper.pxf"), []byte(pxfData), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(child)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.SeverityFor("test_rule", "info") != "error" {
		t.Error("expected config loaded from parent directory")
	}
}

func TestRuleEnabled_NoConfig(t *testing.T) {
	var cfg *Config
	if !cfg.RuleEnabled("anything") {
		t.Error("nil config should enable all rules")
	}
}

func TestRuleEnabled_EmptyLists(t *testing.T) {
	cfg := &Config{pb: nil}
	if !cfg.RuleEnabled("anything") {
		t.Error("empty config should enable all rules")
	}
}

func TestGCLPatterns_Glob(t *testing.T) {
	dir := t.TempDir()
	patternsDir := filepath.Join(dir, "patterns")
	if err := os.MkdirAll(patternsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.gcl", "b.gcl"} {
		if err := os.WriteFile(filepath.Join(patternsDir, name), []byte("test"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	pxfData := `patterns = ["patterns/*.gcl"]`
	if err := os.WriteFile(filepath.Join(dir, ".caliper.pxf"), []byte(pxfData), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := cfg.GCLPatterns(dir)
	if len(files) != 2 {
		t.Errorf("GCLPatterns = %d files, want 2: %v", len(files), files)
	}
}

func TestGCLPatterns_NilConfig(t *testing.T) {
	var cfg *Config
	files := cfg.GCLPatterns("/tmp")
	if len(files) != 0 {
		t.Errorf("expected empty, got %v", files)
	}
}
