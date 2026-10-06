//go:build never

// Package excluded has no file any configuration builds, so ./... does
// not match it.
package excluded

func Excluded() {}
