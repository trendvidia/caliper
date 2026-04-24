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
	EQUALS   // =
	COLON    // :
	COMMA    // ,

	AT_TYPE // @type
)

var tokenNames = map[TokenKind]string{
	EOF: "EOF", ILLEGAL: "ILLEGAL", NEWLINE: "newline", COMMENT: "comment",
	IDENT: "identifier", STRING: "string", INT: "integer", FLOAT: "float",
	BOOL: "bool", NULL: "null", BYTES: "bytes", TIMESTAMP: "timestamp", DURATION: "duration",
	LBRACE: "{", RBRACE: "}", LBRACKET: "[", RBRACKET: "]",
	EQUALS: "=", COLON: ":", COMMA: ",", AT_TYPE: "@type",
}

func (k TokenKind) String() string {
	if s, ok := tokenNames[k]; ok {
		return s
	}
	return fmt.Sprintf("TokenKind(%d)", int(k))
}

// Position represents a location in source text.
type Position struct {
	Line   int
	Column int
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
