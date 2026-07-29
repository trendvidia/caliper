// Copyright 2026 TrendVidia LLC
// SPDX-License-Identifier: MIT

package pxf

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// MaxNestingDepth caps PXF block / list nesting and PB submessage nesting,
// per protowire/docs/HARDENING.md § Recursion. The limit applies to the
// recursive-descent decoders for both formats; iterative skip routines
// (skipBraced/skipBracketed) use the same cap implicitly because they
// only run on already-bounded inputs.
const MaxNestingDepth = 100

// fastSet writes v into msg's fd. When msg's underlying implementation
// exposes SetUnsafe (the trendvidia/protobuf-go fork's addition on
// *dynamicpb.Message) we can skip the runtime typecheck because the
// decoder has already established v's type from fd.Kind() one frame
// up. That avoids the v.Interface() boxing alloc inside
// dynamicpb.typecheck (~16 B per scalar set on values outside Go's
// small-int pool) and is the largest remaining win on PXF unmarshal
// into dynamic messages.
//
// The fast path is opt-in: when consumers depend on upstream
// google.golang.org/protobuf (no SetUnsafe method), the type
// assertion fails and we fall through to [protoreflect.Message.Set].
// This keeps protowire-go compiling against either backend.
func fastSet(msg protoreflect.Message, fd protoreflect.FieldDescriptor, v protoreflect.Value) {
	if u, ok := msg.(interface {
		SetUnsafe(protoreflect.FieldDescriptor, protoreflect.Value)
	}); ok {
		u.SetUnsafe(fd, v)
		return
	}
	msg.Set(fd, v)
}

// fastAppend mirrors [fastSet] for repeating-field elements: skips the
// per-Append typecheck on dynamicpb-backed lists.
func fastAppend(list protoreflect.List, v protoreflect.Value) {
	if u, ok := list.(interface {
		AppendUnsafe(protoreflect.Value)
	}); ok {
		u.AppendUnsafe(v)
		return
	}
	list.Append(v)
}

// fastMapSet mirrors [fastSet] for map entries. dynamicpb's Map.Set
// runs typecheckSingular twice (once for the key, once for the value)
// plus a k.Interface() boxing for the map index — three boxings per
// entry on the standard path. SetUnsafe collapses that to just the
// (necessary) k.Interface() index lookup.
func fastMapSet(m protoreflect.Map, k protoreflect.MapKey, v protoreflect.Value) {
	if u, ok := m.(interface {
		SetUnsafe(protoreflect.MapKey, protoreflect.Value)
	}); ok {
		u.SetUnsafe(k, v)
		return
	}
	m.Set(k, v)
}

// directDecoder fuses lexing and decoding in a single pass,
// writing directly into a proto.Message without intermediate AST nodes.
type directDecoder struct {
	lex            lexer
	current        Token
	resolver       TypeResolver
	discardUnknown bool
	depth          int                            // nesting depth, capped at MaxNestingDepth
	result         *Result                        // nil for plain Unmarshal, non-nil for UnmarshalFull
	rootMsg        protoreflect.Message           // top-level message (for _null FieldMask writes)
	nullMaskFd     protoreflect.FieldDescriptor   // cached _null field, may be nil
	pathPrefix     string                         // dotted path prefix for nested messages
	onSecret       func(path, value string) error // optional pxf.Secret scalar-shorthand hook (see UnmarshalOptions.OnSecretField)
	keyedElem      *keyedElemState                // context for the next decodeFields call: it decodes one element of a keyed repeated field (draft -01 §3.13)
}

func (d *directDecoder) advance() {
	for {
		d.current = d.lex.Next()
		if d.current.Kind != COMMENT && d.current.Kind != NEWLINE {
			return
		}
	}
}

// peekKind returns the next significant token kind without consuming it.
// Used by consumeDirective to disambiguate prefix identifiers from body
// field keys (an IDENT followed by `=` or `:` is the latter).
func (d *directDecoder) peekKind() TokenKind {
	pos, line, col := d.lex.pos, d.lex.line, d.lex.col
	saved := d.current
	d.advance()
	next := d.current.Kind
	d.lex.pos, d.lex.line, d.lex.col = pos, line, col
	d.current = saved
	return next
}

func unmarshalDirect(data []byte, msg protoreflect.Message, resolver TypeResolver, discardUnknown bool, onSecret func(path, value string) error) error {
	var d directDecoder
	d.lex = lexer{input: data, line: 1, col: 1}
	d.resolver = resolver
	d.discardUnknown = discardUnknown
	d.onSecret = onSecret
	d.advance()

	if err := d.consumeDirectives(nil); err != nil {
		return err
	}

	return d.decodeFields(msg, false)
}

func unmarshalDirectFull(data []byte, msg protoreflect.Message, resolver TypeResolver, discardUnknown, skipPostDecode bool, onSecret func(path, value string) error) (*Result, error) {
	var d directDecoder
	d.lex = lexer{input: data, line: 1, col: 1}
	d.resolver = resolver
	d.discardUnknown = discardUnknown
	d.onSecret = onSecret
	d.result = newResult()
	d.rootMsg = msg
	d.nullMaskFd = findNullMaskField(msg.Descriptor())
	d.advance()

	if err := d.consumeDirectives(d.result); err != nil {
		return nil, err
	}

	if err := d.decodeFields(msg, false); err != nil {
		return nil, err
	}
	if !skipPostDecode {
		if err := postDecode(msg, d.result, d.nullMaskFd, ""); err != nil {
			return nil, err
		}
	}
	return d.result, nil
}

// consumeDirectives drains any leading `@type` / `@<name>` / `@dataset`
// directives, recording @<name> and @dataset entries on result (if
// non-nil). Returns when the current token is the first body token.
//
// Enforces the @dataset standalone constraint (draft §3.4.4): a document
// containing any @dataset directive MUST NOT also carry @type or any
// top-level field entries.
func (d *directDecoder) consumeDirectives(result *Result) error {
	sawType := false
	var firstDatasetPos Position
	hasDataset := false
	for {
		switch d.current.Kind {
		case AT_TYPE:
			if hasDataset {
				return errorf(d.current.Pos, "@dataset directive cannot coexist with @type (draft §3.4.4)")
			}
			sawType = true
			d.advance()
			if d.current.Kind != IDENT {
				return errorf(d.current.Pos, "expected type name after @type, got %s", d.current.Kind)
			}
			d.advance()
		case AT_DIRECTIVE:
			dir, err := d.consumeDirective()
			if err != nil {
				return err
			}
			if result != nil {
				result.directives = append(result.directives, dir)
			}
		case AT_DATASET:
			if sawType {
				return errorf(d.current.Pos, "@dataset directive cannot coexist with @type (draft §3.4.4)")
			}
			ds, err := d.consumeDatasetDirective()
			if err != nil {
				return err
			}
			if !hasDataset {
				firstDatasetPos = ds.Pos
				hasDataset = true
			}
			if result != nil {
				result.datasets = append(result.datasets, ds)
			}
		case AT_PROTO:
			pd, err := d.consumeProtoDirective()
			if err != nil {
				return err
			}
			if result != nil {
				result.protos = append(result.protos, pd)
			}
		default:
			if hasDataset && d.current.Kind != EOF {
				return errorf(firstDatasetPos,
					"@dataset directive cannot coexist with top-level field entries (draft §3.4.4)")
			}
			return nil
		}
	}
}

// consumeProtoDirective mirrors parser.parseProtoDirective for the
// direct-decode path. AT_PROTO is current on entry. Handles all four
// body shapes (anonymous, named, source, descriptor) per draft §3.4.5.
func (d *directDecoder) consumeProtoDirective() (ProtoDirective, error) {
	pd := ProtoDirective{Pos: d.current.Pos}
	d.advance() // consume @proto

	switch d.current.Kind {
	case LBRACE:
		pd.Shape = ProtoAnonymous
		body, err := d.captureBraceBody("@proto (anonymous form)")
		if err != nil {
			return pd, err
		}
		pd.Body = body
		return pd, nil

	case IDENT:
		pd.Shape = ProtoNamed
		pd.TypeName = d.current.Value
		d.advance()
		if d.current.Kind != LBRACE {
			return pd, errorf(d.current.Pos, "expected '{' after @proto %s, got %s", pd.TypeName, d.current.Kind)
		}
		body, err := d.captureBraceBody("@proto " + pd.TypeName)
		if err != nil {
			return pd, err
		}
		pd.Body = body
		return pd, nil

	case STRING:
		pd.Shape = ProtoSource
		pd.Body = []byte(d.current.Value)
		d.advance()
		return pd, nil

	case BYTES:
		pd.Shape = ProtoDescriptor
		raw := d.current.Value
		decoded, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			decoded, err = base64.URLEncoding.DecodeString(raw)
			if err != nil {
				return pd, errorf(d.current.Pos, "@proto descriptor body: invalid base64: %v", err)
			}
		}
		pd.Body = decoded
		d.advance()
		return pd, nil

	default:
		return pd, errorf(d.current.Pos,
			"expected '{', dotted identifier, triple-quoted string, or b\"...\" after @proto, got %s",
			d.current.Kind)
	}
}

