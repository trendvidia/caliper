package cfg

// This file implements path quantification over CFGs — the Go equivalent of
// Coccinelle's CTL model checking. Instead of exposing raw CTL operators
// (EF, AF, EG, AG), it provides named path queries that map directly to
// the MCP tool parameters.
//
// The key insight: an LLM should never choose between "exists on some path"
// and "forall paths". Instead, the MCP tool picks the right quantifier based
// on the tool's semantics (e.g., resource leaks need "forall", nil derefs
// need "exists"), and this layer executes it.

import (
	"go/ast"
)

// Quantifier determines whether a path property must hold on all paths
// or just on some path.
type Quantifier int

const (
	// AllPaths requires the property on every path from start to exit.
	// Equivalent to CTL's AF (inevitably).
	AllPaths Quantifier = iota

	// AnyPath requires the property on at least one path.
	// Equivalent to CTL's EF (possibly).
	AnyPath

	// Globally requires the property at every block on every path.
	// Equivalent to CTL's AG (invariant).
	Globally
)

// NodePredicate tests whether a CFG block contains a node satisfying some condition.
type NodePredicate func(block *Block, node ast.Node) bool

// PathResult describes a single path through the CFG that satisfies
// (or violates) a property.
type PathResult struct {
	// Path is the sequence of block IDs from start to the relevant point.
	Path []int

	// Satisfied is true if this path satisfies the property.
	Satisfied bool

	// RelevantNode is the node that caused satisfaction/violation.
	RelevantNode ast.Node

	// RelevantBlock is the block containing RelevantNode.
	RelevantBlock *Block
}

// --------------------------------------------------------------------------
// Path property queries
// --------------------------------------------------------------------------

// Reachable checks whether a node matching the predicate is reachable from
// the given start block, with the given quantifier.
//
// AllPaths: returns violations (paths where predicate is never satisfied).
// AnyPath: returns satisfying paths (paths where predicate is met).
func Reachable(g *Graph, start *Block, pred NodePredicate, q Quantifier) []PathResult {
	switch q {
	case AllPaths:
		return reachableAll(g, start, pred)
	case AnyPath:
		return reachableAny(g, start, pred)
	case Globally:
		return Invariant(g, start, pred)
	default:
		return nil
	}
}

// NeverBetween checks that no node matching `excluded` appears on paths
// between blocks matching `from` and blocks matching `to`.
//
// Returns paths where the excluded node DOES appear (violations).
func NeverBetween(g *Graph, from, to, excluded NodePredicate) []PathResult {
	var results []PathResult

	// Find all blocks matching `from`.
	for _, startBlock := range g.Blocks {
		if !blockMatches(startBlock, from) {
			continue
		}

		// BFS from startBlock looking for `to`, flagging if `excluded` is hit.
		type state struct {
			block   *Block
			path    []int
			tainted bool // true if excluded was seen
			node    ast.Node
		}

		type visitKey struct {
			id      int
			tainted bool
		}
		visited := make(map[visitKey]bool)
		queue := []state{{block: startBlock, path: []int{startBlock.ID}}}

		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]

			key := visitKey{cur.block.ID, cur.tainted}
			if visited[key] && cur.block != startBlock {
				continue
			}
			visited[key] = true

			// Check if this block matches `to`.
			if cur.block != startBlock && blockMatches(cur.block, to) {
				if cur.tainted {
					results = append(results, PathResult{
						Path:          cur.path,
						Satisfied:     false,
						RelevantNode:  cur.node,
						RelevantBlock: cur.block,
					})
				}
				continue // Don't go past `to`.
			}

			for _, succ := range cur.block.Succs {
				// Cycle detection.
				if pathContains(cur.path, succ.ID) {
					continue
				}
				if len(cur.path) >= maxPathLen {
					continue
				}

				tainted := cur.tainted
				taintNode := cur.node

				// Check if successor contains excluded pattern.
				// Use ast.Inspect to walk into child nodes, matching
				// the same behavior as blockMatches.
				if !tainted {
					for _, n := range succ.Nodes {
						ast.Inspect(n, func(child ast.Node) bool {
							if child == nil || tainted {
								return false
							}
							if excluded(succ, child) {
								tainted = true
								taintNode = child
								return false
							}
							return true
						})
						if tainted {
							break
						}
					}
				}

				queue = append(queue, state{
					block:   succ,
					path:    append(append([]int{}, cur.path...), succ.ID),
					tainted: tainted,
					node:    taintNode,
				})
			}
		}
	}

	return results
}

