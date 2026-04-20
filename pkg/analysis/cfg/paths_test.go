package cfg

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"
)

func buildGraphForPaths(t *testing.T, src string) *Graph {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "test.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	fn := file.Decls[0].(*ast.FuncDecl)
	return Build(fset, fn, nil)
}

// predCallTo returns a predicate that matches call expressions to a function name.
func predCallTo(name string) NodePredicate {
	return func(block *Block, node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return false
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			return fn.Name == name
		case *ast.SelectorExpr:
			return fn.Sel.Name == name
		}
		return false
	}
}

func TestReachableAll_NoViolation(t *testing.T) {
	// close() is called on all paths.
	src := `package p; func f() {
		open()
		close()
	}`
	g := buildGraphForPaths(t, src)
	violations := Reachable(g, g.Entry, predCallTo("close"), AllPaths)
	if len(violations) != 0 {
		t.Errorf("expected no violations, got %d", len(violations))
	}
}

func TestReachableAll_Violation(t *testing.T) {
	// close() not called on else branch.
	src := `package p; func f() {
		open()
		if true {
			close()
		}
	}`
	g := buildGraphForPaths(t, src)
	violations := Reachable(g, g.Entry, predCallTo("close"), AllPaths)
	if len(violations) == 0 {
		t.Error("expected violations for missing close on else path")
	}
}

func TestReachableAny_Found(t *testing.T) {
	src := `package p; func f() {
		if true {
			dangerous()
		}
	}`
	g := buildGraphForPaths(t, src)
	results := Reachable(g, g.Entry, predCallTo("dangerous"), AnyPath)
	if len(results) == 0 {
		t.Error("expected to find dangerous() on some path")
	}
}

func TestNeverBetween_NoViolation(t *testing.T) {
	// No blocking call between lock and unlock.
	src := `package p; func f() {
		lock()
		work()
		unlock()
	}`
	g := buildGraphForPaths(t, src)
	violations := NeverBetween(g, predCallTo("lock"), predCallTo("unlock"), predCallTo("blocking"))
	if len(violations) != 0 {
		t.Errorf("expected no violations, got %d", len(violations))
	}
}

func TestAlwaysBefore_Violation(t *testing.T) {
	// use() before init() on some path.
	src := `package p; func f() {
		if true {
			use()
		} else {
			init()
			use()
		}
	}`
	g := buildGraphForPaths(t, src)
	violations := AlwaysBefore(g, predCallTo("init"), predCallTo("use"))
	if len(violations) == 0 {
		t.Error("expected violation: use() reached before init() on true branch")
	}
}

func TestDominates_True(t *testing.T) {
	// check() is before the if, use() is after — check dominates use.
	// Using an if to force them into different blocks.
	src := `package p; func f() {
		check()
		if true {
			use()
		}
	}`
	g := buildGraphForPaths(t, src)
	if !Dominates(g, predCallTo("check"), predCallTo("use")) {
		t.Error("check should dominate use")
	}
}

func TestDominates_False(t *testing.T) {
	src := `package p; func f() {
		if true {
			check()
		}
		use()
	}`
	g := buildGraphForPaths(t, src)
	if Dominates(g, predCallTo("check"), predCallTo("use")) {
		t.Error("check should NOT dominate use — use reachable without check")
	}
}

func TestCycleDetection_InfiniteLoop(t *testing.T) {
	// Infinite loop — path traversal should terminate.
	src := `package p; func f() {
		for {
			work()
		}
	}`
	g := buildGraphForPaths(t, src)

	// This should not hang — cycle detection prevents infinite traversal.
	done := make(chan struct{})
	go func() {
		Reachable(g, g.Entry, predCallTo("missing"), AllPaths)
		close(done)
	}()

	select {
	case <-done:
		// Good — completed without hanging.
	default:
		// Give it a moment.
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Reachable hung on infinite loop CFG — cycle detection failed")
		}
	}
}

