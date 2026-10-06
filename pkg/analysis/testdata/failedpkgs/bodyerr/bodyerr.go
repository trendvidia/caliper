// Package bodyerr has a type error inside a function body and clean
// declarations.
package bodyerr

func Good() int { return 1 }

func Bad() int {
	var x int = "not an int"
	return x
}
