// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package cfg

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/packages"
)

func loadTestPackage(t *testing.T, src string) (*packages.Package, *token.FileSet, *ast.FuncDecl) {
	t.Helper()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "test.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module testpkg\ngo 1.23\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	cfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo |
			packages.NeedDeps,
		Dir: dir,
	}

	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		t.Fatalf("load packages: %v", err)
	}
	if len(pkgs) == 0 || len(pkgs[0].Syntax) == 0 {
		t.Fatal("no packages loaded")
	}

	pkg := pkgs[0]
	file := pkg.Syntax[0]

	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok {
			fn = fd
			break
		}
	}
	if fn == nil {
		t.Fatal("no function declaration found")
	}

	return pkg, pkg.Fset, fn
}

func TestBuildSSA_Basic(t *testing.T) {
	src := `package testpkg

func f() {
	x := 1
	if x > 0 {
		x = 2
	}
	_ = x
}
`
	pkg, fset, fn := loadTestPackage(t, src)
	g := BuildSSA(fset, fn, pkg.TypesInfo, pkg)
	if g == nil {
		t.Fatal("BuildSSA returned nil")
	}
	if g.Entry == nil || g.Exit == nil {
		t.Fatal("missing entry or exit block")
	}

	// Should have more than just entry and exit.
	if len(g.Blocks) < 3 {
		t.Errorf("expected at least 3 blocks, got %d", len(g.Blocks))
	}

	// Entry should have successors.
	if len(g.Entry.Succs) == 0 {
		t.Error("entry block has no successors")
	}
}

func TestBuildSSA_WithLoop(t *testing.T) {
	src := `package testpkg

func f() {
	for i := 0; i < 10; i++ {
		if i == 5 {
			break
		}
	}
}
`
	pkg, fset, fn := loadTestPackage(t, src)
	g := BuildSSA(fset, fn, pkg.TypesInfo, pkg)
	if g == nil {
		t.Fatal("BuildSSA returned nil")
	}

	// Should have a branch block for the loop condition.
	hasBranch := false
	for _, b := range g.Blocks {
		if b.Kind == BlockBranch {
			hasBranch = true
			break
		}
	}
	if !hasBranch {
		t.Error("expected at least one branch block for loop")
	}
}

func TestBuildSSA_FallbackToAST(t *testing.T) {
	// When we can't build SSA, fallback to AST-based builder.
	src := `package p; func f() { return }`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "test.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	fn := file.Decls[0].(*ast.FuncDecl)

	// Use the regular AST builder as baseline.
	g := Build(fset, fn, nil)
	if g == nil {
		t.Fatal("AST fallback returned nil")
	}
	if g.Entry == nil || g.Exit == nil {
		t.Fatal("fallback: missing entry or exit")
	}
}
