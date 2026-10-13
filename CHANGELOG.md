# Changelog

All notable user-visible changes to caliper. Format roughly follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## Unreleased

### Changed

- **Caliper is MIT-licensed, with a `LICENSE` file at the root.** The
  contributor agreement already said so; the repository did not.
- Install docs no longer assume a private repository: no `GOPRIVATE`,
  no authenticated release downloads, no private Homebrew tap.
- CI runs on GitHub-hosted Ubuntu and macOS runners with stock Go
  1.27.2, replacing the self-hosted runner and its go-plus check. Stock
  Go and go-plus give the same result locally: 379 tests run, 379 pass,
  none skipped.

## v1.6.1 — 2026-10-08

### Fixed

- **The call graph no longer panics on a method with type parameters**
  (#40). Go 1.27 allows methods with type parameters of their own, and
  the standard library has one, `math/rand/v2`'s `(*Rand).N`. Any
  program that reached such a method made `BuildInterProceduralContext`
  panic with the type parameter's name (`panic: Int`) inside x/tools'
  `Program.RuntimeTypes`. caliper now requires x/tools v0.51.0, which
  skips those methods there; calls to them are edges in the graph.

- **The export-data load works on go-plus 1.27.2.** With x/tools
  v0.44.0, `DepsFromExportData` on a module importing the standard
  library ended the process with `internal error: package "fmt" without
  types was imported from ...`. The x/tools v0.51.0 bump above fixes it.

- **A panic during call graph construction is returned as an error.**
  `BuildInterProceduralContext` used to let it end the process; the
  next input x/tools cannot handle now fails the build of the context
  instead.

## v1.6.0 — 2026-10-05

### Added

- **`Engine.Failed()` returns the packages the load dropped** (#38).
  `Packages()` leaves out every package with errors, and until now
  nothing recorded that they existed, so a caller describing the whole
  module could not tell a package that failed from one that does not
  exist. `Failed()` returns them with their `Errors` and whatever
  go/packages kept: the syntax that parsed, and for a partly
  type-checked package, the `Types` scope and `TypesInfo` of the
  declarations that resolved. A directory whose files a build
  constraint all excludes is still absent, because `./...` does not
  match it.

- **`InterProceduralContext.Skipped` lists the loaded packages left out of
  the SSA program**, each with the `Cause`: the package whose error outside
  a function body put it out of reach. Such a package is in `Packages()`
  and type-checked cleanly, but imports, possibly transitively, a package
  with a declaration or syntax error, so it has no functions and no
  call-graph edges. Before, that was silent.

## v1.5.0 — 2026-09-11

### Fixed

- **A load refuses a newer toolchain instead of answering wrongly** (#35). go/packages asks for complete types for
  every direct import of a package it type-checks from source, and fills
  them from export data; when that read fails it calls `log.Fatalf` from
  inside its importer, which no caller can recover — an MCP server dies
  with it. A go command from a newer release than the Go this program was
  built with is exactly such a failure. The load now stops before
  starting, with an error naming both versions and what to do about it.
  Measured on steward with a caliper built by go1.26.7 against go1.27.1's
  `go list`: before, the process exited 1 with `internal error: package
  "math/rand/v2" without types was imported from
  "google.golang.org/grpc/internal/transport"`; now it returns the
  refusal. The default load is refused too, for a different reason: it
  completes under skew, but the older type checker cannot process the
  newer toolchain's sources, so the same run left 15 of steward's 117
  packages in the SSA program and reported no error — a call graph
  missing 87% of the module. Unskewed behaviour is unchanged, 117 of 117
  packages before and after, in both modes. Export data unreadable for
  another reason, a truncated build cache say, can still reach the same
  fatal; only the predictable cause is guarded.

- **`context_misuse` no longer flags a detached context in a deferred
  cleanup** (#10). A deferred `Rollback`, `Close` or `Unlock` runs when
  the function returns, including because `ctx` was cancelled or timed
  out, so passing `ctx` there skips the cleanup exactly when it is needed
  most — pgx's `Rollback` returns at once on a done context and leaves
  the connection in a transaction. `context.Background()` inside a
  `defer`, whether deferred directly, inside a deferred closure, or as
  the parent of a `WithTimeout` there, is the recommended idiom and is
  not reported. `context.TODO()` in a `defer` still is. Measured on the
  repositories the issue was filed from: gameflow goes from 19 findings
  to 1, and all 9 removed sites are deferred cleanups (four
  `tx.Rollback` in the trader store, a migration `Rollback`, a
  `conn.Close`, an advisory-unlock `Exec`, two deferred metric
  decrements); govm's 5 and voya's 0 are unchanged.

- **A deferred call is reported once, not twice.** The CFG lists a defer
  statement in the block that registers it and again in a `BlockDefer`
  block where it runs, which is how a path query sees a deferred
  `Unlock` or `Close` on every exit; rules that scan nodes to report on
  them saw both copies. On govm, `unchecked_error` with `include_defers`
  gave 162 findings of which 8 were exact duplicates; on gameflow,
  `context_misuse` gave 19 of which 9 were. Both are now 0, with the
  same unique findings (154 and 1 respectively). Affects
  `context_misuse`, `unchecked_error`, `goroutine_leak` and
  `sql_injection`; `lock_violation` is unchanged, since its loop feeds
  path queries that need both copies.

- **A dependency with a type error inside a function body no longer
  drops its importers from the SSA program** (#30). The program was
  built with `ssautil.AllPackages`, which skips every package go/packages
  marks `IllTyped` — and `IllTyped` propagates, so one dependency with
  any error removed every loaded package importing it, transitively,
  from the call graph, silently: the loaded package's own type-check was
  clean and it stayed in `Engine.Packages()`. On steward, grpc's
  `internal/transport` fails on go-plus 1.27.1 with `undefined:
  http2.TrailerPrefix` inside a function body, and 36 of 117 packages
  had no SSA package and no call-graph nodes. Now a package is skipped
  only for an error outside a function body — a parse error, a type
  error in a declaration, export data that could not be read — or for
  importing such a package; one whose errors are all inside function
  bodies is created from its types alone, so its declarations are there
  for its importers and its unfinished bodies never reach the SSA
  builder. steward: 117 of 117 packages in the SSA program under the
  default load, with the same 13,119 within-module edges as under
  `DepsFromExportData`, edge for edge. Pinned on the fixture by a
  dependency with a body-only error (importer analysed, calls its clean
  function; the dependency's functions have no bodies) and one with a
  declaration error (importer dropped, as before), under both loads.
  An error is placed by its source position, so `//line` directives in
  generated code cannot move a declaration error into a body; and the
  notice go/packages adds when caliper was built with an older Go than
  the `go` command it runs does not count as an error.
  Default-load timing on steward is unchanged (load 1.6–1.7 s, call
  graph 3.8–4.0 s over the now-complete 117 packages; one run of three
  hit 11.4 s on a busy machine).

### Added

- **`EngineOptions.DepsFromExportData`** type-checks the loaded packages
  from source and takes every dependency's types from the compiler's
  export data (`go list -export`) instead of parsing and type-checking
  its source. It drops `packages.NeedDeps` from the load; under it the
  SSA program is built with the loaded packages from syntax and every
  dependency from types alone (#28, for trendvidia/alidade). Default
  `false`, everything unchanged.

  Measured on steward (117 packages, 1,293 reached with dependencies,
  warm build cache, three interleaved runs each, median and range):

  | | Load | SSA + CHA call graph |
  |---|---|---|
  | default | 1.93 s (1.4–2.5) | 3.46 s (2.5–5.1) |
  | `DepsFromExportData` | 0.99 s (0.98–1.3) | 0.36 s (0.29–0.50) |

  The first split load on a tree whose dependencies are not yet in the
  build cache pays their compilation once (3.4 s on steward, where they
  were).

  **Limitation:** no interprocedural analysis through dependency code. A
  dependency's SSA functions have no bodies, so no call-graph edge leaves
  one (steward: 8,150,734 such edges by default, 0 under the option) and
  no taint flows through one. Calls from a loaded package into a
  dependency are still edges to that function; dynamic calls resolve
  among fewer candidates, since a package reached only through a
  dependency carries just the declarations its importers reference
  (steward: 176,011 edges into dependencies by default, 22,519 under the
  option). A dependency that does not compile on the host's toolchain has
  no export data and is type-checked from source with its bodies skipped;
  it is still bodiless in the SSA program, and the loaded packages that
  import it are analysed. Pinned by `TestDepsFromExportData_*` on a
  fixture with a third-party dependency (`github.com/google/uuid`) and a
  local one that type-checks but does not compile: the call-graph edges
  among the loaded packages are identical under both modes, edge for
  edge.

  On steward the within-module graph is not identical but a strict
  superset: 13,119 edges over all 117 packages against 6,366 over 81,
  because grpc's `internal/transport` has a type error inside a function
  body on this toolchain (`undefined: http2.TrailerPrefix`), the default
  load sees it and drops the 36 packages importing it from the SSA
  program, and the split load never type-checks that body. The 6,366 are
  all present under the option and every extra edge touches one of the
  36. The default load's drop is #30.

## v1.4.0 — 2026-09-11

### Added

- **`EngineOptions.Env`** appends `KEY=VALUE` entries to the load's
  environment, after the process environment and the engine's own
  settings. Its purpose is a build configuration the host is not:
  `GOOS=linux`, `GOARCH=amd64` load the files behind `//go:build`
  constraints a tag cannot satisfy, since GOOS and GOARCH are not tags
  (#25, for trendvidia/alidade#43). Empty by default.

## v1.3.0 — 2026-08-29

### Added

- **`EngineOptions.Tests`** includes each package's test files in the load
  (default `false`, the go/packages default). Test files are roughly half
  the Go source in some repositories and are where intent is documented, so
  a consumer building a picture of a codebase usually wants them while a
  linter reporting on shipped code does not. Measured: including tests takes
  protolsp from 1,560 to 4,057 workspace functions in the call graph, and
  protoregistry from 743 to 1,269.
- **`Engine.GeneratedFiles` / `Engine.IsGeneratedFile` /
  `InterProceduralContext.IsGeneratedFunc`** identify code produced by a
  generator. `EngineOptions.SkipGenerated` only filters AST traversal
  (`Walk`, `FuncNodes`) — the SSA program and call graph are built from every
  loaded package and are unaffected by it. That asymmetry was undocumented
  and is a trap for anything walking the call graph: generated functions are
  **35% of protolsp's workspace functions and 45% of protoregistry's** even
  with `SkipGenerated: true`. The behaviour is unchanged and now documented,
  pinned by a test, and filterable by consumers that do not want it.

### Fixed

- **Call-graph tools no longer crash the server.** Every interprocedural
  tool — `find_call_sites`, `find_taint_flow`, and anything else reaching
  `Engine.InterProcedural()` — panicked on any real workspace, exiting the
  MCP server with code 2 rather than returning an error. The
  `packages.Config` load mode set `NeedDeps` but not `NeedImports`;
  `NeedDeps` only propagates the mode into packages already in `Imports`,
  and `Imports` itself is populated by `NeedImports`. Without it every
  `pkg.Imports` was empty, so `ssautil.AllPackages` never created the
  dependency packages and `prog.Build()` panicked on the first unsatisfied
  import — which is any package importing so much as `context`. Measured
  before the fix: 100% of loaded packages had empty `.Imports` across four
  repositories. The suite stayed green because every interprocedural test
  fixture was import-free; one now imports `context` and `fmt` and asserts
  `Imports` is populated. (issue #11)
- **A program with uncreated dependencies is refused, not built.**
  `prog.Build()` spawns a goroutine per package, so its panics cannot be
  recovered by the caller and take the process down. `BuildInterProceduralContext`
  now validates that precondition up front and returns an error naming the
  missing import. (issue #11)

- **`find_call_sites` returns results.** It reported `total: 0` for every
  input on every repository. `FindCallSitesByPattern` matched the pattern
  against function *declarations* (`engine.FuncNodes()` yields
  `*ast.FuncDecl`) while the pattern compiler only accepts *expressions* —
  declaration syntax is rejected as "not a valid expression" — so the match
  loop was unreachable for every valid pattern. It now matches call
  expressions across the corpus, as the tool's own documented example
  (`$x.Close()`) always implied, then enriches the result with call sites
  the call graph knows about for each resolved callee. Measured after the
  fix: 138 hits for `$x.Close()` on protolsp, 229 for `$x.String()` on
  protocompile, 384 for `$x.Close()` on govm — all previously 0. (issue #18)

## v1.2.0 — 2026-08-04

### Added

- **Cross-method lifecycle rules** (issue #8). The bellui pre-v1 audits
  showed the properties worth checking in a GUI toolkit are cross-method,
  while all analyses were function-local. Three new capabilities close
  that gap:
  - **`check_paired_calls`** — type-scoped pairing rule: a match of
    `first_pattern` in any method of a type requires a match of
    `second_pattern` somewhere in the type's method set, or in a
    companion type its methods construct (the widget→renderer link).
    Pairs by field identity when both patterns share a metavariable
    bound to a receiver field, lenient otherwise (bellui #167/#120
    class).
  - **`find_stale_capture`** — capture-at-create vs re-read-at-refresh:
    exported fields copied in a capture method (`CreateRenderer`) and
    never re-read in the re-read method (`Refresh`) of the type or its
    constructed companions (bellui #203 class).
  - **`find_pattern` gains `inside` / `not_inside`** enclosing-context
    filters: `"go"`, `"defer"`, `"callback_of:<pattern>"`,
    `"call:<pattern>"`. Expresses goroutine-context call discipline
    like "widget mutation inside `go`/`time.AfterFunc` contexts must be
    wrapped in `ui.Do(...)`" in one call.

  Both new tools are whole-program analyses (`templates.RunWholeProgram`)
  and honor `//caliper:ignore` suppressions and `.caliper.pxf` severity
  overrides like every other check.

### Fixed

- **`go install`-ed binaries report their version** instead of `dev`.
  `resolveVersion` only consulted `vcs.revision`, but `go install
  pkg@version` builds from the module cache and records no `vcs.*`
  settings at all — it stamps `Main.Version` instead. Now checked as a
  fallback, after the VCS path so local builds keep reporting
  `dev-<rev>[-dirty]` as documented. Affects both binaries.
- **Download instructions that actually work.** `docs/getting-started.md`
  told users to `curl` a `github.com/.../releases/download/...` URL.
  That returns 404 for `trendvidia/caliper` even with a valid token,
  because those URLs authenticate by session cookie rather than by
  header — private-repo assets must come from the REST asset endpoint.
  Documented via `gh release download`, with a raw-`curl` equivalent and
  a `checksums.txt` verification step.

## v1.1.1 — 2026-07-29

### Fixed

- **`go install` works again.** v1.1.0's `go.mod` still carried two
  `replace` directives, and Go refuses to install any module whose
  `go.mod` contains them:

  ```
  The go.mod file for the module providing named packages contains one
  or more replace directives.
  ```

  This broke the exact command documented in the README, for both
  `@latest` and `@v1.1.0`. Homebrew was unaffected, since it builds from
  the release tarball where the replaces resolved against `vendor/`.
  Introduced between v1.0.21 (which had no replaces and installed fine)
  and v1.1.0.

### Added

- **Prebuilt release binaries.** `docs/getting-started.md` has always
  documented downloadable binaries for six platforms, but no release
  ever carried a single asset and the documented `curl` URL returned
  404. Releases now actually ship them, for macOS / Linux / Windows ×
  amd64 / arm64, built `CGO_ENABLED=0` and stamped with
  `-X main.Version`, plus a `checksums.txt`.
- `caliper-analyze` is now documented in the README and getting-started
  install sections; it shipped in v1.1.0 but only `caliper-mcp` was
  mentioned.

### Changed

- The README's `GOPRIVATE` note claimed it was needed because the
  `protowire` dependency is private. protowire-go is public now; the
  requirement remains only because `trendvidia/caliper` itself is a
  private module.

### Build

- **`vendor/` removed** — 505 tracked files, 6.2 MB. It was introduced
  in `18642a4` for one reason: `trendvidia/protowire-go` was a private
  repo and Homebrew's sandboxed build could not fetch it. protowire-go
  and protobuf-go are both public now, so that reason is gone.
  Dependency changes become reviewable again — the mcp-go upgrade
  touched 161 vendored files around a 5-file source change.

  Consumers building from a **tagged release ≤ v1.1.0 are unaffected**:
  those tarballs still contain `vendor/`, and Go selects vendor mode
  automatically when `vendor/modules.txt` is present. Builds from
  `main` now resolve from the module proxy instead.

  The Homebrew formula's explicit `GOFLAGS=-mod=vendor` must be dropped
  for `--head` builds to keep working; without it Go picks the right
  mode for either tree.

- **No `replace` directives left in `go.mod`.** The two remaining
  local-path replaces are gone; caliper now builds from published
  versions alone.
  - `github.com/trendvidia/protowire-go` pinned to **v1.3.2** (was a
    `v0.0.0-00010101000000-000000000000` placeholder resolved through
    `=> ../protowire-go`, so the version in `go.mod` said nothing about
    what was being compiled).
  - `google.golang.org/protobuf` now resolves to stock upstream
    **v1.36.11** — already the required version and the newest upstream
    release; only the `=> ../protobuf-go` fork redirect was removed.
- **Consequence — the PXF decode fast path is off in non-workspace
  builds.** protowire-go probes for `SetUnsafe`/`AppendUnsafe` (a
  `trendvidia/protobuf-go` addition) via a runtime type assertion and
  falls back to `protoreflect.Message.Set` when absent, so upstream
  protobuf compiles fine and simply takes the standard path. caliper
  only decodes `.caliper.pxf` config files, where the difference is
  not measurable. The previous go.mod comment claiming the fork was
  needed to keep the vendored copy *compilable* was incorrect.
- Building from a sibling checkout is no longer required, and no
  credentials are needed either: `trendvidia/protowire-go` and
  `trendvidia/protobuf-go` are public, so both resolve anonymously from
  the module proxy. `GOPRIVATE` is not required for any caliper
  dependency.

## v1.1.0 — 2026-07-29

### Added

- **`caliper-analyze`** — a single-shot CLI for non-LLM consumers. The
  MCP server's session model is protocol overhead when the consumer
  just wants one analysis and a report; `caliper-analyze` spawns,
  analyzes, writes SARIF to stdout, and exits. Aimed at CI gates,
  reviewdog, and GitHub Code Scanning uploads.

  ```
  caliper-analyze -workspace . -format sarif > results.sarif
  ```

  Flags: `-workspace`, `-rules` (comma-separated rule kinds; defaults
  to the PR-default set), `-format` (`sarif` or `json`), `-version`.

### Analyzer behavior

- **`find_unchecked_error`**: large noise-reduction pass against the Go
  standard library — 1201 → ~800 hits (33% drop). Driven by:
  - Built-in stdlib infallibility allowlist (bytes.Buffer, strings.Builder,
    hash.Hash family including Hash32/Hash64 and concrete digest types
    via structural conformance, io.Pipe{Reader,Writer}.CloseWithError,
    math/rand reads, fmt.Print/Printf/Println, fmt.Fprint*/io.WriteString
    /go/printer.Fprint when the writer is `*bytes.Buffer`/`*strings.Builder`
    /`os.Stdout`/`os.Stderr`).
  - Project-local `.caliper.pxf` config: a new `infallible {}` block
    accepts `methods`, `functions`, `writers`, plus `disable_*`
    counterparts. Entries support `*` glob patterns (other shell-glob
    metacharacters are rejected with a stderr diagnostic).
  - CFG-based cleanup-path detector: Close-family calls inside an
    `if err != nil { ... }` body, sandwiched between an err-assign and
    err-check, or inside a function whose name matches cleanup keywords
    (close/clean/dispose/finalize/teardown/shutdown/destroy/release)
    are suppressed.
  - `fmt.State` recognizer: writes to the `fmt.State` parameter inside
    a `Format(s fmt.State, c rune)` method are never flagged, since fmt
    always supplies a buffer-backed implementation. ~50+ stdlib call
    sites — the dominant residual noise after the writer-arg
    suppressions above.
  - `bracketedByErrCheck` walks outer enclosing blocks, so err-check
    bracketing is detected across nesting.
- **`find_error_swallow`**: re-implemented on top of the
  `find_unchecked_error` engine, inheriting its infallibility
  allowlist, CFG cleanup-path detector and writer-arg transitivity
  while keeping its own value-use severity escalation (a dropped error
  whose sibling value is used downstream escalates to `error`). The MCP
  tool name and input schema are unchanged. **Behavior change**: the
  unified check is stricter — bare `f.Close()` discards are now flagged
  by default, where the old implementation required an explicit opt-in.
- **`find_race_condition`**: recognizes `x++`/`x--` as writes.
  Goroutines that captured and incremented a counter previously
  produced no finding at all.
- **`find_race_condition`**: honors Go 1.22+ per-iteration loop scoping
  via `packages.NeedModule`/`Module.GoVersion`. A goroutine capturing
  the loop variable in a Go ≥ 1.22 module is no longer flagged unless
  the closure itself writes the variable.
- **`find_slice_append_race`**: recognizes mutex-protected appends
  (Lock/Unlock bracketing in the same block, or Lock + deferred
  Unlock anywhere in the closure body).
- **`find_lock_violation`**: no longer flags missing Unlock when the
  method value is returned, passed as an argument, or assigned (the
  unlock obligation has been transferred to the caller).

### Configuration

- New `infallible {}` block in `.caliper.pxf`. See
  `docs/getting-started.md` for the schema.
- Per-analysis-path config resolution: each tool call walks up from
  the analysis path looking for `.caliper.pxf`, falling back to the
  server-boot config. Was previously a single boot-time load.
- Every `find_*`/`check_*` tool description now mentions `.caliper.pxf`,
  so MCP clients surface the config path without consulting the docs.

### Performance

- **Persistent result cache** at `$TMPDIR/caliper-cache/`. Read-only
  tool calls cache their results keyed on a hash of (tool name,
  analysis path, arguments, source-file fingerprint). Repeat
  invocations on unchanged source skip the cold `packages.Load`.
  See `docs/getting-started.md` for the cache location and how to
  clear it.
- **Engine cache lock fix**: a fan-out of N parallel tool calls on a
  cold path no longer triggers N concurrent `packages.Load`
  invocations. Per-path mutex map serializes first-load AND
  subsequent analysis on the same engine.

### Transport

- **`mcp-go` back on upstream** at `v0.57.0`, un-patched and un-forked.
  The interim `replace github.com/mark3labs/mcp-go => ../mcp-go` is
  gone: the fork's only patch (panic recovery in the stdio tool-call
  worker) landed upstream in
  [mark3labs/mcp-go#882](https://github.com/mark3labs/mcp-go/pull/882),
  released in v0.54.0, and upstream's version replies `INTERNAL_ERROR`
  to the offending call rather than dropping the message. The `replace`
  also leaked into every `trendvidia/go.work` member, so sibling repos
  were silently compiling ~v0.49 code under a v0.57.0 label; they now
  get the version they ask for.
- **stderr tee** to `$TMPDIR/caliper-mcp-YYYY-MM-DD.log` with line
  timestamps — useful for diagnosing MCP disconnects across
  reconnects.

### Build & release

- **GitHub Actions CI**: gofmt + vet + build + `test -race` +
  golangci-lint on push/PR, on a self-hosted macOS runner pinned to the
  go-plus toolchain.
- Migrated from `github.com/trendvidia/protowire` to `protowire-go`.
- **`-ldflags "-X main.Version=..."`** support: server reports the
  passed version in the MCP initialize handshake. Falls back to
  `dev-<vcs.revision>[-dirty]` from `runtime/debug.BuildInfo` when
  unset.

### Internal

- Engine-scoped infallibility state (was package-level globals).
  Concurrent analyses on different paths can hold distinct allowlists
  without racing.
- `inErrorCleanupBlock` AST helper replaced with CFG-based
  `dominatedByErrTrueBranch`.
- Minor dead-code cleanup: `templates/leaks.go: buildDeferReleasePredicate`,
  `templates/race.go: statementsAfter, isImmutableValueType`,
  `mcp/tools.go: allTools()`.
- `pkg/mcp/server.go` decomposed into focused files; `findInnermostBlock`
  moved to a shared `astutil.go`; `lookupIdent`'s name→object map
  memoized per `TemplateContext`; test engine fixtures shared by
  `(src, goVersion)` hash.

## v1.0.21 and earlier

See the [commit history](https://github.com/trendvidia/caliper/commits/main)
and [releases](https://github.com/trendvidia/caliper/releases).
