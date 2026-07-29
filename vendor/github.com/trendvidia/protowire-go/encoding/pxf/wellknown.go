// Copyright 2026 TrendVidia LLC
// SPDX-License-Identifier: MIT

package pxf

import (
	"fmt"
	"math/big"
	"strings"
	"time"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// Wrapper type full names mapped to inner value kind.
var wrapperTypes = map[protoreflect.FullName]protoreflect.Kind{
	"google.protobuf.BoolValue":   protoreflect.BoolKind,
	"google.protobuf.BytesValue":  protoreflect.BytesKind,
	"google.protobuf.DoubleValue": protoreflect.DoubleKind,
	"google.protobuf.FloatValue":  protoreflect.FloatKind,
	"google.protobuf.Int32Value":  protoreflect.Int32Kind,
	"google.protobuf.Int64Value":  protoreflect.Int64Kind,
	"google.protobuf.StringValue": protoreflect.StringKind,
	"google.protobuf.UInt32Value": protoreflect.Uint32Kind,
	"google.protobuf.UInt64Value": protoreflect.Uint64Kind,
}

func isWrapperType(desc protoreflect.MessageDescriptor) bool {
	_, ok := wrapperTypes[desc.FullName()]
	return ok
}

func isTimestamp(desc protoreflect.MessageDescriptor) bool {
	return desc.FullName() == "google.protobuf.Timestamp"
}

func isDuration(desc protoreflect.MessageDescriptor) bool {
	return desc.FullName() == "google.protobuf.Duration"
}

func isAny(desc protoreflect.MessageDescriptor) bool {
	return desc.FullName() == "google.protobuf.Any"
}

func setTimestampFields(msg protoreflect.Message, t time.Time) {
	d := msg.Descriptor()
	msg.Set(d.Fields().ByName("seconds"), protoreflect.ValueOfInt64(t.Unix()))
	msg.Set(d.Fields().ByName("nanos"), protoreflect.ValueOfInt32(int32(t.Nanosecond())))
}

func setDurationFields(msg protoreflect.Message, dur time.Duration) {
	d := msg.Descriptor()
	nanos := dur.Nanoseconds()
	secs := nanos / 1e9
	nanos -= secs * 1e9
	msg.Set(d.Fields().ByName("seconds"), protoreflect.ValueOfInt64(secs))
	msg.Set(d.Fields().ByName("nanos"), protoreflect.ValueOfInt32(int32(nanos)))
}

func readTimestamp(msg protoreflect.Message) time.Time {
	d := msg.Descriptor()
	secs := msg.Get(d.Fields().ByName("seconds")).Int()
	nanos := msg.Get(d.Fields().ByName("nanos")).Int()
	return time.Unix(secs, nanos).UTC()
}

func readDuration(msg protoreflect.Message) time.Duration {
	d := msg.Descriptor()
	secs := msg.Get(d.Fields().ByName("seconds")).Int()
	nanos := msg.Get(d.Fields().ByName("nanos")).Int()
	return time.Duration(secs)*time.Second + time.Duration(nanos)*time.Nanosecond
}

// Big number type detection.

func isBigInt(desc protoreflect.MessageDescriptor) bool {
	return desc.FullName() == "pxf.BigInt"
}

func isDecimal(desc protoreflect.MessageDescriptor) bool {
	return desc.FullName() == "pxf.Decimal"
}

func isBigFloat(desc protoreflect.MessageDescriptor) bool {
	return desc.FullName() == "pxf.BigFloat"
}

// pxf.Secret well-known type.
//
// Secret carries a sensitive scalar plus optional hint/fingerprint
// metadata. PXF accepts two surface forms:
//
//	pw = "x"                              # scalar shorthand
//	pw { value = "x", hint = "h" }        # explicit block
//
// The codec treats the inner `value` field as a string for transport
// purposes. Memguard handoff happens in the consumer (chameleon
// resolver), not here — this codec only routes bytes between the wire
// and the proto message. Keeping that boundary clean lets protowire-go
// stay free of any memguard dependency.

func isSecret(desc protoreflect.MessageDescriptor) bool {
	return desc.FullName() == "pxf.Secret"
}

// secretHasMetadata reports whether the Secret message carries hint or
// fingerprint, in which case the encoder must emit the explicit block
// form to round-trip those fields. Empty metadata → scalar shorthand.
//
// Note: there is no setSecretValue / readSecretValue helper here.
// Secret.value is a plain string field, so the codec uses the existing
// scalar paths (consumeScalar / writeScalar) directly — adding helpers
// that wrap them would either bypass UTF-8 validation or duplicate
// logic. Compare with pxf.BigInt etc. which DO need helpers because
// their inner representation is multi-field (abs + negative bytes).
func secretHasMetadata(msg protoreflect.Message) bool {
	d := msg.Descriptor()
	if hf := d.Fields().ByName("hint"); hf != nil && msg.Get(hf).String() != "" {
		return true
	}
	if ff := d.Fields().ByName("fingerprint"); ff != nil && msg.Get(ff).String() != "" {
		return true
	}
	return false
}

// Big number field setters (decode direction).

func setBigIntFields(msg protoreflect.Message, v *big.Int) {
	d := msg.Descriptor()
	absBytes := v.Bytes() // unsigned big-endian
	if len(absBytes) > 0 {
		msg.Set(d.Fields().ByName("abs"), protoreflect.ValueOfBytes(absBytes))
	}
	if v.Sign() < 0 {
		msg.Set(d.Fields().ByName("negative"), protoreflect.ValueOfBool(true))
	}
}

func setDecimalFields(msg protoreflect.Message, unscaled *big.Int, scale int32, negative bool) {
	d := msg.Descriptor()
	absBytes := unscaled.Bytes()
	if len(absBytes) > 0 {
		msg.Set(d.Fields().ByName("unscaled"), protoreflect.ValueOfBytes(absBytes))
	}
	if scale != 0 {
		msg.Set(d.Fields().ByName("scale"), protoreflect.ValueOfInt32(scale))
	}
	if negative {
		msg.Set(d.Fields().ByName("negative"), protoreflect.ValueOfBool(true))
	}
}

func setBigFloatFields(msg protoreflect.Message, v *big.Float) {
	d := msg.Descriptor()
	prec := v.Prec()
	if prec == 0 {
		return
	}
	// Extract mantissa as integer: multiply by 2^prec and take Int part.
	// MantExp returns mant in [0.5, 1.0) and exp such that v = mant × 2^exp.
	mant := new(big.Float).SetPrec(prec)
	exp := v.MantExp(mant)
	mant.SetMantExp(mant, int(prec)) // mant × 2^prec → integer
	mantInt, _ := mant.Int(nil)
	if mantInt.Sign() < 0 {
		mantInt.Neg(mantInt)
	}
	mantBytes := mantInt.Bytes()
	if len(mantBytes) > 0 {
		msg.Set(d.Fields().ByName("mantissa"), protoreflect.ValueOfBytes(mantBytes))
	}
	// Adjusted exponent: v = mantInt × 2^(exp - prec)
	adjExp := int32(exp) - int32(prec)
	if adjExp != 0 {
		msg.Set(d.Fields().ByName("exponent"), protoreflect.ValueOfInt32(adjExp))
	}
	msg.Set(d.Fields().ByName("prec"), protoreflect.ValueOfUint32(uint32(prec)))
	if v.Signbit() {
		msg.Set(d.Fields().ByName("negative"), protoreflect.ValueOfBool(true))
	}
}

// Big number field readers (encode direction).

func readBigInt(msg protoreflect.Message) *big.Int {
	d := msg.Descriptor()
	absBytes := msg.Get(d.Fields().ByName("abs")).Bytes()
	negative := msg.Get(d.Fields().ByName("negative")).Bool()
	v := new(big.Int).SetBytes(absBytes)
	if negative {
		v.Neg(v)
	}
	return v
}

func readBigFloat(msg protoreflect.Message) *big.Float {
	d := msg.Descriptor()
	mantBytes := msg.Get(d.Fields().ByName("mantissa")).Bytes()
	exp := int(msg.Get(d.Fields().ByName("exponent")).Int())
	prec := uint(msg.Get(d.Fields().ByName("prec")).Uint())
	negative := msg.Get(d.Fields().ByName("negative")).Bool()

	if prec == 0 {
		return new(big.Float)
	}
	mantInt := new(big.Int).SetBytes(mantBytes)
	bf := new(big.Float).SetPrec(prec).SetInt(mantInt)
	// v = mantInt × 2^exponent, but mantInt was stored as mant × 2^prec,
	// so we need SetMantExp with exp (which is already adjusted: original_exp - prec + prec from SetInt)
	// SetInt gives bf = mantInt = mant × 2^prec
	// We need bf = mant × 2^original_exp = mantInt × 2^(original_exp - prec) = mantInt × 2^adjExp
	bf.SetMantExp(bf, exp) // bf = mantInt × 2^adjExp
	if negative {
		bf.Neg(bf)
	}
	return bf
}

// formatBigInt formats a BigInt message as a decimal string.
func formatBigInt(msg protoreflect.Message) string {
	return readBigInt(msg).Text(10)
}

// readDecimalStr formats a Decimal message as a decimal string preserving scale.
func readDecimalStr(msg protoreflect.Message) string {
	d := msg.Descriptor()
	unscaledBytes := msg.Get(d.Fields().ByName("unscaled")).Bytes()
	scale := int(msg.Get(d.Fields().ByName("scale")).Int())
	negative := msg.Get(d.Fields().ByName("negative")).Bool()

	unscaled := new(big.Int).SetBytes(unscaledBytes)
	digits := unscaled.Text(10)

	var buf strings.Builder
	if negative {
		buf.WriteByte('-')
	}
	if scale <= 0 {
		buf.WriteString(digits)
		return buf.String()
	}
	// Pad with leading zeros if needed (e.g. unscaled=5, scale=2 → "0.05")
	for len(digits) <= scale {
		digits = "0" + digits
	}
	intPart := digits[:len(digits)-scale]
	fracPart := digits[len(digits)-scale:]
	buf.WriteString(intPart)
	buf.WriteByte('.')
	buf.WriteString(fracPart)
	return buf.String()
}

// formatBigFloat formats a BigFloat message as a decimal string.
func formatBigFloat(msg protoreflect.Message) string {
	bf := readBigFloat(msg)
	prec := bf.Prec()
	if prec == 0 {
		return "0"
	}
	// Use enough decimal digits to represent the precision.
	// log10(2) ≈ 0.301, so prec bits ≈ prec*0.301 decimal digits.
	decDigits := int(float64(prec)*0.30103) + 1
	return bf.Text('g', decDigits)
}

// Big number string parsers (decode direction).

func parseBigInt(s string) (*big.Int, error) {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, fmt.Errorf("invalid big integer: %s", s)
	}
	return v, nil
}

func parseDecimal(s string) (unscaled *big.Int, scale int32, negative bool, err error) {
	raw := s
	if len(s) > 0 && s[0] == '-' {
		negative = true
		s = s[1:]
	}
	dotIdx := strings.IndexByte(s, '.')
	if dotIdx >= 0 {
		scale = int32(len(s) - dotIdx - 1)
		s = s[:dotIdx] + s[dotIdx+1:]
	}
	unscaled, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, 0, false, fmt.Errorf("invalid decimal: %s", raw)
	}
	return unscaled, scale, negative, nil
}

func parseBigFloat(s string) (*big.Float, error) {
	bf, _, err := new(big.Float).SetPrec(256).Parse(s, 10)
	if err != nil {
		return nil, fmt.Errorf("invalid big float: %s", s)
	}
	return bf, nil
}
