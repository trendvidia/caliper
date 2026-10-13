// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package patterns

import (
	"go/ast"
	"go/token"
	"testing"
)

func TestPatternSet_RegisterAndGet(t *testing.T) {
	ps := NewPatternSet()

	if err := ps.Register("close_call", "$x.Close()"); err != nil {
		t.Fatalf("register: %v", err)
	}

	entry, ok := ps.Get("close_call")
	if !ok {
		t.Fatal("expected to find close_call")
	}
	if entry.Name != "close_call" {
		t.Errorf("name = %q, want close_call", entry.Name)
	}
	if entry.Pattern == nil {
		t.Fatal("pattern should not be nil")
	}
}

func TestPatternSet_GetMissing(t *testing.T) {
	ps := NewPatternSet()
	if _, ok := ps.Get("nonexistent"); ok {
		t.Error("expected false for nonexistent pattern")
	}
}

func TestPatternSet_Names(t *testing.T) {
	ps := NewPatternSet()
	ps.Register("beta", "$x")
	ps.Register("alpha", "$y")
	ps.Register("gamma", "$z")

	names := ps.Names()
	if len(names) != 3 {
		t.Fatalf("len = %d, want 3", len(names))
	}
	if names[0] != "alpha" || names[1] != "beta" || names[2] != "gamma" {
		t.Errorf("names = %v, want [alpha beta gamma]", names)
	}
}

func TestPatternSet_WithDescription(t *testing.T) {
	ps := NewPatternSet()
	if err := ps.Register("test", "$x", POptDescription("a test pattern")); err != nil {
		t.Fatal(err)
	}
	entry, _ := ps.Get("test")
	if entry.Description != "a test pattern" {
		t.Errorf("description = %q", entry.Description)
	}
}

func TestPatternSet_WithTypeConstraints(t *testing.T) {
	ps := NewPatternSet()
	if err := ps.Register("typed", "$x", POptTypes(TypeConstraint{"x": "int"})); err != nil {
		t.Fatal(err)
	}
	entry, _ := ps.Get("typed")
	if entry.Pattern.typeConstraints == nil {
		t.Error("expected type constraints to be set")
	}
}

func TestPatternSet_WithNameConstraints(t *testing.T) {
	ps := NewPatternSet()
	if err := ps.Register("named", "$x", POptNames(NameConstraint{"x": "^test"})); err != nil {
		t.Fatal(err)
	}
	entry, _ := ps.Get("named")
	if entry.Pattern.nameConstraints == nil {
		t.Error("expected name constraints to be set")
	}

	// Verify the constraint works.
	match := &ast.Ident{Name: "testFoo"}
	if b := entry.Pattern.MatchNode(match, nil); b == nil {
		t.Error("expected match for testFoo")
	}
	noMatch := &ast.Ident{Name: "prodBar"}
	if b := entry.Pattern.MatchNode(noMatch, nil); b != nil {
		t.Error("expected no match for prodBar")
	}
}

func TestPatternSet_InvalidNameConstraint(t *testing.T) {
	ps := NewPatternSet()
	err := ps.Register("bad", "$x", POptNames(NameConstraint{"x": "[invalid"}))
	if err == nil {
		t.Error("expected error for invalid regex")
	}
}

func TestPatternSet_InvalidPattern(t *testing.T) {
	ps := NewPatternSet()
	err := ps.Register("bad", "$")
	if err == nil {
		t.Error("expected error for invalid pattern")
	}
}

func TestPatternSet_Negative(t *testing.T) {
	ps := NewPatternSet()
	if err := ps.Register("neg", "$x.Close()", POptNegative()); err != nil {
		t.Fatal(err)
	}
	entry, _ := ps.Get("neg")
	if !entry.Negative {
		t.Error("expected Negative = true")
	}
}

func TestPatternSet_MatchWithConstraints(t *testing.T) {
	ps := NewPatternSet()
	err := ps.Register("add_ints", "$a + $b",
		POptNames(NameConstraint{"a": "^x$"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := ps.Get("add_ints")

	// x + y matches (a=x satisfies ^x$)
	match := &ast.BinaryExpr{
		X: &ast.Ident{Name: "x"}, Op: token.ADD, Y: &ast.Ident{Name: "y"},
	}
	if b := entry.Pattern.MatchNode(match, nil); b == nil {
		t.Error("expected match for x + y")
	}

	// z + y does not match (a=z fails ^x$)
	noMatch := &ast.BinaryExpr{
		X: &ast.Ident{Name: "z"}, Op: token.ADD, Y: &ast.Ident{Name: "y"},
	}
	if b := entry.Pattern.MatchNode(noMatch, nil); b != nil {
		t.Error("expected no match for z + y")
	}
}

func TestPatternSet_Len(t *testing.T) {
	ps := NewPatternSet()
	if ps.Len() != 0 {
		t.Error("expected 0")
	}
	ps.Register("a", "$x")
	ps.Register("b", "$y")
	if ps.Len() != 2 {
		t.Errorf("len = %d, want 2", ps.Len())
	}
}
