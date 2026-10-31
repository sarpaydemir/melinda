package prompt_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/sarpaydemir/dost/internal/prompt"
)

var (
	allowed = []string{"a", "b", "c", "_x1", "Mixed_Case", "braces", "unused"}
	values  = map[string]string{"a": "A", "b": "", "c": "çş", "_x1": "U", "Mixed_Case": "M", "braces": "{a}{{x}"}
)

func TestRenderSubstitutesFieldsAndCollapsesEscapes(t *testing.T) {
	tests := []struct{ text, want string }{
		{"", ""},
		{"plain", "plain"},
		{"{a}", "A"},
		{"{{", "{"},
		{"}}", "}"},
		{"{{a}}", "{a}"},
		{"{{{a}}}", "{A}"},
		{"{{{{a}}}}", "{{a}}"},
		{"a{{b}}c", "a{b}c"},
		{"{a}{b}", "A"},
		{"{a}{a}", "AA"},
		{"{b}{b}{b}", ""},
		{"{{{{", "{{"},
		{"x{{", "x{"},
		{"x}}", "x}"},
		{"{a}}}", "A}"},
		{"{{{a}", "{A"},
		{"{c}}}{{{c}", "çş}{çş"},
		{"ç{a}ş", "çAş"},
		{"{a}é{b}", "Aé"},
		{"→{c}←", "→çş←"},
		{"{_x1}{Mixed_Case}", "UM"},
		{"line1\n{a}\n\nline3\n", "line1\nA\n\nline3\n"},
		{"[{braces}]", "[{a}{{x}]"},
		{`{{ "k": str, "v": str | null }}`, `{ "k": str, "v": str | null }`},
	}
	for _, tt := range tests {
		tmpl, err := prompt.Parse(tt.text, allowed)
		if err != nil {
			t.Errorf("Parse(%q) error: %v", tt.text, err)
			continue
		}
		if got, err := tmpl.Render(values); err != nil || got != tt.want {
			t.Errorf("Parse(%q).Render() = %q, %v; want %q", tt.text, got, err, tt.want)
		}
	}
}

func TestParseRejectsMalformedTemplatesWithPosition(t *testing.T) {
	tests := []struct{ text, err string }{
		{"{", `line 1, column 1 (offset 0): unmatched '{'`},
		{"a{", `line 1, column 2 (offset 1): unmatched '{'`},
		{"{a", `line 1, column 1 (offset 0): unmatched '{'`},
		{"{{{", `line 1, column 3 (offset 2): unmatched '{'`},
		{"}", `line 1, column 1 (offset 0): unmatched '}'`},
		{"a}", `line 1, column 2 (offset 1): unmatched '}'`},
		{"a}b", `line 1, column 2 (offset 1): unmatched '}'`},
		{"}{", `line 1, column 1 (offset 0): unmatched '}'`},
		{"}}}", `line 1, column 3 (offset 2): unmatched '}'`},
		{"{{}", `line 1, column 3 (offset 2): unmatched '}'`},
		{"{{a}", `line 1, column 4 (offset 3): unmatched '}'`},
		{"{a}}", `line 1, column 4 (offset 3): unmatched '}'`},
		{"{a}x}", `line 1, column 5 (offset 4): unmatched '}'`},
		{"{a{b}}", `line 1, column 3 (offset 2): nested '{' in field`},
		{"{a{", `line 1, column 3 (offset 2): nested '{' in field`},
		{"{a:{b}}", `line 1, column 4 (offset 3): nested '{' in field`},
		{"{}", `line 1, column 1 (offset 0): empty field "{}"`},
		{"{}}", `line 1, column 1 (offset 0): empty field "{}"`},
		{"{0}", `line 1, column 1 (offset 0): positional field "{0}"`},
		{"{01}", `line 1, column 1 (offset 0): positional field "{01}"`},
		{"{a.b}", `line 1, column 1 (offset 0): attribute or index access in field "{a.b}"`},
		{"{a[0]}", `line 1, column 1 (offset 0): attribute or index access in field "{a[0]}"`},
		{"{a[}]}", `line 1, column 1 (offset 0): attribute or index access in field "{a[}"`},
		{"{a!r}", `line 1, column 1 (offset 0): conversion in field "{a!r}"`},
		{"{a!s}", `line 1, column 1 (offset 0): conversion in field "{a!s}"`},
		{"{a!}", `line 1, column 1 (offset 0): conversion in field "{a!}"`},
		{"{!r}", `line 1, column 1 (offset 0): conversion in field "{!r}"`},
		{"{a:>10}", `line 1, column 1 (offset 0): format spec in field "{a:>10}"`},
		{"{a:}", `line 1, column 1 (offset 0): format spec in field "{a:}"`},
		{"{:}", `line 1, column 1 (offset 0): format spec in field "{:}"`},
		{"{ a}", `line 1, column 1 (offset 0): invalid placeholder name "{ a}"`},
		{"{a }", `line 1, column 1 (offset 0): invalid placeholder name "{a }"`},
		{"{a-b}", `line 1, column 1 (offset 0): invalid placeholder name "{a-b}"`},
		{"{1a}", `line 1, column 1 (offset 0): invalid placeholder name "{1a}"`},
		{"{-1}", `line 1, column 1 (offset 0): invalid placeholder name "{-1}"`},
		{"{ä}", `line 1, column 1 (offset 0): invalid placeholder name "{ä}"`},
		{"{a]}", `line 1, column 1 (offset 0): invalid placeholder name "{a]}"`},
		{"{a\n}", `line 1, column 1 (offset 0): invalid placeholder name "{a\n}"`},
		{"{d}", `line 1, column 1 (offset 0): unknown placeholder "{d}"`},
		{"ç{a}{d}", `line 1, column 5 (offset 5): unknown placeholder "{d}"`},
		{"ab\nçd}", `line 2, column 3 (offset 6): unmatched '}'`},
		{"x\n\n{", `line 3, column 1 (offset 3): unmatched '{'`},
		{`Output: {"query": "..."}`, `line 1, column 9 (offset 8): format spec in field "{\"query\": \"...\"}"`},
	}
	for _, tt := range tests {
		tmpl, err := prompt.Parse(tt.text, allowed)
		if want := "prompt: " + tt.err + "; write {{ and }} for literal braces"; err == nil || err.Error() != want {
			t.Errorf("Parse(%q) = %v, %v; want error %q", tt.text, tmpl, err, want)
		}
	}
}

