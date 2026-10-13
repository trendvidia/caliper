// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package transform

import (
	"strings"
	"testing"
)

func TestComputeUnifiedDiff_Basic(t *testing.T) {
	a := []string{
		"package main",
		"",
		"func main() {",
		"\tfmt.Println(\"hello\")",
		"}",
	}
	b := []string{
		"package main",
		"",
		"func main() {",
		"\tfmt.Println(\"world\")",
		"}",
	}

	diff := computeUnifiedDiff("main.go", a, b, 3)

	if diff == "" {
		t.Fatal("expected non-empty diff")
	}

	if !strings.Contains(diff, "--- a/main.go") {
		t.Error("expected --- header")
	}
	if !strings.Contains(diff, "+++ b/main.go") {
		t.Error("expected +++ header")
	}
	if !strings.Contains(diff, "-\tfmt.Println(\"hello\")") {
		t.Errorf("expected removed line, got:\n%s", diff)
	}
	if !strings.Contains(diff, "+\tfmt.Println(\"world\")") {
		t.Errorf("expected added line, got:\n%s", diff)
	}
}

func TestComputeUnifiedDiff_NoChanges(t *testing.T) {
	a := []string{"package main", "", "func main() {}"}
	diff := computeUnifiedDiff("main.go", a, a, 3)
	if diff != "" {
		t.Errorf("expected empty diff for identical files, got:\n%s", diff)
	}
}

func TestComputeUnifiedDiff_Addition(t *testing.T) {
	a := []string{"line1", "line2"}
	b := []string{"line1", "new line", "line2"}

	diff := computeUnifiedDiff("test.go", a, b, 1)

	if !strings.Contains(diff, "+new line") {
		t.Errorf("expected addition, got:\n%s", diff)
	}
}

func TestComputeUnifiedDiff_Deletion(t *testing.T) {
	a := []string{"line1", "to-remove", "line2"}
	b := []string{"line1", "line2"}

	diff := computeUnifiedDiff("test.go", a, b, 1)

	if !strings.Contains(diff, "-to-remove") {
		t.Errorf("expected deletion, got:\n%s", diff)
	}
}

func TestResult_UnifiedDiff(t *testing.T) {
	r := &Result{
		Changes: []Change{
			{
				File:   "/nonexistent/file.go",
				Line:   4,
				Before: "old code",
				After:  "new code",
			},
		},
		Preview: true,
	}

	diffs := r.UnifiedDiff(3)
	if len(diffs) == 0 {
		t.Fatal("expected at least one file diff")
	}
	if diffs[0].File != "/nonexistent/file.go" {
		t.Errorf("expected file path, got %q", diffs[0].File)
	}
	// Should produce synthetic diff since file doesn't exist.
	if !strings.Contains(diffs[0].Diff, "-old code") {
		t.Errorf("expected synthetic diff with old code, got:\n%s", diffs[0].Diff)
	}
	if !strings.Contains(diffs[0].Diff, "+new code") {
		t.Errorf("expected synthetic diff with new code, got:\n%s", diffs[0].Diff)
	}
}

func TestSplitLines(t *testing.T) {
	if lines := splitLines(""); lines != nil {
		t.Errorf("expected nil for empty, got %v", lines)
	}
	if lines := splitLines("a\nb"); len(lines) != 2 {
		t.Errorf("expected 2 lines, got %d", len(lines))
	}
}
