package tracker

import (
	"encoding/json"
	"testing"
	"time"
)

func TestNewDateRefusesDaysTheCalendarLacks(t *testing.T) {
	for _, d := range []struct {
		y int
		m time.Month
		d int
	}{{2026, 2, 29}, {2026, 4, 31}, {2026, 13, 1}, {2026, 0, 1}, {2026, 1, 0}, {0, 1, 1}, {10000, 1, 1}} {
		if got, err := NewDate(d.y, d.m, d.d); err == nil {
			t.Errorf("NewDate(%d, %d, %d) = %v, want an error", d.y, d.m, d.d, got)
		}
	}
	got, err := NewDate(2028, 2, 29)
	if err != nil || got.String() != "2028-02-29" {
		t.Fatalf("NewDate(2028, 2, 29) = %v, %v", got, err)
	}
}

func TestDateCompareOrdersNoDateFirst(t *testing.T) {
	first, _ := NewDate(1, 1, 1)
	later, _ := ParseDate("2026-11-16")
	nextMonth, _ := ParseDate("2026-12-01")
	nextDay, _ := ParseDate("2026-11-17")
	cases := []struct {
		a, b Date
		want int
	}{
		{Date{}, Date{}, 0}, {Date{}, first, -1}, {first, Date{}, 1},
		{first, later, -1}, {later, first, 1}, {later, later, 0},
		{nextMonth, later, 1}, {later, nextMonth, -1}, {nextDay, later, 1}, {nextMonth, nextDay, 1},
	}
	for _, c := range cases {
		if got := c.a.Compare(c.b); got != c.want {
			t.Errorf("%q.Compare(%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestDateTextRoundTrips(t *testing.T) {
	d, err := DateOf(time.Date(2026, 11, 16, 23, 30, 0, 0, time.FixedZone("x", -5*3600)))
	if err != nil || d.String() != "2026-11-16" {
		t.Fatalf("DateOf = %v, %v; want the day in the time's own zone", d, err)
	}
	b, err := json.Marshal([]Date{d, {}})
	if err != nil || string(b) != `["2026-11-16",""]` {
		t.Fatalf("Marshal = %s, %v", b, err)
	}
	var back []Date
	if err := json.Unmarshal(b, &back); err != nil || back[0] != d || !back[1].IsZero() {
		t.Fatalf("Unmarshal = %v, %v", back, err)
	}
	if err := json.Unmarshal([]byte(`["2026-02-30"]`), &back); err == nil {
		t.Fatal("a day the calendar lacks was read")
	}
}
