// Copyright 2026 TrendVidia LLC
// SPDX-License-Identifier: MIT

package pxf

import "fmt"

// TokenKind represents the type of a lexical token.
type TokenKind int

const (
	EOF     TokenKind = iota
	ILLEGAL           // invalid token
	NEWLINE           // \n
	COMMENT           // # or // or /* */

	IDENT     // field_name, EnumValue, package.Name
	STRING    // "hello" or """multi-line"""
	INT       // 123, -456
	FLOAT     // 1.23, -4.5
	BOOL      // true, false
	NULL      // null
	BYTES     // b"base64..."
	TIMESTAMP // 2024-01-15T10:30:00Z
	DURATION  // 30s, 1h30m

	LBRACE   // {
	RBRACE   // }
	LBRACKET // [
	RBRACKET // ]
	LPAREN   // (   used by @dataset column list and row tuples
	RPAREN   // )
	EQUALS   // =
	COLON    // :
	COMMA    // ,

	AT_TYPE      // @type — body's message type, no inline block
	AT_DATASET   // @dataset — row-oriented bulk-data directive (draft §3.4.4)
	AT_PROTO     // @proto — embedded protobuf schema (draft §3.4.5)
	AT_DIRECTIVE // @<name> for any non-reserved name — Value holds the name (without @)
)

var tokenNames = map[TokenKind]string{
	EOF: "EOF", ILLEGAL: "ILLEGAL", NEWLINE: "newline", COMMENT: "comment",
	IDENT: "identifier", STRING: "string", INT: "integer", FLOAT: "float",
	BOOL: "bool", NULL: "null", BYTES: "bytes", TIMESTAMP: "timestamp", DURATION: "duration",
	LBRACE: "{", RBRACE: "}", LBRACKET: "[", RBRACKET: "]", LPAREN: "(", RPAREN: ")",
	EQUALS: "=", COLON: ":", COMMA: ",",
	AT_TYPE: "@type", AT_DATASET: "@dataset", AT_PROTO: "@proto", AT_DIRECTIVE: "@directive",
}

func (k TokenKind) String() string {
	if s, ok := tokenNames[k]; ok {
		return s
	}
	return fmt.Sprintf("TokenKind(%d)", int(k))
}

// Position represents a location in source text.
//
// Offset is the byte index into the input where the token starts. It
// is populated by the lexer and used by callers that need to slice
// the original byte stream (e.g. directive body extraction).
type Position struct {
	Line   int
	Column int
	Offset int
}

func (p Position) String() string {
	return fmt.Sprintf("%d:%d", p.Line, p.Column)
}

// Token is a lexical token with its kind, raw value, and source position.
type Token struct {
	Kind  TokenKind
	Value string
	Pos   Position
}
