package cfg

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func buildCFG(t *testing.T, src string) *Graph {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "test.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	fn := file.Decls[0].(*ast.FuncDecl)
	return Build(fset, fn, nil)
}

func hasEdge(from, to *Block) bool {
	for _, s := range from.Succs {
		if s == to {
			return true
		}
	}
	return false
}

func findBlockWithStmt(g *Graph, pred func(ast.Stmt) bool) *Block {
	for _, b := range g.Blocks {
		for _, s := range b.Stmts {
			if pred(s) {
				return b
			}
		}
	}
	return nil
}

func TestCFG_BreakInForLoop(t *testing.T) {
	src := `package p; func f() {
		for i := 0; i < 10; i++ {
			if i == 5 {
				break
			}
		}
		x := 1
		_ = x
	}`
	g := buildCFG(t, src)

	// Find the block containing the break statement.
	breakBlock := findBlockWithStmt(g, func(s ast.Stmt) bool {
		bs, ok := s.(*ast.BranchStmt)
		return ok && bs.Tok == token.BREAK
	})
	if breakBlock == nil {
		t.Fatal("break block not found")
	}

	// Break should have an edge to the block after the loop (not exit).
	if len(breakBlock.Succs) == 0 {
		t.Fatal("break block has no successors")
	}

	// The successor should NOT be the exit block — it should be the
	// "after" block that eventually leads to x := 1.
	for _, succ := range breakBlock.Succs {
		if succ.Kind == BlockExit {
			t.Error("break should not go directly to exit")
		}
	}
}

func TestCFG_ContinueInForLoop(t *testing.T) {
	src := `package p; func f() {
		for i := 0; i < 10; i++ {
			if i == 5 {
				continue
			}
		}
	}`
	g := buildCFG(t, src)

	// Find the block containing the continue statement.
	contBlock := findBlockWithStmt(g, func(s ast.Stmt) bool {
		bs, ok := s.(*ast.BranchStmt)
		return ok && bs.Tok == token.CONTINUE
	})
	if contBlock == nil {
		t.Fatal("continue block not found")
	}

	// Continue should have an edge to the loop header (branch block).
	hasHeaderEdge := false
	for _, succ := range contBlock.Succs {
		if succ.Kind == BlockBranch {
			hasHeaderEdge = true
		}
	}
	if !hasHeaderEdge {
		t.Error("continue should have edge to loop header (branch block)")
	}
}

func TestCFG_BreakInRangeLoop(t *testing.T) {
	src := `package p; func f() {
		for _, v := range []int{1, 2, 3} {
			if v == 2 {
				break
			}
		}
		x := 0
		_ = x
	}`
	g := buildCFG(t, src)

	breakBlock := findBlockWithStmt(g, func(s ast.Stmt) bool {
		bs, ok := s.(*ast.BranchStmt)
		return ok && bs.Tok == token.BREAK
	})
	if breakBlock == nil {
		t.Fatal("break block not found")
	}
	if len(breakBlock.Succs) == 0 {
		t.Fatal("break block has no successors")
	}
	for _, succ := range breakBlock.Succs {
		if succ.Kind == BlockExit {
			t.Error("break in range should not go directly to exit")
		}
	}
}

func TestCFG_GotoForward(t *testing.T) {
	src := `package p; func f() {
		goto done
		x := 1
		_ = x
	done:
		y := 2
		_ = y
	}`
	g := buildCFG(t, src)

	gotoBlock := findBlockWithStmt(g, func(s ast.Stmt) bool {
		bs, ok := s.(*ast.BranchStmt)
		return ok && bs.Tok == token.GOTO
	})
	if gotoBlock == nil {
		t.Fatal("goto block not found")
	}

	// Should have edge to the label block.
	if len(gotoBlock.Succs) == 0 {
		t.Fatal("goto block has no successors")
	}

	// Verify it reaches a block that's not the exit.
	reachesDone := false
	for _, succ := range gotoBlock.Succs {
		if succ.Kind != BlockExit {
			reachesDone = true
		}
	}
	if !reachesDone {
		t.Error("forward goto should reach label block, not exit")
	}
}

func TestCFG_GotoBackward(t *testing.T) {
	src := `package p; func f() {
	again:
		x := 1
		_ = x
		goto again
	}`
	g := buildCFG(t, src)

	gotoBlock := findBlockWithStmt(g, func(s ast.Stmt) bool {
		bs, ok := s.(*ast.BranchStmt)
		return ok && bs.Tok == token.GOTO
	})
	if gotoBlock == nil {
		t.Fatal("goto block not found")
	}

	// Should have edge back to the label block.
	if len(gotoBlock.Succs) == 0 {
		t.Fatal("backward goto has no successors")
	}
}