// captureBraceBody slices the raw bytes between `{` and the matching
// `}` for the direct-decode path; mirrors parser.captureBraceBody.
// LBRACE is current on entry.
func (d *directDecoder) captureBraceBody(label string) ([]byte, error) {
	open := d.current.Pos.Offset
	close := findMatchingBrace(d.lex.input, open)
	if close < 0 {
		return nil, errorf(d.current.Pos, "%s: unmatched '{'", label)
	}
	body := d.lex.input[open+1 : close]
	for d.lex.pos <= close {
		d.lex.advance()
	}
	d.advance()
	return body, nil
}

// consumeDatasetDirective mirrors parser.parseDatasetDirective for the
// direct-decode path. AT_DATASET is current on entry.
func (d *directDecoder) consumeDatasetDirective() (DatasetDirective, error) {
	ds := DatasetDirective{Pos: d.current.Pos}
	d.advance() // consume @dataset

	// Optional row message type; may be omitted when an anonymous
	// @proto precedes the @dataset in document order (draft §3.4.4).
	if d.current.Kind == IDENT {
		ds.Type = d.current.Value
		d.advance()
	}

	if d.current.Kind != LPAREN {
		return ds, errorf(d.current.Pos, "expected '(' to start @dataset column list, got %s", d.current.Kind)
	}
	d.advance()

	if d.current.Kind != IDENT {
		return ds, errorf(d.current.Pos, "@dataset column list must contain at least one field name, got %s", d.current.Kind)
	}
	for {
		if d.current.Kind != IDENT {
			return ds, errorf(d.current.Pos, "expected column field name, got %s", d.current.Kind)
		}
		colName := d.current.Value
		if containsDot(colName) {
			return ds, errorf(d.current.Pos, "@dataset column %q: dotted column paths are not supported in v1 (draft §3.4.4)", colName)
		}
		ds.Columns = append(ds.Columns, colName)
		d.advance()
		if d.current.Kind == COMMA {
			d.advance()
			continue
		}
		if d.current.Kind == RPAREN {
			break
		}
		return ds, errorf(d.current.Pos, "expected ',' or ')' in @dataset column list, got %s", d.current.Kind)
	}
	d.advance() // consume )

	for d.current.Kind == LPAREN {
		row, err := d.consumeDatasetRow(len(ds.Columns))
		if err != nil {
			return ds, err
		}
		ds.Rows = append(ds.Rows, row)
	}
	return ds, nil
}

// consumeDatasetRow mirrors parser.parseDatasetRow. The fast-path decoder
// re-uses the AST-tier parser internally for cell values; this keeps
// the direct-decode entry point complete (and lets UnmarshalFull
// expose rows via Result.Datasets()) while reserving an inline-bind
// optimization for a future change. LPAREN is current on entry.
func (d *directDecoder) consumeDatasetRow(expected int) (DatasetRow, error) {
	pos := d.current.Pos
	d.advance() // consume (

	row := DatasetRow{Pos: pos, Cells: make([]Value, 0, expected)}
	cell, err := d.consumeRowCell()
	if err != nil {
		return row, err
	}
	row.Cells = append(row.Cells, cell)
	for d.current.Kind == COMMA {
		d.advance()
		cell, err := d.consumeRowCell()
		if err != nil {
			return row, err
		}
		row.Cells = append(row.Cells, cell)
	}
	if d.current.Kind != RPAREN {
		return row, errorf(d.current.Pos, "expected ',' or ')' in @dataset row, got %s", d.current.Kind)
	}
	d.advance()
	if len(row.Cells) != expected {
		return row, errorf(pos, "@dataset row has %d cells, expected %d (column count)", len(row.Cells), expected)
	}
	return row, nil
}

// consumeRowCell consumes one cell of a @dataset row. Returns nil for
// an empty cell (no value between commas, or at row start/end).
// Rejects list / block values per v1 cell-grammar.
func (d *directDecoder) consumeRowCell() (Value, error) {
	switch d.current.Kind {
	case COMMA, RPAREN:
		return nil, nil
	case LBRACKET:
		return nil, errorf(d.current.Pos, "@dataset cells cannot contain list values in v1 (draft §3.4.4)")
	case LBRACE:
		return nil, errorf(d.current.Pos, "@dataset cells cannot contain block values in v1 (draft §3.4.4)")
	}
	// Re-use the AST-tier value parser by handing off to a small inline
	// reader. We construct a one-shot parser around the current lexer
	// state so the value-shape branches stay in one place.
	return d.consumeValue()
}

// consumeValue parses one PXF value at the current decoder position,
// covering the same shapes as parser.parseValue but using the
// directDecoder's state. Used by @dataset row cells.
func (d *directDecoder) consumeValue() (Value, error) {
	pos := d.current.Pos
	switch d.current.Kind {
	case STRING:
		v := &StringVal{Pos: pos, Value: d.current.Value}
		d.advance()
		return v, nil
	case INT:
		v := &IntVal{Pos: pos, Raw: d.current.Value}
		d.advance()
		return v, nil
	case FLOAT:
		v := &FloatVal{Pos: pos, Raw: d.current.Value}
		d.advance()
		return v, nil
	case BOOL:
		v := &BoolVal{Pos: pos, Value: d.current.Value == "true"}
		d.advance()
		return v, nil
	case BYTES:
		raw := d.current.Value
		decoded, err := decodeBase64Lenient(raw)
		if err != nil {
			return nil, errorf(pos, "invalid base64: %v", err)
		}
		v := &BytesVal{Pos: pos, Value: decoded}
		d.advance()
		return v, nil
	case TIMESTAMP:
		t, err := time.Parse(time.RFC3339Nano, d.current.Value)
		if err != nil {
			t, err = time.Parse(time.RFC3339, d.current.Value)
			if err != nil {
				return nil, errorf(pos, "invalid timestamp %q: %v", d.current.Value, err)
			}
		}
		v := &TimestampVal{Pos: pos, Value: t, Raw: d.current.Value}
		d.advance()
		return v, nil
	case DURATION:
		dur, err := time.ParseDuration(d.current.Value)
		if err != nil {
			return nil, errorf(pos, "invalid duration %q: %v", d.current.Value, err)
		}
		v := &DurationVal{Pos: pos, Value: dur, Raw: d.current.Value}
		d.advance()
		return v, nil
	case NULL:
		v := &NullVal{Pos: pos}
		d.advance()
		return v, nil
	case IDENT:
		v := &IdentVal{Pos: pos, Name: d.current.Value}
		d.advance()
		return v, nil
	default:
		return nil, errorf(pos, "expected value, got %s (%q)", d.current.Kind, d.current.Value)
	}
}

// consumeDirective consumes `@<name> *(<prefix-id>) [{ ... }]`. The
// leading AT_DIRECTIVE token is current on entry. Mirrors parser.go's
// parseDirective: zero-or-more prefix identifiers (draft §3.4.2),
// with the single-prefix case populating the legacy Type field for
// v0.72.0 back-compat.
func (d *directDecoder) consumeDirective() (Directive, error) {
	if _, reserved := futureReservedDirectives[d.current.Value]; reserved {
		return Directive{}, errorf(d.current.Pos, "@%s is a spec-reserved directive name with no v1 semantics (draft §3.4.6)", d.current.Value)
	}
	dir := Directive{
		Pos:  d.current.Pos,
		Name: d.current.Value,
	}
	d.advance() // consume AT_DIRECTIVE

	for d.current.Kind == IDENT {
		switch d.peekKind() {
		case EQUALS, COLON:
			// d.current is the first body entry's key; leave it.
			goto prefixesDone
		}
		dir.Prefixes = append(dir.Prefixes, d.current.Value)
		d.advance()
	}
prefixesDone:
	if len(dir.Prefixes) == 1 {
		dir.Type = dir.Prefixes[0]
	}

	if d.current.Kind == LBRACE {
		open := d.current.Pos.Offset
		close := findMatchingBrace(d.lex.input, open)
		if close < 0 {
			return dir, errorf(dir.Pos, "directive @%s: unmatched '{'", dir.Name)
		}
		dir.Body = d.lex.input[open+1 : close]
		// Re-seat the lexer just past the closing `}` and re-derive
		// line/col so error messages remain accurate after the jump.
		d.lex.pos = close + 1
		d.lex.line, d.lex.col = lineColAt(d.lex.input, close+1)
		d.advance()
	}
	return dir, nil
}

