package textfmt

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// FormatTime prints t in UTC as YYYY-MM-DDTHH:MM:SS[.ffffff]+00:00, truncated to microseconds.
func FormatTime(t time.Time) string {
	t = t.UTC()
	layout := "2006-01-02T15:04:05"
	if t.Nanosecond() >= 1000 {
		layout += ".000000"
	}
	return t.Format(layout) + "+00:00"
}

// ParseTime parses YYYY-MM-DD[(T| )HH:MM:SS[.fraction][Z|±HH:MM]] and reports whether it had no offset.
// The fraction is truncated to microseconds; a naive value is returned in UTC.
func ParseTime(s string) (t time.Time, naive bool, err error) {
	invalid := func() (time.Time, bool, error) {
		return time.Time{}, false, fmt.Errorf("textfmt: invalid timestamp %q", s)
	}
	if len(s) < 10 || s[4] != '-' || s[7] != '-' {
		return invalid()
	}
	bad := false
	num := func(digits string) int {
		n, ok := atoi(digits)
		bad = bad || !ok
		return n
	}
	year, month, day := num(s[0:4]), num(s[5:7]), num(s[8:10])
	var hour, minute, sec, usec, offset int
	naive = true
	if rest := s[10:]; rest != "" {
		if len(rest) < 9 || (rest[0] != 'T' && rest[0] != ' ') || rest[3] != ':' || rest[6] != ':' {
			return invalid()
		}
		hour, minute, sec = num(rest[1:3]), num(rest[4:6]), num(rest[7:9])
		rest = rest[9:]
		if frac, ok := strings.CutPrefix(rest, "."); ok {
			n := 0
			for n < len(frac) && isDigit(frac[n]) {
				n++
			}
			bad = bad || n == 0
			usec = num((frac[:n] + "000000")[:6])
			rest = frac[n:]
		}
		switch {
		case rest == "":
		case rest == "Z":
			naive = false
		case len(rest) == 6 && (rest[0] == '+' || rest[0] == '-') && rest[3] == ':':
			offset = num(rest[1:3])*3600 + num(rest[4:6])*60
			if rest[0] == '-' {
				offset = -offset
			}
			naive = false
		default:
			return invalid()
		}
	}
	if bad || year < 1 || month < 1 || month > 12 || hour > 23 || minute > 59 || sec > 59 ||
		max(offset, -offset) >= 24*3600 {
		return invalid()
	}
	loc := time.UTC
	if offset != 0 {
		loc = time.FixedZone("", offset)
	}
	t = time.Date(year, time.Month(month), day, hour, minute, sec, usec*1000, loc)
	if t.Day() != day {
		return invalid()
	}
	return t, naive, nil
}

// Strftime formats t with the directives %Y %m %d %H %M %S %A %a %B %b %%, using English names.
// Other directives are copied unchanged.
func Strftime(t time.Time, layout string) string {
	var b strings.Builder
	for i := 0; i < len(layout); i++ {
		if layout[i] != '%' || i+1 == len(layout) {
			b.WriteByte(layout[i])
			continue
		}
		i++
		switch c := layout[i]; c {
		case 'Y':
			b.WriteString(strconv.Itoa(t.Year()))
		case 'm':
			fmt.Fprintf(&b, "%02d", int(t.Month()))
		case 'd':
			fmt.Fprintf(&b, "%02d", t.Day())
		case 'H':
			fmt.Fprintf(&b, "%02d", t.Hour())
		case 'M':
			fmt.Fprintf(&b, "%02d", t.Minute())
		case 'S':
			fmt.Fprintf(&b, "%02d", t.Second())
		case 'A':
			b.WriteString(t.Weekday().String())
		case 'a':
			b.WriteString(t.Weekday().String()[:3])
		case 'B':
			b.WriteString(t.Month().String())
		case 'b':
			b.WriteString(t.Month().String()[:3])
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// atoi parses a non-empty run of ASCII digits.
func atoi(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}
