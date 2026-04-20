// Command gocaliper-mcp runs the GoCaliper MCP server.
//
// GoCaliper is a semantic code analysis and transformation tool for Go,
// inspired by Coccinelle. It exposes high-level, intent-based tools over MCP
// so that an LLM can request precise, AST-level and control-flow-level
// operations without needing to understand the formal logic underneath.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/trendvidia/gocaliper/pkg/mcp"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	srv, err := mcp.NewServer()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gocaliper: %v\n", err)
		os.Exit(1)
	}

	if err := srv.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "gocaliper: %v\n", err)
		os.Exit(1)
	}
}
