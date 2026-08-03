# Caliper

Semantic code analysis and transformation tool for Go, inspired by [Coccinelle](https://coccinelle.lip6.fr/). Caliper runs as an [MCP](https://modelcontextprotocol.io/) server, exposing AST-level pattern matching, control flow analysis, and code transformation to LLM-powered tools.

## Install

### Homebrew (macOS)

```bash
export HOMEBREW_GITHUB_API_TOKEN=$(gh auth token)
brew install trendvidia/tap/caliper
```

### From source

Caliper ships two binaries — the MCP server, and a single-shot CLI for
CI use:

```bash
# MCP server, for LLM-driven interactive use
GOPRIVATE=github.com/trendvidia/* go install github.com/trendvidia/caliper/cmd/caliper-mcp@latest

# Single-shot analyzer, emits SARIF or JSON and exits
GOPRIVATE=github.com/trendvidia/* go install github.com/trendvidia/caliper/cmd/caliper-analyze@latest
```

Note: `GOPRIVATE` is required because `trendvidia/caliper` is itself a
private module. Its dependencies are all public.

## Claude Code MCP Configuration

Register Caliper as an MCP server using the `claude mcp add` command:

```bash
# Available across all your projects
claude mcp add --scope user caliper -- /opt/homebrew/bin/caliper

# Or for a single project only
claude mcp add caliper -- /opt/homebrew/bin/caliper
```

If you installed via `go install`, use the full path to the binary:

```bash
claude mcp add --scope user caliper -- $(go env GOPATH)/bin/caliper-mcp
```

After adding, restart Claude Code for the MCP server to be loaded.

For team-wide configuration, add a `.mcp.json` file to your project root:

```json
{
  "mcpServers": {
    "caliper": {
      "command": "caliper",
      "args": []
    }
  }
}
```

## Tools

Caliper exposes 26 MCP tools across seven categories.

### Bug Detection

| Tool | Description |
|---|---|
| `find_nil_deref` | Nil pointer dereferences via guard tracking (error/ok/nil checks) |
| `find_resource_leak` | Missing cleanup on all code paths (files, connections, locks) |
| `find_unchecked_error` | Errors assigned but never checked before overwrite |
| `find_error_swallow` | Explicitly discarded errors (`_ = f()`, `f, _ := os.Open()`) with severity tiers |
| `find_lock_violation` | Mutex discipline: missing unlock, double lock, RLock/RUnlock pairs |
| `find_defer_in_loop` | Defer inside for/range loops (accumulates until function returns) |
| `find_context_misuse` | `context.Background()` when parent context available, `context.TODO()` in production |

### Concurrency Analysis

| Tool | Description |
|---|---|
| `find_race_condition` | Data races from goroutine closure captures, with sync.WaitGroup/Once/channel/mutex awareness |
| `find_goroutine_leak` | Goroutines with infinite loops but no termination mechanism |
| `find_slice_append_race` | Concurrent `append` to shared slices (silent data corruption) |

### Security

| Tool | Description |
|---|---|
| `find_sql_injection` | String concatenation into SQL queries |
| `find_taint_flow` | Trace untrusted data (HTTP requests) to sensitive sinks (SQL, exec) across function boundaries |

### Pattern Matching & Exploration

| Tool | Description |
|---|---|
| `find_pattern` | Structural AST pattern matching with metavariables (`$x.Close()`, `if $err != nil { ... }`), plus `inside`/`not_inside` enclosing-context filters (`go`, `defer`, `callback_of:<pattern>`, `call:<pattern>`) |
| `find_type_usage` | All usages of a type: instantiation, argument, return, field, type assertion |
| `find_call_sites` | All callers of a function via call graph analysis (CHA/RTA/VTA) |
| `check_path_property` | Generic CFG path properties: always-before, always-after, never-between, dominates |
| `check_interface` | Verify a type satisfies an interface, report missing methods and signature mismatches |
| `list_equivalences` | Show semantic equivalences used in pattern matching |

### Cross-Method Lifecycle Rules

| Tool | Description |
|---|---|
| `check_paired_calls` | Type-scoped pairing: a `$x.Start()` in any method of a type requires a `$x.Stop()` somewhere in its method set (or a companion type it constructs, e.g. widget→renderer) |
| `find_stale_capture` | Fields copied in a create-style method (`CreateRenderer`) and never re-read in a refresh-style method (`Refresh`) — stale-snapshot bugs |

### Code Transformation

| Tool | Description |
|---|---|
| `apply_transform` | AST-level rewrite: match a pattern, replace with another, preserving metavariable bindings |
| `migrate_api` | Multi-rule API migrations with import path updates |
| `add_missing_cleanup` | Auto-insert missing `defer` statements for acquired resources |

### Pattern Language & Debugging

| Tool | Description |
|---|---|
| `run_patterns` | Load and execute `.gcl` rule files against a codebase |
| `explain_match` | Debug why a pattern did/didn't match at a specific location |
| `explain_finding` | Re-run analysis at a location, show control-flow trace with source annotations |

## Output Formats

All analysis tools support a `format` parameter:

| Format | Use Case |
|---|---|
| `compact` | Default. Short keys, relative paths, minimal tokens for LLM consumption |
| `summary` | Severity counts only — triage and overview |
| `grouped` | Findings grouped by file — human readability |
| `sarif` | SARIF v2.1.0 — GitHub Code Scanning and CI integration |

Additional output parameters:

- `max_findings` — limit output to N findings
- `min_severity` — filter by `info`, `warning`, or `error`

Consecutive identical findings in the same file are automatically grouped with a count and line range (e.g., `"contain.go:106-120:3", count: 15`).

## Inline Suppression

Suppress specific findings with comments:

```go
_, _ = os.Open("file.txt") //caliper:ignore error_swallow

f.Close() //caliper:ignore  // suppresses all rules on this line
```

## LLM-Effective Usage

Caliper's MCP tools are designed so that an LLM never writes formal logic — each tool encapsulates the formal reasoning (CTL model checking, path quantifiers, CFG traversal) behind an intent-based interface.

### What works well via LLM

| Capability | How the LLM uses it |
|---|---|
| Pattern finding | "find all Close() calls" → `find_pattern` with `$x.Close()` |
| Type exploration | "where is `*sql.DB` used?" → `find_type_usage` |
| Interface check | "does MyWidget satisfy fyne.Widget?" → `check_interface` |
| Refactoring | "rename OldFunc to NewFunc" → `apply_transform` |
| Bug detection | "any resource leaks?" → `find_resource_leak` |
| Error hygiene | "find discarded errors" → `find_error_swallow` |
| Migration | "upgrade from v1 to v2 API" → `migrate_api` with rules |
| Validation | "test this pattern" → `explain_match` + iterate |
| Triage findings | "explain this nil deref" → `explain_finding` |
| Policy enforcement | "run our team's rules" → `run_patterns` |

### What's better left to humans

- Authoring complex `.gcl` rule files with `absent`/`contains` logic
- Writing `.gsp` semantic patches with multi-rule dependencies
- Configuring CI gates and choosing severity thresholds
- Defining organizational coding policies

### LLM as Policy-to-Rules Translator

The most powerful workflow combines both: **humans write policy documents, the LLM translates them into enforceable rules.**

Given a policy document (e.g., `POLICIES.md`) and access to the codebase via MCP tools, an LLM can:

1. **Read the policy** — understand natural language requirements ("all database connections must be closed", "never log PII fields", "context.Context must be first parameter")

2. **Explore the codebase** — use `find_pattern` and `find_type_usage` to learn naming conventions, package structure, and existing patterns

3. **Generate `.gcl` rules** — translate each policy into pattern rules:
   ```
   # Generated by LLM from POLICIES.md § 3.2
   version = 1
   name = "db-safety"
   patterns = {
     open_db: { pattern = "$db, $err := sql.Open($args...)" }
     close_db: { pattern = "$db.Close()" }
   }
   rules = [{
     name = "unclosed-db-connection"
     severity = "error"
     match = "@open_db"
     absent = "@close_db"
     message = "Database connection opened but never closed"
   }]
   ```

4. **Validate the rules** — use `run_patterns` to test against the codebase, check for false positives, refine patterns

5. **Generate CI configuration** — produce `.caliper.pxf` config, GitHub Actions workflow, or other CI gate setup

This workflow means:
- **Policy authors** write natural language (in a wiki, RFC, or markdown file)
- **LLM** translates to machine-enforceable rules (one-time or on policy change)
- **CI** runs the rules on every PR (no LLM in the loop)
- **Developers** see actionable findings with clear messages tied back to policy

### Example: Full Policy Enforcement Pipeline

```
POLICIES.md (human-authored)
    ↓
LLM reads policies + explores codebase (via MCP tools)
    ↓
patterns/*.gcl (LLM-generated, human-reviewed)
    ↓
.caliper.pxf (points to pattern files)
    ↓
CI: caliper check --diff (runs on every PR)
    ↓
SARIF → GitHub Code Scanning (findings in PR review)
```

The LLM acts as a bridge between human intent and machine enforcement. It only needs to run once (or when policies change) — the resulting `.gcl` files are deterministic and run without an LLM.

## Configuration

Caliper uses PXF format (protowire) for configuration. Create `.caliper.pxf` in your project root:

```
severity = {
  resource_leak: "error"
  unchecked_error: "warning"
}
rules {
  enable = ["resource_leak", "unchecked_error"]
}
engine {
  skip_generated = true
  build_tags = ["integration"]
}
patterns = ["patterns/*.gcl"]

# Extend find_unchecked_error's allowlist with project-local APIs whose
# error returns are known not to occur in practice. Entries support `*`
# globs. See docs/getting-started.md for the full reference.
infallible {
  methods = ["github.com/myorg/mylib.Logger.*"]
  writers = ["github.com/myorg/mylib.NopWriter"]
}
```

## License

MIT
