package cron

import (
	"strings"
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestExprNext(t *testing.T) {
	utc := time.UTC
	ny := mustLoc(t, "America/New_York")
	tests := []struct {
		expr string
		from time.Time
		want time.Time // zero: no next time
	}{
		{"* * * * *", time.Date(2026, 1, 1, 10, 0, 30, 0, utc), time.Date(2026, 1, 1, 10, 1, 0, 0, utc)},
		// Strictly after: a matching minute does not match itself.
		{"0 9 * * *", time.Date(2026, 1, 1, 9, 0, 0, 0, utc), time.Date(2026, 1, 2, 9, 0, 0, 0, utc)},
		{"0 9 * * *", time.Date(2026, 1, 1, 8, 59, 59, 0, utc), time.Date(2026, 1, 1, 9, 0, 0, 0, utc)},
		{"*/15 * * * *", time.Date(2026, 1, 1, 10, 7, 0, 0, utc), time.Date(2026, 1, 1, 10, 15, 0, 0, utc)},
		{"5/20 * * * *", time.Date(2026, 1, 1, 10, 26, 0, 0, utc), time.Date(2026, 1, 1, 10, 45, 0, 0, utc)},
		{"0 9-17/4 * * *", time.Date(2026, 1, 1, 14, 0, 0, 0, utc), time.Date(2026, 1, 1, 17, 0, 0, 0, utc)},
		{"0 0 1,15 * *", time.Date(2026, 1, 2, 0, 0, 0, 0, utc), time.Date(2026, 1, 15, 0, 0, 0, 0, utc)},
		// Weekday names; 2026-01-01 is a Thursday.
		{"0 8 * * mon-fri", time.Date(2026, 1, 2, 9, 0, 0, 0, utc), time.Date(2026, 1, 5, 8, 0, 0, 0, utc)},
		{"0 0 * * 7", time.Date(2026, 1, 1, 0, 0, 0, 0, utc), time.Date(2026, 1, 4, 0, 0, 0, 0, utc)},
		{"0 0 1 jun *", time.Date(2026, 7, 1, 0, 0, 0, 0, utc), time.Date(2027, 6, 1, 0, 0, 0, 0, utc)},
		// Both day fields restricted: either matches (the 13th, or a Friday).
		{"0 0 13 * fri", time.Date(2026, 1, 1, 0, 0, 0, 0, utc), time.Date(2026, 1, 2, 0, 0, 0, 0, utc)},
		{"0 0 13 * fri", time.Date(2026, 1, 9, 1, 0, 0, 0, utc), time.Date(2026, 1, 13, 0, 0, 0, 0, utc)},
		// One restricted: it alone decides.
		{"0 0 * * fri", time.Date(2026, 1, 9, 1, 0, 0, 0, utc), time.Date(2026, 1, 16, 0, 0, 0, 0, utc)},
		{"0 0 29 2 *", time.Date(2026, 1, 1, 0, 0, 0, 0, utc), time.Date(2028, 2, 29, 0, 0, 0, 0, utc)},
		{"@daily", time.Date(2026, 1, 1, 12, 0, 0, 0, utc), time.Date(2026, 1, 2, 0, 0, 0, 0, utc)},
		{"@hourly", time.Date(2026, 1, 1, 12, 0, 0, 0, utc), time.Date(2026, 1, 1, 13, 0, 0, 0, utc)},
		{"0 0 30 2 *", time.Date(2026, 1, 1, 0, 0, 0, 0, utc), time.Time{}},
		// Year end wraps.
		{"0 0 1 1 *", time.Date(2026, 12, 31, 23, 59, 0, 0, utc), time.Date(2027, 1, 1, 0, 0, 0, 0, utc)},
		// In New York: 9:00 local is 13:00 UTC under EDT.
		{"0 9 * * *", time.Date(2026, 10, 20, 8, 0, 0, 0, ny), time.Date(2026, 10, 20, 9, 0, 0, 0, ny)},
		// 2:30 does not exist on 2026-03-08 (spring forward).
		{"30 2 * * *", time.Date(2026, 3, 8, 0, 0, 0, 0, ny), time.Date(2026, 3, 9, 2, 30, 0, 0, ny)},
		// 3:00 after the jump is reached.
		{"0 3 * * *", time.Date(2026, 3, 8, 0, 0, 0, 0, ny), time.Date(2026, 3, 8, 3, 0, 0, 0, ny)},
		// Fall back (2026-11-01): the day still advances.
		{"0 12 * * *", time.Date(2026, 11, 1, 0, 30, 0, 0, ny), time.Date(2026, 11, 1, 12, 0, 0, 0, ny)},
	}
	for _, tt := range tests {
		e, err := ParseExpr(tt.expr)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", tt.expr, err)
		}
		if got := e.Next(tt.from); !got.Equal(tt.want) {
			t.Errorf("%q.Next(%s) = %s, want %s", tt.expr, tt.from, got, tt.want)
		}
	}
}

