package templates

import (
	"fmt"
	"go/ast"
	"go/types"
	"os"
	"regexp"
	"strings"

	pb "github.com/trendvidia/caliper/pkg/caliperpb"
)

// infallibleStdlibMethods lists stdlib methods whose `error` return is
// documented to always be nil. The key is "<pkgpath>.<TypeName>.<Method>"
// (no pointer prefix; pointer and value receivers collapse to the same key).
//
// For interface-typed receivers (e.g., a value of static type hash.Hash),
// the key uses the interface's package and name, since that is all we know
// statically. Concrete types satisfying the interface inherit the entry.
//
// Flagging these as "unchecked errors" is pure noise and the dominant source
// of false positives in find_unchecked_error. Source for each entry is the
// godoc of the method on the standard library.
var infallibleStdlibMethods = map[string]bool{
	// bytes.Buffer: "err is always nil" per godoc.
	"bytes.Buffer.Write":       true,
	"bytes.Buffer.WriteByte":   true,
	"bytes.Buffer.WriteRune":   true,
	"bytes.Buffer.WriteString": true,

	// strings.Builder: "The returned error is always nil." per godoc.
	"strings.Builder.Write":       true,
	"strings.Builder.WriteByte":   true,
	"strings.Builder.WriteRune":   true,
	"strings.Builder.WriteString": true,

	// hash.Hash.Write: "It never returns an error." per godoc. All
	// crypto/{md5,sha1,sha256,sha512}, crypto/hmac, hash/{crc32,crc64,
	// adler32,fnv,maphash} types implement this contract; calls through
	// the interface match this entry.
	"hash.Hash.Write": true,

	// hash.Hash32 / hash.Hash64 embed hash.Hash and inherit the same
	// "never returns an error" contract, but selection on a value of
	// static type Hash32/Hash64 keys the receiver type as Hash32/Hash64,
	// not the embedded Hash. Without explicit entries, fnv.New32a() and
	// fnv.New64a() (which return Hash32/Hash64) get false positives.
	"hash.Hash32.Write": true,
	"hash.Hash64.Write": true,

	// io.PipeReader/PipeWriter.CloseWithError: "always returns nil" per
	// godoc.
	"io.PipeReader.CloseWithError": true,
	"io.PipeWriter.CloseWithError": true,

	// math/rand.Rand.Read: "always returns len(p) and a nil error" per
	// godoc. (math/rand only — crypto/rand.Read can fail.)
	"math/rand.Rand.Read": true,
}

// infallibleStdlibFuncs lists package-level stdlib functions whose error
// return is, by long-standing convention, not worth checking per call.
// These functions write to os.Stdout: errors only occur if stdout has been
// closed or is connected to a broken pipe — a process-level failure, not a
// per-call concern. errcheck excludes these by default for the same reason.
var infallibleStdlibFuncs = map[string]bool{
	"fmt.Print":   true,
	"fmt.Printf":  true,
	"fmt.Println": true,

	// math/rand.Read: "always returns len(p) and a nil error" per godoc.
	"math/rand.Read": true,
}

// writerArgInfallibleFuncs are stdlib functions whose first argument is an
// io.Writer and whose error return originates from that writer. When the
// writer is itself infallible, the whole call is transitively infallible.
//
// io.WriteString uses the writer's WriteString method when available
// (which *bytes.Buffer and *strings.Builder both implement infallibly) and
// otherwise calls Write exactly once — also infallible for those types.
//
// go/printer.Fprint writes formatted Go source to its first arg; in
// caliper's own codebase (and most tools that pretty-print AST) the writer
// is a local *bytes.Buffer.
var writerArgInfallibleFuncs = map[string]bool{
	"fmt.Fprint":        true,
	"fmt.Fprintf":       true,
	"fmt.Fprintln":      true,
	"io.WriteString":    true,
	"go/printer.Fprint": true,
}

// infallibleStdlibWriters lists writer types whose Write is infallible.
// Used to recognize transitively-infallible fmt.Fprint*(w, ...) calls.
var infallibleStdlibWriters = map[string]bool{
	"bytes.Buffer":    true,
	"strings.Builder": true,
}

