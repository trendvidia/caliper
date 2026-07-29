// Copyright 2026 TrendVidia LLC
// SPDX-License-Identifier: MIT

package pxf

import (
	"strings"
	"time"
	"unicode/utf8"
	"unsafe"
)

type lexer struct {
	input []byte
	pos   int
	line  int
	col   int

	// tolerant enables the error-recovering behaviors used by
	// [ParseTolerant]: unterminated strings end at the newline (or EOF),
	// unterminated triple-quoted strings / block comments / bytes
	// literals end at EOF, invalid escape sequences are kept literally,
	// and bytes literals skip base64 pre-validation (the parser reports
	// it instead). Each recovery reports a positioned error via onErr.
	tolerant bool
	onErr    func(pos Position, msg string)
}

// reportErr records a recoverable lexical error in tolerant mode.
func (l *lexer) reportErr(pos Position, msg string) {
	if l.onErr != nil {
		l.onErr(pos, msg)
	}
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
	return Position{Line: l.line, Column: l.col, Offset: l.pos}
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
	case ch == '(':
		l.advance()
		return Token{Kind: LPAREN, Value: "(", Pos: pos}
	case ch == ')':
		l.advance()
		return Token{Kind: RPAREN, Value: ")", Pos: pos}
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

	case ch == '-' || ch == '+' || isDigit(ch):
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
	if l.tolerant {
		for l.pos < len(l.input) {
			l.advance()
		}
		l.reportErr(pos, "unterminated block comment")
		return Token{Kind: COMMENT, Value: string(l.input[start:l.pos]), Pos: pos}
	}
	return Token{Kind: ILLEGAL, Value: "unterminated block comment", Pos: pos}
}

