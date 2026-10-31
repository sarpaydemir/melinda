package textfmt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// Encode is v as compact JSON, as encoding/json writes it but without escaping HTML characters, U+2028, or
// U+2029.
func Encode(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return unescapeSeparators(bytes.TrimSuffix(b.Bytes(), []byte("\n"))), nil
}

// EncodeASCII writes a value as Decode returns it, or a tree of such values, as JSON with ", " and ": " as
// separators and every character outside printable ASCII escaped as \uXXXX, in UTF-16 surrogate pairs beyond the
// Basic Multilingual Plane. Non-finite floats and other types are an error.
func EncodeASCII(v any) ([]byte, error) {
	return appendValue(nil, v, true)
}

// EncodeCompact writes a value as Decode returns it, or a tree of such values, as Encode writes JSON: no spaces,
// raw UTF-8, and no HTML escaping. Non-finite floats and other types are an error.
func EncodeCompact(v any) ([]byte, error) {
	return appendValue(nil, v, false)
}

// appendValue writes v in one pass: with ", " and ": " and ASCII escapes when ascii is set, compactly otherwise.
func appendValue(b []byte, v any, ascii bool) ([]byte, error) {
	comma, colon := ",", ":"
	if ascii {
		comma, colon = ", ", ": "
	}
	switch v := v.(type) {
	case nil:
		return append(b, "null"...), nil
	case bool:
		return strconv.AppendBool(b, v), nil
	case string:
		return appendString(b, v, ascii), nil
	case int64:
		return strconv.AppendInt(b, v, 10), nil
	case *big.Int:
		return v.Append(b, 10), nil
	case float64:
		f, err := json.Marshal(v)
		return append(b, f...), err
	case []any:
		b = append(b, '[')
		for i, x := range v {
			if i > 0 {
				b = append(b, comma...)
			}
			var err error
			if b, err = appendValue(b, x, ascii); err != nil {
				return nil, err
			}
		}
		return append(b, ']'), nil
	case Object:
		b = append(b, '{')
		for i, m := range v {
			if i > 0 {
				b = append(b, comma...)
			}
			b = append(appendString(b, m.Key, ascii), colon...)
			var err error
			if b, err = appendValue(b, m.Value, ascii); err != nil {
				return nil, err
			}
		}
		return append(b, '}'), nil
	}
	return nil, fmt.Errorf("textfmt: cannot encode %T", v)
}

// appendString writes s quoted, escaping control characters, and everything outside printable ASCII when ascii is set.
func appendString(b []byte, s string, ascii bool) []byte {
	b = append(b, '"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b = append(b, '\\', byte(r))
		case r == '\b':
			b = append(b, `\b`...)
		case r == '\f':
			b = append(b, `\f`...)
		case r == '\n':
			b = append(b, `\n`...)
		case r == '\r':
			b = append(b, `\r`...)
		case r == '\t':
			b = append(b, `\t`...)
		case r >= 0x20 && !ascii:
			b = utf8.AppendRune(b, r)
		case r < 0x20 || r >= 0x7f:
			for _, u := range utf16.Encode([]rune{r}) {
				b = fmt.Appendf(b, `\u%04x`, u)
			}
		default:
			b = append(b, byte(r))
		}
	}
	return append(b, '"')
}

// unescapeSeparators turns the \u2028 and \u2029 escapes that encoding/json writes in every string back into the
// characters. It works in place.
func unescapeSeparators(b []byte) []byte {
	out := b[:0]
	for i := 0; i < len(b); i++ {
		if b[i] != '\\' {
			out = append(out, b[i])
			continue
		}
		switch string(b[i:min(i+6, len(b))]) {
		case `\u2028`:
			out = append(out, "\u2028"...)
			i += 5
		case `\u2029`:
			out = append(out, "\u2029"...)
			i += 5
		default: // an escaped character, or the first two bytes of another \uXXXX
			out = append(out, b[i], b[i+1])
			i++
		}
	}
	return out
}
