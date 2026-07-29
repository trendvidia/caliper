// Copyright 2026 TrendVidia LLC
// SPDX-License-Identifier: MIT

package pxf

import (
	"bytes"
	"encoding/base64"
	"fmt"
)

// FormatDocument pretty-prints an AST Document, preserving comments.
// Unlike Marshal (which works from a proto.Message and loses comments),
// this formats directly from the parsed AST.
func FormatDocument(doc *Document) []byte {
	var buf bytes.Buffer
	f := &formatter{buf: &buf, indent: "  "}

	if doc.TypeURL != "" {
		buf.WriteString("@type ")
		buf.WriteString(doc.TypeURL)
		buf.WriteString("\n\n")
	}

	f.writeComments(doc.LeadingComments, 0)
	f.formatEntries(doc.Entries, 0)

	return buf.Bytes()
}

// FormatValue renders v as its PXF source literal — the exact text the
// formatter emits for a value inside [FormatDocument]. It is the
// single-value entry point for tools that splice a rendered value back
// into a buffer, so they need not re-implement PXF literal rendering
// (string quoting, raw int/float forms, base64 bytes, enum idents,
// durations, timestamps).
//
// Multi-line values (lists, block values) render with two-space
// indentation and no trailing newline; the first line carries no
// leading indent, so a caller splicing into an indented context must
// re-indent the continuation lines itself. The [Rewriter] does this
// automatically — prefer its methods when editing a document in place.
//
// A [BadVal] renders as nothing (it spans no source), so formatting a
// value tree that contains one may not reparse.
func FormatValue(v Value) []byte {
	return AppendValue(nil, v)
}

// AppendValue appends the PXF source literal of v to dst and returns
// the extended slice. It is the allocation-friendly form of
// [FormatValue]; see that function for the rendering details.
func AppendValue(dst []byte, v Value) []byte {
	buf := bytes.NewBuffer(dst)
	f := &formatter{buf: buf, indent: "  "}
	f.formatValue(v, 0)
	return buf.Bytes()
}

type formatter struct {
	buf    *bytes.Buffer
	indent string
}

func (f *formatter) writeIndent(level int) {
	for range level {
		f.buf.WriteString(f.indent)
	}
}

func (f *formatter) writeComments(comments []Comment, level int) {
	for _, c := range comments {
		f.writeIndent(level)
		f.buf.WriteString(c.Text)
		f.buf.WriteByte('\n')
	}
}

func (f *formatter) formatEntries(entries []Entry, level int) {
	for _, entry := range entries {
		switch e := entry.(type) {
		case *Assignment:
			f.writeComments(e.LeadingComments, level)
			f.writeIndent(level)
			if e.KeyQuoted {
				fmt.Fprintf(f.buf, "%q", e.Key)
			} else {
				f.buf.WriteString(e.Key)
			}
			f.buf.WriteString(" = ")
			f.formatValue(e.Value, level)
			if e.TrailingComment != "" {
				f.buf.WriteString(" ")
				f.buf.WriteString(e.TrailingComment)
			}
			f.buf.WriteByte('\n')

		case *MapEntry:
			f.writeComments(e.LeadingComments, level)
			f.writeIndent(level)
			if needsQuoting(e.Key) {
				fmt.Fprintf(f.buf, "%q", e.Key)
			} else {
				f.buf.WriteString(e.Key)
			}
			f.buf.WriteString(": ")
			f.formatValue(e.Value, level)
			if e.TrailingComment != "" {
				f.buf.WriteString(" ")
				f.buf.WriteString(e.TrailingComment)
			}
			f.buf.WriteByte('\n')

		case *Block:
			f.writeComments(e.LeadingComments, level)
			f.writeIndent(level)
			if e.NameQuoted {
				fmt.Fprintf(f.buf, "%q", e.Name)
			} else {
				f.buf.WriteString(e.Name)
			}
			f.buf.WriteString(" {\n")
			f.formatEntries(e.Entries, level+1)
			f.writeIndent(level)
			f.buf.WriteString("}\n")
		}
	}
}

func (f *formatter) formatValue(val Value, level int) {
	switch v := val.(type) {
	case *StringVal:
		fmt.Fprintf(f.buf, "%q", v.Value)
	case *IntVal:
		f.buf.WriteString(v.Raw)
	case *FloatVal:
		f.buf.WriteString(v.Raw)
	case *BoolVal:
		if v.Value {
			f.buf.WriteString("true")
		} else {
			f.buf.WriteString("false")
		}
	case *BytesVal:
		f.buf.WriteString(`b"`)
		f.buf.WriteString(base64.StdEncoding.EncodeToString(v.Value))
		f.buf.WriteByte('"')
	case *NullVal:
		f.buf.WriteString("null")
	case *BadVal:
		// A tolerant-parse placeholder has no source text; emitting
		// nothing means the output may not reparse (documented on
		// BadVal). Explicit so the omission is a decision, not a
		// fall-through.
	case *IdentVal:
		f.buf.WriteString(v.Name)
	case *TimestampVal:
		f.buf.WriteString(v.Raw)
	case *DurationVal:
		f.buf.WriteString(v.Raw)
	case *ListVal:
		f.buf.WriteString("[\n")
		for i, elem := range v.Elements {
			f.writeIndent(level + 1)
			f.formatValue(elem, level+1)
			if i < len(v.Elements)-1 {
				f.buf.WriteByte(',')
			}
			f.buf.WriteByte('\n')
		}
		f.writeIndent(level)
		f.buf.WriteByte(']')
	case *BlockVal:
		f.buf.WriteString("{\n")
		f.formatEntries(v.Entries, level+1)
		f.writeIndent(level)
		f.buf.WriteByte('}')
	}
}

func needsQuoting(s string) bool {
	if s == "" {
		return true
	}
	for i, r := range s {
		if i == 0 {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_') {
				return true
			}
		} else {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_') {
				return true
			}
		}
	}
	return false
}
