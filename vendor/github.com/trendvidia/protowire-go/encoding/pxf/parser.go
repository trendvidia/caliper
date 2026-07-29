// Copyright 2026 TrendVidia LLC
// SPDX-License-Identifier: MIT

package pxf

import (
	"encoding/base64"
	"fmt"
	"sort"
	"time"
)

type parser struct {
	lex      *lexer
	current  Token
	comments []Comment // pending comments not yet attached to an entry

	// tolerant enables the error-recovering mode behind [ParseTolerant]:
	// instead of returning the first syntax error, the parser records it
	// in errs and resynchronizes at the nearest entry or block boundary.
	// All error-return paths are annotated so that when tolerant is set
	// they record-and-recover; when clear the strict behavior is
	// byte-for-byte what it always was.
	tolerant bool
	errs     []Error

	// prevEnd is the position just past the last byte of the most
	// recently consumed significant token. Tolerant recovery anchors
	// synthesized BadVal placeholders here — the spot where the missing
	// value would have been typed (e.g. right after a dangling '=').
	prevEnd Position
}

func newParser(input []byte) *parser {
	p := &parser{lex: newLexer(input)}
	p.advance()
	return p
}

func newTolerantParser(input []byte) *parser {
	p := &parser{lex: newLexer(input), tolerant: true}
	p.lex.tolerant = true
	p.lex.onErr = func(pos Position, msg string) {
		p.errs = append(p.errs, Error{Pos: pos, Msg: msg})
	}
	p.advance()
	return p
}

// record appends a recoverable syntax error in tolerant mode.
func (p *parser) record(err error) {
	if e, ok := err.(*Error); ok {
		p.errs = append(p.errs, *e)
		return
	}
	p.errs = append(p.errs, Error{Pos: p.current.Pos, Msg: err.Error()})
}

// soft routes a recoverable syntax error: strict mode returns it to
// the caller (the first error aborts the parse, preserving Parse's
// all-or-nothing contract), tolerant mode records it and returns nil
// so the call site can recover.
func (p *parser) soft(err error) error {
	if !p.tolerant {
		return err
	}
	p.record(err)
	return nil
}

// tokenEnd returns the position just past the last byte of the
// current token. The lexer sits exactly at that byte after producing
// a token, and the parser's advance() only pre-reads trivia, never the
// next significant token, so this holds whenever the parser is
// positioned on p.current.
func (p *parser) tokenEnd() Position {
	return p.lex.currentPos()
}

// tokenErrMsg renders the current token for an error message. ILLEGAL
// tokens carry the lexer's diagnostic in Value; surface that directly
// instead of the opaque kind name.
func (p *parser) tokenErrMsg() string {
	if p.current.Kind == ILLEGAL {
		return p.current.Value
	}
	return fmt.Sprintf("%s (%q)", p.current.Kind, p.current.Value)
}

// skipBalanced consumes the open token the parser is positioned on
// together with everything through its matching close token (or EOF),
// returning the position just past the close token. Tolerant-mode
// recovery for blocks / lists that cannot be descended into (e.g.
// past MaxNestingDepth).
func (p *parser) skipBalanced(open, close TokenKind) Position {
	depth := 0
	for {
		switch p.current.Kind {
		case open:
			depth++
		case close:
			depth--
			if depth <= 0 {
				end := p.tokenEnd()
				p.advance()
				return end
			}
		case EOF:
			return p.current.Pos
		}
		p.advance()
	}
}

// currentStartsEntry reports whether the current token looks like the
// key of a new entry: a key-shaped token (IDENT, STRING, INT) followed
// by '=', ':', or '{'. Tolerant recovery uses it to decide that a
// key-shaped token belongs to the NEXT entry rather than to the
// construct being parsed.
func (p *parser) currentStartsEntry() bool {
	switch p.current.Kind {
	case IDENT, STRING, INT:
		switch p.peekKind() {
		case EQUALS, COLON, LBRACE:
			return true
		}
	}
	return false
}

// syncTopLevel advances to the next plausible top-of-document
// construct: a directive, a token that can start a body entry, or EOF.
// Tolerant-mode recovery after a malformed directive.
func (p *parser) syncTopLevel() {
	for {
		switch p.current.Kind {
		case EOF, AT_TYPE, AT_DIRECTIVE, AT_DATASET, AT_PROTO, IDENT, STRING, INT:
			return
		}
		p.advance()
	}
}

// advance consumes the next token. Comments and newlines are collected
// into pending comments instead of being skipped, so they can be attached
// to the following entry.
func (p *parser) advance() {
	p.prevEnd = p.lex.currentPos()
	for {
		p.current = p.lex.Next()
		if p.current.Kind == NEWLINE {
			continue
		}
		if p.current.Kind == COMMENT {
			p.comments = append(p.comments, Comment{
				Pos:  p.current.Pos,
				Text: p.current.Value,
			})
			continue
		}
		return
	}
}