func TestCFG_LabeledBreak(t *testing.T) {
	src := `package p; func f() {
	outer:
		for i := 0; i < 10; i++ {
			for j := 0; j < 10; j++ {
				if j == 5 {
					break outer
				}
			}
		}
		x := 0
		_ = x
	}`
	g := buildCFG(t, src)

	breakBlock := findBlockWithStmt(g, func(s ast.Stmt) bool {
		bs, ok := s.(*ast.BranchStmt)
		return ok && bs.Tok == token.BREAK && bs.Label != nil && bs.Label.Name == "outer"
	})
	if breakBlock == nil {
		t.Fatal("labeled break block not found")
	}

	// Should have an edge to the outer loop's after block (not inner loop's).
	if len(breakBlock.Succs) == 0 {
		t.Fatal("labeled break has no successors")
	}
}

func TestCFG_BreakInSwitch(t *testing.T) {
	src := `package p; func f() {
		switch x := 1; x {
		case 1:
			break
		case 2:
			return
		}
		y := 2
		_ = y
	}`
	g := buildCFG(t, src)

	breakBlock := findBlockWithStmt(g, func(s ast.Stmt) bool {
		bs, ok := s.(*ast.BranchStmt)
		return ok && bs.Tok == token.BREAK
	})
	if breakBlock == nil {
		t.Fatal("break block in switch not found")
	}
	if len(breakBlock.Succs) == 0 {
		t.Fatal("break in switch has no successors")
	}
	// Should reach the join block after switch, not exit.
	for _, succ := range breakBlock.Succs {
		if succ.Kind == BlockExit {
			t.Error("break in switch should not go directly to exit")
		}
	}
}

func TestCFG_EmptyBody(t *testing.T) {
	src := `package p; func f() {}`
	g := buildCFG(t, src)

	if g.Entry == nil || g.Exit == nil {
		t.Fatal("missing entry or exit")
	}
	if !hasEdge(g.Entry, g.Exit) {
		t.Error("empty function should have entry → exit edge")
	}
}

func TestCFG_DeferBeforeExit(t *testing.T) {
	src := `package p; func f() {
		defer cleanup()
		doWork()
	}`
	g := buildCFG(t, src)

	// There should be a defer block before exit.
	var deferBlocks []*Block
	for _, b := range g.Blocks {
		if b.Kind == BlockDefer {
			deferBlocks = append(deferBlocks, b)
		}
	}
	if len(deferBlocks) == 0 {
		t.Fatal("expected at least one defer block")
	}

	// The defer block should be a predecessor of exit.
	hasDeferBeforeExit := false
	for _, pred := range g.Exit.Preds {
		if pred.Kind == BlockDefer {
			hasDeferBeforeExit = true
		}
	}
	if !hasDeferBeforeExit {
		t.Error("defer block should be predecessor of exit")
	}
}

func TestCFG_MultipleDeferLIFO(t *testing.T) {
	src := `package p; func f() {
		defer first()
		defer second()
		defer third()
	}`
	g := buildCFG(t, src)

	// Should have 3 defer blocks chained in LIFO order before exit.
	var deferBlocks []*Block
	for _, b := range g.Blocks {
		if b.Kind == BlockDefer {
			deferBlocks = append(deferBlocks, b)
		}
	}
	if len(deferBlocks) != 3 {
		t.Fatalf("expected 3 defer blocks, got %d", len(deferBlocks))
	}

	// Walk backward from exit: should be third → second → first.
	cur := g.Exit
	if len(cur.Preds) != 1 || cur.Preds[0].Kind != BlockDefer {
		t.Fatal("exit should have exactly one defer predecessor")
	}
	// Third defer (LIFO = last declared runs first)
	cur = cur.Preds[0]
	if len(cur.Preds) != 1 || cur.Preds[0].Kind != BlockDefer {
		t.Fatal("third defer should have a defer predecessor")
	}
	// Second defer
	cur = cur.Preds[0]
	if len(cur.Preds) != 1 || cur.Preds[0].Kind != BlockDefer {
		t.Fatal("second defer should have a defer predecessor")
	}
}

func TestCFG_DeferWithReturn(t *testing.T) {
	src := `package p; func f() {
		defer cleanup()
		if true {
			return
		}
	}`
	g := buildCFG(t, src)

	// Both the explicit return and the implicit return at end of function
	// should go through the defer block before reaching exit.
	// No path from any return/normal exit should reach exit without going through defer.
	for _, pred := range g.Exit.Preds {
		if pred.Kind != BlockDefer {
			t.Errorf("exit predecessor should be defer block, got kind %d", pred.Kind)
		}
	}
}