// lineColAt returns the (1-based line, 1-based column) of byte offset
// off in input. Used to re-seat the lexer after a directive block jump.
func lineColAt(input []byte, off int) (int, int) {
	line, col := 1, 1
	if off > len(input) {
		off = len(input)
	}
	for i := 0; i < off; i++ {
		if input[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}

// decodeFields reads key=value / key{} entries into msg.
// If inBlock, it expects and consumes a closing '}'.
//
// Each entry into decodeFields counts as one level of recursive descent
// for HARDENING.md § Recursion: the top-level call is depth 1, the first
// nested submessage depth 2, and so on. The depth counter is decremented
// on return so siblings see the correct depth.
func (d *directDecoder) decodeFields(msg protoreflect.Message, inBlock bool) error {
	d.depth++
	if d.depth > MaxNestingDepth {
		return errorf(d.current.Pos, "nesting depth exceeds MaxNestingDepth=%d", MaxNestingDepth)
	}
	defer func() { d.depth-- }()

	desc := msg.Descriptor()
	fields := desc.Fields()
	var setOneofs map[string]string

	// A non-nil keyedElem applies to exactly this body: the immediate
	// entries of one element of a keyed repeated field. Consume it so
	// nested submessages don't inherit the key-field checks.
	ke := d.keyedElem
	d.keyedElem = nil

	for {
		if inBlock && d.current.Kind == RBRACE {
			d.advance()
			return nil
		}
		if d.current.Kind == EOF {
			if inBlock {
				return errorf(d.current.Pos, "expected '}', got EOF")
			}
			return nil
		}

		pos := d.current.Pos
		if d.current.Kind != IDENT && d.current.Kind != STRING && d.current.Kind != INT {
			return errorf(pos, "expected identifier, string, or integer, got %s (%q)", d.current.Kind, d.current.Value)
		}
		keyQuoted := d.current.Kind == STRING
		key := d.current.Value
		d.advance()

		switch d.current.Kind {
		case EQUALS:
			d.advance()
			if keyQuoted {
				// Grammar accepts a string at entry-name position
				// everywhere; the schema layer restricts it to keyed
				// repeated fields' blocks (draft -01 §3.13), which have
				// their own decode loop — in message context a quoted
				// name never names a field.
				return quotedNameUnkeyedError(pos, key)
			}
			fd := fields.ByName(protoreflect.Name(key))
			if fd == nil {
				if d.discardUnknown {
					d.skipValue()
					continue
				}
				return errorf(pos, "unknown field %q in %s", key, desc.FullName())
			}
			if err := checkOneofDirect(fd, &setOneofs, pos); err != nil {
				return err
			}
			if d.current.Kind == NULL {
				if d.result != nil {
					path := d.pathPrefix + string(fd.Name())
					d.result.markNull(path)
					if d.nullMaskFd != nil {
						addToNullMask(d.rootMsg, d.nullMaskFd, path)
					}
				}
				d.advance()
				continue
			}
			if ke != nil && fd.Name() == ke.keyName && d.current.Kind == STRING {
				// Explicit assignment to the element's key field: the
				// empty string is never a valid key, and in the named
				// (keyed-block) form the value must agree with the
				// entry name (draft -01 §3.13).
				if err := ke.checkExplicitKey(d.current.Value, d.current.Pos); err != nil {
					return err
				}
			}
			if d.result != nil {
				d.result.markPresent(d.pathPrefix + string(fd.Name()))
			}
			if err := d.decodeFieldValue(msg, fd); err != nil {
				return err
			}

		case LBRACE:
			d.advance()
			if keyQuoted {
				return quotedNameUnkeyedError(pos, key)
			}
			fd := fields.ByName(protoreflect.Name(key))
			if fd == nil {
				if d.discardUnknown {
					d.skipBraced()
					continue
				}
				return errorf(pos, "unknown field %q in %s", key, desc.FullName())
			}
			if fd.Kind() != protoreflect.MessageKind && fd.Kind() != protoreflect.GroupKind {
				return errorf(pos, "field %q is not a message type, cannot use block syntax", key)
			}
			if fd.IsList() {
				// Keyed repeated field (draft -01 §3.13): the block form
				// is a sequence of named entries, one per element.
				if keyFd := KeyField(fd); keyFd != nil {
					if d.result != nil {
						d.result.markPresent(d.pathPrefix + string(fd.Name()))
					}
					if err := d.decodeKeyedBlockBody(msg, fd, keyFd); err != nil {
						return err
					}
					continue
				}
				if d.current.Kind == STRING {
					// The block spells the keyed form on a field with no
					// (pxf.key); report the quoted entry name — the more
					// specific schema violation — rather than the
					// generic shape error.
					return quotedNameUnkeyedError(d.current.Pos, d.current.Value)
				}
				return errorf(pos, "repeated field %q must use list syntax: %s = [...]", key, key)
			}
			if fd.IsMap() {
				return errorf(pos, "map field %q must use assignment syntax: %s = { ... }", key, key)
			}
			if err := checkOneofDirect(fd, &setOneofs, pos); err != nil {
				return err
			}
			if d.result != nil {
				d.result.markPresent(d.pathPrefix + string(fd.Name()))
			}
			// pxf.Secret block form with hook: route `value` through
			// d.onSecret the same way scalar shorthand does, leaving
			// Secret.value empty on the proto message. Hint and
			// fingerprint subfields are assigned normally — they are
			// diagnostic, not sensitive.
			if d.onSecret != nil && isSecret(fd.Message()) {
				sub := msg.Mutable(fd).Message()
				path := d.pathPrefix + string(fd.Name())
				if err := d.decodeSecretBlockInto(sub, path); err != nil {
					return err
				}
				continue
			}
			// Any with block syntax: name { @type = "..." ... }
			if isAny(fd.Message()) && d.resolver != nil && d.current.Kind == AT_TYPE {
				if err := d.decodeAnyBlock(msg, fd); err != nil {
					return err
				}
				continue
			}
			sub := msg.Mutable(fd).Message()
			saved := d.pathPrefix
			d.pathPrefix = d.pathPrefix + string(fd.Name()) + "."
			if err := d.decodeFields(sub, true); err != nil {
				return err
			}
			d.pathPrefix = saved

		case COLON:
			return errorf(pos, "unexpected ':' in message context, use '=' for field assignments")

		default:
			return errorf(d.current.Pos, "expected '=', ':', or '{' after %q, got %s", key, d.current.Kind)
		}
	}
}

func checkOneofDirect(fd protoreflect.FieldDescriptor, setOneofs *map[string]string, pos Position) error {
	oo := fd.ContainingOneof()
	if oo == nil || oo.IsSynthetic() {
		return nil
	}
	name := string(oo.Name())
	if *setOneofs == nil {
		*setOneofs = make(map[string]string, 2)
	}
	if prev, ok := (*setOneofs)[name]; ok {
		return errorf(pos, "oneof %q: field %q conflicts with already-set field %q", name, fd.Name(), prev)
	}
	(*setOneofs)[name] = string(fd.Name())
	return nil
}

func (d *directDecoder) decodeFieldValue(msg protoreflect.Message, fd protoreflect.FieldDescriptor) error {
	if fd.IsMap() {
		return d.decodeMapInline(msg, fd)
	}
	if fd.IsList() {
		// Keyed repeated field written `name = { ... }`: a block-tail is
		// an abbreviation of `= { ... }` (draft -01 §3.13), so the
		// assignment spelling of the keyed block form is equally valid.
		if d.current.Kind == LBRACE {
			if keyFd := KeyField(fd); keyFd != nil {
				d.advance()
				return d.decodeKeyedBlockBody(msg, fd, keyFd)
			}
		}
		return d.decodeListInline(msg, fd)
	}
	if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
		return d.decodeMsgValue(msg, fd)
	}
	v, err := d.consumeScalar(fd)
	if err != nil {
		return err
	}
	fastSet(msg, fd, v)
	return nil
}

// markInnerPresent records `<pathPrefix><field>.<name>` in the result
// for each inner-field name. Used by WKT scalar-shorthand decoders so
// presence tracking stays consistent with what block-form parsing
// produces — `pw = "x"` and `pw { value = "x" }` both leave
// `pw.value` marked present.
//
// Only meaningful for top-level decodeMsgValue. consumeListMsg and
// decodeMapInline don't track per-element inner-field presence
// (the parent list/map field is the unit of presence in those
// contexts).
func (d *directDecoder) markInnerPresent(fd protoreflect.FieldDescriptor, names ...string) {
	if d.result == nil {
		return
	}
	base := d.pathPrefix + string(fd.Name())
	for _, name := range names {
		d.result.markPresent(base + "." + name)
	}
}

func (d *directDecoder) decodeMsgValue(msg protoreflect.Message, fd protoreflect.FieldDescriptor) error {
	mdesc := fd.Message()

	if isTimestamp(mdesc) && d.current.Kind == TIMESTAMP {
		t, err := time.Parse(time.RFC3339Nano, d.current.Value)
		if err != nil {
			t, err = time.Parse(time.RFC3339, d.current.Value)
			if err != nil {
				return errorf(d.current.Pos, "invalid timestamp %q: %v", d.current.Value, err)
			}
		}
		sub := msg.Mutable(fd).Message()
		setTimestampFields(sub, t)
		d.markInnerPresent(fd, "seconds", "nanos")
		d.advance()
		return nil
	}
	if isDuration(mdesc) && d.current.Kind == DURATION {
		dur, err := time.ParseDuration(d.current.Value)
		if err != nil {
			return errorf(d.current.Pos, "invalid duration %q: %v", d.current.Value, err)
		}
		sub := msg.Mutable(fd).Message()
		setDurationFields(sub, dur)
		d.markInnerPresent(fd, "seconds", "nanos")
		d.advance()
		return nil
	}
	if _, ok := wrapperTypes[mdesc.FullName()]; ok && d.current.Kind != LBRACE {
		innerFd := mdesc.Fields().ByName("value")
		v, err := d.consumeScalar(innerFd)
		if err != nil {
			return err
		}
		sub := msg.Mutable(fd).Message()
		fastSet(sub, innerFd, v)
		d.markInnerPresent(fd, "value")
		return nil
	}
	if isBigInt(mdesc) && d.current.Kind == INT {
		bi, err := parseBigInt(d.current.Value)
		if err != nil {
			return errorf(d.current.Pos, "%v", err)
		}
		sub := msg.Mutable(fd).Message()
		setBigIntFields(sub, bi)
		d.markInnerPresent(fd, "abs", "negative")
		d.advance()
		return nil
	}
	if isDecimal(mdesc) && (d.current.Kind == INT || d.current.Kind == FLOAT) {
		unscaled, scale, negative, err := parseDecimal(d.current.Value)
		if err != nil {
			return errorf(d.current.Pos, "%v", err)
		}
		sub := msg.Mutable(fd).Message()
		setDecimalFields(sub, unscaled, scale, negative)
		d.markInnerPresent(fd, "unscaled", "scale", "negative")
		d.advance()
		return nil
	}
	if isBigFloat(mdesc) && (d.current.Kind == INT || d.current.Kind == FLOAT) {
		bf, err := parseBigFloat(d.current.Value)
		if err != nil {
			return errorf(d.current.Pos, "%v", err)
		}
		sub := msg.Mutable(fd).Message()
		setBigFloatFields(sub, bf)
		d.markInnerPresent(fd, "mantissa", "exponent", "prec", "negative")
		d.advance()
		return nil
	}
	// pxf.Secret: scalar shorthand `pw = "x"`. Block form is intercepted
	// in decodeFields' LBRACE case (and for repeated/map elements in
	// consumeListMsg / decodeMapInline) — when OnSecretField is set,
	// the inner `value` assignment routes through the hook the same
	// way scalar shorthand does, closing the plaintext-in-heap window
	// for both surface forms.
	if isSecret(mdesc) && d.current.Kind == STRING {
		innerFd := mdesc.Fields().ByName("value")
		if d.onSecret != nil {
			pos := d.current.Pos
			value := d.current.Value
			if !utf8.ValidString(value) {
				return errorf(pos, "invalid UTF-8 in pxf.Secret value for field %q", fd.Name())
			}
			path := d.pathPrefix + string(fd.Name())
			if err := d.onSecret(path, value); err != nil {
				return errorf(pos, "pxf.Secret hook for %q: %v", path, err)
			}
			d.advance()
			// Mutate the message so presence reporting / hint+fingerprint
			// access still work, but leave the inner `value` field unset.
			msg.Mutable(fd).Message()
			d.markInnerPresent(fd, "value")
			return nil
		}
		v, err := d.consumeScalar(innerFd)
		if err != nil {
			return err
		}
		sub := msg.Mutable(fd).Message()
		fastSet(sub, innerFd, v)
		d.markInnerPresent(fd, "value")
		return nil
	}
	// google.protobuf.Any with sugar syntax
	if isAny(mdesc) && d.resolver != nil && d.current.Kind == LBRACE {
		return d.decodeAnyValue(msg, fd)
	}

	if d.current.Kind != LBRACE {
		return errorf(d.current.Pos, "expected '{' for message field %q", fd.Name())
	}
	d.advance()
	sub := msg.Mutable(fd).Message()
	return d.decodeFields(sub, true)
}

// decodeAnyBlock decodes Any sugar when the opening { has already been consumed
// (block syntax: `name { @type = "..." ... }`).
func (d *directDecoder) decodeAnyBlock(msg protoreflect.Message, fd protoreflect.FieldDescriptor) error {
	return d.decodeAnyInner(msg, fd)
}

// decodeAnyValue decodes Any sugar from assignment syntax: `name = { @type = "..." ... }`.
func (d *directDecoder) decodeAnyValue(msg protoreflect.Message, fd protoreflect.FieldDescriptor) error {
	d.advance() // consume {
	return d.decodeAnyInner(msg, fd)
}

func (d *directDecoder) decodeAnyInner(msg protoreflect.Message, fd protoreflect.FieldDescriptor) error {
	if d.current.Kind != AT_TYPE {
		return errorf(d.current.Pos, "Any field requires @type as first entry")
	}
	d.advance() // consume @type
	if d.current.Kind != EQUALS {
		return errorf(d.current.Pos, "expected '=' after @type")
	}
	d.advance()
	if d.current.Kind != STRING {
		return errorf(d.current.Pos, "expected string type URL after @type =")
	}
	typeURL := d.current.Value
	d.advance()

	innerDesc, err := d.resolver.FindMessageByURL(typeURL)
	if err != nil {
		return errorf(d.current.Pos, "cannot resolve Any type %q: %v", typeURL, err)
	}

	inner := dynamicpb.NewMessage(innerDesc)
	if err := d.decodeFields(inner.ProtoReflect(), true); err != nil {
		return err
	}

	packed, err := proto.Marshal(inner)
	if err != nil {
		return fmt.Errorf("cannot marshal Any inner message: %w", err)
	}

	anyMsg := msg.Mutable(fd).Message()
	anyDesc := fd.Message()
	anyMsg.Set(anyDesc.Fields().ByName("type_url"), protoreflect.ValueOfString(typeURL))
	anyMsg.Set(anyDesc.Fields().ByName("value"), protoreflect.ValueOfBytes(packed))
	return nil
}

func (d *directDecoder) decodeListInline(msg protoreflect.Message, fd protoreflect.FieldDescriptor) error {
	if d.current.Kind != LBRACKET {
		return errorf(d.current.Pos, "expected '[' for repeated field %q", fd.Name())
	}
	d.advance()

	list := msg.Mutable(fd).List()
	keyFd := KeyField(fd) // non-nil: anonymous form of a keyed repeated field (draft -01 §3.13)

	for d.current.Kind != RBRACKET && d.current.Kind != EOF {
		if d.current.Kind == NULL {
			return errorf(d.current.Pos, "null is not allowed in repeated field %q", fd.Name())
		}
		if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
			v, err := d.consumeListMsg(fd, list, keyFd)
			if err != nil {
				return err
			}
			fastAppend(list, v)
		} else if fd.Kind() == protoreflect.EnumKind {
			v, err := d.consumeEnum(fd)
			if err != nil {
				return err
			}
			fastAppend(list, v)
		} else {
			v, err := d.consumeScalar(fd)
			if err != nil {
				return err
			}
			fastAppend(list, v)
		}
		if d.current.Kind == COMMA {
			d.advance()
		}
	}

	if d.current.Kind != RBRACKET {
		return errorf(d.current.Pos, "expected ']', got %s", d.current.Kind)
	}
	d.advance()
	return nil
}

