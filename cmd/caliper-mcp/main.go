// Command caliper-mcp runs the Caliper MCP server.
//
// Caliper is a semantic code analysis and transformation tool for Go,
// inspired by Coccinelle. It exposes high-level, intent-based tools over MCP
// so that an LLM can request precise, AST-level and control-flow-level
// operations without needing to understand the formal logic underneath.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"

	"github.com/trendvidia/caliper/pkg/mcp"
)

func main() {
	// Ensure full stack traces on fatal crashes (concurrent map access, etc.)
	// so the root cause can be diagnosed from MCP client logs.
	os.Setenv("GOTRACEBACK", "crash")

	// Top-level panic recovery — last resort before process death.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "caliper: fatal panic: %v\n%s\n", r, debug.Stack())
			os.Exit(2)
		}
	}()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	srv, err := mcp.NewServer()
	if err != nil {
		fmt.Fprintf(os.Stderr, "caliper: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "caliper: server starting\n")

	if err := srv.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "caliper: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "caliper: server stopped\n")
}
