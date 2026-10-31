package textfmt_test

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sarpaydemir/dost/internal/textfmt"
)

func TestClipCountsCodePoints(t *testing.T) {
	tests := []struct {
		s    string
		n    int
		want string
	}{
		{"hello", 3, "hel"},
		{"hello", 5, "hello"},
		{"hello", 99, "hello"},
		{"hello", 0, ""},
		{"hello", -1, ""},
		{"", 3, ""},
		{"çğış", 2, "çğ"},
		{"İstanbul", 1, "İ"},
		{"😀😀😀", 2, "😀😀"},
		{"e\u0301x", 1, "e"},
		{"e\u0301x", 2, "e\u0301"},
		{"👨\u200d👩", 2, "👨\u200d"},
		{"a\xffb", 2, "a\xff"},
	}
	for _, tt := range tests {
		if got := textfmt.Clip(tt.s, tt.n); got != tt.want {
			t.Errorf("Clip(%q, %d) = %q, want %q", tt.s, tt.n, got, tt.want)
		}
	}
}

func FuzzClip(f *testing.F) {
	f.Add("😀a\u0301İ", 2)
	f.Add("plain", 9)
	f.Add("a\xffb", 1)
	f.Fuzz(func(t *testing.T, s string, n int) {
		got := textfmt.Clip(s, n)
		if !strings.HasPrefix(s, got) {
			t.Fatalf("Clip(%q, %d) = %q, not a prefix", s, n, got)
		}
		if want := min(max(n, 0), utf8.RuneCountInString(s)); utf8.RuneCountInString(got) != want {
			t.Fatalf("Clip(%q, %d) = %q, want %d code points", s, n, got, want)
		}
		if utf8.ValidString(s) && !utf8.ValidString(got) {
			t.Fatalf("Clip(%q, %d) = %q, split a UTF-8 sequence", s, n, got)
		}
	})
}

func TestTailKeepsLastElements(t *testing.T) {
	xs := []int{1, 2, 3, 4}
	tests := []struct {
		n    int
		want []int
	}{
		{1, []int{4}},
		{3, []int{2, 3, 4}},
		{4, []int{1, 2, 3, 4}},
		{9, []int{1, 2, 3, 4}},
		{0, []int{1, 2, 3, 4}},
		{-2, []int{1, 2, 3, 4}},
	}
	for _, tt := range tests {
		if got := textfmt.Tail(xs, tt.n); !slices.Equal(got, tt.want) {
			t.Errorf("Tail(%v, %d) = %v, want %v", xs, tt.n, got, tt.want)
		}
	}
	if got := textfmt.Tail([]string(nil), 3); len(got) != 0 {
		t.Errorf("Tail(nil, 3) = %v, want empty", got)
	}
}

func TestStripRemovesUnicodeWhitespace(t *testing.T) {
	tests := []struct{ in, want string }{
		{"\t\n\v\f\r x \x1c\x1d\x1e\x1f", "x"},
		{"\u0085\u00a0x\u3000\u2028\u2029\u1680\u202f\u205f\u2000\u200a", "x"},
		{"  a b  ", "a b"},
		{"\u200bx\u200b", "\u200bx\u200b"},
		{"\ufeffx", "\ufeffx"},
		{"\u180ex", "\u180ex"},
		{" \t ", ""},
	}
	for _, tt := range tests {
		if got := textfmt.Strip(tt.in); got != tt.want {
			t.Errorf("Strip(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestLowerAppliesFullUnicodeLowercasing(t *testing.T) {
	tests := []struct{ in, want string }{
		{"@BOT", "@bot"},
		{"İSTANBUL", "i\u0307stanbul"},
		{"ÇĞIÖŞÜ", "çğiöşü"},
		{"ıi", "ıi"},
		{"Σ", "σ"},
		{"ΑΣ", "ας"},
		{"ΑΣ1", "ας1"},
		{"1Σ", "1σ"},
		{"ΑΣΑ", "ασα"},
		{"ΑΣ ΑΣ. Σ", "ας ας. σ"},
		{"Α\u0301Σ ΑΣ\u0301", "α\u0301ς ας\u0301"},
		{"'ΑΣ' Α'Σ", "'ας' α'ς"},
		{"ǅ", "ǆ"},
		{"Ⅻ Ⓐ", "ⅻ ⓐ"},
		{"ẞ", "ß"},
		{"\u212a \u212b", "k \u00e5"},
		{"\U00010400", "\U00010428"},
		{"e\u0301😀", "e\u0301😀"},
	}
	for _, tt := range tests {
		if got := textfmt.Lower(tt.in); got != tt.want {
			t.Errorf("Lower(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
