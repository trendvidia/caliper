// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package cfg

import (
	"go/ast"
	"go/token"
	"go/types"
)

// --------------------------------------------------------------------------
// CFG representation
// --------------------------------------------------------------------------

// BlockKind classifies a basic block.
type BlockKind int

const (
	BlockEntry BlockKind = iota
	BlockExit
	BlockNormal
	BlockBranch // if/switch condition
	BlockReturn
	BlockDefer
	BlockPanic
)

// Block is a basic block in the control flow graph.
type Block struct {
	ID    int
	Kind  BlockKind
	Stmts []ast.Stmt // statements in this block
	Nodes []ast.Node // all AST nodes (for matching)
	Succs []*Block   // successor blocks
	Preds []*Block   // predecessor blocks
	Pos   token.Pos  // position of the first statement
}

// Graph is a control flow graph for a single function.
type Graph struct {
	Entry  *Block
	Exit   *Block
	Blocks []*Block
	Func   *ast.FuncDecl
	Info   *types.Info
	Fset   *token.FileSet
}

// Build constructs a CFG from a function declaration.
func Build(fset *token.FileSet, fn *ast.FuncDecl, info *types.Info) *Graph {
	b := &builder{
		fset: fset,
		info: info,
		fn:   fn,
	}
	return b.build()
}

// loopContext tracks the header and exit blocks for the innermost loop/switch/select,
// so break and continue can be wired correctly.
type loopContext struct {
	header *Block // loop condition (continue target)
	exit   *Block // block after the loop (break target)
	label  string // optional label for labeled break/continue
}

type builder struct {
	fset         *token.FileSet
	info         *types.Info
	fn           *ast.FuncDecl
	blocks       []*Block
	nextID       int
	loopStack    []loopContext       // stack of enclosing loops/switches
	labels       map[string]*Block   // label → target block (for goto)
	gotos        map[string][]*Block // label → blocks needing edges to that label (forward gotos)
	pendingLabel string              // label for the next loop/switch/select being built
	defers       []*ast.DeferStmt    // defer statements in order of appearance
}

func (b *builder) newBlock(kind BlockKind) *Block {
	blk := &Block{ID: b.nextID, Kind: kind}
	b.nextID++
	b.blocks = append(b.blocks, blk)
	return blk
}

func (b *builder) addEdge(from, to *Block) {
	from.Succs = append(from.Succs, to)
	to.Preds = append(to.Preds, from)
}

func (b *builder) build() *Graph {
	b.labels = make(map[string]*Block)
	b.gotos = make(map[string][]*Block)

	entry := b.newBlock(BlockEntry)
	exit := b.newBlock(BlockExit)

	if b.fn.Body == nil {
		b.addEdge(entry, exit)
		return &Graph{
			Entry:  entry,
			Exit:   exit,
			Blocks: b.blocks,
			Func:   b.fn,
			Info:   b.info,
			Fset:   b.fset,
		}
	}

	// Build CFG from function body.
	last := b.buildStmtList(entry, b.fn.Body.List, exit)
	if last != nil && last != exit {
		// Implicit return at end of function.
		b.addEdge(last, exit)
	}

	// Resolve any remaining forward gotos.
	for label, sources := range b.gotos {
		if target, ok := b.labels[label]; ok {
			for _, src := range sources {
				b.addEdge(src, target)
			}
		}
	}

	// Insert defer blocks before exit on all return/exit paths.
	// Defers execute in LIFO order (last defer first).
	if len(b.defers) > 0 {
		b.insertDeferChain(exit)
	}

	return &Graph{
		Entry:  entry,
		Exit:   exit,
		Blocks: b.blocks,
		Func:   b.fn,
		Info:   b.info,
		Fset:   b.fset,
	}
}

func (b *builder) pushLoop(header, exit *Block, label string) {
	b.loopStack = append(b.loopStack, loopContext{header: header, exit: exit, label: label})
}

func (b *builder) popLoop() {
	b.loopStack = b.loopStack[:len(b.loopStack)-1]
}