// flushComments returns and clears pending comments.
func (p *parser) flushComments() []Comment {
	if len(p.comments) == 0 {
		return nil
	}
	c := p.comments
	p.comments = nil
	return c
}

// takeTrailingComment removes and returns the text of a pending comment that
// trails a value inline — one written after the value on the same source line
// (e.g. `restrict = true # deny-by-default`). end is the value's end position.
// A newline terminates every line, so at most one such comment exists and it
// is the first pending comment. Without this, an inline comment on the final
// entry before EOF or a block's '}' would never be flushed to any entry and
// would be dropped; comments with a following entry would instead surface only
// as that entry's leading comments. Returns "" when the first pending comment
// begins on a later line (a genuine leading comment for the next entry) or
// before the value (a rare comment wedged between the separator and value).
func (p *parser) takeTrailingComment(end Position) string {
	if len(p.comments) == 0 {
		return ""
	}
	c := p.comments[0]
	if c.Pos.Line != end.Line || c.Pos.Offset < end.Offset {
		return ""
	}
	p.comments = p.comments[1:]
	return c.Text
}

// peekKind returns the kind of the next significant token (skipping
// newlines and comments) without consuming it or disturbing pending-
// comment accumulation. Used by parseDirective to disambiguate "this
// IDENT is a directive prefix" from "this IDENT is a body field key".
func (p *parser) peekKind() TokenKind {
	pos, line, col := p.lex.pos, p.lex.line, p.lex.col
	saved := p.current
	savedPrevEnd := p.prevEnd
	nComments := len(p.comments)
	nErrs := len(p.errs) // tolerant lexing may report during the peek
	p.advance()
	next := p.current.Kind
	p.lex.pos, p.lex.line, p.lex.col = pos, line, col
	p.current = saved
	p.prevEnd = savedPrevEnd
	p.comments = p.comments[:nComments]
	p.errs = p.errs[:nErrs]
	return next
}

// Parse parses PXF source into an AST Document with comments attached.
func Parse(input []byte) (*Document, error) {
	return newParser(input).parseDocument()
}

// ParseTolerant parses PXF source in error-tolerant mode, for editor
// tooling that needs structure exactly when the buffer is broken
// (completion mid-keystroke, hover on a half-typed entry). Instead of
// stopping at the first syntax error the way [Parse] does, it recovers
// at entry and block boundaries and returns the best-effort AST
// together with every positioned error, in source order. The document
// is never nil; a syntactically valid input yields the same AST as
// [Parse] and an empty error slice. go/parser's AllErrors mode is the
// model.
//
// Recovery behaviors:
//
//   - A missing or malformed value (`key =` at end of line, `key`
//     alone) is synthesized as a [BadVal] placeholder so the entry —
//     its key and position — still appears in the AST.
//   - Unclosed blocks and lists are closed at EOF with their parsed
//     contents intact.
//   - An unterminated string ends at the newline (or EOF). Note this
//     differs from [Parse], which permits a raw newline inside a
//     simple-quoted string; in tolerant mode the newline is taken as
//     evidence of a mid-edit literal.
//   - Unterminated triple-quoted strings, bytes literals, and block
//     comments end at EOF (bytes literals at the newline).
//   - A malformed directive is skipped to the next directive or body
//     entry.
//
// The returned AST may contain [BadVal] nodes and is meant for
// tooling (completion, hover, diagnostics), not for decoding into
// messages — use [Parse] or [Unmarshal] once the document is valid.
func ParseTolerant(input []byte) (*Document, []Error) {
	p := newTolerantParser(input)
	doc, err := p.parseDocument()
	if err != nil {
		// parseDocument never returns an error in tolerant mode;
		// defensive belt-and-braces.
		p.record(err)
	}
	if doc == nil {
		doc = &Document{}
	}
	// Errors are recorded in parse order, which tracks source order
	// except when a lexer error surfaces during lookahead; sort so
	// consumers can rely on positional order.
	sort.SliceStable(p.errs, func(i, j int) bool {
		return p.errs[i].Pos.Offset < p.errs[j].Pos.Offset
	})
	return doc, p.errs
}

