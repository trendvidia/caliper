// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package templates

import (
	"strings"
	"testing"
)

// TestSourceNodes_DeferredCallReportedOnce pins that each rule scanning
// CFG nodes reports a deferred call once. The CFG lists a defer where it
// is registered and again where it runs, and before sourceNodes these
// rules reported both: measured on govm, unchecked_error with deferred
// calls included gave 162 findings of which 8 were exact duplicates, and
// on gameflow context_misuse gave 19 of which 9 were.
func TestSourceNodes_DeferredCallReportedOnce(t *testing.T) {
	src := `package testpkg

import (
	"context"
	"os"
)

func use(ctx context.Context) {}

func closeIt(f *os.File) {
	defer f.Close() // UNCHECKED
}

func spawn() {
	defer func() {
		go func() { // LEAK
			for {
			}
		}()
	}()
}

func todo() {
	defer use(context.TODO()) // TODO
}
`
	lineOf := func(marker string) int {
		for i, l := range strings.Split(src, "\n") {
			if strings.Contains(l, marker) {
				return i + 1
			}
		}
		t.Fatalf("marker %q not in source", marker)
		return 0
	}
	engine := setupProject(t, src)

	for _, tc := range []struct {
		name   string
		tmpl   Template
		kind   string
		marker string
	}{
		{"unchecked_error with defers", &UncheckedError{IncludeDefers: true}, "unchecked_error", "// UNCHECKED"},
		{"goroutine_leak in a deferred closure", &GoroutineLeak{}, "goroutine_leak", "// LEAK"},
		{"context_misuse TODO in a defer", &ContextMisuse{FlagTodo: true}, "context_misuse", "// TODO"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := lineOf(tc.marker)
			var lines []int
			for _, f := range findingsOfKind(RunTemplate(engine, tc.tmpl), tc.kind) {
				lines = append(lines, f.Location.Line)
			}
			if len(lines) != 1 || lines[0] != want {
				t.Errorf("%s findings at lines %v; want exactly one, at line %d", tc.kind, lines, want)
			}
		})
	}
}
