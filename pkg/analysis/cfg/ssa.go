// Package cfg — SSA-based CFG construction.
//
// This file provides an alternative CFG backend using golang.org/x/tools/go/ssa.
// The SSA representation provides more precise dataflow information than the
// AST-based CFG builder, at the cost of losing direct correspondence to source
// syntax.
package cfg

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"golang.org/x/tools/go/packages"
)

// BuildSSA constructs a CFG from SSA form for a function.
// The resulting Graph uses the same Block/Graph types as the AST-based builder,
// making it interchangeable for path analysis.
func BuildSSA(fset *token.FileSet, fn *ast.FuncDecl, info *types.Info, pkg *packages.Package) *Graph {
	// Build SSA program from the package.
	prog, ssaPkgs := ssautil.AllPackages([]*packages.Package{pkg}, ssa.SanityCheckFunctions)
	prog.Build()

	if len(ssaPkgs) == 0 {
		return buildFallback(fset, fn, info)
	}

	ssaPkg := ssaPkgs[0]
	if ssaPkg == nil {
		return buildFallback(fset, fn, info)
	}

	return BuildSSAFromProgram(fset, fn, info, prog, ssaPkg)
}

// BuildSSAFromProgram constructs a CFG from an existing SSA program and
// package. This avoids rebuilding the SSA program when an
// InterProceduralContext already holds one.
func BuildSSAFromProgram(fset *token.FileSet, fn *ast.FuncDecl, info *types.Info, prog *ssa.Program, ssaPkg *ssa.Package) *Graph {
	// Find the SSA function matching the AST function declaration.
	ssaFn := findSSAFunction(prog, ssaPkg, fn)
	if ssaFn == nil || len(ssaFn.Blocks) == 0 {
		return buildFallback(fset, fn, info)
	}

	return convertSSAToGraph(fset, fn, info, ssaFn)
}

// findSSAFunction locates the SSA function corresponding to an AST
// function declaration within a package.
func findSSAFunction(prog *ssa.Program, ssaPkg *ssa.Package, fn *ast.FuncDecl) *ssa.Function {
	if fn.Recv == nil {
		return ssaPkg.Func(fn.Name.Name)
	}

	// Method — search through all members.
	for _, member := range ssaPkg.Members {
		typ, ok := member.(*ssa.Type)
		if !ok {
			continue
		}

		named := typ.Type()

		// Check named type's method set.
		mset := prog.MethodSets.MethodSet(named)
		for i := 0; i < mset.Len(); i++ {
			sel := mset.At(i)
			if sel.Obj().Name() == fn.Name.Name {
				return prog.MethodValue(sel)
			}
		}

		// Check pointer receiver method set.
		mset = prog.MethodSets.MethodSet(types.NewPointer(named))
		for i := 0; i < mset.Len(); i++ {
			sel := mset.At(i)
			if sel.Obj().Name() == fn.Name.Name {
				return prog.MethodValue(sel)
			}
		}
	}
	return nil
}

// convertSSAToGraph maps SSA basic blocks to our Graph/Block representation.
func convertSSAToGraph(fset *token.FileSet, fn *ast.FuncDecl, info *types.Info, ssaFn *ssa.Function) *Graph {
	blockMap := make(map[*ssa.BasicBlock]*Block)
	var blocks []*Block
	nextID := 0

	newBlock := func(kind BlockKind) *Block {
		blk := &Block{ID: nextID, Kind: kind}
		nextID++
		blocks = append(blocks, blk)
		return blk
	}

	// Create entry and exit blocks.
	entry := newBlock(BlockEntry)
	exit := newBlock(BlockExit)

	// Map SSA blocks to our blocks.
	for _, ssaBlock := range ssaFn.Blocks {
		kind := BlockNormal

		// Classify block based on its terminator.
		if len(ssaBlock.Instrs) > 0 {
			lastInstr := ssaBlock.Instrs[len(ssaBlock.Instrs)-1]
			switch lastInstr.(type) {
			case *ssa.Return:
				kind = BlockReturn
			case *ssa.If:
				kind = BlockBranch
			case *ssa.Panic:
				kind = BlockPanic
			}
		}

		blk := newBlock(kind)
		blk.Pos = ssaBlock.Instrs[0].Pos()

		// Collect AST nodes from SSA instructions for matching.
		for _, instr := range ssaBlock.Instrs {
			if pos := instr.Pos(); pos.IsValid() {
				// Store the instruction position for source mapping.
				// SSA instructions don't directly map to AST nodes,
				// but we can store a synthetic node for position tracking.
				blk.Nodes = append(blk.Nodes, &ssaNodeWrapper{
					pos:   pos,
					end:   pos,
					instr: instr,
				})
			}
		}

		blockMap[ssaBlock] = blk
	}

	// Wire edges based on SSA successor/predecessor relationships.
	addEdge := func(from, to *Block) {
		from.Succs = append(from.Succs, to)
		to.Preds = append(to.Preds, from)
	}

	for _, ssaBlock := range ssaFn.Blocks {
		blk := blockMap[ssaBlock]

		for _, succ := range ssaBlock.Succs {
			if target, ok := blockMap[succ]; ok {
				addEdge(blk, target)
			}
		}

		// Return blocks → exit.
		if blk.Kind == BlockReturn {
			addEdge(blk, exit)
		}
	}

	// Wire entry → first SSA block.
	if firstBlock, ok := blockMap[ssaFn.Blocks[0]]; ok {
		addEdge(entry, firstBlock)
	}

	// If exit has no predecessors, connect it to entry (empty function).
	if len(exit.Preds) == 0 {
		addEdge(entry, exit)
	}

	return &Graph{
		Entry:  entry,
		Exit:   exit,
		Blocks: blocks,
		Func:   fn,
		Info:   info,
		Fset:   fset,
	}
}

// buildFallback uses the AST-based builder as a fallback.
func buildFallback(fset *token.FileSet, fn *ast.FuncDecl, info *types.Info) *Graph {
	return Build(fset, fn, info)
}

// ssaNodeWrapper wraps an SSA instruction to implement ast.Node.
type ssaNodeWrapper struct {
	pos   token.Pos
	end   token.Pos
	instr ssa.Instruction
}

func (w *ssaNodeWrapper) Pos() token.Pos { return w.pos }
func (w *ssaNodeWrapper) End() token.Pos { return w.end }

// Instr returns the underlying SSA instruction.
func (w *ssaNodeWrapper) Instr() ssa.Instruction { return w.instr }
