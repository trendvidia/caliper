# Caliper Roadmap

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
- [x] Structured diff output — return unified diffs that LLMs can review before applying
- [x] Progress notifications for large codebases (MCP progress tokens)
- [x] Caching — reuse parsed packages across tool calls within a session
- [x] Configurable severity levels and rule sets via `.caliper.yaml`

### Pattern Language
- [x] Pattern file format (`.gcl`) for reusable, shareable patterns
- [x] Pattern composition — define named sub-patterns and reference them
- [x] Negative patterns — `!$x.Close()` to match absence of a call
- [x] Regex constraints on metavariables — `$x where name matches /^test.*/`

### Distribution
- [x] Homebrew formula
- [ ] Docker image for CI usage
- [x] Claude Code MCP config snippet (`claude_desktop_config.json` example)
- [ ] VS Code extension that runs Caliper as an MCP server

## Operational Quality

Items surfaced during dogfooding (running caliper against caliper, then
against `clair`, `appviewer`, and the trendvidia/go stdlib fork). These
are smaller than a phase but high-leverage — most affect either daily UX
or signal-to-noise of existing checks.

### Engine cache persistence across MCP reconnects ✓ (via result caching)

Claude Code's MCP transport closes stdin after each tool batch, so caliper
exits cleanly and is re-spawned on the next batch. Every batch therefore
pays the full cold `packages.Load` cost (multi-second on a stdlib-sized
project).

