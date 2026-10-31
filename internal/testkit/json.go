package testkit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
)

// JSONEqual reports whether a and b encode the same JSON value. Whitespace and object key order do not matter;
// number literals must match exactly, so 1 and 1.0 differ.
func JSONEqual(a, b []byte) (bool, error) {
	va, err := decodeJSON(a)
	if err != nil {
		return false, err
	}
	vb, err := decodeJSON(b)
	if err != nil {
		return false, err
	}
	return reflect.DeepEqual(va, vb), nil
}

func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("invalid JSON %q: %w", data, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("invalid JSON %q: data after the value", data)
	}
	return v, nil
}
