package mcp

import (
	"strings"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// Helpers for extracting typed parameters from MCP CallToolRequests and
// for parsing common qualified-name shapes used in tool inputs.

// stringParam returns the string value at key in the request's args, or
// "" if the key is missing or holds a non-string value.
func stringParam(req mcpsdk.CallToolRequest, key string) string {
	v, ok := req.GetArguments()[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

// boolParam returns the bool value at key, or false if missing/wrong-type.
func boolParam(req mcpsdk.CallToolRequest, key string) bool {
	v, ok := req.GetArguments()[key]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	if !ok {
		return false
	}
	return b
}

// boolParamDefault returns the bool value at key, or def if missing/wrong-type.
func boolParamDefault(req mcpsdk.CallToolRequest, key string, def bool) bool {
	v, ok := req.GetArguments()[key]
	if !ok {
		return def
	}
	b, ok := v.(bool)
	if !ok {
		return def
	}
	return b
}

// stringSliceParam returns the []string value at key. JSON arrays arrive
// as []any; non-string elements are skipped.
func stringSliceParam(req mcpsdk.CallToolRequest, key string) []string {
	v, ok := req.GetArguments()[key]
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// intParam returns the int value at key. JSON numbers arrive as float64,
// so the helper handles that conversion.
func intParam(req mcpsdk.CallToolRequest, key string) int {
	v, ok := req.GetArguments()[key]
	if !ok {
		return 0
	}
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

// parseQualifiedType splits a Go type expression like "*os.File" into
// the package qualifier and base name: ("os", "File"). For unqualified
// names like "string" it returns ("", "string").
func parseQualifiedType(s string) (string, string) {
	s = strings.TrimPrefix(s, "*")
	if idx := strings.LastIndex(s, "."); idx >= 0 {
		return s[:idx], s[idx+1:]
	}
	return "", s
}

// parseQualifiedName splits a fully-qualified function/method reference
// like "database/sql.Query" into ("database/sql", "Query"). For
// unqualified names returns ("", name).
func parseQualifiedName(s string) (string, string) {
	if idx := strings.LastIndex(s, "."); idx >= 0 {
		return s[:idx], s[idx+1:]
	}
	return "", s
}
