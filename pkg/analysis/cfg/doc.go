// Package cfg builds control flow graphs from Go functions and provides
// path-level analysis primitives — the engine behind Caliper's
// control-flow-aware tools.
//
// This is the Go equivalent of Coccinelle's CTL engine. Instead of
// exposing raw temporal logic operators, it provides high-level path
// queries that the MCP tool layer can call directly.
package cfg
