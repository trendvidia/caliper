// Copyright 2026 TrendVidia LLC
// SPDX-License-Identifier: MIT

// Package pxf implements the PXF text format codec for protobuf messages.
//
// PXF (Protocol-buffer eXchange Format) is a human-editable, comment-aware
// text encoding for protobuf messages. The format spec, grammar, and
// canonical fixtures live in the sibling repository
// trendvidia/protowire; this package is the Go reference implementation.
//
// # Choosing a code path
//
// Five top-level entry points serve different needs:
//
//   - [Unmarshal] / [Marshal] — fast path. A fused single-pass
//     lexer+decoder writes directly into the [google.golang.org/protobuf/proto.Message]
//     with zero-copy token strings and no AST allocations. Use this in
//     hot paths or when you do not need the source comments.
//   - [UnmarshalFull] — like [Unmarshal] but returns a [Result] tracking
//     which fields were set, null, or absent, validating required
//     fields, and applying defaults from (pxf.required) /
//     (pxf.default) annotations.
//   - [Parse] / [FormatDocument] — AST path. Produces a [Document] with
//     comments attached to entries. Use when you need to round-trip a
//     PXF document while preserving its formatting and comments.
//     [FormatValue] / [AppendValue] render a single value to its source
//     literal for tools that splice values back into a buffer.
//   - [ParseTolerant] — error-tolerant AST path for editor tooling.
//     Recovers at entry/block boundaries and returns a best-effort
//     [Document] plus all positioned errors, so completion and hover
//     keep working on mid-edit buffers that [Parse] would reject.
//   - [NewRewriter] — lossless targeted editing. Splices set/remove
//     edits into the original source by byte span, so machine edits
//     to a hand-written document leave everything else byte-stable:
//     comments, blank lines, ordering, indentation, and formatting
//     quirks. [FormatDocument] remains the explicit, on-demand
//     normalizer. Edits address fields by dotted path
//     ([Rewriter.Set] / [Rewriter.Remove]) or by the AST node the
//     caller holds from [Rewriter.Document] ([Rewriter.ReplaceValue] /
//     [Rewriter.AppendEntry] / [Rewriter.SetSpan]) — the latter reach a
//     specific node among same-key siblings that a path cannot.
//
// # Keyed repeated fields
//
// A repeated message-typed field annotated (pxf.key) = "<field>"
// (draft -01 §3.13) reads and writes as a block of named blocks —
// entry name = key-field value, document order = list order:
//
//	children {
//	  greeting    { type = "Label" }
//	  counter_row { type = "HBox"  }
//	}
//
// The Unmarshal family decodes both this and the anonymous list form;
// [Marshal] emits the keyed form whenever every key is present,
// non-empty, and distinct. [CanonicalizeKeyed] plus [FormatDocument]
// canonicalize a parsed document the same way (the reference `pxf fmt`
// pipeline), [KeyedDiagnostics] surfaces the keyed schema checks as
// positioned diagnostics for editor tooling, and [IsKeyed] /
// [KeyField] / [KeyFieldName] expose the annotation to tools that
// would otherwise re-derive it from raw descriptors. Violations decode
// as the typed [KeyedError].
//
// # Concurrency
//
// The package-level functions ([Unmarshal], [Marshal], [Parse],
// [FormatDocument]) are safe for concurrent use — they do not share
// mutable state. The [MarshalOptions] and [UnmarshalOptions] value
// types are also safe to share, provided the [TypeResolver] embedded
// in them is concurrency-safe (the registry-client resolver from
// trendvidia/protoregistry is).
//
// A [Result] returned from [UnmarshalFull] is not safe for concurrent
// modification, but read-only access from multiple goroutines is fine.
package pxf