// consumeListMsg consumes one message-typed element of a repeated
// field. keyFd is non-nil when fd is a keyed repeated field, in which
// case the element body gets the key-field decode checks of draft -01
// §3.13 (an explicit empty-string key is rejected).
func (d *directDecoder) consumeListMsg(fd protoreflect.FieldDescriptor, list protoreflect.List, keyFd protoreflect.FieldDescriptor) (protoreflect.Value, error) {
	mdesc := fd.Message()

	if isTimestamp(mdesc) && d.current.Kind == TIMESTAMP {
		t, err := time.Parse(time.RFC3339Nano, d.current.Value)
		if err != nil {
			t, err = time.Parse(time.RFC3339, d.current.Value)
			if err != nil {
				return protoreflect.Value{}, errorf(d.current.Pos, "invalid timestamp %q: %v", d.current.Value, err)
			}
		}
		sub := list.NewElement().Message()
		setTimestampFields(sub, t)
		d.advance()
		return protoreflect.ValueOfMessage(sub), nil
	}
	if isDuration(mdesc) && d.current.Kind == DURATION {
		dur, err := time.ParseDuration(d.current.Value)
		if err != nil {
			return protoreflect.Value{}, errorf(d.current.Pos, "invalid duration %q: %v", d.current.Value, err)
		}
		sub := list.NewElement().Message()
		setDurationFields(sub, dur)
		d.advance()
		return protoreflect.ValueOfMessage(sub), nil
	}
	if _, ok := wrapperTypes[mdesc.FullName()]; ok && d.current.Kind != LBRACE {
		innerFd := mdesc.Fields().ByName("value")
		v, err := d.consumeScalar(innerFd)
		if err != nil {
			return protoreflect.Value{}, err
		}
		sub := list.NewElement().Message()
		fastSet(sub, innerFd, v)
		return protoreflect.ValueOfMessage(sub), nil
	}
	if isBigInt(mdesc) && d.current.Kind == INT {
		bi, err := parseBigInt(d.current.Value)
		if err != nil {
			return protoreflect.Value{}, errorf(d.current.Pos, "%v", err)
		}
		sub := list.NewElement().Message()
		setBigIntFields(sub, bi)
		d.advance()
		return protoreflect.ValueOfMessage(sub), nil
	}
	if isDecimal(mdesc) && (d.current.Kind == INT || d.current.Kind == FLOAT) {
		unscaled, scale, negative, err := parseDecimal(d.current.Value)
		if err != nil {
			return protoreflect.Value{}, errorf(d.current.Pos, "%v", err)
		}
		sub := list.NewElement().Message()
		setDecimalFields(sub, unscaled, scale, negative)
		d.advance()
		return protoreflect.ValueOfMessage(sub), nil
	}
	if isBigFloat(mdesc) && (d.current.Kind == INT || d.current.Kind == FLOAT) {
		bf, err := parseBigFloat(d.current.Value)
		if err != nil {
			return protoreflect.Value{}, errorf(d.current.Pos, "%v", err)
		}
		sub := list.NewElement().Message()
		setBigFloatFields(sub, bf)
		d.advance()
		return protoreflect.ValueOfMessage(sub), nil
	}
	// pxf.Secret in repeated context: accept scalar shorthand (a list
	// of plain strings), otherwise fall through to the generic
	// block-form path. With OnSecretField set, both forms route the
	// inner value through the hook; scalar shorthand here, block
	// form via decodeSecretBlockInto below.
	if isSecret(mdesc) && d.current.Kind == STRING {
		innerFd := mdesc.Fields().ByName("value")
		if d.onSecret != nil {
			pos := d.current.Pos
			value := d.current.Value
			if !utf8.ValidString(value) {
				return protoreflect.Value{}, errorf(pos, "invalid UTF-8 in pxf.Secret value for field %q", fd.Name())
			}
			path := fmt.Sprintf("%s%s[%d]", d.pathPrefix, fd.Name(), list.Len())
			if err := d.onSecret(path, value); err != nil {
				return protoreflect.Value{}, errorf(pos, "pxf.Secret hook for %q: %v", path, err)
			}
			d.advance()
			sub := list.NewElement().Message()
			return protoreflect.ValueOfMessage(sub), nil
		}
		v, err := d.consumeScalar(innerFd)
		if err != nil {
			return protoreflect.Value{}, err
		}
		sub := list.NewElement().Message()
		fastSet(sub, innerFd, v)
		return protoreflect.ValueOfMessage(sub), nil
	}
	// pxf.Secret block form in repeated context — route value
	// through the hook the same way scalar shorthand does.
	if d.onSecret != nil && isSecret(mdesc) && d.current.Kind == LBRACE {
		path := fmt.Sprintf("%s%s[%d]", d.pathPrefix, fd.Name(), list.Len())
		d.advance() // consume {
		sub := list.NewElement().Message()
		if err := d.decodeSecretBlockInto(sub, path); err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfMessage(sub), nil
	}

	if d.current.Kind != LBRACE {
		return protoreflect.Value{}, errorf(d.current.Pos, "expected '{' for repeated message element")
	}
	d.advance()
	sub := list.NewElement().Message()
	if keyFd != nil {
		d.keyedElem = &keyedElemState{field: fd.Name(), keyName: keyFd.Name()}
	}
	if err := d.decodeFields(sub, true); err != nil {
		return protoreflect.Value{}, err
	}
	return protoreflect.ValueOfMessage(sub), nil
}

