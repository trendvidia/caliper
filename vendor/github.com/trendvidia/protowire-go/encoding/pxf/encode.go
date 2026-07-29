// Copyright 2026 TrendVidia LLC
// SPDX-License-Identifier: MIT

package pxf

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// MarshalOptions configures PXF encoding.
type MarshalOptions struct {
	Indent       string       // indentation per level, default "  "
	EmitDefaults bool         // emit fields with zero values
	TypeURL      string       // emit @type directive if non-empty
	TypeResolver TypeResolver // resolve Any type URLs for sugar encoding
	NullFields   *Result      // emit null fields from a Result (for messages without null_mask)
}

// Marshal formats msg as PXF text with default options.
func Marshal(msg proto.Message) ([]byte, error) {
	return MarshalOptions{}.Marshal(msg)
}

// Marshal formats msg as PXF text.
func (o MarshalOptions) Marshal(msg proto.Message) ([]byte, error) {
	if o.Indent == "" {
		o.Indent = "  "
	}
	var buf bytes.Buffer
	enc := &encoder{buf: &buf, indent: o.Indent, emitDefaults: o.EmitDefaults, resolver: o.TypeResolver, nullFields: o.NullFields}

	// Discover null_mask once at the top level.
	desc := msg.ProtoReflect().Descriptor()
	enc.nullMaskFd = findNullMaskField(desc)
	if enc.nullMaskFd != nil && msg.ProtoReflect().Has(enc.nullMaskFd) {
		enc.nullSet = readNullMask(msg.ProtoReflect(), enc.nullMaskFd)
	} else if o.NullFields != nil {
		enc.nullSet = make(map[string]bool, len(o.NullFields.nullFields))
		for path := range o.NullFields.nullFields {
			enc.nullSet[path] = true
		}
	}

	if o.TypeURL != "" {
		buf.WriteString("@type ")
		buf.WriteString(o.TypeURL)
		buf.WriteString("\n\n")
	}
	if err := enc.encodeMessage(msg.ProtoReflect(), 0); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type encoder struct {
	buf          *bytes.Buffer
	indent       string
	emitDefaults bool
	resolver     TypeResolver
	nullFields   *Result
	nullMaskFd   protoreflect.FieldDescriptor // cached, top-level only
	nullSet      map[string]bool              // cached, top-level only
	pathPrefix   string                       // dotted path prefix for nested null lookup
	skipKeyFd    protoreflect.FieldDescriptor // key field to omit from the NEXT encodeMessage call (keyed-block entry body, draft -01 §3.13)
	scratch      [64]byte                     // scratch buffer for number formatting
}

func (e *encoder) writeIndent(level int) {
	for range level {
		e.buf.WriteString(e.indent)
	}
}

// writeFieldPrefix writes: <indent>name =
func (e *encoder) writeFieldPrefix(level int, name protoreflect.Name) {
	e.writeIndent(level)
	e.buf.WriteString(string(name))
	e.buf.WriteString(" = ")
}

func (e *encoder) encodeMessage(msg protoreflect.Message, level int) error {
	fields := msg.Descriptor().Fields()

	// A pending skipKeyFd applies to exactly this body: the entry name
	// of a keyed-block entry already carries the key field's value, so
	// the field is not additionally emitted inside the block (draft -01
	// §3.13). Consume it so nested messages don't inherit the skip.
	skipKey := e.skipKeyFd
	e.skipKeyFd = nil

	for i := range fields.Len() {
		fd := fields.Get(i)
		if skipKey != nil && fd == skipKey {
			continue
		}

		// Skip the _null FieldMask field itself.
		if e.nullMaskFd != nil && e.pathPrefix == "" && fd.Number() == e.nullMaskFd.Number() {
			continue
		}

		path := e.pathPrefix + string(fd.Name())

		// Emit null for fields in the null set.
		if e.nullSet != nil && e.nullSet[path] {
			e.writeFieldPrefix(level, fd.Name())
			e.buf.WriteString("null\n")
			continue
		}

		if !e.emitDefaults && !msg.Has(fd) {
			continue
		}
		val := msg.Get(fd)

		if fd.IsMap() {
			if err := e.encodeMapField(fd, val, level); err != nil {
				return err
			}
			continue
		}

		if fd.IsList() {
			if err := e.encodeListField(fd, val, level); err != nil {
				return err
			}
			continue
		}

		if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
			if !msg.Has(fd) {
				continue
			}
			if err := e.encodeMessageField(fd, val.Message(), level); err != nil {
				return err
			}
			continue
		}

		e.writeFieldPrefix(level, fd.Name())
		if err := e.writeScalar(fd, val); err != nil {
			return err
		}
		e.buf.WriteByte('\n')
	}
	return nil
}