func (p *parser) parseDocument() (*Document, error) {
	doc := &Document{}
	doc.LeadingComments = p.flushComments() // comments before any directive or body entry

	// Top-of-document directives. @type and @<name> may interleave in any
	// order; @type populates TypeURL, others append to Directives.
	// doc.BodyOffset is the byte right after the last directive's
	// closing `}` (block form) or last token (bare form). Stays 0 when
	// there are no directives, so chameleon hashes from byte 0.
directives:
	for {
		switch p.current.Kind {
		case AT_TYPE:
			p.advance() // consume @type
			missingName := p.current.Kind != IDENT
			if !missingName && p.tolerant && p.currentStartsEntry() {
				// A dangling `@type` with the body starting right after:
				// an IDENT that is itself followed by '=', ':', or '{'
				// is the first entry's key, not the type name.
				missingName = true
			}
			if missingName {
				if err := p.soft(errorf(p.current.Pos, "expected type name after @type, got %s", p.current.Kind)); err != nil {
					return nil, err
				}
				p.syncTopLevel()
				continue
			}
			doc.TypeURL = p.current.Value
			doc.BodyOffset = p.current.Pos.Offset + len(p.current.Value)
			p.advance()
		case AT_DIRECTIVE:
			d, end, err := p.parseDirective()
			if err != nil {
				if err := p.soft(err); err != nil {
					return nil, err
				}
				p.syncTopLevel()
				continue
			}
			doc.Directives = append(doc.Directives, *d)
			doc.BodyOffset = end
		case AT_DATASET:
			ds, end, err := p.parseDatasetDirective()
			if err != nil {
				if err := p.soft(err); err != nil {
					return nil, err
				}
				p.syncTopLevel()
				continue
			}
			doc.Datasets = append(doc.Datasets, *ds)
			doc.BodyOffset = end
		case AT_PROTO:
			pd, end, err := p.parseProtoDirective()
			if err != nil {
				if err := p.soft(err); err != nil {
					return nil, err
				}
				p.syncTopLevel()
				continue
			}
			doc.Protos = append(doc.Protos, *pd)
			doc.BodyOffset = end
		default:
			break directives
		}
	}

	// Standalone constraint (draft §3.4.4): a document containing any
	// @dataset directive MUST NOT also carry @type or top-level field
	// entries — the @dataset header IS the document's type declaration.
	if len(doc.Datasets) > 0 {
		if doc.TypeURL != "" {
			if err := p.soft(errorf(doc.Datasets[0].Pos,
				"@dataset directive cannot coexist with @type; the @dataset header declares the document's type (draft §3.4.4)")); err != nil {
				return nil, err
			}
		}
		if p.current.Kind != EOF {
			if err := p.soft(errorf(p.current.Pos,
				"@dataset directive cannot coexist with top-level field entries; the document's payload is the @dataset rows (draft §3.4.4)")); err != nil {
				return nil, err
			}
		}
	}

	doc.Entries = make([]Entry, 0, 8)
	for p.current.Kind != EOF {
		// Top-level: only field_entry is allowed. The document represents a
		// proto message, never a map<K,V>; map_entry (`:` form) is reserved
		// for the inside of a `{ ... }` block. See docs/grammar.ebnf → document.
		entry, err := p.parseEntry(0, false)
		if err != nil {
			return nil, err
		}
		// Tolerant mode returns (nil, nil) for an entry it skipped past;
		// parseEntry has already consumed at least one token.
		if entry != nil {
			doc.Entries = append(doc.Entries, entry)
		}
	}
	return doc, nil
}

// parseDatasetDirective reads `@dataset <type> ( col1, col2, ... ) row*`.
// AT_DATASET is current on entry. Returns the table plus the byte offset
// immediately after the directive's last token (the `)` of the last
// row, the `)` of the column list when there are no rows, or earlier
// on error). See draft §3.4.4.
func (p *parser) parseDatasetDirective() (*DatasetDirective, int, error) {
	leading := p.flushComments()
	atPos := p.current.Pos
	ds := &DatasetDirective{
		Pos:             atPos,
		LeadingComments: leading,
	}
	p.advance() // consume @dataset

	// Optional: row message type (dotted identifier). Type MAY be
	// omitted when an anonymous @proto directive precedes the @dataset
	// in document order (draft §3.4.4 Anonymous binding).
	if p.current.Kind == IDENT {
		ds.Type = p.current.Value
		p.advance()
	}

	// Required: column list in `( ... )`. At least one column.
	if p.current.Kind != LPAREN {
		return nil, 0, errorf(p.current.Pos, "expected '(' to start @dataset column list, got %s", p.current.Kind)
	}
	p.advance() // consume (

	if p.current.Kind != IDENT {
		return nil, 0, errorf(p.current.Pos, "@dataset column list must contain at least one field name, got %s", p.current.Kind)
	}
	for {
		if p.current.Kind != IDENT {
			return nil, 0, errorf(p.current.Pos, "expected column field name, got %s", p.current.Kind)
		}
		colName := p.current.Value
		// v1: column entries are unqualified field names; dotted paths
		// reserved for a future revision.
		if containsDot(colName) {
			return nil, 0, errorf(p.current.Pos, "@dataset column %q: dotted column paths are not supported in v1 (draft §3.4.4)", colName)
		}
		ds.Columns = append(ds.Columns, colName)
		p.advance()
		if p.current.Kind == COMMA {
			p.advance()
			continue
		}
		if p.current.Kind == RPAREN {
			break
		}
		return nil, 0, errorf(p.current.Pos, "expected ',' or ')' in @dataset column list, got %s", p.current.Kind)
	}
	endOffset := p.current.Pos.Offset + 1 // past `)`
	p.advance()                           // consume )

	// Zero or more rows.
	for p.current.Kind == LPAREN {
		row, rowEnd, err := p.parseDatasetRow(len(ds.Columns))
		if err != nil {
			return nil, 0, err
		}
		ds.Rows = append(ds.Rows, *row)
		endOffset = rowEnd
	}
	return ds, endOffset, nil
}

