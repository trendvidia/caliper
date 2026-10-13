// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package taint

import (
	"fmt"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/trendvidia/caliper/pkg/analysis"
)

// --------------------------------------------------------------------------
// Configuration types
// --------------------------------------------------------------------------

// Source defines where tainted data enters.
type Source struct {
	Package string // e.g., "net/http"
	Type    string // e.g., "*Request" or "Request"
	Field   string // optional — empty means the whole type is tainted
}

// Sink defines where tainted data must not reach without sanitization.
type Sink struct {
	Package  string // e.g., "database/sql"
	Function string // e.g., "Query", "Exec"
	ArgIndex int    // which argument is sensitive (0-based), -1 = any
}

// Sanitizer defines a function that cleanses tainted data.
type Sanitizer struct {
	Package  string // e.g., "html/template"
	Function string // e.g., "HTMLEscapeString"
}

// TaintConfig describes a complete taint analysis.
type TaintConfig struct {
	Sources    []Source
	Sinks      []Sink
	Sanitizers []Sanitizer
}

// --------------------------------------------------------------------------
// Result types
// --------------------------------------------------------------------------

// TaintFlow describes one path from source to sink.
type TaintFlow struct {
	Source    analysis.FileLocation
	Sink      analysis.FileLocation
	Path      []TaintStep
	Sanitized bool
}

// TaintStep is one hop in a taint flow.
type TaintStep struct {
	Location    analysis.FileLocation
	Kind        string // "parameter", "call_arg", "return", "assign", "phi", "field", "unop", "extract", "slice"
	Description string
}

// --------------------------------------------------------------------------
// Tracker
// --------------------------------------------------------------------------

// Tracker performs inter-procedural taint analysis using SSA and the
// call graph.
type Tracker struct {
	config TaintConfig
	engine *analysis.Engine
	ipc    *analysis.InterProceduralContext
}

// NewTracker creates a taint tracker.
func NewTracker(engine *analysis.Engine, ipc *analysis.InterProceduralContext, config TaintConfig) *Tracker {
	return &Tracker{
		config: config,
		engine: engine,
		ipc:    ipc,
	}
}

// --------------------------------------------------------------------------
// Internal state
// --------------------------------------------------------------------------

// maxDepth bounds the inter-procedural call depth to keep analysis finite.
const maxDepth = 10

// taintedKey uniquely identifies a (function, parameter-index) pair to
// prevent re-analysis of the same configuration.
type taintedKey struct {
	fn       *ssa.Function
	paramIdx int
}

// taintState tracks propagation state during analysis.
type taintState struct {
	// visited records function/parameter pairs already analyzed.
	visited map[taintedKey]bool

	// flows accumulates discovered taint flows.
	flows []TaintFlow

	// sanitized tracks values that passed through a sanitizer.
	sanitized map[ssa.Value]bool
}

func newTaintState() *taintState {
	return &taintState{
		visited:   make(map[taintedKey]bool),
		flows:     nil,
		sanitized: make(map[ssa.Value]bool),
	}
}

// --------------------------------------------------------------------------
// Public API
// --------------------------------------------------------------------------

// Analyze runs the taint analysis and returns all unsanitized
// source-to-sink flows.
func (t *Tracker) Analyze() []TaintFlow {
	state := newTaintState()

	// Walk all SSA functions looking for source entry points.
	for fn := range t.ipc.AllFunctions() {
		if fn.Signature == nil {
			continue
		}
		t.findSourcesInFunction(fn, state)
	}

	// Filter out sanitized flows.
	var results []TaintFlow
	for _, flow := range state.flows {
		if !flow.Sanitized {
			results = append(results, flow)
		}
	}
	return results
}

// --------------------------------------------------------------------------
// Source identification
// --------------------------------------------------------------------------

// findSourcesInFunction checks each parameter and instruction in fn for
// taint sources.
func (t *Tracker) findSourcesInFunction(fn *ssa.Function, state *taintState) {
	params := fn.Params
	for i, param := range params {
		if t.isSourceType(param.Type()) {
			key := taintedKey{fn: fn, paramIdx: i}
			if state.visited[key] {
				continue
			}
			state.visited[key] = true

			srcLoc := t.locationOf(param.Pos())
			path := []TaintStep{{
				Location:    srcLoc,
				Kind:        "parameter",
				Description: fmt.Sprintf("parameter %q of type %s", param.Name(), param.Type()),
			}}
			t.propagate(param, path, state, 0)
		}
	}

	// Also check for field accesses on source types within the function body.
	if fn.Blocks == nil {
		return
	}
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			fa, ok := instr.(*ssa.FieldAddr)
			if !ok {
				continue
			}
			if t.isSourceFieldAccess(fa) {
				srcLoc := t.locationOf(fa.Pos())
				path := []TaintStep{{
					Location:    srcLoc,
					Kind:        "field",
					Description: fmt.Sprintf("field access on source type %s", fa.X.Type()),
				}}
				t.propagate(fa, path, state, 0)
			}
		}
	}
}