func TestCycleDetection_LoopWithBreak(t *testing.T) {
	src := `package p; func f() {
		for i := 0; i < 10; i++ {
			if i == 5 {
				cleanup()
				break
			}
		}
	}`
	g := buildGraphForPaths(t, src)

	// Should find cleanup on some path.
	results := Reachable(g, g.Entry, predCallTo("cleanup"), AnyPath)
	if len(results) == 0 {
		t.Error("expected to find cleanup() on some path")
	}
}

func TestCycleDetection_GotoLoop(t *testing.T) {
	// Backward goto creates a cycle.
	src := `package p; func f() {
	again:
		work()
		goto again
	}`
	g := buildGraphForPaths(t, src)

	done := make(chan struct{})
	go func() {
		Reachable(g, g.Entry, predCallTo("missing"), AllPaths)
		close(done)
	}()

	select {
	case <-done:
	default:
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Reachable hung on goto loop — cycle detection failed")
		}
	}
}

func TestNeverBetween_Violation(t *testing.T) {
	// blocking() appears between lock() and unlock().
	src := `package p; func f() {
		lock()
		if true {
			blocking()
		}
		unlock()
	}`
	g := buildGraphForPaths(t, src)
	violations := NeverBetween(g, predCallTo("lock"), predCallTo("unlock"), predCallTo("blocking"))
	if len(violations) == 0 {
		t.Error("expected violation: blocking() between lock() and unlock()")
	}
}

func TestAlwaysBefore_NoViolation(t *testing.T) {
	// init() is always called before use() on all paths.
	src := `package p; func f() {
		init()
		if true {
			use()
		} else {
			use()
		}
	}`
	g := buildGraphForPaths(t, src)
	violations := AlwaysBefore(g, predCallTo("init"), predCallTo("use"))
	if len(violations) != 0 {
		t.Errorf("expected no violations, got %d", len(violations))
	}
}

func TestReachableAny_NotFound(t *testing.T) {
	// missing() is never called.
	src := `package p; func f() {
		a()
		b()
	}`
	g := buildGraphForPaths(t, src)
	results := Reachable(g, g.Entry, predCallTo("missing"), AnyPath)
	if len(results) != 0 {
		t.Errorf("expected 0 results for missing function, got %d", len(results))
	}
}

func TestReachableAll_MultipleExitPaths(t *testing.T) {
	// cleanup() called on both return paths.
	src := `package p; func f() {
		if true {
			cleanup()
			return
		}
		cleanup()
	}`
	g := buildGraphForPaths(t, src)
	violations := Reachable(g, g.Entry, predCallTo("cleanup"), AllPaths)
	if len(violations) != 0 {
		t.Errorf("expected no violations, got %d", len(violations))
	}
}

func TestDominates_EntryDominatesAll(t *testing.T) {
	// Entry-level call dominates everything after it.
	src := `package p; func f() {
		setup()
		if true {
			work()
		}
	}`
	g := buildGraphForPaths(t, src)
	if !Dominates(g, predCallTo("setup"), predCallTo("work")) {
		t.Error("setup should dominate work")
	}
}

func TestDominates_NeitherExists(t *testing.T) {
	src := `package p; func f() {
		a()
	}`
	g := buildGraphForPaths(t, src)
	// Neither "missing1" nor "missing2" exist → should return false.
	if Dominates(g, predCallTo("missing1"), predCallTo("missing2")) {
		t.Error("should return false when neither predicate matches")
	}
}

// --------------------------------------------------------------------------
// Invariant tests
// --------------------------------------------------------------------------

func TestInvariant_AllBlocksSatisfy(t *testing.T) {
	// No dangerous() call anywhere — safety predicate "no dangerous()" holds globally.
	src := `package p; func f() {
		work()
		if true {
			work()
		} else {
			work()
		}
	}`
	g := buildGraphForPaths(t, src)

	// Safety predicate: returns true for all safe nodes, false for dangerous ones.
	noDangerous := func(block *Block, node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok {
			return ident.Name != "dangerous"
		}
		return true
	}

	violations := Invariant(g, g.Entry, noDangerous)
	if len(violations) != 0 {
		t.Errorf("expected no violations, got %d", len(violations))
	}
}

