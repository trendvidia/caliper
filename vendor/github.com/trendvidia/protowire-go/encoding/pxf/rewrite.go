// Copyright 2026 TrendVidia LLC
// SPDX-License-Identifier: MIT

package pxf

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

// Rewriter applies targeted, format-preserving edits to a PXF
// document. Unlike [FormatDocument], which normalizes the whole
// document, a Rewriter splices replacement bytes into the original
// source, so everything outside the edited spans — comments, blank
// lines, key ordering, indentation, number and string formatting
// quirks — round-trips byte-for-byte. This is the write path for
// tools that machine-edit documents a human also hand-edits (editor
// settings writers, property panels, manifest updaters).
//
//	r, err := pxf.NewRewriter(src)
//	r.Set("server.port", &pxf.IntVal{Raw: "9090"})
//	r.Remove("server.debug")
//	out, err := r.Bytes()
//
// Fields are addressed by dotted path; each segment names an
// [Assignment] key, [Block] name, or [MapEntry] key in the enclosing
// scope, and the first match wins. Keys that themselves contain a
// dot cannot be addressed. Edits are computed against the document
// as originally parsed: staging an edit does not make new fields
// addressable by later calls, and two Sets that both create the same
// missing intermediate block will create it twice (stage one Set with
// a [BlockVal] value instead).
//
// Keyed collections (draft -01 §3.13) have their own editors —
// [Rewriter.SetKeyed], [Rewriter.RemoveKeyed],
// [Rewriter.InsertKeyedElement], [Rewriter.RemoveKeyedElement],
// [Rewriter.RenameKeyedElement], [Rewriter.MoveKeyedElement] — that
// take the element key as an opaque atom, since entry names may
// contain dots and need not be identifier-shaped. They cover a field
// bound more than once (§3.13 concatenation) by searching every
// binding, and the anonymous list form (`f = [ { k = "x" } ]`) once
// its key field is named with [Rewriter.KeyedByField]; the keyed-block
// form needs no declaration.
//
// A Rewriter is not safe for concurrent use.
type Rewriter struct {
	src   []byte
	doc   *Document
	edits []spanEdit
	// insertSeq gives each [Rewriter.AppendEntry] a distinct edit
	// identity so repeated appends into one block accumulate rather
	// than deduplicating (as same-path Sets do).
	insertSeq int
	// keyFields records the (pxf.key) field name declared for a keyed
	// collection at a dotted fieldPath via [Rewriter.KeyedByField]. It
	// lets the keyed editors address a collection written in the
	// anonymous list form, where the key is the value of the key field
	// rather than an entry name. nil until the first declaration.
	keyFields map[string]string
}

// spanEdit replaces src[start:end) with text. start == end is a pure
// insertion; text == nil is a pure deletion.
type spanEdit struct {
	path       string
	start, end int
	text       []byte
}

// NewRewriter parses src (strictly, via [Parse]) and returns a
// Rewriter over it. The source must be valid PXF: rewriting is a
// writer's concern, and a broken document has no stable spans to
// splice into — use [ParseTolerant] for read-side tooling on broken
// buffers.
func NewRewriter(src []byte) (*Rewriter, error) {
	doc, err := Parse(src)
	if err != nil {
		return nil, err
	}
	return &Rewriter{src: src, doc: doc}, nil
}

// Document returns the parsed document the Rewriter addresses paths
// against. It reflects the original source, not staged edits.
func (r *Rewriter) Document() *Document { return r.doc }

