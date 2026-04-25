# Finding Hidden Resource Leaks in Go's Standard Library with Caliper

Go's `defer` statement is one of the language's most elegant features. It guarantees cleanup runs when a function returns, making resource management straightforward and hard to forget. But there's a subtle trap: when `defer` appears inside a loop, the cleanup doesn't run at the end of each iteration — it accumulates until the enclosing function returns. This means resources pile up silently, and in long-running loops, the consequences range from memory pressure to file descriptor exhaustion.

Using [Caliper](https://github.com/trendvidia/caliper), a semantic code analysis tool designed for AI-assisted development, we found five instances of this bug in Go's own standard library — in the HTTP transport, the network dialer, and the DNS resolver.

## What Caliper Found

### 1. Happy Eyeballs Dialer — Context Cancellation Leak

Go's `net.Dialer` implements [RFC 8305](https://datatracker.ietf.org/doc/html/rfc8305) (Happy Eyeballs v2), which races IPv4 and IPv6 connections to reduce latency. The fallback logic creates a new context on each fallback attempt:

```go
// net/dial.go, line 697
for {
    select {
    case <-fallbackTimer.C:
        fallbackCtx, fallbackCancel := context.WithCancel(ctx)
        defer fallbackCancel()  // BUG: accumulates each iteration
        go startRacer(fallbackCtx, false)

    case res := <-results:
        // ...
    }
}
```

Every time the fallback timer fires, a new context is created and `defer fallbackCancel()` is registered. These deferred calls don't execute until `dialParallel` returns. In practice the loop usually runs only a few times, but the pattern is wrong — if the primary connection is slow and multiple fallback attempts are made, cancel functions accumulate on the defer stack without ever being called.

### 2. Sequential Dial — Deadline Leak per Address

When dialing a host that resolves to multiple addresses, Go tries each one with a proportional deadline:

```go
// net/dial.go, line 739
dialCtx := ctx
if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
    partialDeadline, err := partialDeadline(time.Now(), deadline, len(ras)-i)
    // ...
    if partialDeadline.Before(deadline) {
        var cancel context.CancelFunc
        dialCtx, cancel = context.WithDeadline(ctx, partialDeadline)
        defer cancel()  // BUG: accumulates per address
    }
}

c, err := sd.dialSingle(dialCtx, ra)
```

Each address gets its own context with a partial deadline, and each `defer cancel()` piles up. A host with many A/AAAA records (common with CDNs and load balancers) accumulates one context leak per address attempted.

### 3. DNS Resolver — Protocol Fallback Leak

The DNS resolver tries UDP first, then falls back to TCP:

```go
// net/dnsclient_unix.go, line 181
for _, network := range networks {
    ctx, cancel := context.WithDeadline(ctx, time.Now().Add(timeout))
    defer cancel()  // BUG: accumulates per protocol attempt

    c, err := r.dial(ctx, network, server)
    // ...
}
```

With the default configuration, `networks` is `["udp", "tcp"]`, so at most two cancel functions accumulate. The impact is minimal, but the pattern is still incorrect — the context from the UDP attempt is never canceled when the code moves on to TCP.

### 4. IPv6 Capability Probing — Socket Leak

During initialization, Go probes whether IPv6 is available by opening test sockets:

```go
// net/ipsock_posix.go, line 61
for i := range probes {
    s, err := sysSocket(syscall.AF_INET6, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
    if err != nil {
        continue
    }
    defer poll.CloseFunc(s)  // BUG: sockets closed only when function returns
    // ...
}
```

Each probe creates a raw socket and defers its close. The sockets remain open until the entire probing function returns, rather than being closed after each probe completes.

### 5. HTTP Transport — Timer Leak

In the HTTP round-trip implementation, a response header timeout timer is created inside a `for-select` loop:

```go
// net/http/transport.go, line 2920
if d := pc.t.ResponseHeaderTimeout; d > 0 {
    timer := time.NewTimer(d)
    defer timer.Stop() // prevent leaks
    respHeaderTimer = timer.C
}
```

The comment says "prevent leaks," but the defer is inside a loop — each iteration that creates a timer defers another `Stop()` call, and none of them execute until the function returns. The irony is that the code was written specifically to prevent leaks and introduces one instead.

## Why This Bug Is Hard to Find

### Linters don't see it

Standard Go linters (`go vet`, `staticcheck`, `golangci-lint`) don't flag `defer` inside loops. The code is syntactically valid and type-correct. There's no compile-time error and no obvious runtime failure — the deferred functions do eventually run, just later than intended.

### Code review misses it

The `defer` keyword signals "this resource will be cleaned up" to a human reader. It creates a false sense of security. In code review, `defer cancel()` immediately after `context.WithCancel()` looks correct — it's the recommended pattern. The bug is not in the `defer` itself but in the structural relationship between the `defer` and the enclosing loop, which requires understanding the control flow across multiple lines of code.

The Go standard library is one of the most carefully reviewed codebases in the Go ecosystem. It has extensive test coverage, is read by millions of developers, and has been refined over more than a decade. If this bug survives there, it survives everywhere.

### Tests don't catch it

The resource leak from defer-in-loop is a slow accumulation, not a crash. Tests pass because the deferred cleanup does eventually happen — when the function returns. The leak is visible only under sustained load or when the loop runs many iterations, conditions that unit tests rarely exercise.

### LLMs reproduce it

AI coding assistants trained on Go code have learned the `defer` cleanup pattern deeply. When generating code that creates resources in a loop, they reliably produce `defer cleanup()` — because that's the dominant pattern in their training data. The defer-in-loop variant looks identical to the correct single-use pattern, so LLMs generate it without hesitation.

## Why Structural Analysis Catches It

Caliper's `find_defer_in_loop` analyzer works at the control flow graph (CFG) level, not at the text level. It identifies `defer` statements whose enclosing block is a loop body (for, for-range, or for-select) by analyzing the structural relationship between the defer and its surrounding control flow.

This is fundamentally different from pattern matching on source text. A text-based linter would need to match `defer` inside curly braces that belong to a `for` statement — but Go's syntax makes this ambiguous without full parsing. A `defer` inside a `select` case inside a `for` loop (as in the Happy Eyeballs dialer) is three levels of nesting deep. A `defer` inside an `if` inside a `for` (as in the HTTP transport) is similarly buried. Caliper's CFG-based approach handles all of these uniformly because it reasons about the loop structure itself, not about the textual nesting of braces.

## The Correct Fix

The fix for defer-in-loop is to either call the cleanup explicitly at the end of each iteration, or extract the loop body into a helper function where the `defer` runs at the right scope:

```go
// Option 1: explicit cleanup
for _, network := range networks {
    ctx, cancel := context.WithDeadline(ctx, time.Now().Add(timeout))
    err := doQuery(ctx, network, server)
    cancel() // explicit, runs each iteration
    if err != nil {
        continue
    }
}

// Option 2: helper function
for _, network := range networks {
    if err := queryWithTimeout(ctx, network, server, timeout); err != nil {
        continue
    }
}

func queryWithTimeout(ctx context.Context, network, server string, timeout time.Duration) error {
    ctx, cancel := context.WithDeadline(ctx, time.Now().Add(timeout))
    defer cancel() // now scoped to the helper, runs each call
    // ...
}
```

## What This Means

Finding five instances of a subtle resource management bug in Go's standard library — in the network dialer, DNS resolver, and HTTP transport — demonstrates something important about the state of code analysis tooling.

The existing linting ecosystem is excellent at catching syntactic issues, style violations, and common mistakes. But structural bugs that emerge from the interaction between language semantics (`defer` runs at function return) and control flow (loops create repeated execution) fall into a gap. They're too subtle for pattern-matching linters, too context-dependent for LLMs, and too invisible for code review.

Caliper was built to fill that gap. It runs as an MCP server, exposing semantic analysis capabilities to AI-powered development tools. When an AI assistant generates code with a defer inside a loop, Caliper catches it before it ships. When a human reviewer approves a change that introduces this pattern, Caliper flags it in CI. The goal is not to replace existing tools but to catch the class of bugs they structurally cannot see.
