// Package declerr has a type error in a declaration: the declarations
// an importer relies on are not trustworthy.
package declerr

// Fine is a clean function importers can call.
func Fine() int { return 1 }

// X has an undefined type.
var X MissingType
