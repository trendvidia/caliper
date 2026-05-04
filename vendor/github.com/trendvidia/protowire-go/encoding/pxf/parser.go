package pxf

import (
	"encoding/base64"
	"time"
)

type parser struct {
	lex      *lexer
	current  Token
	comments []Comment // pending comments not yet attached to an entry
}

func newParser(input []byte) *parser {
	p := &parser{lex: newLexer(input)}
	p.advance()
	return p
}

// advance consumes the next token. Comments and newlines are collected
// into pending comments instead of being skipped, so they can be attached
// to the following entry.
func (p *parser) advance() {
	for {
		p.current = p.lex.Next()
		if p.current.Kind == NEWLINE {
			continue
		}
		if p.current.Kind == COMMENT {
			p.comments = append(p.comments, Comment{
				Pos:  p.current.Pos,
				Text: p.current.Value,
			})
			continue
		}
		return
	}
}

// flushComments returns and clears pending comments.
func (p *parser) flushComments() []Comment {
	if len(p.comments) == 0 {
		return nil
	}
	c := p.comments
	p.comments = nil
	return c
}

// Parse parses PXF source into an AST Document with comments attached.
func Parse(input []byte) (*Document, error) {
	return newParser(input).parseDocument()
}

func (p *parser) parseDocument() (*Document, error) {
	doc := &Document{}
	doc.LeadingComments = p.flushComments() // comments before @type

	if p.current.Kind == AT_TYPE {
		p.advance() // consume @type
		if p.current.Kind != IDENT {
			return nil, errorf(p.current.Pos, "expected type name after @type, got %s", p.current.Kind)
		}
		doc.TypeURL = p.current.Value
		p.advance()
	}
	doc.Entries = make([]Entry, 0, 8)
	for p.current.Kind != EOF {
		entry, err := p.parseEntry()
		if err != nil {
			return nil, err
		}
		doc.Entries = append(doc.Entries, entry)
	}
	return doc, nil
}

func (p *parser) parseEntry() (Entry, error) {
	leading := p.flushComments()

	pos := p.current.Pos
	if p.current.Kind != IDENT && p.current.Kind != STRING && p.current.Kind != INT {
		return nil, errorf(pos, "expected identifier, string, or integer, got %s (%q)", p.current.Kind, p.current.Value)
	}
	key := p.current.Value
	p.advance()

	switch p.current.Kind {
	case EQUALS:
		p.advance()
		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		return &Assignment{Pos: pos, Key: key, Value: val, LeadingComments: leading}, nil

	case COLON:
		p.advance()
		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		return &MapEntry{Pos: pos, Key: key, Value: val, LeadingComments: leading}, nil

	case LBRACE:
		p.advance() // consume {
		entries, err := p.parseBody()
		if err != nil {
			return nil, err
		}
		return &Block{Pos: pos, Name: key, Entries: entries, LeadingComments: leading}, nil

	default:
		return nil, errorf(p.current.Pos, "expected '=', ':', or '{' after %q, got %s", key, p.current.Kind)
	}
}

func (p *parser) parseValue() (Value, error) {
	pos := p.current.Pos

	switch p.current.Kind {
	case STRING:
		v := &StringVal{Pos: pos, Value: p.current.Value}
		p.advance()
		return v, nil

	case INT:
		v := &IntVal{Pos: pos, Raw: p.current.Value}
		p.advance()
		return v, nil

	case FLOAT:
		v := &FloatVal{Pos: pos, Raw: p.current.Value}
		p.advance()
		return v, nil

	case BOOL:
		v := &BoolVal{Pos: pos, Value: p.current.Value == "true"}
		p.advance()
		return v, nil

	case BYTES:
		decoded, err := base64.StdEncoding.DecodeString(p.current.Value)
		if err != nil {
			decoded, err = base64.RawStdEncoding.DecodeString(p.current.Value)
			if err != nil {
				return nil, errorf(pos, "invalid base64: %v", err)
			}
		}
		v := &BytesVal{Pos: pos, Value: decoded}
		p.advance()
		return v, nil

	case TIMESTAMP:
		t, err := time.Parse(time.RFC3339Nano, p.current.Value)
		if err != nil {
			t, err = time.Parse(time.RFC3339, p.current.Value)
			if err != nil {
				return nil, errorf(pos, "invalid timestamp %q: %v", p.current.Value, err)
			}
		}
		v := &TimestampVal{Pos: pos, Value: t, Raw: p.current.Value}
		p.advance()
		return v, nil

	case DURATION:
		d, err := time.ParseDuration(p.current.Value)
		if err != nil {
			return nil, errorf(pos, "invalid duration %q: %v", p.current.Value, err)
		}
		v := &DurationVal{Pos: pos, Value: d, Raw: p.current.Value}
		p.advance()
		return v, nil

	case NULL:
		v := &NullVal{Pos: pos}
		p.advance()
		return v, nil

	case IDENT:
		v := &IdentVal{Pos: pos, Name: p.current.Value}
		p.advance()
		return v, nil

	case LBRACKET:
		return p.parseList()

	case LBRACE:
		return p.parseBlockVal()

	default:
		return nil, errorf(pos, "expected value, got %s (%q)", p.current.Kind, p.current.Value)
	}
}

func (p *parser) parseList() (Value, error) {
	pos := p.current.Pos
	p.advance() // consume [

	elems := make([]Value, 0, 4)
	for p.current.Kind != RBRACKET && p.current.Kind != EOF {
		elem, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		elems = append(elems, elem)
		if p.current.Kind == COMMA {
			p.advance()
		}
	}
	if p.current.Kind != RBRACKET {
		return nil, errorf(p.current.Pos, "expected ']', got %s", p.current.Kind)
	}
	p.advance()
	return &ListVal{Pos: pos, Elements: elems}, nil
}

func (p *parser) parseBlockVal() (Value, error) {
	pos := p.current.Pos
	p.advance() // consume {
	entries, err := p.parseBody()
	if err != nil {
		return nil, err
	}
	return &BlockVal{Pos: pos, Entries: entries}, nil
}

func (p *parser) parseBody() ([]Entry, error) {
	entries := make([]Entry, 0, 4)
	for p.current.Kind != RBRACE && p.current.Kind != EOF {
		entry, err := p.parseEntry()
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if p.current.Kind != RBRACE {
		return nil, errorf(p.current.Pos, "expected '}', got %s", p.current.Kind)
	}
	p.advance()
	return entries, nil
}
