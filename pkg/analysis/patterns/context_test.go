package patterns

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// parseTestFile parses source and returns the file. Context predicates
// are purely syntactic (no type info needed), so tests parse directly
// instead of loading packages.
func parseTestFile(t *testing.T, src string) *ast.File {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "test.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return file
}

// matchLocations runs pat over file with the given filter and returns
// the matched nodes.
func filteredMatches(t *testing.T, file *ast.File, patternStr string, filter *ContextFilter) []ast.Node {
	t.Helper()
	pat, err := Compile(patternStr)
	if err != nil {
		t.Fatalf("compile %q: %v", patternStr, err)
	}
	var out []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		if pat.MatchNode(n, nil) == nil {
			return true
		}
		if filter.Allows(file, n, nil) {
			out = append(out, n)
		}
		return true
	})
	return out
}

const contextFixture = `package p

func direct(w Widget) {
	w.SetText("direct")
}

func onGoroutine(w Widget) {
	go func() {
		w.SetText("goroutine")
	}()
}

func onTimer(w Widget) {
	time.AfterFunc(time.Second, func() {
		w.SetText("timer")
	})
}

func wrapped(w Widget) {
	go func() {
		ui.Do(func() {
			w.SetText("wrapped")
		})
	}()
}

func deferred(w Widget) {
	defer w.SetText("deferred")
}
`

func TestContextFilter_InsideGo(t *testing.T) {
	file := parseTestFile(t, contextFixture)
	spec, err := ParseContextSpec("go")
	if err != nil {
		t.Fatal(err)
	}
	matches := filteredMatches(t, file, `$w.SetText($msg)`, &ContextFilter{Inside: []*ContextSpec{spec}})
	// onGoroutine and wrapped are inside go statements.
	if len(matches) != 2 {
		t.Fatalf("inside go: got %d matches, want 2", len(matches))
	}
}

func TestContextFilter_InsideDefer(t *testing.T) {
	file := parseTestFile(t, contextFixture)
	spec, err := ParseContextSpec("defer")
	if err != nil {
		t.Fatal(err)
	}
	matches := filteredMatches(t, file, `$w.SetText($msg)`, &ContextFilter{Inside: []*ContextSpec{spec}})
	if len(matches) != 1 {
		t.Fatalf("inside defer: got %d matches, want 1", len(matches))
	}
}

func TestContextFilter_CallbackOf(t *testing.T) {
	file := parseTestFile(t, contextFixture)

	// Full call pattern.
	spec, err := ParseContextSpec("callback_of:time.AfterFunc($d, $f)")
	if err != nil {
		t.Fatal(err)
	}
	matches := filteredMatches(t, file, `$w.SetText($msg)`, &ContextFilter{Inside: []*ContextSpec{spec}})
	if len(matches) != 1 {
		t.Fatalf("callback_of full pattern: got %d matches, want 1", len(matches))
	}

	// Bare callee form.
	spec, err = ParseContextSpec("callback_of:time.AfterFunc")
	if err != nil {
		t.Fatal(err)
	}
	matches = filteredMatches(t, file, `$w.SetText($msg)`, &ContextFilter{Inside: []*ContextSpec{spec}})
	if len(matches) != 1 {
		t.Fatalf("callback_of bare callee: got %d matches, want 1", len(matches))
	}
}

func TestContextFilter_NotInsideCall(t *testing.T) {
	file := parseTestFile(t, contextFixture)
	goSpec, err := ParseContextSpec("go")
	if err != nil {
		t.Fatal(err)
	}
	timerSpec, err := ParseContextSpec("callback_of:time.AfterFunc")
	if err != nil {
		t.Fatal(err)
	}
	doSpec, err := ParseContextSpec("call:ui.Do")
	if err != nil {
		t.Fatal(err)
	}

	// The issue-#8 goroutine-discipline rule: mutation inside go/AfterFunc
	// contexts, minus those already wrapped in ui.Do.
	filter := &ContextFilter{
		Inside:    []*ContextSpec{goSpec, timerSpec},
		NotInside: []*ContextSpec{doSpec},
	}
	matches := filteredMatches(t, file, `$w.SetText($msg)`, filter)
	// onGoroutine + onTimer violate; wrapped is excused by ui.Do; direct
	// and deferred are outside the Inside contexts.
	if len(matches) != 2 {
		t.Fatalf("goroutine-discipline rule: got %d matches, want 2", len(matches))
	}
}

func TestContextFilter_EmptyAllowsAll(t *testing.T) {
	file := parseTestFile(t, contextFixture)
	matches := filteredMatches(t, file, `$w.SetText($msg)`, nil)
	if len(matches) != 5 {
		t.Fatalf("nil filter: got %d matches, want 5", len(matches))
	}
}

func TestParseContextSpec_Errors(t *testing.T) {
	for _, bad := range []string{"", "goroutine", "inside:go", "callback_of:", "call:$$$"} {
		if _, err := ParseContextSpec(bad); err == nil {
			t.Errorf("ParseContextSpec(%q): expected error, got nil", bad)
		}
	}
}