func (l *lexer) lexString(pos Position) Token {
	l.advance() // opening "
	var sb strings.Builder
	var buf [utf8.UTFMax]byte
	tolerant := l.tolerant // hoisted: checked once per byte below
	for l.pos < len(l.input) {
		if tolerant && l.peek() == '\n' {
			// Recovery for mid-edit buffers: treat the newline as the
			// end of the (unterminated) string, leaving the newline for
			// the next token. Strict mode instead consumes the newline
			// into the literal and keeps scanning for the closing quote.
			l.reportErr(pos, "unterminated string: ended at newline")
			return Token{Kind: STRING, Value: sb.String(), Pos: pos}
		}
		ch := l.advance()
		if ch == '"' {
			return Token{Kind: STRING, Value: sb.String(), Pos: pos}
		}
		if ch != '\\' {
			sb.WriteByte(ch)
			continue
		}
		if l.pos >= len(l.input) {
			if tolerant {
				l.reportErr(pos, "unterminated escape sequence")
				return Token{Kind: STRING, Value: sb.String(), Pos: pos}
			}
			return Token{Kind: ILLEGAL, Value: "unterminated escape sequence", Pos: pos}
		}
		var escPos Position
		if tolerant { // strict mode never reads it; skip the capture
			escPos = l.currentPos()
		}
		esc := l.advance()
		switch esc {
		case '"', '\\', '\'', '?':
			sb.WriteByte(esc)
		case 'a':
			sb.WriteByte(0x07)
		case 'b':
			sb.WriteByte(0x08)
		case 'f':
			sb.WriteByte(0x0C)
		case 'n':
			sb.WriteByte('\n')
		case 'r':
			sb.WriteByte('\r')
		case 't':
			sb.WriteByte('\t')
		case 'v':
			sb.WriteByte(0x0B)
		case 'x':
			b, ok := l.readHexByte()
			if !ok {
				if tolerant {
					l.reportErr(escPos, `invalid \x escape: expected 2 hex digits`)
					sb.WriteString(`\x`)
					continue
				}
				return Token{Kind: ILLEGAL, Value: `invalid \x escape: expected 2 hex digits`, Pos: pos}
			}
			sb.WriteByte(b)
		case '0', '1', '2', '3':
			b, ok := l.readOctRest(esc)
			if !ok {
				if tolerant {
					l.reportErr(escPos, `invalid octal escape: expected 3 octal digits`)
					sb.WriteByte('\\')
					sb.WriteByte(esc)
					continue
				}
				return Token{Kind: ILLEGAL, Value: `invalid octal escape: expected 3 octal digits`, Pos: pos}
			}
			sb.WriteByte(b)
		case 'u':
			r, ok := l.readHexRune(4)
			if !ok || !utf8.ValidRune(r) {
				if tolerant {
					l.reportErr(escPos, `invalid \u escape: expected 4 hex digits forming a valid codepoint`)
					sb.WriteString(`\u`)
					continue
				}
				return Token{Kind: ILLEGAL, Value: `invalid \u escape: expected 4 hex digits forming a valid codepoint`, Pos: pos}
			}
			n := utf8.EncodeRune(buf[:], r)
			sb.Write(buf[:n])
		case 'U':
			r, ok := l.readHexRune(8)
			if !ok || !utf8.ValidRune(r) {
				if tolerant {
					l.reportErr(escPos, `invalid \U escape: expected 8 hex digits forming a valid codepoint`)
					sb.WriteString(`\U`)
					continue
				}
				return Token{Kind: ILLEGAL, Value: `invalid \U escape: expected 8 hex digits forming a valid codepoint`, Pos: pos}
			}
			n := utf8.EncodeRune(buf[:], r)
			sb.Write(buf[:n])
		default:
			if tolerant {
				l.reportErr(escPos, `unknown escape sequence \`+string(esc))
				sb.WriteByte('\\')
				sb.WriteByte(esc)
				continue
			}
			return Token{Kind: ILLEGAL, Value: `unknown escape sequence \` + string(esc), Pos: pos}
		}
	}
	if tolerant {
		l.reportErr(pos, "unterminated string")
		return Token{Kind: STRING, Value: sb.String(), Pos: pos}
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
	if l.tolerant {
		for l.pos < len(l.input) {
			l.advance()
		}
		l.reportErr(pos, "unterminated triple-quoted string")
		return Token{Kind: STRING, Value: dedent(string(l.input[start:l.pos])), Pos: pos}
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
	if l.pos >= len(l.input) || l.peek() != '"' {
		return Token{Kind: ILLEGAL, Value: `expected '"' after b`, Pos: pos}
	}
	l.advance() // opening "
	start := l.pos
	for l.pos < len(l.input) {
		ch := l.peek()
		if ch == '"' {
			raw := string(l.input[start:l.pos])
			l.advance() // closing "
			// Accept both standard and URL-safe base64, with or without
			// padding, per RFC 4648 §5 (referenced by draft §3.7).
			// Tolerant mode defers validation to the parser, which turns
			// a decode failure into a positioned error plus a BadVal.
			if !l.tolerant {
				if _, err := decodeBase64Lenient(raw); err != nil {
					return Token{Kind: ILLEGAL, Value: "invalid base64 in bytes literal", Pos: pos}
				}
			}
			return Token{Kind: BYTES, Value: raw, Pos: pos}
		}
		if ch == '\n' {
			if l.tolerant {
				// Leave the newline for the next token.
				l.reportErr(pos, "unterminated bytes literal: ended at newline")
				return Token{Kind: BYTES, Value: string(l.input[start:l.pos]), Pos: pos}
			}
			return Token{Kind: ILLEGAL, Value: "unterminated bytes literal", Pos: pos}
		}
		l.advance()
	}
	if l.tolerant {
		l.reportErr(pos, "unterminated bytes literal")
		return Token{Kind: BYTES, Value: string(l.input[start:l.pos]), Pos: pos}
	}
	return Token{Kind: ILLEGAL, Value: "unterminated bytes literal", Pos: pos}
}

func (l *lexer) lexDirective(pos Position) Token {
	l.advance() // @
	start := l.pos
	for l.pos < len(l.input) && isIdentPart(l.input[l.pos]) {
		l.advance()
	}
	name := string(l.input[start:l.pos])
	if name == "" {
		return Token{Kind: ILLEGAL, Value: "@", Pos: pos}
	}
	if name == "type" {
		return Token{Kind: AT_TYPE, Value: "@type", Pos: pos}
	}
	if name == "dataset" {
		return Token{Kind: AT_DATASET, Value: "@dataset", Pos: pos}
	}
	if name == "proto" {
		return Token{Kind: AT_PROTO, Value: "@proto", Pos: pos}
	}
	return Token{Kind: AT_DIRECTIVE, Value: name, Pos: pos}
}

func (l *lexer) lexNumber(pos Position) Token {
	start := l.pos
	neg := false
	if sign := l.peek(); sign == '-' || sign == '+' {
		l.advance()
		// Spec §3.8: "+inf" and "-inf" are the only signed identifier
		// literals. A '+' sign is valid for nothing else.
		if l.hasIdent("inf") {
			l.advance()
			l.advance()
			l.advance()
			return Token{Kind: FLOAT, Value: l.viewString(start, l.pos), Pos: pos}
		}
		if sign == '+' || l.pos >= len(l.input) || !isDigit(l.peek()) {
			return Token{Kind: ILLEGAL, Value: string(sign), Pos: pos}
		}
		neg = true
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
			ch == ',' || ch == ']' || ch == '}' || ch == ')' || ch == '#' {
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

// hasIdent reports whether the input at the current position reads
// exactly the identifier word (not a prefix of a longer identifier).
// Does not advance.
func (l *lexer) hasIdent(word string) bool {
	end := l.pos + len(word)
	if end > len(l.input) {
		return false
	}
	if string(l.input[l.pos:end]) != word {
		return false
	}
	return end == len(l.input) || !isIdentPart(l.input[end])
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

func isDigit(ch byte) bool { return ch >= '0' && ch <= '9' }
func isIdentStart(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_'
}
func isIdentPart(ch byte) bool { return isIdentStart(ch) || isDigit(ch) || ch == '.' }
func isDurationUnit(ch byte) bool {
	return ch == 'h' || ch == 'm' || ch == 's' || ch == 'n' || ch == 'u'
}
func isLowerAlpha(ch byte) bool { return ch >= 'a' && ch <= 'z' }

func hexVal(ch byte) (int, bool) {
	switch {
	case ch >= '0' && ch <= '9':
		return int(ch - '0'), true
	case ch >= 'a' && ch <= 'f':
		return int(ch-'a') + 10, true
	case ch >= 'A' && ch <= 'F':
		return int(ch-'A') + 10, true
	}
	return 0, false
}

// readHexByte reads exactly 2 hex digits and returns the assembled byte.
func (l *lexer) readHexByte() (byte, bool) {
	if l.pos+1 >= len(l.input) {
		return 0, false
	}
	hi, ok1 := hexVal(l.input[l.pos])
	lo, ok2 := hexVal(l.input[l.pos+1])
	if !ok1 || !ok2 {
		return 0, false
	}
	l.advance()
	l.advance()
	return byte(hi<<4 | lo), true
}

// readHexRune reads exactly n hex digits and returns the assembled rune.
// Validity (range, surrogates) is checked by the caller via utf8.ValidRune.
func (l *lexer) readHexRune(n int) (rune, bool) {
	if l.pos+n > len(l.input) {
		return 0, false
	}
	var r rune
	for i := 0; i < n; i++ {
		v, ok := hexVal(l.input[l.pos])
		if !ok {
			return 0, false
		}
		r = r<<4 | rune(v)
		l.advance()
	}
	return r, true
}

// readOctRest reads two more octal digits after the leading one already consumed
// as part of the escape (\nnn — exactly 3 octal digits total). Restricted to
// leading 0-3 by the caller so the value can never overflow a byte.
func (l *lexer) readOctRest(first byte) (byte, bool) {
	if l.pos+1 >= len(l.input) {
		return 0, false
	}
	d1, ok1 := octVal(l.input[l.pos])
	d2, ok2 := octVal(l.input[l.pos+1])
	if !ok1 || !ok2 {
		return 0, false
	}
	l.advance()
	l.advance()
	return byte(int(first-'0')<<6 | d1<<3 | d2), true
}

func octVal(ch byte) (int, bool) {
	if ch >= '0' && ch <= '7' {
		return int(ch - '0'), true
	}
	return 0, false
}
