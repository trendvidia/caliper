// Copyright 2026 TrendVidia LLC
// SPDX-License-Identifier: MIT

package pxf

// Per-row proto binding for @dataset rows. Sits atop the streaming
// DatasetReader (DatasetReader.Scan) and is also exported as a standalone
// helper (BindRow) for callers that iterate the materializing path's
// Result.Datasets()[i].Rows.
//
// Implementation strategy: convert each non-nil cell back to its PXF
// text representation, concatenate as a `<column> = <value>\n` body,
// and run through the existing Unmarshal pipeline. That reuses every
// branch of the existing decoder — WKT timestamps and durations,
// wrapper-type nullability, enum-by-name resolution, pxf.required /
// pxf.default, oneof handling — instead of growing a parallel
// Value-to-FieldDescriptor switch with ~50 arms. The cost is a small
// format + reparse per row; that's an acceptable trade for a row-
// streaming API whose consumers have already opted into the
// convenience tier.

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strconv"

	"google.golang.org/protobuf/proto"
)

// Scan reads the next row and binds its cells to the matching fields
// of msg by column name. Returns [io.EOF] when the row sequence is
// exhausted; returns the same sticky error as [DatasetReader.Next] on
// any read or parse failure.
//
// msg's descriptor MUST resolve the field names this reader's
// Columns() list refers to. Type compatibility against the @dataset
// header's declared type is the caller's responsibility — a row
// whose columns don't match msg's fields surfaces as a per-field
// "field not found" or type-mismatch error from the underlying
// Unmarshal call.
//
// Cell-state semantics (mirrors draft §3.4.4):
//
//   - nil Value (empty cell) — field absent. (pxf.default) is
//     applied if declared on the field; (pxf.required) errors if
//     neither default nor value is present.
//   - *NullVal — field cleared, per §3.9 (clears optional /
//     wrapper / oneof; rejects on non-nullable scalars).
//   - any other Value — field set to that value.
func (tr *DatasetReader) Scan(msg proto.Message) error {
	row, err := tr.Next()
	if err != nil {
		return err
	}
	return BindRow(msg, tr.columns, row)
}

// BindRow binds row.Cells to the fields of msg by column name. The
// columns slice MUST have the same length as row.Cells; mismatch is
// a programmer error and panics.
//
// Exported so callers iterating the materializing path's
// Result.Datasets()[i].Rows can reuse the same logic. Same cell-state
// semantics as [DatasetReader.Scan].
func BindRow(msg proto.Message, columns []string, row DatasetRow) error {
	if len(columns) != len(row.Cells) {
		return fmt.Errorf("pxf: BindRow: %d columns vs %d cells", len(columns), len(row.Cells))
	}
	body, err := rowToPXFBody(columns, row)
	if err != nil {
		return err
	}
	// Run the synthetic body through the standard unmarshal pipeline.
	// SkipValidate avoids re-running the reserved-name check per row
	// (DatasetReader's NewDatasetReader / the materializing UnmarshalFull
	// already validated the descriptor once at bind time).
	return UnmarshalOptions{SkipValidate: true}.Unmarshal(body, msg)
}

// rowToPXFBody renders a row as a PXF body: one `<column> = <value>`
// entry per non-nil cell, in column order. Empty cells produce no
// entry (the field stays absent from the decoder's perspective).
func rowToPXFBody(columns []string, row DatasetRow) ([]byte, error) {
	var buf bytes.Buffer
	for i, cell := range row.Cells {
		if cell == nil {
			continue
		}
		buf.WriteString(columns[i])
		buf.WriteString(" = ")
		if err := writeCellValue(&buf, cell); err != nil {
			return nil, err
		}
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// writeCellValue formats a single cell value as PXF text. v1 @dataset
// cells are scalar-shaped (no list, no block), so we only handle the
// leaf-value variants — list and block AST nodes are unreachable here
// because parseDatasetRow / consumeRowCell rejects them before the
// streaming reader hands them to BindRow.
func writeCellValue(buf *bytes.Buffer, v Value) error {
	switch v := v.(type) {
	case *StringVal:
		buf.WriteString(strconv.Quote(v.Value))
	case *IntVal:
		buf.WriteString(v.Raw)
	case *FloatVal:
		buf.WriteString(v.Raw)
	case *BoolVal:
		if v.Value {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case *BytesVal:
		buf.WriteString(`b"`)
		buf.WriteString(base64.StdEncoding.EncodeToString(v.Value))
		buf.WriteByte('"')
	case *NullVal:
		buf.WriteString("null")
	case *IdentVal:
		buf.WriteString(v.Name)
	case *TimestampVal:
		buf.WriteString(v.Raw)
	case *DurationVal:
		buf.WriteString(v.Raw)
	default:
		return fmt.Errorf("pxf: BindRow: unexpected cell value type %T (v1 @dataset cells are scalar-shaped)", v)
	}
	return nil
}