// Set stages an upsert of the field at path to the given value. If
// the path resolves to an existing [Assignment] or [MapEntry], only
// that entry's value span is replaced — the key, separator, alignment,
// and any trailing comment stay put. If the path (or a suffix of it)
// does not exist, the missing chain is inserted at the end of the
// deepest existing enclosing block, matching its sibling indentation.
//
// Setting a path that resolves to a [Block] is an error: replacing a
// block wholesale would discard the comments inside it. Set its leaf
// fields individually, or Remove it and Set a [BlockVal].
//
// A Rewriter works on syntax alone — it has no schema. An inserted
// leaf uses the ':' map form when an existing sibling is a map entry
// and the '=' field form otherwise, so inserting the first entry of a
// map into an empty block writes '=' where the schema may expect ':';
// prefer seeding empty map blocks with a [BlockVal] of [MapEntry]
// values.
//
// Calling Set again with the same path replaces the previously staged
// edit for that path.
func (r *Rewriter) Set(path string, v Value) error {
	if v == nil {
		return fmt.Errorf("pxf: Set %s: nil value", path)
	}
	if containsBadVal(v) {
		return fmt.Errorf("pxf: Set %s: cannot write a BadVal placeholder", path)
	}
	t, err := r.resolve(path)
	if err != nil {
		return err
	}
	return r.setResolved("Set "+path, path, t, v)
}

// setResolved stages the value edit for a resolved target — replacing
// an existing entry's value span, or inserting the missing chain. op is
// the display prefix for error messages ("Set <path>"); pathKey is the
// staging identity (same-key edits replace each other).
func (r *Rewriter) setResolved(op, pathKey string, t *target, v Value) error {
	if t.entry != nil {
		old, ok := entryValue(t.entry)
		if !ok {
			return fmt.Errorf("pxf: %s: path addresses a block; set its fields individually, or Remove it and Set a BlockVal", op)
		}
		start := old.pos().Offset
		end := old.end().Offset
		indent := r.lineIndentAt(t.entry.pos().Offset)
		r.stage(spanEdit{path: pathKey, start: start, end: end, text: r.renderValue(v, indent, r.stepFor(t.containerEntries))})
		return nil
	}
	return r.stageInsert(op, pathKey, t, v)
}

// Remove stages the deletion of the entry at path. When the entry sits
// alone on its line(s), the whole lines are removed, including a
// trailing comment on the entry's last line; leading comment lines
// above the entry are kept (they may describe the surrounding group).
// When the entry shares a line with other content, only its exact span
// is removed. Removing a path that does not exist is an error.
func (r *Rewriter) Remove(path string) error {
	t, err := r.resolve(path)
	if err != nil {
		return err
	}
	if t.entry == nil {
		return fmt.Errorf("pxf: Remove %s: no such entry", path)
	}
	start, end, _ := r.entryRemovalSpan(t.entry)
	r.stage(spanEdit{path: path, start: start, end: end})
	return nil
}

// entryRemovalSpan returns the source span removing e should delete:
// when the entry sits alone on its line(s), the whole lines including a
// trailing comment on the last one (wholeLines true); otherwise the
// exact entry span plus the spaces separating it from what follows.
func (r *Rewriter) entryRemovalSpan(e Entry) (start, end int, wholeLines bool) {
	start = e.pos().Offset
	end = e.end().Offset

	ls := lineStartOffset(r.src, start)
	if isBlank(r.src[ls:start]) {
		// The entry starts its line; try to consume through end of line.
		le := end
		for le < len(r.src) && (r.src[le] == ' ' || r.src[le] == '\t') {
			le++
		}
		if le < len(r.src) && (r.src[le] == '#' || (r.src[le] == '/' && le+1 < len(r.src) && r.src[le+1] == '/')) {
			for le < len(r.src) && r.src[le] != '\n' {
				le++
			}
		} else if le+1 < len(r.src) && r.src[le] == '/' && r.src[le+1] == '*' {
			// A trailing block comment is consumed only when it closes on
			// the same line; a multi-line /* ... */ is not the entry's own
			// trailing comment, so fall through to span-only removal.
			if close := bytes.Index(r.src[le:], []byte("*/")); close >= 0 {
				through := le + close + 2
				if !bytes.Contains(r.src[le:through], []byte("\n")) {
					le = through
					for le < len(r.src) && (r.src[le] == ' ' || r.src[le] == '\t') {
						le++
					}
				}
			}
		}
		if le >= len(r.src) || r.src[le] == '\n' {
			if le < len(r.src) {
				le++ // include the newline
			}
			return ls, le, true
		}
	}
	// The entry shares its line(s) with other content: remove its exact
	// span plus the spaces that separated it from what follows.
	for end < len(r.src) && (r.src[end] == ' ' || r.src[end] == '\t') {
		end++
	}
	return start, end, false
}

