package textfmt

import (
	"bytes"
	"encoding/json"
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
