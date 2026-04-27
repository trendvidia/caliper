package templates

import (
	"go/ast"
	"go/token"
)

// findInnermostBlock returns the deepest *ast.BlockStmt under root that
// lexically contains pos. Used by templates that need to identify the
// statement-level scope around a call (e.g., for sibling-statement
// inspection in cleanup-path detection or mutex-bracketing checks).
func findInnermostBlock(root ast.Node, pos token.Pos) *ast.BlockStmt {
	var found *ast.BlockStmt
	ast.Inspect(root, func(n ast.Node) bool {
		block, ok := n.(*ast.BlockStmt)
		if !ok {
			return true
		}
		if block.Pos() <= pos && pos <= block.End() {
			found = block
		}
		return true
	})
	return found
}