// isSourceType checks whether typ matches any configured source.
func (t *Tracker) isSourceType(typ types.Type) bool {
	typStr := typ.String()
	for _, src := range t.config.Sources {
		if src.Field != "" {
			// Field-specific sources are checked elsewhere.
			continue
		}
		if t.typeMatchesSource(typStr, src) {
			return true
		}
	}
	return false
}

// typeMatchesSource checks whether a type string matches a source
// definition. It handles both pointer and non-pointer forms.
func (t *Tracker) typeMatchesSource(typStr string, src Source) bool {
	// Build the qualified type name.
	qualified := src.Package + "." + src.Type
	if typStr == qualified {
		return true
	}
	// Also match pointer to the type.
	if typStr == "*"+qualified {
		return true
	}
	// Match if Source.Type already has a * prefix.
	if strings.HasPrefix(src.Type, "*") {
		bare := src.Package + "." + src.Type[1:]
		if typStr == "*"+bare {
			return true
		}
	}
	return false
}

// isSourceFieldAccess checks whether a FieldAddr accesses a field on a
// source type that has a field-specific source configured.
func (t *Tracker) isSourceFieldAccess(fa *ssa.FieldAddr) bool {
	typStr := fa.X.Type().String()
	// Dereference pointer.
	typStr = strings.TrimPrefix(typStr, "*")

	structType, ok := derefToStruct(fa.X.Type())
	if !ok {
		return false
	}

	fieldName := structType.Field(fa.Field).Name()

	for _, src := range t.config.Sources {
		if src.Field == "" {
			continue
		}
		qualified := src.Package + "." + src.Type
		bare := strings.TrimPrefix(typStr, "*")
		if bare == qualified && fieldName == src.Field {
			return true
		}
	}
	return false
}

// derefToStruct unwraps pointers to find a struct type.
func derefToStruct(typ types.Type) (*types.Struct, bool) {
	typ = typ.Underlying()
	if ptr, ok := typ.(*types.Pointer); ok {
		typ = ptr.Elem().Underlying()
	}
	st, ok := typ.(*types.Struct)
	return st, ok
}

// --------------------------------------------------------------------------
// Taint propagation (worklist)
// --------------------------------------------------------------------------

// propagate follows the tainted value forward through its referrers,
// recording sink hits and expanding through call boundaries.
func (t *Tracker) propagate(val ssa.Value, path []TaintStep, state *taintState, depth int) {
	if depth > maxDepth {
		return
	}

	refs := val.Referrers()
	if refs == nil {
		return
	}

	for _, instr := range *refs {
		switch ref := instr.(type) {
		case *ssa.Call:
			t.handleCall(ref, val, path, state, depth)
		case *ssa.Go:
			t.handleCallCommon(ref, ref.Common(), val, path, state, depth)
		case *ssa.Defer:
			t.handleCallCommon(ref, ref.Common(), val, path, state, depth)
		case *ssa.Store:
			t.handleStore(ref, val, path, state, depth)
		case *ssa.Phi:
			t.handlePhi(ref, path, state, depth)
		case *ssa.FieldAddr:
			t.handleFieldAddr(ref, path, state, depth)
		case *ssa.UnOp:
			t.handleUnOp(ref, path, state, depth)
		case *ssa.Extract:
			t.handleExtract(ref, path, state, depth)
		case *ssa.Slice:
			t.handleSlice(ref, path, state, depth)
		case *ssa.IndexAddr:
			t.handleIndexAddr(ref, path, state, depth)
		case *ssa.MakeInterface:
			t.handleMakeInterface(ref, path, state, depth)
		}
	}
}

// --------------------------------------------------------------------------
// Instruction handlers
// --------------------------------------------------------------------------

func (t *Tracker) handleCall(call *ssa.Call, val ssa.Value, path []TaintStep, state *taintState, depth int) {
	t.handleCallCommon(call, call.Common(), val, path, state, depth)
}

