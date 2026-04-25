# Getting Started with Caliper

## What Is Caliper

Caliper is a semantic code analysis tool for Go. It exposes AST-level pattern matching, control flow analysis, and code transformation through the [Model Context Protocol](https://modelcontextprotocol.io/) (MCP), making its capabilities available to AI coding assistants like Claude Code, Gemini CLI, GitHub Copilot, and OpenAI Codex.

Unlike traditional linters that operate on syntax, Caliper reasons about control flow graphs, dataflow paths, and cross-function relationships. It catches bugs that compile cleanly, pass tests, and survive code review — nil dereferences from unchecked type assertions, resource leaks from `defer` inside loops, data races from goroutine closure captures, and discarded errors that silently corrupt program state.

Caliper's output is optimized for LLM consumption: compact JSON with short keys, relative paths, and no redundant information. It is designed to be the verification layer between "the AI wrote it" and "we shipped it."

### Capabilities

**Bug detection** — nil pointer dereferences, resource leaks, discarded errors, defer-in-loop accumulation, lock violations, context misuse.

**Concurrency analysis** — data races from goroutine closure captures, goroutine leaks, concurrent slice append corruption.

**Security** — SQL injection via string concatenation, taint flow tracing from HTTP inputs to sensitive sinks.

**Pattern matching** — structural AST search with metavariables, type usage exploration, call graph analysis, interface satisfaction checks.

**Code transformation** — AST-level rewrite rules, API migration with import updates, automatic insertion of missing cleanup.

**Policy enforcement** — team-defined rules in `.gcl` files, executed at build time or in CI, with SARIF output for GitHub Code Scanning.

## Installation

### Download

Download the appropriate binary for your platform from the [releases page](https://github.com/trendvidia/caliper/releases):

| Platform | Architecture | File |
|----------|-------------|------|
| macOS | Apple Silicon (M1/M2/M3/M4) | `caliper-darwin-arm64` |
| macOS | Intel | `caliper-darwin-amd64` |
| Linux | ARM64 | `caliper-linux-arm64` |
| Linux | x86_64 | `caliper-linux-amd64` |
| Windows | ARM64 | `caliper-windows-arm64.exe` |
| Windows | x86_64 | `caliper-windows-amd64.exe` |

#### macOS / Linux

```bash
# Download (replace PLATFORM and ARCH with your values)
curl -Lo caliper https://github.com/trendvidia/caliper/releases/latest/download/caliper-PLATFORM-ARCH
chmod +x caliper
sudo mv caliper /usr/local/bin/
```

Example for Apple Silicon Mac:

```bash
curl -Lo caliper https://github.com/trendvidia/caliper/releases/latest/download/caliper-darwin-arm64
chmod +x caliper
sudo mv caliper /usr/local/bin/
```

#### Windows

Download `caliper-windows-amd64.exe` (or `arm64`) from the releases page, rename it to `caliper.exe`, and add its directory to your `PATH`.

### Homebrew (macOS)

```bash
brew install trendvidia/tap/caliper
```

### From Source

```bash
GOPRIVATE=github.com/trendvidia/* go install github.com/trendvidia/caliper/cmd/caliper-mcp@latest
```

### Verify Installation

```bash
caliper --version
```

## Connecting to AI Coding Assistants

Caliper communicates via MCP over stdio. Each AI tool has its own way of registering MCP servers.

### Claude Code

Claude Code has native MCP support via the `claude mcp add` command.

**Per-user (available in all projects):**

```bash
claude mcp add --scope user caliper -- caliper
```

**Per-project (current directory only):**

```bash
claude mcp add caliper -- caliper
```

**Team-wide (committed to repo):** Create `.mcp.json` in your project root:

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

After adding, restart Claude Code or run `/mcp` to connect.

### Gemini CLI

Gemini CLI reads MCP server configuration from `~/.gemini/settings.json`:

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

For per-project configuration, create `.gemini/settings.json` in your project root with the same format.

### GitHub Copilot (VS Code)

Copilot in VS Code supports MCP servers through the `mcp.json` configuration. Create or edit `.vscode/mcp.json` in your project:

```json
{
  "servers": {
    "caliper": {
      "command": "caliper",
      "args": []
    }
  }
}
```

Alternatively, add to your VS Code user settings (`settings.json`):

```json
{
  "mcp": {
    "servers": {
      "caliper": {
        "command": "caliper",
        "args": []
      }
    }
  }
}
```

### OpenAI Codex CLI

Codex CLI reads MCP configuration from `~/.codex/config.json`:

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

### Cursor

Cursor supports MCP servers via its settings. Create `.cursor/mcp.json` in your project root:

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

Or configure globally through Cursor Settings > MCP Servers > Add Server.

### Windsurf

Windsurf reads MCP configuration from `~/.codeium/windsurf/mcp_config.json`:

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

### Zed

Zed supports MCP via its settings file. Add to `~/.config/zed/settings.json`:

```json
{
  "context_servers": {
    "caliper": {
      "command": {
        "path": "caliper",
        "args": []
      }
    }
  }
}
```

### Other MCP-Compatible Tools

Any tool that supports the [Model Context Protocol](https://modelcontextprotocol.io/) can use Caliper. The server communicates over stdio with JSON-RPC. The general pattern is:

1. Point the tool at the `caliper` binary
2. Set the transport to `stdio`
3. No arguments required — Caliper auto-detects the Go module in the working directory

If your tool requires a full path, use `which caliper` (macOS/Linux) or `where caliper` (Windows) to find it.

## First Run

Once connected, ask your AI assistant to analyze your Go project:

> "Run Caliper's error swallow analyzer on this project."

> "Check this codebase for nil pointer dereferences."

> "Are there any defer statements inside loops?"

> "Find potential race conditions in the server package."

The AI assistant will call the appropriate Caliper tool and report findings with file locations, severity levels, and fix suggestions.

## Example: Analyzing a Package

Here is what a typical interaction looks like when an AI assistant uses Caliper:

```
You: Are there any discarded errors in the auth package?
Assistant: [calls find_error_swallow on auth/]

Caliper found 3 discarded errors in the auth package:

  auth/github.go:104 (error) - error from io.ReadAll() explicitly discarded
  auth/oidc.go:198   (error) - error from io.ReadAll() explicitly discarded
  auth/storage.go:125 (error) - error from os.Hostname() explicitly discarded

The io.ReadAll() errors at github.go:104 and oidc.go:198 are the most
concerning -- if the read fails, the subsequent JSON unmarshal will get
nil input and produce a confusing error. Consider handling the error:

  body, err := io.ReadAll(resp.Body)
  if err != nil {
      return nil, fmt.Errorf("read response: %w", err)
  }

The os.Hostname() at storage.go:125 is used for a best-effort machine
seed, so an empty string on failure is acceptable. You could suppress
the finding with:

  host, _ := os.Hostname() //caliper:ignore error_swallow
```

## Output Formats

Caliper supports multiple output formats via the `format` parameter:

| Format | Use Case |
|--------|----------|
| `compact` | Default. Minimal tokens for LLM context windows |
| `summary` | Severity counts only -- quick triage |
| `grouped` | Findings grouped by file -- human readability |
| `sarif` | SARIF v2.1.0 -- GitHub Code Scanning and CI integration |

Additional parameters:

- `max_findings` -- limit output to N findings
- `min_severity` -- filter by `info`, `warning`, or `error`

## Suppressing Findings

Suppress specific findings with inline comments:

```go
_, _ = os.Open("file.txt") //caliper:ignore error_swallow

f.Close() //caliper:ignore  // suppresses all rules on this line
```

## Next Steps

- See the full [tool reference](../README.md) for all 24 MCP tools
- Read about [finding defer-in-loop bugs in Go's standard library](defer-in-loop-go-stdlib.md)
- Read the [positioning document](../POSITIONING.md) to understand Caliper's place in the ecosystem
