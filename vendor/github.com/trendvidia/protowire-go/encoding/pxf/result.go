// Copyright 2026 TrendVidia LLC
// SPDX-License-Identifier: MIT

package pxf

import "github.com/trendvidia/protowire-go/check"

// Result captures field-level presence metadata from PXF decoding.
// It tracks which fields were explicitly set, which were set to null,
// and which were absent from the input.
// Fields are identified by dotted paths (e.g., "name", "nested.value").
type Result struct {
	nullFields    map[string]struct{}
	presentFields map[string]struct{}
	directives    []Directive
	datasets      []DatasetDirective
	protos        []ProtoDirective
	report        *check.Report
}

// Report returns the validation report produced by
// [UnmarshalOptions.Validator], or nil when no validator ran. It is
// populated even when the decode failed with a *check.Error, so callers
// can inspect the full violation list rather than just the error text.
func (r *Result) Report() *check.Report {
	return r.report
}

// Directives returns the `@<name> *(prefix) [{ ... }]` blocks the
// decoder saw at the document root, in source order. Excludes the
// spec-defined directives (`@type`, `@dataset`, `@proto`, `@entry`),
// which have their own accessors. Callers typically iterate and call
// UnmarshalFull on each Directive.Body against their chosen message.
func (r *Result) Directives() []Directive {
	return r.directives
}

// Datasets returns the `@dataset` directives the decoder saw at the
// document root, in source order. Each DatasetDirective carries a row
// message type, a column list, and a sequence of row tuples. See
// draft §3.4.4 for cell-state semantics.
//
// A document that uses `@dataset` will have an empty body — the rows
// are the document's payload, not the bound message. Callers walk
// each DatasetDirective.Rows and bind each row's cells to a fresh
// instance of DatasetDirective.Type via the consumer-supplied schema.
func (r *Result) Datasets() []DatasetDirective {
	return r.datasets
}

// Protos returns the `@proto` directives the decoder saw at the
// document root, in source order. Each ProtoDirective carries one of
// four body shapes (anonymous, named, source, descriptor) per draft
// §3.4.5; callers inspect Shape and decode Body accordingly.
func (r *Result) Protos() []ProtoDirective {
	return r.protos
}

func newResult() *Result {
	return &Result{
		nullFields:    make(map[string]struct{}),
		presentFields: make(map[string]struct{}),
	}
}

func (r *Result) markNull(path string) {
	r.nullFields[path] = struct{}{}
	r.presentFields[path] = struct{}{}
}

func (r *Result) markPresent(path string) {
	r.presentFields[path] = struct{}{}
}

// IsNull reports whether the field at the given path was explicitly set to null.
// Use dotted paths for nested fields: "nested.value".
func (r *Result) IsNull(path string) bool {
	_, ok := r.nullFields[path]
	return ok
}

// IsAbsent reports whether the field at the given path was not mentioned in the input.
func (r *Result) IsAbsent(path string) bool {
	_, ok := r.presentFields[path]
	return !ok
}

// IsSet reports whether the field at the given path was set to a concrete (non-null) value.
func (r *Result) IsSet(path string) bool {
	_, inPresent := r.presentFields[path]
	_, inNull := r.nullFields[path]
	return inPresent && !inNull
}

// NullFields returns the paths of all fields explicitly set to null.
func (r *Result) NullFields() []string {
	paths := make([]string, 0, len(r.nullFields))
	for p := range r.nullFields {
		paths = append(paths, p)
	}
	return paths
}

// PresentFields returns the paths of all fields encountered during
// parsing — both fields set to a concrete value and fields set to null.
// Equivalent to the union of [Result.IsSet] and [Result.IsNull] paths.
//
// Useful for layered-config systems (chameleon) that union per-layer
// presence into a merged-result presence set, then run defaults /
// required-validation against that union.
func (r *Result) PresentFields() []string {
	paths := make([]string, 0, len(r.presentFields))
	for p := range r.presentFields {
		paths = append(paths, p)
	}
	return paths
}
