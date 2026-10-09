// Package genericmethod reaches methods with type parameters of their
// own: math/rand/v2's (*Rand).N through a *rand.Rand held in an
// interface, and one declared here.
package genericmethod

import "math/rand/v2"

// Box holds a value.
type Box struct{ v int }

// Map applies f to the box's value.
func (b *Box) Map[T any](f func(int) T) T { return f(b.v) }

// Held keeps its values behind interfaces, so the program's runtime
// types include *rand.Rand and *Box with their method sets.
var Held []any = []any{rand.New(rand.NewPCG(1, 2)), &Box{v: 3}}

// Roll calls both generic methods.
func Roll(r *rand.Rand, b *Box) string {
	n := r.N(6)
	return b.Map[string](func(v int) string { return string(rune('0' + v + n)) })
}
