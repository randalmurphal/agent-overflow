package tempo

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// dateLayout is how Tempo writes a calendar date.
const dateLayout = "2006-01-02"

// Date is a calendar day with no time zone, as Tempo stores a worklog's
// start date, a schedule day and an approval period's ends. The zero Date
// is no date.
type Date struct {
	// days counts days from 0001-01-01, plus one, so zero is no date.
	days int64
}

// epochDays is the day count of 1970-01-01 from 0001-01-01.
const epochDays = 719162

// NewDate is the day year-month-day. It refuses a day the calendar does
// not have, such as February 30.
func NewDate(year int, month time.Month, day int) (Date, error) {
	t := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	if t.Year() != year || t.Month() != month || t.Day() != day || year < 1 || year > 9999 {
		return Date{}, fmt.Errorf("tempo: %04d-%02d-%02d is not a calendar date", year, int(month), day)
	}
	return dateOfUTC(t), nil
}

// DateOf is the calendar day t falls on in t's own location.
func DateOf(t time.Time) Date {
	y, m, d := t.Date()
	return dateOfUTC(time.Date(y, m, d, 0, 0, 0, 0, time.UTC))
}

func dateOfUTC(t time.Time) Date {
	return Date{days: t.Unix()/86400 + epochDays + 1}
}

// ParseDate reads YYYY-MM-DD.
func ParseDate(s string) (Date, error) {
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		return Date{}, fmt.Errorf("tempo: %q is not a YYYY-MM-DD date", s)
	}
	return dateOfUTC(t), nil
}

// IsZero reports the zero Date, which is no date.
func (d Date) IsZero() bool { return d.days == 0 }

// utc is d at midnight UTC.
func (d Date) utc() time.Time {
	return time.Unix((d.days-1-epochDays)*86400, 0).UTC()
}

// Year, Month and Day are d's parts; zero for the zero Date.
func (d Date) Year() int {
	if d.IsZero() {
		return 0
	}
	return d.utc().Year()
}

func (d Date) Month() time.Month {
	if d.IsZero() {
		return 0
	}
	return d.utc().Month()
}

func (d Date) Day() int {
	if d.IsZero() {
		return 0
	}
	return d.utc().Day()
}

// Weekday is d's day of the week.
func (d Date) Weekday() time.Weekday { return d.utc().Weekday() }

// AddDays is the day n days after d, or before it for a negative n.
// The zero Date stays zero.
func (d Date) AddDays(n int) Date {
	if d.IsZero() {
		return d
	}
	return Date{days: d.days + int64(n)}
}

// Compare is -1 when d is before e, 0 when they are the same day and +1
// when d is after e. The zero Date is before every date.
func (d Date) Compare(e Date) int {
	switch {
	case d.days < e.days:
		return -1
	case d.days > e.days:
		return 1
	}
	return 0
}

// Before and After compare d with e as Compare does.
func (d Date) Before(e Date) bool { return d.days < e.days }
func (d Date) After(e Date) bool  { return d.days > e.days }

// In is midnight of d in loc.
func (d Date) In(loc *time.Location) time.Time {
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
}

// String is YYYY-MM-DD, or "" for the zero Date.
func (d Date) String() string {
	if d.IsZero() {
		return ""
	}
	return d.utc().Format(dateLayout)
}

// MarshalText writes String; the zero Date is refused.
func (d Date) MarshalText() ([]byte, error) {
	if d.IsZero() {
		return nil, fmt.Errorf("tempo: the zero Date has no text form")
	}
	return []byte(d.String()), nil
}

// UnmarshalText reads YYYY-MM-DD.
func (d *Date) UnmarshalText(b []byte) error {
	parsed, err := ParseDate(string(b))
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// TimeOfDay is a wall-clock time within a day, to the second. The zero
// TimeOfDay is no time, which is distinct from midnight.
type TimeOfDay struct {
	// sec is seconds after midnight, plus one, so zero is no time.
	sec int32
}

// Clock is hour:minute:second. It refuses a value outside 00:00:00 to
// 23:59:59.
func Clock(hour, minute, second int) (TimeOfDay, error) {
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 || second < 0 || second > 59 {
		return TimeOfDay{}, fmt.Errorf("tempo: %02d:%02d:%02d is not a time of day", hour, minute, second)
	}
	return TimeOfDay{sec: int32(hour*3600+minute*60+second) + 1}, nil
}

// ParseTimeOfDay reads HH:MM:SS or HH:MM.
func ParseTimeOfDay(s string) (TimeOfDay, error) {
	parts := strings.Split(s, ":")
	bad := fmt.Errorf("tempo: %q is not an HH:MM:SS time of day", s)
	if len(parts) < 2 || len(parts) > 3 {
		return TimeOfDay{}, bad
	}
	var n [3]int
	for i, p := range parts {
		if len(p) != 2 && (i != 0 || len(p) != 1) {
			return TimeOfDay{}, bad
		}
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 {
			return TimeOfDay{}, bad
		}
		n[i] = v
	}
	t, err := Clock(n[0], n[1], n[2])
	if err != nil {
		return TimeOfDay{}, bad
	}
	return t, nil
}

// IsSet reports whether t names a time.
func (t TimeOfDay) IsSet() bool { return t.sec != 0 }

// SinceMidnight is t as a duration after midnight; zero when t is not
// set.
func (t TimeOfDay) SinceMidnight() time.Duration {
	if !t.IsSet() {
		return 0
	}
	return time.Duration(t.sec-1) * time.Second
}

// String is HH:MM:SS, or "" when t is not set.
func (t TimeOfDay) String() string {
	if !t.IsSet() {
		return ""
	}
	s := int(t.sec - 1)
	return fmt.Sprintf("%02d:%02d:%02d", s/3600, s%3600/60, s%60)
}

// Start is when a worklog begins, as Tempo stores it: a calendar date and
// an optional time of day on the author's own wall clock, with no time
// zone. The client sends and returns both exactly as given and never
// converts them between zones; a caller that has an instant converts it
// in the author's zone first (DateOf of the time in that zone, and Clock
// of its hour, minute and second). Worklog.StartUTC is Tempo's own
// reading of the same moment and is never sent.
type Start struct {
	Date Date
	// Time is the start time of day; unset leaves it to Tempo.
	Time TimeOfDay
}

// seconds is d in Tempo's unit, whole seconds, rounded to the nearest
// second. It is the one conversion from a duration to Tempo's seconds.
func seconds(d time.Duration) int64 {
	return int64(d.Round(time.Second) / time.Second)
}

// duration is Tempo's seconds as a duration.
func duration(sec int64) time.Duration {
	return time.Duration(sec) * time.Second
}