// AlwaysBefore checks that `required` always appears before `target` on
// every path from function entry.
//
// Returns violations: paths where target is reached without required first.
func AlwaysBefore(g *Graph, required, target NodePredicate) []PathResult {
	var results []PathResult

	type state struct {
		block     *Block
		path      []int
		satisfied bool // required was seen
	}

	type visitKey struct {
		id        int
		satisfied bool
	}
	visited := make(map[visitKey]bool)
	queue := []state{{block: g.Entry, path: []int{g.Entry.ID}}}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		satisfied := cur.satisfied

		// Check if required is in this block.
		if !satisfied && blockMatches(cur.block, required) {
			satisfied = true
		}

		key := visitKey{cur.block.ID, satisfied}
		if visited[key] {
			continue
		}
		visited[key] = true

		// Check if target is in this block without required being seen.
		if !satisfied && blockMatches(cur.block, target) {
			results = append(results, PathResult{
				Path:          cur.path,
				Satisfied:     false,
				RelevantBlock: cur.block,
			})
			continue
		}

		for _, succ := range cur.block.Succs {
			// Cycle detection.
			if pathContains(cur.path, succ.ID) {
				continue
			}
			if len(cur.path) >= maxPathLen {
				continue
			}
			queue = append(queue, state{
				block:     succ,
				path:      append(append([]int{}, cur.path...), succ.ID),
				satisfied: satisfied,
			})
		}
	}

	return results
}

// Dominates checks whether every path to `target` must go through `dominator`.
//
// Returns true if dominator dominates target in the CFG.
func Dominates(g *Graph, dominator, target NodePredicate) bool {
	// Find dominator blocks and target blocks.
	var domBlocks, targetBlocks []*Block
	for _, blk := range g.Blocks {
		if blockMatches(blk, dominator) {
			domBlocks = append(domBlocks, blk)
		}
		if blockMatches(blk, target) {
			targetBlocks = append(targetBlocks, blk)
		}
	}

	if len(domBlocks) == 0 || len(targetBlocks) == 0 {
		return false
	}

	// Simple dominator check: see if target is reachable from entry
	// without going through any dominator block.
	domSet := make(map[int]bool)
	for _, db := range domBlocks {
		domSet[db.ID] = true
	}

	targetSet := make(map[int]bool)
	for _, tb := range targetBlocks {
		targetSet[tb.ID] = true
	}

	// BFS from entry, skipping dominator blocks.
	// If a block is a dominator, we don't traverse past it — all paths
	// through it are considered "going through the dominator".
	visited := make(map[int]bool)
	queue := []*Block{g.Entry}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		if visited[cur.ID] {
			continue
		}
		visited[cur.ID] = true

		// If we reached a target without going through dominator → doesn't dominate.
		if targetSet[cur.ID] && !domSet[cur.ID] {
			return false
		}

		// Don't traverse past dominator blocks.
		if domSet[cur.ID] {
			continue
		}

		queue = append(queue, cur.Succs...)
	}

	return true
}

// --------------------------------------------------------------------------
// Internal helpers
// --------------------------------------------------------------------------

// maxPathLen is the maximum number of blocks in a path before we cut off
// traversal. This prevents infinite loops on cyclic CFGs.
const maxPathLen = 1000

// pathContains checks if a block ID appears in a path (cycle detection).
func pathContains(path []int, id int) bool {
	for _, p := range path {
		if p == id {
			return true
		}
	}
	return false
}

