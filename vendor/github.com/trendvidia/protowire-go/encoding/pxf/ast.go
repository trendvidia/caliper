// Copyright 2026 TrendVidia LLC
// SPDX-License-Identifier: MIT

package pxf

import (
	"fmt"
	"time"
)

// Comment represents a comment in source text.
type Comment struct {
	Pos  Position
	Text string // raw text including the comment prefix (# or // or /* */)
}

// Document is the root AST node of a PXF file.
type Document struct {
	TypeURL         string             // from @type directive, may be empty
	Directives      []Directive        // @<name> *(prefix) [{ ... }] entries before the body, in source order; excludes spec-defined directives
	Datasets        []DatasetDirective // @dataset directives in source order; per draft §3.4.4 a document with any @dataset MUST NOT have @type or body entries
	Protos          []ProtoDirective   // @proto directives in source order (draft §3.4.5)
	BodyOffset      int                // byte offset in the input where the schema-typed body begins (after all leading directives)
	Entries         []Entry
	LeadingComments []Comment // comments before the first entry (or after @type)
}

// DatasetDirective is a `@dataset <type> ( col1, col2, ... ) row*`
// entry at document root (draft §3.4.4). It carries many instances of
// one message type in a single document — the protowire-native CSV.
//
// Cells are scalar-shaped in v1: list ('[ ... ]') and block ('{ ... }')
// values are not permitted in cells. An empty cell (no value between
// commas) denotes an absent field; a `null` literal denotes a present-
// but-null field; any other value denotes a present field with that
// value. See [DatasetRow] for cell representation.
//
// A document with any DatasetDirective MUST NOT have a @type directive
// or any top-level field entries: the @dataset header IS the document's
// type declaration. Decoders enforce this in [Parse].
//
// Type MAY be empty when an anonymous @proto directive (draft §3.4.5)
// precedes the @dataset in document order; the anonymous schema is
// consumed as the row message type.
type DatasetDirective struct {
	Pos             Position
	Type            string       // row message type, e.g. "trades.v1.Trade"; empty if bound to a preceding anonymous @proto
	Columns         []string     // top-level field names on Type; len(Columns) >= 1
	Rows            []DatasetRow // zero or more rows
	LeadingComments []Comment
}

// DatasetRow is one parenthesized cell tuple in a @dataset directive.
// Cells is the same length as the containing DatasetDirective.Columns.
// A nil Value in Cells denotes an absent field (the "empty cell"
// between two commas); a *NullVal denotes a present-but-null field;
// any other Value denotes a present field with that value.
type DatasetRow struct {
	Pos   Position
	Cells []Value // nil entries denote absent fields; len == len(dataset.Columns)
}

// ProtoShape distinguishes the four body shapes of a @proto directive
// (draft §3.4.5).
type ProtoShape int

const (
	// ProtoAnonymous is `@proto { <message-body> }` — defines an
	// unnamed message used by the next typed directive in document
	// order that does not carry an explicit type name.
	ProtoAnonymous ProtoShape = iota
	// ProtoNamed is `@proto <dotted-name> { <message-body> }` — sugar
	// for a single named message; TypeName carries the dotted name.
	ProtoNamed
	// ProtoSource is `@proto """<proto-source>"""` — a complete
	// Protocol Buffers source file.
	ProtoSource
	// ProtoDescriptor is `@proto b"<base64-FileDescriptorSet>"` —
	// a base64-encoded google.protobuf.FileDescriptorSet.
	ProtoDescriptor
)

func (s ProtoShape) String() string {
	switch s {
	case ProtoAnonymous:
		return "anonymous"
	case ProtoNamed:
		return "named"
	case ProtoSource:
		return "source"
	case ProtoDescriptor:
		return "descriptor"
	default:
		return fmt.Sprintf("ProtoShape(%d)", int(s))
	}
}

// ProtoDirective is a `@proto <body>` entry at document root
// (draft §3.4.5). It carries an embedded protobuf schema, making the
// PXF document self-describing.
//
// The Shape field distinguishes the four lexically-determined body
// forms (anonymous, named, source, descriptor). Body holds the raw
// bytes of the body, interpreted per Shape:
//
//   - ProtoAnonymous, ProtoNamed: the bytes between the opening `{`
//     and matching `}` (both exclusive). The bytes are protobuf
//     message-body source (field declarations and nested types) and
//     must be compiled by a downstream consumer.
//   - ProtoSource: the contents of the triple-quoted string (with
//     leading-LF stripping and common-prefix dedent already applied).
//     The bytes are a complete .proto source file.
//   - ProtoDescriptor: the base64-decoded bytes of the bytes literal.
//     The bytes are a serialised google.protobuf.FileDescriptorSet.
type ProtoDirective struct {
	Pos             Position
	Shape           ProtoShape
	TypeName        string // dotted message type name; non-empty only when Shape == ProtoNamed
	Body            []byte
	LeadingComments []Comment
}

