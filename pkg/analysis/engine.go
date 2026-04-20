// Package analysis provides the core analysis engine for GoCaliper.
//
// It wraps golang.org/x/tools/go/packages for type-aware loading and
// provides a unified entry point for pattern matching, control-flow
// analysis, and transformations.
package analysis

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Engine is the core analysis engine. It loads Go packages with full
// type information and provides access to ASTs, type checker, and SSA.
type Engine struct {
	fset *token.FileSet
	pkgs []*packages.Package
	opts EngineOptions
}

// EngineOptions configures analysis behavior.
type EngineOptions struct {
	// SkipGenerated skips files with "// Code generated" headers (default: true).
	SkipGenerated bool
	// SkipCgo skips files that import "C" (default: true).
	SkipCgo bool
	// BuildTags sets build tags for package loading.
	BuildTags []string
}

// DefaultOptions returns the default engine options.
func DefaultOptions() EngineOptions {
	return EngineOptions{
		SkipGenerated: true,
		SkipCgo:       true,
	}
}

// NewEngine creates a new analysis engine rooted at the given directory.
func NewEngine(dir string, patterns ...string) (*Engine, error) {
	return NewEngineWithOptions(dir, DefaultOptions(), patterns...)
}

// NewEngineWithOptions creates an engine with explicit options.
func NewEngineWithOptions(dir string, opts EngineOptions, patterns ...string) (*Engine, error) {
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}

	cfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo,
		Dir: dir,
	}

	// Apply build tags.
	if len(opts.BuildTags) > 0 {
		cfg.BuildFlags = []string{"-tags=" + strings.Join(opts.BuildTags, ",")}
	}

	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, fmt.Errorf("loading packages: %w", err)
	}

	// Check for load errors.
	var errs []error
	for _, pkg := range pkgs {
		for _, e := range pkg.Errors {
			errs = append(errs, e)
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("package errors: %v", errs[0])
	}

	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no packages found in %s", dir)
	}

	return &Engine{
		fset: pkgs[0].Fset,
		pkgs: pkgs,
		opts: opts,
	}, nil
}

// Fset returns the shared file set.
func (e *Engine) Fset() *token.FileSet { return e.fset }

// Packages returns all loaded packages.
func (e *Engine) Packages() []*packages.Package { return e.pkgs }

// FileLocation describes a position in source code.
type FileLocation struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
	End    int    `json:"end_line,omitempty"`
}

// LocationOf returns the FileLocation for a token position.
func (e *Engine) LocationOf(pos token.Pos) FileLocation {
	p := e.fset.Position(pos)
	return FileLocation{
		File:   p.Filename,
		Line:   p.Line,
		Column: p.Column,
	}
}

// WalkFunc visits every ast.Node across all loaded packages.
type WalkFunc func(pkg *packages.Package, file *ast.File, node ast.Node, info *types.Info) bool

// Walk traverses all AST nodes across all loaded packages.
func (e *Engine) Walk(fn WalkFunc) {
	for _, pkg := range e.pkgs {
		for _, file := range pkg.Syntax {
			if e.shouldSkipFile(file) {
				continue
			}
			ast.Inspect(file, func(n ast.Node) bool {
				if n == nil {
					return false
				}
				return fn(pkg, file, n, pkg.TypesInfo)
			})
		}
	}
}

// FuncNodes returns all function declarations across loaded packages.
func (e *Engine) FuncNodes() []*FuncContext {
	var funcs []*FuncContext
	for _, pkg := range e.pkgs {
		for _, file := range pkg.Syntax {
			if e.shouldSkipFile(file) {
				continue
			}
			for _, decl := range file.Decls {
				if fd, ok := decl.(*ast.FuncDecl); ok {
					funcs = append(funcs, &FuncContext{
						Pkg:  pkg,
						File: file,
						Func: fd,
						Info: pkg.TypesInfo,
					})
				}
			}
		}
	}
	return funcs
}

// shouldSkipFile checks if a file should be excluded from analysis
// based on engine options (generated code, cgo).
func (e *Engine) shouldSkipFile(file *ast.File) bool {
	if e.opts.SkipGenerated && isGenerated(file) {
		return true
	}
	if e.opts.SkipCgo && isCgoFile(file) {
		return true
	}
	return false
}

// isGenerated checks for the standard "Code generated" marker.
// See https://pkg.go.dev/cmd/go/internal/generate
func isGenerated(file *ast.File) bool {
	return ast.IsGenerated(file)
}

// isCgoFile checks if a file imports "C" (cgo pseudo-package).
func isCgoFile(file *ast.File) bool {
	for _, imp := range file.Imports {
		if imp.Path.Value == `"C"` {
			return true
		}
	}
	return false
}

// IsWorkspace checks if the directory contains a go.work file,
// indicating Go workspace mode.
func IsWorkspace(dir string) bool {
	return fileExists(dir + "/go.work")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// NewEngineForFile creates an engine that loads only the package containing
// the given file. This is much faster than NewEngine for single-file operations.
func NewEngineForFile(filePath string) (*Engine, error) {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return nil, fmt.Errorf("resolving file path: %w", err)
	}
	return NewEngineWithOptions(filepath.Dir(absPath), DefaultOptions(), "file="+absPath)
}

// FuncContext bundles a function declaration with its surrounding context.
type FuncContext struct {
	Pkg  *packages.Package
	File *ast.File
	Func *ast.FuncDecl
	Info *types.Info
}
