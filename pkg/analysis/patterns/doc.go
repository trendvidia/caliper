// Package patterns implements AST pattern matching with metavariables
// for Go source code.
//
// Pattern syntax:
//
//	$x       — captures any expression/identifier as metavariable "x"
//	$_       — matches any expression without capturing
//	$x...    — matches zero or more arguments/elements (variadic)
//	...      — matches zero or more statements in a block
//
// Patterns are parsed as Go code fragments, with $-prefixed identifiers
// treated as metavariables.
package patterns
