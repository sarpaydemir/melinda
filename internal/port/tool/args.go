package tool

import (
	"fmt"
	"math/big"

	"github.com/sarpaydemir/dost/internal/textfmt"
)

// Args are the decoded arguments of a call.
type Args struct{ members textfmt.Object }

// Arguments decodes c.Args, keeping key order and number kinds. Text that is not JSON decodes as no arguments;
// JSON that is not an object is an error.
func (c Call) Arguments() (Args, error) {
	v, err := textfmt.Decode(c.Args, true)
	if err != nil {
		return Args{}, nil
	}
	members, ok := v.(textfmt.Object)
	if !ok {
		return Args{}, typeError(v, "get")
	}
	return Args{members}, nil
}

// String returns the first truthy value of keys with surrounding whitespace removed, or "" when none is truthy.
// A truthy value that is not a string is an error.
func (a Args) String(keys ...string) (string, error) {
	for _, key := range keys {
		v := a.value(key)
		if !textfmt.Truthy(v) {
			continue
		}
		s, ok := v.(string)
		if !ok {
			return "", typeError(v, "strip")
		}
		return textfmt.Strip(s), nil
	}
	return "", nil
}

// Int returns the value of key converted by textfmt.Int, or def.
func (a Args) Int(key string, def int) int { return textfmt.Int(a.value(key), def) }

func (a Args) value(key string) any {
	for _, m := range a.members {
		if m.Key == key {
			return m.Value
		}
	}
	return nil
}

// typeError is the established error text for using method on a value of v's type.
func typeError(v any, method string) error {
	name := "NoneType"
	switch v.(type) {
	case bool:
		name = "bool"
	case string:
		name = "str"
	case int64, *big.Int:
		name = "int"
	case float64:
		name = "float"
	case []any:
		name = "list"
	case textfmt.Object:
		name = "dict"
	}
	return fmt.Errorf("'%s' object has no attribute '%s'", name, method)
}