func TestRenderFailsWhenAUsedPlaceholderHasNoValue(t *testing.T) {
	tmpl, err := prompt.Parse("{a} and {b}", allowed)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := tmpl.Render(map[string]string{"a": "x"}); err == nil || err.Error() != `prompt: no value for placeholder "b"` {
		t.Errorf("Render without b = %q, %v; want missing-value error", got, err)
	}
	if got, err := tmpl.Render(map[string]string{"a": "x", "b": "y", "extra": "z"}); err != nil || got != "x and y" {
		t.Errorf("Render with an extra value = %q, %v; want %q", got, err, "x and y")
	}
}

func TestRawKeepsEveryBrace(t *testing.T) {
	text := `Output JSON: [{"fact": "..."}] or NULL. {{a}} {a} } {`
	got, err := prompt.Raw(text).Render(nil)
	if err != nil || got != text {
		t.Errorf("Raw(%q).Render(nil) = %q, %v; want the text unchanged", text, got, err)
	}
}

func TestTemplateRendersConcurrently(t *testing.T) {
	tmpl, err := prompt.Parse("{a}-{c}", allowed)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				if got, err := tmpl.Render(values); err != nil || got != "A-çş" {
					t.Errorf("Render() = %q, %v", got, err)
					return
				}
			}
		})
	}
	wg.Wait()
}

var (
	unescape         = strings.NewReplacer("{{", "{", "}}", "}")
	fieldsAndEscapes = strings.NewReplacer("{{", "", "}}", "", "{a}", "")
)

func FuzzParse(f *testing.F) {
	for _, seed := range []string{"", "{a}", "{{{a}}}", "}}{{", "{a}}}{{{a}", "ç{a}ş", "{a!r}", "{a:{a}}", "{", "}"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		tmpl, err := prompt.Parse(text, []string{"a"})
		if wantOK := !strings.ContainsAny(fieldsAndEscapes.Replace(text), "{}"); (err == nil) != wantOK {
			t.Fatalf("Parse(%q) error = %v, want accepted = %v", text, err, wantOK)
		}
		if err != nil {
			return
		}
		// Rendering each field as its own source text must give back the text with only the escapes collapsed.
		got, err := tmpl.Render(map[string]string{"a": "{a}"})
		if want := unescape.Replace(text); err != nil || got != want {
			t.Fatalf("Parse(%q).Render() = %q, %v; want %q", text, got, err, want)
		}
	})
}
