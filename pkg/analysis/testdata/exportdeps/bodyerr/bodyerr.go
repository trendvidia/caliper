// Package bodyerr has a type error inside a function body and nowhere
// else: its declarations type-check, its package does not compile.
package bodyerr

// Good is a clean function importers can call.
func Good() int { return 1 }

// Bad refers to an undefined name inside its body.
func Bad() int { return undefinedName }
