# GoCaliper Roadmap

## Phase 1 — Complete Core Engine ✓

### Pattern Matching
- [x] Implement `substituteBindings` in `transform/rewriter.go` — deep-clone replace pattern AST and substitute metavariables with bound values
- [x] Add type constraint filtering to pattern matcher (currently parsed but not enforced)
- [x] Support statement-level `...` wildcard (match zero or more statements in a block body)
- [x] Handle patterns that span multiple statements (e.g., `$x, $err := $fn($args...); if $err != nil { ... }`)

### CFG / Path Analysis
- [x] Wire `break`/`continue`/`goto` to their correct CFG targets in `cfg/builder.go`
- [x] Handle `defer` semantics properly — defer runs at function exit, model this in the CFG exit paths
- [x] Add cycle detection to prevent infinite loops in path traversal on recursive CFGs
- [x] Integrate `golang.org/x/tools/go/ssa` as an alternative CFG backend for more precise dataflow

### Isomorphisms
- [x] Implement pattern expansion in the matcher — when matching, also try all equivalent forms from `iso.Registry`
- [x] Add `errors.As` ↔ type assertion equivalence
- [x] Add `context.TODO()` ↔ `context.Background()` equivalence (configurable)
- [x] Add `sync.Mutex` ↔ `sync.RWMutex` partial equivalence (Lock/Unlock subset)

## Phase 2 — Complete MCP Tool Implementations ✓

### Stub Tools
- [x] `find_type_usage` — walk `types.Info.Uses` and `types.Info.Defs` to find all references to a type
- [x] `find_nil_deref` — trace pointer values through CFG, check for nil-guard dominance before dereference
- [x] `explain_match` — record matching steps during pattern match, return structured trace
- [x] `check_path_property` — route `from_pattern`/`target_pattern`/`property` params to the correct `cfg.*` function

### Cleanup Inserter
- [x] Implement `CleanupInserter.Preview()` — run `ResourceLeak` template, then for each finding compute the insertion point (after error check, before next statement)
- [x] Implement `CleanupInserter.Apply()` — use text-level rewriting to insert `defer` or explicit cleanup calls
- [x] Handle the common Go pattern: `f, err := os.Open(...); if err != nil { return err }; defer f.Close()`

### Import Management
- [x] Integrate `golang.org/x/tools/imports` (goimports) into `APIMigration.fixImports()`
- [x] Track old→new import mappings from migration rules and apply them

## Phase 3 — Robustness & Testing

### Test Suite
- [x] Unit tests for pattern matcher — cover all node types, metavariable binding, variadic args
- [x] Unit tests for CFG builder — verify graph structure for if/for/switch/select/defer
- [x] Unit tests for path analysis — AllPaths vs AnyPath, NeverBetween, AlwaysBefore, Dominates
- [x] Integration tests for each template (resource leaks, unchecked errors, lock violations) with real Go source fixtures
- [x] End-to-end MCP tests — send tool requests over stdio, verify JSON responses
- [x] Golden file tests for transformations — input Go file → expected output Go file

### Edge Cases
- [x] Handle generated code (`// Code generated` header) — skip by default
- [x] Handle build tags and platform-specific files
- [x] Handle cgo files gracefully (skip or partial analysis)
- [x] Large codebase performance — lazy package loading, incremental analysis
- [x] Multiple modules / workspace mode (`go.work`)

## Phase 4 — Advanced Analysis ✓

### Inter-procedural Analysis
- [x] Cross-function metavariable binding — find a function definition, then match all its call sites (Coccinelle's rule chaining across files)
- [x] Call graph construction using `golang.org/x/tools/go/callgraph` (CHA, RTA, or pointer analysis)
- [x] Taint tracking — trace a value from source (e.g., `http.Request`) to sink (e.g., `sql.Query`) across function boundaries

### Richer Path Properties
- [x] `EG` (for all paths, globally) — property holds on every state of every path, not just eventually
- [x] Nested path quantifiers — "on all paths from Lock to Unlock, there exists no path through a blocking op" (currently flat)
- [x] Loop-sensitive analysis — distinguish "first iteration" from "subsequent iterations"
- [x] Context-sensitive analysis — track `context.Context` cancellation paths

### Additional Templates
- [x] `find_goroutine_leak` — goroutine started but channel/context never closed
- [x] `find_sql_injection` — string concatenation into SQL query
- [x] `find_race_condition` — shared variable accessed from goroutine without synchronization
- [x] `find_context_misuse` — context.Background() used where a parent context is available

## Phase 5 — Developer Experience

### MCP Integration
- [ ] Structured diff output — return unified diffs that LLMs can review before applying
- [ ] Progress notifications for large codebases (MCP progress tokens)
- [ ] Caching — reuse parsed packages across tool calls within a session
- [ ] Configurable severity levels and rule sets via `.gocaliper.yaml`

### Pattern Language
- [ ] Pattern file format (`.gcl`) for reusable, shareable patterns
- [ ] Pattern composition — define named sub-patterns and reference them
- [ ] Negative patterns — `!$x.Close()` to match absence of a call
- [ ] Regex constraints on metavariables — `$x where name matches /^test.*/`

### Distribution
- [ ] Homebrew formula
- [ ] Docker image for CI usage
- [ ] Claude Code MCP config snippet (`claude_desktop_config.json` example)
- [ ] VS Code extension that runs GoCaliper as an MCP server