// parseDatasetRow reads `( cell ( ',' cell )* )` with an arity check
// against expected. LPAREN is current on entry. Returns the row plus
// the byte offset immediately past the closing `)`.
func (p *parser) parseDatasetRow(expected int) (*DatasetRow, int, error) {
	pos := p.current.Pos
	p.advance() // consume (

	row := &DatasetRow{Pos: pos, Cells: make([]Value, 0, expected)}
	// First cell.
	cell, err := p.parseRowCell()
	if err != nil {
		return nil, 0, err
	}
	row.Cells = append(row.Cells, cell)
	// Remaining cells.
	for p.current.Kind == COMMA {
		p.advance()
		cell, err := p.parseRowCell()
		if err != nil {
			return nil, 0, err
		}
		row.Cells = append(row.Cells, cell)
	}
	if p.current.Kind != RPAREN {
		return nil, 0, errorf(p.current.Pos, "expected ',' or ')' in @dataset row, got %s", p.current.Kind)
	}
	endOffset := p.current.Pos.Offset + 1
	p.advance() // consume )

	if len(row.Cells) != expected {
		return nil, 0, errorf(pos, "@dataset row has %d cells, expected %d (column count)", len(row.Cells), expected)
	}
	return row, endOffset, nil
}

// parseRowCell consumes one cell of a @dataset row. Returns nil for an
// empty cell (no value between two commas, or at row start/end).
// Rejects list ('[ ... ]') and block ('{ ... }') values per v1
// cell-grammar (draft §3.4.4).
func (p *parser) parseRowCell() (Value, error) {
	switch p.current.Kind {
	case COMMA, RPAREN:
		return nil, nil
	case LBRACKET:
		return nil, errorf(p.current.Pos, "@dataset cells cannot contain list values in v1 (draft §3.4.4)")
	case LBRACE:
		return nil, errorf(p.current.Pos, "@dataset cells cannot contain block values in v1 (draft §3.4.4)")
	}
	return p.parseValue(0)
}

// parseProtoDirective reads `@proto <body>` where <body> is one of
// four lexically-distinguished shapes (draft §3.4.5):
//
//   - `{ <message-body> }`                  (anonymous)
//   - `<dotted-name> { <message-body> }`    (named)
//   - `"""<proto-source>"""`                (source-form file)
//   - `b"<base64-FileDescriptorSet>"`       (descriptor)
//
// AT_PROTO is current on entry. For the brace-bounded shapes (anonymous
// and named), the body is captured as raw bytes between `{` and the
// matching `}` (exclusive); the contents are protobuf source and are
// NOT decoded as PXF entries. Returns the directive plus the byte
// offset immediately after the directive's last token.
func (p *parser) parseProtoDirective() (*ProtoDirective, int, error) {
	leading := p.flushComments()
	atPos := p.current.Pos
	pd := &ProtoDirective{
		Pos:             atPos,
		LeadingComments: leading,
	}
	p.advance() // consume @proto

	switch p.current.Kind {
	case LBRACE:
		// Anonymous: @proto { <message-body> }
		pd.Shape = ProtoAnonymous
		body, end, err := p.captureBraceBody("@proto (anonymous form)")
		if err != nil {
			return nil, 0, err
		}
		pd.Body = body
		return pd, end, nil

	case IDENT:
		// Named: @proto <dotted-name> { <message-body> }
		pd.Shape = ProtoNamed
		pd.TypeName = p.current.Value
		p.advance()
		if p.current.Kind != LBRACE {
			return nil, 0, errorf(p.current.Pos, "expected '{' after @proto %s, got %s", pd.TypeName, p.current.Kind)
		}
		body, end, err := p.captureBraceBody("@proto " + pd.TypeName)
		if err != nil {
			return nil, 0, err
		}
		pd.Body = body
		return pd, end, nil

	case STRING:
		// Source: @proto """<proto-source>""" (triple-quoted) or "<...>"
		// (simple-quoted, accepted lenient v1). The lexer's STRING token
		// Value already has the unescaped content with delimiters
		// stripped and (for triple-quoted) leading-LF / dedent applied.
		pd.Shape = ProtoSource
		pd.Body = []byte(p.current.Value)
		endOffset := p.lex.pos
		p.advance()
		return pd, endOffset, nil

	case BYTES:
		// Descriptor: @proto b"<base64-FileDescriptorSet>"
		pd.Shape = ProtoDescriptor
		raw := p.current.Value
		decoded, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			// Try URL-safe alphabet (allowed per draft §3.7).
			decoded, err = base64.URLEncoding.DecodeString(raw)
			if err != nil {
				return nil, 0, errorf(p.current.Pos, "@proto descriptor body: invalid base64: %v", err)
			}
		}
		pd.Body = decoded
		endOffset := p.lex.pos
		p.advance()
		return pd, endOffset, nil

	default:
		return nil, 0, errorf(p.current.Pos,
			"expected '{', dotted identifier, triple-quoted string, or b\"...\" after @proto, got %s",
			p.current.Kind)
	}
}

