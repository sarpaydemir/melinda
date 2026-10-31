package textfmt_test

import (
	"regexp"
	"testing"

	"github.com/sarpaydemir/dost/internal/textfmt"
)

const (
	word  = `\p{L}\p{N}_`
	space = `\t-\r\x1c-\x1f\x85\p{Z}`
)

func TestTranslatePatternRewritesClassShorthands(t *testing.T) {
	tests := []struct{ in, want string }{
		{`@\w+\s*`, `@[` + word + `]+[` + space + `]*`},
		{`\W\S\D\d`, `[^` + word + `][^` + space + `][^\p{Nd}][\p{Nd}]`},
		{`/cmd\s+(.+)`, `/cmd[` + space + `]+(.+)`},
		{`[\w.-]+@[^\s@]`, `[` + word + `.-]+@[^` + space + `@]`},
		{`[]\d]`, `[]\p{Nd}]`},
		{`[^]\w]`, `[^]` + word + `]`},
		{`[\]\w]`, `[\]` + word + `]`},
		{`\\w \[\w\]`, `\\w \[[` + word + `]\]`},
		{`\101\0\.\(?=`, `\101\0\.\(?=`},
		{`(?i)(?P<name>a)(?:b)`, `(?i)(?P<name>a)(?:b)`},
		{`[(?=]`, `[(?=]`},
		{`ç\w`, `ç[` + word + `]`},
	}
	for _, tt := range tests {
		got, err := textfmt.TranslatePattern(tt.in)
		if err != nil {
			t.Errorf("TranslatePattern(%s): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("TranslatePattern(%s) = %s, want %s", tt.in, got, tt.want)
		}
		if _, err := regexp.Compile(got); err != nil {
			t.Errorf("TranslatePattern(%s) = %s does not compile: %v", tt.in, got, err)
		}
	}
}

func TestTranslatePatternRejectsConstructsWithoutEquivalent(t *testing.T) {
	for _, in := range []string{
		`\bword`, `a\B`, `a(?=b)`, `a(?!b)`, `(?<=a)b`, `(?<!a)b`,
		`(a)\1`, `(a)\12`, `(a)\18`, `(a)\9`, `(?P<x>a)(?P=x)`,
		`[\W]`, `[a\S]`, `[\D]`,
	} {
		if got, err := textfmt.TranslatePattern(in); err == nil {
			t.Errorf("TranslatePattern(%s) = %s, want error", in, got)
		}
	}
}

func TestTranslatedClassesMatchUnicodeText(t *testing.T) {
	strip := func(t *testing.T, s string) string {
		t.Helper()
		p, err := textfmt.TranslatePattern(`@\w+\s*`)
		if err != nil {
			t.Fatal(err)
		}
		return regexp.MustCompile(p).ReplaceAllString(s, "")
	}
	tests := []struct{ in, want string }{
		{"@ali_veli  hi", "hi"},
		{"@İsmail\u00a0x", "x"},
		{"@a\u0301b c", "\u0301b c"},
		{"@x\x1cy", "y"},
		{"@x\v\x1fy", "y"},
		{"@١٢ z", "z"},
		{"@x²\u3000y", "y"},
		{"a@b\u200bc", "a\u200bc"},
	}
	for _, tt := range tests {
		if got := strip(t, tt.in); got != tt.want {
			t.Errorf("strip mentions from %q = %q, want %q", tt.in, got, tt.want)
		}
	}

	digit := func(s string) bool {
		p, _ := textfmt.TranslatePattern(`^\d$`)
		return regexp.MustCompile(p).MatchString(s)
	}
	for s, want := range map[string]bool{"7": true, "٣": true, "１": true, "²": false, "Ⅻ": false, "a": false} {
		if got := digit(s); got != want {
			t.Errorf(`\d matches %q = %v, want %v`, s, got, want)
		}
	}
}
