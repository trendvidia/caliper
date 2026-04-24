# Caliper — Positioning & Use Cases

## When AI Agents Rely on Caliper

**Before suggesting code changes.** When an AI assistant refactors or modifies Go code, it works from local context — it sees the file, maybe a few related files. It can miss that a variable captured in a goroutine three functions away creates a race, or that an error discarded on line 40 causes a nil deref on line 65. Caliper catches cross-cutting concerns that require whole-program reasoning.

**During code review.** AI assistants are good at style, logic, and design — but unreliable at concurrency bugs, resource leaks, and lock ordering violations. These require dataflow and control-flow analysis that's expensive to do in-context. A dedicated analyzer is more reliable and faster.

**After writing code.** AI-generated code works for the happy path, but doesn't always consider every error path or edge case. Running `find_error_swallow` and `find_nil_deref` on freshly generated code is a natural "did I miss anything" check.

**When onboarding to a codebase.** Running the full suite against a new repo gives an instant map of technical debt hotspots — where the bodies are buried.

## Market Positioning

The market already has Go linters (`staticcheck`, `golangci-lint`, `go vet`). Caliper's positioning is distinct along four axes.

### 1. The Analyzer That Speaks LLM

This is the core differentiator. Caliper isn't just a linter — it's a semantic analysis service designed to be consumed by AI agents. Traditional linters output for humans in terminals. Caliper outputs compact, structured, token-efficient JSON designed for LLM context windows.

No other tool in this space is optimized for the AI-agent workflow:

- Compact output that fits in context without truncation
- Relative paths with roots so the agent can navigate
- Short keys that minimize token waste
- No redundant information across fields

*"Your AI coding assistant is only as good as the tools it can call. Caliper gives it eyes for bugs it can't see on its own."*

### 2. Catches What Linters Can't — and What LLMs Won't

Traditional linters are syntactic. LLMs are probabilistic. Neither is great at:

- Dataflow analysis across function boundaries (nil propagation)
- Concurrency reasoning (races, goroutine leaks, lock ordering)
- Cross-file resource lifecycle tracking

Caliper fills the gap between "compiles" and "correct."

### 3. Semantic Rules as Code — Your Architecture, Enforced

Every team has rules that live in tribal knowledge:

- "Never call Refresh() inside CreateRenderer()"
- "All database queries must go through the repository layer"
- "Never use context.TODO() in production paths"
- "Auth tokens must be encrypted at rest"

Today these are enforced by code review, which means they're enforced inconsistently. Caliper turns them into automated checks that run at build time.

*"Document your architecture decisions once. Enforce them forever."*

This is fundamentally different from generic linting — it's custom static analysis without writing a custom analyzer. Writing a custom `go/analysis` pass is a multi-day effort requiring deep AST knowledge. Writing a Caliper semantic rule should be a 5-minute definition.

### 4. The Sharp Pitch

> *Caliper is the static analysis engine purpose-built for AI-assisted development — it finds the bugs your LLM introduces and enforces the rules your linter can't express.*

## Target Buyers

| Buyer | Pain Point | Value |
|-------|-----------|-------|
| **Platform/infra teams** | Can't enforce architectural boundaries at scale | Semantic rules as guardrails |
| **Security teams** | Manual audit doesn't scale | Automated taint flow, error handling, context misuse |
| **Teams adopting AI coding tools** | AI generates plausible but subtly wrong code | Caliper as the verification layer |
| **Framework authors** | Users misuse APIs in ways they can't control | Ship framework-specific rules with the library |

## Market Timing

Every team is adopting AI coding tools. Every team is discovering that AI-generated code passes tests but introduces subtle correctness issues. Caliper is the trust layer between "the AI wrote it" and "we shipped it."
