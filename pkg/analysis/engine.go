// Package analysis provides the core analysis engine for Caliper.
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
	"sync"

	"golang.org/x/tools/go/packages"
)

// Engine is the core analysis engine. It loads Go packages with full
// type information and provides access to ASTs, type checker, and SSA.
type Engine struct {
	fset    *token.FileSet
	pkgs    []*packages.Package
	opts    EngineOptions
	ipcOnce sync.Once
	ipc     *InterProceduralContext
	ipcErr  error
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
		patterns = detectPatterns(dir)
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

	// For stdlib (GOROOT/src), set GOROOT env and don't set Dir
	// since the stdlib isn't a regular module.
	if isGoRootSrc(dir) {
		goroot := filepath.Dir(dir) // /path/to/go/libexec/src → /path/to/go/libexec
		cfg.Dir = ""
		cfg.Env = append(os.Environ(), "GOROOT="+goroot)
	}

	// Apply build tags.
	if len(opts.BuildTags) > 0 {
		cfg.BuildFlags = []string{"-tags=" + strings.Join(opts.BuildTags, ",")}
	}

	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, fmt.Errorf("loading packages: %w", err)
	}

	// Filter out packages with errors but keep those that loaded successfully.
	// This is important for large codebases (like stdlib) where some packages
	// may fail due to build constraints, cgo, or platform-specific code.
	var validPkgs []*packages.Package
	for _, pkg := range pkgs {
		if len(pkg.Errors) == 0 && pkg.Types != nil {
			validPkgs = append(validPkgs, pkg)
		}
	}

	// If ALL packages failed, report the first error.
	if len(validPkgs) == 0 {
		if len(pkgs) > 0 && len(pkgs[0].Errors) > 0 {
			return nil, fmt.Errorf("package errors: %v", pkgs[0].Errors[0])
		}
		return nil, fmt.Errorf("no packages found in %s", dir)
	}

	pkgs = validPkgs

	return &Engine{
		fset: pkgs[0].Fset,
		pkgs: pkgs,
		opts: opts,
	}, nil
}

// detectPatterns determines the right package patterns for the given directory.
// For GOROOT/src, it uses "std" to load the stdlib. Otherwise "./...".
func detectPatterns(dir string) []string {
	if isGoRootSrc(dir) {
		return []string{"std"}
	}
	// Check for subdirectory of GOROOT/src (e.g., GOROOT/src/go/ast).
	goroot := os.Getenv("GOROOT")
	if goroot != "" {
		srcDir := filepath.Join(goroot, "src")
		if rel, err := filepath.Rel(srcDir, dir); err == nil && !strings.HasPrefix(rel, "..") {
			// Convert filesystem path to import path.
			return []string{rel + "/..."}
		}
	}
	return []string{"./..."}
}

// isGoRootSrc checks if a directory is the Go stdlib source root (GOROOT/src).
// It detects this by checking for "module std" in go.mod (Go 1.21+) or by
// filesystem heuristics for older Go versions.
func isGoRootSrc(dir string) bool {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}

	// Primary check: look for "module std" in go.mod.
	if data, err := os.ReadFile(filepath.Join(abs, "go.mod")); err == nil {
		if strings.Contains(string(data), "module std") {
			return true
		}
	}

	// Fallback: check for stdlib directory markers + GOROOT structure.
	for _, marker := range []string{"fmt", "go", "net", "os"} {
		if _, err := os.Stat(filepath.Join(abs, marker)); err != nil {
			return false
		}
	}
	parent := filepath.Dir(abs)
	if _, err := os.Stat(filepath.Join(parent, "pkg")); err == nil {
		return true
	}
	return false
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

// InterProcedural returns the inter-procedural analysis context, building
// it lazily on first call. Uses CHA by default. The context is cached for
// subsequent calls.
func (e *Engine) InterProcedural() (*InterProceduralContext, error) {
	return e.InterProceduralWith(AlgoCHA)
}

// InterProceduralWith returns the inter-procedural context using the
// specified call graph algorithm. Only the first call builds the context;
// subsequent calls return the cached result regardless of algorithm.
func (e *Engine) InterProceduralWith(algo CallGraphAlgorithm) (*InterProceduralContext, error) {
	e.ipcOnce.Do(func() {
		e.ipc, e.ipcErr = BuildInterProceduralContext(e, algo)
	})
	return e.ipc, e.ipcErr
}
