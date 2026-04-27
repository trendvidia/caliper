# Changelog

All notable user-visible changes to caliper. Format roughly follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) without strict
versioning until the project tags its first release.

## Unreleased

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

- Replaced vendored `mcp-go` with a local fork
  (`replace github.com/mark3labs/mcp-go => ../mcp-go`). The previous
  vendored copy had local patches that drifted from upstream; the
  fork picks up upstream's worker-pool architecture for concurrent
  tool calls and adds a panic-recovery patch in `toolCallWorker`.
- **stderr tee** to `$TMPDIR/caliper-mcp-YYYY-MM-DD.log` with line
  timestamps — useful for diagnosing MCP disconnects across
  reconnects.

### Build & release

- **GitHub Actions CI**: gofmt + vet + build + `test -race` +
  golangci-lint on push/PR.
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