// readNullMask reads the null_mask FieldMask field and returns a set of null field names.
func readNullMask(msg protoreflect.Message, nmFd protoreflect.FieldDescriptor) map[string]bool {
	fmMsg := msg.Get(nmFd).Message()
	pathsFd := fmMsg.Descriptor().Fields().ByName("paths")
	list := fmMsg.Get(pathsFd).List()
	m := make(map[string]bool, list.Len())
	for i := range list.Len() {
		m[list.Get(i).String()] = true
	}
	return m
}

func (e *encoder) encodeMessageField(fd protoreflect.FieldDescriptor, sub protoreflect.Message, level int) error {
	mdesc := fd.Message()

	if isTimestamp(mdesc) {
		t := readTimestamp(sub)
		e.writeFieldPrefix(level, fd.Name())
		e.buf.WriteString(t.Format(time.RFC3339Nano))
		e.buf.WriteByte('\n')
		return nil
	}
	if isDuration(mdesc) {
		d := readDuration(sub)
		e.writeFieldPrefix(level, fd.Name())
		e.buf.WriteString(d.String())
		e.buf.WriteByte('\n')
		return nil
	}
	if isWrapperType(mdesc) {
		innerFd := mdesc.Fields().ByName("value")
		e.writeFieldPrefix(level, fd.Name())
		if err := e.writeScalar(innerFd, sub.Get(innerFd)); err != nil {
			return err
		}
		e.buf.WriteByte('\n')
		return nil
	}
	if isBigInt(mdesc) {
		e.writeFieldPrefix(level, fd.Name())
		e.buf.WriteString(formatBigInt(sub))
		e.buf.WriteByte('\n')
		return nil
	}
	if isDecimal(mdesc) {
		e.writeFieldPrefix(level, fd.Name())
		e.buf.WriteString(readDecimalStr(sub))
		e.buf.WriteByte('\n')
		return nil
	}
	if isBigFloat(mdesc) {
		e.writeFieldPrefix(level, fd.Name())
		e.buf.WriteString(formatBigFloat(sub))
		e.buf.WriteByte('\n')
		return nil
	}
	// pxf.Secret: scalar shorthand iff hint and fingerprint are empty
	// (otherwise we would silently drop authoring metadata on re-emit).
	// The block-form fallthrough handles the metadata case.
	if isSecret(mdesc) && !secretHasMetadata(sub) {
		innerFd := mdesc.Fields().ByName("value")
		e.writeFieldPrefix(level, fd.Name())
		if err := e.writeScalar(innerFd, sub.Get(innerFd)); err != nil {
			return err
		}
		e.buf.WriteByte('\n')
		return nil
	}
	// Any sugar: @type + inline fields
	if isAny(mdesc) && e.resolver != nil {
		encoded, err := e.tryEncodeAny(fd, sub, level)
		if err != nil {
			return err
		}
		if encoded {
			return nil
		}
	}

	e.writeIndent(level)
	e.buf.WriteString(string(fd.Name()))
	e.buf.WriteString(" {\n")
	saved := e.pathPrefix
	e.pathPrefix = e.pathPrefix + string(fd.Name()) + "."
	if err := e.encodeMessage(sub, level+1); err != nil {
		return err
	}
	e.pathPrefix = saved
	e.writeIndent(level)
	e.buf.WriteString("}\n")
	return nil
}