func reachableAll(g *Graph, start *Block, pred NodePredicate) []PathResult {
	// Find paths from start to exit where pred is never satisfied.
	// These are violations: the property doesn't hold on these paths.
	var violations []PathResult

	type state struct {
		block *Block
		path  []int
		found bool
	}

	// Use state-aware visited: (blockID, found) → visited.
	// This allows exploring a block both with and without the predicate satisfied.
	type visitKey struct {
		id    int
		found bool
	}
	visited := make(map[visitKey]bool)
	queue := []state{{block: start, path: []int{start.ID}}}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		found := cur.found
		if !found && blockMatches(cur.block, pred) {
			found = true
		}

		key := visitKey{cur.block.ID, found}
		if visited[key] {
			continue
		}
		visited[key] = true

		// Reached exit without satisfying predicate.
		if cur.block.Kind == BlockExit || cur.block.Kind == BlockReturn {
			if !found {
				violations = append(violations, PathResult{
					Path:          cur.path,
					Satisfied:     false,
					RelevantBlock: cur.block,
				})
			}
			continue
		}

		for _, succ := range cur.block.Succs {
			// Cycle detection: skip if this block is already in our path.
			if pathContains(cur.path, succ.ID) {
				continue
			}
			// Safety limit on path length.
			if len(cur.path) >= maxPathLen {
				continue
			}
			queue = append(queue, state{
				block: succ,
				path:  append(append([]int{}, cur.path...), succ.ID),
				found: found,
			})
		}
	}

	return violations
}

func reachableAny(g *Graph, start *Block, pred NodePredicate) []PathResult {
	// Find any path from start where pred is satisfied.
	var results []PathResult

	visited := make(map[int]bool)
	type state struct {
		block *Block
		path  []int
	}
	queue := []state{{block: start, path: []int{start.ID}}}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		if visited[cur.block.ID] {
			continue
		}
		visited[cur.block.ID] = true

		if blockMatches(cur.block, pred) {
			// Find the specific matching node for the result.
			for _, n := range cur.block.Nodes {
				var found ast.Node
				ast.Inspect(n, func(child ast.Node) bool {
					if child == nil || found != nil {
						return false
					}
					if pred(cur.block, child) {
						found = child
						return false
					}
					return true
				})
				if found != nil {
					results = append(results, PathResult{
						Path:          cur.path,
						Satisfied:     true,
						RelevantNode:  found,
						RelevantBlock: cur.block,
					})
					break
				}
			}
		}

		for _, succ := range cur.block.Succs {
			// Cycle detection: skip if already in path.
			if pathContains(cur.path, succ.ID) {
				continue
			}
			if len(cur.path) >= maxPathLen {
				continue
			}
			queue = append(queue, state{
				block: succ,
				path:  append(append([]int{}, cur.path...), succ.ID),
			})
		}
	}

	return results
}

func blockMatches(block *Block, pred NodePredicate) bool {
	for _, n := range block.Nodes {
		// Check the node itself and all child nodes.
		matched := false
		ast.Inspect(n, func(child ast.Node) bool {
			if child == nil || matched {
				return false
			}
			if pred(block, child) {
				matched = true
				return false
			}
			return true
		})
		if matched {
			return true
		}
	}
	return false
}

// --------------------------------------------------------------------------
// AG invariant and subgraph extraction
// --------------------------------------------------------------------------

// Invariant checks that a safety predicate holds at EVERY block on EVERY
// path from start to exit. The predicate is a safety check: it must return
// true for every node that is "safe". If any node in a block causes the
// predicate to return false, that block is a violation.
//
// Unlike reachableAll (which checks eventuality — pred satisfied before exit),
// Invariant checks a global property — pred must never be violated at any
// block on any path.
//
// Example: to check "no blocking call between lock and unlock", use a
// predicate that returns true for all nodes EXCEPT blocking calls.
func Invariant(g *Graph, start *Block, pred NodePredicate) []PathResult {
	var violations []PathResult

	type state struct {
		block *Block
		path  []int
	}

	visited := make(map[int]bool)
	queue := []state{{block: start, path: []int{start.ID}}}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		if visited[cur.block.ID] {
			continue
		}
		visited[cur.block.ID] = true

		// Check invariant: does any node in this block VIOLATE the predicate?
		if violationNode := blockViolation(cur.block, pred); violationNode != nil {
			violations = append(violations, PathResult{
				Path:          cur.path,
				Satisfied:     false,
				RelevantNode:  violationNode,
				RelevantBlock: cur.block,
			})
			continue // Don't traverse past a violation.
		}

		for _, succ := range cur.block.Succs {
			if pathContains(cur.path, succ.ID) {
				continue
			}
			if len(cur.path) >= maxPathLen {
				continue
			}
			queue = append(queue, state{
				block: succ,
				path:  append(append([]int{}, cur.path...), succ.ID),
			})
		}
	}

	return violations
}