// findLoop finds the loop context for a break/continue, optionally by label.
func (b *builder) findLoop(label string) *loopContext {
	if label == "" {
		if len(b.loopStack) == 0 {
			return nil
		}
		return &b.loopStack[len(b.loopStack)-1]
	}
	for i := len(b.loopStack) - 1; i >= 0; i-- {
		if b.loopStack[i].label == label {
			return &b.loopStack[i]
		}
	}
	return nil
}

// buildStmtList processes a list of statements, chaining blocks.
// Returns the last block in the chain (or nil if all paths exit).
func (b *builder) buildStmtList(current *Block, stmts []ast.Stmt, exit *Block) *Block {
	for _, stmt := range stmts {
		if current == nil {
			// After a return/goto/break/continue, subsequent code is unreachable
			// unless it's a labeled statement (reachable via goto).
			if ls, ok := stmt.(*ast.LabeledStmt); ok {
				// Create a new disconnected block for the label.
				// It will be connected when gotos to this label are resolved.
				current = b.buildStmt(b.newBlock(BlockNormal), ls, exit)
				continue
			}
			continue // skip unreachable code
		}
		current = b.buildStmt(current, stmt, exit)
	}
	return current
}

func (b *builder) buildStmt(current *Block, stmt ast.Stmt, exit *Block) *Block {
	switch s := stmt.(type) {
	case *ast.ReturnStmt:
		current.Stmts = append(current.Stmts, s)
		current.Nodes = append(current.Nodes, s)
		ret := b.newBlock(BlockReturn)
		b.addEdge(current, ret)
		b.addEdge(ret, exit)
		return nil // no fall-through after return

	case *ast.IfStmt:
		return b.buildIf(current, s, exit)

	case *ast.ForStmt:
		return b.buildFor(current, s, exit)

	case *ast.RangeStmt:
		return b.buildRange(current, s, exit)

	case *ast.SwitchStmt:
		return b.buildSwitch(current, s, exit)

	case *ast.TypeSwitchStmt:
		return b.buildTypeSwitch(current, s, exit)

	case *ast.SelectStmt:
		return b.buildSelect(current, s, exit)

	case *ast.DeferStmt:
		current.Stmts = append(current.Stmts, s)
		current.Nodes = append(current.Nodes, s)
		// Record the defer for insertion before exit blocks.
		b.defers = append(b.defers, s)
		return current

	case *ast.GoStmt:
		current.Stmts = append(current.Stmts, s)
		current.Nodes = append(current.Nodes, s)
		return current

	case *ast.BlockStmt:
		return b.buildStmtList(current, s.List, exit)

	case *ast.BranchStmt:
		current.Stmts = append(current.Stmts, s)
		current.Nodes = append(current.Nodes, s)

		label := ""
		if s.Label != nil {
			label = s.Label.Name
		}

		switch s.Tok {
		case token.BREAK:
			if lc := b.findLoop(label); lc != nil {
				b.addEdge(current, lc.exit)
			}
			return nil // no fall-through after break

		case token.CONTINUE:
			if lc := b.findLoop(label); lc != nil && lc.header != nil {
				b.addEdge(current, lc.header)
			}
			return nil // no fall-through after continue

		case token.GOTO:
			if label != "" {
				if target, ok := b.labels[label]; ok {
					// Backward goto — label already seen.
					b.addEdge(current, target)
				} else {
					// Forward goto — record for later resolution.
					b.gotos[label] = append(b.gotos[label], current)
				}
			}
			return nil // no fall-through after goto

		case token.FALLTHROUGH:
			// Fallthrough in switch — handled by the block's normal successor.
			return current
		}
		return nil

	case *ast.LabeledStmt:
		// Register the label and process the underlying statement.
		lbl := s.Label.Name
		labelBlock := b.newBlock(BlockNormal)
		labelBlock.Pos = s.Pos()
		b.addEdge(current, labelBlock)
		b.labels[lbl] = labelBlock

		// Resolve any forward gotos to this label.
		if pending, ok := b.gotos[lbl]; ok {
			for _, src := range pending {
				b.addEdge(src, labelBlock)
			}
			delete(b.gotos, lbl)
		}

		// Set pending label so the next loop/switch/select picks it up.
		prevLabel := b.pendingLabel
		b.pendingLabel = lbl
		result := b.buildStmt(labelBlock, s.Stmt, exit)
		b.pendingLabel = prevLabel
		return result

	default:
		// Expression statements, assignments, declarations, etc.
		current.Stmts = append(current.Stmts, stmt)
		current.Nodes = append(current.Nodes, stmt)
		return current
	}
}

