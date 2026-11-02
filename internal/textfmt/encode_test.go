package textfmt_test

import (
	"math"
	"math/big"
	"runtime"
	"strings"
	"testing"

	"github.com/sarpaydemir/melinda/internal/textfmt"
)

func TestEncodeWritesCompactJSONWithCharactersAsTheyAre(t *testing.T) {
	v := struct {
		T string `json:"t"`
		N []any  `json:"n"`
	}{"İş <a> & \u2028 \u2029 \x7f \x01 \b \f é 😀 \\ \" / \\u2028", []any{1, nil, true}}
	got, err := textfmt.Encode(v)
	want := "{\"t\":\"İş <a> & \u2028 \u2029 \x7f \\u0001 \\b \\f é 😀 \\\\ \\\" / \\\\u2028\",\"n\":[1,null,true]}"
	if string(got) != want || err != nil {
		t.Errorf("Encode = %q, %v; want %q", got, err, want)
	}
}

func TestEncodeASCIIWritesObjectsInOrderWithSpacedSeparatorsAndASCIIEscapes(t *testing.T) {
	v := textfmt.Object{
		{Key: "a", Value: "İş <a> & \u2028 \x7f \x01 \b\f\n\t é 😀 \\ \" /"},
		{Key: "b", Value: []any{int64(1), nil, true, textfmt.Object{{Key: "c", Value: textfmt.Object{}}}}},
		{Key: "d", Value: []any{}},
	}
	got, err := textfmt.EncodeASCII(v)
	want := `{"a": "\u0130\u015f <a> & \u2028 \u007f \u0001 \b\f\n\t \u00e9 \ud83d\ude00 \\ \" /", "b": [1, null, true, {"c": {}}], "d": []}`
	if string(got) != want || err != nil {
		t.Errorf("EncodeASCII = %s, %v\nwant %s", got, err, want)
	}
}

func TestEncodeASCIIWritesLargeIntegersAndFloats(t *testing.T) {
	huge, _ := new(big.Int).SetString("-98765432109876543210", 10)
	got, err := textfmt.EncodeASCII([]any{huge, 0.5, "\r"})
	if want := `[-98765432109876543210, 0.5, "\r"]`; string(got) != want || err != nil {
		t.Errorf("EncodeASCII = %s, %v; want %s", got, err, want)
	}
}

func TestEncodeASCIIRejectsNonFiniteFloatsAndUnknownTypes(t *testing.T) {
	for _, v := range []any{math.Inf(1), textfmt.Object{{Key: "n", Value: []any{math.NaN()}}}, map[string]any{}} {
		if got, err := textfmt.EncodeASCII(v); err == nil {
			t.Errorf("EncodeASCII(%v) = %s, want an error", v, got)
		}
	}
}

func TestEncodeASCIIHandlesDeepNestingInLinearSpace(t *testing.T) {
	const depth = 10000
	v := any(textfmt.Object{})
	for range depth {
		v = textfmt.Object{{Key: "a", Value: []any{v}}}
	}
	got, allocated, err := allocatedBy(func() ([]byte, error) { return textfmt.EncodeASCII(v) })
	if want := strings.Repeat(`{"a": [`, depth) + "{}" + strings.Repeat("]}", depth); string(got) != want || err != nil {
		t.Errorf("EncodeASCII of %d levels = %d bytes, %v; want %d bytes", depth, len(got), err, len(want))
	}
	if allocated > 16*uint64(len(got)) {
		t.Errorf("EncodeASCII of %d levels allocated %d bytes for %d bytes of output", depth, allocated, len(got))
	}
}

func TestEncodeCompactHandlesDeepNestingInLinearSpace(t *testing.T) {
	const depth = 10000
	v := any(textfmt.Object{})
	for range depth {
		v = textfmt.Object{{Key: "<a>", Value: []any{v}}}
	}
	got, allocated, err := allocatedBy(func() ([]byte, error) { return textfmt.EncodeCompact(v) })
	if want := strings.Repeat(`{"<a>":[`, depth) + "{}" + strings.Repeat("]}", depth); string(got) != want || err != nil {
		t.Errorf("EncodeCompact of %d levels = %d bytes, %v; want %d bytes", depth, len(got), err, len(want))
	}
	if allocated > 16*uint64(len(got)) {
		t.Errorf("EncodeCompact of %d levels allocated %d bytes for %d bytes of output", depth, allocated, len(got))
	}
}

func allocatedBy(encode func() ([]byte, error)) ([]byte, uint64, error) {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got, err := encode()
	runtime.ReadMemStats(&after)
	return got, after.TotalAlloc - before.TotalAlloc, err
}