func (e *encoder) encodeListField(fd protoreflect.FieldDescriptor, val protoreflect.Value, level int) error {
	list := val.List()
	if list.Len() == 0 && !e.emitDefaults {
		return nil
	}

	// Keyed repeated field (draft -01 §3.13): emit the keyed block form
	// whenever every element's key is present, non-empty, and distinct;
	// otherwise fall through to the anonymous list form (elements with
	// absent or duplicate keys can only be represented anonymously).
	if keyFd := KeyField(fd); keyFd != nil && list.Len() > 0 {
		if keyedFormEligible(list, keyFd) {
			return e.encodeKeyedList(fd, list, keyFd, level)
		}
	}

	e.writeFieldPrefix(level, fd.Name())
	e.buf.WriteString("[\n")

	for i := range list.Len() {
		elem := list.Get(i)

		if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
			sub := elem.Message()
			mdesc := fd.Message()

			if isTimestamp(mdesc) {
				t := readTimestamp(sub)
				e.writeIndent(level + 1)
				e.buf.WriteString(t.Format(time.RFC3339Nano))
			} else if isDuration(mdesc) {
				d := readDuration(sub)
				e.writeIndent(level + 1)
				e.buf.WriteString(d.String())
			} else if isWrapperType(mdesc) {
				innerFd := mdesc.Fields().ByName("value")
				e.writeIndent(level + 1)
				if err := e.writeScalar(innerFd, sub.Get(innerFd)); err != nil {
					return err
				}
			} else if isBigInt(mdesc) {
				e.writeIndent(level + 1)
				e.buf.WriteString(formatBigInt(sub))
			} else if isDecimal(mdesc) {
				e.writeIndent(level + 1)
				e.buf.WriteString(readDecimalStr(sub))
			} else if isBigFloat(mdesc) {
				e.writeIndent(level + 1)
				e.buf.WriteString(formatBigFloat(sub))
			} else if isSecret(mdesc) && !secretHasMetadata(sub) {
				innerFd := mdesc.Fields().ByName("value")
				e.writeIndent(level + 1)
				if err := e.writeScalar(innerFd, sub.Get(innerFd)); err != nil {
					return err
				}
			} else {
				e.writeIndent(level + 1)
				e.buf.WriteString("{\n")
				if err := e.encodeMessage(sub, level+2); err != nil {
					return err
				}
				e.writeIndent(level + 1)
				e.buf.WriteByte('}')
			}
		} else {
			e.writeIndent(level + 1)
			if err := e.writeScalar(fd, elem); err != nil {
				return err
			}
		}

		if i < list.Len()-1 {
			e.buf.WriteByte(',')
		}
		e.buf.WriteByte('\n')
	}

	e.writeIndent(level)
	e.buf.WriteString("]\n")
	return nil
}

// keyedFormEligible reports whether every element of list has a
// non-empty key value that is distinct within the collection — the
// draft -01 §3.13 condition for emitting the keyed block form.
func keyedFormEligible(list protoreflect.List, keyFd protoreflect.FieldDescriptor) bool {
	seen := make(map[string]struct{}, list.Len())
	for i := range list.Len() {
		key := list.Get(i).Message().Get(keyFd).String()
		if key == "" {
			return false
		}
		if _, dup := seen[key]; dup {
			return false
		}
		seen[key] = struct{}{}
	}
	return true
}

// encodeKeyedList emits a keyed repeated field in the keyed block form:
// one named block per element, in list order. Entry names are written
// unquoted when identifier-safe and quoted otherwise; the key field is
// not additionally emitted inside the entry's block.
func (e *encoder) encodeKeyedList(fd protoreflect.FieldDescriptor, list protoreflect.List, keyFd protoreflect.FieldDescriptor, level int) error {
	e.writeIndent(level)
	e.buf.WriteString(string(fd.Name()))
	e.buf.WriteString(" {\n")
	for i := range list.Len() {
		sub := list.Get(i).Message()
		key := sub.Get(keyFd).String()
		e.writeIndent(level + 1)
		if identSafeEntryName(key) {
			e.buf.WriteString(key)
		} else {
			e.writeQuotedString(key)
		}
		e.buf.WriteString(" {\n")
		e.skipKeyFd = keyFd
		if err := e.encodeMessage(sub, level+2); err != nil {
			return err
		}
		e.writeIndent(level + 1)
		e.buf.WriteString("}\n")
	}
	e.writeIndent(level)
	e.buf.WriteString("}\n")
	return nil
}

