package tool

import (
	"encoding/json"
	"fmt"

	"github.com/sarpaydemir/melinda/internal/textfmt"
)

// Decode decodes the JSON arguments of a call, keeping key order and number kinds. Text that is not JSON decodes
// as an empty object.
func Decode(args json.RawMessage) any {
	v, err := textfmt.Decode(args, true)
	if err != nil {
		return textfmt.Object{}
	}
	return v
}

// Args are the decoded arguments of a call.
type Args struct{ members textfmt.Object }

// Arguments decodes c.Args as Decode does; arguments that are not an object are an error.
func (c Call) Arguments() (Args, error) {
	v := Decode(c.Args)
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
		v := a.members.Get(key)
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

// Lookup returns the value of key as decoded, and whether the arguments have it.
func (a Args) Lookup(key string) (any, bool) {
	for _, m := range a.members {
		if m.Key == key {
			return m.Value, true
		}
	}
	return nil, false
}

// Int returns the value of key converted by textfmt.Int, or def.
func (a Args) Int(key string, def int) int { return textfmt.Int(a.members.Get(key), def) }

// typeError is the established error text for using method on a value of v's type.
func typeError(v any, method string) error {
	return fmt.Errorf("'%s' object has no attribute '%s'", textfmt.TypeName(v), method)
}