func TestInvariant_SomeBlockFails(t *testing.T) {
	// dangerous() violates the safety predicate "no dangerous calls".
	src := `package p; func f() {
		work()
		if true {
			dangerous()
		} else {
			work()
		}
	}`
	g := buildGraphForPaths(t, src)

	// Safety predicate: returns false when a dangerous() call is found.
	noDangerous := func(block *Block, node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok {
			return ident.Name != "dangerous"
		}
		return true
	}

	violations := Invariant(g, g.Entry, noDangerous)
	if len(violations) == 0 {
		t.Error("expected violations: dangerous() block violates the safety predicate")
	}
}

func TestInvariant_ViaReachable(t *testing.T) {
	// Invariant should be callable through Reachable with Globally quantifier.
	src := `package p; func f() {
		work()
		if true {
			dangerous()
		} else {
			work()
		}
	}`
	g := buildGraphForPaths(t, src)

	// Safety predicate: returns false when a dangerous() call is found.
	noDangerous := func(block *Block, node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok {
			return ident.Name != "dangerous"
		}
		return true
	}

	violations := Reachable(g, g.Entry, noDangerous, Globally)
	if len(violations) == 0 {
		t.Error("expected violations via Reachable with Globally quantifier")
	}
}

func TestInvariant_EmptyFunction(t *testing.T) {
	// Empty function — no blocks with nodes, so no violations.
	src := `package p; func f() {}`
	g := buildGraphForPaths(t, src)
	violations := Invariant(g, g.Entry, predCallTo("work"))
	if len(violations) != 0 {
		t.Errorf("expected no violations for empty function, got %d", len(violations))
	}
}

// --------------------------------------------------------------------------
// SubgraphBetween tests
// --------------------------------------------------------------------------

func TestSubgraphBetween_Basic(t *testing.T) {
	src := `package p; func f() {
		lock()
		if true {
			blocking()
		}
		unlock()
	}`
	g := buildGraphForPaths(t, src)
	sub := SubgraphBetween(g, predCallTo("lock"), predCallTo("unlock"))
	// sub should have entry, exit, and blocks between lock and unlock.
	if sub == nil {
		t.Fatal("SubgraphBetween returned nil")
	}
	if len(sub.Blocks) < 3 { // at least entry, exit, and one block
		t.Errorf("expected at least 3 blocks in subgraph, got %d", len(sub.Blocks))
	}
}

func TestSubgraphBetween_NoFromMatch(t *testing.T) {
	src := `package p; func f() {
		work()
		unlock()
	}`
	g := buildGraphForPaths(t, src)
	sub := SubgraphBetween(g, predCallTo("lock"), predCallTo("unlock"))
	if sub != nil {
		t.Error("expected nil when from predicate has no matches")
	}
}

func TestSubgraphBetween_NoToMatch(t *testing.T) {
	src := `package p; func f() {
		lock()
		work()
	}`
	g := buildGraphForPaths(t, src)
	sub := SubgraphBetween(g, predCallTo("lock"), predCallTo("missing"))
	if sub != nil {
		t.Error("expected nil when to predicate has no matches")
	}
}

func TestSubgraphBetween_ComposedWithInvariant(t *testing.T) {
	// Compose: extract lock->unlock subgraph, then check no blocking() in it.
	src := `package p; func f() {
		lock()
		if true {
			blocking()
		}
		unlock()
	}`
	g := buildGraphForPaths(t, src)
	sub := SubgraphBetween(g, predCallTo("lock"), predCallTo("unlock"))
	if sub == nil {
		t.Fatal("SubgraphBetween returned nil")
	}

	// Create predicate that is satisfied when there's NO blocking call.
	noBlocking := func(block *Block, node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true // non-call nodes are fine
		}
		if ident, ok := call.Fun.(*ast.Ident); ok {
			return ident.Name != "blocking"
		}
		return true
	}

	violations := Invariant(sub, sub.Entry, noBlocking)
	if len(violations) == 0 {
		t.Error("expected violations: blocking() appears in lock->unlock subgraph")
	}
}