Implementation taken: **result caching** rather than engine persistence.
Persisting `*types.Package` + `*ast.File` across processes turned out to
be impractical (unexported state, circular references in go/types data
that don't gob/json cleanly). Result caching achieves the same UX win
for the common case — repeat tool calls on unchanged source — without
the serialization-layer-for-go/types lift.

- [x] Per-tool-call result cache at `$TMPDIR/caliper-cache/`
  (`pkg/mcp/resultcache.go`)
- [x] Cache key: `sha(toolName + absPath + argsJSON + source-fingerprint)`
- [x] Source fingerprint: hash over `(relpath, mtime, size)` for every
  `.go` file under the analysis path; skips `vendor/`, `testdata/`,
  `.git/`, `node_modules/`. ~35ms on Go stdlib.
- [x] Read-only tools wrapped with `cachedHandler`; mutation tools
  (`apply_transform`, `migrate_api`, `add_missing_cleanup`) bypass
  caching since they alter source.
- [x] Per-process fingerprint memo so a fan-out of N tool calls walks
  the source tree once.
- [ ] Cache eviction (currently unbounded). Note in docs: clear via
  `rm -rf $TMPDIR/caliper-cache`.
- [ ] Future: explore `*types.Package` rehydration via go/gcexportdata
  for cross-tool engine sharing. Bigger lift but eliminates the cold-
  load tax even when args differ.

### Consolidate `find_unchecked_error` and `find_error_swallow`

The two checks fire on substantially overlapping patterns with different
wording, so users see the same hit reported twice with different `kind`
values. Sweeps on stdlib showed >50% overlap.

- [ ] Audit which patterns each check uniquely catches
- [ ] Merge into one check with a `mode` parameter (`strict`/`lenient`),
  OR have one explicitly defer to the other (e.g. `error_swallow` skips
  any finding `unchecked_error` would also raise)
- [ ] Update tool descriptions and the Phase-1 docs to reflect the
  consolidated tool

### Per-project `.caliper.pxf` discoverability

The per-analysis-path config resolution shipped this session, but the
MCP tool descriptions (returned via the protocol) don't mention it.
LLMs and human callers have no signal that dropping a `.caliper.pxf`
near their project changes behavior.

- [ ] Add "respects `.caliper.pxf` walking up from the analysis path" to
  the descriptions of `find_unchecked_error`, `find_resource_leak`, and
  any other check that consults the config
- [ ] Mirror the same line in `docs/getting-started.md`'s "Project
  Configuration" section
- [ ] Consider an `mcp__caliper__describe_config` tool that reports which
  `.caliper.pxf` (if any) was applied for a given path — helps debug
  "why is this finding still firing"

### Targeted `fmt.State` Format-method recognizer

Largest remaining noise source on stdlib (~117 `fmt.Fprintf` hits). Most
fire inside `func (T) Format(s fmt.State, c rune)` methods writing to
`s` — the writer is `fmt.State`, an interface, but in practice always
backed by an internal buffer when called by the `fmt` package. Pattern-
match the enclosing function shape rather than do full writer-flow.

- [ ] Recognizer: when the call is inside a method matching
  `Format(_ fmt.State, _ rune)` AND the writer arg is the fmt.State
  parameter, suppress
- [ ] Regression test: a `Format` method that takes errors-mattering
  writes still fires
- [ ] Cost: ~30 lines, no flow analysis required

### Pre-existing analyzer gaps surfaced this session

Two narrow bugs found while writing tests for unrelated features:

- [ ] `find_race_condition` doesn't recognize `count++` /
  `*ast.IncDecStmt` as a write — only `*ast.AssignStmt` is checked.
  Misses races on counter-style mutations.
- [ ] `find_unchecked_error` / `bracketedByErrCheck` only sees uses
  in the same lexical block as the call — ignores uses outside the
  enclosing for/switch. Limits the "save err, do thing, check err"
  recognizer.

Both are local fixes (≤20 lines each) but defensive — small payoff
individually, easy to do as a cleanup pass.

### CHANGELOG / release notes

Caliper's behavior has shifted substantially across recent commits
(Go 1.22+ loop scoping, hash conformance, cleanup-path detection,
engine-scoped config, mcp-go fork). Users updating the binary have no
signal about what changed.

- [ ] `CHANGELOG.md` at repo root — entries grouped by user-visible
  category (analyzer behavior changes, config schema, transport,
  internal)
- [ ] Update on each release that touches user-observable behavior
- [ ] Eventually: tie to git tags so `caliper --version` can print
  the relevant changelog entry

## Phase 6 — Coccinelle Parity

The features below close the remaining gap between Caliper and what Coccinelle provides for C/Linux kernel refactoring. They transform Caliper from a pattern-finder into a full semantic patch engine for Go.

### CFG-Aware Pattern Matching (the `...` that understands control flow)

Currently `...` is syntactic (matches any statements in a block). In Coccinelle, `...` means "there exists a control-flow path between these two points." This is the single most important gap.

- [ ] `...` in patterns uses CFG reachability, not just statement list matching — `$x.Lock(); ...; $x.Unlock()` means "Lock reaches Unlock on some path," not "they appear in the same block"
- [ ] `when` clauses on `...` — path constraints that filter which paths are acceptable:
  - `... when != $x.Unlock()` — path must NOT contain Unlock (for finding missing unlocks)
  - `... when any` — match even if only some paths satisfy (existential, default is universal)
  - `... when strict` — no alternative paths allowed (every path from A must reach B)
  - `... when != return` — path must not contain a return statement
- [ ] `<... pattern ...>` — existential dots: pattern appears somewhere within the enclosing scope (any path, any depth)
- [ ] `<+... pattern ...+>` — universal dots: pattern appears on ALL paths through the scope
- [ ] Dots with binding — `...; $x = $expr; ...` where bindings from the dots-bounded region are captured and available in subsequent rules

### Semantic Patch Language (SmPL for Go)

The `.gcl` format handles find-and-report. SmPL-style semantic patches handle large-scale refactoring with `-`/`+` annotations, rule chaining, and inherited metavariables.

- [ ] `.gsp` file format (Go Semantic Patch) with `-`/`+` line annotations for expressing transformations as diffs:
  ```
  @rule1@
  identifier fn;
  expression E;
  @@
  - old_api(E)
  + new_api(context.TODO(), E)
  ```
- [ ] Multi-rule patches — rule chaining where later rules inherit metavariables from earlier rules:
  ```
  @rule1@
  identifier pkg;
  @@
    import "pkg"

  @rule2 depends on rule1@
  expression E;
  @@
  - pkg.OldFunc(E)
  + pkg.NewFunc(context.TODO(), E)
  ```
- [ ] `depends on` / `depends on !` — conditional rule execution (run rule2 only if rule1 matched / didn't match)
- [ ] Inherited metavariables — metavariables bound in rule N are available in rule N+1 without re-matching
- [ ] Star rules (`@rule*@`) — find-only rules that report matches without transforming (for auditing)
- [ ] MCP `apply_semantic_patch` tool — load and apply `.gsp` files with preview/apply modes

### Disjunction and Optional Patterns

- [ ] Disjunction patterns — `( pattern1 | pattern2 | pattern3 )` to match any of several alternatives in a single rule
- [ ] Optional match — `?pattern` matches zero or one occurrence (for optional error checks, optional context args)
- [ ] Nested disjunctions — disjunctions within `-`/`+` blocks to handle multiple refactoring variants in one rule

### Metavariable System

Caliper's metavariables are untyped (match anything). Coccinelle's are typed (identifier, expression, statement, type, constant, etc.), which prevents false positives.

- [ ] Typed metavariable declarations — `identifier $fn;`, `expression $e;`, `statement $s;`, `type $T;`, `constant $c;`
- [ ] `idexpression` — metavariable that only matches simple identifiers (not complex expressions), the most common case in Coccinelle
- [ ] Position metavariables — `position $p;` captures source location for script rules and reporting
- [ ] Fresh identifiers — `fresh identifier $new = $old ## "_v2";` generates new names in replacements (complex name computation via GoVM script rules)
- [ ] Metavariable constraints in declarations — `identifier $fn != "init";` or `expression $e : type == "*os.File";` (unify type/name constraints with metavar decl syntax)
- [ ] Inherited metavariable scoping — explicit `@@` section declaring which metavars are inherited vs fresh

### Script Rules (powered by GoVM)

Coccinelle uses Python/OCaml scripts for logic too complex for patterns. Caliper uses GoVM (`github.com/trendvidia/govm`) — a full Go interpreter with JIT, bytecode caching, and sandboxing — so script rules are written in real Go, not a foreign DSL.

- [ ] Script filter predicates using GoVM's `eval` package — compile-once/eval-many for high-throughput filtering:
  ```
  @script:go depends on rule1@
  fn << rule1.fn;
  @@
  fn.Name != "main" && !strings.HasPrefix(fn.Name, "Test")
  ```
- [ ] Full Go script rules via GoVM bytecode — for complex logic that needs loops, maps, or helper functions:
  ```
  @script:go depends on rule1@
  x << rule1.x;
  @@
  func filter() bool {
      // Full Go: loops, string ops, type inspection
      return len(x.Name) > 3 && unicode.IsUpper(rune(x.Name[0]))
  }
  ```
- [ ] Native function injection — register AST/type inspection helpers into the GoVM sandbox:
  - `typeof(node)` → returns type string of a bound metavariable
  - `implements(node, "io.Closer")` → interface satisfaction check
  - `callers(fn)` → call graph query from script context
  - `hasAnnotation(fn, "//go:generate")` → comment/directive checks
- [ ] Script-computed replacements — script returns a string or AST fragment used as the replacement text
- [ ] Script-based message formatting — custom error messages using bound metavariable values and Go's `fmt.Sprintf`
- [ ] Iteration until fixpoint — repeatedly apply a semantic patch until no more matches (for transitive refactoring)
- [ ] GoVM sandbox configuration — resource limits (timeout, max goroutines, no file I/O) enforced per-rule; only safe stdlib modules loaded (fmt, strings, math, regexp, strconv, unicode)
- [ ] Bytecode caching — compiled script rules cached at `~/.govm/cache/` via content-addressed hashing, skip recompilation on repeated runs

### Batch CLI Mode

Coccinelle's `spatch` is a CLI tool that processes entire kernel trees. Caliper needs an equivalent for CI pipelines and large-scale refactoring campaigns.

- [ ] `caliper run` CLI command — run `.gcl`/`.gsp` files against a codebase, output findings to stdout
- [ ] `caliper patch` CLI command — apply `.gsp` semantic patches, write modified files or output unified diffs
- [ ] `caliper check` CLI command — exit non-zero if any findings (for CI gates)
- [ ] SARIF output format — standard static analysis format for GitHub Code Scanning / VS Code integration
- [ ] Parallel file processing — process packages concurrently with worker pool (already have `parmap`-style infra in engine)
- [ ] `--include`/`--exclude` glob filters for targeting specific files/packages
- [ ] `--diff` mode — only analyze files changed in a git diff (PR-scoped analysis)
- [ ] Configuration via `.caliper.pxf` `patterns:` to auto-discover `.gcl`/`.gsp` files

### LLM-Driven Policy Enforcement

The most powerful workflow: humans write policy documents in natural language, the LLM translates them into enforceable `.gcl` rules, and CI runs the rules without an LLM in the loop.

- [ ] `generate_policy_rules` MCP meta-tool — given a policy description + codebase, generates `.gcl` rules:
  - Reads natural language policy (from a file, URL, or inline text)
  - Explores codebase via `find_pattern`/`find_type_usage` to learn conventions
  - Generates `.gcl` rules encoding each policy item
  - Validates rules via `run_patterns`, reports false positive rate
  - Outputs ready-to-commit `.gcl` file + suggested `.caliper.pxf` config
- [ ] Policy validation loop — iteratively refine generated rules: run against codebase, present findings to user, adjust patterns to reduce false positives
- [ ] CI config generation — produce GitHub Actions / GitLab CI snippets that run `caliper check` on PRs
- [ ] Policy drift detection — re-run policy→rules translation periodically, diff against committed rules, flag policies that have drifted from enforcement
- [ ] POLICIES.md convention — standard location and format for policy documents that the tool can discover and process
- [ ] Multi-repo policy distribution — generate rules once, publish to a shared pattern library, consume across repos via `.caliper.pxf` remote patterns

### Isomorphism Files

Coccinelle loads equivalences from `.iso` files that users can customize. Caliper's `pkg/iso` is hardcoded.

- [ ] `.iso` file format for user-defined equivalences:
  ```
  Expression
  @ eq_nil @
  expression E;
  @@
  E == nil => !(E != nil)
  ```
- [ ] Load `.iso` files from `.caliper.pxf` config or `--iso` CLI flag
- [ ] Conditional isomorphisms — only apply in certain contexts (e.g., error-handling isos only in `if err != nil` blocks)
- [ ] Isomorphism composition — apply multiple `.iso` files, with priority ordering

### Distribution (remaining)

- [ ] Docker image for CI usage
- [ ] VS Code extension that runs Caliper as an MCP server
- [ ] `caliper init` command — scaffold `.caliper.pxf` and example `.gcl` files
- [ ] Pattern library repository — community-maintained `.gcl`/`.gsp` files for common Go idioms and migrations
