// Copyright 2026 TrendVidia LLC
// SPDX-License-Identifier: MIT

package pxf

// Keyed repeated fields (draft-trendvidia-protowire-01 §3.13,
// trendvidia/protowire#116): a `repeated <Message>` field carrying the
// (pxf.key) option may be written as a block of named blocks — entry
// name = key-field value, entry order = list order. This file holds the
// schema-layer pieces shared by the decoder, the encoder, and tooling:
// the typed decode errors, the tolerant-diagnostics walker, and the
// schema-aware fmt canonicalizer. The descriptor helpers ([IsKeyed],
// [KeyField], [KeyFieldName]) live in annotations.go; the decode loop
// itself in decode_fast.go; keyed emission in encode.go.

import (
	"fmt"
	"sort"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// KeyedErrorKind classifies the keyed-repeated-field decode errors of
// draft -01 §3.13.
type KeyedErrorKind int

const (
	// KeyedDuplicateKey: two entries in the same keyed block whose
	// names are equal after unquoting.
	KeyedDuplicateKey KeyedErrorKind = iota + 1
	// KeyedKeyConflict: an explicit key-field assignment inside a named
	// entry that disagrees with the entry name.
	KeyedKeyConflict
	// KeyedEmptyKey: the empty string used as a key — a quoted entry
	// name "", or an explicit empty-string assignment to the key field
	// of an element of a keyed repeated field, in either surface form.
	KeyedEmptyKey
	// KeyedQuotedNameUnkeyed: a quoted entry name anywhere other than
	// inside a keyed repeated field's block. Grammatically well-formed;
	// rejected by the schema layer because a string entry name never
	// names a field.
	KeyedQuotedNameUnkeyed
)

func (k KeyedErrorKind) String() string {
	switch k {
	case KeyedDuplicateKey:
		return "duplicate key"
	case KeyedKeyConflict:
		return "key conflict"
	case KeyedEmptyKey:
		return "empty key"
	case KeyedQuotedNameUnkeyed:
		return "quoted entry name outside keyed field"
	default:
		return fmt.Sprintf("KeyedErrorKind(%d)", int(k))
	}
}

// KeyedError is a keyed-repeated-field decode error (draft -01 §3.13),
// returned by the Unmarshal family and surfaced as a diagnostic by
// [KeyedDiagnostics]. Distinguish causes with errors.As and Kind.
type KeyedError struct {
	Pos  Position
	Kind KeyedErrorKind
	// Field is the PXF name of the keyed repeated field involved.
	// Empty for KeyedQuotedNameUnkeyed, where no keyed field exists.
	Field string
	// Key is the offending key or entry name, when one applies.
	Key string
	Msg string
}

func (e *KeyedError) Error() string {
	return fmt.Sprintf("%s: %s", e.Pos, e.Msg)
}

func keyedEmptyNameError(pos Position, field protoreflect.Name) *KeyedError {
	return &KeyedError{Kind: KeyedEmptyKey, Pos: pos, Field: string(field),
		Msg: fmt.Sprintf("empty entry name in keyed field %q: the empty string is not a valid key", field)}
}

func keyedDuplicateKeyError(pos Position, field protoreflect.Name, key string) *KeyedError {
	return &KeyedError{Kind: KeyedDuplicateKey, Pos: pos, Field: string(field), Key: key,
		Msg: fmt.Sprintf("duplicate key %q in keyed field %q", key, field)}
}

func quotedNameUnkeyedError(pos Position, name string) *KeyedError {
	return &KeyedError{Kind: KeyedQuotedNameUnkeyed, Pos: pos, Key: name,
		Msg: fmt.Sprintf("quoted entry name %q is only valid inside a keyed repeated field's block (draft -01 §3.13)", name)}
}

// keyedElemState carries the key-field checks for the immediate body of
// one element of a keyed repeated field: an explicit assignment to the
// key field must not be empty, and in the named (keyed-block) form must
// agree with the entry name. Used by both the direct decoder and the
// diagnostics walker.
type keyedElemState struct {
	field     protoreflect.Name // the keyed repeated field's PXF name
	keyName   protoreflect.Name // the element message's key field name
	entryName string            // entry name; meaningful only when named
	named     bool              // keyed-block form (true) vs anonymous list element (false)
}

func (ke *keyedElemState) checkExplicitKey(value string, pos Position) *KeyedError {
	if value == "" {
		return &KeyedError{Kind: KeyedEmptyKey, Pos: pos, Field: string(ke.field),
			Msg: fmt.Sprintf("explicit empty-string assignment to key field %q of keyed field %q: the empty string is not a valid key", ke.keyName, ke.field)}
	}
	if ke.named && value != ke.entryName {
		return &KeyedError{Kind: KeyedKeyConflict, Pos: pos, Field: string(ke.field), Key: ke.entryName,
			Msg: fmt.Sprintf("key field %q = %q conflicts with entry name %q in keyed field %q", ke.keyName, value, ke.entryName, ke.field)}
	}
	return nil
}

// identSafeEntryName reports whether s can be written as an unquoted
// entry name: it matches the identifier production (ident-start
// followed by ident-part bytes, dots included) and is not one of the
// value keywords null / true / false. Canonical form emits unquoted
// names exactly when this holds (draft -01 §3.13).
func identSafeEntryName(s string) bool {
	if s == "" || s == "true" || s == "false" || s == "null" {
		return false
	}
	if !isIdentStart(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isIdentPart(s[i]) {
			return false
		}
	}
	return true
}

// KeyedDiagnostics walks a parsed document against desc and returns
// every keyed-repeated-field schema violation (draft -01 §3.13) as a
// positioned diagnostic, in source order: duplicate entry names within
// a keyed block, empty keys, explicit key-field assignments that
// disagree with their entry name, and quoted entry names outside a
// keyed repeated field's block.
//
// It is the tolerant counterpart to the hard errors the Unmarshal
// family returns: editor tooling (protolsp) runs [ParseTolerant], binds
// a schema later, and surfaces these as diagnostics instead of failing
// the parse. Entries that don't resolve against the schema (unknown
// fields, shape mismatches) are skipped — they are ordinary schema
// errors, not keyed diagnostics.
func KeyedDiagnostics(doc *Document, desc protoreflect.MessageDescriptor) []KeyedError {
	if doc == nil || desc == nil {
		return nil
	}
	var out []KeyedError
	diagEntries(doc.Entries, desc, nil, &out)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Pos.Offset < out[j].Pos.Offset
	})
	return out
}

