package prompt

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Template is a compiled prompt text; it is immutable and safe for concurrent use.
type Template struct {
	segments []segment
}

// segment is literal text, or a placeholder when name is set.
type segment struct {
	literal, name string
}

var namePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Raw returns a template that renders text unchanged, braces included.
func Raw(text string) *Template {
	return &Template{segments: []segment{{literal: text}}}
}

// Parse compiles text, in which {name} is a placeholder for a name in allowed and {{ and }} are literal braces.
// Any other use of a brace is an error that reports its byte offset.
func Parse(text string, allowed []string) (*Template, error) {
	t := &Template{}
	var lit strings.Builder
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c != '{' && c != '}' {
			lit.WriteByte(c)
			continue
		}
		if i+1 < len(text) && text[i+1] == c {
			lit.WriteByte(c)
			i++
			continue
		}
		if c == '}' {
			return nil, errorAt(text, i, "unmatched '}'")
		}
		n := strings.IndexAny(text[i+1:], "{}")
		if n < 0 {
			return nil, errorAt(text, i, "unmatched '{'")
		}
		end := i + 1 + n
		if text[end] == '{' {
			return nil, errorAt(text, end, "nested '{' in field")
		}
		name := text[i+1 : end]
		if problem := checkName(name, allowed); problem != "" {
			return nil, errorAt(text, i, problem+" "+strconv.Quote(text[i:end+1]))
		}
		if lit.Len() > 0 {
			t.segments = append(t.segments, segment{literal: lit.String()})
			lit.Reset()
		}
		t.segments = append(t.segments, segment{name: name})
		i = end
	}
	if lit.Len() > 0 {
		t.segments = append(t.segments, segment{literal: lit.String()})
	}
	return t, nil
}

// checkName describes what makes the field content name unusable as a placeholder, or returns "".
func checkName(name string, allowed []string) string {
	switch k := strings.IndexAny(name, "!:"); {
	case name == "":
		return "empty field"
	case k >= 0 && name[k] == '!':
		return "conversion in field"
	case k >= 0:
		return "format spec in field"
	case strings.ContainsAny(name, ".["):
		return "attribute or index access in field"
	case strings.Trim(name, "0123456789") == "":
		return "positional field"
	case !namePattern.MatchString(name):
		return "invalid placeholder name"
	case !slices.Contains(allowed, name):
		return "unknown placeholder"
	}
	return ""
}

// errorAt reports problem at a byte offset of text, with the line and column (in characters) an editor shows.
func errorAt(text string, offset int, problem string) error {
	before := text[:offset]
	line := strings.Count(before, "\n") + 1
	column := utf8.RuneCountInString(before[strings.LastIndexByte(before, '\n')+1:]) + 1
	return fmt.Errorf("prompt: line %d, column %d (offset %d): %s; write {{ and }} for literal braces",
		line, column, offset, problem)
}

// Render substitutes values verbatim for the placeholders; a placeholder without a value is an error.
func (t *Template) Render(values map[string]string) (string, error) {
	var b strings.Builder
	for _, s := range t.segments {
		if s.name == "" {
			b.WriteString(s.literal)
			continue
		}
		v, ok := values[s.name]
		if !ok {
			return "", fmt.Errorf("prompt: no value for placeholder %q", s.name)
		}
		b.WriteString(v)
	}
	return b.String(), nil
}