// ReplaceValue stages a replacement of the source span of an existing
// value node with the rendering of with. Unlike [Rewriter.Set], which
// addresses a value by a first-match dotted path, ReplaceValue targets
// the exact node the caller holds from [Rewriter.Document] — so it can
// mutate one node among siblings that share a key (repeated widget
// nodes in a children block, say) that no path can single out.
//
// old must be a value parsed from this Rewriter's source (it carries
// the span to replace); a hand-built value or a [BadVal] has no span
// and is rejected. Continuation lines of a multi-line replacement are
// re-indented to old's line, matching Set. Calling ReplaceValue again
// for the same node replaces the previously staged edit.
func (r *Rewriter) ReplaceValue(old, with Value) error {
	if old == nil {
		return fmt.Errorf("pxf: ReplaceValue: nil old value")
	}
	if with == nil {
		return fmt.Errorf("pxf: ReplaceValue: nil replacement value")
	}
	if containsBadVal(with) {
		return fmt.Errorf("pxf: ReplaceValue: cannot write a BadVal placeholder")
	}
	start := old.pos().Offset
	end := old.end().Offset
	if start >= end {
		return fmt.Errorf("pxf: ReplaceValue: old value has no source span (not a parsed value, or a BadVal)")
	}
	return r.SetSpan(start, end, r.renderValue(with, r.lineLeadingIndent(start), r.indentStep()))
}

// SetSpan stages a replacement of src[start:end) with text — the raw
// escape hatch beneath [Rewriter.ReplaceValue] and [Rewriter.Set].
// start == end is a pure insertion; a nil text is a pure deletion.
// Routing edits through the Rewriter (rather than splicing bytes by
// hand) keeps overlap detection, edit batching, and the reparse
// safety-net in [Rewriter.Bytes]. text is copied, so the caller may
// reuse its buffer. Staging another edit over the identical span
// replaces this one (last call wins).
func (r *Rewriter) SetSpan(start, end int, text []byte) error {
	if start < 0 || end < start || end > len(r.src) {
		return fmt.Errorf("pxf: SetSpan: span [%d:%d) out of range for source of length %d", start, end, len(r.src))
	}
	r.stage(spanEdit{path: spanPath(start, end), start: start, end: end, text: append([]byte(nil), text...)})
	return nil
}

// AppendEntry stages the insertion of e as the last entry of block,
// formatted to match the block's existing entries: multi-line blocks
// get a new line indented like the siblings (or the block's own indent
// plus one level when the block is empty), placed just before the
// closing brace; an inline block (`foo { ... }`, including the empty
// `foo { }`) gets the entry spliced inline before the brace. block must
// be a node from this Rewriter's [Rewriter.Document]. Repeated
// AppendEntry calls into one block accumulate in call order.
func (r *Rewriter) AppendEntry(block *Block, e Entry) error {
	if block == nil {
		return fmt.Errorf("pxf: AppendEntry: nil block")
	}
	if e == nil {
		return fmt.Errorf("pxf: AppendEntry: nil entry")
	}
	if entryContainsBadVal(e) {
		return fmt.Errorf("pxf: AppendEntry: cannot write a BadVal placeholder")
	}
	closeOff := block.End.Offset - 1
	scopeStart := block.Pos.Offset
	if closeOff < scopeStart || closeOff >= len(r.src) || r.src[closeOff] != '}' {
		return fmt.Errorf("pxf: AppendEntry: block %q has no source span (not a parsed block)", block.Name)
	}
	insertAt, text := r.renderAppendEntry(scopeStart, closeOff, block.Entries, e)
	r.stageInsertEdit(insertAt, text)
	return nil
}