func diagEntries(entries []Entry, desc protoreflect.MessageDescriptor, ke *keyedElemState, out *[]KeyedError) {
	fields := desc.Fields()
	for _, e := range entries {
		switch n := e.(type) {
		case *Assignment:
			if n.KeyQuoted {
				*out = append(*out, *quotedNameUnkeyedError(n.Pos, n.Key))
				continue
			}
			fd := fields.ByName(protoreflect.Name(n.Key))
			if fd == nil {
				continue
			}
			if ke != nil && fd.Name() == ke.keyName {
				if sv, ok := n.Value.(*StringVal); ok {
					if kerr := ke.checkExplicitKey(sv.Value, sv.Pos); kerr != nil {
						*out = append(*out, *kerr)
					}
				}
			}
			diagValue(n.Value, fd, out)
		case *Block:
			if n.NameQuoted {
				*out = append(*out, *quotedNameUnkeyedError(n.Pos, n.Name))
				continue
			}
			fd := fields.ByName(protoreflect.Name(n.Name))
			if fd == nil {
				continue
			}
			if keyFd := KeyField(fd); keyFd != nil {
				diagKeyedBlock(n.Entries, fd, keyFd, out)
				continue
			}
			if fd.IsList() && fd.Kind() == protoreflect.MessageKind {
				// Block form on a repeated field with no (pxf.key): the
				// shape itself is an ordinary schema error, but any
				// quoted entry names inside are the draft -01 §3.13
				// diagnostic — surface those.
				for _, be := range n.Entries {
					switch bn := be.(type) {
					case *Assignment:
						if bn.KeyQuoted {
							*out = append(*out, *quotedNameUnkeyedError(bn.Pos, bn.Key))
						}
					case *Block:
						if bn.NameQuoted {
							*out = append(*out, *quotedNameUnkeyedError(bn.Pos, bn.Name))
						}
					}
				}
				continue
			}
			if fd.Kind() == protoreflect.MessageKind && !fd.IsList() && !fd.IsMap() {
				diagEntries(n.Entries, fd.Message(), nil, out)
			}
		}
	}
}

