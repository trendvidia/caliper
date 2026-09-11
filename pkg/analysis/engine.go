package analysis

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"os/exec"
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

	genOnce   sync.Once
	generated map[string]bool
}

// pathRemap maps system GOROOT paths back to fork paths in output.
type pathRemap struct {
	from string // e.g., /opt/homebrew/.../libexec/src
	to   string // e.g., /Users/bene/.../go/src
}

// EngineOptions configures analysis behavior.
type EngineOptions struct {
	// SkipGenerated skips files with "// Code generated" headers
	// (default: true).
	//
	// Scope: AST traversal only — [Engine.Walk] and [Engine.FuncNodes].
	// It does NOT filter the SSA program or the call graph, which are
	// built from every loaded package. A consumer walking the call graph
	// therefore sees generated functions whatever this is set to; use
	// [InterProceduralContext.IsGeneratedFunc] to filter there.
	SkipGenerated bool
	// SkipCgo skips files that import "C" (default: true). Same AST-only
	// scope as SkipGenerated.
	SkipCgo bool
	// Tests includes each package's test files, and the generated test
	// binary packages, in the load (default: false — the go/packages
	// default). Test files are roughly half the Go source in some
	// repositories, and they are where intent is documented, so a
	// consumer building a picture of a codebase usually wants them; a
	// linter reporting on shipped code usually does not.
	Tests bool
	// BuildTags sets build tags for package loading.
	BuildTags []string
	// Env is appended to the load's environment, in KEY=VALUE form, after
	// the process environment and the engine's own settings. Its purpose
	// is a configuration the host is not: GOOS=linux and GOARCH=amd64
	// load the files behind //go:build constraints a tag cannot satisfy,
	// since GOOS and GOARCH are not tags (trendvidia/alidade#43). Empty
	// by default, and not touched by DefaultOptions.
	Env []string

	// pathRemap remaps file paths in output (internal, set by engine).
	pathRemap *pathRemap
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
		// NeedImports is load-bearing, not decorative: NeedDeps only
		// propagates this mode INTO the packages already in Imports, and
		// Imports itself is populated by NeedImports. Without it every
		// pkg.Imports is empty, so ssautil.AllPackages never calls
		// CreatePackage for a dependency and prog.Build() panics on the
		// first unsatisfied import — which is every real package, since
		// they all import at least "context" or "fmt".
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedImports |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo |
			packages.NeedDeps |
			packages.NeedModule,
		Dir:   dir,
		Tests: opts.Tests,
	}

	// For stdlib (module std), disable go.work (a workspace file above
	// the target would interfere). If the target has a compiled toolchain,
	// use it as GOROOT. Otherwise (fork/checkout), use overlay to map
	// the fork's source files onto the system GOROOT so the toolchain
	// works but the analyzed source comes from the fork.
	if stdRoot := findStdlibRoot(dir); stdRoot != "" {
		goroot := filepath.Dir(stdRoot) // .../go/src → .../go
		cfg.Env = append(os.Environ(), "GOWORK=off")
		if hasCompiledToolchain(goroot) {
			cfg.Dir = ""
			cfg.Env = append(cfg.Env, "GOROOT="+goroot)
		} else {
			// Fork without compiled toolchain — use overlay.
			sysGoroot := systemGOROOT()
			if sysGoroot != "" {
				overlay, err := buildStdlibOverlay(sysGoroot, stdRoot)
				if err == nil && len(overlay) > 0 {
					cfg.Overlay = overlay
					cfg.Dir = ""
					cfg.Env = append(cfg.Env, "GOROOT="+sysGoroot)
					// Store path remap so findings show fork paths, not system GOROOT.
					opts.pathRemap = &pathRemap{
						from: filepath.Join(sysGoroot, "src"),
						to:   stdRoot,
					}
				}
			}
		}
	}

	// Apply build tags.
	if len(opts.BuildTags) > 0 {
		cfg.BuildFlags = []string{"-tags=" + strings.Join(opts.BuildTags, ",")}
	}

	// Apply the caller's environment last, so it wins over the process
	// environment and over the stdlib settings above.
	if len(opts.Env) > 0 {
		if cfg.Env == nil {
			cfg.Env = os.Environ()
		}
		cfg.Env = append(cfg.Env, opts.Env...)
	}

	pkgs, err := packages.Load(cfg, patterns...)

	// If loading failed, retry with GOWORK=off in case a go.work file
	// above the target is interfering.
	if err != nil && !hasEnv(cfg.Env, "GOWORK=off") {
		cfg.Env = append(append(os.Environ(), "GOWORK=off"), opts.Env...)
		pkgs, err = packages.Load(cfg, patterns...)
	}

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

	// If ALL packages failed, check if a go.work might be interfering
	// and retry with GOWORK=off.
	if len(validPkgs) == 0 && !hasEnv(cfg.Env, "GOWORK=off") {
		cfg.Env = append(append(os.Environ(), "GOWORK=off"), opts.Env...)
		pkgs, err = packages.Load(cfg, patterns...)
		if err == nil {
			for _, pkg := range pkgs {
				if len(pkg.Errors) == 0 && pkg.Types != nil {
					validPkgs = append(validPkgs, pkg)
				}
			}
		}
	}

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
	if stdRoot := findStdlibRoot(dir); stdRoot != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			abs = dir
		}
		if abs == stdRoot {
			return []string{"std"}
		}
		// Subdirectory of stdlib src — convert to import path.
		if rel, err := filepath.Rel(stdRoot, abs); err == nil && !strings.HasPrefix(rel, "..") {
			return []string{rel + "/..."}
		}
		return []string{"std"}
	}
	// Also check system GOROOT for installed Go.
	goroot := os.Getenv("GOROOT")
	if goroot != "" {
		srcDir := filepath.Join(goroot, "src")
		abs, err := filepath.Abs(dir)
		if err != nil {
			abs = dir
		}
		if rel, err := filepath.Rel(srcDir, abs); err == nil && !strings.HasPrefix(rel, "..") {
			if rel == "." {
				return []string{"std"}
			}
			return []string{rel + "/..."}
		}
	}
	return []string{"./..."}
}

