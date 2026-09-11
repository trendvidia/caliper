// Package usesbodyerr type-checks cleanly and imports a dependency whose
// only error is inside a function body.
package usesbodyerr

import "local.example/bodyerr"

// Use calls the dependency's clean function.
func Use() int { return bodyerr.Good() + helper() }

func helper() int { return 1 }