func (e *encoder) encodeMapField(fd protoreflect.FieldDescriptor, val protoreflect.Value, level int) error {
	m := val.Map()
	if m.Len() == 0 && !e.emitDefaults {
		return nil
	}

	e.writeFieldPrefix(level, fd.Name())
	e.buf.WriteString("{\n")

	// Collect and sort keys. Pre-format keys once to avoid repeated
	// allocations during sort comparisons.
	type mapKV struct {
		keyStr string
		val    protoreflect.Value
	}
	entries := make([]mapKV, 0, m.Len())
	keyFd := fd.MapKey()
	m.Range(func(k protoreflect.MapKey, v protoreflect.Value) bool {
		entries = append(entries, mapKV{formatMapKey(keyFd, k), v})
		return true
	})
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].keyStr < entries[j].keyStr
	})

	valFd := fd.MapValue()

	for _, kv := range entries {
		if valFd.Kind() == protoreflect.MessageKind || valFd.Kind() == protoreflect.GroupKind {
			sub := kv.val.Message()
			mdesc := valFd.Message()

			// WKT scalar emission in map-value position. Mirrors the
			// equivalent block in encodeMessageField / encodeListField
			// so a Timestamp / Duration / wrapper / BigInt / Decimal /
			// BigFloat / Secret value renders as `key: <scalar>`
			// rather than `key: { value = <scalar> }`. Secret falls
			// back to block form when hint or fingerprint is set, so
			// authoring metadata round-trips.
			if isTimestamp(mdesc) {
				t := readTimestamp(sub)
				e.writeIndent(level + 1)
				e.buf.WriteString(kv.keyStr)
				e.buf.WriteString(": ")
				e.buf.WriteString(t.Format(time.RFC3339Nano))
				e.buf.WriteByte('\n')
				continue
			}
			if isDuration(mdesc) {
				d := readDuration(sub)
				e.writeIndent(level + 1)
				e.buf.WriteString(kv.keyStr)
				e.buf.WriteString(": ")
				e.buf.WriteString(d.String())
				e.buf.WriteByte('\n')
				continue
			}
			if isWrapperType(mdesc) {
				innerFd := mdesc.Fields().ByName("value")
				e.writeIndent(level + 1)
				e.buf.WriteString(kv.keyStr)
				e.buf.WriteString(": ")
				if err := e.writeScalar(innerFd, sub.Get(innerFd)); err != nil {
					return err
				}
				e.buf.WriteByte('\n')
				continue
			}
			if isBigInt(mdesc) {
				e.writeIndent(level + 1)
				e.buf.WriteString(kv.keyStr)
				e.buf.WriteString(": ")
				e.buf.WriteString(formatBigInt(sub))
				e.buf.WriteByte('\n')
				continue
			}
			if isDecimal(mdesc) {
				e.writeIndent(level + 1)
				e.buf.WriteString(kv.keyStr)
				e.buf.WriteString(": ")
				e.buf.WriteString(readDecimalStr(sub))
				e.buf.WriteByte('\n')
				continue
			}
			if isBigFloat(mdesc) {
				e.writeIndent(level + 1)
				e.buf.WriteString(kv.keyStr)
				e.buf.WriteString(": ")
				e.buf.WriteString(formatBigFloat(sub))
				e.buf.WriteByte('\n')
				continue
			}
			if isSecret(mdesc) && !secretHasMetadata(sub) {
				innerFd := mdesc.Fields().ByName("value")
				e.writeIndent(level + 1)
				e.buf.WriteString(kv.keyStr)
				e.buf.WriteString(": ")
				if err := e.writeScalar(innerFd, sub.Get(innerFd)); err != nil {
					return err
				}
				e.buf.WriteByte('\n')
				continue
			}

			e.writeIndent(level + 1)
			e.buf.WriteString(kv.keyStr)
			e.buf.WriteString(": {\n")
			if err := e.encodeMessage(sub, level+2); err != nil {
				return err
			}
			e.writeIndent(level + 1)
			e.buf.WriteString("}\n")
		} else {
			e.writeIndent(level + 1)
			e.buf.WriteString(kv.keyStr)
			e.buf.WriteString(": ")
			if err := e.writeScalar(valFd, kv.val); err != nil {
				return err
			}
			e.buf.WriteByte('\n')
		}
	}

	e.writeIndent(level)
	e.buf.WriteString("}\n")
	return nil
}

