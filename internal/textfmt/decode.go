package textfmt

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"unicode/utf8"
)

// Object is a JSON object whose members keep the order in which their keys first appeared.
type Object []Member

// Member is one key and value of an Object.
type Member struct {
	Key   string
	Value any
}

// Get returns the value of the member key, or nil when o has none.
func (o Object) Get(key string) any {
	for _, m := range o {
		if m.Key == key {
			return m.Value
		}
	}
	return nil
}

const (
	maxDepth     = 10000
	maxIntDigits = 4300
)

// Decode parses JSON into nil, bool, string, int64, *big.Int (beyond int64), float64, []any, or Object.
// A repeated key keeps its first position and its last value; allowNonFinite accepts NaN, Infinity, and
// -Infinity. Integers longer than 4300 digits are rejected.
func Decode(data []byte, allowNonFinite bool) (any, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("textfmt: JSON is not valid UTF-8")
	}
	d := &decoder{data: data, nonFinite: allowNonFinite}
	v, err := d.value(0)
	if err != nil {
		return nil, err
	}
	d.skipSpace()
	if d.pos < len(d.data) {
		return nil, d.fail("extra data")
	}
	return v, nil
}

type decoder struct {
	data      []byte
	pos       int
	nonFinite bool
}

func (d *decoder) fail(what string) error {
	return fmt.Errorf("textfmt: JSON %s at offset %d", what, d.pos)
}

func (d *decoder) peek() byte {
	if d.pos < len(d.data) {
		return d.data[d.pos]
	}
	return 0
}

func (d *decoder) skipSpace() {
	for {
		switch d.peek() {
		case ' ', '\t', '\n', '\r':
			d.pos++
		default:
			return
		}
	}
}

// literal consumes word if the input continues with it.
func (d *decoder) literal(word string) bool {
	if len(d.data)-d.pos < len(word) || string(d.data[d.pos:d.pos+len(word)]) != word {
		return false
	}
	d.pos += len(word)
	return true
}

// value parses the value at the current position; depth counts the containers around it.
func (d *decoder) value(depth int) (any, error) {
	d.skipSpace()
	switch c := d.peek(); {
	case (c == '{' || c == '[') && depth == maxDepth:
		return nil, d.fail("nesting too deep")
	case c == '{':
		return d.object(depth + 1)
	case c == '[':
		return d.array(depth + 1)
	case c == '"':
		return d.string()
	case d.nonFinite && d.literal("-Infinity"):
		return math.Inf(-1), nil
	case c == '-' || isDigit(c):
		return d.number()
	case d.literal("null"):
		return nil, nil
	case d.literal("true"):
		return true, nil
	case d.literal("false"):
		return false, nil
	case d.nonFinite && d.literal("NaN"):
		return math.NaN(), nil
	case d.nonFinite && d.literal("Infinity"):
		return math.Inf(1), nil
	}
	return nil, d.fail("expecting value")
}

func (d *decoder) object(depth int) (any, error) {
	d.pos++
	obj := Object{}
	index := map[string]int{}
	d.skipSpace()
	if d.peek() == '}' {
		d.pos++
		return obj, nil
	}
	for {
		d.skipSpace()
		if d.peek() != '"' {
			return nil, d.fail("expecting property name")
		}
		key, err := d.string()
		if err != nil {
			return nil, err
		}
		d.skipSpace()
		if d.peek() != ':' {
			return nil, d.fail("expecting ':'")
		}
		d.pos++
		v, err := d.value(depth)
		if err != nil {
			return nil, err
		}
		if i, ok := index[key]; ok {
			obj[i].Value = v
		} else {
			index[key] = len(obj)
			obj = append(obj, Member{Key: key, Value: v})
		}
		d.skipSpace()
		switch d.peek() {
		case ',':
			d.pos++
		case '}':
			d.pos++
			return obj, nil
		default:
			return nil, d.fail("expecting ',' or '}'")
		}
	}
}

func (d *decoder) array(depth int) (any, error) {
	d.pos++
	arr := []any{}
	d.skipSpace()
	if d.peek() == ']' {
		d.pos++
		return arr, nil
	}
	for {
		v, err := d.value(depth)
		if err != nil {
			return nil, err
		}
		arr = append(arr, v)
		d.skipSpace()
		switch d.peek() {
		case ',':
			d.pos++
		case ']':
			d.pos++
			return arr, nil
		default:
			return nil, d.fail("expecting ',' or ']'")
		}
	}
}

func (d *decoder) string() (string, error) {
	start := d.pos
	escaped := false
	for d.pos++; d.pos < len(d.data); {
		switch c := d.data[d.pos]; {
		case c == '"':
			d.pos++
			raw := d.data[start:d.pos]
			if !escaped {
				return string(raw[1 : len(raw)-1]), nil
			}
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return "", d.fail("invalid string escape")
			}
			return s, nil
		case c == '\\':
			escaped = true
			d.pos += 2
		case c < 0x20:
			return "", d.fail("control character in string")
		default:
			d.pos++
		}
	}
	return "", d.fail("unterminated string")
}

func (d *decoder) number() (any, error) {
	start := d.pos
	if d.peek() == '-' {
		d.pos++
	}
	intStart := d.pos
	if d.digits() == 0 {
		return nil, d.fail("expecting value")
	}
	intDigits := d.pos - intStart
	if d.data[intStart] == '0' && intDigits > 1 {
		return nil, d.fail("leading zero in number")
	}
	isFloat := false
	if d.peek() == '.' {
		d.pos++
		if d.digits() == 0 {
			return nil, d.fail("expecting fraction digits")
		}
		isFloat = true
	}
	if c := d.peek(); c == 'e' || c == 'E' {
		d.pos++
		if c := d.peek(); c == '+' || c == '-' {
			d.pos++
		}
		if d.digits() == 0 {
			return nil, d.fail("expecting exponent digits")
		}
		isFloat = true
	}
	lit := string(d.data[start:d.pos])
	if isFloat {
		f, err := strconv.ParseFloat(lit, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return nil, d.fail("invalid number")
		}
		return f, nil
	}
	if intDigits > maxIntDigits {
		return nil, d.fail("integer exceeds 4300 digits")
	}
	if n, err := strconv.ParseInt(lit, 10, 64); err == nil {
		return n, nil
	}
	n, _ := new(big.Int).SetString(lit, 10)
	return n, nil
}

// digits consumes ASCII digits and returns how many it consumed.
func (d *decoder) digits() int {
	start := d.pos
	for isDigit(d.peek()) {
		d.pos++
	}
	return d.pos - start
}
