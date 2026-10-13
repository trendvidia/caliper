// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package mcp

import (
	"fmt"
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
// as []any. A bare string is accepted as a one-element list. Any other
// shape is an error rather than a silent nil — dropping a malformed
// filter parameter would make unfiltered results look filtered.
func stringSliceParam(req mcpsdk.CallToolRequest, key string) ([]string, error) {
	v, ok := req.GetArguments()[key]
	if !ok {
		return nil, nil
	}
	if s, ok := v.(string); ok {
		return []string{s}, nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("parameter %q must be an array of strings, got %T", key, v)
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("parameter %q must contain only strings, got %T element", key, e)
		}
		out = append(out, s)
	}
	return out, nil
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