// decodeKeyedBlockBody decodes the block form of a keyed repeated field
// (draft -01 §3.13): a sequence of named entries — `name { ... }` or
// equivalently `name = { ... }` — where each entry name (unquoted
// value, for string-literal names) populates the element's key field
// and entry order is list order. Duplicate entry names within the
// block, the empty string as a name, and a disagreeing explicit
// key-field assignment inside an entry are decode errors ([KeyedError]).
// The opening '{' has been consumed; the closing '}' is consumed before
// returning.
func (d *directDecoder) decodeKeyedBlockBody(msg protoreflect.Message, fd, keyFd protoreflect.FieldDescriptor) error {
	d.depth++
	if d.depth > MaxNestingDepth {
		return errorf(d.current.Pos, "nesting depth exceeds MaxNestingDepth=%d", MaxNestingDepth)
	}
	defer func() { d.depth-- }()

	list := msg.Mutable(fd).List()
	var seen map[string]struct{}
	for {
		switch d.current.Kind {
		case RBRACE:
			d.advance()
			return nil
		case EOF:
			return errorf(d.current.Pos, "expected '}' to close keyed field %q, got EOF", fd.Name())
		case IDENT, STRING:
			// entry name
		default:
			return errorf(d.current.Pos, "expected entry name (identifier or string) in keyed field %q, got %s (%q)",
				fd.Name(), d.current.Kind, d.current.Value)
		}
		namePos := d.current.Pos
		// IDENT token values are zero-copy views into the input; the name
		// is stored into the message, so clone.
		name := strings.Clone(d.current.Value)
		if name == "" {
			return keyedEmptyNameError(namePos, fd.Name())
		}
		if !utf8.ValidString(name) {
			return errorf(namePos, "invalid UTF-8 in entry name for keyed field %q", fd.Name())
		}
		if _, dup := seen[name]; dup {
			return keyedDuplicateKeyError(namePos, fd.Name(), name)
		}
		if seen == nil {
			seen = make(map[string]struct{}, 4)
		}
		seen[name] = struct{}{}
		d.advance()

		switch d.current.Kind {
		case LBRACE:
			d.advance()
		case EQUALS:
			d.advance()
			if d.current.Kind != LBRACE {
				return errorf(d.current.Pos,
					"keyed entry %q of field %q must have a block value ('{ ... }'): the element type is a message",
					name, fd.Name())
			}
			d.advance()
		default:
			return errorf(d.current.Pos, "expected '{' or '=' after entry name %q in keyed field %q, got %s",
				name, fd.Name(), d.current.Kind)
		}

		sub := list.NewElement().Message()
		fastSet(sub, keyFd, protoreflect.ValueOfString(name))
		d.keyedElem = &keyedElemState{field: fd.Name(), keyName: keyFd.Name(), entryName: name, named: true}
		if err := d.decodeFields(sub, true); err != nil {
			return err
		}
		fastAppend(list, protoreflect.ValueOfMessage(sub))
	}
}