// Directive is a top-of-document `@<name> *(<prefix-id>) [{ ... }]`
// entry. The canonical use is side-channel metadata that sits alongside
// the schema-typed body — e.g. chameleon's `@header
// chameleon.v1.LayerHeader { id = "x" }` — but the grammar is open-ended:
// any name except `type` is accepted, followed by zero-or-more prefix
// identifiers and an optional inline block.
//
// Prefix identifiers are positional and per-directive. The two
// registrations defined by the protowire spec:
//
//   - One prefix identifier (v0.72.0 conventional shape) — the
//     identifier names the inner block's message type, dotted. Used by
//     `@header` and similar.
//   - `@entry` (draft §3.4.3) — zero, one, or two prefix identifiers
//     (label, type); a single prefix is disambiguated by the presence
//     of a `.` (dotted ⇒ type; bare ⇒ label).
//
// Body holds the RAW bytes between the opening `{` and matching `}`
// (both exclusive), suitable for handing back to [UnmarshalFull] /
// [Unmarshal] against the consumer's chosen message. Body is nil when
// the directive has no inline block.
type Directive struct {
	Pos      Position
	Name     string   // e.g. "header"; never "type" (those go to Document.TypeURL)
	Prefixes []string // identifiers between @<name> and the optional `{ ... }`, in source order
	// Type is preserved for v0.72.0-era consumers: when exactly one
	// prefix identifier was supplied, Type holds it (matching the
	// previous single-Type field's behavior). For zero or two-plus
	// prefixes, Type is empty and callers MUST read Prefixes directly.
	// New code should use Prefixes; Type is retained to avoid churning
	// downstream consumers that haven't migrated.
	Type            string
	Body            []byte // raw inner bytes of the block; nil if the directive has no `{ ... }`
	LeadingComments []Comment
}

// Entry is a node that can appear in a message or map body.
type Entry interface {
	entryNode()
	pos() Position
	end() Position
}

// Assignment represents "key = value" (field assignment in message context).
type Assignment struct {
	Pos Position
	End Position // just past the value's last byte
	Key string
	// KeyQuoted records that Key was written as a string literal
	// (`"name" = { ... }`, the quoted entry-name form of draft -01
	// §3.13). Key always holds the unquoted (denoted) value; the flag
	// exists so [FormatDocument] round-trips the source spelling. A
	// quoted name is only meaningful as the key of a keyed repeated
	// field's entry — the schema layer rejects it anywhere else.
	KeyQuoted       bool
	Value           Value
	LeadingComments []Comment // comments on lines before this entry
	TrailingComment string    // inline comment after value on same line
}

func (*Assignment) entryNode()      {}
func (a *Assignment) pos() Position { return a.Pos }
func (a *Assignment) end() Position { return a.End }

// MapEntry represents "key: value" (entry in map context).
type MapEntry struct {
	Pos             Position
	End             Position // just past the value's last byte
	Key             string
	Value           Value
	LeadingComments []Comment
	TrailingComment string
}

func (*MapEntry) entryNode()      {}
func (e *MapEntry) pos() Position { return e.Pos }
func (e *MapEntry) end() Position { return e.End }

// Block represents "name { entries }" (nested message).
type Block struct {
	Pos  Position
	End  Position // just past the closing '}'
	Name string
	// NameQuoted records that Name was written as a string literal
	// (`"us-east-1" { ... }`, draft -01 §3.13). Name always holds the
	// unquoted (denoted) value; the flag preserves the source spelling
	// for [FormatDocument]. A quoted name is only meaningful as the key
	// of a keyed repeated field's entry — the schema layer rejects it
	// anywhere else.
	NameQuoted      bool
	Entries         []Entry
	LeadingComments []Comment
}

func (*Block) entryNode()      {}
func (b *Block) pos() Position { return b.Pos }
func (b *Block) end() Position { return b.End }

// Value is an expression on the right side of = or :.
type Value interface {
	valueNode()
	pos() Position
	end() Position
}

// StringVal is a quoted string literal.
type StringVal struct {
	Pos   Position
	End   Position // just past the closing quote
	Value string
}

func (*StringVal) valueNode()      {}
func (v *StringVal) pos() Position { return v.Pos }
func (v *StringVal) end() Position { return v.End }

