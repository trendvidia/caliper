// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package cfg

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func buildGraphForLoops(t *testing.T, src string) *Graph {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "test.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	fn := file.Decls[0].(*ast.FuncDecl)
	return Build(fset, fn, nil)
}

func TestFindLoops_SimpleFor(t *testing.T) {
	src := `package p; func f() {
		for i := 0; i < 10; i++ {
			work()
		}
	}`
	g := buildGraphForLoops(t, src)
	loops := FindLoops(g)
	if len(loops) == 0 {
		t.Fatal("expected at least one loop")
	}
	loop := loops[0]
	if loop.Header == nil {
		t.Fatal("loop header is nil")
	}
	if len(loop.BackEdges) == 0 {
		t.Error("expected at least one back-edge")
	}
	if len(loop.Body) < 2 {
		t.Errorf("expected at least 2 blocks in loop body, got %d", len(loop.Body))
	}
	if len(loop.Exit) == 0 {
		t.Error("expected at least one exit block")
	}
}

func TestFindLoops_InfiniteFor(t *testing.T) {
	src := `package p; func f() {
		for {
			work()
		}
	}`
	g := buildGraphForLoops(t, src)
	loops := FindLoops(g)
	if len(loops) == 0 {
		t.Fatal("expected at least one loop for infinite for-loop")
	}
}

func TestFindLoops_NestedLoops(t *testing.T) {
	src := `package p; func f() {
		for i := 0; i < 10; i++ {
			for j := 0; j < 10; j++ {
				work()
			}
		}
	}`
	g := buildGraphForLoops(t, src)
	loops := FindLoops(g)
	if len(loops) < 2 {
		t.Errorf("expected at least 2 loops for nested for-loops, got %d", len(loops))
	}
}

func TestFindLoops_GotoLoop(t *testing.T) {
	src := `package p; func f() {
	again:
		work()
		goto again
	}`
	g := buildGraphForLoops(t, src)
	loops := FindLoops(g)
	if len(loops) == 0 {
		t.Fatal("expected at least one loop from backward goto")
	}
}

func TestFindLoops_NoLoop(t *testing.T) {
	src := `package p; func f() {
		a()
		b()
		c()
	}`
	g := buildGraphForLoops(t, src)
	loops := FindLoops(g)
	if len(loops) != 0 {
		t.Errorf("expected no loops in straight-line code, got %d", len(loops))
	}
}

func TestFindLoops_ForRange(t *testing.T) {
	src := `package p; func f() {
		for _, v := range []int{1, 2, 3} {
			_ = v
		}
	}`
	g := buildGraphForLoops(t, src)
	loops := FindLoops(g)
	if len(loops) == 0 {
		t.Fatal("expected at least one loop for range loop")
	}
}

func TestClassifyIteration_InLoop(t *testing.T) {
	src := `package p; func f() {
		for i := 0; i < 10; i++ {
			work()
		}
	}`
	g := buildGraphForLoops(t, src)
	loops := FindLoops(g)
	if len(loops) == 0 {
		t.Fatal("expected at least one loop")
	}

	// The header block should be classified as FirstIteration.
	loop := loops[0]
	iter := ClassifyIteration(g, loop.Header)
	if iter != FirstIteration {
		t.Errorf("loop header: expected FirstIteration, got %d", iter)
	}
}

func TestClassifyIteration_NotInLoop(t *testing.T) {
	src := `package p; func f() {
		work()
	}`
	g := buildGraphForLoops(t, src)

	// Entry block is not in a loop.
	iter := ClassifyIteration(g, g.Entry)
	if iter != NotInLoop {
		t.Errorf("entry block: expected NotInLoop, got %d", iter)
	}
}

func TestFindLoops_ExitBlocks(t *testing.T) {
	src := `package p; func f() {
		for i := 0; i < 10; i++ {
			work()
		}
		done()
	}`
	g := buildGraphForLoops(t, src)
	loops := FindLoops(g)
	if len(loops) == 0 {
		t.Fatal("expected at least one loop")
	}
	loop := loops[0]
	// The header block (with condition i < 10) should be an exit block
	// because it has a successor outside the loop body (the "after" block).
	if len(loop.Exit) == 0 {
		t.Error("expected at least one exit block for bounded for-loop")
	}
}
