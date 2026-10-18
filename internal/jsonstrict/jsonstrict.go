// Package jsonstrict reads a JSON request body only if it has exactly one meaning.
//
// encoding/json accepts documents that other parsers read differently: a key given twice (encoding/json keeps the
// last, some parsers keep the first), invalid UTF-8 (each bad byte becomes U+FFFD) and data after the first value
// (Decoder.Decode stops after one value). A gateway routes a request on what it parsed and then a provider parses
// it again, so it must not accept any of these: it could route and account for one model while the provider serves
// another. ParseObject refuses them, at every depth.
package jsonstrict

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// MaxDepth is how deeply arrays and objects may nest, the top-level object counting as 1. A tool's JSON Schema in a
// chat request nests a few levels per property; 128 leaves room for any real one and bounds the parser's stack.
const MaxDepth = 128

// Member is one key of an object, as it decodes, and its value, byte for byte as the client wrote it.
type Member struct {
	Key   string
	Value json.RawMessage
}

// ParseObject reads b as a single JSON object and returns its members in order. It fails if b isn't valid UTF-8,
// isn't one JSON object (with only whitespace after it), has an object anywhere in it that holds the same key twice
// (compared after unescaping, so "a" and "a" are the same key), or nests deeper than MaxDepth.
func ParseObject(b []byte) ([]Member, error) {
	if !utf8.Valid(b) {
		return nil, errors.New("the body is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber() // numbers are only skipped here; don't parse them as floats
	p := &parser{dec: dec, size: int64(len(b))}
	tok, err := dec.Token()
	if err != nil {
		return nil, p.syntaxError(err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("the body is not a JSON object")
	}
	var members []Member
	seen := map[string]bool{}
	for dec.More() {
		key, err := p.nextKey(seen)
		if err != nil {
			return nil, err
		}
		// The offset is now just past the key; after the value it is just past the value. Between the two are
		// whitespace, the colon and the value itself, and a value never starts with either.
		start := dec.InputOffset()
		if err := p.skipValue(1); err != nil {
			return nil, err
		}
		raw := bytes.TrimLeft(b[start:dec.InputOffset()], " \t\r\n:")
		members = append(members, Member{Key: key, Value: json.RawMessage(raw)})
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, p.syntaxError(err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("the body has more after the JSON object")
	}
	return members, nil
}

// parser is one body being read.
type parser struct {
	dec  *json.Decoder
	size int64 // the body's length
}

// nextKey reads an object's next key and records it in seen, failing if seen already holds it.
func (p *parser) nextKey(seen map[string]bool) (string, error) {
	tok, err := p.dec.Token()
	if err != nil {
		return "", p.syntaxError(err)
	}
	key, ok := tok.(string)
	if !ok { // the decoder only returns a string here, but don't rely on it silently
		return "", fmt.Errorf("the body is not valid JSON: expected a key, got %v", tok)
	}
	if seen[key] {
		return "", fmt.Errorf("the key %q appears twice in one object", key)
	}
	seen[key] = true
	return key, nil
}

// skipValue reads one value, checking every object in it. open is how many arrays and objects enclose the value.
func (p *parser) skipValue(open int) error {
	tok, err := p.dec.Token()
	if err != nil {
		return p.syntaxError(err)
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil // a string, number, true, false or null
	}
	if open+1 > MaxDepth {
		return fmt.Errorf("the body nests deeper than %d arrays and objects", MaxDepth)
	}
	switch d {
	case '[':
		for p.dec.More() {
			if err := p.skipValue(open + 1); err != nil {
				return err
			}
		}
	case '{':
		seen := map[string]bool{}
		for p.dec.More() {
			if _, err := p.nextKey(seen); err != nil {
				return err
			}
			if err := p.skipValue(open + 1); err != nil {
				return err
			}
		}
	}
	if _, err := p.dec.Token(); err != nil { // the closing bracket or brace
		return p.syntaxError(err)
	}
	return nil
}

// syntaxError words the decoder's error for a client. A body cut short shows up as io.EOF, io.ErrUnexpectedEOF or,
// inside a number, as a syntax error at the very end.
func (p *parser) syntaxError(err error) error {
	var se *json.SyntaxError
	if err == io.EOF || err == io.ErrUnexpectedEOF || (errors.As(err, &se) && se.Offset >= p.size) {
		return errors.New("the body is not valid JSON: it ends too soon")
	}
	return fmt.Errorf("the body is not valid JSON: %w", err)
}