// IntVal is an integer literal (raw text, decoded later by schema).
type IntVal struct {
	Pos Position
	End Position // just past the literal's last byte
	Raw string
}

func (*IntVal) valueNode()      {}
func (v *IntVal) pos() Position { return v.Pos }
func (v *IntVal) end() Position { return v.End }

// FloatVal is a floating-point literal.
type FloatVal struct {
	Pos Position
	End Position // just past the literal's last byte
	Raw string
}

func (*FloatVal) valueNode()      {}
func (v *FloatVal) pos() Position { return v.Pos }
func (v *FloatVal) end() Position { return v.End }

// BoolVal is a boolean literal (true / false).
type BoolVal struct {
	Pos   Position
	End   Position // just past the literal's last byte
	Value bool
}

func (*BoolVal) valueNode()      {}
func (v *BoolVal) pos() Position { return v.Pos }
func (v *BoolVal) end() Position { return v.End }

// BytesVal is a base64-encoded bytes literal (b"...").
type BytesVal struct {
	Pos   Position
	End   Position // just past the closing quote
	Value []byte
}

func (*BytesVal) valueNode()      {}
func (v *BytesVal) pos() Position { return v.Pos }
func (v *BytesVal) end() Position { return v.End }

// NullVal represents an explicit null literal.
type NullVal struct {
	Pos Position
	End Position // just past the literal's last byte
}

func (*NullVal) valueNode()      {}
func (v *NullVal) pos() Position { return v.Pos }
func (v *NullVal) end() Position { return v.End }

// IdentVal is an unquoted identifier used as a value (enum names).
type IdentVal struct {
	Pos  Position
	End  Position // just past the identifier's last byte
	Name string
}

func (*IdentVal) valueNode()      {}
func (v *IdentVal) pos() Position { return v.Pos }
func (v *IdentVal) end() Position { return v.End }

// TimestampVal is an RFC 3339 timestamp literal.
type TimestampVal struct {
	Pos   Position
	End   Position // just past the literal's last byte
	Value time.Time
	Raw   string
}

func (*TimestampVal) valueNode()      {}
func (v *TimestampVal) pos() Position { return v.Pos }
func (v *TimestampVal) end() Position { return v.End }

// DurationVal is a Go-style duration literal (e.g. 30s, 1h30m).
type DurationVal struct {
	Pos   Position
	End   Position // just past the literal's last byte
	Value time.Duration
	Raw   string
}

func (*DurationVal) valueNode()      {}
func (v *DurationVal) pos() Position { return v.Pos }
func (v *DurationVal) end() Position { return v.End }

// BadVal is a placeholder for a value that was required but missing or
// malformed. It is produced only by [ParseTolerant] — [Parse] never
// emits one. For a missing value, Pos points just past the token that
// required it (right after a dangling '=' or key), the spot where the
// value would be typed; for a malformed literal, Pos points at the
// offending token. The corresponding syntax error is in the []Error
// returned alongside the document. [FormatDocument] renders a BadVal
// as nothing, so formatting a tolerant AST that contains one may not
// reparse.
type BadVal struct {
	Pos Position
	End Position // == Pos; a BadVal spans no source bytes
}

func (*BadVal) valueNode()      {}
func (v *BadVal) pos() Position { return v.Pos }
func (v *BadVal) end() Position { return v.End }

// ListVal is a list value: [elem, elem, ...].
type ListVal struct {
	Pos      Position
	End      Position // just past the closing ']'
	Elements []Value
}

func (*ListVal) valueNode()      {}
func (v *ListVal) pos() Position { return v.Pos }
func (v *ListVal) end() Position { return v.End }

// BlockVal is an anonymous block value: { entries }.
// Used for maps (key: value pairs) and inline messages in lists.
type BlockVal struct {
	Pos     Position
	End     Position // just past the closing '}'
	Entries []Entry
}

func (*BlockVal) valueNode()      {}
func (v *BlockVal) pos() Position { return v.Pos }
func (v *BlockVal) end() Position { return v.End }

// EntrySpan returns the source byte span [start, end) covered by an
// entry parsed with [Parse] or [ParseTolerant]: start is the position
// of the entry's key and end is just past its value (past the closing
// '}' for a [Block]). Leading comments and surrounding whitespace are
// not included. Entries constructed by hand have a zero end.
func EntrySpan(e Entry) (start, end Position) { return e.pos(), e.end() }

// ValueSpan returns the source byte span [start, end) covered by a
// value parsed with [Parse] or [ParseTolerant]. A [BadVal] spans no
// bytes (start == end). Values constructed by hand have a zero end.
func ValueSpan(v Value) (start, end Position) { return v.pos(), v.end() }
