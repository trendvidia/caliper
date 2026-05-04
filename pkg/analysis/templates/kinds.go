package templates

// ByKind returns a canonical Template instance for the given rule kind,
// or nil if the kind is unknown OR requires constructor parameters that
// can't be defaulted (today: only "resource_leak", which needs an
// acquire pattern + release method).
//
// External entrypoints — the MCP server's per-finding "explain trace"
// path and cmd/caliper-analyze's --rules flag — both consume rule kinds
// as strings; this is the single source of truth for the mapping.
//
// Adding a new analyzer? Append a case here and a corresponding entry
// to DefaultKinds (if it's appropriate to run by default).
func ByKind(kind string) Template {
	switch kind {
	case "nil_deref":
		return &NilDeref{}
	case "unchecked_error":
		return &UncheckedError{}
	case "lock_violation", "blocking_under_lock":
		return &LockViolation{CheckBlockingUnderLock: true}
	case "goroutine_leak":
		return &GoroutineLeak{}
	case "race_condition":
		return &RaceCondition{}
	case "context_misuse":
		return &ContextMisuse{FlagTodo: true}
	case "defer_in_loop":
		return &DeferInLoop{}
	case "error_swallow":
		return &UncheckedError{Strict: true, KindName: "error_swallow"}
	case "slice_append_race":
		return &SliceAppendRace{}
	case "sql_injection":
		return &SQLInjection{}
	}
	return nil
}

// DefaultKinds is the canonical "run on every commit / PR" rule set.
// Excludes rules that need parameters (resource_leak), exploration-only
// tools (find_pattern, find_type_usage), and aliases (blocking_under_lock
// is the same template as lock_violation under a different name).
//
// Stable: external CI integrations (cmd/caliper-analyze with no
// --rules flag) consume this list as their default. Adding a kind to
// DefaultKinds means existing CI runs immediately start producing
// findings under that kind — treat this as a versioned contract.
var DefaultKinds = []string{
	"nil_deref",
	"unchecked_error",
	"lock_violation",
	"goroutine_leak",
	"race_condition",
	"context_misuse",
	"defer_in_loop",
	"error_swallow",
	"slice_append_race",
	"sql_injection",
}
