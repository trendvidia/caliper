package pxf

import (
	"encoding/base64"
	"strings"
	"time"
	"unsafe"
)

type lexer struct {
	input []byte
	pos   int
	line  int
	col   int
}

func newLexer(input []byte) *lexer {
	return &lexer{input: input, line: 1, col: 1}
}

func (l *lexer) peek() byte {
	if l.pos >= len(l.input) {
		return 0
	}
	return l.input[l.pos]
}

func (l *lexer) peekAt(offset int) byte {
	i := l.pos + offset
	if i >= len(l.input) {
		return 0
	}
	return l.input[i]
}

func (l *lexer) advance() byte {
	if l.pos >= len(l.input) {
		return 0
	}
	ch := l.input[l.pos]
	l.pos++
	if ch == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	return ch
}

func (l *lexer) currentPos() Position {
	return Position{Line: l.line, Column: l.col}
}

func (l *lexer) skipSpaces() {
	for l.pos < len(l.input) {
		ch := l.input[l.pos]
		if ch == ' ' || ch == '\t' || ch == '\r' {
			l.advance()
		} else {
			break
		}
	}
}

// Next returns the next token from the input.
func (l *lexer) Next() Token {
	l.skipSpaces()
	if l.pos >= len(l.input) {
		return Token{Kind: EOF, Pos: l.currentPos()}
	}

	pos := l.currentPos()
	ch := l.peek()

	switch {
	case ch == '\n':
		l.advance()
		return Token{Kind: NEWLINE, Pos: pos}

	case ch == '#':
		return l.lexLineComment(pos)
	case ch == '/' && l.peekAt(1) == '/':
		return l.lexLineComment(pos)
	case ch == '/' && l.peekAt(1) == '*':
		return l.lexBlockComment(pos)

	case ch == '"':
		if l.peekAt(1) == '"' && l.peekAt(2) == '"' {
			return l.lexTripleString(pos)
		}
		return l.lexString(pos)
	case ch == 'b' && l.peekAt(1) == '"':
		return l.lexBytes(pos)

	case ch == '{':
		l.advance()
		return Token{Kind: LBRACE, Value: "{", Pos: pos}
	case ch == '}':
		l.advance()
		return Token{Kind: RBRACE, Value: "}", Pos: pos}
	case ch == '[':
		l.advance()
		return Token{Kind: LBRACKET, Value: "[", Pos: pos}
	case ch == ']':
		l.advance()
		return Token{Kind: RBRACKET, Value: "]", Pos: pos}
	case ch == '=':
		l.advance()
		return Token{Kind: EQUALS, Value: "=", Pos: pos}
	case ch == ':':
		l.advance()
		return Token{Kind: COLON, Value: ":", Pos: pos}
	case ch == ',':
		l.advance()
		return Token{Kind: COMMA, Value: ",", Pos: pos}
	case ch == '@':
		return l.lexDirective(pos)

	case ch == '-' || isDigit(ch):
		return l.lexNumber(pos)

	case isIdentStart(ch):
		return l.lexIdent(pos)

	default:
		l.advance()
		return Token{Kind: ILLEGAL, Value: string(ch), Pos: pos}
	}
}

func (l *lexer) lexLineComment(pos Position) Token {
	start := l.pos
	for l.pos < len(l.input) && l.input[l.pos] != '\n' {
		l.advance()
	}
	return Token{Kind: COMMENT, Value: string(l.input[start:l.pos]), Pos: pos}
}

func (l *lexer) lexBlockComment(pos Position) Token {
	start := l.pos
	l.advance() // /
	l.advance() // *
	for l.pos+1 < len(l.input) {
		if l.input[l.pos] == '*' && l.input[l.pos+1] == '/' {
			l.advance() // *
			l.advance() // /
			return Token{Kind: COMMENT, Value: string(l.input[start:l.pos]), Pos: pos}
		}
		l.advance()
	}
	return Token{Kind: ILLEGAL, Value: "unterminated block comment", Pos: pos}
}

