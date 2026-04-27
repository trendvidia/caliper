package templates

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/trendvidia/caliper/pkg/analysis"
	pb "github.com/trendvidia/caliper/pkg/caliperpb"
)

// setupProject creates a temp Go project with the given source file and
// returns an analysis.Engine loaded from it. Defaults to go 1.23.
func setupProject(t *testing.T, src string) *analysis.Engine {
	return setupProjectWithGoVersion(t, src, "1.23")
}

// setupProjectWithGoVersion is like setupProject but lets the test pick
// the `go` directive in go.mod — needed to exercise behavior that depends
// on language version (e.g., per-iteration loop scoping in Go 1.22+).
func setupProjectWithGoVersion(t *testing.T, src, goVersion string) *analysis.Engine {
	t.Helper()

	dir := t.TempDir()
	goMod := "module testpkg\n\ngo " + goVersion + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "code.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	engine, err := analysis.NewEngine(dir)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return engine
}

// findingsOfKind filters findings by kind.
func findingsOfKind(findings []Finding, kind string) []Finding {
	var out []Finding
	for _, f := range findings {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

// --------------------------------------------------------------------------
// ResourceLeak tests
// --------------------------------------------------------------------------

func TestResourceLeak_MissingClose(t *testing.T) {
	src := `package testpkg

import "os"

func leaky() {
	f, _ := os.Open("test.txt")
	_ = f
}
`
	engine := setupProject(t, src)
	tmpl, err := NewResourceLeak("$f, $_ := os.Open($path)", "Close", true)
	if err != nil {
		t.Fatal(err)
	}
	findings := RunTemplate(engine, tmpl)
	leaks := findingsOfKind(findings, "resource_leak")
	if len(leaks) == 0 {
		t.Error("expected resource leak finding for os.Open without Close")
	}
}

func TestResourceLeak_WithDeferClose(t *testing.T) {
	// No branching — single path through defer to exit.
	src := `package testpkg

import "os"

func safe() {
	f, _ := os.Open("test.txt")
	defer f.Close()
	_ = f
}
`
	engine := setupProject(t, src)
	tmpl, err := NewResourceLeak("$f, $_ := os.Open($path)", "Close", true)
	if err != nil {
		t.Fatal(err)
	}
	findings := RunTemplate(engine, tmpl)
	leaks := findingsOfKind(findings, "resource_leak")
	if len(leaks) > 0 {
		t.Errorf("expected no resource leak with defer Close, got %d: %s", len(leaks), leaks[0].Message)
	}
}

func TestResourceLeak_CloseOnOnePath(t *testing.T) {
	src := `package testpkg

import "os"

func partialClose(flag bool) {
	f, _ := os.Open("test.txt")
	if flag {
		f.Close()
		return
	}
	// error path: no close
}
`
	engine := setupProject(t, src)
	tmpl, err := NewResourceLeak("$f, $_ := os.Open($path)", "Close", true)
	if err != nil {
		t.Fatal(err)
	}
	findings := RunTemplate(engine, tmpl)
	leaks := findingsOfKind(findings, "resource_leak")
	if len(leaks) == 0 {
		t.Error("expected resource leak finding when Close only on one path")
	}
}

// --------------------------------------------------------------------------
// UncheckedError tests
// --------------------------------------------------------------------------

func TestUncheckedError_DiscardedReturn(t *testing.T) {
	src := `package testpkg

import "os"

func bad() {
	f, _ := os.Open("test.txt")
	f.Close()
}
`
	engine := setupProject(t, src)
	tmpl := &UncheckedError{}
	findings := RunTemplate(engine, tmpl)
	errs := findingsOfKind(findings, "unchecked_error")
	if len(errs) == 0 {
		t.Error("expected unchecked error finding for f.Close() with discarded return")
	}
}

func TestUncheckedError_BlankIdentifier(t *testing.T) {
	src := `package testpkg

import "os"

func blanked() {
	f, _ := os.Open("test.txt")
	_ = f.Close()
}
`
	engine := setupProject(t, src)
	// IncludeExplicit = true to flag _ = f.Close()
	tmpl := &UncheckedError{IncludeExplicit: true}
	findings := RunTemplate(engine, tmpl)
	errs := findingsOfKind(findings, "unchecked_error")
	if len(errs) == 0 {
		t.Error("expected unchecked error finding for _ = f.Close()")
	}
}

func TestUncheckedError_BlankIdentifier_NotFlagged(t *testing.T) {
	src := `package testpkg

import "os"

func blanked() {
	f, _ := os.Open("test.txt")
	_ = f.Close()
}
`
	engine := setupProject(t, src)
	// IncludeExplicit = false (default) should NOT flag _ = f.Close()
	tmpl := &UncheckedError{IncludeExplicit: false}
	findings := RunTemplate(engine, tmpl)
	// Filter to only findings that mention "explicitly ignored" (blank identifier)
	var explicit []Finding
	for _, f := range findings {
		if f.Kind == "unchecked_error" && strings.Contains(f.Message, "explicitly ignored") {
			explicit = append(explicit, f)
		}
	}
	if len(explicit) > 0 {
		t.Error("expected no explicit blank identifier findings when IncludeExplicit=false")
	}
}

func TestUncheckedError_ProperlyHandled(t *testing.T) {
	src := `package testpkg

import (
	"fmt"
	"os"
)

func good() error {
	f, err := os.Open("test.txt")
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing: %w", err)
	}
	return nil
}
`
	engine := setupProject(t, src)
	tmpl := &UncheckedError{}
	findings := RunTemplate(engine, tmpl)
	errs := findingsOfKind(findings, "unchecked_error")
	if len(errs) > 0 {
		t.Errorf("expected no unchecked error findings when errors handled, got %d: %s", len(errs), errs[0].Message)
	}
}

func TestUncheckedError_DeferIncluded(t *testing.T) {
	src := `package testpkg

import "os"

func deferred() error {
	f, err := os.Open("test.txt")
	if err != nil {
		return err
	}
	defer f.Close()
	return nil
}
`
	engine := setupProject(t, src)
	// IncludeDefers = true should flag defer f.Close()
	tmpl := &UncheckedError{IncludeDefers: true}
	findings := RunTemplate(engine, tmpl)
	var deferFindings []Finding
	for _, f := range findings {
		if f.Kind == "unchecked_error" && strings.Contains(f.Message, "deferred") {
			deferFindings = append(deferFindings, f)
		}
	}
	if len(deferFindings) == 0 {
		t.Error("expected unchecked error finding for defer f.Close() when IncludeDefers=true")
	}
}

func TestUncheckedError_DeferNotIncluded(t *testing.T) {
	src := `package testpkg

import "os"

func deferred() error {
	f, err := os.Open("test.txt")
	if err != nil {
		return err
	}
	defer f.Close()
	return nil
}
`
	engine := setupProject(t, src)
	// IncludeDefers = false (default) should NOT flag defer f.Close()
	tmpl := &UncheckedError{IncludeDefers: false}
	findings := RunTemplate(engine, tmpl)
	var deferFindings []Finding
	for _, f := range findings {
		if f.Kind == "unchecked_error" && strings.Contains(f.Message, "deferred") {
			deferFindings = append(deferFindings, f)
		}
	}
	if len(deferFindings) > 0 {
		t.Error("expected no deferred error findings when IncludeDefers=false")
	}
}

func TestUncheckedError_InfallibleStdlib(t *testing.T) {
	// Methods/functions whose error return is documented to always be nil
	// or is, by long-standing convention, not worth checking per call.
	// Flagging them produces noise that erodes trust in the rest of the
	// tool's output.
	src := `package testpkg

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"math/rand"
	"strings"
)

func writes() {
	var buf bytes.Buffer
	buf.Write([]byte("a"))
	buf.WriteByte('b')
	buf.WriteRune('c')
	buf.WriteString("d")

	var sb strings.Builder
	sb.Write([]byte("a"))
	sb.WriteByte('b')
	sb.WriteRune('c')
	sb.WriteString("d")

	h := sha256.New()
	h.Write([]byte("payload"))

	// Conventional: writes to os.Stdout.
	fmt.Print("a")
	fmt.Printf("%s", "b")
	fmt.Println("c")

	// Transitively infallible: Fprint* into infallible writers.
	fmt.Fprint(&buf, "a")
	fmt.Fprintf(&buf, "%s", "b")
	fmt.Fprintln(&sb, "c")

	// Documented infallible: PipeWriter/PipeReader CloseWithError.
	pr, pw := io.Pipe()
	pw.CloseWithError(nil)
	pr.CloseWithError(nil)

	// Transitively infallible: io.WriteString to an infallible writer.
	io.WriteString(&buf, "x")
	io.WriteString(&sb, "y")

	// Documented infallible: math/rand package-level and method Read.
	p := make([]byte, 8)
	rand.Read(p)
	r := rand.New(rand.NewSource(1))
	r.Read(p)
}
`
	engine := setupProject(t, src)
	tmpl := &UncheckedError{IncludeExplicit: true, IncludeDefers: true}
	findings := RunTemplate(engine, tmpl)
	errs := findingsOfKind(findings, "unchecked_error")
	if len(errs) > 0 {
		t.Errorf("expected no unchecked_error findings for infallible stdlib calls, got %d:", len(errs))
		for _, f := range errs {
			t.Errorf("  %s", f.Message)
		}
	}
}

func TestUncheckedError_HashConformanceConcrete(t *testing.T) {
	// A type that structurally implements hash.Hash inherits the
	// "Write never returns an error" contract — even if it's a
	// concrete struct (not an interface value of static type hash.Hash)
	// and lives outside the stdlib hash package. This covers stdlib's
	// many concrete digest types (sha256.digest, sha3.state,
	// crypto/internal/fips140/...) and the user's own digests.
	src := `package testpkg

type myDigest struct{}

func (myDigest) Write(p []byte) (int, error) { return len(p), nil }
func (myDigest) Sum(b []byte) []byte         { return b }
func (myDigest) Reset()                      {}
func (myDigest) Size() int                   { return 32 }
func (myDigest) BlockSize() int              { return 64 }

func use() {
	var d myDigest
	d.Write([]byte("payload"))
}
`
	engine := setupProject(t, src)
	tmpl := &UncheckedError{}
	findings := RunTemplate(engine, tmpl)
	if got := len(findingsOfKind(findings, "unchecked_error")); got > 0 {
		t.Errorf("expected 0 findings for hash-shaped concrete type, got %d: %s", got, findings[0].Message)
	}
}

func TestUncheckedError_HashConformancePartialNotMatched(t *testing.T) {
	// A type that LOOKS like a hash but is missing one method does NOT
	// inherit the contract — guards against accidental over-recognition
	// (e.g., io.Writer, bufio.Writer, custom buffered writers all have
	// Write but not Sum/Reset/Size/BlockSize).
	src := `package testpkg

type writer struct{}

func (writer) Write(p []byte) (int, error) { return len(p), nil }

func use() {
	var w writer
	w.Write([]byte("payload"))
}
`
	engine := setupProject(t, src)
	tmpl := &UncheckedError{}
	findings := RunTemplate(engine, tmpl)
	if len(findingsOfKind(findings, "unchecked_error")) == 0 {
		t.Error("expected unchecked_error finding for plain Write-only type (missing Sum/Reset/Size/BlockSize)")
	}
}

func TestUncheckedError_HashConformanceDisabled(t *testing.T) {
	// Disabling hash.Hash.Write must also disable the structural
	// inheritance — otherwise the disable is bypassable just by using
	// a concrete digest type instead of an interface value.
	src := `package testpkg

type myDigest struct{}

func (myDigest) Write(p []byte) (int, error) { return len(p), nil }
func (myDigest) Sum(b []byte) []byte         { return b }
func (myDigest) Reset()                      {}
func (myDigest) Size() int                   { return 32 }
func (myDigest) BlockSize() int              { return 64 }

func use() {
	var d myDigest
	d.Write([]byte("payload"))
}
`
	engine := setupProject(t, src)
	infallibleState := NewInfallibleState(&pb.Infallible{
		DisableMethods: []string{"hash.Hash.Write"},
	})
	tmpl := &UncheckedError{}
	findings := RunTemplateWithConfig(engine, tmpl, infallibleState)
	if len(findingsOfKind(findings, "unchecked_error")) == 0 {
		t.Error("expected finding when hash.Hash.Write is disabled — structural inheritance must respect the disable")
	}
}

func TestUncheckedError_HashSubInterfaces(t *testing.T) {
	// hash.Hash32 and hash.Hash64 embed hash.Hash and inherit the same
	// "never returns an error" contract. Because the static type of the
	// receiver is Hash32/Hash64 (not Hash), the allowlist needs explicit
	// entries for each interface — fnv.New32a() and fnv.New64a() return
	// Hash32/Hash64 respectively.
	src := `package testpkg

import "hash/fnv"

func writes() {
	h32 := fnv.New32a()
	h32.Write([]byte("a"))

	h64 := fnv.New64a()
	h64.Write([]byte("b"))
}
`
	engine := setupProject(t, src)
	tmpl := &UncheckedError{}
	findings := RunTemplate(engine, tmpl)
	if got := len(findingsOfKind(findings, "unchecked_error")); got > 0 {
		t.Errorf("expected 0 findings for hash.Hash32/Hash64.Write, got %d: %s", got, findings[0].Message)
	}
}

func TestUncheckedError_WriterArgInfallibleStdoutStderr(t *testing.T) {
	// fmt.Fprint*/io.WriteString to os.Stdout or os.Stderr are
	// conventional logging — silenced by value-based writer recognition.
	src := `package testpkg

import (
	"fmt"
	"io"
	"os"
)

func writes() {
	fmt.Fprintln(os.Stdout, "a")
	fmt.Fprintln(os.Stderr, "b")
	fmt.Fprintf(os.Stderr, "%s", "c")
	io.WriteString(os.Stdout, "d")
}
`
	engine := setupProject(t, src)
	tmpl := &UncheckedError{}
	findings := RunTemplate(engine, tmpl)
	if got := len(findingsOfKind(findings, "unchecked_error")); got != 0 {
		t.Errorf("expected 0 findings (writes to os.Stdout/Stderr are infallible), got %d", got)
	}
}

func TestUncheckedError_WriterArgFallibleOtherFile(t *testing.T) {
	// A *os.File that is NOT os.Stdout/Stderr can fail (real disk file,
	// network FD) and must still be flagged. This guards against the
	// value-based check accidentally over-broadening to all *os.File.
	src := `package testpkg

import (
	"fmt"
	"io"
	"os"
)

func writes(f *os.File) {
	fmt.Fprintln(f, "a")
	io.WriteString(f, "b")
}
`
	engine := setupProject(t, src)
	tmpl := &UncheckedError{}
	findings := RunTemplate(engine, tmpl)
	if got := len(findingsOfKind(findings, "unchecked_error")); got != 2 {
		t.Errorf("expected 2 findings (Fprintln+WriteString to a non-Stdout/Stderr *os.File), got %d", got)
	}
}

func TestUncheckedError_WriterArgDisabledStderr(t *testing.T) {
	// A project that wants to check stderr writes can list "os.Stderr"
	// in disable_writers — the value-based shortcut must honor it.
	src := `package testpkg

import (
	"fmt"
	"os"
)

func writes() {
	fmt.Fprintln(os.Stderr, "a")
}
`
	engine := setupProject(t, src)
	infallibleState := NewInfallibleState(&pb.Infallible{
		DisableWriters: []string{"os.Stderr"},
	})
	tmpl := &UncheckedError{}
	findings := RunTemplateWithConfig(engine, tmpl, infallibleState)
	if got := len(findingsOfKind(findings, "unchecked_error")); got != 1 {
		t.Errorf("expected 1 finding (os.Stderr disabled), got %d", got)
	}
}

func TestUncheckedError_StillFlagsRealStdlib(t *testing.T) {
	// Sanity check: removing the noise must not silence real findings.
	// os.File.Close() does return a meaningful error and must still fire.
	src := `package testpkg

import "os"

func bad() {
	f, _ := os.Open("test.txt")
	f.Close()
}
`
	engine := setupProject(t, src)
	tmpl := &UncheckedError{}
	findings := RunTemplate(engine, tmpl)
	if len(findingsOfKind(findings, "unchecked_error")) == 0 {
		t.Error("expected unchecked_error finding for f.Close()")
	}
}

func TestUncheckedError_ConfigUserMethodExact(t *testing.T) {
	// User registers a third-party method as infallible. Because the
	// project source is testpkg, the type lives at testpkg.Logger.Debug.
	src := `package testpkg

type Logger struct{}

func (Logger) Debug(msg string) error { return nil }

func use() {
	var l Logger
	l.Debug("hi")
}
`
	engine := setupProject(t, src)
	infallibleState := NewInfallibleState(&pb.Infallible{
		Methods: []string{"testpkg.Logger.Debug"},
	})
	tmpl := &UncheckedError{}
	findings := RunTemplateWithConfig(engine, tmpl, infallibleState)
	if got := len(findingsOfKind(findings, "unchecked_error")); got != 0 {
		t.Errorf("expected 0 findings (Logger.Debug is allowlisted), got %d", got)
	}
}

func TestUncheckedError_ConfigUserMethodGlob(t *testing.T) {
	// Glob form: silence every method on Logger.
	src := `package testpkg

type Logger struct{}

func (Logger) Debug(msg string) error { return nil }
func (Logger) Info(msg string) error  { return nil }
func (Logger) Warn(msg string) error  { return nil }

func use() {
	var l Logger
	l.Debug("a")
	l.Info("b")
	l.Warn("c")
}
`
	engine := setupProject(t, src)
	infallibleState := NewInfallibleState(&pb.Infallible{
		Methods: []string{"testpkg.Logger.*"},
	})
	tmpl := &UncheckedError{}
	findings := RunTemplateWithConfig(engine, tmpl, infallibleState)
	if got := len(findingsOfKind(findings, "unchecked_error")); got != 0 {
		t.Errorf("expected 0 findings (Logger.* glob), got %d", got)
	}
}

func TestUncheckedError_ConfigUserFunction(t *testing.T) {
	// Register a package-qualified function (fmt.Errorf returns error;
	// as a bare expression statement, it would normally be flagged as
	// "silently discarded"). With user-function config, it's silenced.
	src := `package testpkg

import "fmt"

func use() {
	fmt.Errorf("oh no")
}
`
	engine := setupProject(t, src)
	infallibleState := NewInfallibleState(&pb.Infallible{
		Functions: []string{"fmt.Errorf"},
	})
	tmpl := &UncheckedError{}
	findings := RunTemplateWithConfig(engine, tmpl, infallibleState)
	if got := len(findingsOfKind(findings, "unchecked_error")); got != 0 {
		t.Errorf("expected 0 findings (fmt.Errorf is user-allowlisted), got %d", got)
	}
}

func TestUncheckedError_ConfigUserFunctionBareCall(t *testing.T) {
	// Project-local function called without a package qualifier (the
	// common case inside its own package). Must match the user-registered
	// allowlist via *ast.Ident, not just *ast.SelectorExpr.
	src := `package testpkg

func Trace(msg string) error { return nil }

func use() {
	Trace("x")
}
`
	engine := setupProject(t, src)
	infallibleState := NewInfallibleState(&pb.Infallible{
		Functions: []string{"testpkg.Trace"},
	})
	tmpl := &UncheckedError{}
	findings := RunTemplateWithConfig(engine, tmpl, infallibleState)
	if got := len(findingsOfKind(findings, "unchecked_error")); got != 0 {
		t.Errorf("expected 0 findings (intra-package bare call to allowlisted Trace), got %d", got)
	}
}

func TestUncheckedError_ConfigUserWriter(t *testing.T) {
	// User registers a third-party writer type as infallible; calls of
	// fmt.Fprint*(w, ...) where w is that type become transitively safe.
	src := `package testpkg

import "fmt"

type NopWriter struct{}

func (NopWriter) Write(p []byte) (int, error) { return len(p), nil }

func use() {
	var w NopWriter
	fmt.Fprintln(&w, "x")
	fmt.Fprintf(&w, "%s", "y")
}
`
	engine := setupProject(t, src)
	infallibleState := NewInfallibleState(&pb.Infallible{
		Writers: []string{"testpkg.NopWriter"},
	})
	tmpl := &UncheckedError{}
	findings := RunTemplateWithConfig(engine, tmpl, infallibleState)
	if got := len(findingsOfKind(findings, "unchecked_error")); got != 0 {
		t.Errorf("expected 0 findings (NopWriter is registered as infallible), got %d", got)
	}
}

func TestUncheckedError_ConfigDisableHardcoded(t *testing.T) {
	// User policy says they DO want bytes.Buffer.Write checked, even
	// though caliper hardcodes it as infallible. Disabled entry must
	// unmask the finding.
	src := `package testpkg

import "bytes"

func use() {
	var buf bytes.Buffer
	buf.Write([]byte("a"))
}
`
	engine := setupProject(t, src)
	infallibleState := NewInfallibleState(&pb.Infallible{
		DisableMethods: []string{"bytes.Buffer.Write"},
	})
	tmpl := &UncheckedError{}
	findings := RunTemplateWithConfig(engine, tmpl, infallibleState)
	if got := len(findingsOfKind(findings, "unchecked_error")); got != 1 {
		t.Errorf("expected 1 finding (disable_methods unmasks bytes.Buffer.Write), got %d", got)
	}
}

func TestUncheckedError_ConfigDisableGlob(t *testing.T) {
	// Disable a whole family with a glob.
	src := `package testpkg

import "bytes"

func use() {
	var buf bytes.Buffer
	buf.Write([]byte("a"))
	buf.WriteByte('b')
	buf.WriteString("c")
}
`
	engine := setupProject(t, src)
	infallibleState := NewInfallibleState(&pb.Infallible{
		DisableMethods: []string{"bytes.Buffer.Write*"},
	})
	tmpl := &UncheckedError{}
	findings := RunTemplateWithConfig(engine, tmpl, infallibleState)
	if got := len(findingsOfKind(findings, "unchecked_error")); got != 3 {
		t.Errorf("expected 3 findings (disable glob unmasks Write/WriteByte/WriteString), got %d", got)
	}
}

func TestUncheckedError_ConfigBadGlobDiagnostic(t *testing.T) {
	// A malformed glob (unmatched bracket) should be skipped without
	// crashing, and a diagnostic should be written to stderr so the user
	// can see why their pattern isn't taking effect. Valid entries in
	// the same list should still apply.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStderr := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = origStderr }()

	infallibleState := NewInfallibleState(&pb.Infallible{
		Methods: []string{
			"testpkg.Logger.De?bug",     // '?' is not a supported wildcard
			"testpkg.Logger.GoodMethod", // valid exact entry
		},
	})

	w.Close()
	out, _ := io.ReadAll(r)
	stderr := string(out)

	if !strings.Contains(stderr, "infallible.methods") {
		t.Errorf("expected diagnostic mentioning infallible.methods, got: %q", stderr)
	}
	if !strings.Contains(stderr, "De?bug") {
		t.Errorf("expected diagnostic to quote the bad entry, got: %q", stderr)
	}
	// Valid entry must still match.
	if !infallibleState.matchUserMethod("testpkg.Logger.GoodMethod") {
		t.Error("expected valid entry to remain registered after bad glob was skipped")
	}
}

func TestUncheckedError_CleanupPathClose(t *testing.T) {
	// Close()/closeBody()/Stop()-style discards inside an
	// `if err != nil { ... }` cleanup branch are conventional and
	// shouldn't be flagged. The original error is what the caller will
	// see; the cleanup's own error is intentionally dropped.
	src := `package testpkg

import "os"

func read() error {
	f, err := os.Open("x")
	if err != nil {
		return err
	}
	if _, err := f.Read(make([]byte, 1)); err != nil {
		f.Close()  // cleanup-path: must NOT be flagged
		return err
	}
	return f.Close()  // happy-path: handled
}
`
	engine := setupProject(t, src)
	tmpl := &UncheckedError{}
	findings := RunTemplate(engine, tmpl)
	if got := len(findingsOfKind(findings, "unchecked_error")); got > 0 {
		t.Errorf("expected 0 findings (Close in if-err cleanup branch), got %d: %s", got, findings[0].Message)
	}
}

func TestUncheckedError_NonCleanupCallInErrBranchStillFires(t *testing.T) {
	// Method-name filter: only Close-family calls get the cleanup-path
	// pass. A plain error-returning method discarded inside an if-err
	// block is still likely a real bug.
	src := `package testpkg

type T struct{}

func (T) DoWork() error { return nil }

func use(err error) {
	var t T
	if err != nil {
		t.DoWork()  // not a cleanup shape — should fire
	}
}
`
	engine := setupProject(t, src)
	tmpl := &UncheckedError{}
	findings := RunTemplate(engine, tmpl)
	if len(findingsOfKind(findings, "unchecked_error")) == 0 {
		t.Error("expected unchecked_error finding for non-cleanup method call inside if-err branch")
	}
}

func TestUncheckedError_CloseOutsideErrBranchStillFires(t *testing.T) {
	// Sanity guard: Close() in normal flow (not in a cleanup branch)
	// must still be flagged so we don't lose real findings.
	src := `package testpkg

import "os"

func bad() {
	f, _ := os.Open("test.txt")
	f.Close()  // not in any if-err branch — must fire
}
`
	engine := setupProject(t, src)
	tmpl := &UncheckedError{}
	findings := RunTemplate(engine, tmpl)
	if len(findingsOfKind(findings, "unchecked_error")) == 0 {
		t.Error("expected unchecked_error finding for Close() outside any cleanup branch")
	}
}

func TestUncheckedError_ConcurrentDifferentConfigs(t *testing.T) {
	// Two analyses on distinct projects with distinct InfallibleStates
	// must not see each other's allowlist. This is the regression
	// guard for the package-level state race that motivated moving
	// state onto TemplateContext. Run under -race for full effect.
	srcA := `package testpkg

type LoggerA struct{}

func (LoggerA) Log(msg string) error { return nil }

func use() {
	var l LoggerA
	l.Log("a")
}
`
	srcB := `package testpkg

type LoggerB struct{}

func (LoggerB) Log(msg string) error { return nil }

func use() {
	var l LoggerB
	l.Log("b")
}
`
	engineA := setupProject(t, srcA)
	engineB := setupProject(t, srcB)

	// A allowlists LoggerA only; B allowlists LoggerB only. If state
	// were package-global the second ConfigureInfallible would clobber
	// the first and at least one analysis would see the wrong allowlist.
	stateA := NewInfallibleState(&pb.Infallible{Methods: []string{"testpkg.LoggerA.Log"}})
	stateB := NewInfallibleState(&pb.Infallible{Methods: []string{"testpkg.LoggerB.Log"}})

	const iters = 20
	errCh := make(chan error, 2*iters)
	var wg sync.WaitGroup
	for i := 0; i < iters; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			fs := RunTemplateWithConfig(engineA, &UncheckedError{}, stateA)
			if got := len(findingsOfKind(fs, "unchecked_error")); got != 0 {
				errCh <- fmt.Errorf("engineA: expected 0 findings (LoggerA allowlisted), got %d", got)
			}
		}()
		go func() {
			defer wg.Done()
			fs := RunTemplateWithConfig(engineB, &UncheckedError{}, stateB)
			if got := len(findingsOfKind(fs, "unchecked_error")); got != 0 {
				errCh <- fmt.Errorf("engineB: expected 0 findings (LoggerB allowlisted), got %d", got)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

// --------------------------------------------------------------------------
// LockViolation tests
// --------------------------------------------------------------------------

func TestLockViolation_MissingUnlock(t *testing.T) {
	src := `package testpkg

import "sync"

func noUnlock() {
	var mu sync.Mutex
	mu.Lock()
	// no unlock
}
`
	engine := setupProject(t, src)
	tmpl := &LockViolation{}
	findings := RunTemplate(engine, tmpl)
	locks := findingsOfKind(findings, "lock_violation")
	if len(locks) == 0 {
		t.Error("expected lock violation finding for Lock without Unlock")
	}
}

func TestLockViolation_WithDeferUnlock(t *testing.T) {
	src := `package testpkg

import "sync"

func safe() {
	var mu sync.Mutex
	mu.Lock()
	defer mu.Unlock()
}
`
	engine := setupProject(t, src)
	tmpl := &LockViolation{}
	findings := RunTemplate(engine, tmpl)
	locks := findingsOfKind(findings, "lock_violation")
	if len(locks) > 0 {
		t.Errorf("expected no lock violation with defer Unlock, got %d: %s", len(locks), locks[0].Message)
	}
}

func TestLockViolation_UnlockOnOnePath(t *testing.T) {
	src := `package testpkg

import "sync"

func partial(flag bool) {
	var mu sync.Mutex
	mu.Lock()
	if flag {
		mu.Unlock()
		return
	}
	// error path: no unlock
}
`
	engine := setupProject(t, src)
	tmpl := &LockViolation{}
	findings := RunTemplate(engine, tmpl)
	locks := findingsOfKind(findings, "lock_violation")
	if len(locks) == 0 {
		t.Error("expected lock violation when Unlock only on one path")
	}
}

func TestLockViolation_BlockingUnderLock(t *testing.T) {
	// The blocking op must be in a different CFG block than Lock/Unlock
	// for NeverBetween to detect it (it checks successor blocks).
	src := `package testpkg

import "sync"

func blocking(flag bool) {
	var mu sync.Mutex
	ch := make(chan int)
	mu.Lock()
	if flag {
		ch <- 1
	}
	mu.Unlock()
}
`
	engine := setupProject(t, src)
	tmpl := &LockViolation{CheckBlockingUnderLock: true}
	findings := RunTemplate(engine, tmpl)
	blocking := findingsOfKind(findings, "blocking_under_lock")
	if len(blocking) == 0 {
		t.Error("expected blocking_under_lock finding for channel send while locked")
	}
}

func TestLockViolation_RLockRUnlock(t *testing.T) {
	src := `package testpkg

import "sync"

func readLockMissing() {
	var mu sync.RWMutex
	mu.RLock()
	// no RUnlock
}
`
	engine := setupProject(t, src)
	tmpl := &LockViolation{}
	findings := RunTemplate(engine, tmpl)
	locks := findingsOfKind(findings, "lock_violation")
	if len(locks) == 0 {
		t.Error("expected lock violation for RLock without RUnlock")
	}
}

func TestLockViolation_UnlockReturnedAsValue(t *testing.T) {
	// Pattern: function locks, then returns the Unlock method value for
	// the caller to invoke (e.g. via defer). The unlock obligation is
	// transferred — must not be flagged as missing.
	src := `package testpkg

import "sync"

func acquire() func() {
	var mu sync.Mutex
	mu.Lock()
	return mu.Unlock
}
`
	engine := setupProject(t, src)
	tmpl := &LockViolation{}
	findings := RunTemplate(engine, tmpl)
	if got := len(findingsOfKind(findings, "lock_violation")); got > 0 {
		t.Errorf("expected no lock_violation when Unlock is returned as a method value, got %d", got)
	}
}

func TestLockViolation_UnlockPassedAsArg(t *testing.T) {
	// Pattern: function locks, then passes the Unlock method value to
	// another function that will invoke it. Same transfer semantics.
	src := `package testpkg

import "sync"

func register(fn func()) {}

func acquire() {
	var mu sync.Mutex
	mu.Lock()
	register(mu.Unlock)
}
`
	engine := setupProject(t, src)
	tmpl := &LockViolation{}
	findings := RunTemplate(engine, tmpl)
	if got := len(findingsOfKind(findings, "lock_violation")); got > 0 {
		t.Errorf("expected no lock_violation when Unlock is passed as arg, got %d", got)
	}
}

func TestLockViolation_StillFlagsTrueMissing(t *testing.T) {
	// Sanity: the transfer-detection escape hatch must NOT silence a
	// genuine missing-unlock case.
	src := `package testpkg

import "sync"

func leaked() {
	var mu sync.Mutex
	mu.Lock()
	// no Unlock anywhere — true bug
}
`
	engine := setupProject(t, src)
	tmpl := &LockViolation{}
	findings := RunTemplate(engine, tmpl)
	if len(findingsOfKind(findings, "lock_violation")) == 0 {
		t.Error("expected lock_violation for genuine missing Unlock")
	}
}

// --------------------------------------------------------------------------
// NilDeref tests
// --------------------------------------------------------------------------

func TestNilDeref_UncheckedErrorReturn(t *testing.T) {
	// The dereference must be in a different CFG block than the assignment
	// for checkGuardBeforeDeref to detect it (it skips the start block).
	src := `package testpkg

import "os"

func unchecked(flag bool) {
	f, err := os.Open("test.txt")
	if flag {
		f.Close()
	}
	_ = err
}
`
	engine := setupProject(t, src)
	tmpl := &NilDeref{}
	findings := RunTemplate(engine, tmpl)
	nils := findingsOfKind(findings, "nil_deref")
	if len(nils) == 0 {
		t.Error("expected nil deref finding when error not checked before use")
	}
}

func TestNilDeref_CheckedError(t *testing.T) {
	src := `package testpkg

import "os"

func checked() {
	f, err := os.Open("test.txt")
	if err != nil {
		return
	}
	f.Close()
}
`
	engine := setupProject(t, src)
	tmpl := &NilDeref{}
	findings := RunTemplate(engine, tmpl)
	nils := findingsOfKind(findings, "nil_deref")
	if len(nils) > 0 {
		t.Errorf("expected no nil deref when error checked first, got %d: %s", len(nils), nils[0].Message)
	}
}

func TestNilDeref_MapLookup(t *testing.T) {
	src := `package testpkg

type Thing struct{ Name string }

func mapLookup(flag bool) {
	m := map[string]*Thing{"a": {Name: "a"}}
	v, ok := m["a"]
	if flag {
		v.Name = "b"
	}
	_ = ok
}
`
	engine := setupProject(t, src)
	tmpl := &NilDeref{}
	findings := RunTemplate(engine, tmpl)
	nils := findingsOfKind(findings, "nil_deref")
	if len(nils) == 0 {
		t.Error("expected nil deref finding for map lookup without ok check")
	}
}

func TestNilDeref_MapLookupChecked(t *testing.T) {
	src := `package testpkg

type Item struct{ Name string }

func mapChecked(flag bool) {
	m := map[string]*Item{"a": {Name: "a"}}
	v, ok := m["a"]
	if !ok {
		return
	}
	if flag {
		v.Name = "b"
	}
}
`
	engine := setupProject(t, src)
	tmpl := &NilDeref{}
	findings := RunTemplate(engine, tmpl)
	nils := findingsOfKind(findings, "nil_deref")
	if len(nils) > 0 {
		t.Errorf("expected no nil deref when ok is checked, got %d: %s", len(nils), nils[0].Message)
	}
}

func TestNilDeref_TypeAssertInlineIf(t *testing.T) {
	// The most common Go idiom: if x, ok := expr.(Type); ok { x.Method() }
	// This must NOT be flagged — ok IS checked as the if condition.
	src := `package testpkg

import "io"

func typeAssertInline(v interface{}) {
	if r, ok := v.(io.Reader); ok {
		r.Read(nil)
	}
}
`
	engine := setupProject(t, src)
	tmpl := &NilDeref{}
	findings := RunTemplate(engine, tmpl)
	nils := findingsOfKind(findings, "nil_deref")
	if len(nils) > 0 {
		t.Errorf("expected no nil deref for if r, ok := v.(Type); ok { r.Use() }, got %d: %s", len(nils), nils[0].Message)
	}
}

func TestNilDeref_TypeAssertSeparate(t *testing.T) {
	// Separated form: x, ok := expr.(Type); if ok { x.Method() }
	src := `package testpkg

import "io"

func typeAssertSeparate(v interface{}) {
	r, ok := v.(io.Reader)
	if ok {
		r.Read(nil)
	}
}
`
	engine := setupProject(t, src)
	tmpl := &NilDeref{}
	findings := RunTemplate(engine, tmpl)
	nils := findingsOfKind(findings, "nil_deref")
	if len(nils) > 0 {
		t.Errorf("expected no nil deref for x, ok := v.(Type); if ok { x.Use() }, got %d: %s", len(nils), nils[0].Message)
	}
}

func TestNilDeref_TypeAssertUnchecked(t *testing.T) {
	// Type assertion with ok not checked — this SHOULD be flagged.
	// The dereference must be in a different CFG block than the assignment
	// (checkGuardBeforeDeref skips the start block).
	src := `package testpkg

import "io"

func typeAssertUnchecked(v interface{}, flag bool) {
	r, ok := v.(io.Reader)
	if flag {
		r.Read(nil)
	}
	_ = ok
}
`
	engine := setupProject(t, src)
	tmpl := &NilDeref{}
	findings := RunTemplate(engine, tmpl)
	nils := findingsOfKind(findings, "nil_deref")
	if len(nils) == 0 {
		t.Error("expected nil deref finding for type assertion with ok not checked before use")
	}
}

func TestNilDeref_ValueNilCheck(t *testing.T) {
	// Value checked for nil directly: x, _ := expr.(Type); if x != nil { x.Use() }
	src := `package testpkg

type Closer interface{ Close() }

func valueNilCheck(v interface{}) {
	c, _ := v.(Closer)
	if c != nil {
		c.Close()
	}
}
`
	engine := setupProject(t, src)
	tmpl := &NilDeref{}
	findings := RunTemplate(engine, tmpl)
	nils := findingsOfKind(findings, "nil_deref")
	if len(nils) > 0 {
		t.Errorf("expected no nil deref when value is nil-checked, got %d: %s", len(nils), nils[0].Message)
	}
}

func TestNilDeref_ShortCircuitNilCheck(t *testing.T) {
	// Short-circuit && with nil check: x != nil && x.Field
	src := `package testpkg

import "net/url"

func shortCircuit() {
	u, _ := url.Parse("http://example.com")
	if u != nil && u.Scheme == "https" {
		_ = u.Host
	}
}
`
	engine := setupProject(t, src)
	tmpl := &NilDeref{}
	findings := RunTemplate(engine, tmpl)
	nils := findingsOfKind(findings, "nil_deref")
	if len(nils) > 0 {
		t.Errorf("expected no nil deref for x != nil && x.Field short-circuit, got %d: %s", len(nils), nils[0].Message)
	}
}

// --------------------------------------------------------------------------
// SQLInjection tests
// --------------------------------------------------------------------------

func TestSQLInjection_Concatenation(t *testing.T) {
	src := `package testpkg

type DB struct{}

func (db *DB) Query(query string, args ...interface{}) {}
func (db *DB) Exec(query string, args ...interface{}) {}

func vulnerable(id string) {
	db := &DB{}
	db.Query("SELECT * FROM users WHERE id=" + id)
}
`
	engine := setupProject(t, src)
	tmpl := &SQLInjection{}
	findings := RunTemplate(engine, tmpl)
	sqli := findingsOfKind(findings, "sql_injection")
	if len(sqli) == 0 {
		t.Error("expected sql_injection finding for string concatenation in Query")
	}
}

func TestSQLInjection_Parameterized(t *testing.T) {
	src := `package testpkg

type DB struct{}

func (db *DB) Query(query string, args ...interface{}) {}

func safe(id string) {
	db := &DB{}
	db.Query("SELECT * FROM users WHERE id=?", id)
}
`
	engine := setupProject(t, src)
	tmpl := &SQLInjection{}
	findings := RunTemplate(engine, tmpl)
	sqli := findingsOfKind(findings, "sql_injection")
	if len(sqli) > 0 {
		t.Errorf("expected no sql_injection finding for parameterized query, got %d: %s", len(sqli), sqli[0].Message)
	}
}

func TestSQLInjection_StringLiteral(t *testing.T) {
	src := `package testpkg

type DB struct{}

func (db *DB) Query(query string, args ...interface{}) {}

func literal() {
	db := &DB{}
	db.Query("SELECT 1")
}
`
	engine := setupProject(t, src)
	tmpl := &SQLInjection{}
	findings := RunTemplate(engine, tmpl)
	sqli := findingsOfKind(findings, "sql_injection")
	if len(sqli) > 0 {
		t.Errorf("expected no sql_injection finding for string literal query, got %d: %s", len(sqli), sqli[0].Message)
	}
}

func TestSQLInjection_Sprintf(t *testing.T) {
	src := `package testpkg

import "fmt"

type DB struct{}

func (db *DB) Query(query string, args ...interface{}) {}

func sprintfInjection(name string) {
	db := &DB{}
	db.Query(fmt.Sprintf("SELECT * FROM users WHERE name='%s'", name))
}
`
	engine := setupProject(t, src)
	tmpl := &SQLInjection{}
	findings := RunTemplate(engine, tmpl)
	sqli := findingsOfKind(findings, "sql_injection")
	if len(sqli) == 0 {
		t.Error("expected sql_injection finding for fmt.Sprintf query construction")
	}
}

// --------------------------------------------------------------------------
// ContextMisuse tests
// --------------------------------------------------------------------------

func TestContextMisuse_BackgroundWithParent(t *testing.T) {
	src := `package testpkg

import "context"

func hasParent(ctx context.Context) {
	bg := context.Background()
	_ = bg
}
`
	engine := setupProject(t, src)
	tmpl := &ContextMisuse{FlagTodo: true}
	findings := RunTemplate(engine, tmpl)
	misuse := findingsOfKind(findings, "context_misuse")
	if len(misuse) == 0 {
		t.Error("expected context_misuse finding for context.Background() when parent ctx available")
	}
}

func TestContextMisuse_NoParentContext(t *testing.T) {
	src := `package testpkg

import "context"

func noParent() {
	bg := context.Background()
	_ = bg
}
`
	engine := setupProject(t, src)
	tmpl := &ContextMisuse{FlagTodo: true}
	findings := RunTemplate(engine, tmpl)
	// Filter to only Background-related findings.
	var bgFindings []Finding
	for _, f := range findingsOfKind(findings, "context_misuse") {
		if strings.Contains(f.Message, "Background") {
			bgFindings = append(bgFindings, f)
		}
	}
	if len(bgFindings) > 0 {
		t.Errorf("expected no context_misuse finding for Background() without parent ctx, got %d: %s",
			len(bgFindings), bgFindings[0].Message)
	}
}

func TestContextMisuse_TodoFlagged(t *testing.T) {
	src := `package testpkg

import "context"

func usesTodo() {
	ctx := context.TODO()
	_ = ctx
}
`
	engine := setupProject(t, src)
	tmpl := &ContextMisuse{FlagTodo: true}
	findings := RunTemplate(engine, tmpl)
	misuse := findingsOfKind(findings, "context_misuse")
	if len(misuse) == 0 {
		t.Error("expected context_misuse finding for context.TODO() when FlagTodo=true")
	}
}

func TestContextMisuse_TodoNotFlagged(t *testing.T) {
	src := `package testpkg

import "context"

func usesTodo() {
	ctx := context.TODO()
	_ = ctx
}
`
	engine := setupProject(t, src)
	tmpl := &ContextMisuse{FlagTodo: false}
	findings := RunTemplate(engine, tmpl)
	misuse := findingsOfKind(findings, "context_misuse")
	if len(misuse) > 0 {
		t.Errorf("expected no context_misuse finding for context.TODO() when FlagTodo=false, got %d: %s",
			len(misuse), misuse[0].Message)
	}
}

// --------------------------------------------------------------------------
// Suppression tests
// --------------------------------------------------------------------------

func TestSuppression_CaliperIgnore(t *testing.T) {
	src := `package testpkg

import "os"

func suppressed() {
	_, _ = os.Open("file.txt") //caliper:ignore error_swallow
}
`
	engine := setupProject(t, src)
	tmpl := &ErrorSwallow{}
	findings := RunTemplate(engine, tmpl)
	errs := findingsOfKind(findings, "error_swallow")
	if len(errs) > 0 {
		t.Errorf("expected finding to be suppressed by //caliper:ignore, got %d: %s", len(errs), errs[0].Message)
	}
}

func TestSuppression_NotSuppressed(t *testing.T) {
	src := `package testpkg

import "os"

func notSuppressed() {
	_, _ = os.Open("file.txt")
}
`
	engine := setupProject(t, src)
	tmpl := &ErrorSwallow{}
	findings := RunTemplate(engine, tmpl)
	errs := findingsOfKind(findings, "error_swallow")
	if len(errs) == 0 {
		t.Error("expected error_swallow finding when not suppressed")
	}
}

// --------------------------------------------------------------------------
// CheckInterfaceSatisfaction tests
// --------------------------------------------------------------------------

func TestCheckInterface_Satisfies(t *testing.T) {
	src := `package testpkg

import "io"

type MyReader struct{}

func (r *MyReader) Read(p []byte) (int, error) {
	return 0, io.EOF
}
`
	engine := setupProject(t, src)
	result := CheckInterfaceSatisfaction(engine, "MyReader", "io.Reader")
	if !result.Satisfies {
		t.Errorf("expected MyReader to satisfy io.Reader, got missing: %v", result.Missing)
	}
}

func TestCheckInterface_MissingMethod(t *testing.T) {
	src := `package testpkg

import _ "io"

type Incomplete struct{}
`
	engine := setupProject(t, src)
	result := CheckInterfaceSatisfaction(engine, "Incomplete", "io.Reader")
	if result.Satisfies {
		t.Error("expected Incomplete to NOT satisfy io.Reader")
	}
	if len(result.Missing) == 0 {
		t.Error("expected missing methods reported")
	}
}

// --------------------------------------------------------------------------
// SliceAppendRace tests
// --------------------------------------------------------------------------

func TestSliceAppendRace_SharedAppend(t *testing.T) {
	src := `package testpkg

func collector() {
	var results []int
	for i := 0; i < 10; i++ {
		i := i
		go func() {
			results = append(results, i)
		}()
	}
	_ = results
}
`
	engine := setupProject(t, src)
	tmpl := &SliceAppendRace{}
	findings := RunTemplate(engine, tmpl)
	races := findingsOfKind(findings, "slice_append_race")
	if len(races) == 0 {
		t.Error("expected slice_append_race finding for shared slice append in goroutine")
	}
}

func TestSliceAppendRace_LocalSlice(t *testing.T) {
	// Slice declared inside the closure is safe.
	src := `package testpkg

func localSlice() {
	go func() {
		var results []int
		results = append(results, 1)
		_ = results
	}()
}
`
	engine := setupProject(t, src)
	tmpl := &SliceAppendRace{}
	findings := RunTemplate(engine, tmpl)
	races := findingsOfKind(findings, "slice_append_race")
	if len(races) > 0 {
		t.Errorf("expected no slice_append_race for local slice, got %d: %s", len(races), races[0].Message)
	}
}

func TestSliceAppendRace_LockedInline(t *testing.T) {
	// Append protected by a Lock()/Unlock() pair in the same block —
	// must not flag. This is the pattern caliper's own RunTemplate
	// uses, and the original false positive that motivated the fix.
	src := `package testpkg

import "sync"

func collector() {
	var (
		mu      sync.Mutex
		results []int
	)
	for i := 0; i < 10; i++ {
		i := i
		go func() {
			mu.Lock()
			results = append(results, i)
			mu.Unlock()
		}()
	}
	_ = results
}
`
	engine := setupProject(t, src)
	tmpl := &SliceAppendRace{}
	findings := RunTemplate(engine, tmpl)
	races := findingsOfKind(findings, "slice_append_race")
	if len(races) > 0 {
		t.Errorf("expected no slice_append_race when append is bracketed by Lock/Unlock, got %d: %s", len(races), races[0].Message)
	}
}

func TestSliceAppendRace_LockedDeferred(t *testing.T) {
	// Append protected by Lock() + deferred Unlock() — the most common
	// Go idiom. Must not flag.
	src := `package testpkg

import "sync"

func collector() {
	var (
		mu      sync.Mutex
		results []int
	)
	for i := 0; i < 10; i++ {
		i := i
		go func() {
			mu.Lock()
			defer mu.Unlock()
			results = append(results, i)
		}()
	}
	_ = results
}
`
	engine := setupProject(t, src)
	tmpl := &SliceAppendRace{}
	findings := RunTemplate(engine, tmpl)
	races := findingsOfKind(findings, "slice_append_race")
	if len(races) > 0 {
		t.Errorf("expected no slice_append_race when Lock + defer Unlock, got %d: %s", len(races), races[0].Message)
	}
}

func TestSliceAppendRace_LockOnlyAfterAppend(t *testing.T) {
	// Lock taken AFTER the append doesn't protect it — must still flag.
	src := `package testpkg

import "sync"

func collector() {
	var (
		mu      sync.Mutex
		results []int
	)
	for i := 0; i < 10; i++ {
		i := i
		go func() {
			results = append(results, i)
			mu.Lock()
			mu.Unlock()
		}()
	}
	_ = results
}
`
	engine := setupProject(t, src)
	tmpl := &SliceAppendRace{}
	findings := RunTemplate(engine, tmpl)
	if len(findingsOfKind(findings, "slice_append_race")) == 0 {
		t.Error("expected slice_append_race when Lock comes after the append (still racy)")
	}
}

// --------------------------------------------------------------------------
// ErrorSwallow tests
// --------------------------------------------------------------------------

func TestErrorSwallow_ExplicitBlank(t *testing.T) {
	src := `package testpkg

import "os"

func swallower() {
	_, _ = os.Open("file.txt")
}
`
	engine := setupProject(t, src)
	tmpl := &ErrorSwallow{}
	findings := RunTemplate(engine, tmpl)
	errs := findingsOfKind(findings, "error_swallow")
	if len(errs) == 0 {
		t.Error("expected error_swallow finding for _, _ = os.Open()")
	}
}

func TestErrorSwallow_HandledError(t *testing.T) {
	src := `package testpkg

import "os"

func handler() error {
	f, err := os.Open("file.txt")
	if err != nil {
		return err
	}
	f.Close()
	return nil
}
`
	engine := setupProject(t, src)
	tmpl := &ErrorSwallow{}
	findings := RunTemplate(engine, tmpl)
	errs := findingsOfKind(findings, "error_swallow")
	if len(errs) > 0 {
		t.Errorf("expected no error_swallow finding when error is handled, got %d: %s", len(errs), errs[0].Message)
	}
}

func TestErrorSwallow_DroppedReturn(t *testing.T) {
	src := `package testpkg

import "os"

func dropper() {
	f, _ := os.Open("file.txt")
	f.Close()
}
`
	engine := setupProject(t, src)
	// With IncludeDropped, f.Close() should be flagged.
	tmpl := &ErrorSwallow{IncludeDropped: true}
	findings := RunTemplate(engine, tmpl)
	// Should find both: the _ in os.Open AND the dropped f.Close()
	errs := findingsOfKind(findings, "error_swallow")
	if len(errs) < 2 {
		t.Errorf("expected at least 2 error_swallow findings (blank + dropped), got %d", len(errs))
	}
}

// --------------------------------------------------------------------------
// DeferInLoop tests
// --------------------------------------------------------------------------

func TestDeferInLoop_DeferInForLoop(t *testing.T) {
	src := `package testpkg

import "os"

func leaky() {
	for i := 0; i < 10; i++ {
		f, _ := os.Open("file.txt")
		defer f.Close()
	}
}
`
	engine := setupProject(t, src)
	tmpl := &DeferInLoop{}
	findings := RunTemplate(engine, tmpl)
	defers := findingsOfKind(findings, "defer_in_loop")
	if len(defers) == 0 {
		t.Error("expected defer_in_loop finding for defer inside for loop")
	}
}

func TestDeferInLoop_DeferInRangeLoop(t *testing.T) {
	src := `package testpkg

import "os"

func leakyRange(files []string) {
	for _, name := range files {
		f, _ := os.Open(name)
		defer f.Close()
	}
}
`
	engine := setupProject(t, src)
	tmpl := &DeferInLoop{}
	findings := RunTemplate(engine, tmpl)
	defers := findingsOfKind(findings, "defer_in_loop")
	if len(defers) == 0 {
		t.Error("expected defer_in_loop finding for defer inside range loop")
	}
}

func TestDeferInLoop_DeferOutsideLoop(t *testing.T) {
	// Defer outside a loop is fine.
	src := `package testpkg

import "os"

func safe() {
	f, _ := os.Open("file.txt")
	defer f.Close()
}
`
	engine := setupProject(t, src)
	tmpl := &DeferInLoop{}
	findings := RunTemplate(engine, tmpl)
	defers := findingsOfKind(findings, "defer_in_loop")
	if len(defers) > 0 {
		t.Errorf("expected no defer_in_loop finding for defer outside loop, got %d: %s", len(defers), defers[0].Message)
	}
}

func TestDeferInLoop_DeferInClosureInLoop(t *testing.T) {
	// Defer inside a closure called in a loop is safe — the closure
	// creates its own defer scope.
	src := `package testpkg

import "os"

func safeWithClosure(files []string) {
	for _, name := range files {
		func() {
			f, _ := os.Open(name)
			defer f.Close()
		}()
	}
}
`
	engine := setupProject(t, src)
	tmpl := &DeferInLoop{}
	findings := RunTemplate(engine, tmpl)
	defers := findingsOfKind(findings, "defer_in_loop")
	if len(defers) > 0 {
		t.Errorf("expected no defer_in_loop finding for defer inside closure in loop, got %d: %s", len(defers), defers[0].Message)
	}
}

// --------------------------------------------------------------------------
// GoroutineLeak tests
// --------------------------------------------------------------------------

func TestGoroutineLeak_NoTermination(t *testing.T) {
	src := `package testpkg

func work() {}

func leaker() {
	go func() {
		for {
			work()
		}
	}()
}
`
	engine := setupProject(t, src)
	tmpl := &GoroutineLeak{}
	findings := RunTemplate(engine, tmpl)
	leaks := findingsOfKind(findings, "goroutine_leak")
	if len(leaks) == 0 {
		t.Error("expected goroutine leak finding for goroutine without termination mechanism")
	}
}

func TestGoroutineLeak_WithContextDone(t *testing.T) {
	src := `package testpkg

import "context"

func safe(ctx context.Context) {
	go func() {
		<-ctx.Done()
	}()
}
`
	engine := setupProject(t, src)
	tmpl := &GoroutineLeak{}
	findings := RunTemplate(engine, tmpl)
	leaks := findingsOfKind(findings, "goroutine_leak")
	if len(leaks) > 0 {
		t.Errorf("expected no goroutine leak with ctx.Done(), got %d: %s", len(leaks), leaks[0].Message)
	}
}

func TestGoroutineLeak_NoGoroutines(t *testing.T) {
	src := `package testpkg

func noGoroutines() {
	x := 1
	_ = x
}
`
	engine := setupProject(t, src)
	tmpl := &GoroutineLeak{}
	findings := RunTemplate(engine, tmpl)
	leaks := findingsOfKind(findings, "goroutine_leak")
	if len(leaks) > 0 {
		t.Errorf("expected no findings for function without goroutines, got %d", len(leaks))
	}
}

func TestGoroutineLeak_OneShotWorker(t *testing.T) {
	// A goroutine that does finite work and returns should NOT be flagged.
	src := `package testpkg

import "fmt"

func oneShotWorker() {
	go func() {
		result := compute()
		fmt.Println(result)
	}()
}

func compute() int { return 42 }
`
	engine := setupProject(t, src)
	tmpl := &GoroutineLeak{}
	findings := RunTemplate(engine, tmpl)
	leaks := findingsOfKind(findings, "goroutine_leak")
	if len(leaks) > 0 {
		t.Errorf("expected no goroutine leak for one-shot worker, got %d: %s", len(leaks), leaks[0].Message)
	}
}

// --------------------------------------------------------------------------
// RaceCondition tests
// --------------------------------------------------------------------------

func TestRaceCondition_SharedVariable(t *testing.T) {
	src := `package testpkg

func racer() {
	x := 0
	go func() {
		x = 1
	}()
	x = 2
	_ = x
}
`
	engine := setupProject(t, src)
	tmpl := &RaceCondition{}
	findings := RunTemplate(engine, tmpl)
	races := findingsOfKind(findings, "race_condition")
	if len(races) == 0 {
		t.Error("expected race condition finding for shared variable modified in goroutine and main")
	}
}

func TestRaceCondition_NoSharing(t *testing.T) {
	src := `package testpkg

func noRace() {
	go func() {
		x := 1
		_ = x
	}()
}
`
	engine := setupProject(t, src)
	tmpl := &RaceCondition{}
	findings := RunTemplate(engine, tmpl)
	races := findingsOfKind(findings, "race_condition")
	if len(races) > 0 {
		t.Errorf("expected no race condition findings for goroutine with only local variables, got %d: %s", len(races), races[0].Message)
	}
}

func TestRaceCondition_LoopVariable_Pre122(t *testing.T) {
	// Pre-Go-1.22, the loop variable is reused across iterations, so a
	// goroutine capturing `i` races with the loop's iteration update.
	src := `package testpkg

import "fmt"

func loopRace() {
	for i := 0; i < 10; i++ {
		go func() {
			fmt.Println(i)
		}()
	}
}
`
	engine := setupProjectWithGoVersion(t, src, "1.21")
	tmpl := &RaceCondition{}
	findings := RunTemplate(engine, tmpl)
	races := findingsOfKind(findings, "race_condition")
	if len(races) == 0 {
		t.Error("expected race condition finding for pre-1.22 loop-var capture")
	}
}

func TestRaceCondition_LoopVariable_Post122(t *testing.T) {
	// Go 1.22+ scopes loop variables per iteration, so capturing `i` in a
	// goroutine that does not write it is safe and must not be flagged.
	src := `package testpkg

import "fmt"

func loopRace() {
	for i := 0; i < 10; i++ {
		go func() {
			fmt.Println(i)
		}()
	}
}
`
	engine := setupProjectWithGoVersion(t, src, "1.22")
	tmpl := &RaceCondition{}
	findings := RunTemplate(engine, tmpl)
	races := findingsOfKind(findings, "race_condition")
	if len(races) > 0 {
		t.Errorf("expected no race finding under Go 1.22+ per-iteration scoping, got %d: %s", len(races), races[0].Message)
	}
}

func TestRaceCondition_NonLoopVarStillRacy_Post122(t *testing.T) {
	// Per-iteration loop scoping doesn't change the race story for
	// non-loop variables. A shared counter mutated by both goroutine
	// and the surrounding code still races, even under Go 1.22+.
	src := `package testpkg

func loopRace() {
	count := 0
	for i := 0; i < 10; i++ {
		go func() {
			count = count + 1
		}()
		count = count + 1
	}
	_ = count
}
`
	engine := setupProjectWithGoVersion(t, src, "1.22")
	tmpl := &RaceCondition{}
	findings := RunTemplate(engine, tmpl)
	races := findingsOfKind(findings, "race_condition")
	if len(races) == 0 {
		t.Error("expected race finding for non-loop captured var written from goroutine")
	}
}

func TestRaceCondition_ChannelVariable(t *testing.T) {
	// Channels are designed for concurrent use and should not be flagged.
	src := `package testpkg

func chanUser() {
	ch := make(chan int, 1)
	go func() {
		ch <- 42
	}()
	<-ch
}
`
	engine := setupProject(t, src)
	tmpl := &RaceCondition{}
	findings := RunTemplate(engine, tmpl)
	races := findingsOfKind(findings, "race_condition")
	if len(races) > 0 {
		t.Errorf("expected no race for channel variable, got %d: %s", len(races), races[0].Message)
	}
}

func TestRaceCondition_ReadOnlyCapture(t *testing.T) {
	// Read-only capture of an immutable value type (string) should not be flagged.
	src := `package testpkg

import "fmt"

func readOnly() {
	path := "/tmp/test"
	go func() {
		fmt.Println(path)
	}()
	fmt.Println(path)
}
`
	engine := setupProject(t, src)
	tmpl := &RaceCondition{}
	findings := RunTemplate(engine, tmpl)
	races := findingsOfKind(findings, "race_condition")
	if len(races) > 0 {
		t.Errorf("expected no race for read-only string capture, got %d: %s", len(races), races[0].Message)
	}
}

func TestRaceCondition_ChannelSync(t *testing.T) {
	// Variable access after a channel receive is synchronized.
	src := `package testpkg

import "fmt"

func chanSync() {
	result := 0
	done := make(chan bool, 1)
	go func() {
		result = 42
		done <- true
	}()
	<-done
	fmt.Println(result)
}
`
	engine := setupProject(t, src)
	tmpl := &RaceCondition{}
	findings := RunTemplate(engine, tmpl)
	races := findingsOfKind(findings, "race_condition")
	if len(races) > 0 {
		t.Errorf("expected no race for channel-synchronized variable, got %d: %s", len(races), races[0].Message)
	}
}

func TestRaceCondition_WaitGroupSync(t *testing.T) {
	// Variable access after wg.Wait() is synchronized.
	src := `package testpkg

import "sync"

func wgSync() {
	var wg sync.WaitGroup
	result := 0
	wg.Add(1)
	go func() {
		defer wg.Done()
		result = 42
	}()
	wg.Wait()
	_ = result
}
`
	engine := setupProject(t, src)
	tmpl := &RaceCondition{}
	findings := RunTemplate(engine, tmpl)
	races := findingsOfKind(findings, "race_condition")
	if len(races) > 0 {
		t.Errorf("expected no race for WaitGroup-synchronized variable, got %d: %s", len(races), races[0].Message)
	}
}

func TestRaceCondition_ReadOnlyPointerCapture(t *testing.T) {
	// Read-only pointer capture — goroutine calls methods but doesn't
	// reassign the variable. Not a race.
	src := `package testpkg

import "fmt"

type Watcher struct{ Name string }

func (w *Watcher) Watch() string { return w.Name }

func readOnlyPointer() {
	w := &Watcher{Name: "test"}
	go func() {
		fmt.Println(w.Watch())
	}()
	fmt.Println(w.Watch())
}
`
	engine := setupProject(t, src)
	tmpl := &RaceCondition{}
	findings := RunTemplate(engine, tmpl)
	races := findingsOfKind(findings, "race_condition")
	if len(races) > 0 {
		t.Errorf("expected no race for read-only pointer capture, got %d: %s", len(races), races[0].Message)
	}
}
