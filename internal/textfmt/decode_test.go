package textfmt_test

import (
	"encoding/json"
	"math"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sarpaydemir/dost/internal/textfmt"
)

func TestDecodeKeepsOrderAndNumberKinds(t *testing.T) {
	huge, _ := new(big.Int).SetString("-98765432109876543210", 10)
	tests := []struct {
		in   string
		want any
	}{
		{`null`, nil},
		{`true`, true},
		{` "x" `, "x"},
		{`1`, int64(1)},
		{`-0`, int64(0)},
		{`1.0`, 1.0},
		{`1E+2`, 100.0},
		{`9223372036854775807`, int64(math.MaxInt64)},
		{`-98765432109876543210`, huge},
		{`[]`, []any{}},
		{`{}`, textfmt.Object{}},
		{`{"b": 1, "a": [2, "x"]}`, textfmt.Object{{Key: "b", Value: int64(1)}, {Key: "a", Value: []any{int64(2), "x"}}}},
		{`{"a":1,"b":2,"a":3}`, textfmt.Object{{Key: "a", Value: int64(3)}, {Key: "b", Value: int64(2)}}},
		{"\t{\n\"k\"\r:\"\\u00e9\\ud83d\\ude00\\/\"}\n", textfmt.Object{{Key: "k", Value: "é😀/"}}},
	}
	for _, tt := range tests {
		got, err := textfmt.Decode([]byte(tt.in), false)
		if err != nil {
			t.Errorf("Decode(%s): %v", tt.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Decode(%s) = %#v, want %#v", tt.in, got, tt.want)
		}
	}
}

func TestDecodeOverflowingFloatsBecomeInfinityOrZero(t *testing.T) {
	tests := map[string]float64{`1e400`: math.Inf(1), `-1e400`: math.Inf(-1), `1e-400`: 0}
	for in, want := range tests {
		if got, err := textfmt.Decode([]byte(in), false); err != nil || got != want {
			t.Errorf("Decode(%s) = %v, %v; want %v", in, got, err, want)
		}
	}
}

func TestDecodeNonFiniteLiteralsOnlyWhenAllowed(t *testing.T) {
	tests := map[string]string{`NaN`: "nan", `Infinity`: "inf", `-Infinity`: "-inf", `[NaN, -Infinity]`: "[nan, -inf]"}
	for in, want := range tests {
		if _, err := textfmt.Decode([]byte(in), false); err == nil {
			t.Errorf("Decode(%s, false) succeeded, want error", in)
		}
		got, err := textfmt.Decode([]byte(in), true)
		if err != nil {
			t.Errorf("Decode(%s, true): %v", in, err)
			continue
		}
		if r := textfmt.Repr(got); r != want {
			t.Errorf("Repr(Decode(%s, true)) = %s, want %s", in, r, want)
		}
	}
}

func TestDecodeRejectsMalformedInput(t *testing.T) {
	for _, in := range []string{
		``, ` `, `nul`, `nullx`, `True`, `-`, `+1`, `01`, `1.`, `.5`, `1e`, `1e+`, `0x10`,
		`[1,]`, `[1 2]`, `{"a":1,}`, `{"a" 1}`, `{a:1}`, `{"a":1`, `"abc`, `"a\x"`, `"\u12"`,
		"\"tab\there\"", "\"\xff\"", `{} {}`, `NaN`, `-Infinity`,
		strings.Repeat("1", 4301), "-" + strings.Repeat("1", 4301), nested(10001),
	} {
		if got, err := textfmt.Decode([]byte(in), false); err == nil {
			t.Errorf("Decode(%.40q) = %#v, want error", in, got)
		}
	}
	for _, in := range []string{strings.Repeat("1", 4300), "-" + strings.Repeat("1", 4300), nested(10000)} {
		if _, err := textfmt.Decode([]byte(in), false); err != nil {
			t.Errorf("Decode(%.40q): %v", in, err)
		}
	}
}

func nested(depth int) string { return strings.Repeat("[", depth) + strings.Repeat("]", depth) }

func FuzzDecode(f *testing.F) {
	seeds := []string{`{"a":[1,2.5,"x",null,true]}`, `{"a":1,"a":2}`, `"\ud800"`, `-0.0e-0`, `[NaN]`, `12345678901234567890`}
	for _, seed := range seeds {
		f.Add([]byte(seed), false)
		f.Add([]byte(seed), true)
	}
	f.Fuzz(func(t *testing.T, data []byte, nonFinite bool) {
		v, err := textfmt.Decode(data, nonFinite)
		if err != nil {
			if !nonFinite && len(data) <= 4300 && utf8.Valid(data) && json.Valid(data) {
				t.Fatalf("Decode(%q) rejected valid JSON: %v", data, err)
			}
			return
		}
		if !nonFinite && !json.Valid(data) {
			t.Fatalf("Decode(%q) accepted invalid JSON", data)
		}
		textfmt.Repr(v)
		textfmt.Truthy(v)
		textfmt.Int(v, 0)
	})
}
