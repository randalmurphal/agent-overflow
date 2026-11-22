package tracker

import (
	"cmp"
	"fmt"
	"time"
)

// Date is a calendar day with no time or zone: a date field's value, a
// worklog's day. The zero Date is no date; every other Date is a real day
// in years 1 to 9999, since NewDate, ParseDate and UnmarshalText are the
// only ways to make one.
type Date struct {
	year  int
	month time.Month
	day   int
}

// NewDate is the day year-month-day. It refuses a day the calendar does
// not have, such as February 30, and a year outside 1 to 9999.
func NewDate(year int, month time.Month, day int) (Date, error) {
	t := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	if year < 1 || year > 9999 || t.Year() != year || t.Month() != month || t.Day() != day {
		return Date{}, fmt.Errorf("tracker: %04d-%02d-%02d is not a day", year, int(month), day)
	}
	return Date{year: year, month: month, day: day}, nil
}

// DateOf is the calendar day t falls on in t's own location.
func DateOf(t time.Time) (Date, error) {
	return NewDate(t.Year(), t.Month(), t.Day())
}

// ParseDate reads YYYY-MM-DD.
func ParseDate(s string) (Date, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return Date{}, fmt.Errorf("tracker: date %q: %w", s, err)
	}
	return NewDate(t.Year(), t.Month(), t.Day())
}

// IsZero reports no date.
func (d Date) IsZero() bool { return d == Date{} }

// Year, Month and Day are d's parts; zero for no date.
func (d Date) Year() int         { return d.year }
func (d Date) Month() time.Month { return d.month }
func (d Date) Day() int          { return d.day }

// Compare is -1 when d is before e, 0 when they are the same day and +1
// when d is after e. No date is before every date.
func (d Date) Compare(e Date) int {
	return cmp.Or(cmp.Compare(d.year, e.year), cmp.Compare(d.month, e.month), cmp.Compare(d.day, e.day))
}

// String is YYYY-MM-DD, or "" for no date.
func (d Date) String() string {
	if d.IsZero() {
		return ""
	}
	return fmt.Sprintf("%04d-%02d-%02d", d.year, int(d.month), d.day)
}

// MarshalText writes String: YYYY-MM-DD, or empty for no date.
func (d Date) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// UnmarshalText reads YYYY-MM-DD, or empty as no date.
func (d *Date) UnmarshalText(b []byte) error {
	if len(b) == 0 {
		*d = Date{}
		return nil
	}
	v, err := ParseDate(string(b))
	if err != nil {
		return err
	}
	*d = v
	return nil
}