// infallibleStdlibWriterVars lists package-level variables whose Write is,
// by long-standing convention, not worth checking per call. Distinct from
// infallibleStdlibWriters (which matches by type) because these are
// *os.File values — other *os.File values (real disk files, network FDs)
// can absolutely fail and must remain flagged.
//
// Both share the disable_writers list: a project that genuinely wants to
// check stderr writes can list "os.Stderr" there.
var infallibleStdlibWriterVars = map[string]bool{
	"os.Stdout": true,
	"os.Stderr": true,
}

// keySet matches a key string against a fixed set of allowed entries. Each
// entry is either an exact string (O(1) map lookup) or a glob pattern
// containing '*' (linear scan against compiled regexes). '*' matches any
// sequence of characters including dots and slashes; all other characters
// are literal.
type keySet struct {
	exact map[string]bool
	globs []*regexp.Regexp
}

// newKeySet partitions entries into exact and glob buckets and compiles
// each glob to a regexp. Returns the populated set plus any per-entry
// errors so the caller can surface them — bad entries are skipped rather
// than crashing analysis.
func newKeySet(entries []string) (*keySet, []error) {
	if len(entries) == 0 {
		return nil, nil
	}
	ks := &keySet{exact: make(map[string]bool)}
	var errs []error
	for _, e := range entries {
		if err := validateEntry(e); err != nil {
			errs = append(errs, fmt.Errorf("entry %q: %w", e, err))
			continue
		}
		if strings.ContainsRune(e, '*') {
			re, err := compileGlob(e)
			if err != nil {
				errs = append(errs, fmt.Errorf("entry %q: %w", e, err))
				continue
			}
			ks.globs = append(ks.globs, re)
		} else {
			ks.exact[e] = true
		}
	}
	return ks, errs
}

// validateEntry rejects entries that contain shell-glob metacharacters
// caliper does not support. The only wildcard is '*'; the others are
// almost always typos (e.g., a user writing "Logger.De?bug" expecting
// '?' to act as a single-char wildcard) that would otherwise silently
// match nothing.
func validateEntry(e string) error {
	for _, r := range e {
		switch r {
		case '?', '[', ']', '{', '}':
			return fmt.Errorf("unsupported glob character %q (only '*' is supported)", r)
		}
	}
	return nil
}

// loadKeySet wraps newKeySet, logging any glob compile errors to stderr
// with a field-name prefix so the user can locate the bad entry in their
// .caliper.pxf.
func loadKeySet(entries []string, field string) *keySet {
	ks, errs := newKeySet(entries)
	for _, err := range errs {
		fmt.Fprintf(os.Stderr, "caliper: infallible.%s: %v\n", field, err)
	}
	return ks
}

func (ks *keySet) Match(key string) bool {
	if ks == nil {
		return false
	}
	if ks.exact[key] {
		return true
	}
	for _, re := range ks.globs {
		if re.MatchString(key) {
			return true
		}
	}
	return false
}