// captureBraceBody slices the raw bytes between `{` and the matching
// `}` (both exclusive) without decoding the contents as PXF. The
// parser's current token must be LBRACE on entry. Repositions the
// lexer to the byte right after the closing `}` and primes the parser
// to that token. Used by parseProtoDirective for anonymous/named
// @proto bodies, whose interior is protobuf source rather than PXF.
func (p *parser) captureBraceBody(label string) ([]byte, int, error) {
	open := p.current.Pos.Offset
	close := findMatchingBrace(p.lex.input, open)
	if close < 0 {
		return nil, 0, errorf(p.current.Pos, "%s: unmatched '{'", label)
	}
	body := p.lex.input[open+1 : close]
	// Walk the lexer past the closing `}` one byte at a time so line/col
	// remain accurate for subsequent error messages.
	for p.lex.pos <= close {
		p.lex.advance()
	}
	p.advance() // prime current token past `}`
	return body, close + 1, nil
}

// containsDot reports whether s has a '.' rune. Inlined here so we
// don't pull strings.Contains into parser.go for one call site.
func containsDot(s string) bool {
	for i := range len(s) {
		if s[i] == '.' {
			return true
		}
	}
	return false
}

// parseDirective reads `@<name> *(<prefix-id>) [{ ... }]`. The
// AT_DIRECTIVE token is current on entry. Returns the directive plus
// the byte offset immediately after the directive's last token (the
// `}` for block form, the last prefix identifier for bare form, or
// `@<name>` if neither is present).
//
// The grammar accepts zero-or-more prefix identifiers between `@<name>`
// and the optional `{ ... }` block (draft §3.4.2). Specific directive
// registrations may impose a cardinality; the parser does not.
func (p *parser) parseDirective() (*Directive, int, error) {
	leading := p.flushComments()
	atPos := p.current.Pos
	name := p.current.Value
	if _, reserved := futureReservedDirectives[name]; reserved {
		// In tolerant mode, record but keep parsing the directive with
		// open-grammar rules, so the rest of the document still gets an
		// AST.
		if err := p.soft(errorf(atPos, "@%s is a spec-reserved directive name with no v1 semantics (draft §3.4.6)", name)); err != nil {
			return nil, 0, err
		}
	}
	d := &Directive{
		Pos:             atPos,
		Name:            name,
		LeadingComments: leading,
	}
	endOffset := atPos.Offset + 1 + len(name) // `@` + name
	p.advance()                               // consume AT_DIRECTIVE

	// Zero-or-more prefix identifiers. PXF is whitespace-insignificant,
	// so we can't end the prefix run at a newline. Instead, one-token
	// lookahead disambiguates: an IDENT followed by `=` or `:` is a
	// body field key, not a directive prefix.
	for p.current.Kind == IDENT {
		switch p.peekKind() {
		case EQUALS, COLON:
			// p.current is the first body entry's key; leave it for
			// the body parser.
			goto prefixesDone
		}
		d.Prefixes = append(d.Prefixes, p.current.Value)
		endOffset = p.current.Pos.Offset + len(p.current.Value)
		p.advance()
	}
prefixesDone:
	// Back-compat: a single prefix identifier populates the legacy
	// Type field, matching v0.72.0's single-Type shape so existing
	// consumers (e.g. chameleon's `@header T { ... }` reader) keep
	// working unchanged.
	if len(d.Prefixes) == 1 {
		d.Type = d.Prefixes[0]
	}

	// Optional inline block. Use parseBlockVal so the inner content is
	// validated (string / brace / comment well-formedness); then slice
	// the raw bytes between { and } from the input for Body.
	if p.current.Kind == LBRACE {
		open := p.current.Pos.Offset
		if _, err := p.parseBlockVal(0); err != nil {
			return nil, 0, err
		}
		close := findMatchingBrace(p.lex.input, open)
		if close < 0 {
			// parseBlockVal succeeded, so a matching brace must exist —
			// this is defensive belt-and-braces.
			return nil, 0, errorf(d.Pos, "directive @%s: unmatched '{'", d.Name)
		}
		d.Body = p.lex.input[open+1 : close]
		endOffset = close + 1
	}
	return d, endOffset, nil
}

