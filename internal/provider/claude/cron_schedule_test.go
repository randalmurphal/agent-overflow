package claude

import (
	"testing"
	"time"
)

// The grammar is the 2.1.284 CLI's (`T`/`XB`): anything it refuses must
// not parse here, or AO would track a job the CLI never armed.
func TestParseCronScheduleMatchesTheCLIGrammar(t *testing.T) {
	for _, expr := range []string{
		"* * * * *", "*/5 * * * *", "0 9 * * 1-5", "17 3 1 1 *", "0,30 8-18/2 * * *",
		"0 0 * * 7", "0 0 * * 0-7", "  1   2 3 4 5  ",
	} {
		if _, ok := parseCronSchedule(expr); !ok {
			t.Errorf("parseCronSchedule(%q) refused an expression the CLI accepts", expr)
		}
	}
	for _, expr := range []string{
		"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *",
		"* * * * 8", "*/0 * * * *", "5-3 * * * *", "1-5/0 * * * *", "1,,2 * * * *",
		"* * * JAN *", "* * * * MON", "? * * * *", "-1 * * * *", "99999999999999999999 * * * *",
	} {
		if _, ok := parseCronSchedule(expr); ok {
			t.Errorf("parseCronSchedule(%q) accepted an expression the CLI refuses", expr)
		}
	}
}

func TestCronScheduleNextFollowsTheCLIMatchingRules(t *testing.T) {
	at := func(s string) time.Time {
		t.Helper()
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, tc := range []struct {
		expr, after, want string
	}{
		// The next whole minute, never the current one.
		{"* * * * *", "2026-10-07T10:00:00Z", "2026-10-07T10:01:00Z"},
		{"* * * * *", "2026-10-07T10:00:59Z", "2026-10-07T10:01:00Z"},
		{"*/15 * * * *", "2026-10-07T10:15:00Z", "2026-10-07T10:30:00Z"},
		// 2026-10-07 is a Wednesday; 1 = Monday.
		{"0 9 * * 1", "2026-10-07T10:00:00Z", "2026-10-12T09:00:00Z"},
		// 7 is Sunday, and 0-7 is every day (a full set).
		{"0 0 * * 7", "2026-10-07T10:00:00Z", "2026-10-11T00:00:00Z"},
		// Both day fields restricted: either one matches (Friday the 9th
		// comes before the 13th).
		{"0 0 13 * 5", "2026-10-07T10:00:00Z", "2026-10-09T00:00:00Z"},
		// A day-of-month field covering 1-31 counts as unrestricted, so the
		// weekday governs rather than ORing every day in.
		{"0 0 1-31 * 5", "2026-10-07T10:00:00Z", "2026-10-09T00:00:00Z"},
		// Month skip rolls the year.
		{"17 3 1 1 *", "2026-10-07T10:00:00Z", "2027-01-01T03:17:00Z"},
	} {
		schedule, ok := parseCronSchedule(tc.expr)
		if !ok {
			t.Fatalf("parse %q", tc.expr)
		}
		got, ok := schedule.next(at(tc.after))
		if !ok || !got.Equal(at(tc.want)) {
			t.Errorf("%q after %s = %v (%v), want %s", tc.expr, tc.after, got, ok, tc.want)
		}
	}
	// Valid grammar, no calendar date: the CLI refuses it at CronCreate,
	// and next reports no match inside the search horizon.
	never, _ := parseCronSchedule("0 0 31 2 *")
	if got, ok := never.next(at("2026-10-07T10:00:00Z")); ok {
		t.Fatalf("Feb 31 matched at %v", got)
	}
}

// fireBy must never fall before the CLI's real fire. The every-minute job
// in the 2.1.284 capture was created at 02:06:41 and fired at 13s past
// each following minute; each fire must land at or before the deadline
// computed from the previous anchor.
func TestCronFireByBoundsTheCapturedFires(t *testing.T) {
	schedule, _ := parseCronSchedule("* * * * *")
	anchor := time.Date(2026, 10, 11, 2, 6, 41, 448e6, time.UTC)
	for _, fired := range []time.Time{
		time.Date(2026, 10, 11, 2, 7, 13, 207e6, time.UTC),
		time.Date(2026, 10, 11, 2, 8, 13, 214e6, time.UTC),
		time.Date(2026, 10, 11, 2, 9, 13, 228e6, time.UTC),
	} {
		by, ok := schedule.fireBy(anchor, true)
		if !ok || fired.After(by) {
			t.Fatalf("fire at %v is after the deadline %v computed from %v", fired, by, anchor)
		}
		anchor = fired
	}
}

// The deadline is the next match plus the largest delay any permitted CLI
// scheduler config can add: the gap to the following match, capped at 30
// minutes. A one-shot fires at its match or earlier.
func TestCronFireByUsesTheLargestPermittedDelay(t *testing.T) {
	anchor := time.Date(2026, 10, 7, 10, 0, 30, 0, time.UTC)
	for _, tc := range []struct {
		expr      string
		recurring bool
		want      time.Time
	}{
		{"* * * * *", true, time.Date(2026, 10, 7, 10, 2, 0, 0, time.UTC)},
		{"*/5 * * * *", true, time.Date(2026, 10, 7, 10, 10, 0, 0, time.UTC)},
		{"0 * * * *", true, time.Date(2026, 10, 7, 11, 30, 0, 0, time.UTC)},
		{"0 9 * * *", true, time.Date(2026, 10, 8, 9, 30, 0, 0, time.UTC)},
		{"0 9 * * *", false, time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)},
	} {
		schedule, _ := parseCronSchedule(tc.expr)
		got, ok := schedule.fireBy(anchor, tc.recurring)
		if !ok || !got.Equal(tc.want) {
			t.Errorf("fireBy(%q, recurring=%v) = %v, want %v", tc.expr, tc.recurring, got, tc.want)
		}
	}
}