func TestParseExprErrors(t *testing.T) {
	for _, tt := range []struct{ expr, want string }{
		{"", "want 5 fields"},
		{"* * * *", "want 5 fields"},
		{"* * * * * *", "want 5 fields"},
		{"60 * * * *", "minute: 60 out of range"},
		{"* 24 * * *", "hour: 24 out of range"},
		{"* * 0 * *", "day of month: 0 out of range"},
		{"* * * 13 *", "month: 13 out of range"},
		{"* * * * 8", "day of week: 8 out of range"},
		{"*/0 * * * *", "bad step"},
		{"5-1 * * * *", "backwards"},
		{"x * * * *", "bad value"},
		{"@reboot", "want 5 fields"},
	} {
		_, err := ParseExpr(tt.expr)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("ParseExpr(%q) = %v, want an error containing %q", tt.expr, err, tt.want)
		}
	}
}

func TestScheduleNext(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		s    Schedule
		want time.Time
		ok   bool
	}{
		{"at future", Schedule{Kind: KindAt, At: now.Add(time.Hour)}, now.Add(time.Hour), true},
		{"at past", Schedule{Kind: KindAt, At: now.Add(-time.Hour)}, time.Time{}, false},
		{"at now", Schedule{Kind: KindAt, At: now}, time.Time{}, false},
		{"every", Schedule{Kind: KindEvery, Every: 90 * time.Second}, now.Add(90 * time.Second), true},
		{"every zero", Schedule{Kind: KindEvery}, time.Time{}, false},
		// 09:00 in Tokyo (UTC+9) is 00:00 UTC the next day.
		{"cron tz", Schedule{Kind: KindCron, Expr: "0 9 * * *", TZ: "Asia/Tokyo"}, time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC), true},
		{"cron utc", Schedule{Kind: KindCron, Expr: "0 9 * * *", TZ: "UTC"}, time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC), true},
		{"cron bad tz", Schedule{Kind: KindCron, Expr: "0 9 * * *", TZ: "Mars/Olympus"}, time.Time{}, false},
		{"cron bad expr", Schedule{Kind: KindCron, Expr: "nope"}, time.Time{}, false},
		{"unknown kind", Schedule{Kind: "sometimes"}, time.Time{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.s.Next(now)
			if ok != tt.ok || !got.Equal(tt.want) {
				t.Errorf("Next = %s, %v; want %s, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestScheduleValidate(t *testing.T) {
	for _, tt := range []struct {
		s    Schedule
		want string // "" means valid
	}{
		{Schedule{Kind: KindAt, At: time.Now()}, ""},
		{Schedule{Kind: KindAt}, "requires a time"},
		{Schedule{Kind: KindEvery, Every: time.Second}, ""},
		{Schedule{Kind: KindEvery, Every: -time.Second}, "positive interval"},
		{Schedule{Kind: KindEvery, Every: time.Minute, TZ: "UTC"}, "tz can only be used with cron"},
		{Schedule{Kind: KindCron, Expr: "0 9 * * 1-5", TZ: "Europe/Berlin"}, ""},
		{Schedule{Kind: KindCron, Expr: "0 9 * * 1-5"}, ""},
		{Schedule{Kind: KindCron, Expr: " "}, "requires an expression"},
		{Schedule{Kind: KindCron, Expr: "0 9 * *"}, "want 5 fields"},
		{Schedule{Kind: KindCron, Expr: "0 9 * * *", TZ: "Nowhere/Land"}, "unknown timezone"},
		{Schedule{Kind: KindCron, Expr: "0 9 * * *", TZ: "Local"}, "unknown timezone"},
		{Schedule{Kind: "x"}, "unknown schedule kind"},
	} {
		err := tt.s.Validate()
		if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
			t.Errorf("Validate(%+v) = %v, want %q", tt.s, err, tt.want)
		}
	}
}

func TestScheduleString(t *testing.T) {
	at := time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)
	for _, tt := range []struct {
		s    Schedule
		want string
	}{
		{Schedule{Kind: KindEvery, Every: 2 * time.Hour}, "every 2h"},
		{Schedule{Kind: KindEvery, Every: 90 * time.Minute}, "every 90m"},
		{Schedule{Kind: KindEvery, Every: 45 * time.Second}, "every 45s"},
		{Schedule{Kind: KindEvery, Every: 1500 * time.Millisecond}, "every 1500ms"},
		{Schedule{Kind: KindCron, Expr: "0 9 * * *", TZ: "UTC"}, "cron: 0 9 * * * (UTC)"},
		{Schedule{Kind: KindCron, Expr: "0 9 * * *"}, "cron: 0 9 * * *"},
		{Schedule{Kind: KindAt, At: at}, "at 2026-10-20T09:30:00Z"},
	} {
		if got := tt.s.String(); got != tt.want {
			t.Errorf("String() = %q, want %q", got, tt.want)
		}
	}
}
