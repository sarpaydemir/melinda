package textfmt

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Repr prints a value of a type Decode returns, or a []Object, as a literal: {'k': v}, [a, b], 'text', 1, 1.0,
// None, True, False. Other types print with fmt's %v.
func Repr(v any) string {
	var b strings.Builder
	writeRepr(&b, v)
	return b.String()
}

// Str prints a string verbatim and any other value as Repr does.
func Str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return Repr(v)
}

// Truthy reports whether v is anything but "", 0, 0.0, [], {}, nil, or false; other types are truthy.
func Truthy(v any) bool {
	switch v := v.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case int64:
		return v != 0
	case *big.Int:
		return v.Sign() != 0
	case float64:
		return v != 0
	case []any:
		return len(v) > 0
	case []Object:
		return len(v) > 0
	case Object:
		return len(v) > 0
	default:
		return true
	}
}

// TypeName is the name of the type of v, a value Decode returns, as the established output spells it: NoneType,
// bool, str, int, float, list, or dict.
func TypeName(v any) string {
	switch v.(type) {
	case bool:
		return "bool"
	case string:
		return "str"
	case int64, *big.Int:
		return "int"
	case float64:
		return "float"
	case []any:
		return "list"
	case Object:
		return "dict"
	}
	return "NoneType"
}

// Int converts v to an int, or returns def when v is falsy or not convertible. True gives 1, a float truncates
// toward zero, and a string may hold surrounding whitespace, a sign, and decimal digits of any script with single
// underscores between them, up to 4300 digits. Values beyond the int range saturate.
func Int(v any, def int) int {
	switch v := v.(type) {
	case bool:
		if v {
			return 1
		}
	case int64:
		if v != 0 {
			return clamp(v)
		}
	case *big.Int:
		switch {
		case v.IsInt64() && v.Sign() != 0:
			return clamp(v.Int64())
		case v.Sign() > 0:
			return math.MaxInt
		case v.Sign() < 0:
			return math.MinInt
		}
	case float64:
		switch {
		case v == 0 || math.IsNaN(v) || math.IsInf(v, 0):
		case v >= math.MaxInt64:
			return math.MaxInt
		case v <= math.MinInt64:
			return math.MinInt
		default:
			return clamp(int64(v))
		}
	case string:
		if n, ok := parseInt(v); ok {
			return n
		}
	}
	return def
}

func clamp(n int64) int { return int(max(math.MinInt, min(math.MaxInt, n))) }

// Float converts v to a float64 and reports whether it could. True gives 1, and a string may hold surrounding
// whitespace and a decimal or exponent literal, inf, infinity, or nan in any case, with a sign, digits of any
// script, and single underscores between digits.
func Float(v any) (float64, bool) {
	switch v := v.(type) {
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	case int64:
		return float64(v), true
	case *big.Int:
		f, _ := new(big.Float).SetInt(v).Float64()
		return f, true
	case float64:
		return v, true
	case string:
		return parseFloat(v)
	}
	return 0, false
}

func parseFloat(s string) (float64, bool) {
	rs := []rune(strings.TrimFunc(s, unicode.IsSpace))
	b := make([]byte, 0, len(rs))
	for i, r := range rs {
		if d, ok := decimal(r); ok {
			b = append(b, '0'+d)
			continue
		}
		if r == '_' && i > 0 && i+1 < len(rs) {
			_, before := decimal(rs[i-1])
			_, after := decimal(rs[i+1])
			if before && after {
				continue
			}
		}
		if r >= utf8.RuneSelf || r == '_' || r == 'x' || r == 'X' { // hexadecimal is not a decimal literal
			return 0, false
		}
		b = append(b, byte(r))
	}
	text := string(b)
	if len(text) > 0 && (text[0] == '+' || text[0] == '-') && strings.EqualFold(text[1:], "nan") {
		return math.NaN(), true
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	return f, true
}

func parseInt(s string) (int, bool) {
	s = strings.TrimFunc(s, unicode.IsSpace)
	sign := ""
	if s != "" && (s[0] == '+' || s[0] == '-') {
		sign, s = s[:1], s[1:]
	}
	digits := make([]byte, 0, min(len(s), maxIntDigits))
	afterDigit := false
	for _, r := range s {
		if r == '_' && afterDigit {
			afterDigit = false
			continue
		}
		d, ok := decimal(r)
		if !ok || len(digits) == maxIntDigits {
			return 0, false
		}
		digits = append(digits, '0'+d)
		afterDigit = true
	}
	if !afterDigit {
		return 0, false
	}
	n, _ := strconv.ParseInt(sign+string(digits), 10, 64) // only ErrRange is possible, and n is then saturated
	return clamp(n), true
}

// decimal returns the value of a decimal digit from any script. Unicode assigns each script's digits as a
// contiguous run in order from zero.
func decimal(r rune) (byte, bool) {
	if !unicode.Is(unicode.Nd, r) {
		return 0, false
	}
	zero := r
	for unicode.Is(unicode.Nd, zero-1) {
		zero--
	}
	return byte((r - zero) % 10), true
}

func writeRepr(b *strings.Builder, v any) {
	switch v := v.(type) {
	case nil:
		b.WriteString("None")
	case bool:
		if v {
			b.WriteString("True")
		} else {
			b.WriteString("False")
		}
	case string:
		writeQuoted(b, v)
	case int64:
		b.WriteString(strconv.FormatInt(v, 10))
	case *big.Int:
		b.WriteString(v.String())
	case float64:
		b.WriteString(formatFloat(v))
	case []any:
		writeList(b, v)
	case []Object:
		writeList(b, v)
	case Object:
		b.WriteByte('{')
		for i, m := range v {
			if i > 0 {
				b.WriteString(", ")
			}
			writeQuoted(b, m.Key)
			b.WriteString(": ")
			writeRepr(b, m.Value)
		}
		b.WriteByte('}')
	default:
		fmt.Fprint(b, v)
	}
}

func writeList[T any](b *strings.Builder, xs []T) {
	b.WriteByte('[')
	for i, x := range xs {
		if i > 0 {
			b.WriteString(", ")
		}
		writeRepr(b, x)
	}
	b.WriteByte(']')
}

// writeQuoted quotes s with ' unless s contains ' and no ", escaping backslashes, the quote, \t, \n, \r, and
// every non-printable character.
func writeQuoted(b *strings.Builder, s string) {
	quote := '\''
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	b.WriteRune(quote)
	for _, r := range s {
		switch {
		case r == quote || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case unicode.IsPrint(r):
			b.WriteRune(r)
		case r <= 0xff:
			fmt.Fprintf(b, `\x%02x`, r)
		case r <= 0xffff:
			fmt.Fprintf(b, `\u%04x`, r)
		default:
			fmt.Fprintf(b, `\U%08x`, r)
		}
	}
	b.WriteRune(quote)
}

// formatFloat prints the shortest round-trip form, in exponent notation outside 1e-4 <= |f| < 1e16 and with
// ".0" appended to integral values otherwise.
func formatFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	if a := math.Abs(f); a != 0 && (a < 1e-4 || a >= 1e16) {
		return strconv.FormatFloat(f, 'e', -1, 64)
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}
