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

// The CLI matches in local wall-clock time, where JavaScript's Date carries
// a time inside a spring-forward gap past it and a fall-back hour happens
// twice. The search moves forward across both: a 03:00 job is found the
// morning the clocks skip 02:00, a job in the skipped hour waits for the
// next day, as the CLI's walk lands on 03:00 and moves on, and the repeated
// hour matches once, at its first occurrence.
func TestCronScheduleNextCrossesDaylightSavingChanges(t *testing.T) {
	zone, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database:", err)
	}
	local := func(year int, month time.Month, day, hour, minute int) time.Time {
		return time.Date(year, month, day, hour, minute, 0, 0, zone)
	}
	for _, tc := range []struct {
		expr        string
		after, want time.Time
	}{
		// Spring forward, 2026-03-08 02:00 EST -> 03:00 EDT.
		{"0 3 * * *", local(2026, 3, 7, 12, 0), local(2026, 3, 8, 3, 0)},
		{"0 2 * * *", local(2026, 3, 7, 12, 0), local(2026, 3, 9, 2, 0)},
		{"30 2 * * *", local(2026, 3, 7, 12, 0), local(2026, 3, 9, 2, 30)},
		{"*/20 * * * *", local(2026, 3, 8, 1, 50), local(2026, 3, 8, 3, 0)},
		{"0 4 * * *", local(2026, 3, 8, 1, 59), local(2026, 3, 8, 4, 0)},
		// Fall back, 2026-11-01 02:00 EDT -> 01:00 EST.
		{"0 3 * * *", local(2026, 10, 31, 12, 0), local(2026, 11, 1, 3, 0)},
		{"30 1 * * *", time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC).In(zone), time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)},
		{"30 1 * * *", time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC).In(zone), local(2026, 11, 2, 1, 30)},
	} {
		schedule, ok := parseCronSchedule(tc.expr)
		if !ok {
			t.Fatalf("parse %q", tc.expr)
		}
		got, ok := schedule.next(tc.after)
		if !ok || !got.Equal(tc.want) {
			t.Errorf("%q after %s = %v (%v), want %s", tc.expr, tc.after, got, ok, tc.want)
		}
	}
}

// A match and a deadline are strictly after their anchor for every anchor,
// one inside the second occurrence of a fall-back hour included, where the
// wall clock alone names an earlier instant. Anchors run every 50 seconds
// through three hours either side of each 2026 offset change of zones that
// shift by an hour at different wall times and by 30 minutes.
func TestCronScheduleNextIsAfterEveryAnchorAcrossOffsetChanges(t *testing.T) {
	t.Parallel()
	exprs := []string{"* * * * *", "*/20 * * * *", "0 * * * *", "30 * * * *", "45 1 * * *", "15 2 * * *", "0 3 * * *"}
	for _, name := range []string{"Europe/Berlin", "America/New_York", "Australia/Lord_Howe"} {
		zone, err := time.LoadLocation(name)
		if err != nil {
			t.Skip("no tz database:", err)
		}
		var changes []time.Time
		start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		for at := start; at.Year() == 2026; at = at.Add(time.Hour) {
			if _, before := at.In(zone).Zone(); func() bool { _, after := at.Add(time.Hour).In(zone).Zone(); return after != before }() {
				changes = append(changes, at.Add(time.Hour))
			}
		}
		if len(changes) != 2 {
			t.Fatalf("%s: offset changes %v, want two in 2026", name, changes)
		}
		for _, expr := range exprs {
			schedule, ok := parseCronSchedule(expr)
			if !ok {
				t.Fatalf("parse %q", expr)
			}
			for _, change := range changes {
				for at := change.Add(-3 * time.Hour); at.Before(change.Add(3 * time.Hour)); at = at.Add(50 * time.Second) {
					anchor := at.In(zone)
					match, ok := schedule.next(anchor)
					if !ok || !match.After(anchor) || match.Sub(anchor) > 48*time.Hour {
						t.Fatalf("%s %q after %s (%s) = %s (%s), %v", name, expr, anchor, anchor.UTC(), match, match.UTC(), ok)
					}
					for _, recurring := range []bool{false, true} {
						if fireBy, ok := schedule.fireBy(anchor, recurring); !ok || fireBy.Before(match) {
							t.Fatalf("%s %q after %s: fire by %s (%v) before the match %s", name, expr, anchor, fireBy, ok, match)
						}
					}
				}
			}
		}
	}
}

// From inside the second occurrence of a repeated hour the search keeps the
// anchor's instant: the next minute, and the next match of the hour, are
// those after it, not their first occurrence an hour earlier.
func TestCronScheduleNextFromTheRepeatedHour(t *testing.T) {
	t.Parallel()
	zone, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no tz database:", err)
	}
	// 2026-10-25 03:00 CEST -> 02:00 CET; 01:10Z is the second 02:10.
	anchor := time.Date(2026, 10, 25, 1, 10, 0, 0, time.UTC).In(zone)
	for _, tc := range []struct {
		expr string
		want time.Time
	}{
		{"* * * * *", time.Date(2026, 10, 25, 1, 11, 0, 0, time.UTC)},
		{"30 * * * *", time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC)},
		{"0 * * * *", time.Date(2026, 10, 25, 2, 0, 0, 0, time.UTC)},
	} {
		schedule, _ := parseCronSchedule(tc.expr)
		if got, ok := schedule.next(anchor); !ok || !got.Equal(tc.want) {
			t.Errorf("%q after %s = %s (%v), want %s", tc.expr, anchor, got.UTC(), ok, tc.want)
		}
	}
	every, _ := parseCronSchedule("* * * * *")
	if fireBy, ok := every.fireBy(anchor, true); !ok || !fireBy.Equal(time.Date(2026, 10, 25, 1, 12, 0, 0, time.UTC)) {
		t.Errorf("recurring fire by %s (%v), want 01:12Z", fireBy.UTC(), ok)
	}
}
