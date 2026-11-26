package cron

import (
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestMatches(t *testing.T) {
	cases := []struct {
		expr string
		when string
		want bool
	}{
		{"* * * * *", "2026-01-01 00:00", true},
		{"*/15 * * * *", "2026-01-01 10:30", true},
		{"*/15 * * * *", "2026-01-01 10:31", false},
		{"0 3 * * *", "2026-01-01 03:00", true},
		{"0 3 * * *", "2026-01-01 04:00", false},
		{"0 9-17 * * 1-5", "2026-11-26 12:00", true},  // Thursday
		{"0 9-17 * * 1-5", "2026-11-28 12:00", false}, // Saturday
		{"0 0 1,15 * *", "2026-03-15 00:00", true},
		{"0 0 * * 7", "2026-11-29 00:00", true},  // 7 = Sunday
		{"0 0 13 * 5", "2026-11-13 00:00", true}, // Friday the 13th: both match
		{"0 0 13 * 5", "2026-11-20 00:00", true}, // dom OR dow when both are restricted
		{"0 0 13 * 5", "2026-11-21 00:00", false},
		{"@daily", "2026-06-01 00:00", true},
		{"@hourly", "2026-06-01 07:00", true},
		{"@hourly", "2026-06-01 07:01", false},
		{"5-59/20 * * * *", "2026-06-01 07:25", true},
	}
	for _, c := range cases {
		s, err := Parse(c.expr)
		if err != nil {
			t.Fatalf("%q: %v", c.expr, err)
		}
		if got := s.Matches(at(c.when)); got != c.want {
			t.Errorf("%q at %s = %v, want %v", c.expr, c.when, got, c.want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, expr := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "*/0 * * * *", "a * * * *", "5-1 * * * *", "@never"} {
		if _, err := Parse(expr); err == nil {
			t.Errorf("%q should not parse", expr)
		}
	}
}

func TestNext(t *testing.T) {
	s, _ := Parse("30 2 * * *")
	if got := s.Next(at("2026-01-01 02:30")); !got.Equal(at("2026-01-02 02:30")) {
		t.Fatalf("next = %v", got)
	}
}
