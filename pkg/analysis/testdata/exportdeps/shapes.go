// Package exportdeps is a fixture for EngineOptions.DepsFromExportData: a
// module with a third-party dependency (github.com/google/uuid), a
// standard-library one (fmt), a static call into each, an interface
// dispatched within the module, and a type the standard library calls
// back into (fmt's Stringer).
package exportdeps

import (
	"fmt"

	"github.com/google/uuid"
	"local.example/nocompile"
)

// Shape is dispatched dynamically within the module.
type Shape interface {
	Area() float64
}

// Square implements Shape and fmt.Stringer.
type Square struct{ Side float64 }

// Area is the module's own implementation of Shape.
func (s Square) Area() float64 { return s.Side * s.Side }

// String is called back from inside fmt when Square is printed.
func (s Square) String() string { return fmt.Sprintf("square %v", s.Side) }

// Circle is the second Shape, so the dynamic call has two candidates.
type Circle struct{ Radius float64 }

// Area is the module's second implementation of Shape.
func (c Circle) Area() float64 { return 3 * c.Radius * c.Radius }

// Total sums areas through the interface: a within-module dynamic call.
func Total(shapes []Shape) float64 {
	sum := 0.0
	for _, s := range shapes {
		sum += s.Area()
	}
	return sum
}

// Tag calls a third-party function statically and prints a Stringer.
func Tag(s Square) string {
	id := uuid.New()
	fmt.Println(s)
	return id.String() + ":" + describe(s)
}

// describe is a within-module static callee.
func describe(s Shape) string { return fmt.Sprint(s.Area()) }

// Boundary calls into a dependency that has no export data.
func Boundary() int { return nocompile.Double() }