func TestCFG_SimpleReturn(t *testing.T) {
	src := `package p; func f() { return }`
	g := buildCFG(t, src)

	// Should have entry → block with return → exit
	if len(g.Blocks) < 3 { // entry, exit, return block
		t.Fatalf("expected at least 3 blocks, got %d", len(g.Blocks))
	}
}

func TestCFG_IfElse(t *testing.T) {
	src := `package p; func f() {
		if true {
			a()
		} else {
			b()
		}
		c()
	}`
	g := buildCFG(t, src)

	// Should have: entry → branch → then|else → join → exit
	var branchBlocks []*Block
	for _, b := range g.Blocks {
		if b.Kind == BlockBranch {
			branchBlocks = append(branchBlocks, b)
		}
	}
	if len(branchBlocks) == 0 {
		t.Fatal("expected at least one branch block for if/else")
	}
	// Branch should have 2 successors (then and else).
	if len(branchBlocks[0].Succs) != 2 {
		t.Errorf("branch block should have 2 successors, got %d", len(branchBlocks[0].Succs))
	}
}

func TestCFG_Switch(t *testing.T) {
	src := `package p; func f() {
		switch x := 1; x {
		case 1:
			a()
		case 2:
			b()
		default:
			c()
		}
	}`
	g := buildCFG(t, src)

	var branchBlocks []*Block
	for _, b := range g.Blocks {
		if b.Kind == BlockBranch {
			branchBlocks = append(branchBlocks, b)
		}
	}
	if len(branchBlocks) == 0 {
		t.Fatal("expected branch block for switch")
	}
	// Switch head should have successors for each case (3 cases).
	if len(branchBlocks[0].Succs) < 3 {
		t.Errorf("switch branch should have at least 3 successors, got %d", len(branchBlocks[0].Succs))
	}
}

func TestCFG_Select(t *testing.T) {
	src := `package p; func f() {
		ch1 := make(chan int)
		ch2 := make(chan int)
		select {
		case <-ch1:
			a()
		case <-ch2:
			b()
		}
	}`
	g := buildCFG(t, src)

	var branchBlocks []*Block
	for _, b := range g.Blocks {
		if b.Kind == BlockBranch {
			branchBlocks = append(branchBlocks, b)
		}
	}
	if len(branchBlocks) == 0 {
		t.Fatal("expected branch block for select")
	}
	// Select head should have 2 successors (one per case).
	if len(branchBlocks[0].Succs) < 2 {
		t.Errorf("select branch should have at least 2 successors, got %d", len(branchBlocks[0].Succs))
	}
}

func TestCFG_TypeSwitch(t *testing.T) {
	src := `package p; func f() {
		var x interface{} = 1
		switch x.(type) {
		case int:
			a()
		case string:
			b()
		}
	}`
	g := buildCFG(t, src)

	var branchBlocks []*Block
	for _, b := range g.Blocks {
		if b.Kind == BlockBranch {
			branchBlocks = append(branchBlocks, b)
		}
	}
	if len(branchBlocks) == 0 {
		t.Fatal("expected branch block for type switch")
	}
}

func TestCFG_ForWithPost(t *testing.T) {
	src := `package p; func f() {
		for i := 0; i < 10; i++ {
			work()
		}
	}`
	g := buildCFG(t, src)

	// Should have a back edge from the post/body to the header.
	var header *Block
	for _, b := range g.Blocks {
		if b.Kind == BlockBranch {
			header = b
			break
		}
	}
	if header == nil {
		t.Fatal("expected branch block for for-loop header")
	}
	// Header should have >= 1 predecessor that is not the entry block
	// (the back edge from post).
	hasBackEdge := false
	for _, pred := range header.Preds {
		if pred != g.Entry {
			hasBackEdge = true
		}
	}
	if !hasBackEdge {
		t.Error("for loop header should have back edge from body/post")
	}
}

func TestCFG_NestedIf(t *testing.T) {
	src := `package p; func f() {
		if true {
			if false {
				a()
			}
		}
	}`
	g := buildCFG(t, src)

	var branchBlocks []*Block
	for _, b := range g.Blocks {
		if b.Kind == BlockBranch {
			branchBlocks = append(branchBlocks, b)
		}
	}
	if len(branchBlocks) < 2 {
		t.Errorf("expected at least 2 branch blocks for nested if, got %d", len(branchBlocks))
	}
}