// renderAppendEntry computes the insertion that appends e as the last
// entry of the scope whose opening entry starts at scopeStart and whose
// closing '}' sits at closeOff, formatted to match the scope's existing
// siblings. Shared by [Rewriter.AppendEntry] and the keyed-collection
// editors, which stage the result under their own edit identities.
func (r *Rewriter) renderAppendEntry(scopeStart, closeOff int, siblings []Entry, e Entry) (insertAt int, text []byte) {
	if !bytes.Contains(r.src[scopeStart:closeOff], []byte("\n")) {
		// Inline block `{ ... }` (or `{}`): splice ` <entry> ` before '}'.
		var sb bytes.Buffer
		if closeOff > 0 && r.src[closeOff-1] != ' ' && r.src[closeOff-1] != '\t' {
			sb.WriteByte(' ')
		}
		writeEntryInline(&sb, e)
		sb.WriteByte(' ')
		return closeOff, sb.Bytes()
	}

	// Multi-line block: insert a full line just above the closing brace,
	// indented like the existing siblings. A nested body (a block entry's
	// children) steps in by the document's own indent width, not a fixed
	// two spaces.
	step := r.stepFor(siblings)
	indent := r.childIndent(siblings, scopeStart)
	if len(siblings) == 0 {
		// No sibling reveals the base indent; derive it from the block's
		// own line plus one step, rather than childIndent's 2-space guess.
		indent = r.lineLeadingIndent(scopeStart) + step
	}
	insertAt = closeOff
	var prefix []byte
	ls := lineStartOffset(r.src, closeOff)
	if isBlank(r.src[ls:closeOff]) {
		insertAt = ls
	} else {
		// The '}' shares a line with the last entry; break the line.
		prefix = []byte("\n")
	}
	var sb bytes.Buffer
	sb.Write(prefix)
	sb.Write(renderEntryLines(e, indent, step))
	sb.WriteByte('\n')
	return insertAt, sb.Bytes()
}

// stageInsertEdit records a zero-width insertion of text at off under a
// unique edit identity, so multiple insertions at the same offset are
// all kept (in call order) rather than deduplicated.
func (r *Rewriter) stageInsertEdit(off int, text []byte) {
	r.insertSeq++
	r.stage(spanEdit{path: fmt.Sprintf("\x00insert#%d", r.insertSeq), start: off, end: off, text: text})
}

// spanPath is the dedup key for a span-addressed edit. The NUL prefix
// keeps it distinct from every dotted path.
func spanPath(start, end int) string {
	return fmt.Sprintf("\x00span:%d:%d", start, end)
}