func (d *directDecoder) decodeMapInline(msg protoreflect.Message, fd protoreflect.FieldDescriptor) error {
	if d.current.Kind != LBRACE {
		return errorf(d.current.Pos, "expected '{' for map field %q", fd.Name())
	}
	d.advance()

	m := msg.Mutable(fd).Map()
	keyFd := fd.MapKey()
	valFd := fd.MapValue()

	for d.current.Kind != RBRACE && d.current.Kind != EOF {
		pos := d.current.Pos
		if d.current.Kind != IDENT && d.current.Kind != STRING && d.current.Kind != INT {
			return errorf(pos, "expected map key, got %s", d.current.Kind)
		}
		keyStr := strings.Clone(d.current.Value)
		d.advance()

		switch d.current.Kind {
		case COLON:
			d.advance()
		case EQUALS:
			return errorf(d.current.Pos, "unexpected '=' in map, use ':' for map entries")
		default:
			return errorf(d.current.Pos, "expected ':' after map key, got %s", d.current.Kind)
		}

		k, err := decodeMapKey(keyFd, keyStr, pos)
		if err != nil {
			return err
		}

		if d.current.Kind == NULL {
			return errorf(d.current.Pos, "null is not allowed as map value in field %q", fd.Name())
		}

		if valFd.Kind() == protoreflect.MessageKind || valFd.Kind() == protoreflect.GroupKind {
			mdesc := valFd.Message()

			// WKT scalar shortcuts in map-value position. Mirrors the
			// equivalent block in decodeMsgValue / consumeListMsg so
			// `tenants = { "acme": "key" }` works for pxf.Secret,
			// `weights = { "x": 42 }` works for pxf.BigInt, etc.
			if isTimestamp(mdesc) && d.current.Kind == TIMESTAMP {
				t, err := time.Parse(time.RFC3339Nano, d.current.Value)
				if err != nil {
					t, err = time.Parse(time.RFC3339, d.current.Value)
					if err != nil {
						return errorf(d.current.Pos, "invalid timestamp %q: %v", d.current.Value, err)
					}
				}
				sub := m.NewValue().Message()
				setTimestampFields(sub, t)
				d.advance()
				fastMapSet(m, k, protoreflect.ValueOfMessage(sub))
				continue
			}
			if isDuration(mdesc) && d.current.Kind == DURATION {
				dur, err := time.ParseDuration(d.current.Value)
				if err != nil {
					return errorf(d.current.Pos, "invalid duration %q: %v", d.current.Value, err)
				}
				sub := m.NewValue().Message()
				setDurationFields(sub, dur)
				d.advance()
				fastMapSet(m, k, protoreflect.ValueOfMessage(sub))
				continue
			}
			if _, ok := wrapperTypes[mdesc.FullName()]; ok && d.current.Kind != LBRACE {
				innerFd := mdesc.Fields().ByName("value")
				v, err := d.consumeScalar(innerFd)
				if err != nil {
					return err
				}
				sub := m.NewValue().Message()
				fastSet(sub, innerFd, v)
				fastMapSet(m, k, protoreflect.ValueOfMessage(sub))
				continue
			}
			if isBigInt(mdesc) && d.current.Kind == INT {
				bi, err := parseBigInt(d.current.Value)
				if err != nil {
					return errorf(d.current.Pos, "%v", err)
				}
				sub := m.NewValue().Message()
				setBigIntFields(sub, bi)
				d.advance()
				fastMapSet(m, k, protoreflect.ValueOfMessage(sub))
				continue
			}
			if isDecimal(mdesc) && (d.current.Kind == INT || d.current.Kind == FLOAT) {
				unscaled, scale, negative, err := parseDecimal(d.current.Value)
				if err != nil {
					return errorf(d.current.Pos, "%v", err)
				}
				sub := m.NewValue().Message()
				setDecimalFields(sub, unscaled, scale, negative)
				d.advance()
				fastMapSet(m, k, protoreflect.ValueOfMessage(sub))
				continue
			}
			if isBigFloat(mdesc) && (d.current.Kind == INT || d.current.Kind == FLOAT) {
				bf, err := parseBigFloat(d.current.Value)
				if err != nil {
					return errorf(d.current.Pos, "%v", err)
				}
				sub := m.NewValue().Message()
				setBigFloatFields(sub, bf)
				d.advance()
				fastMapSet(m, k, protoreflect.ValueOfMessage(sub))
				continue
			}
			if isSecret(mdesc) && d.current.Kind == STRING {
				innerFd := mdesc.Fields().ByName("value")
				if d.onSecret != nil {
					pos := d.current.Pos
					value := d.current.Value
					if !utf8.ValidString(value) {
						return errorf(pos, "invalid UTF-8 in pxf.Secret value for field %q", fd.Name())
					}
					path := fmt.Sprintf("%s%s[%s]", d.pathPrefix, fd.Name(), formatMapKeyForPath(k))
					if err := d.onSecret(path, value); err != nil {
						return errorf(pos, "pxf.Secret hook for %q: %v", path, err)
					}
					d.advance()
					sub := m.NewValue().Message()
					fastMapSet(m, k, protoreflect.ValueOfMessage(sub))
					continue
				}
				v, err := d.consumeScalar(innerFd)
				if err != nil {
					return err
				}
				sub := m.NewValue().Message()
				fastSet(sub, innerFd, v)
				fastMapSet(m, k, protoreflect.ValueOfMessage(sub))
				continue
			}
			// pxf.Secret block form in map-value context — route value
			// through the hook so plaintext never lands on Secret.value.
			if d.onSecret != nil && isSecret(mdesc) && d.current.Kind == LBRACE {
				path := fmt.Sprintf("%s%s[%s]", d.pathPrefix, fd.Name(), formatMapKeyForPath(k))
				d.advance() // consume {
				sub := m.NewValue().Message()
				if err := d.decodeSecretBlockInto(sub, path); err != nil {
					return err
				}
				fastMapSet(m, k, protoreflect.ValueOfMessage(sub))
				continue
			}

			if d.current.Kind != LBRACE {
				return errorf(d.current.Pos, "expected '{' for map message value")
			}
			d.advance()
			sub := m.NewValue().Message()
			if err := d.decodeFields(sub, true); err != nil {
				return err
			}
			fastMapSet(m, k, protoreflect.ValueOfMessage(sub))
		} else if valFd.Kind() == protoreflect.EnumKind {
			v, err := d.consumeEnum(valFd)
			if err != nil {
				return err
			}
			fastMapSet(m, k, v)
		} else {
			v, err := d.consumeScalar(valFd)
			if err != nil {
				return err
			}
			fastMapSet(m, k, v)
		}
	}

	if d.current.Kind != RBRACE {
		return errorf(d.current.Pos, "expected '}', got %s", d.current.Kind)
	}
	d.advance()
	return nil
}

// isNonFiniteIdent reports whether tok is one of the spec's unsigned
// non-finite float identifiers (§3.8: "inf", "nan"). The signed forms
// "+inf"/"-inf" lex as FLOAT tokens, so they need no special case here.
// strconv.ParseFloat accepts all four spellings directly.
func isNonFiniteIdent(tok Token) bool {
	return tok.Kind == IDENT && (tok.Value == "inf" || tok.Value == "nan")
}

func (d *directDecoder) consumeScalar(fd protoreflect.FieldDescriptor) (protoreflect.Value, error) {
	pos := d.current.Pos

	switch fd.Kind() {
	case protoreflect.StringKind:
		if d.current.Kind != STRING {
			return protoreflect.Value{}, errorf(pos, "expected string for field %q", fd.Name())
		}
		// HARDENING.md § UTF-8: proto3 string fields are valid UTF-8.
		// PXF \xHH and \NNN byte escapes can produce invalid sequences;
		// reject at the assignment site so b"…" / bytes fields stay raw.
		if !utf8.ValidString(d.current.Value) {
			return protoreflect.Value{}, errorf(pos, "invalid UTF-8 in string field %q (use b\"…\" for raw bytes)", fd.Name())
		}
		v := protoreflect.ValueOfString(d.current.Value)
		d.advance()
		return v, nil

	case protoreflect.BoolKind:
		if d.current.Kind != BOOL {
			return protoreflect.Value{}, errorf(pos, "expected bool for field %q", fd.Name())
		}
		v := protoreflect.ValueOfBool(d.current.Value == "true")
		d.advance()
		return v, nil

	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		if d.current.Kind != INT {
			return protoreflect.Value{}, errorf(pos, "expected integer for field %q", fd.Name())
		}
		n, err := strconv.ParseInt(d.current.Value, 10, 32)
		if err != nil {
			return protoreflect.Value{}, errorf(pos, "invalid int32: %s", d.current.Value)
		}
		d.advance()
		return protoreflect.ValueOfInt32(int32(n)), nil

	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		if d.current.Kind != INT {
			return protoreflect.Value{}, errorf(pos, "expected integer for field %q", fd.Name())
		}
		n, err := strconv.ParseInt(d.current.Value, 10, 64)
		if err != nil {
			return protoreflect.Value{}, errorf(pos, "invalid int64: %s", d.current.Value)
		}
		d.advance()
		return protoreflect.ValueOfInt64(n), nil

	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		if d.current.Kind != INT {
			return protoreflect.Value{}, errorf(pos, "expected integer for field %q", fd.Name())
		}
		n, err := strconv.ParseUint(d.current.Value, 10, 32)
		if err != nil {
			return protoreflect.Value{}, errorf(pos, "invalid uint32: %s", d.current.Value)
		}
		d.advance()
		return protoreflect.ValueOfUint32(uint32(n)), nil

	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		if d.current.Kind != INT {
			return protoreflect.Value{}, errorf(pos, "expected integer for field %q", fd.Name())
		}
		n, err := strconv.ParseUint(d.current.Value, 10, 64)
		if err != nil {
			return protoreflect.Value{}, errorf(pos, "invalid uint64: %s", d.current.Value)
		}
		d.advance()
		return protoreflect.ValueOfUint64(n), nil

	case protoreflect.FloatKind:
		if d.current.Kind != FLOAT && d.current.Kind != INT && !isNonFiniteIdent(d.current) {
			return protoreflect.Value{}, errorf(pos, "expected number for field %q", fd.Name())
		}
		f, err := strconv.ParseFloat(d.current.Value, 32)
		if err != nil {
			return protoreflect.Value{}, errorf(pos, "invalid float: %s", d.current.Value)
		}
		d.advance()
		return protoreflect.ValueOfFloat32(float32(f)), nil

	case protoreflect.DoubleKind:
		if d.current.Kind != FLOAT && d.current.Kind != INT && !isNonFiniteIdent(d.current) {
			return protoreflect.Value{}, errorf(pos, "expected number for field %q", fd.Name())
		}
		f, err := strconv.ParseFloat(d.current.Value, 64)
		if err != nil {
			return protoreflect.Value{}, errorf(pos, "invalid double: %s", d.current.Value)
		}
		d.advance()
		return protoreflect.ValueOfFloat64(f), nil

	case protoreflect.BytesKind:
		if d.current.Kind != BYTES {
			return protoreflect.Value{}, errorf(pos, "expected bytes for field %q", fd.Name())
		}
		decoded, err := decodeBase64Lenient(d.current.Value)
		if err != nil {
			return protoreflect.Value{}, errorf(pos, "invalid base64 for field %q: %v", fd.Name(), err)
		}
		d.advance()
		return protoreflect.ValueOfBytes(decoded), nil

	case protoreflect.EnumKind:
		return d.consumeEnum(fd)

	default:
		return protoreflect.Value{}, errorf(pos, "unsupported kind %s for field %q", fd.Kind(), fd.Name())
	}
}

