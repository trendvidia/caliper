// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package cfg

// This file implements natural loop detection in CFGs using dominator analysis.
// A natural loop is defined by a back-edge B→A where A dominates B. The loop
// body consists of all blocks that can reach B without going through A.
//
// This is the Go equivalent of the loop-handling logic in Coccinelle's CTL
// engine, which must distinguish first-iteration vs subsequent-iteration
// behavior when matching semantic patches.

// LoopInfo describes a natural loop in the CFG.
type LoopInfo struct {
	Header    *Block   // the loop header block (target of back-edge)
	BackEdges []*Block // blocks with back-edges to header
	Body      []*Block // all blocks in the loop body
	Exit      []*Block // blocks that exit the loop
}

// LoopIteration classifies which iteration a block belongs to.
type LoopIteration int

const (
	FirstIteration LoopIteration = iota
	SubsequentIteration
	NotInLoop
)

// FindLoops identifies all natural loops in the CFG.
//
// Implementation:
//  1. Compute dominators using a simple iterative algorithm
//  2. Find back-edges: edge B→A where A dominates B
//  3. For each back-edge, collect the natural loop body
//  4. Identify exit blocks: body blocks with successors outside the body
func FindLoops(g *Graph) []LoopInfo {
	doms := computeDominators(g)
	backEdges := findBackEdges(g, doms)

	// Group back-edges by header.
	headerEdges := make(map[int][]*Block)
	for _, be := range backEdges {
		headerEdges[be.header.ID] = append(headerEdges[be.header.ID], be.source)
	}

	var loops []LoopInfo
	for headerID, sources := range headerEdges {
		header := blockByID(g, headerID)
		if header == nil {
			continue
		}

		// Collect the natural loop body: all blocks that can reach any
		// back-edge source without going through the header.
		body := collectLoopBody(header, sources)

		// Find exit blocks: body blocks with successors outside the body.
		bodySet := make(map[int]bool)
		for _, blk := range body {
			bodySet[blk.ID] = true
		}
		var exits []*Block
		for _, blk := range body {
			for _, succ := range blk.Succs {
				if !bodySet[succ.ID] {
					exits = append(exits, blk)
					break
				}
			}
		}

		loops = append(loops, LoopInfo{
			Header:    header,
			BackEdges: sources,
			Body:      body,
			Exit:      exits,
		})
	}

	return loops
}

// ClassifyIteration returns whether a block is in the first or subsequent
// iteration of a loop.
//
// A block is in the first iteration if it is reachable from the loop header
// without going through any back-edge. A block is in a subsequent iteration
// if it is only reachable via a back-edge to the header.
func ClassifyIteration(g *Graph, block *Block) LoopIteration {
	loops := FindLoops(g)

	for _, loop := range loops {
		bodySet := make(map[int]bool)
		for _, blk := range loop.Body {
			bodySet[blk.ID] = true
		}
		if !bodySet[block.ID] {
			continue
		}

		// Check if block is reachable from the header without going
		// through a back-edge. We BFS from the header, but exclude
		// back-edge sources → header transitions.
		backEdgeSet := make(map[int]bool)
		for _, be := range loop.BackEdges {
			backEdgeSet[be.ID] = true
		}

		// BFS from header, skipping back-edge → header transitions.
		visited := make(map[int]bool)
		queue := []*Block{loop.Header}
		visited[loop.Header.ID] = true
		reachableFirst := make(map[int]bool)
		reachableFirst[loop.Header.ID] = true

		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]

			for _, succ := range cur.Succs {
				if !bodySet[succ.ID] {
					continue
				}
				if visited[succ.ID] {
					continue
				}
				// If succ is the header and cur is a back-edge source,
				// this is the back-edge — skip it.
				if succ.ID == loop.Header.ID && backEdgeSet[cur.ID] {
					continue
				}
				visited[succ.ID] = true
				reachableFirst[succ.ID] = true
				queue = append(queue, succ)
			}
		}

		if reachableFirst[block.ID] {
			return FirstIteration
		}
		return SubsequentIteration
	}

	return NotInLoop
}

