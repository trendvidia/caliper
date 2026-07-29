// Copyright 2026 TrendVidia LLC
// SPDX-License-Identifier: MIT

package pxf

import "fmt"

// Error represents a parse or decode error with source position.
type Error struct {
	Pos Position
	Msg string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Pos, e.Msg)
}

func errorf(pos Position, format string, args ...any) *Error {
	return &Error{Pos: pos, Msg: fmt.Sprintf(format, args...)}
}
