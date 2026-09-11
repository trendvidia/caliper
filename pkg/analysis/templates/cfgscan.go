package templates

import (
	"go/ast"

	"github.com/trendvidia/caliper/pkg/analysis/cfg"
)

// sourceNodes calls fn for every CFG node at the place it appears in the
// source, once.
//
// The CFG lists a defer statement twice: in the block that registers it,
// and again in a BlockDefer block on the exit chain, where it runs. Path
// queries need the second — it is how an unlock or a Close in a defer is
// seen on every exit — but a rule that scans nodes to report on them
// wants each statement once, and iterating ctx.CFG.Blocks directly
// reports every deferred call twice. BlockDefer blocks hold only those
// copies, so skipping them loses nothing
// (cfg.TestBuild_DeferBlocksHoldOnlyCopies pins that, and
// TestSourceNodes_DeferredCallReportedOnce pins each rule using this).
func sourceNodes(g *cfg.Graph, fn func(*cfg.Block, ast.Node)) {
	for _, b := range g.Blocks {
		if b.Kind == cfg.BlockDefer {
			continue
		}
		for _, n := range b.Nodes {
			fn(b, n)
		}
	}
}