// diagValue descends into an assignment's value per fd's shape: keyed
// blocks, anonymous list elements, singular submessages, and message-
// valued map entries.
func diagValue(v Value, fd protoreflect.FieldDescriptor, out *[]KeyedError) {
	if fd.IsMap() {
		bv, ok := v.(*BlockVal)
		if !ok || fd.MapValue().Kind() != protoreflect.MessageKind {
			return
		}
		for _, e := range bv.Entries {
			me, ok := e.(*MapEntry)
			if !ok {
				continue
			}
			if inner, ok := me.Value.(*BlockVal); ok {
				diagEntries(inner.Entries, fd.MapValue().Message(), nil, out)
			}
		}
		return
	}
	if fd.IsList() {
		if fd.Kind() != protoreflect.MessageKind {
			return
		}
		keyFd := KeyField(fd)
		switch val := v.(type) {
		case *ListVal:
			var elemState *keyedElemState
			if keyFd != nil {
				elemState = &keyedElemState{field: fd.Name(), keyName: keyFd.Name()}
			}
			for _, elem := range val.Elements {
				if bv, ok := elem.(*BlockVal); ok {
					diagEntries(bv.Entries, fd.Message(), elemState, out)
				}
			}
		case *BlockVal:
			// `name = { ... }` spelling of the keyed block form.
			if keyFd != nil {
				diagKeyedBlock(val.Entries, fd, keyFd, out)
			}
		}
		return
	}
	if fd.Kind() == protoreflect.MessageKind {
		if bv, ok := v.(*BlockVal); ok {
			diagEntries(bv.Entries, fd.Message(), nil, out)
		}
	}
}

// diagKeyedBlock checks the entries of one keyed block: empty and
// duplicate names (compared by unquoted value), then each entry's body
// with the key-field context.
func diagKeyedBlock(entries []Entry, fd, keyFd protoreflect.FieldDescriptor, out *[]KeyedError) {
	seen := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		var name string
		var pos Position
		var body []Entry
		switch n := e.(type) {
		case *Block:
			name, pos, body = n.Name, n.Pos, n.Entries
		case *Assignment:
			bv, ok := n.Value.(*BlockVal)
			if !ok {
				// A non-block value on a named entry is an ordinary
				// decode error (the element type is a message), not a
				// keyed diagnostic.
				continue
			}
			name, pos, body = n.Key, n.Pos, bv.Entries
		default:
			continue
		}
		switch {
		case name == "":
			*out = append(*out, *keyedEmptyNameError(pos, fd.Name()))
		default:
			if _, dup := seen[name]; dup {
				*out = append(*out, *keyedDuplicateKeyError(pos, fd.Name(), name))
			}
			seen[name] = struct{}{}
		}
		diagEntries(body, fd.Message(), &keyedElemState{
			field: fd.Name(), keyName: keyFd.Name(), entryName: name, named: true,
		}, out)
	}
}