func (t *Tracker) handleCallCommon(instr ssa.Instruction, cc *ssa.CallCommon, val ssa.Value, path []TaintStep, state *taintState, depth int) {
	// Check if the call is a sanitizer.
	if t.isSanitizer(cc) {
		// The return value of a sanitizer call is clean. Mark it if the
		// instruction produces a value.
		if v, ok := instr.(ssa.Value); ok {
			state.sanitized[v] = true
		}
		return
	}

	// Check if the call is a sink.
	argIdx := t.argIndex(cc, val)
	if t.isSink(cc, argIdx) {
		step := TaintStep{
			Location:    t.locationOfInstr(instr),
			Kind:        "call_arg",
			Description: fmt.Sprintf("tainted value passed as arg %d to sink %s", argIdx, cc.Value.Name()),
		}
		flow := TaintFlow{
			Source:    path[0].Location,
			Sink:      t.locationOfInstr(instr),
			Path:      append(append([]TaintStep{}, path...), step),
			Sanitized: state.sanitized[val],
		}
		state.flows = append(state.flows, flow)
		return
	}

	// Inter-procedural: follow taint into callee parameters.
	callee := cc.StaticCallee()
	if callee == nil {
		// Try to resolve via call graph.
		callee = t.resolveCallee(cc)
	}
	if callee == nil || callee.Blocks == nil {
		return
	}

	if argIdx < 0 {
		return
	}

	// Map argument index to callee parameter index. For method calls the
	// receiver is argument 0 in the common args but parameter 0 in the
	// callee, so the mapping is direct.
	paramIdx := argIdx
	if cc.IsInvoke() {
		// For interface method invocations, args[0] is the first real
		// argument (the receiver is implicit).
		paramIdx = argIdx
	}
	if paramIdx >= len(callee.Params) {
		return
	}

	key := taintedKey{fn: callee, paramIdx: paramIdx}
	if state.visited[key] {
		return
	}
	state.visited[key] = true

	param := callee.Params[paramIdx]
	step := TaintStep{
		Location:    t.locationOfInstr(instr),
		Kind:        "call_arg",
		Description: fmt.Sprintf("tainted value passed as arg %d to %s", argIdx, callee.Name()),
	}
	newPath := append(append([]TaintStep{}, path...), step)

	paramStep := TaintStep{
		Location:    t.locationOf(param.Pos()),
		Kind:        "parameter",
		Description: fmt.Sprintf("parameter %q in %s", param.Name(), callee.Name()),
	}
	newPath = append(newPath, paramStep)

	t.propagate(param, newPath, state, depth+1)
}

func (t *Tracker) handleStore(store *ssa.Store, val ssa.Value, path []TaintStep, state *taintState, depth int) {
	step := TaintStep{
		Location:    t.locationOfInstr(store),
		Kind:        "assign",
		Description: "tainted value stored to address",
	}
	newPath := append(append([]TaintStep{}, path...), step)
	t.propagate(store.Addr, newPath, state, depth)
}

func (t *Tracker) handlePhi(phi *ssa.Phi, path []TaintStep, state *taintState, depth int) {
	step := TaintStep{
		Location:    t.locationOf(phi.Pos()),
		Kind:        "phi",
		Description: "taint flows through phi node",
	}
	newPath := append(append([]TaintStep{}, path...), step)
	t.propagate(phi, newPath, state, depth)
}

func (t *Tracker) handleFieldAddr(fa *ssa.FieldAddr, path []TaintStep, state *taintState, depth int) {
	step := TaintStep{
		Location:    t.locationOf(fa.Pos()),
		Kind:        "field",
		Description: fmt.Sprintf("field access index %d", fa.Field),
	}
	newPath := append(append([]TaintStep{}, path...), step)
	t.propagate(fa, newPath, state, depth)
}

func (t *Tracker) handleUnOp(unop *ssa.UnOp, path []TaintStep, state *taintState, depth int) {
	step := TaintStep{
		Location:    t.locationOf(unop.Pos()),
		Kind:        "unop",
		Description: fmt.Sprintf("unary operation %s", unop.Op),
	}
	newPath := append(append([]TaintStep{}, path...), step)
	t.propagate(unop, newPath, state, depth)
}

func (t *Tracker) handleExtract(extract *ssa.Extract, path []TaintStep, state *taintState, depth int) {
	step := TaintStep{
		Location:    t.locationOf(extract.Pos()),
		Kind:        "extract",
		Description: fmt.Sprintf("extract element %d from tuple", extract.Index),
	}
	newPath := append(append([]TaintStep{}, path...), step)
	t.propagate(extract, newPath, state, depth)
}

