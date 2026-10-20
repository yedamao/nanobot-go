package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Expr is a parsed standard 5-field cron expression:
//
//	minute hour day-of-month month day-of-week
//
// Each field accepts *, numbers, ranges (a-b), lists (a,b), steps (*/n,
// a-b/n, a/n), and month and weekday names (JAN-DEC, SUN-SAT); 0 and 7
// both mean Sunday. The macros @yearly, @annually, @monthly, @weekly,
// @daily, @midnight and @hourly are accepted. As in Vixie cron (and
// nanobot's croniter), a day matches when both day fields are
// unrestricted, when only one is restricted and it matches, or when both
// are restricted and either matches.
type Expr struct {
	minute, hour, dom, month, dow uint64
	// domStar and dowStar record a field written as * (or */n), which
	// makes the other day field decide alone.
	domStar, dowStar bool
}

type field struct {
	name     string
	min, max int
	names    map[string]int
}

var (
	minuteField = field{name: "minute", min: 0, max: 59}
	hourField   = field{name: "hour", min: 0, max: 23}
	domField    = field{name: "day of month", min: 1, max: 31}
	monthField  = field{name: "month", min: 1, max: 12, names: map[string]int{
		"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
	}}
	dowField = field{name: "day of week", min: 0, max: 7, names: map[string]int{
		"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
	}}
)

var macros = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

// ParseExpr parses a 5-field cron expression or macro.
func ParseExpr(s string) (*Expr, error) {
	spec := strings.TrimSpace(s)
	if m, ok := macros[strings.ToLower(spec)]; ok {
		spec = m
	}
	parts := strings.Fields(spec)
	if len(parts) != 5 {
		return nil, fmt.Errorf("cron expression %q: want 5 fields (minute hour day month weekday), got %d", s, len(parts))
	}
	var e Expr
	var err error
	if e.minute, _, err = minuteField.parse(parts[0]); err != nil {
		return nil, fmt.Errorf("cron expression %q: %w", s, err)
	}
	if e.hour, _, err = hourField.parse(parts[1]); err != nil {
		return nil, fmt.Errorf("cron expression %q: %w", s, err)
	}
	if e.dom, e.domStar, err = domField.parse(parts[2]); err != nil {
		return nil, fmt.Errorf("cron expression %q: %w", s, err)
	}
	if e.month, _, err = monthField.parse(parts[3]); err != nil {
		return nil, fmt.Errorf("cron expression %q: %w", s, err)
	}
	if e.dow, e.dowStar, err = dowField.parse(parts[4]); err != nil {
		return nil, fmt.Errorf("cron expression %q: %w", s, err)
	}
	if e.dow&(1<<7) != 0 { // 7 is Sunday too
		e.dow |= 1
	}
	return &e, nil
}

// parse returns the field's bit set and whether it starts with *.
func (f field) parse(s string) (bits uint64, star bool, err error) {
	for i, part := range strings.Split(s, ",") {
		b, st, err := f.parseTerm(part)
		if err != nil {
			return 0, false, err
		}
		bits |= b
		if i == 0 {
			star = st
		}
	}
	return bits, star && !strings.Contains(s, ","), nil
}

// parseTerm parses one list element: *, n, a-b, each optionally /step.
func (f field) parseTerm(s string) (uint64, bool, error) {
	rng, stepStr, hasStep := strings.Cut(s, "/")
	step := 1
	if hasStep {
		n, err := strconv.Atoi(stepStr)
		if err != nil || n <= 0 {
			return 0, false, fmt.Errorf("%s: bad step %q", f.name, stepStr)
		}
		step = n
	}
	var lo, hi int
	star := false
	switch {
	case rng == "*" || rng == "?":
		lo, hi, star = f.min, f.max, true
	case strings.Contains(rng, "-"):
		a, b, _ := strings.Cut(rng, "-")
		var err error
		if lo, err = f.value(a); err != nil {
			return 0, false, err
		}
		if hi, err = f.value(b); err != nil {
			return 0, false, err
		}
		if lo > hi {
			return 0, false, fmt.Errorf("%s: range %q is backwards", f.name, rng)
		}
	default:
		v, err := f.value(rng)
		if err != nil {
			return 0, false, err
		}
		lo, hi = v, v
		if hasStep { // a/n means a through the end
			hi = f.max
		}
	}
	var bits uint64
	for v := lo; v <= hi; v += step {
		bits |= 1 << uint(v)
	}
	return bits, star, nil
}

func (f field) value(s string) (int, error) {
	if v, ok := f.names[strings.ToLower(s)]; ok {
		return v, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s: bad value %q", f.name, s)
	}
	if v < f.min || v > f.max {
		return 0, fmt.Errorf("%s: %d out of range [%d, %d]", f.name, v, f.min, f.max)
	}
	return v, nil
}

// Next returns the first time strictly after t that matches e, in t's
// location, or the zero time if there is none within five years (an
// impossible date such as 30 February). Wall-clock times skipped by a
// daylight-saving jump do not match; times in a repeated hour match on
// both passes.
func (e *Expr) Next(t time.Time) time.Time {
	loc := t.Location()
	// Start at the next whole minute.
	t = t.Truncate(time.Minute).Add(time.Minute)
	limit := t.Year() + 5

	// forward moves t to next, or by a minute if a daylight-saving
	// transition made next not later than t, so the search always ends.
	forward := func(next time.Time) {
		if next.After(t) {
			t = next
		} else {
			t = t.Add(time.Minute)
		}
	}
	for t.Year() <= limit {
		if e.month&(1<<uint(t.Month())) == 0 {
			forward(time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc))
			continue
		}
		if !e.dayMatches(t) {
			forward(time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc))
			continue
		}
		if e.hour&(1<<uint(t.Hour())) == 0 {
			forward(time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc))
			continue
		}
		if e.minute&(1<<uint(t.Minute())) == 0 {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}

func (e *Expr) dayMatches(t time.Time) bool {
	dom := e.dom&(1<<uint(t.Day())) != 0
	dow := e.dow&(1<<uint(t.Weekday())) != 0
	if e.domStar || e.dowStar {
		return dom && dow
	}
	return dom || dow
}
