# Changelog

All notable user-visible changes to caliper. Format roughly follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## Unreleased

### Build

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