// findStdlibRoot walks up from dir looking for a directory containing a
// go.mod with "module std" (the Go stdlib). Returns the stdlib src root
// path, or "" if not found.
func findStdlibRoot(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}

	for {
		if isStdlibDir(abs) {
			return abs
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			break
		}
		abs = parent
	}
	return ""
}

// isStdlibDir checks if a directory is a Go stdlib source root.
func isStdlibDir(abs string) bool {
	// Primary: check for "module std" in go.mod.
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
	_, err := os.Stat(filepath.Join(parent, "pkg"))
	return err == nil
}

// isGoRootSrc is an alias for backward compatibility with tests.
func isGoRootSrc(dir string) bool {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	return isStdlibDir(abs)
}

// systemGOROOT returns the system Go's GOROOT using `go env GOROOT`.
func systemGOROOT() string {
	if goroot := os.Getenv("GOROOT"); goroot != "" {
		return goroot
	}
	out, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// buildStdlibOverlay builds a packages.Config.Overlay that maps system
// GOROOT source paths to fork source file contents. This lets the Go
// toolchain work (from system GOROOT) while analyzing the fork's code.
//
// Overlays the entire fork src so that cross-package dependencies
// (e.g., a modified function in reflectdata referenced from walk)
// resolve correctly during type checking.
func buildStdlibOverlay(sysGoroot, forkSrcRoot string) (map[string][]byte, error) {
	sysSrcRoot := filepath.Join(sysGoroot, "src")
	overlay := make(map[string][]byte)

	absForkSrc, err := filepath.Abs(forkSrcRoot)
	if err != nil {
		return nil, err
	}

	err = filepath.Walk(absForkSrc, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			if name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || name == "cmd" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Skip test files to reduce overlay size.
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, err := filepath.Rel(absForkSrc, path)
		if err != nil {
			return nil
		}

		sysPath := filepath.Join(sysSrcRoot, rel)

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		overlay[sysPath] = data
		return nil
	})
	if err != nil {
		return nil, err
	}

	return overlay, nil
}

// hasCompiledToolchain checks if a GOROOT has compiled tools (compile, link, etc.)
// under pkg/tool/. A source-only checkout won't have these.
func hasCompiledToolchain(goroot string) bool {
	toolDir := filepath.Join(goroot, "pkg", "tool")
	entries, err := os.ReadDir(toolDir)
	if err != nil {
		return false
	}
	// Should have at least one platform directory (e.g., darwin_arm64).
	return len(entries) > 0
}

// hasEnv checks if an env slice contains a specific key=value pair.
func hasEnv(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
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
	file := p.Filename
	// Remap system GOROOT paths to fork paths when using overlay.
	if e.opts.pathRemap != nil {
		from := e.opts.pathRemap.from
		// Ensure consistent prefix matching with trailing separator.
		if !strings.HasSuffix(from, string(filepath.Separator)) {
			from += string(filepath.Separator)
		}
		if strings.HasPrefix(file, from) {
			to := e.opts.pathRemap.to
			if !strings.HasSuffix(to, string(filepath.Separator)) {
				to += string(filepath.Separator)
			}
			file = to + file[len(from):]
		}
	}
	return FileLocation{
		File:   file,
		Line:   p.Line,
		Column: p.Column,
	}
}

// WalkFunc visits every ast.Node across all loaded packages.
type WalkFunc func(pkg *packages.Package, file *ast.File, node ast.Node, info *types.Info) bool

// GeneratedFiles returns the set of file paths that carry a
// "// Code generated ... DO NOT EDIT." marker, whatever SkipGenerated is
// set to. Consumers that build their own view of the corpus — a code
// graph, say — need this because SkipGenerated does not reach the SSA
// program or the call graph.
func (e *Engine) GeneratedFiles() map[string]bool {
	e.genOnce.Do(func() {
		e.generated = make(map[string]bool)
		for _, pkg := range e.pkgs {
			for _, file := range pkg.Syntax {
				if ast.IsGenerated(file) {
					e.generated[e.fset.Position(file.Pos()).Filename] = true
				}
			}
		}
	})
	return e.generated
}

// IsGeneratedFile reports whether path was produced by a code generator.
func (e *Engine) IsGeneratedFile(path string) bool {
	return e.GeneratedFiles()[path]
}

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