// --------------------------------------------------------------------------
// Internal: dominator computation
// --------------------------------------------------------------------------

// backEdge represents a back-edge in the CFG.
type backEdge struct {
	source *Block // the block with the edge
	header *Block // the dominator (target of back-edge)
}

// computeDominators computes the immediate dominator for each block using
// the standard iterative algorithm. Returns a map from block ID to its
// dominator set (set of block IDs that dominate it).
func computeDominators(g *Graph) map[int]map[int]bool {
	// Initialize: entry dominates only itself; all others dominate
	// themselves and potentially everything.
	allIDs := make(map[int]bool)
	for _, blk := range g.Blocks {
		allIDs[blk.ID] = true
	}

	doms := make(map[int]map[int]bool)
	for _, blk := range g.Blocks {
		if blk == g.Entry {
			doms[blk.ID] = map[int]bool{blk.ID: true}
		} else {
			// Start with all blocks as potential dominators.
			d := make(map[int]bool)
			for id := range allIDs {
				d[id] = true
			}
			doms[blk.ID] = d
		}
	}

	// Iterate until fixed point.
	changed := true
	for changed {
		changed = false
		for _, blk := range g.Blocks {
			if blk == g.Entry {
				continue
			}

			// New dom set = intersection of all predecessors' dom sets, plus self.
			var newDom map[int]bool
			for _, pred := range blk.Preds {
				predDom := doms[pred.ID]
				if newDom == nil {
					newDom = make(map[int]bool)
					for id := range predDom {
						newDom[id] = true
					}
				} else {
					// Intersect.
					for id := range newDom {
						if !predDom[id] {
							delete(newDom, id)
						}
					}
				}
			}

			if newDom == nil {
				newDom = make(map[int]bool)
			}
			newDom[blk.ID] = true

			// Check if changed.
			if !domSetsEqual(doms[blk.ID], newDom) {
				doms[blk.ID] = newDom
				changed = true
			}
		}
	}

	return doms
}

func domSetsEqual(a, b map[int]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for id := range a {
		if !b[id] {
			return false
		}
	}
	return true
}

// findBackEdges identifies back-edges: edges B→A where A dominates B.
func findBackEdges(g *Graph, doms map[int]map[int]bool) []backEdge {
	var edges []backEdge
	for _, blk := range g.Blocks {
		for _, succ := range blk.Succs {
			// B→A is a back-edge if A dominates B.
			if doms[blk.ID][succ.ID] {
				edges = append(edges, backEdge{source: blk, header: succ})
			}
		}
	}
	return edges
}

// collectLoopBody collects all blocks in a natural loop.
// The loop is defined by a header and a set of back-edge sources.
// The body includes the header and all blocks that can reach the back-edge
// sources without leaving through the header.
func collectLoopBody(header *Block, sources []*Block) []*Block {
	body := make(map[int]*Block)
	body[header.ID] = header

	// Work list: start from back-edge sources, walk backward to header.
	var worklist []*Block
	for _, src := range sources {
		if _, ok := body[src.ID]; !ok {
			body[src.ID] = src
			worklist = append(worklist, src)
		}
	}

	for len(worklist) > 0 {
		cur := worklist[0]
		worklist = worklist[1:]

		for _, pred := range cur.Preds {
			if _, ok := body[pred.ID]; !ok {
				body[pred.ID] = pred
				worklist = append(worklist, pred)
			}
		}
	}

	// Collect into slice.
	result := make([]*Block, 0, len(body))
	for _, blk := range body {
		result = append(result, blk)
	}
	return result
}

// blockByID finds a block by ID in the graph.
func blockByID(g *Graph, id int) *Block {
	for _, blk := range g.Blocks {
		if blk.ID == id {
			return blk
		}
	}
	return nil
}
