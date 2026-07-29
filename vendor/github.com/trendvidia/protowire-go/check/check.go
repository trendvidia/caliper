// Copyright 2026 TrendVidia LLC
// SPDX-License-Identifier: MIT

// Package check defines the validation seam for protowire-go decoders.
//
// The decoders in encoding/pxf, encoding/pb, and encoding/sbe accept an
// optional [Validator] that runs after a successful decode and reports
// data violations. This package owns only the seam — the interface, the
// report types, and the error wrapper. Validation engines live in
// separate modules and plug in at link time; the core module has no
// dependency on any engine.
//
// When a validator reports violations, the decode fails with a [*Error]
// wrapping the full [Report]:
//
//	opts := pxf.UnmarshalOptions{Validator: myValidator}
//	err := opts.Unmarshal(data, msg)
//	var ce *check.Error
//	if errors.As(err, &ce) {
//	    for _, v := range ce.Report.Violations { ... }
//	}
package check

import "fmt"

// Validator runs data validation over a decoded value.
//
// v is the decoded value: a proto.Message for the pxf and sbe decoders,
// a pointer to a protowire-tagged Go struct for the pb decoder. Engines
// that only handle proto messages should return an engine error (not a
// Report) for values they cannot validate.
//
// The returned Report carries data violations; the returned error is
// reserved for engine failure (unresolvable schema, bad constraint
// expression, unsupported value type). A clean pass returns a nil or
// empty Report and a nil error. Implementations must be safe for
// concurrent use — decoders may share one validator across goroutines.
type Validator interface {
	Validate(v any) (*Report, error)
}

// Violation describes a single failed validation rule.
type Violation struct {
	// Path is the dotted path of the offending field, using the same
	// scheme as pxf presence tracking: "name", "db.password",
	// "keys[0]", "tenants[acme]". Empty for message-level rules.
	Path string

	// RuleID identifies the violated rule, namespaced by engine so
	// reports from different validators stay distinguishable — e.g.
	// "buf.validate.string.min_len" (protovalidate) or
	// "rfc001.required" (protocheck).
	RuleID string

	// Message is a human-readable description of the violation.
	Message string
}

// Report is the outcome of a validation pass: zero or more violations.
type Report struct {
	Violations []Violation
}

// OK reports whether the report contains no violations.
// A nil *Report is OK.
func (r *Report) OK() bool {
	return r == nil || len(r.Violations) == 0
}

// Error is the decode failure produced when a Validator reports
// violations. Retrieve it with errors.As to access the full Report.
type Error struct {
	Report *Report
}

func (e *Error) Error() string {
	n := 0
	if e.Report != nil {
		n = len(e.Report.Violations)
	}
	if n == 0 {
		return "check: validation failed"
	}
	v := e.Report.Violations[0]
	msg := v.Message
	if v.Path != "" {
		msg = v.Path + ": " + msg
	}
	if v.RuleID != "" {
		msg += " (" + v.RuleID + ")"
	}
	if n == 1 {
		return "check: " + msg
	}
	return fmt.Sprintf("check: %d violations; first: %s", n, msg)
}

// Validate applies v to a decoded value with the standard semantics
// shared by all protowire-go decoders: a nil validator is a no-op,
// engine errors propagate as-is, and a report with violations fails
// with a [*Error]. The report is returned in every non-engine-error
// case so callers can attach it to their own result surfaces.
func Validate(v Validator, decoded any) (*Report, error) {
	if v == nil {
		return nil, nil
	}
	rep, err := v.Validate(decoded)
	if err != nil {
		return nil, err
	}
	if !rep.OK() {
		return rep, &Error{Report: rep}
	}
	return rep, nil
}