// Bytes applies the staged edits and returns the rewritten document.
// Edits must not overlap (e.g. a Remove of a block combined with a Set
// inside that block); overlapping edits are reported as an error. As a
// safety net for writer bugs, the result is reparsed before being
// returned — a rewrite that produces invalid PXF is an error, and the
// original source is never modified.
func (r *Rewriter) Bytes() ([]byte, error) {
	if len(r.edits) == 0 {
		return append([]byte(nil), r.src...), nil
	}
	edits := append([]spanEdit(nil), r.edits...)
	sort.SliceStable(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	for i := 1; i < len(edits); i++ {
		if edits[i-1].end > edits[i].start {
			return nil, fmt.Errorf("pxf: conflicting edits: %s overlaps %s", edits[i-1].path, edits[i].path)
		}
	}
	var out bytes.Buffer
	out.Grow(len(r.src) + 64)
	last := 0
	for _, e := range edits {
		out.Write(r.src[last:e.start])
		out.Write(e.text)
		last = e.end
	}
	out.Write(r.src[last:])
	result := out.Bytes()
	if _, err := Parse(result); err != nil {
		return nil, fmt.Errorf("pxf: rewrite produced an invalid document: %w", err)
	}
	return result, nil
}

// stage records an edit, replacing any previously staged edit for the
// same path (last call wins).
func (r *Rewriter) stage(e spanEdit) {
	for i := range r.edits {
		if r.edits[i].path == e.path {
			r.edits[i] = e
			return
		}
	}
	r.edits = append(r.edits, e)
}

// target is the result of resolving a dotted path: either a concrete
// entry, or the deepest existing scope plus the missing trailing
// segments.
type target struct {
	entry            Entry    // non-nil when the full path resolved
	container        Entry    // innermost resolved ancestor (*Block, or *Assignment/*MapEntry holding a *BlockVal); nil = document top level
	containerEntries []Entry  // the entry list the last lookup ran in
	missing          []string // unresolved trailing segments; empty when entry != nil
}

func (r *Rewriter) resolve(path string) (*target, error) {
	return r.resolveIn(r.doc.Entries, nil, path)
}

// resolveIn resolves a dotted path against an arbitrary entry scope —
// the document top level for [Rewriter.resolve], or one keyed element's
// body for the keyed-collection editors. container is the entry owning
// the scope (nil for the top level).
func (r *Rewriter) resolveIn(entries []Entry, container Entry, path string) (*target, error) {
	segs := strings.Split(path, ".")
	for _, seg := range segs {
		if seg == "" {
			return nil, fmt.Errorf("pxf: invalid path %q: empty segment", path)
		}
	}
	for i, seg := range segs {
		e := findEntry(entries, seg)
		if e == nil {
			return &target{container: container, containerEntries: entries, missing: segs[i:]}, nil
		}
		if i == len(segs)-1 {
			return &target{entry: e, container: container, containerEntries: entries}, nil
		}
		if b, ok := e.(*Block); ok {
			entries = b.Entries
		} else {
			val, _ := entryValue(e)
			bv, ok := val.(*BlockVal)
			if !ok {
				return nil, fmt.Errorf("pxf: path %q: segment %q is a scalar field, not a block", path, seg)
			}
			entries = bv.Entries
		}
		container = e
	}
	return nil, fmt.Errorf("pxf: invalid path %q", path) // unreachable: segs is never empty
}

// entryKey returns the key (assignment / map entry) or name (block)
// an entry is addressed by.
func entryKey(e Entry) string {
	switch n := e.(type) {
	case *Assignment:
		return n.Key
	case *MapEntry:
		return n.Key
	case *Block:
		return n.Name
	}
	return ""
}

// entryValue returns the value of a leaf entry; ok is false for a
// [Block], which has entries rather than a value.
func entryValue(e Entry) (Value, bool) {
	switch n := e.(type) {
	case *Assignment:
		return n.Value, true
	case *MapEntry:
		return n.Value, true
	}
	return nil, false
}

// findEntry returns the first entry in entries whose key or block name
// is key, or nil.
func findEntry(entries []Entry, key string) Entry {
	for _, e := range entries {
		if entryKey(e) == key {
			return e
		}
	}
	return nil
}

// containsBadVal reports whether v is or contains a [BadVal]
// placeholder anywhere in its value tree.
func containsBadVal(v Value) bool {
	switch n := v.(type) {
	case *BadVal:
		return true
	case *ListVal:
		for _, e := range n.Elements {
			if containsBadVal(e) {
				return true
			}
		}
	case *BlockVal:
		for _, e := range n.Entries {
			if val, ok := entryValue(e); ok && containsBadVal(val) {
				return true
			}
		}
	}
	return false
}

// stageInsert builds the text for the missing tail of a path and
// stages its insertion at the end of the deepest existing scope. op is
// the display prefix for error messages; pathKey the staging identity.
func (r *Rewriter) stageInsert(op, pathKey string, t *target, v Value) error {
	// The intermediate (block-creating) segments must be bare
	// identifiers; the leaf may need quoting only in map (':') form.
	for _, seg := range t.missing[:len(t.missing)-1] {
		if needsQuoting(seg) {
			return fmt.Errorf("pxf: %s: segment %q is not a valid block name", op, seg)
		}
	}
	leaf := t.missing[len(t.missing)-1]
	mapForm := hasMapEntry(t.containerEntries)
	if needsQuoting(leaf) && !mapForm {
		return fmt.Errorf("pxf: %s: key %q needs a map (':') context", op, leaf)
	}
	step := r.stepFor(t.containerEntries)

	// Locate the insertion scope: [open..close) of the container block,
	// or the end of the document for the top level.
	var closeOff int   // offset of the closing '}' (top level: len(src))
	var scopeStart int // offset the block's entry starts at, for indent fallback
	switch c := t.container.(type) {
	case nil:
		closeOff = len(r.src)
		scopeStart = -1
	case *Block:
		closeOff = c.End.Offset - 1
		scopeStart = c.Pos.Offset
	case *Assignment:
		closeOff = c.Value.(*BlockVal).End.Offset - 1
		scopeStart = c.Pos.Offset
	case *MapEntry:
		closeOff = c.Value.(*BlockVal).End.Offset - 1
		scopeStart = c.Pos.Offset
	}
	if scopeStart >= 0 && (closeOff < 0 || closeOff >= len(r.src) || r.src[closeOff] != '}') {
		return fmt.Errorf("pxf: %s: internal error: container span does not end at '}'", op)
	}

	if scopeStart >= 0 && !bytes.Contains(r.src[scopeStart:closeOff], []byte("\n")) {
		// Inline block `{ ... }` (or `{}`): splice ` key = value ` just
		// before the closing brace.
		var sb bytes.Buffer
		if closeOff > 0 && r.src[closeOff-1] != ' ' && r.src[closeOff-1] != '\t' {
			sb.WriteByte(' ')
		}
		sb.Write(r.renderChain(t.missing, "", step, mapForm, v, true))
		sb.WriteByte(' ')
		r.stage(spanEdit{path: pathKey, start: closeOff, end: closeOff, text: sb.Bytes()})
		return nil
	}

	// Multi-line scope: insert full lines just above the closing brace
	// (top level: at end of input), indented like the existing siblings.
	indent := r.siblingIndent(t)
	insertAt := closeOff
	var prefix []byte
	if scopeStart >= 0 {
		ls := lineStartOffset(r.src, closeOff)
		if isBlank(r.src[ls:closeOff]) {
			insertAt = ls
		} else {
			// The '}' shares a line with the last entry; break the line.
			prefix = []byte("\n")
		}
	} else if len(r.src) > 0 && r.src[len(r.src)-1] != '\n' {
		prefix = []byte("\n")
	}
	var sb bytes.Buffer
	sb.Write(prefix)
	sb.Write(r.renderChain(t.missing, indent, step, mapForm, v, false))
	sb.WriteByte('\n')
	r.stage(spanEdit{path: pathKey, start: insertAt, end: insertAt, text: sb.Bytes()})
	return nil
}

// renderChain renders the missing path tail: zero or more nested block
// opens, then `leaf = value` (or `leaf: value` in map form).
// baseIndent is the indent of the first rendered line; nested lines add
// one step per level. In inline form everything renders on one line and
// baseIndent/step are ignored.
func (r *Rewriter) renderChain(missing []string, baseIndent, step string, mapForm bool, v Value, inline bool) []byte {
	var sb bytes.Buffer
	// Only the leaf's immediate container decides '=' vs ':'; synthesized
	// intermediate blocks are messages, so their leaf uses '='.
	leafMap := mapForm && len(missing) == 1
	for i, seg := range missing[:len(missing)-1] {
		if !inline {
			sb.WriteString(indentAt(baseIndent, step, i))
		}
		sb.WriteString(seg)
		if inline {
			sb.WriteString(" { ")
		} else {
			sb.WriteString(" {\n")
		}
	}
	leaf := missing[len(missing)-1]
	depth := len(missing) - 1
	if !inline {
		sb.WriteString(indentAt(baseIndent, step, depth))
	}
	if leafMap && needsQuoting(leaf) {
		fmt.Fprintf(&sb, "%q", leaf)
	} else {
		sb.WriteString(leaf)
	}
	if leafMap {
		sb.WriteString(": ")
	} else {
		sb.WriteString(" = ")
	}
	sb.Write(r.renderValue(v, indentAt(baseIndent, step, depth), step))
	for i := len(missing) - 2; i >= 0; i-- {
		if inline {
			sb.WriteString(" }")
		} else {
			sb.WriteByte('\n')
			sb.WriteString(indentAt(baseIndent, step, i))
			sb.WriteByte('}')
		}
	}
	return sb.Bytes()
}

// indentAt returns base plus depth levels of step.
func indentAt(base, step string, depth int) string {
	return base + strings.Repeat(step, depth)
}

// renderValue formats v on its own (via the [FormatDocument]
// formatter) and re-indents any continuation lines with linePrefix so
// multi-line values (lists, blocks) align under the entry they are
// spliced into. Nested bodies step in by step per level, matching the
// document's indent width rather than a fixed two spaces.
func (r *Rewriter) renderValue(v Value, linePrefix, step string) []byte {
	var buf bytes.Buffer
	f := &formatter{buf: &buf, indent: step}
	f.formatValue(v, 0)
	out := buf.Bytes()
	if linePrefix != "" && bytes.IndexByte(out, '\n') >= 0 {
		out = bytes.ReplaceAll(out, []byte("\n"), []byte("\n"+linePrefix))
	}
	return out
}

// siblingIndent returns the indentation for a line inserted into the
// target's container: the indent of the container's first entry when
// there is one on its own line, otherwise the container's own indent
// plus one level (top level: none).
func (r *Rewriter) siblingIndent(t *target) string {
	containerPos := -1
	if t.container != nil {
		containerPos = t.container.pos().Offset
	}
	return r.childIndent(t.containerEntries, containerPos)
}

// childIndent returns the indentation a new child of a block should
// use: the indent of the first existing child on its own line, else the
// block's own indent plus one level. containerPos < 0 means the
// document top level, which is not indented.
func (r *Rewriter) childIndent(entries []Entry, containerPos int) string {
	if len(entries) > 0 {
		first := entries[0].pos().Offset
		ls := lineStartOffset(r.src, first)
		if isBlank(r.src[ls:first]) {
			return string(r.src[ls:first])
		}
	}
	if containerPos < 0 {
		return ""
	}
	return r.lineIndentAt(containerPos) + "  "
}

// stepFor infers the per-level indentation step to use for content
// inserted among entries: prefer a nested block among those entries (a
// sibling's body is the most relevant reference), then a document-wide
// scan, then a two-space default when nothing reveals it.
func (r *Rewriter) stepFor(entries []Entry) string {
	if s := r.scanIndentStep(entries); s != "" {
		return s
	}
	return r.indentStep()
}

// indentStep infers the document's per-level indentation step from a
// document-wide scan, or two spaces when nothing reveals it.
func (r *Rewriter) indentStep() string {
	if s := r.scanIndentStep(r.doc.Entries); s != "" {
		return s
	}
	return "  "
}

// scanIndentStep returns the whitespace the first own-line child of the
// first nested block (or block value) in entries adds over that block's
// own line — the document's indentation step — or "" when no nested
// block on its own lines reveals one.
func (r *Rewriter) scanIndentStep(entries []Entry) string {
	for _, e := range entries {
		var kids []Entry
		switch n := e.(type) {
		case *Block:
			kids = n.Entries
		default:
			if v, ok := entryValue(e); ok {
				if bv, ok := v.(*BlockVal); ok {
					kids = bv.Entries
				}
			}
		}
		if len(kids) == 0 {
			continue
		}
		base := r.lineLeadingIndent(e.pos().Offset)
		first := kids[0].pos().Offset
		ls := lineStartOffset(r.src, first)
		if isBlank(r.src[ls:first]) {
			ci := string(r.src[ls:first])
			if len(ci) > len(base) && strings.HasPrefix(ci, base) {
				return ci[len(base):]
			}
		}
		if s := r.scanIndentStep(kids); s != "" {
			return s
		}
	}
	return ""
}

// lineIndentAt returns the whitespace prefix of the line containing
// off, or "" when non-whitespace precedes off on its line.
func (r *Rewriter) lineIndentAt(off int) string {
	ls := lineStartOffset(r.src, off)
	if isBlank(r.src[ls:off]) {
		return string(r.src[ls:off])
	}
	return ""
}

// lineLeadingIndent returns the leading whitespace of the line
// containing off, regardless of what else precedes off on the line.
// Used to align continuation lines of a value spliced after its key.
func (r *Rewriter) lineLeadingIndent(off int) string {
	ls := lineStartOffset(r.src, off)
	i := ls
	for i < off && (r.src[i] == ' ' || r.src[i] == '\t') {
		i++
	}
	return string(r.src[ls:i])
}

// renderEntryLines formats a single entry for insertion into a
// multi-line block: fully formatted (nested blocks indented by step per
// level), every line prefixed with baseIndent, and no trailing newline
// (the caller controls line breaks).
func renderEntryLines(e Entry, baseIndent, step string) []byte {
	var buf bytes.Buffer
	f := &formatter{buf: &buf, indent: step}
	f.formatEntries([]Entry{e}, 0)
	out := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if baseIndent != "" {
		out = append([]byte(baseIndent), bytes.ReplaceAll(out, []byte("\n"), []byte("\n"+baseIndent))...)
	}
	return out
}

// writeEntryInline renders an entry on a single line for insertion into
// an inline block, e.g. `a = 1`, `"k": "v"`, or `n { a = 1 }`.
func writeEntryInline(buf *bytes.Buffer, e Entry) {
	f := &formatter{buf: buf, indent: "  "}
	switch n := e.(type) {
	case *Assignment:
		if n.KeyQuoted {
			fmt.Fprintf(buf, "%q", n.Key)
		} else {
			buf.WriteString(n.Key)
		}
		buf.WriteString(" = ")
		f.formatValue(n.Value, 0)
	case *MapEntry:
		if needsQuoting(n.Key) {
			fmt.Fprintf(buf, "%q", n.Key)
		} else {
			buf.WriteString(n.Key)
		}
		buf.WriteString(": ")
		f.formatValue(n.Value, 0)
	case *Block:
		if n.NameQuoted {
			fmt.Fprintf(buf, "%q", n.Name)
		} else {
			buf.WriteString(n.Name)
		}
		buf.WriteString(" {")
		for _, c := range n.Entries {
			buf.WriteByte(' ')
			writeEntryInline(buf, c)
		}
		buf.WriteString(" }")
	}
}

// entryContainsBadVal reports whether e holds a [BadVal] anywhere in
// its value tree (including nested block entries).
func entryContainsBadVal(e Entry) bool {
	switch n := e.(type) {
	case *Assignment:
		return containsBadVal(n.Value)
	case *MapEntry:
		return containsBadVal(n.Value)
	case *Block:
		for _, c := range n.Entries {
			if entryContainsBadVal(c) {
				return true
			}
		}
	}
	return false
}

// hasMapEntry reports whether any sibling is a ':'-form map entry,
// which decides the separator for inserted leaves.
func hasMapEntry(entries []Entry) bool {
	for _, e := range entries {
		if _, ok := e.(*MapEntry); ok {
			return true
		}
	}
	return false
}

// lineStartOffset returns the offset of the first byte of the line
// containing off.
func lineStartOffset(src []byte, off int) int {
	i := off
	for i > 0 && src[i-1] != '\n' {
		i--
	}
	return i
}

// isBlank reports whether b is all spaces and tabs.
func isBlank(b []byte) bool {
	for _, c := range b {
		if c != ' ' && c != '\t' {
			return false
		}
	}
	return true
}