// compileGlob converts a '*'-glob into an anchored regexp where '*' means
// "any sequence including dots and slashes" and every other character is
// literal.
func compileGlob(pattern string) (*regexp.Regexp, error) {
	var sb strings.Builder
	sb.WriteString("^")
	for _, r := range pattern {
		if r == '*' {
			sb.WriteString(".*")
		} else {
			sb.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	sb.WriteString("$")
	return regexp.Compile(sb.String())
}

// InfallibleState holds the project-local Infallible config materialized
// into the keysets the matcher consults. Engine-scoped (held on the
// engine cache entry, threaded through TemplateContext) so concurrent
// analyses on different paths can each see their own config without
// racing on package-level state. A nil *InfallibleState means "no
// project-local extensions" — the hardcoded stdlib defaults still apply.
type InfallibleState struct {
	userMethods     *keySet
	userFuncs       *keySet
	userWriters     *keySet
	disabledMethods *keySet
	disabledFuncs   *keySet
	disabledWriters *keySet
}

// NewInfallibleState materializes inf into matchable keysets, logging
// any malformed entries to stderr with a field-name prefix. Returns nil
// if inf is nil — callers handle nil as "no project config".
func NewInfallibleState(inf *pb.Infallible) *InfallibleState {
	if inf == nil {
		return nil
	}
	return &InfallibleState{
		userMethods:     loadKeySet(inf.GetMethods(), "methods"),
		userFuncs:       loadKeySet(inf.GetFunctions(), "functions"),
		userWriters:     loadKeySet(inf.GetWriters(), "writers"),
		disabledMethods: loadKeySet(inf.GetDisableMethods(), "disable_methods"),
		disabledFuncs:   loadKeySet(inf.GetDisableFunctions(), "disable_functions"),
		disabledWriters: loadKeySet(inf.GetDisableWriters(), "disable_writers"),
	}
}

func (s *InfallibleState) matchUserMethod(key string) bool {
	if s == nil {
		return false
	}
	return s.userMethods.Match(key)
}

func (s *InfallibleState) matchUserFunc(key string) bool {
	if s == nil {
		return false
	}
	return s.userFuncs.Match(key)
}

func (s *InfallibleState) matchUserWriter(key string) bool {
	if s == nil {
		return false
	}
	return s.userWriters.Match(key)
}

func (s *InfallibleState) disablesMethod(key string) bool {
	if s == nil {
		return false
	}
	return s.disabledMethods.Match(key)
}

func (s *InfallibleState) disablesFunc(key string) bool {
	if s == nil {
		return false
	}
	return s.disabledFuncs.Match(key)
}

func (s *InfallibleState) disablesWriter(key string) bool {
	if s == nil {
		return false
	}
	return s.disabledWriters.Match(key)
}

// isInfallibleErrorReturn reports whether the call targets a stdlib API
// whose error return is documented or conventionally treated as always nil,
// or a project-local API the user has registered as such. Such calls should
// not be flagged as unchecked errors.
//
// state may be nil — the hardcoded stdlib defaults still apply in that case.
func isInfallibleErrorReturn(call *ast.CallExpr, info *types.Info, state *InfallibleState) bool {
	if info == nil {
		return false
	}
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		// Method calls live in info.Selections (not info.Uses).
		// Package-qualified function calls (e.g. fmt.Println) are not
		// selections — sel.X is a package identifier — so they fall
		// through to the package-level function path.
		if selection := info.Selections[fn]; selection != nil {
			return isInfallibleMethodCall(selection, state)
		}
		return isInfallibleFuncCall(info.ObjectOf(fn.Sel), call, info, state)
	case *ast.Ident:
		// Bare identifier call: an intra-package function or a name
		// brought in via dot-import.
		return isInfallibleFuncCall(info.ObjectOf(fn), call, info, state)
	}
	return false
}

// isInfallibleMethodCall checks whether a method-selection's receiver type
// and method name fall into the infallible-methods allowlist (built-in or
// user-registered, with built-in entries masked by the user's disable list).
func isInfallibleMethodCall(selection *types.Selection, state *InfallibleState) bool {
	fn, ok := selection.Obj().(*types.Func)
	if !ok {
		return false
	}
	origRecvType := selection.Recv()
	recvType := origRecvType
	if ptr, ok := recvType.(*types.Pointer); ok {
		recvType = ptr.Elem()
	}
	named, ok := recvType.(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	if obj == nil || obj.Pkg() == nil {
		return false
	}
	key := obj.Pkg().Path() + "." + obj.Name() + "." + fn.Name()
	if infallibleStdlibMethods[key] && !state.disablesMethod(key) {
		return true
	}
	if state.matchUserMethod(key) {
		return true
	}
	// Fall back: when a concrete type implements hash.Hash structurally,
	// inherit the "Write never returns an error" contract.
	if fn.Name() == "Write" && !state.disablesMethod("hash.Hash.Write") &&
		implementsHashHash(origRecvType) {
		return true
	}
	return false
}

// implementsHashHash reports whether t structurally has the method set
// of hash.Hash: Write([]byte) (int, error), Sum([]byte) []byte, Reset(),
// Size() int, BlockSize() int. Any type satisfying this method set is
// treated as a hash implementation regardless of declared interface
// relationships — we don't need to look up the actual hash.Hash
// interface in the type universe.
func implementsHashHash(t types.Type) bool {
	if t == nil {
		return false
	}
	checks := []struct {
		name    string
		params  []string
		results []string
	}{
		{"Write", []string{"[]byte"}, []string{"int", "error"}},
		{"Sum", []string{"[]byte"}, []string{"[]byte"}},
		{"Reset", nil, nil},
		{"Size", nil, []string{"int"}},
		{"BlockSize", nil, []string{"int"}},
	}
	for _, c := range checks {
		if !hasMethodSig(t, c.name, c.params, c.results) {
			return false
		}
	}
	return true
}

func hasMethodSig(t types.Type, name string, params, results []string) bool {
	ms := types.NewMethodSet(t)
	for i := 0; i < ms.Len(); i++ {
		sel := ms.At(i)
		if sel.Obj().Name() != name {
			continue
		}
		sig, ok := sel.Obj().Type().(*types.Signature)
		if !ok {
			continue
		}
		if signatureMatches(sig, params, results) {
			return true
		}
	}
	return false
}

func signatureMatches(sig *types.Signature, params, results []string) bool {
	if sig.Params().Len() != len(params) {
		return false
	}
	if sig.Results().Len() != len(results) {
		return false
	}
	for i, want := range params {
		if sig.Params().At(i).Type().String() != want {
			return false
		}
	}
	for i, want := range results {
		if sig.Results().At(i).Type().String() != want {
			return false
		}
	}
	return true
}

// isInfallibleFuncCall checks whether a non-method call targets an
// infallible package-level function (built-in or user-registered) or a
// transitively-infallible writer-arg function.
func isInfallibleFuncCall(obj types.Object, call *ast.CallExpr, info *types.Info, state *InfallibleState) bool {
	fn, ok := obj.(*types.Func)
	if !ok || fn.Pkg() == nil {
		return false
	}
	key := fn.Pkg().Path() + "." + fn.Name()
	if infallibleStdlibFuncs[key] && !state.disablesFunc(key) {
		return true
	}
	if state.matchUserFunc(key) {
		return true
	}
	if writerArgInfallibleFuncs[key] && len(call.Args) >= 1 {
		return isInfallibleWriterArg(call.Args[0], info, state)
	}
	return false
}

// isInfallibleWriterArg reports whether arg is a writer whose Write method
// is infallible. Two paths:
//
//   - Value-based: a direct reference to a package-level variable in the
//     infallibleStdlibWriterVars set (e.g. os.Stdout, os.Stderr). Other
//     values of the same type (like a *os.File pointing at a real file)
//     stay fallible and must still be flagged.
//   - Type-based: a value whose static type is a hardcoded infallible
//     writer (bytes.Buffer, strings.Builder) or a user-registered one.
//
// Both paths consult disabled_writers to honor user overrides.
func isInfallibleWriterArg(arg ast.Expr, info *types.Info, state *InfallibleState) bool {
	if sel, ok := arg.(*ast.SelectorExpr); ok {
		if v, ok := info.ObjectOf(sel.Sel).(*types.Var); ok && v.Pkg() != nil {
			key := v.Pkg().Path() + "." + v.Name()
			if infallibleStdlibWriterVars[key] && !state.disablesWriter(key) {
				return true
			}
		}
	}

	t := info.TypeOf(arg)
	if t == nil {
		return false
	}
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	if obj == nil || obj.Pkg() == nil {
		return false
	}
	key := obj.Pkg().Path() + "." + obj.Name()
	if infallibleStdlibWriters[key] && !state.disablesWriter(key) {
		return true
	}
	return state.matchUserWriter(key)
}