func (l *lexer) lexString(pos Position) Token {
	l.advance() // opening "
	var sb strings.Builder
	for l.pos < len(l.input) {
		ch := l.advance()
		if ch == '"' {
			return Token{Kind: STRING, Value: sb.String(), Pos: pos}
		}
		if ch == '\\' {
			if l.pos >= len(l.input) {
				return Token{Kind: ILLEGAL, Value: "unterminated escape sequence", Pos: pos}
			}
			esc := l.advance()
			switch esc {
			case '"':
				sb.WriteByte('"')
			case '\\':
				sb.WriteByte('\\')
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			default:
				sb.WriteByte('\\')
				sb.WriteByte(esc)
			}
			continue
		}
		sb.WriteByte(ch)
	}
	return Token{Kind: ILLEGAL, Value: "unterminated string", Pos: pos}
}

func (l *lexer) lexTripleString(pos Position) Token {
	l.advance() // "
	l.advance() // "
	l.advance() // "
	start := l.pos
	for l.pos+2 < len(l.input) {
		if l.input[l.pos] == '"' && l.input[l.pos+1] == '"' && l.input[l.pos+2] == '"' {
			raw := string(l.input[start:l.pos])
			l.advance() // "
			l.advance() // "
			l.advance() // "
			return Token{Kind: STRING, Value: dedent(raw), Pos: pos}
		}
		l.advance()
	}
	return Token{Kind: ILLEGAL, Value: "unterminated triple-quoted string", Pos: pos}
}

// dedent processes a triple-quoted string body:
//   - strips leading newline after opening """
//   - uses closing line indent as base indent to strip from each line
//   - strips trailing whitespace-only line (before closing """)
func dedent(s string) string {
	if len(s) > 0 && s[0] == '\n' {
		s = s[1:]
	}
	lines := strings.Split(s, "\n")
	if len(lines) == 0 {
		return ""
	}
	last := lines[len(lines)-1]
	if strings.TrimSpace(last) == "" {
		indent := last
		lines = lines[:len(lines)-1]
		for i, line := range lines {
			lines[i] = strings.TrimPrefix(line, indent)
		}
	}
	return strings.Join(lines, "\n")
}

func (l *lexer) lexBytes(pos Position) Token {
	l.advance() // b
	tok := l.lexString(pos)
	if tok.Kind != STRING {
		return tok
	}
	if _, err := base64.StdEncoding.DecodeString(tok.Value); err != nil {
		if _, err2 := base64.RawStdEncoding.DecodeString(tok.Value); err2 != nil {
			return Token{Kind: ILLEGAL, Value: "invalid base64 in bytes literal", Pos: pos}
		}
	}
	return Token{Kind: BYTES, Value: tok.Value, Pos: pos}
}

func (l *lexer) lexDirective(pos Position) Token {
	l.advance() // @
	start := l.pos
	for l.pos < len(l.input) && isIdentPart(l.input[l.pos]) {
		l.advance()
	}
	name := string(l.input[start:l.pos])
	if name == "type" {
		return Token{Kind: AT_TYPE, Value: "@type", Pos: pos}
	}
	return Token{Kind: ILLEGAL, Value: "@" + name, Pos: pos}
}

func (l *lexer) lexNumber(pos Position) Token {
	start := l.pos
	neg := false
	if l.peek() == '-' {
		neg = true
		l.advance()
		if l.pos >= len(l.input) || !isDigit(l.peek()) {
			return Token{Kind: ILLEGAL, Value: "-", Pos: pos}
		}
	}

	digitStart := l.pos
	for l.pos < len(l.input) && isDigit(l.peek()) {
		l.advance()
	}
	digitCount := l.pos - digitStart

	// Timestamp: exactly 4 digits followed by '-', only non-negative
	if !neg && digitCount == 4 && l.pos < len(l.input) && l.peek() == '-' {
		return l.lexTimestamp(pos, start)
	}
	// Float: '.' or 'e'/'E'
	if l.pos < len(l.input) && (l.peek() == '.' || l.peek() == 'e' || l.peek() == 'E') {
		return l.lexFloat(pos, start)
	}
	// Duration: digits followed by a time unit letter
	if l.pos < len(l.input) && isDurationUnit(l.peek()) {
		return l.lexDuration(pos, start)
	}

	return Token{Kind: INT, Value: l.viewString(start, l.pos), Pos: pos}
}