// findMatchingBrace returns the offset of the `}` that matches the `{`
// at openOffset. Returns -1 on unterminated input. Mirrors the lexer's
// string / comment handling so braces inside literals don't confuse
// the brace count.
func findMatchingBrace(input []byte, openOffset int) int {
	depth := 1
	i := openOffset + 1
	for i < len(input) {
		ch := input[i]
		switch {
		case ch == '{':
			depth++
			i++
		case ch == '}':
			depth--
			if depth == 0 {
				return i
			}
			i++
		case ch == '"':
			i = skipDirString(input, i)
			if i < 0 {
				return -1
			}
		case ch == 'b' && i+1 < len(input) && input[i+1] == '"':
			i = skipDirBytes(input, i)
			if i < 0 {
				return -1
			}
		case ch == '#':
			i = skipDirEOL(input, i+1)
		case ch == '/' && i+1 < len(input) && input[i+1] == '/':
			i = skipDirEOL(input, i+2)
		case ch == '/' && i+1 < len(input) && input[i+1] == '*':
			j := i + 2
			closed := false
			for j+1 < len(input) {
				if input[j] == '*' && input[j+1] == '/' {
					j += 2
					closed = true
					break
				}
				j++
			}
			if !closed {
				return -1
			}
			i = j
		default:
			i++
		}
	}
	return -1
}

func skipDirString(input []byte, i int) int {
	if i+2 < len(input) && input[i+1] == '"' && input[i+2] == '"' {
		j := i + 3
		for j+2 < len(input) {
			if input[j] == '"' && input[j+1] == '"' && input[j+2] == '"' {
				return j + 3
			}
			j++
		}
		return -1
	}
	j := i + 1
	for j < len(input) {
		if input[j] == '\\' {
			if j+1 >= len(input) {
				return -1
			}
			j += 2
			continue
		}
		if input[j] == '"' {
			return j + 1
		}
		if input[j] == '\n' {
			return -1
		}
		j++
	}
	return -1
}

func skipDirBytes(input []byte, i int) int {
	j := i + 2 // past `b"`
	for j < len(input) {
		if input[j] == '"' {
			return j + 1
		}
		if input[j] == '\n' {
			return -1
		}
		j++
	}
	return -1
}

func skipDirEOL(input []byte, i int) int {
	for i < len(input) && input[i] != '\n' {
		i++
	}
	return i
}

// parseEntry, parseValue, parseList, parseBlockVal, and parseBody thread an
// explicit depth counter for HARDENING.md § Recursion. depth is the number of
// open '{' / '[' the parser has descended into; rejection happens when a fresh
// descent would push depth past MaxNestingDepth.
//
// allowMapEntry gates the `:` (map-entry) form: false at document top level,
// true inside any '{ ... }' block where the surrounding field could be a
// map<K,V>. See docs/grammar.ebnf → field_entry, map_entry.
func (p *parser) parseEntry(depth int, allowMapEntry bool) (Entry, error) {
	leading := p.flushComments()

	pos := p.current.Pos
	if p.current.Kind != IDENT && p.current.Kind != STRING && p.current.Kind != INT {
		if !p.tolerant {
			return nil, errorf(pos, "expected identifier, string, or integer, got %s (%q)", p.current.Kind, p.current.Value)
		}
		// Skip the offending token and let the caller's loop retry at
		// the next one. Hand the leading comments back so they attach
		// to the next real entry.
		p.record(errorf(pos, "expected identifier, string, or integer, got %s", p.tokenErrMsg()))
		p.comments = append(leading, p.comments...)
		p.advance()
		return nil, nil
	}
	keyKind := p.current.Kind
	key := p.current.Value
	p.advance()

	switch p.current.Kind {
	case EQUALS:
		// `=` denotes a field assignment on a proto message; the key is
		// an identifier (= proto field name) or a string literal (quoted
		// entry name, draft -01 §3.13 — the grammar accepts it everywhere,
		// the schema layer restricts it to keyed repeated fields). Integer
		// keys are only valid with `:` (map entries).
		if keyKind == INT {
			// Tolerant mode records and keeps the entry as an assignment.
			if err := p.soft(errorf(pos,
				"field assignment with '=' requires an identifier or string key, got %s (%q); use ':' for map entries",
				keyKind, key)); err != nil {
				return nil, err
			}
		}
		p.advance()
		val, err := p.parseValue(depth)
		if err != nil {
			return nil, err
		}
		trailing := p.takeTrailingComment(val.end())
		return &Assignment{Pos: pos, End: val.end(), Key: key, KeyQuoted: keyKind == STRING, Value: val, LeadingComments: leading, TrailingComment: trailing}, nil

	case COLON:
		// Map entry. Only allowed inside a '{ ... }' block, never at
		// document top level.
		if !allowMapEntry {
			// Tolerant mode records and keeps the entry as a map entry.
			if err := p.soft(errorf(pos,
				"map entry (':' form) is only allowed inside a '{ … }' block; use '=' for top-level field assignments")); err != nil {
				return nil, err
			}
		}
		p.advance()
		val, err := p.parseValue(depth)
		if err != nil {
			return nil, err
		}
		trailing := p.takeTrailingComment(val.end())
		return &MapEntry{Pos: pos, End: val.end(), Key: key, Value: val, LeadingComments: leading, TrailingComment: trailing}, nil

	case LBRACE:
		// `{ ... }` denotes a submessage field; the name is an identifier
		// or a string literal (quoted entry name, draft -01 §3.13). Same
		// integer-key rule as `=` applies.
		if keyKind == INT {
			if err := p.soft(errorf(pos,
				"submessage block requires an identifier or string key, got %s (%q)",
				keyKind, key)); err != nil {
				return nil, err
			}
		}
		if depth+1 > MaxNestingDepth {
			if err := p.soft(errorf(p.current.Pos, "nesting depth exceeds MaxNestingDepth=%d", MaxNestingDepth)); err != nil {
				return nil, err
			}
			// Too deep to descend; skip the whole block, keep the entry.
			end := p.skipBalanced(LBRACE, RBRACE)
			return &Block{Pos: pos, End: end, Name: key, NameQuoted: keyKind == STRING, LeadingComments: leading}, nil
		}
		p.advance() // consume {
		entries, bodyEnd, err := p.parseBody(depth + 1)
		if err != nil {
			return nil, err
		}
		return &Block{Pos: pos, End: bodyEnd, Name: key, NameQuoted: keyKind == STRING, Entries: entries, LeadingComments: leading}, nil

	default:
		if err := p.soft(errorf(p.current.Pos, "expected '=', ':', or '{' after %q, got %s", key, p.current.Kind)); err != nil {
			return nil, err
		}
		// A dangling key — the mid-edit shape completion fires on.
		// Keep it as an assignment with a BadVal so tooling still sees
		// the key and its position; do not consume the current token,
		// which is most likely the next entry's key. The BadVal anchors
		// just past the key, where the missing value belongs.
		bad := &BadVal{Pos: p.prevEnd, End: p.prevEnd}
		return &Assignment{Pos: pos, End: bad.End, Key: key, Value: bad, LeadingComments: leading}, nil
	}
}