func (b *builder) buildIf(current *Block, s *ast.IfStmt, exit *Block) *Block {
	if s.Init != nil {
		current = b.buildStmt(current, s.Init, exit)
		if current == nil {
			return nil
		}
	}

	// Condition node.
	cond := b.newBlock(BlockBranch)
	cond.Nodes = append(cond.Nodes, s.Cond)
	cond.Pos = s.Pos()
	b.addEdge(current, cond)

	// Join point after the if.
	join := b.newBlock(BlockNormal)

	// Then branch.
	thenStart := b.newBlock(BlockNormal)
	b.addEdge(cond, thenStart)
	thenEnd := b.buildStmtList(thenStart, s.Body.List, exit)
	if thenEnd != nil {
		b.addEdge(thenEnd, join)
	}

	// Else branch.
	if s.Else != nil {
		elseStart := b.newBlock(BlockNormal)
		b.addEdge(cond, elseStart)
		elseEnd := b.buildStmt(elseStart, s.Else, exit)
		if elseEnd != nil {
			b.addEdge(elseEnd, join)
		}
	} else {
		b.addEdge(cond, join)
	}

	// If both branches return, join is unreachable.
	if len(join.Preds) == 0 {
		return nil
	}
	return join
}

func (b *builder) buildFor(current *Block, s *ast.ForStmt, exit *Block) *Block {
	if s.Init != nil {
		current = b.buildStmt(current, s.Init, exit)
		if current == nil {
			return nil
		}
	}

	header := b.newBlock(BlockBranch)
	b.addEdge(current, header)

	body := b.newBlock(BlockNormal)
	after := b.newBlock(BlockNormal)

	if s.Cond != nil {
		header.Nodes = append(header.Nodes, s.Cond)
		b.addEdge(header, body)  // condition true
		b.addEdge(header, after) // condition false
	} else {
		b.addEdge(header, body) // infinite loop
	}

	// Push loop context so break/continue can find their targets.
	// For continue, the target is the header (or post if present).
	continueTarget := header
	b.pushLoop(continueTarget, after, b.currentLabel())
	bodyEnd := b.buildStmtList(body, s.Body.List, exit)
	b.popLoop()

	if bodyEnd != nil {
		if s.Post != nil {
			bodyEnd = b.buildStmt(bodyEnd, s.Post, exit)
		}
		if bodyEnd != nil {
			b.addEdge(bodyEnd, header) // back edge
		}
	}

	return after
}

func (b *builder) buildRange(current *Block, s *ast.RangeStmt, exit *Block) *Block {
	header := b.newBlock(BlockBranch)
	header.Nodes = append(header.Nodes, s.X)
	b.addEdge(current, header)

	body := b.newBlock(BlockNormal)
	after := b.newBlock(BlockNormal)

	b.addEdge(header, body)
	b.addEdge(header, after) // range exhausted

	b.pushLoop(header, after, b.currentLabel())
	bodyEnd := b.buildStmtList(body, s.Body.List, exit)
	b.popLoop()

	if bodyEnd != nil {
		b.addEdge(bodyEnd, header)
	}

	return after
}

func (b *builder) buildSwitch(current *Block, s *ast.SwitchStmt, exit *Block) *Block {
	if s.Init != nil {
		current = b.buildStmt(current, s.Init, exit)
		if current == nil {
			return nil
		}
	}

	head := b.newBlock(BlockBranch)
	if s.Tag != nil {
		head.Nodes = append(head.Nodes, s.Tag)
	}
	b.addEdge(current, head)

	join := b.newBlock(BlockNormal)
	hasDefault := false

	// Switch supports break (exits to join), not continue.
	b.pushLoop(nil, join, b.currentLabel())
	for _, cc := range s.Body.List {
		clause := cc.(*ast.CaseClause)
		if clause.List == nil {
			hasDefault = true
		}
		caseBlock := b.newBlock(BlockNormal)
		b.addEdge(head, caseBlock)
		caseEnd := b.buildStmtList(caseBlock, clause.Body, exit)
		if caseEnd != nil {
			b.addEdge(caseEnd, join)
		}
	}
	b.popLoop()

	if !hasDefault {
		b.addEdge(head, join) // implicit default: fall through
	}

	if len(join.Preds) == 0 {
		return nil
	}
	return join
}