// writeScalar writes a scalar value directly to the buffer (no intermediate string).
func (e *encoder) writeScalar(fd protoreflect.FieldDescriptor, val protoreflect.Value) error {
	switch fd.Kind() {
	case protoreflect.StringKind:
		e.writeQuotedString(val.String())
	case protoreflect.BoolKind:
		if val.Bool() {
			e.buf.WriteString("true")
		} else {
			e.buf.WriteString("false")
		}
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		b := strconv.AppendInt(e.scratch[:0], val.Int(), 10)
		e.buf.Write(b)
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		b := strconv.AppendUint(e.scratch[:0], val.Uint(), 10)
		e.buf.Write(b)
	case protoreflect.FloatKind:
		e.writeFloat(val.Float(), 32)
	case protoreflect.DoubleKind:
		e.writeFloat(val.Float(), 64)
	case protoreflect.BytesKind:
		e.buf.WriteString(`b"`)
		e.buf.WriteString(base64.StdEncoding.EncodeToString(val.Bytes()))
		e.buf.WriteByte('"')
	case protoreflect.EnumKind:
		ev := fd.Enum().Values().ByNumber(val.Enum())
		if ev != nil {
			e.buf.WriteString(string(ev.Name()))
		} else {
			b := strconv.AppendInt(e.scratch[:0], int64(val.Enum()), 10)
			e.buf.Write(b)
		}
	default:
		return fmt.Errorf("unsupported kind: %s", fd.Kind())
	}
	return nil
}

func (e *encoder) writeFloat(f float64, bits int) {
	switch {
	case math.IsInf(f, 1):
		e.buf.WriteString("inf")
	case math.IsInf(f, -1):
		e.buf.WriteString("-inf")
	case math.IsNaN(f):
		e.buf.WriteString("nan")
	default:
		b := strconv.AppendFloat(e.scratch[:0], f, 'g', -1, bits)
		e.buf.Write(b)
	}
}

// writeQuotedString writes a Go-style quoted string directly to the buffer.
func (e *encoder) writeQuotedString(s string) {
	e.buf.WriteByte('"')
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch ch {
		case '"':
			e.buf.WriteString(`\"`)
		case '\\':
			e.buf.WriteString(`\\`)
		case '\n':
			e.buf.WriteString(`\n`)
		case '\r':
			e.buf.WriteString(`\r`)
		case '\t':
			e.buf.WriteString(`\t`)
		default:
			if ch < 0x20 {
				e.buf.WriteString(`\x`)
				e.buf.WriteByte("0123456789abcdef"[ch>>4])
				e.buf.WriteByte("0123456789abcdef"[ch&0xf])
			} else {
				e.buf.WriteByte(ch)
			}
		}
	}
	e.buf.WriteByte('"')
}

func formatMapKey(fd protoreflect.FieldDescriptor, key protoreflect.MapKey) string {
	switch fd.Kind() {
	case protoreflect.StringKind:
		s := key.String()
		if isValidIdent(s) {
			return s
		}
		return strconv.Quote(s)
	case protoreflect.BoolKind:
		if key.Bool() {
			return "true"
		}
		return "false"
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return strconv.FormatInt(key.Int(), 10)
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return strconv.FormatUint(key.Uint(), 10)
	default:
		return fmt.Sprint(key.Interface())
	}
}

// tryEncodeAny encodes a google.protobuf.Any with sugar syntax.
// Returns (true, nil) if successful, (false, nil) to fall back to regular encoding.
func (e *encoder) tryEncodeAny(fd protoreflect.FieldDescriptor, anyMsg protoreflect.Message, level int) (bool, error) {
	anyDesc := fd.Message()
	typeURL := anyMsg.Get(anyDesc.Fields().ByName("type_url")).String()
	valueBytes := anyMsg.Get(anyDesc.Fields().ByName("value")).Bytes()

	if typeURL == "" {
		return false, nil
	}

	innerDesc, err := e.resolver.FindMessageByURL(typeURL)
	if err != nil {
		return false, nil // can't resolve, fall back
	}

	inner := dynamicpb.NewMessage(innerDesc)
	if err := proto.Unmarshal(valueBytes, inner); err != nil {
		return false, nil
	}

	e.writeIndent(level)
	e.buf.WriteString(string(fd.Name()))
	e.buf.WriteString(" {\n")

	e.writeIndent(level + 1)
	e.buf.WriteString("@type = ")
	e.writeQuotedString(typeURL)
	e.buf.WriteByte('\n')

	if err := e.encodeMessage(inner.ProtoReflect(), level+1); err != nil {
		return false, err
	}

	e.writeIndent(level)
	e.buf.WriteString("}\n")
	return true, nil
}

func isValidIdent(s string) bool {
	if s == "" || s == "true" || s == "false" || s == "null" {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_') {
				return false
			}
		} else {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_') {
				return false
			}
		}
	}
	return true
}
