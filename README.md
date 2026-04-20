# GoCaliper

Semantic code analysis and transformation tool for Go, inspired by [Coccinelle](https://coccinelle.lip6.fr/). GoCaliper runs as an [MCP](https://modelcontextprotocol.io/) server, exposing AST-level pattern matching, control flow analysis, and code transformation to LLM-powered tools.

## Install

```bash
brew install trendvidia/tap/gocaliper --head
```

Or build from source:

```bash
go install github.com/trendvidia/gocaliper/cmd/gocaliper-mcp@latest
```

## Claude Code MCP Configuration

Add GoCaliper to your Claude Code settings (`~/.claude/settings.json`):

```json
{
  "mcpServers": {
    "gocaliper": {
      "command": "gocaliper",
      "args": []
    }
  }
}
```

Or for a project-specific setup, add to `.claude/settings.json` in your repo root.

If you installed via `go install` instead of Homebrew, use the full path:

```json
{
  "mcpServers": {
    "gocaliper": {
      "command": "/path/to/go/bin/gocaliper-mcp",
      "args": []
    }
  }
}
```

## Tools

GoCaliper exposes the following MCP tools:

**Pattern Matching**
- `find_pattern` — match code using AST patterns with metavariables
- `find_type_usage` — find all usages of a specific type
- `list_equivalences` — show semantic equivalences used in matching

**Control Flow Analysis**
- `find_resource_leak` — detect missing cleanup on code paths
- `find_unchecked_error` — find ignored/unchecked errors
- `find_lock_violation` — detect mutex discipline violations
- `find_nil_deref` — find nil pointer dereferences
- `check_path_property` — generic path property checker

**Code Transformation**
- `apply_transform` — rewrite code matching a pattern
- `migrate_api` — multi-rule API migrations with import updates
- `add_missing_cleanup` — auto-insert missing defer statements

**Advanced Analysis**
- `find_goroutine_leak` — detect leaked goroutines
- `find_sql_injection` — find SQL injection vulnerabilities
- `find_race_condition` — detect data races in concurrent code
- `find_context_misuse` — find context.Background() misuse
- `find_taint_flow` — trace untrusted data to sensitive sinks
- `find_call_sites` — find all callers of a function

**Debugging**
- `explain_match` — debug why a pattern did/didn't match

## License

MIT
