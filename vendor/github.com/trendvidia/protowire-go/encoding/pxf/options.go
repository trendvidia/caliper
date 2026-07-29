// Copyright 2026 TrendVidia LLC
// SPDX-License-Identifier: MIT

package pxf

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/trendvidia/protowire-go/check"
)

// TypeResolver resolves protobuf type URLs to message descriptors.
// Required for encoding/decoding google.protobuf.Any fields with sugar syntax.
type TypeResolver interface {
	FindMessageByURL(url string) (protoreflect.MessageDescriptor, error)
}

// UnmarshalOptions configures PXF decoding.
type UnmarshalOptions struct {
	// TypeResolver resolves type URLs for google.protobuf.Any fields.
	// When set, Any fields use sugar syntax: @type = "..." + inline fields.
	// When nil, Any fields decode as regular messages (type_url + value).
	TypeResolver TypeResolver

	// DiscardUnknown silently skips fields not found in the schema
	// instead of returning an error.
	DiscardUnknown bool

	// SkipPostDecode disables the per-parse pass that applies
	// (pxf.default) values to absent fields and validates
	// (pxf.required) fields. Layered configuration systems (e.g.
	// chameleon) need defaults + required to apply on the MERGED
	// result, not per-layer — a base layer may legitimately omit a
	// required field that a higher layer provides, and per-layer
	// defaults silently get clobbered by merge's "absent → fall
	// through" rule. With SkipPostDecode = true, callers get raw
	// presence tracking and run their own post-merge passes via
	// [IsRequired] and [Default].
	SkipPostDecode bool

	// SkipValidate disables the per-call schema reserved-name check
	// (draft §3.13). The default behavior — running the check on every
	// decode call — is the safe one because the check catches schemas
	// that would silently produce unreachable enum values or fields.
	// Callers that have already validated their descriptors out-of-band
	// (e.g. a registry-load step that pre-screens schemas before
	// caching their descriptors) may set this to bypass the per-call
	// recheck. Validate explicitly via [ValidateDescriptor] when
	// pre-screening.
	SkipValidate bool

	// OnSecretField, if non-nil, is called for each pxf.Secret-typed
	// field's value, in every form it can be written: scalar shorthand
	// (`pw = "x"`), list elements (`["a", "b"]`), map values
	// (`{ "acme": "k" }`), AND block form (`pw { value = "x", hint = "h" }`).
	// In all of them the hook receives the dotted field path and the value
	// string. When the hook returns nil, the decoder skips the standard
	// assignment to the inner `value` field on the Secret message — the
	// caller is responsible for routing the value to whatever destination
	// honors its own memory-management contract (e.g. a memguard Enclave).
	// Presence tracking on `value` is still updated so Result.IsSet reports
	// the field as set. A Secret's `hint`/`fingerprint` subfields are always
	// assigned to the proto message normally; they are diagnostic, not
	// sensitive.
	//
	// Path scheme:
	//
	//   pw = "x"                           → "pw"
	//   db { password = "x" }              → "db.password"
	//   pw { value = "x", hint = "h" }     → "pw"      (block form)
	//   backup_keys = ["a", "b"]           → "backup_keys[0]", "backup_keys[1]"
	//   tenant_keys = { "acme": "k" }      → "tenant_keys[acme]"
	//
	// Memory note: the value crosses as a Go string. PXF is a text codec, so
	// the plaintext also exists as a substring of the input []byte for the
	// duration of the decode; the hook closes the window from the *message*
	// onward, not from the input buffer. Callers wanting the tightest window
	// should route the value into protected memory immediately and, where the
	// threat model demands it, decode from a buffer they control and wipe.
	//
	// Errors from the hook abort the decode and propagate.
	OnSecretField func(path, value string) error

	// Validator, if non-nil, runs data validation on the decoded
	// message after the decode (and, for the Full variants, after the
	// post-decode defaults/required pass). When it reports violations
	// the decode fails with a *check.Error, retrievable via errors.As;
	// the Full variants additionally return their Result with the
	// check.Report attached (see [Result.Report]).
	//
	// This is a different mechanism from SkipValidate above: that
	// knob controls the per-call schema reserved-name check on the
	// descriptor, while Validator checks the decoded data against an
	// engine-defined rule set (e.g. buf.validate annotations via a
	// protovalidate adapter, or the RFC-001 surface via protocheck).
	Validator check.Validator
}

// UnmarshalFull decodes PXF data into msg and returns field presence metadata.
// Unlike Unmarshal, it tracks which fields are set, null, or absent,
// validates required fields, and applies default values.
func UnmarshalFull(data []byte, msg proto.Message) (*Result, error) {
	return UnmarshalOptions{}.UnmarshalFull(data, msg)
}

// UnmarshalFull decodes PXF data into msg and returns field presence metadata.
// If a Validator reports violations, the Result (with the check.Report
// attached) is returned alongside the *check.Error.
func (o UnmarshalOptions) UnmarshalFull(data []byte, msg proto.Message) (*Result, error) {
	r := msg.ProtoReflect()
	if !o.SkipValidate {
		if err := asValidationError(ValidateFile(r.Descriptor().ParentFile())); err != nil {
			return nil, err
		}
	}
	result, err := unmarshalDirectFull(data, r, o.TypeResolver, o.DiscardUnknown, o.SkipPostDecode, o.OnSecretField)
	if err != nil {
		return nil, err
	}
	result.report, err = check.Validate(o.Validator, msg)
	return result, err
}