func (l *lexer) lexFloat(pos Position, start int) Token {
	if l.peek() == '.' {
		l.advance()
		for l.pos < len(l.input) && isDigit(l.peek()) {
			l.advance()
		}
	}
	if l.pos < len(l.input) && (l.peek() == 'e' || l.peek() == 'E') {
		l.advance()
		if l.pos < len(l.input) && (l.peek() == '+' || l.peek() == '-') {
			l.advance()
		}
		for l.pos < len(l.input) && isDigit(l.peek()) {
			l.advance()
		}
	}
	return Token{Kind: FLOAT, Value: l.viewString(start, l.pos), Pos: pos}
}

func (l *lexer) lexTimestamp(pos Position, start int) Token {
	// Read characters that can be part of an RFC 3339 timestamp.
	for l.pos < len(l.input) {
		ch := l.peek()
		if ch == ' ' || ch == '\n' || ch == '\t' || ch == '\r' ||
			ch == ',' || ch == ']' || ch == '}' || ch == '#' {
			break
		}
		if ch == '/' && (l.peekAt(1) == '/' || l.peekAt(1) == '*') {
			break
		}
		l.advance()
	}
	raw := l.viewString(start, l.pos)
	if _, err := time.Parse(time.RFC3339Nano, raw); err != nil {
		if _, err2 := time.Parse(time.RFC3339, raw); err2 != nil {
			return Token{Kind: ILLEGAL, Value: "invalid timestamp: " + raw, Pos: pos}
		}
	}
	return Token{Kind: TIMESTAMP, Value: raw, Pos: pos}
}

func (l *lexer) lexDuration(pos Position, start int) Token {
	for l.pos < len(l.input) && (isDigit(l.peek()) || isLowerAlpha(l.peek())) {
		l.advance()
	}
	raw := l.viewString(start, l.pos)
	if _, err := time.ParseDuration(raw); err != nil {
		return Token{Kind: ILLEGAL, Value: "invalid duration: " + raw, Pos: pos}
	}
	return Token{Kind: DURATION, Value: raw, Pos: pos}
}

func (l *lexer) lexIdent(pos Position) Token {
	start := l.pos
	for l.pos < len(l.input) && isIdentPart(l.input[l.pos]) {
		l.advance()
	}
	val := l.viewString(start, l.pos)
	if val == "true" || val == "false" {
		return Token{Kind: BOOL, Value: val, Pos: pos}
	}
	if val == "null" {
		return Token{Kind: NULL, Value: val, Pos: pos}
	}
	return Token{Kind: IDENT, Value: val, Pos: pos}
}

// viewString returns a zero-copy string view into the input buffer.
// Safe only for values that are NOT stored in proto messages (field names,
// numbers, timestamps, durations — all consumed during decode and discarded).
func (l *lexer) viewString(start, end int) string {
	if start >= end {
		return ""
	}
	return unsafe.String(&l.input[start], end-start)
}

func isDigit(ch byte) bool       { return ch >= '0' && ch <= '9' }
func isIdentStart(ch byte) bool  { return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_' }
func isIdentPart(ch byte) bool   { return isIdentStart(ch) || isDigit(ch) || ch == '.' }
func isDurationUnit(ch byte) bool { return ch == 'h' || ch == 'm' || ch == 's' || ch == 'n' || ch == 'u' }
func isLowerAlpha(ch byte) bool  { return ch >= 'a' && ch <= 'z' }