// decodeSecretBlockInto parses a `{ value = "x", hint = "h",
// fingerprint = "f" }` block into the given pxf.Secret-typed message
// sub. Routes the `value` subfield through d.onSecret (which must be
// non-nil — callers gate on this); assigns hint and fingerprint to
// sub normally. Presence is marked on each subfield supplied by the
// source.
//
// The opening `{` MUST have been consumed by the caller. The closing
// `}` is consumed before returning.
//
// Subfield order isn't significant; any of value/hint/fingerprint may
// be absent. Unknown subfields are an error — pxf.Secret is a closed
// shape and tolerating extras would mask schema drift.
//
// This is the block-form counterpart to the scalar-shorthand routing
// in decodeMsgValue, consumeListMsg, and decodeMapInline. Together
// they close the plaintext-in-heap window for both Secret surface
// forms (chameleon's pre-v0.9.0 best-effort residual).
func (d *directDecoder) decodeSecretBlockInto(sub protoreflect.Message, path string) error {
	d.depth++
	if d.depth > MaxNestingDepth {
		return errorf(d.current.Pos, "nesting depth exceeds MaxNestingDepth=%d", MaxNestingDepth)
	}
	defer func() { d.depth-- }()

	desc := sub.Descriptor()
	hintFd := desc.Fields().ByName("hint")
	fingerprintFd := desc.Fields().ByName("fingerprint")

	for {
		switch d.current.Kind {
		case RBRACE:
			d.advance()
			return nil
		case EOF:
			return errorf(d.current.Pos, "expected '}' to close pxf.Secret block, got EOF")
		case IDENT:
			// fall through to handle named subfield
		default:
			return errorf(d.current.Pos, "expected pxf.Secret field name (value/hint/fingerprint), got %s", d.current.Kind)
		}
		name := d.current.Value
		namePos := d.current.Pos
		d.advance()
		if d.current.Kind != EQUALS {
			return errorf(d.current.Pos, "expected '=' after pxf.Secret field %q, got %s", name, d.current.Kind)
		}
		d.advance()
		if d.current.Kind != STRING {
			return errorf(d.current.Pos, "expected string value for pxf.Secret %s, got %s", name, d.current.Kind)
		}
		if !utf8.ValidString(d.current.Value) {
			return errorf(d.current.Pos, "invalid UTF-8 in pxf.Secret %s", name)
		}
		switch name {
		case "value":
			if err := d.onSecret(path, d.current.Value); err != nil {
				return errorf(d.current.Pos, "pxf.Secret hook for %q: %v", path, err)
			}
			if d.result != nil {
				d.result.markPresent(path + ".value")
			}
		case "hint":
			fastSet(sub, hintFd, protoreflect.ValueOfString(d.current.Value))
			if d.result != nil {
				d.result.markPresent(path + ".hint")
			}
		case "fingerprint":
			fastSet(sub, fingerprintFd, protoreflect.ValueOfString(d.current.Value))
			if d.result != nil {
				d.result.markPresent(path + ".fingerprint")
			}
		default:
			return errorf(namePos, "unknown pxf.Secret field %q (expected value/hint/fingerprint)", name)
		}
		d.advance()
	}
}

