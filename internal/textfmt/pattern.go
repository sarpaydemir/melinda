package textfmt

import (
	"fmt"
	"strings"
)

// classes holds the members of the Unicode-aware \w, \s, and \d sets; \W, \S, and \D are their complements.
var classes = map[byte]string{
	'w': `\p{L}\p{N}_`,
	's': `\t-\r\x1c-\x1f\x85\p{Z}`,
	'd': `\p{Nd}`,
}

// TranslatePattern rewrites \w \W \s \S \d \D in p as Unicode-aware RE2 classes. It rejects what RE2 would read
// differently or not at all: \b, \B, lookaround, backreferences, and \W \S \D inside a set.
func TranslatePattern(p string) (string, error) {
	var b strings.Builder
	inSet := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '\\' && i+1 < len(p):
			i++
			e := p[i]
			set, isClass := classes[e|0x20]
			negated := e < 'a'
			switch {
			case isClass && !inSet && negated:
				b.WriteString("[^" + set + "]")
			case isClass && !inSet:
				b.WriteString("[" + set + "]")
			case isClass && negated:
				return "", fmt.Errorf("textfmt: pattern %q: \\%c inside a set is not supported", p, e)
			case isClass:
				b.WriteString(set)
			case !inSet && (e == 'b' || e == 'B'):
				return "", fmt.Errorf("textfmt: pattern %q: word boundary \\%c is not supported", p, e)
			case !inSet && '1' <= e && e <= '9' && !isOctalEscape(p[i:]):
				return "", fmt.Errorf("textfmt: pattern %q: backreference \\%c is not supported", p, e)
			default:
				b.WriteByte(c)
				b.WriteByte(e)
			}
			continue
		case c == '[' && !inSet:
			inSet = true
			b.WriteByte(c)
			if strings.HasPrefix(p[i+1:], "^") {
				b.WriteByte('^')
				i++
			}
			if strings.HasPrefix(p[i+1:], "]") {
				b.WriteByte(']')
				i++
			}
			continue
		case c == ']' && inSet:
			inSet = false
		case c == '(' && !inSet:
			for _, group := range []string{"(?=", "(?!", "(?<=", "(?<!", "(?P="} {
				if strings.HasPrefix(p[i:], group) {
					return "", fmt.Errorf("textfmt: pattern %q: %s...) is not supported", p, group)
				}
			}
		}
		b.WriteByte(c)
	}
	return b.String(), nil
}

// isOctalEscape reports whether s, the text after a backslash, starts with three octal digits.
func isOctalEscape(s string) bool {
	if len(s) < 3 {
		return false
	}
	for i := range 3 {
		if s[i] < '0' || s[i] > '7' {
			return false
		}
	}
	return true
}