func (p *parser) parseValue(depth int) (Value, error) {
	pos := p.current.Pos

	if p.tolerant && p.currentStartsEntry() {
		// The current token is the NEXT entry's key, not this value —
		// the value is missing (the `key =` dangling mid-edit shape).
		// Do not consume it; anchor the placeholder and the error just
		// past the separator, where the value belongs.
		p.record(errorf(p.prevEnd, "missing value"))
		return &BadVal{Pos: p.prevEnd, End: p.prevEnd}, nil
	}

	switch p.current.Kind {
	case STRING:
		v := &StringVal{Pos: pos, End: p.tokenEnd(), Value: p.current.Value}
		p.advance()
		return v, nil

	case INT:
		v := &IntVal{Pos: pos, End: p.tokenEnd(), Raw: p.current.Value}
		p.advance()
		return v, nil

	case FLOAT:
		v := &FloatVal{Pos: pos, End: p.tokenEnd(), Raw: p.current.Value}
		p.advance()
		return v, nil

	case BOOL:
		v := &BoolVal{Pos: pos, End: p.tokenEnd(), Value: p.current.Value == "true"}
		p.advance()
		return v, nil

	case BYTES:
		decoded, err := decodeBase64Lenient(p.current.Value)
		if err != nil {
			if err := p.soft(errorf(pos, "invalid base64: %v", err)); err != nil {
				return nil, err
			}
			p.advance()
			return &BadVal{Pos: pos, End: pos}, nil
		}
		v := &BytesVal{Pos: pos, End: p.tokenEnd(), Value: decoded}
		p.advance()
		return v, nil

	case TIMESTAMP:
		t, err := time.Parse(time.RFC3339Nano, p.current.Value)
		if err != nil {
			t, err = time.Parse(time.RFC3339, p.current.Value)
			if err != nil {
				if err := p.soft(errorf(pos, "invalid timestamp %q: %v", p.current.Value, err)); err != nil {
					return nil, err
				}
				p.advance()
				return &BadVal{Pos: pos, End: pos}, nil
			}
		}
		v := &TimestampVal{Pos: pos, End: p.tokenEnd(), Value: t, Raw: p.current.Value}
		p.advance()
		return v, nil

	case DURATION:
		d, err := time.ParseDuration(p.current.Value)
		if err != nil {
			if err := p.soft(errorf(pos, "invalid duration %q: %v", p.current.Value, err)); err != nil {
				return nil, err
			}
			p.advance()
			return &BadVal{Pos: pos, End: pos}, nil
		}
		v := &DurationVal{Pos: pos, End: p.tokenEnd(), Value: d, Raw: p.current.Value}
		p.advance()
		return v, nil

	case NULL:
		v := &NullVal{Pos: pos, End: p.tokenEnd()}
		p.advance()
		return v, nil

	case IDENT:
		v := &IdentVal{Pos: pos, End: p.tokenEnd(), Name: p.current.Value}
		p.advance()
		return v, nil

	case LBRACKET:
		return p.parseList(depth)

	case LBRACE:
		return p.parseBlockVal(depth)

	default:
		if !p.tolerant {
			return nil, errorf(pos, "expected value, got %s (%q)", p.current.Kind, p.current.Value)
		}
		p.record(errorf(pos, "expected value, got %s", p.tokenErrMsg()))
		switch p.current.Kind {
		case EOF, RBRACE, RBRACKET:
			// Leave closers for the enclosing construct to consume.
		default:
			p.advance()
		}
		return &BadVal{Pos: pos, End: pos}, nil
	}
}