// CanonicalizeKeyed rewrites doc in place to the canonical keyed form
// of draft -01 §3.13, using desc as the document's message schema. Per
// keyed repeated field binding it:
//
//   - converts an eligible anonymous list binding (every element with
//     exactly one non-empty, distinct explicit key assignment) to the
//     keyed block form, removing the now-implicit key assignments;
//   - normalizes `name = { ... }` entry spellings to `name { ... }`;
//   - unquotes quoted entry names that are identifier-safe;
//   - drops redundant (agreeing) explicit key-field assignments inside
//     named entries.
//
// Bindings that are not eligible for the keyed form — duplicate keys,
// absent or empty keys — are left in the anonymous form, and entries
// that don't resolve against the schema are left untouched, so
// formatting an invalid document never destroys information. Callers
// typically follow with [FormatDocument]; this pair is the reference
// `pxf fmt` pipeline.
func CanonicalizeKeyed(doc *Document, desc protoreflect.MessageDescriptor) {
	if doc == nil || desc == nil {
		return
	}
	doc.Entries = canonEntries(doc.Entries, desc)
}

func canonEntries(entries []Entry, desc protoreflect.MessageDescriptor) []Entry {
	fields := desc.Fields()
	for i, e := range entries {
		switch n := e.(type) {
		case *Assignment:
			if n.KeyQuoted {
				continue // invalid outside keyed blocks; leave untouched
			}
			fd := fields.ByName(protoreflect.Name(n.Key))
			if fd == nil {
				continue
			}
			entries[i] = canonAssignment(n, fd)
		case *Block:
			if n.NameQuoted {
				continue
			}
			fd := fields.ByName(protoreflect.Name(n.Name))
			if fd == nil {
				continue
			}
			if keyFd := KeyField(fd); keyFd != nil {
				canonKeyedEntries(n, fd, keyFd)
				continue
			}
			if fd.Kind() == protoreflect.MessageKind && !fd.IsList() && !fd.IsMap() {
				n.Entries = canonEntries(n.Entries, fd.Message())
			}
		}
	}
	return entries
}

func canonAssignment(n *Assignment, fd protoreflect.FieldDescriptor) Entry {
	if fd.IsMap() {
		if bv, ok := n.Value.(*BlockVal); ok && fd.MapValue().Kind() == protoreflect.MessageKind {
			for _, e := range bv.Entries {
				if me, ok := e.(*MapEntry); ok {
					if inner, ok := me.Value.(*BlockVal); ok {
						inner.Entries = canonEntries(inner.Entries, fd.MapValue().Message())
					}
				}
			}
		}
		return n
	}
	if fd.IsList() {
		if fd.Kind() != protoreflect.MessageKind {
			return n
		}
		keyFd := KeyField(fd)
		switch val := n.Value.(type) {
		case *ListVal:
			if keyFd != nil {
				return canonAnonymousKeyed(n, val, fd, keyFd)
			}
			for _, elem := range val.Elements {
				if bv, ok := elem.(*BlockVal); ok {
					bv.Entries = canonEntries(bv.Entries, fd.Message())
				}
			}
		case *BlockVal:
			if keyFd != nil {
				// `children = { ... }` → `children { ... }`.
				blk := &Block{Pos: n.Pos, End: n.End, Name: n.Key, Entries: val.Entries, LeadingComments: n.LeadingComments}
				canonKeyedEntries(blk, fd, keyFd)
				return blk
			}
		}
		return n
	}
	if fd.Kind() == protoreflect.MessageKind {
		if bv, ok := n.Value.(*BlockVal); ok {
			bv.Entries = canonEntries(bv.Entries, fd.Message())
		}
	}
	return n
}

// canonKeyedEntries normalizes the entries of a keyed block in place:
// assignment-spelled entries become blocks, identifier-safe quoted
// names are unquoted, redundant agreeing key assignments are dropped,
// and entry bodies are canonicalized recursively.
func canonKeyedEntries(b *Block, fd, keyFd protoreflect.FieldDescriptor) {
	keyName := string(keyFd.Name())
	for i, e := range b.Entries {
		var eb *Block
		switch n := e.(type) {
		case *Block:
			eb = n
		case *Assignment:
			if bv, ok := n.Value.(*BlockVal); ok {
				eb = &Block{Pos: n.Pos, End: n.End, Name: n.Key, NameQuoted: n.KeyQuoted,
					Entries: bv.Entries, LeadingComments: n.LeadingComments}
				b.Entries[i] = eb
			}
		}
		if eb == nil {
			continue // malformed entry; leave untouched
		}
		if eb.NameQuoted && identSafeEntryName(eb.Name) {
			eb.NameQuoted = false
		}
		eb.Entries = dropKeyAssignments(eb.Entries, keyName, eb.Name)
		eb.Entries = canonEntries(eb.Entries, fd.Message())
	}
}