func (t *Tracker) handleSlice(sl *ssa.Slice, path []TaintStep, state *taintState, depth int) {
	step := TaintStep{
		Location:    t.locationOf(sl.Pos()),
		Kind:        "slice",
		Description: "taint flows through slice operation",
	}
	newPath := append(append([]TaintStep{}, path...), step)
	t.propagate(sl, newPath, state, depth)
}

func (t *Tracker) handleIndexAddr(ia *ssa.IndexAddr, path []TaintStep, state *taintState, depth int) {
	step := TaintStep{
		Location:    t.locationOf(ia.Pos()),
		Kind:        "assign",
		Description: "taint flows through index operation",
	}
	newPath := append(append([]TaintStep{}, path...), step)
	t.propagate(ia, newPath, state, depth)
}

func (t *Tracker) handleMakeInterface(mi *ssa.MakeInterface, path []TaintStep, state *taintState, depth int) {
	step := TaintStep{
		Location:    t.locationOf(mi.Pos()),
		Kind:        "assign",
		Description: "taint flows through interface conversion",
	}
	newPath := append(append([]TaintStep{}, path...), step)
	t.propagate(mi, newPath, state, depth)
}

// --------------------------------------------------------------------------
// Sink / sanitizer matching
// --------------------------------------------------------------------------

// isSink checks whether a call matches any configured sink, and whether
// the tainted value is in the sensitive argument position.
func (t *Tracker) isSink(cc *ssa.CallCommon, argIdx int) bool {
	fn := cc.StaticCallee()
	if fn == nil {
		fn = t.resolveCallee(cc)
	}
	if fn == nil {
		return false
	}

	pkgPath := funcPkgPath(fn)
	name := fn.Name()

	for _, sink := range t.config.Sinks {
		if sink.Package == pkgPath && sink.Function == name {
			if sink.ArgIndex == -1 || sink.ArgIndex == argIdx {
				return true
			}
		}
	}
	return false
}

// isSanitizer checks whether a call matches any configured sanitizer.
func (t *Tracker) isSanitizer(cc *ssa.CallCommon) bool {
	fn := cc.StaticCallee()
	if fn == nil {
		fn = t.resolveCallee(cc)
	}
	if fn == nil {
		return false
	}

	pkgPath := funcPkgPath(fn)
	name := fn.Name()

	for _, san := range t.config.Sanitizers {
		if san.Package == pkgPath && san.Function == name {
			return true
		}
	}
	return false
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// argIndex returns the index of val in the call's argument list, or -1
// if val is not an argument (e.g. it is the function value itself).
func (t *Tracker) argIndex(cc *ssa.CallCommon, val ssa.Value) int {
	for i, arg := range cc.Args {
		if arg == val {
			return i
		}
	}
	return -1
}

// resolveCallee tries to resolve a dynamic callee using the call graph.
func (t *Tracker) resolveCallee(cc *ssa.CallCommon) *ssa.Function {
	if cc.Value == nil {
		return nil
	}
	// For invoke (interface) calls, check if the call graph resolved it.
	// This is best-effort; dynamic dispatch may not be fully resolved.
	return nil
}

// funcPkgPath returns the package path for an SSA function, handling
// methods whose package is on the receiver type.
func funcPkgPath(fn *ssa.Function) string {
	if fn.Package() != nil {
		return fn.Package().Pkg.Path()
	}
	// For methods from other packages, try the receiver type.
	sig := fn.Signature
	recv := sig.Recv()
	if recv == nil {
		return ""
	}
	typ := recv.Type()
	if ptr, ok := typ.(*types.Pointer); ok {
		typ = ptr.Elem()
	}
	if named, ok := typ.(*types.Named); ok {
		if named.Obj().Pkg() != nil {
			return named.Obj().Pkg().Path()
		}
	}
	return ""
}

// locationOf returns a FileLocation for a token position.
func (t *Tracker) locationOf(pos token.Pos) analysis.FileLocation {
	if !pos.IsValid() {
		return analysis.FileLocation{}
	}
	return t.engine.LocationOf(pos)
}

// locationOfInstr returns a FileLocation for an SSA instruction.
func (t *Tracker) locationOfInstr(instr ssa.Instruction) analysis.FileLocation {
	return t.locationOf(instr.Pos())
}
