package templates

import (
	"go/ast"
	"go/types"
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
var writerArgInfallibleFuncs = map[string]bool{
	"fmt.Fprint":     true,
	"fmt.Fprintf":    true,
	"fmt.Fprintln":   true,
	"io.WriteString": true,
}

// infallibleStdlibWriters lists writer types whose Write is infallible.
// Used to recognize transitively-infallible fmt.Fprint*(w, ...) calls.
var infallibleStdlibWriters = map[string]bool{
	"bytes.Buffer":    true,
	"strings.Builder": true,
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
// each glob to a regexp. Bad globs are silently skipped — config should
// not crash analysis.
func newKeySet(entries []string) *keySet {
	if len(entries) == 0 {
		return nil
	}
	ks := &keySet{exact: make(map[string]bool)}
	for _, e := range entries {
		if strings.ContainsRune(e, '*') {
			if re, err := compileGlob(e); err == nil {
				ks.globs = append(ks.globs, re)
			}
		} else {
			ks.exact[e] = true
		}
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

// Package-level state holding the project-local Infallible config. Read
// per-call by isInfallibleErrorReturn; written exactly once at engine boot
// via ConfigureInfallible. Tests may invoke ConfigureInfallible repeatedly
// — analyses are not concurrent with config changes in either case, so no
// mutex is needed.
var (
	userInfallibleMethods  *keySet
	userInfallibleFuncs    *keySet
	userInfallibleWriters  *keySet
	disabledStdlibMethods  *keySet
	disabledStdlibFuncs    *keySet
	disabledStdlibWriters  *keySet
)

// ConfigureInfallible installs (or clears, if inf is nil) the project-local
// extensions to find_unchecked_error's allowlist. Intended to be called once
// at startup, before any analysis runs.
func ConfigureInfallible(inf *pb.Infallible) {
	if inf == nil {
		userInfallibleMethods = nil
		userInfallibleFuncs = nil
		userInfallibleWriters = nil
		disabledStdlibMethods = nil
		disabledStdlibFuncs = nil
		disabledStdlibWriters = nil
		return
	}
	userInfallibleMethods = newKeySet(inf.GetMethods())
	userInfallibleFuncs = newKeySet(inf.GetFunctions())
	userInfallibleWriters = newKeySet(inf.GetWriters())
	disabledStdlibMethods = newKeySet(inf.GetDisableMethods())
	disabledStdlibFuncs = newKeySet(inf.GetDisableFunctions())
	disabledStdlibWriters = newKeySet(inf.GetDisableWriters())
}

// isInfallibleErrorReturn reports whether the call targets a stdlib API
// whose error return is documented or conventionally treated as always nil,
// or a project-local API the user has registered as such. Such calls should
// not be flagged as unchecked errors.
func isInfallibleErrorReturn(call *ast.CallExpr, info *types.Info) bool {
	if info == nil {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}

	// Method calls live in info.Selections (not info.Uses). Package-level
	// function calls (e.g. fmt.Println) are not selections — sel.X is a
	// package identifier — so they fall through to info.ObjectOf.
	if selection := info.Selections[sel]; selection != nil {
		fn, ok := selection.Obj().(*types.Func)
		if !ok {
			return false
		}
		recvType := selection.Recv()
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
		if infallibleStdlibMethods[key] && !disabledStdlibMethods.Match(key) {
			return true
		}
		return userInfallibleMethods.Match(key)
	}

	fn, ok := info.ObjectOf(sel.Sel).(*types.Func)
	if !ok || fn.Pkg() == nil {
		return false
	}
	key := fn.Pkg().Path() + "." + fn.Name()
	if infallibleStdlibFuncs[key] && !disabledStdlibFuncs.Match(key) {
		return true
	}
	if userInfallibleFuncs.Match(key) {
		return true
	}
	if writerArgInfallibleFuncs[key] && len(call.Args) >= 1 {
		return isInfallibleWriterArg(info.TypeOf(call.Args[0]))
	}
	return false
}

// isInfallibleWriterArg reports whether t is a writer type whose Write
// method is infallible — either a stdlib writer in the hardcoded set (and
// not disabled by config) or a user-registered writer.
func isInfallibleWriterArg(t types.Type) bool {
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
	if infallibleStdlibWriters[key] && !disabledStdlibWriters.Match(key) {
		return true
	}
	return userInfallibleWriters.Match(key)
}
