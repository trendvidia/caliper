package mcp

import (
	"context"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// progress sends MCP progress notifications during long-running operations.
// It is a no-op if the request did not include a progress token.
type progress struct {
	token mcpsdk.ProgressToken
	srv   *mcpserver.MCPServer
	ctx   context.Context
}

// newProgress extracts the progress token from a request and returns a
// progress notifier. All methods are safe to call even if no token was sent.
func newProgress(ctx context.Context, req mcpsdk.CallToolRequest) *progress {
	p := &progress{ctx: ctx}
	if req.Params.Meta != nil {
		p.token = req.Params.Meta.ProgressToken
	}
	if p.token != nil {
		p.srv = mcpserver.ServerFromContext(ctx)
	}
	return p
}

// active returns true if the client sent a progress token.
func (p *progress) active() bool {
	return p.token != nil && p.srv != nil
}

// send sends a progress notification. current and total are arbitrary
// progress units. message is an optional human-readable status.
func (p *progress) send(current, total float64, message string) {
	if !p.active() {
		return
	}
	params := map[string]any{
		"progressToken": p.token,
		"progress":      current,
		"total":         total,
	}
	if message != "" {
		params["message"] = message
	}
	p.srv.SendNotificationToClient(p.ctx, "notifications/progress", params)
}
