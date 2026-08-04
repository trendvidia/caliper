package analysis

import (
	"go/types"
)

// Cross-method analyses (paired lifecycle calls, capture-vs-re-read)
// operate on a type's whole method set rather than one function at a
// time. This file provides the receiver-type index they share.

// ReceiverTypeName resolves the named type of a method's receiver,
// dereferencing pointers and unwrapping aliases. Returns nil for plain
// functions and for methods on unresolvable receivers.
func ReceiverTypeName(fc *FuncContext) *types.TypeName {
	if fc.Func.Recv == nil || len(fc.Func.Recv.List) == 0 {
		return nil
	}
	fn, ok := fc.Info.Defs[fc.Func.Name].(*types.Func)
	if !ok {
		return nil
	}
	recv := fn.Signature().Recv()
	if recv == nil {
		return nil
	}
	return NamedTypeOf(recv.Type())
}

// NamedTypeOf resolves a type to its named type's TypeName, unwrapping
// aliases and one level of pointer indirection. Returns nil for
// unnamed types (slices, maps, funcs, anonymous structs, ...).
func NamedTypeOf(t types.Type) *types.TypeName {
	if t == nil {
		return nil
	}
	t = types.Unalias(t)
	if ptr, ok := t.(*types.Pointer); ok {
		t = types.Unalias(ptr.Elem())
	}
	named, ok := t.(*types.Named)
	if !ok {
		return nil
	}
	return named.Obj()
}

// MethodSets indexes all method declarations across loaded packages by
// their receiver's named type. Plain functions are excluded.
func (e *Engine) MethodSets() map[*types.TypeName][]*FuncContext {
	sets := make(map[*types.TypeName][]*FuncContext)
	for _, fc := range e.FuncNodes() {
		tn := ReceiverTypeName(fc)
		if tn == nil {
			continue
		}
		sets[tn] = append(sets[tn], fc)
	}
	return sets
}
