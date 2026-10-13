// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package cfg

import (
	"go/ast"
	"testing"
)

// TestBuild_DeferBlocksHoldOnlyCopies pins the invariant the templates'
// node scanners rely on to report each deferred call once: a BlockDefer
// block holds only defer statements, and each of them also appears
// exactly once in a non-defer block — the one that registers it. Skipping
// BlockDefer blocks therefore loses no statement, and iterating every
// block visits each deferred call twice.
func TestBuild_DeferBlocksHoldOnlyCopies(t *testing.T) {
	src := `package p

func f(c bool) {
	defer a()
	if c {
		defer b()
		return
	}
	for i := 0; i < 3; i++ {
		defer d(i)
	}
	e()
}
`
	g := buildGraphForPaths(t, src)

	inSource := make(map[ast.Node]int)
	inDefer := make(map[ast.Node]int)
	sourceDefers := 0
	for _, b := range g.Blocks {
		for _, n := range b.Nodes {
			if b.Kind == BlockDefer {
				inDefer[n]++
				continue
			}
			inSource[n]++
			if _, ok := n.(*ast.DeferStmt); ok {
				sourceDefers++
			}
		}
	}

	if len(inDefer) != 3 || sourceDefers != 3 {
		t.Fatalf("defer statements: %d in BlockDefer blocks, %d in other blocks; want 3 and 3", len(inDefer), sourceDefers)
	}
	for n, k := range inDefer {
		if _, ok := n.(*ast.DeferStmt); !ok {
			t.Errorf("a BlockDefer block holds a %T; want only defer statements", n)
		}
		if k != 1 {
			t.Errorf("defer at offset %d is in %d BlockDefer blocks; want 1", n.Pos(), k)
		}
		if inSource[n] != 1 {
			t.Errorf("defer at offset %d appears %d times outside BlockDefer blocks; want exactly 1, where it is registered", n.Pos(), inSource[n])
		}
	}
}