// formatMapKeyForPath renders a protoreflect.MapKey into the string
// fragment used inside `field[<here>]` for the OnSecretField path.
// Mirrors chameleon's internal/pathfmt.MapKey byte-for-byte: string
// keys are double-quoted (so `tenant_keys[acme]` actually renders as
// `tenant_keys["acme"]`); numeric and bool keys appear bare. The two
// implementations must agree exactly — chameleon's secret.Map lookup
// keys are produced by the same scheme and any drift would silently
// break Get() on map-valued secrets.
func formatMapKeyForPath(k protoreflect.MapKey) string {
	switch v := k.Interface().(type) {
	case string:
		return strconv.Quote(v)
	case int32:
		return strconv.FormatInt(int64(v), 10)
	case int64:
		return strconv.FormatInt(v, 10)
	case uint32:
		return strconv.FormatUint(uint64(v), 10)
	case uint64:
		return strconv.FormatUint(v, 10)
	case bool:
		return strconv.FormatBool(v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func (d *directDecoder) consumeEnum(fd protoreflect.FieldDescriptor) (protoreflect.Value, error) {
	pos := d.current.Pos
	switch d.current.Kind {
	case IDENT:
		ev := fd.Enum().Values().ByName(protoreflect.Name(d.current.Value))
		if ev == nil {
			return protoreflect.Value{}, errorf(pos, "unknown enum value %q for %s", d.current.Value, fd.Enum().FullName())
		}
		d.advance()
		return protoreflect.ValueOfEnum(ev.Number()), nil
	case INT:
		n, err := strconv.ParseInt(d.current.Value, 10, 32)
		if err != nil {
			return protoreflect.Value{}, errorf(pos, "invalid enum number: %s", d.current.Value)
		}
		d.advance()
		return protoreflect.ValueOfEnum(protoreflect.EnumNumber(n)), nil
	default:
		return protoreflect.Value{}, errorf(pos, "expected enum name or number for field %q", fd.Name())
	}
}

// skipValue skips the current value token (scalar, block, or list).
func (d *directDecoder) skipValue() {
	switch d.current.Kind {
	case LBRACE:
		d.advance()
		d.skipBraced()
	case LBRACKET:
		d.advance()
		d.skipBracketed()
	default:
		d.advance() // scalar
	}
}

// skipBraced skips tokens until the matching closing '}'.
func (d *directDecoder) skipBraced() {
	depth := 1
	for depth > 0 && d.current.Kind != EOF {
		switch d.current.Kind {
		case LBRACE:
			depth++
		case RBRACE:
			depth--
		}
		d.advance()
	}
}

// skipBracketed skips tokens until the matching closing ']'.
func (d *directDecoder) skipBracketed() {
	depth := 1
	for depth > 0 && d.current.Kind != EOF {
		switch d.current.Kind {
		case LBRACKET:
			depth++
		case RBRACKET:
			depth--
		}
		d.advance()
	}
}

// addToNullMask appends a dotted field path to the root message's _null FieldMask.
func addToNullMask(rootMsg protoreflect.Message, nullMaskFd protoreflect.FieldDescriptor, path string) {
	fmMsg := rootMsg.Mutable(nullMaskFd).Message()
	pathsFd := fmMsg.Descriptor().Fields().ByName("paths")
	list := fmMsg.Mutable(pathsFd).List()
	list.Append(protoreflect.ValueOfString(path))
}

// postDecode validates required fields and applies defaults, recursing into
// nested messages that were present in the input.
func postDecode(msg protoreflect.Message, result *Result, nullMaskFd protoreflect.FieldDescriptor, pathPrefix string) error {
	desc := msg.Descriptor()
	fields := desc.Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		if nullMaskFd != nil && fd.Number() == nullMaskFd.Number() {
			continue
		}
		path := pathPrefix + string(fd.Name())
		_, isPresent := result.presentFields[path]
		if !isPresent {
			if isRequired(fd) {
				return errorf(Position{Line: 1, Column: 1}, "required field %q is absent", path)
			}
			if def, ok := getDefault(fd); ok {
				if err := applyDefault(msg, fd, def); err != nil {
					return err
				}
			}
		} else if (fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind) &&
			!fd.IsList() && !fd.IsMap() && !result.IsNull(path) && msg.Has(fd) {
			// Recurse into present, non-null message fields.
			sub := msg.Mutable(fd).Message()
			if err := postDecode(sub, result, nil, path+"."); err != nil {
				return err
			}
		}
		// null + default → do NOT apply default (null is intentional)
	}
	return nil
}

// ApplyDefault parses a (pxf.default) value string and sets it on the
// given message field. The string is the same PXF literal form the
// annotation accepts: `42` for ints, `true`/`false` for bools, `"hello"`
// for strings, base64 for bytes, RFC3339 for timestamps inside their
// inner fields, etc.
//
// Exported for layered-config consumers (e.g. chameleon) that run a
// post-merge defaults pass with [UnmarshalOptions.SkipPostDecode].
// In-tree callers (postDecode) use the lowercase alias.
func ApplyDefault(msg protoreflect.Message, fd protoreflect.FieldDescriptor, def string) error {
	return applyDefaultImpl(msg, fd, def)
}

// applyDefault preserves the existing in-package call shape; ApplyDefault
// is the public spelling.
func applyDefault(msg protoreflect.Message, fd protoreflect.FieldDescriptor, def string) error {
	return applyDefaultImpl(msg, fd, def)
}

func applyDefaultImpl(msg protoreflect.Message, fd protoreflect.FieldDescriptor, def string) error {
	switch fd.Kind() {
	case protoreflect.StringKind:
		msg.Set(fd, protoreflect.ValueOfString(def))
	case protoreflect.BoolKind:
		msg.Set(fd, protoreflect.ValueOfBool(def == "true"))
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		n, err := strconv.ParseInt(def, 10, 32)
		if err != nil {
			return fmt.Errorf("invalid default int32 %q for field %q: %w", def, fd.Name(), err)
		}
		msg.Set(fd, protoreflect.ValueOfInt32(int32(n)))
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		n, err := strconv.ParseInt(def, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid default int64 %q for field %q: %w", def, fd.Name(), err)
		}
		msg.Set(fd, protoreflect.ValueOfInt64(n))
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		n, err := strconv.ParseUint(def, 10, 32)
		if err != nil {
			return fmt.Errorf("invalid default uint32 %q for field %q: %w", def, fd.Name(), err)
		}
		msg.Set(fd, protoreflect.ValueOfUint32(uint32(n)))
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		n, err := strconv.ParseUint(def, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid default uint64 %q for field %q: %w", def, fd.Name(), err)
		}
		msg.Set(fd, protoreflect.ValueOfUint64(n))
	case protoreflect.FloatKind:
		f, err := strconv.ParseFloat(def, 32)
		if err != nil {
			return fmt.Errorf("invalid default float %q for field %q: %w", def, fd.Name(), err)
		}
		msg.Set(fd, protoreflect.ValueOfFloat32(float32(f)))
	case protoreflect.DoubleKind:
		f, err := strconv.ParseFloat(def, 64)
		if err != nil {
			return fmt.Errorf("invalid default double %q for field %q: %w", def, fd.Name(), err)
		}
		msg.Set(fd, protoreflect.ValueOfFloat64(f))
	case protoreflect.BytesKind:
		decoded, err := decodeBase64Lenient(def)
		if err != nil {
			return fmt.Errorf("invalid default bytes %q for field %q: %w", def, fd.Name(), err)
		}
		msg.Set(fd, protoreflect.ValueOfBytes(decoded))
	case protoreflect.EnumKind:
		ev := fd.Enum().Values().ByName(protoreflect.Name(def))
		if ev == nil {
			n, err := strconv.ParseInt(def, 10, 32)
			if err != nil {
				return fmt.Errorf("invalid default enum %q for field %q", def, fd.Name())
			}
			msg.Set(fd, protoreflect.ValueOfEnum(protoreflect.EnumNumber(n)))
		} else {
			msg.Set(fd, protoreflect.ValueOfEnum(ev.Number()))
		}
	case protoreflect.MessageKind:
		return applyMessageDefault(msg, fd, def)
	default:
		return fmt.Errorf("default values not supported for kind %s (field %q)", fd.Kind(), fd.Name())
	}
	return nil
}

// applyMessageDefault handles defaults for well-known message types.
func applyMessageDefault(msg protoreflect.Message, fd protoreflect.FieldDescriptor, def string) error {
	mdesc := fd.Message()

	if isTimestamp(mdesc) {
		t, err := time.Parse(time.RFC3339Nano, def)
		if err != nil {
			t, err = time.Parse(time.RFC3339, def)
			if err != nil {
				return fmt.Errorf("invalid default timestamp %q for field %q: %w", def, fd.Name(), err)
			}
		}
		sub := msg.Mutable(fd).Message()
		setTimestampFields(sub, t)
		return nil
	}

	if isDuration(mdesc) {
		dur, err := time.ParseDuration(def)
		if err != nil {
			return fmt.Errorf("invalid default duration %q for field %q: %w", def, fd.Name(), err)
		}
		sub := msg.Mutable(fd).Message()
		setDurationFields(sub, dur)
		return nil
	}

	if innerKind, ok := wrapperTypes[mdesc.FullName()]; ok {
		innerFd := mdesc.Fields().ByName("value")
		sub := msg.Mutable(fd).Message()
		v, err := parseScalarDefault(innerKind, def, fd)
		if err != nil {
			return err
		}
		sub.Set(innerFd, v)
		return nil
	}

	if isBigInt(mdesc) {
		bi, err := parseBigInt(def)
		if err != nil {
			return fmt.Errorf("invalid default big integer %q for field %q: %w", def, fd.Name(), err)
		}
		sub := msg.Mutable(fd).Message()
		setBigIntFields(sub, bi)
		return nil
	}
	if isDecimal(mdesc) {
		unscaled, scale, negative, err := parseDecimal(def)
		if err != nil {
			return fmt.Errorf("invalid default decimal %q for field %q: %w", def, fd.Name(), err)
		}
		sub := msg.Mutable(fd).Message()
		setDecimalFields(sub, unscaled, scale, negative)
		return nil
	}
	if isBigFloat(mdesc) {
		bf, err := parseBigFloat(def)
		if err != nil {
			return fmt.Errorf("invalid default big float %q for field %q: %w", def, fd.Name(), err)
		}
		sub := msg.Mutable(fd).Message()
		setBigFloatFields(sub, bf)
		return nil
	}

	return fmt.Errorf("default values not supported for message type %s (field %q)", mdesc.FullName(), fd.Name())
}

// parseScalarDefault parses a default string for a scalar kind.
func parseScalarDefault(kind protoreflect.Kind, def string, fd protoreflect.FieldDescriptor) (protoreflect.Value, error) {
	switch kind {
	case protoreflect.StringKind:
		return protoreflect.ValueOfString(def), nil
	case protoreflect.BoolKind:
		return protoreflect.ValueOfBool(def == "true"), nil
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		n, err := strconv.ParseInt(def, 10, 32)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("invalid default int32 %q for field %q: %w", def, fd.Name(), err)
		}
		return protoreflect.ValueOfInt32(int32(n)), nil
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		n, err := strconv.ParseInt(def, 10, 64)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("invalid default int64 %q for field %q: %w", def, fd.Name(), err)
		}
		return protoreflect.ValueOfInt64(n), nil
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		n, err := strconv.ParseUint(def, 10, 32)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("invalid default uint32 %q for field %q: %w", def, fd.Name(), err)
		}
		return protoreflect.ValueOfUint32(uint32(n)), nil
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		n, err := strconv.ParseUint(def, 10, 64)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("invalid default uint64 %q for field %q: %w", def, fd.Name(), err)
		}
		return protoreflect.ValueOfUint64(n), nil
	case protoreflect.FloatKind:
		f, err := strconv.ParseFloat(def, 32)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("invalid default float %q for field %q: %w", def, fd.Name(), err)
		}
		return protoreflect.ValueOfFloat32(float32(f)), nil
	case protoreflect.DoubleKind:
		f, err := strconv.ParseFloat(def, 64)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("invalid default double %q for field %q: %w", def, fd.Name(), err)
		}
		return protoreflect.ValueOfFloat64(f), nil
	default:
		return protoreflect.Value{}, fmt.Errorf("unsupported default kind %s for field %q", kind, fd.Name())
	}
}