// dropKeyAssignments removes `keyName = "entryName"` assignments — the
// redundant agreeing spelling of an entry's key — from entries.
// Disagreeing or non-string assignments are kept (the document is
// invalid; formatting must not silently change its meaning). Leading
// comments of a dropped assignment move to the next surviving entry.
func dropKeyAssignments(entries []Entry, keyName, entryName string) []Entry {
	var pending []Comment
	out := entries[:0]
	for _, e := range entries {
		if a, ok := e.(*Assignment); ok && !a.KeyQuoted && a.Key == keyName {
			if sv, ok := a.Value.(*StringVal); ok && sv.Value == entryName {
				pending = append(pending, a.LeadingComments...)
				continue
			}
		}
		if len(pending) > 0 {
			switch n := e.(type) {
			case *Assignment:
				n.LeadingComments = append(pending, n.LeadingComments...)
			case *Block:
				n.LeadingComments = append(pending, n.LeadingComments...)
			case *MapEntry:
				n.LeadingComments = append(pending, n.LeadingComments...)
			}
			pending = nil
		}
		out = append(out, e)
	}
	return out
}

// canonAnonymousKeyed converts an eligible anonymous list binding of a
// keyed repeated field to the keyed block form. Ineligible bindings
// (non-block elements, absent / empty / duplicate / non-string keys)
// stay anonymous; their element bodies are still canonicalized.
func canonAnonymousKeyed(n *Assignment, lv *ListVal, fd, keyFd protoreflect.FieldDescriptor) Entry {
	keyName := string(keyFd.Name())
	type namedElem struct {
		key string
		bv  *BlockVal
	}
	elems := make([]namedElem, 0, len(lv.Elements))
	seen := make(map[string]struct{}, len(lv.Elements))
	eligible := true
	for _, v := range lv.Elements {
		bv, ok := v.(*BlockVal)
		if !ok {
			eligible = false
			break
		}
		key, ok := explicitKeyOf(bv.Entries, keyName)
		if !ok || key == "" {
			eligible = false
			break
		}
		if _, dup := seen[key]; dup {
			eligible = false
			break
		}
		seen[key] = struct{}{}
		elems = append(elems, namedElem{key, bv})
	}
	if !eligible {
		for _, v := range lv.Elements {
			if bv, ok := v.(*BlockVal); ok {
				bv.Entries = canonEntries(bv.Entries, fd.Message())
			}
		}
		return n
	}
	blk := &Block{Pos: n.Pos, End: n.End, Name: n.Key, LeadingComments: n.LeadingComments}
	for _, el := range elems {
		body := dropKeyAssignments(el.bv.Entries, keyName, el.key)
		body = canonEntries(body, fd.Message())
		blk.Entries = append(blk.Entries, &Block{
			Pos:        el.bv.Pos,
			End:        el.bv.End,
			Name:       el.key,
			NameQuoted: !identSafeEntryName(el.key),
			Entries:    body,
		})
	}
	return blk
}

// explicitKeyOf returns the value of the single explicit string
// assignment to keyName among entries. ok is false when there is no
// such assignment, more than one, a quoted-key spelling, or a
// non-string value.
func explicitKeyOf(entries []Entry, keyName string) (key string, ok bool) {
	found := false
	for _, e := range entries {
		a, isAssign := e.(*Assignment)
		if !isAssign || a.Key != keyName {
			continue
		}
		sv, isStr := a.Value.(*StringVal)
		if !isStr || a.KeyQuoted || found {
			return "", false
		}
		key = sv.Value
		found = true
	}
	return key, found
}
