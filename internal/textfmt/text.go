package textfmt

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Clip returns the first n code points of s.
func Clip(s string, n int) string {
	for i := range s {
		if n <= 0 {
			return s[:i]
		}
		n--
	}
	return s
}

// Tail returns the last n elements of xs, or all of them when n <= 0 or n >= len(xs).
func Tail[T any](xs []T, n int) []T {
	if n <= 0 || n >= len(xs) {
		return xs
	}
	return xs[len(xs)-n:]
}

// Strip removes leading and trailing Unicode whitespace, including U+001C..U+001F.
func Strip(s string) string { return strings.TrimFunc(s, isSpace) }

func isSpace(r rune) bool { return unicode.IsSpace(r) || ('\x1c' <= r && r <= '\x1f') }

// Lower applies full Unicode lowercasing: U+0130 becomes "i\u0307" and a word-final capital sigma becomes ς.
func Lower(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i, r := range s {
		switch r {
		case 'İ':
			b.WriteString("i\u0307")
		case 'Σ':
			if finalSigma(s, i) {
				b.WriteRune('ς')
			} else {
				b.WriteRune('σ')
			}
		default:
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// finalSigma reports whether the sigma at byte offset i follows a cased letter and does not precede one,
// ignoring case-ignorable characters in both directions.
func finalSigma(s string, i int) bool {
	before := s[:i]
	for before != "" {
		r, size := utf8.DecodeLastRuneInString(before)
		if !caseIgnorable(r) {
			if !cased(r) {
				return false
			}
			break
		}
		before = before[:len(before)-size]
	}
	if before == "" {
		return false
	}
	for _, r := range s[i+len("Σ"):] {
		if !caseIgnorable(r) {
			return !cased(r)
		}
	}
	return true
}

// cased reports the Unicode Cased property.
func cased(r rune) bool {
	return unicode.In(r, unicode.Lu, unicode.Ll, unicode.Lt, unicode.Other_Lowercase, unicode.Other_Uppercase)
}

// caseIgnorable reports the Unicode Case_Ignorable property; the listed runes are its Word_Break members.
func caseIgnorable(r rune) bool {
	switch r {
	case '\'', '.', ':', '\u00b7', '\u0387', '\u055f', '\u05f4', '\u2018', '\u2019', '\u2024',
		'\u2027', '\ufe13', '\ufe52', '\ufe55', '\uff07', '\uff0e', '\uff1a':
		return true
	}
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk)
}