// blockViolation returns the first node in a block where the predicate
// returns false, or nil if all nodes satisfy the predicate.
func blockViolation(block *Block, pred NodePredicate) ast.Node {
	for _, n := range block.Nodes {
		var violation ast.Node
		ast.Inspect(n, func(child ast.Node) bool {
			if child == nil || violation != nil {
				return false
			}
			if !pred(block, child) {
				violation = child
				return false
			}
			return true
		})
		if violation != nil {
			return violation
		}
	}
	return nil
}

// SubgraphBetween extracts the CFG portion between blocks matching from and
// blocks matching to. Returns a new Graph where:
//   - Entry is a synthetic entry block connected to all from-matching blocks
//   - Exit is a synthetic exit block connected from all to-matching blocks
//   - Only blocks reachable from from-blocks that can also reach to-blocks
//     are included
//   - Block objects are shared (not cloned) — this is a view over the original
func SubgraphBetween(g *Graph, from, to NodePredicate) *Graph {
	// Step 1: Find all blocks matching from and to.
	var fromBlocks, toBlocks []*Block
	for _, blk := range g.Blocks {
		if blockMatches(blk, from) {
			fromBlocks = append(fromBlocks, blk)
		}
		if blockMatches(blk, to) {
			toBlocks = append(toBlocks, blk)
		}
	}

	if len(fromBlocks) == 0 || len(toBlocks) == 0 {
		return nil
	}

	// Step 2: Forward BFS from fromBlocks to find all reachable blocks.
	forwardReachable := make(map[int]bool)
	{
		queue := make([]*Block, len(fromBlocks))
		copy(queue, fromBlocks)
		for _, blk := range fromBlocks {
			forwardReachable[blk.ID] = true
		}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for _, succ := range cur.Succs {
				if !forwardReachable[succ.ID] {
					forwardReachable[succ.ID] = true
					queue = append(queue, succ)
				}
			}
		}
	}

	// Step 3: Backward BFS from toBlocks to find all blocks that can reach to.
	backwardReachable := make(map[int]bool)
	{
		queue := make([]*Block, len(toBlocks))
		copy(queue, toBlocks)
		for _, blk := range toBlocks {
			backwardReachable[blk.ID] = true
		}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for _, pred := range cur.Preds {
				if !backwardReachable[pred.ID] {
					backwardReachable[pred.ID] = true
					queue = append(queue, pred)
				}
			}
		}
	}

	// Step 4: Intersection — blocks reachable from `from` that can reach `to`.
	subBlocks := make(map[int]*Block)
	for _, blk := range g.Blocks {
		if forwardReachable[blk.ID] && backwardReachable[blk.ID] {
			subBlocks[blk.ID] = blk
		}
	}

	if len(subBlocks) == 0 {
		return nil
	}

	// Step 5: Create synthetic entry/exit and wire edges.
	nextID := 0
	for _, blk := range g.Blocks {
		if blk.ID >= nextID {
			nextID = blk.ID + 1
		}
	}

	syntheticEntry := &Block{ID: nextID, Kind: BlockEntry}
	nextID++
	syntheticExit := &Block{ID: nextID, Kind: BlockExit}

	// Wire synthetic entry to from-blocks that are in the subgraph.
	for _, blk := range fromBlocks {
		if _, ok := subBlocks[blk.ID]; ok {
			syntheticEntry.Succs = append(syntheticEntry.Succs, blk)
		}
	}

	// Wire to-blocks to synthetic exit.
	for _, blk := range toBlocks {
		if _, ok := subBlocks[blk.ID]; ok {
			blk.Succs = append(blk.Succs, syntheticExit)
			syntheticExit.Preds = append(syntheticExit.Preds, blk)
		}
	}

	// Collect all blocks for the new graph.
	var allBlocks []*Block
	allBlocks = append(allBlocks, syntheticEntry, syntheticExit)
	for _, blk := range g.Blocks {
		if _, ok := subBlocks[blk.ID]; ok {
			allBlocks = append(allBlocks, blk)
		}
	}

	return &Graph{
		Entry:  syntheticEntry,
		Exit:   syntheticExit,
		Blocks: allBlocks,
		Func:   g.Func,
		Info:   g.Info,
		Fset:   g.Fset,
	}
}