func (b *builder) buildTypeSwitch(current *Block, s *ast.TypeSwitchStmt, exit *Block) *Block {
	if s.Init != nil {
		current = b.buildStmt(current, s.Init, exit)
		if current == nil {
			return nil
		}
	}

	head := b.newBlock(BlockBranch)
	b.addEdge(current, head)

	join := b.newBlock(BlockNormal)
	hasDefault := false

	b.pushLoop(nil, join, b.currentLabel())
	for _, cc := range s.Body.List {
		clause := cc.(*ast.CaseClause)
		if clause.List == nil {
			hasDefault = true
		}
		caseBlock := b.newBlock(BlockNormal)
		b.addEdge(head, caseBlock)
		caseEnd := b.buildStmtList(caseBlock, clause.Body, exit)
		if caseEnd != nil {
			b.addEdge(caseEnd, join)
		}
	}
	b.popLoop()

	if !hasDefault {
		b.addEdge(head, join)
	}

	if len(join.Preds) == 0 {
		return nil
	}
	return join
}

func (b *builder) buildSelect(current *Block, s *ast.SelectStmt, exit *Block) *Block {
	head := b.newBlock(BlockBranch)
	b.addEdge(current, head)

	join := b.newBlock(BlockNormal)

	b.pushLoop(nil, join, b.currentLabel())
	for _, cc := range s.Body.List {
		clause := cc.(*ast.CommClause)
		caseBlock := b.newBlock(BlockNormal)
		b.addEdge(head, caseBlock)
		caseEnd := b.buildStmtList(caseBlock, clause.Body, exit)
		if caseEnd != nil {
			b.addEdge(caseEnd, join)
		}
	}
	b.popLoop()

	if len(join.Preds) == 0 {
		return nil
	}
	return join
}

// insertDeferChain intercepts edges leading to the exit block and inserts
// a chain of defer blocks (in LIFO order) before exit.
func (b *builder) insertDeferChain(exit *Block) {
	// Capture exit's current predecessors before modifying anything.
	preds := make([]*Block, len(exit.Preds))
	copy(preds, exit.Preds)
	exit.Preds = nil

	// Build a chain of defer blocks in LIFO order.
	// Chain: chainStart → ... → chainEnd → exit
	var deferBlocks []*Block
	for i := len(b.defers) - 1; i >= 0; i-- {
		db := b.newBlock(BlockDefer)
		db.Stmts = append(db.Stmts, b.defers[i])
		db.Nodes = append(db.Nodes, b.defers[i])
		deferBlocks = append(deferBlocks, db)
	}

	// Chain defer blocks together → exit.
	for i := 0; i < len(deferBlocks)-1; i++ {
		b.addEdge(deferBlocks[i], deferBlocks[i+1])
	}
	// Last defer block → exit.
	b.addEdge(deferBlocks[len(deferBlocks)-1], exit)

	chainStart := deferBlocks[0]

	// Redirect all original predecessors of exit to the chain start.
	for _, pred := range preds {
		for i, s := range pred.Succs {
			if s == exit {
				pred.Succs[i] = chainStart
				break
			}
		}
		chainStart.Preds = append(chainStart.Preds, pred)
	}
}

// currentLabel returns the label of the most recently entered LabeledStmt,
// if the current statement being built was wrapped in one. It checks the
// labels map for any label whose target block hasn't been fully processed yet.
// Since we set labels before building loop bodies, this works as expected.
func (b *builder) currentLabel() string {
	// The labeled statement handler sets the label before calling buildStmt
	// on the inner statement. We track this via a simple field.
	return b.pendingLabel
}
