// Package mcp implements the MCP server for Caliper.
//
// It registers all tools and routes incoming requests to the appropriate
// analysis engine, template, or transformation. The server runs over
// stdio (standard MCP transport).
package mcp