func (p *parser) parseList(depth int) (Value, error) {
	if depth+1 > MaxNestingDepth {
		if err := p.soft(errorf(p.current.Pos, "nesting depth exceeds MaxNestingDepth=%d", MaxNestingDepth)); err != nil {
			return nil, err
		}
		pos := p.current.Pos
		end := p.skipBalanced(LBRACKET, RBRACKET)
		return &ListVal{Pos: pos, End: end}, nil
	}
	pos := p.current.Pos
	p.advance() // consume [

	elems := make([]Value, 0, 4)
	for p.current.Kind != RBRACKET && p.current.Kind != EOF {
		before := p.current.Pos.Offset
		elem, err := p.parseValue(depth + 1)
		if err != nil {
			return nil, err
		}
		if p.tolerant && p.current.Pos.Offset == before {
			if _, bad := elem.(*BadVal); bad {
				// parseValue recovered without consuming anything (e.g.
				// a stray '}' that closes an enclosing block, or a token
				// that starts the next entry); bail out of the list so
				// the enclosing construct can resynchronize.
				break
			}
		}
		elems = append(elems, elem)
		if p.current.Kind == COMMA {
			p.advance()
		}
	}
	if p.current.Kind != RBRACKET {
		// Tolerant mode closes the list here, keeping the elements
		// parsed so far.
		if err := p.soft(errorf(p.current.Pos, "expected ']', got %s", p.current.Kind)); err != nil {
			return nil, err
		}
		return &ListVal{Pos: pos, End: p.current.Pos, Elements: elems}, nil
	}
	listEnd := p.tokenEnd()
	p.advance()
	return &ListVal{Pos: pos, End: listEnd, Elements: elems}, nil
}

func (p *parser) parseBlockVal(depth int) (Value, error) {
	if depth+1 > MaxNestingDepth {
		if err := p.soft(errorf(p.current.Pos, "nesting depth exceeds MaxNestingDepth=%d", MaxNestingDepth)); err != nil {
			return nil, err
		}
		pos := p.current.Pos
		end := p.skipBalanced(LBRACE, RBRACE)
		return &BlockVal{Pos: pos, End: end}, nil
	}
	pos := p.current.Pos
	p.advance() // consume {
	entries, bodyEnd, err := p.parseBody(depth + 1)
	if err != nil {
		return nil, err
	}
	return &BlockVal{Pos: pos, End: bodyEnd, Entries: entries}, nil
}

func (p *parser) parseBody(depth int) ([]Entry, Position, error) {
	entries := make([]Entry, 0, 4)
	for p.current.Kind != RBRACE && p.current.Kind != EOF {
		// Inside a '{ ... }' block we don't know whether the surrounding
		// field is a message or a map<K,V>; both forms are accepted and
		// disambiguated by the schema layer.
		entry, err := p.parseEntry(depth, true)
		if err != nil {
			return nil, Position{}, err
		}
		// Tolerant mode returns (nil, nil) for an entry it skipped past.
		if entry != nil {
			entries = append(entries, entry)
		}
	}
	if p.current.Kind != RBRACE {
		if !p.tolerant {
			return nil, Position{}, errorf(p.current.Pos, "expected '}', got %s", p.current.Kind)
		}
		// Unclosed block: close it at EOF with the entries parsed so far.
		p.record(errorf(p.current.Pos, "unclosed block: expected '}' before end of input"))
		return entries, p.current.Pos, nil
	}
	end := p.tokenEnd()
	p.advance()
	return entries, end, nil
}

// decodeBase64Lenient decodes a bytes-literal payload accepting both
// standard and URL-safe alphabets, with or without padding, matching
// the lexer's acceptance rule (RFC 4648 §5, referenced by draft §3.7).
func decodeBase64Lenient(raw string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err == nil {
		return decoded, nil
	}
	if decoded, e := base64.RawStdEncoding.DecodeString(raw); e == nil {
		return decoded, nil
	}
	if decoded, e := base64.URLEncoding.DecodeString(raw); e == nil {
		return decoded, nil
	}
	if decoded, e := base64.RawURLEncoding.DecodeString(raw); e == nil {
		return decoded, nil
	}
	return nil, err
}
