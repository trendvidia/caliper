// Package usesdeclerr type-checks cleanly by itself and imports a
// dependency whose declarations do not.
package usesdeclerr

import "local.example/declerr"

// Use calls the dependency's clean function.
func Use() int { return declerr.Fine() }
