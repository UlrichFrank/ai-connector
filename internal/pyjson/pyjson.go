// Package pyjson writes JSON the way Python's json.dumps(value, ensure_ascii=False) does — ", " and ": " as
// separators, the keys in their order, non-ASCII as it is. Laya reads its states in this form (the same text as with
// the Python bridge), and chat templates render tools and arguments with it (Jinja's tojson).
package pyjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// String quotes s like json.dumps(s, ensure_ascii=False).
func String(s string) string {
	var b strings.Builder
	pyString(&b, s)
	return b.String()
}

// Marshal re-serialises a JSON value that way (keeping the order of the keys).
func Marshal(raw json.RawMessage) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var b strings.Builder
	if err := pyValue(dec, &b); err != nil {
		return "", err
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", fmt.Errorf("trailing data after the JSON value")
	}
	return b.String(), nil
}

func pyValue(dec *json.Decoder, b *strings.Builder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch v := tok.(type) {
	case json.Delim:
		closing := map[json.Delim]json.Delim{'{': '}', '[': ']'}[v]
		b.WriteRune(rune(v))
		first := true
		for dec.More() {
			if !first {
				b.WriteString(", ")
			}
			first = false
			if v == '{' {
				k, err := dec.Token()
				if err != nil {
					return err
				}
				pyString(b, k.(string))
				b.WriteString(": ")
			}
			if err := pyValue(dec, b); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil { // the closing delimiter
			return err
		}
		b.WriteRune(rune(closing))
	case string:
		pyString(b, v)
	case json.Number:
		b.WriteString(v.String())
	case bool:
		if v {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case nil:
		b.WriteString("null")
	}
	return nil
}

// pyString writes s quoted like json.dumps with ensure_ascii=False: only ", \ and control characters are escaped.
func pyString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}
