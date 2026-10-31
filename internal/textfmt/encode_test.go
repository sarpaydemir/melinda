package textfmt_test

import (
	"testing"

	"github.com/sarpaydemir/dost/internal/textfmt"
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
