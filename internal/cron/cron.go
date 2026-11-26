// Package cron parses standard five-field cron expressions.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed cron expression. Each field is a bitmask of allowed values.
type Schedule struct {
	minute, hour, dom, month, dow uint64
	domStar, dowStar              bool
}

var macros = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

// Parse parses "min hour day-of-month month day-of-week" or an @macro.
func Parse(expr string) (*Schedule, error) {
	expr = strings.TrimSpace(expr)
	if m, ok := macros[strings.ToLower(expr)]; ok {
		expr = m
	}
	f := strings.Fields(expr)
	if len(f) != 5 {
		return nil, fmt.Errorf("cron schedule %q must have 5 fields (minute hour day month weekday) or be one of @hourly, @daily, @weekly, @monthly, @yearly", expr)
	}
	var (
		s   Schedule
		err error
	)
	if s.minute, _, err = field(f[0], 0, 59); err != nil {
		return nil, fmt.Errorf("minute: %w", err)
	}
	if s.hour, _, err = field(f[1], 0, 23); err != nil {
		return nil, fmt.Errorf("hour: %w", err)
	}
	if s.dom, s.domStar, err = field(f[2], 1, 31); err != nil {
		return nil, fmt.Errorf("day of month: %w", err)
	}
	if s.month, _, err = field(f[3], 1, 12); err != nil {
		return nil, fmt.Errorf("month: %w", err)
	}
	if s.dow, s.dowStar, err = field(f[4], 0, 7); err != nil {
		return nil, fmt.Errorf("day of week: %w", err)
	}
	if s.dow&(1<<7) != 0 { // 7 is Sunday too
		s.dow |= 1
	}
	return &s, nil
}

func field(expr string, min, max int) (mask uint64, star bool, err error) {
	for _, part := range strings.Split(expr, ",") {
		rng, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			step, err = strconv.Atoi(stepStr)
			if err != nil || step <= 0 {
				return 0, false, fmt.Errorf("invalid step in %q", part)
			}
		}
		lo, hi := min, max
		switch {
		case rng == "*":
			if !hasStep {
				star = true
			}
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			if lo, err = strconv.Atoi(a); err != nil {
				return 0, false, fmt.Errorf("invalid value %q", part)
			}
			if hi, err = strconv.Atoi(b); err != nil {
				return 0, false, fmt.Errorf("invalid value %q", part)
			}
		default:
			if lo, err = strconv.Atoi(rng); err != nil {
				return 0, false, fmt.Errorf("invalid value %q", part)
			}
			hi = lo
			if hasStep {
				hi = max
			}
		}
		if lo < min || hi > max || lo > hi {
			return 0, false, fmt.Errorf("value %q out of range %d-%d", part, min, max)
		}
		for v := lo; v <= hi; v += step {
			mask |= 1 << uint(v)
		}
	}
	return mask, star, nil
}

// Matches reports whether t (truncated to the minute) satisfies the schedule.
func (s *Schedule) Matches(t time.Time) bool {
	if s.minute&(1<<uint(t.Minute())) == 0 || s.hour&(1<<uint(t.Hour())) == 0 || s.month&(1<<uint(t.Month())) == 0 {
		return false
	}
	dom := s.dom&(1<<uint(t.Day())) != 0
	dow := s.dow&(1<<uint(t.Weekday())) != 0
	// Classic cron: when both day fields are restricted, either may match.
	if !s.domStar && !s.dowStar {
		return dom || dow
	}
	return dom && dow
}

// Next returns the first matching minute strictly after t, or the zero time
// if there is none within five years.
func (s *Schedule) Next(t time.Time) time.Time {
	t = t.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if s.Matches(t) {
			return t
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}
}
