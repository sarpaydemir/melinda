package textfmt_test

import (
	"math"
	"math/big"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/sarpaydemir/dost/internal/textfmt"
)

func decode(t *testing.T, in string) any {
	t.Helper()
	v, err := textfmt.Decode([]byte(in), false)
	if err != nil {
		t.Fatalf("Decode(%s): %v", in, err)
	}
	return v
}

func TestReprOfDecodedJSON(t *testing.T) {
	tests := []struct{ in, want string }{
		{`{"b": 1, "a": [1, 2.0, "x"], "c": {"d": null}}`, `{'b': 1, 'a': [1, 2.0, 'x'], 'c': {'d': None}}`},
		{`{"a":1,"b":2,"a":3}`, `{'a': 3, 'b': 2}`},
		{`[true, false, null, {}, [], "", 0, 0.0, -0.0]`, `[True, False, None, {}, [], '', 0, 0.0, -0.0]`},
		{`"é😀\n\"'"`, `'é😀\n"\''`},
		{`"it's"`, `"it's"`},
		{`{"it's": "a\"b"}`, `{"it's": 'a"b'}`},
		{`{"k": "v\u0000"}`, `{'k': 'v\x00'}`},
		{`-0`, `0`},
		{`9223372036854775807`, `9223372036854775807`},
		{`9223372036854775808`, `9223372036854775808`},
		{`-98765432109876543210`, `-98765432109876543210`},
		{`1E+2`, `100.0`},
	}
	for _, tt := range tests {
		if got := textfmt.Repr(decode(t, tt.in)); got != tt.want {
			t.Errorf("Repr(%s) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestReprQuotesAndEscapesStrings(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", `''`},
		{"plain", `'plain'`},
		{"a'b", `"a'b"`},
		{`a"b`, `'a"b'`},
		{`a'"b`, `'a\'"b'`},
		{`\`, `'\\'`},
		{"tab\tnl\nret\r", `'tab\tnl\nret\r'`},
		{"\x00\x1f\x7f", `'\x00\x1f\x7f'`},
		{"\v\f\x1c", `'\x0b\x0c\x1c'`},
		{"\u0080\u009f\u00a0\u00ad", `'\x80\x9f\xa0\xad'`},
		{"é e\u0301 日本語 İı", "'é e\u0301 日本語 İı'"},
		{"\u200b\u2028\u2029\ufeff\u0600\u061c", `'\u200b\u2028\u2029\ufeff\u0600\u061c'`},
		{"\u0378\ud7ff\ue000", `'\u0378\ud7ff\ue000'`},
		{"😀\U000e0001\U000f0000\U0010ffff", `'😀\U000e0001\U000f0000\U0010ffff'`},
		{"👨\u200d👩", `'👨\u200d👩'`},
		{"\ufffd", "'\ufffd'"},
	}
	for _, tt := range tests {
		if got := textfmt.Repr(tt.in); got != tt.want {
			t.Errorf("Repr(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestReprFloatsUseShortestRoundTripForm(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{0, "0.0"},
		{math.Copysign(0, -1), "-0.0"},
		{1, "1.0"},
		{100, "100.0"},
		{2.5, "2.5"},
		{0.1, "0.1"},
		{0.30000000000000004, "0.30000000000000004"},
		{1e15, "1000000000000000.0"},
		{9999999999999998, "9999999999999998.0"},
		{1e16, "1e+16"},
		{1.5e16, "1.5e+16"},
		{123456789012345678, "1.2345678901234568e+17"},
		{1e22, "1e+22"},
		{-1e22, "-1e+22"},
		{1e-4, "0.0001"},
		{1e-5, "1e-05"},
		{1e-7, "1e-07"},
		{123.456e-10, "1.23456e-08"},
		{5e-324, "5e-324"},
		{math.MaxFloat64, "1.7976931348623157e+308"},
		{9007199254740993, "9007199254740992.0"},
		{math.Inf(1), "inf"},
		{math.Inf(-1), "-inf"},
		{math.NaN(), "nan"},
	}
	for _, tt := range tests {
		if got := textfmt.Repr(tt.in); got != tt.want {
			t.Errorf("Repr(%v) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestReprOfBuiltValues(t *testing.T) {
	results := []textfmt.Object{
		{{Key: "title", Value: "it's"}, {Key: "score", Value: 0.5}},
		{{Key: "title", Value: "b"}, {Key: "tags", Value: []any{}}},
	}
	tests := []struct {
		in   any
		want string
	}{
		{results, `[{'title': "it's", 'score': 0.5}, {'title': 'b', 'tags': []}]`},
		{[]textfmt.Object{}, `[]`},
		{[]any{textfmt.Object{}, results[1]}, `[{}, {'title': 'b', 'tags': []}]`},
		{7, `7`},
	}
	for _, tt := range tests {
		if got := textfmt.Repr(tt.in); got != tt.want {
			t.Errorf("Repr(%#v) = %s, want %s", tt.in, got, tt.want)
		}
	}
	if textfmt.Truthy([]textfmt.Object{}) || !textfmt.Truthy(results) {
		t.Error("Truthy([]Object) does not follow its length")
	}
}

func FuzzRepr(f *testing.F) {
	for _, seed := range []string{"", "it's", `a'"b`, "\x00\t\u0085\u200b😀\U000e0001", `\\'`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !utf8.ValidString(s) {
			return
		}
		got := textfmt.Repr(s)
		back, err := unquote(got)
		if err != nil {
			t.Fatalf("Repr(%q) = %s: %v", s, got, err)
		}
		if back != s {
			t.Fatalf("Repr(%q) = %s, reads back as %q", s, got, back)
		}
		for _, r := range got {
			if !unicode.IsPrint(r) {
				t.Fatalf("Repr(%q) = %s holds non-printable %U", s, got, r)
			}
		}
	})
}

// unquote reads a quoted literal in the format Repr produces.
func unquote(lit string) (string, error) {
	if len(lit) < 2 || (lit[0] != '\'' && lit[0] != '"') || lit[len(lit)-1] != lit[0] {
		return "", strconv.ErrSyntax
	}
	quote, body := lit[0], lit[1:len(lit)-1]
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c == quote {
			return "", strconv.ErrSyntax
		}
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		if i+1 == len(body) {
			return "", strconv.ErrSyntax
		}
		i++
		width := map[byte]int{'x': 2, 'u': 4, 'U': 8}[body[i]]
		switch body[i] {
		case '\\', '\'', '"':
			b.WriteByte(body[i])
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 'x', 'u', 'U':
			if i+width >= len(body) {
				return "", strconv.ErrSyntax
			}
			n, err := strconv.ParseUint(body[i+1:i+1+width], 16, 32)
			if err != nil || strings.ToLower(body[i+1:i+1+width]) != body[i+1:i+1+width] {
				return "", strconv.ErrSyntax
			}
			b.WriteRune(rune(n))
			i += width
		default:
			return "", strconv.ErrSyntax
		}
	}
	return b.String(), nil
}

func TestStrPrintsStringsVerbatim(t *testing.T) {
	tests := []struct {
		in   any
		want string
	}{
		{"it's\n", "it's\n"},
		{nil, "None"},
		{true, "True"},
		{int64(-3), "-3"},
		{1.0, "1.0"},
		{[]any{"a"}, "['a']"},
		{textfmt.Object{{Key: "k", Value: "v"}}, "{'k': 'v'}"},
	}
	for _, tt := range tests {
		if got := textfmt.Str(tt.in); got != tt.want {
			t.Errorf("Str(%#v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestTruthy(t *testing.T) {
	falsy := []string{`""`, `0`, `-0`, `0.0`, `-0.0`, `0e5`, `[]`, `{}`, `null`, `false`}
	truthy := []string{`" "`, `"0"`, `"false"`, `1`, `-1`, `0.5`, `1e-300`, `[0]`, `[[]]`, `{"a":null}`, `true`,
		`99999999999999999999`}
	for _, in := range falsy {
		if textfmt.Truthy(decode(t, in)) {
			t.Errorf("Truthy(%s) = true", in)
		}
	}
	for _, in := range truthy {
		if !textfmt.Truthy(decode(t, in)) {
			t.Errorf("Truthy(%s) = false", in)
		}
	}
	if !textfmt.Truthy(math.NaN()) {
		t.Error("Truthy(NaN) = false")
	}
}

func TestIntConvertsValueOrReturnsDefault(t *testing.T) {
	const def = -1
	tests := []struct {
		in   any
		want int
	}{
		{nil, def},
		{false, def},
		{true, 1},
		{int64(0), def},
		{int64(42), 42},
		{int64(-7), -7},
		{0.0, def},
		{2.9, 2},
		{-2.9, -2},
		{1e30, math.MaxInt},
		{-1e30, math.MinInt},
		{math.Inf(1), def},
		{math.NaN(), def},
		{new(big.Int).Lsh(big.NewInt(1), 100), math.MaxInt},
		{new(big.Int).Lsh(big.NewInt(-1), 100), math.MinInt},
		{[]any{int64(1)}, def},
		{textfmt.Object{{Key: "a", Value: int64(1)}}, def},
		{"", def},
		{" 42 ", 42},
		{"+5", 5},
		{"-5", -5},
		{"-0", 0},
		{"007", 7},
		{"1_000", 1000},
		{"1_2_3", 123},
		{"0_7", 7},
		{"\u00a012\u3000", 12},
		{"\v5\f", 5},
		{"5\u0085", 5},
		{"１２", 12},
		{"٣", 3},
		{"๓", 3},
		{"\U0001d7d85", 5},
		{"99999999999999999999", math.MaxInt},
		{"-99999999999999999999", math.MinInt},
		{strings.Repeat("1", 4301), def},
		{"1__0", def},
		{"_1", def},
		{"1_", def},
		{"+_1", def},
		{"4.0", def},
		{"1e3", def},
		{"0x10", def},
		{"  ", def},
		{"+", def},
		{"- 5", def},
		{"\x1c5", def},
		{"\x7f5", def},
		{"5\u200b", def},
	}
	for _, tt := range tests {
		if got := textfmt.Int(tt.in, def); got != tt.want {
			t.Errorf("Int(%#v) = %d, want %d", tt.in, got, tt.want)
		}
	}
}
