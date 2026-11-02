package textfmt_test

import (
	"testing"
	"time"

	"github.com/sarpaydemir/melinda/internal/textfmt"
)

func TestFormatTimePrintsUTCWithMicroseconds(t *testing.T) {
	plus3 := time.FixedZone("", 3*3600)
	tests := []struct {
		name string
		in   time.Time
		want string
	}{
		{"microseconds", time.Date(2024, 3, 5, 7, 8, 9, 123456000, time.UTC), "2024-03-05T07:08:09.123456+00:00"},
		{"whole second omits fraction", time.Date(2024, 3, 5, 7, 8, 9, 0, time.UTC), "2024-03-05T07:08:09+00:00"},
		{"leading zeros kept", time.Date(2024, 3, 5, 7, 8, 9, 10000, time.UTC), "2024-03-05T07:08:09.000010+00:00"},
		{"trailing zeros kept", time.Date(2024, 3, 5, 7, 8, 9, 500000000, time.UTC), "2024-03-05T07:08:09.500000+00:00"},
		{"nanoseconds truncated", time.Date(2024, 3, 5, 7, 8, 9, 999999999, time.UTC), "2024-03-05T07:08:09.999999+00:00"},
		{"sub-microsecond omits fraction", time.Date(2024, 3, 5, 7, 8, 9, 999, time.UTC), "2024-03-05T07:08:09+00:00"},
		{"converted to UTC", time.Date(2024, 3, 5, 1, 0, 0, 0, plus3), "2024-03-04T22:00:00+00:00"},
		{"four-digit year", time.Date(999, 1, 2, 3, 4, 5, 0, time.UTC), "0999-01-02T03:04:05+00:00"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := textfmt.FormatTime(tt.in); got != tt.want {
				t.Errorf("FormatTime = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseTimeAcceptsISODateTimes(t *testing.T) {
	tests := []struct {
		in    string
		want  time.Time
		naive bool
	}{
		{"2024-03-05", time.Date(2024, 3, 5, 0, 0, 0, 0, time.UTC), true},
		{"2024-03-05T07:08:09", time.Date(2024, 3, 5, 7, 8, 9, 0, time.UTC), true},
		{"2024-03-05 07:08:09", time.Date(2024, 3, 5, 7, 8, 9, 0, time.UTC), true},
		{"2024-03-05T07:08:09.1", time.Date(2024, 3, 5, 7, 8, 9, 100000000, time.UTC), true},
		{"2024-03-05T07:08:09.123", time.Date(2024, 3, 5, 7, 8, 9, 123000000, time.UTC), true},
		{"2024-03-05T07:08:09.123456", time.Date(2024, 3, 5, 7, 8, 9, 123456000, time.UTC), true},
		{"2024-03-05T07:08:09.1234567", time.Date(2024, 3, 5, 7, 8, 9, 123456000, time.UTC), true},
		{"2024-03-05T07:08:09Z", time.Date(2024, 3, 5, 7, 8, 9, 0, time.UTC), false},
		{"2024-03-05T07:08:09.000010+00:00", time.Date(2024, 3, 5, 7, 8, 9, 10000, time.UTC), false},
		{"2024-03-05T07:08:09-00:00", time.Date(2024, 3, 5, 7, 8, 9, 0, time.UTC), false},
		{"2024-03-05T07:08:09+03:00", time.Date(2024, 3, 5, 4, 8, 9, 0, time.UTC), false},
		{"2024-03-05T07:08:09.5-05:30", time.Date(2024, 3, 5, 12, 38, 9, 500000000, time.UTC), false},
		{"2024-03-05T07:08:09+23:59", time.Date(2024, 3, 4, 7, 9, 9, 0, time.UTC), false},
		{"2024-03-05T07:08:09+03:60", time.Date(2024, 3, 5, 3, 8, 9, 0, time.UTC), false},
		{"2024-02-29T23:59:59", time.Date(2024, 2, 29, 23, 59, 59, 0, time.UTC), true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, naive, err := textfmt.ParseTime(tt.in)
			if err != nil {
				t.Fatalf("ParseTime: %v", err)
			}
			if !got.Equal(tt.want) || naive != tt.naive {
				t.Errorf("ParseTime = %v naive=%v, want %v naive=%v", got, naive, tt.want, tt.naive)
			}
			if naive && got.Location() != time.UTC {
				t.Errorf("naive value in %v, want UTC", got.Location())
			}
		})
	}
}

func TestParseTimeRejectsInvalidValues(t *testing.T) {
	for _, in := range []string{
		"",
		"2024-03",
		"2024/03/05",
		" 2024-03-05",
		"2024-03-05 ",
		"0000-01-01",
		"2024-13-01",
		"2023-02-29",
		"2024-02-30",
		"2024-03-00",
		"2024-03-05T",
		"2024-03-05T24:00:00",
		"2024-03-05T07:60:00",
		"2024-03-05T07:08:60",
		"2024-03-05T07:08:09.",
		"2024-03-05T07:08:09z",
		"2024-03-05T07:08:09+24:00",
		"2024-03-05T07:08:09+23:60",
		"2024-03-05T07:08:09+3:00",
		"2024-03-05T7:08:09",
		"2024-03-05T07:08:09.12a",
		"２０２４-03-05",
		"+024-03-05",
	} {
		if got, _, err := textfmt.ParseTime(in); err == nil {
			t.Errorf("ParseTime(%q) = %v, want error", in, got)
		}
	}
}

func TestParseTimeReadsFormatTimeOutput(t *testing.T) {
	for _, in := range []time.Time{
		time.Date(2024, 3, 5, 7, 8, 9, 123456789, time.UTC),
		time.Date(2024, 3, 5, 7, 8, 9, 0, time.FixedZone("", -7*3600)),
	} {
		got, naive, err := textfmt.ParseTime(textfmt.FormatTime(in))
		if want := in.Truncate(time.Microsecond); err != nil || naive || !got.Equal(want) {
			t.Errorf("ParseTime(FormatTime(%v)) = %v naive=%v err=%v, want %v", in, got, naive, err, want)
		}
	}
}

func TestStrftimeUsesEnglishNames(t *testing.T) {
	ts := time.Date(2024, 3, 5, 7, 8, 9, 0, time.UTC)
	tests := []struct{ layout, want string }{
		{"%Y-%m-%d %H:%M", "2024-03-05 07:08"},
		{"%Y-%m-%d %H:%M:%S (%A)", "2024-03-05 07:08:09 (Tuesday)"},
		{"%a %B %b", "Tue March Mar"},
		{"100%%", "100%"},
		{"%%%", "%%"},
		{"%Q", "%Q"},
		{"trailing %", "trailing %"},
		{"%ç", "%ç"},
	}
	for _, tt := range tests {
		if got := textfmt.Strftime(ts, tt.layout); got != tt.want {
			t.Errorf("Strftime(%q) = %q, want %q", tt.layout, got, tt.want)
		}
	}
	if got := textfmt.Strftime(time.Date(999, 1, 1, 0, 0, 0, 0, time.UTC), "%Y"); got != "999" {
		t.Errorf("Strftime(year 999, %%Y) = %q, want unpadded %q", got, "999")
	}
}

func TestStrftimeNamesEveryDayAndMonth(t *testing.T) {
	days := []string{"Monday Mon", "Tuesday Tue", "Wednesday Wed", "Thursday Thu", "Friday Fri", "Saturday Sat",
		"Sunday Sun"}
	for i, want := range days {
		if got := textfmt.Strftime(time.Date(2024, 1, 1+i, 0, 0, 0, 0, time.UTC), "%A %a"); got != want {
			t.Errorf("day %d = %q, want %q", i, got, want)
		}
	}
	months := []string{"January Jan", "February Feb", "March Mar", "April Apr", "May May", "June Jun", "July Jul",
		"August Aug", "September Sep", "October Oct", "November Nov", "December Dec"}
	for i, want := range months {
		if got := textfmt.Strftime(time.Date(2024, time.Month(i+1), 1, 0, 0, 0, 0, time.UTC), "%B %b"); got != want {
			t.Errorf("month %d = %q, want %q", i+1, got, want)
		}
	}
}
