// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package cfg

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func buildGraphForContext(t *testing.T, src string) *Graph {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "test.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			return Build(fset, fn, nil)
		}
	}
	t.Fatal("no function declaration found")
	return nil
}

func TestTraceContextFlow_HasParentContext(t *testing.T) {
	src := `package p

import "context"

func f(ctx context.Context) {
	_ = ctx
}`
	g := buildGraphForContext(t, src)
	result := TraceContextFlow(g, nil)
	if !result.HasParentContext {
		t.Error("expected HasParentContext to be true")
	}
}

func TestTraceContextFlow_NoParentContext(t *testing.T) {
	src := `package p; func f(x int) {
	_ = x
}`
	g := buildGraphForContext(t, src)
	result := TraceContextFlow(g, nil)
	if result.HasParentContext {
		t.Error("expected HasParentContext to be false")
	}
}

func TestTraceContextFlow_CreatesBackground(t *testing.T) {
	src := `package p

import "context"

func f() {
	ctx := context.Background()
	_ = ctx
}`
	g := buildGraphForContext(t, src)
	result := TraceContextFlow(g, nil)
	if !result.CreatesBackground {
		t.Error("expected CreatesBackground to be true")
	}
}

func TestTraceContextFlow_CreatesTodo(t *testing.T) {
	src := `package p

import "context"

func f() {
	ctx := context.TODO()
	_ = ctx
}`
	g := buildGraphForContext(t, src)
	result := TraceContextFlow(g, nil)
	if !result.CreatesTodo {
		t.Error("expected CreatesTodo to be true")
	}
}

func TestTraceContextFlow_DerivesContext(t *testing.T) {
	src := `package p

import "context"

func f() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = ctx
}`
	g := buildGraphForContext(t, src)
	result := TraceContextFlow(g, nil)
	if !result.DerivesContext {
		t.Error("expected DerivesContext to be true")
	}
	if !result.CreatesBackground {
		t.Error("expected CreatesBackground to be true")
	}
}

func TestTraceContextFlow_NoContextCalls(t *testing.T) {
	src := `package p; func f() {
	x := 1
	_ = x
}`
	g := buildGraphForContext(t, src)
	result := TraceContextFlow(g, nil)
	if result.HasParentContext {
		t.Error("expected HasParentContext to be false")
	}
	if result.CreatesBackground {
		t.Error("expected CreatesBackground to be false")
	}
	if result.CreatesTodo {
		t.Error("expected CreatesTodo to be false")
	}
	if result.DerivesContext {
		t.Error("expected DerivesContext to be false")
	}
}

func TestTraceContextFlow_MissingCancel(t *testing.T) {
	// cancel is assigned but never called — should produce violations.
	src := `package p

import "context"

func f() {
	ctx, cancel := context.WithCancel(context.Background())
	_ = ctx
	_ = cancel
}`
	g := buildGraphForContext(t, src)
	result := TraceContextFlow(g, nil)
	if !result.DerivesContext {
		t.Error("expected DerivesContext to be true")
	}
	if len(result.CancellationPaths) == 0 {
		t.Error("expected cancellation violations: cancel() is never called")
	}
}

func TestTraceContextFlow_CancelCalledProperly(t *testing.T) {
	// cancel() is called via defer — should produce no violations.
	src := `package p

import "context"

func f() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = ctx
}`
	g := buildGraphForContext(t, src)
	result := TraceContextFlow(g, nil)
	if len(result.CancellationPaths) != 0 {
		t.Errorf("expected no cancellation violations, got %d", len(result.CancellationPaths))
	}
}

func TestTraceContextFlow_EmptyFunction(t *testing.T) {
	src := `package p; func f() {}`
	g := buildGraphForContext(t, src)
	result := TraceContextFlow(g, nil)
	if result.HasParentContext {
		t.Error("expected HasParentContext to be false for empty function")
	}
}
