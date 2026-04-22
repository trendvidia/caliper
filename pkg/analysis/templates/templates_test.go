package templates

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trendvidia/caliper/pkg/analysis"
)

// setupProject creates a temp Go project with the given source file and
// returns an analysis.Engine loaded from it.
func setupProject(t *testing.T, src string) *analysis.Engine {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module testpkg\n\ngo 1.23\n"), 0o644); err != nil {
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

func TestRaceCondition_LoopVariable(t *testing.T) {
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
	engine := setupProject(t, src)
	tmpl := &RaceCondition{}
	findings := RunTemplate(engine, tmpl)
	races := findingsOfKind(findings, "race_condition")
	if len(races) == 0 {
		t.Error("expected race condition finding for loop variable captured by goroutine closure")
	}
}
