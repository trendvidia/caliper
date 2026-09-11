// Package nocompile type-checks but does not compile: Magic has no body
// and no assembly file supplies one, so `go list -export` produces no
// export data for it and go/packages falls back to type-checking it from
// source. Under DepsFromExportData that fallback skips function bodies.
package nocompile

// Magic is declared without a body.
func Magic() int

// Double has a body that calls Magic.
func Double() int { return Magic() * 2 }
