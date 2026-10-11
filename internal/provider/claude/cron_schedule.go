package claude

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// cronSchedule is a CronCreate expression parsed with the CLI's own
// grammar and matched with its own rules (claude-wire.md §E11). Both are
// read from the 2.1.284 bundle (functions `T`, `XB` and `sct`), not from
// a cron library: the CLI's day-of-month/day-of-week rule keys on whether
// a field's SET covers its whole range, which differs from the common
// "field was written as *" reading, and a deadline computed by the other
// rule could fall before the CLI's real fire.
//
// Each field is a bitset of the values it allows. The grammar: five
// whitespace-separated fields (minute 0-59, hour 0-23, day of month 1-31,
// month 1-12, day of week 0-6 with 7 meaning 0), each a comma list of
// `*`, `*/n`, `a-b`, `a-b/n` or `n`. No names, no `?`, `L` or `#`.
type cronSchedule struct {
	minute, hour, dayOfMonth, month, dayOfWeek uint64
	// The CLI tests fullness by set size (31 days, 7 weekdays), so
	// `1-31` and `*` are the same answer.
	dayOfMonthFull, dayOfWeekFull bool
}

type cronFieldRange struct{ min, max int }

var cronFieldRanges = [5]cronFieldRange{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}

var (
	cronStarPattern  = regexp.MustCompile(`^\*(?:/(\d+))?$`)
	cronRangePattern = regexp.MustCompile(`^(\d+)-(\d+)(?:/(\d+))?$`)
	cronValuePattern = regexp.MustCompile(`^\d+$`)
)

// cronSearchLimit is the CLI's iteration cap for one next-match search.
// Each step advances at least one minute, so the search horizon is at
// least 366 days; an expression with no match inside it (`0 0 31 2 *`)
// has none for the CLI either, which refuses it at CronCreate.
const cronSearchLimit = 527040

// parseCronSchedule parses expr, reporting false for anything the CLI
// would reject.
func parseCronSchedule(expr string) (cronSchedule, bool) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return cronSchedule{}, false
	}
	var sets [5]uint64
	for i, field := range fields {
		set, ok := parseCronField(field, cronFieldRanges[i])
		if !ok {
			return cronSchedule{}, false
		}
		sets[i] = set
	}
	return cronSchedule{
		minute:         sets[0],
		hour:           sets[1],
		dayOfMonth:     sets[2],
		month:          sets[3],
		dayOfWeek:      sets[4],
		dayOfMonthFull: bitCount(sets[2]) == 31,
		dayOfWeekFull:  bitCount(sets[4]) == 7,
	}, true
}

func parseCronField(field string, bounds cronFieldRange) (uint64, bool) {
	weekday := bounds == cronFieldRanges[4]
	var set uint64
	for _, part := range strings.Split(field, ",") {
		if m := cronStarPattern.FindStringSubmatch(part); m != nil {
			step := 1
			if m[1] != "" {
				var ok bool
				if step, ok = cronNumber(m[1]); !ok || step < 1 {
					return 0, false
				}
			}
			for v := bounds.min; v <= bounds.max; v += step {
				set |= 1 << v
			}
			continue
		}
		if m := cronRangePattern.FindStringSubmatch(part); m != nil {
			lo, okLo := cronNumber(m[1])
			hi, okHi := cronNumber(m[2])
			step := 1
			okStep := true
			if m[3] != "" {
				step, okStep = cronNumber(m[3])
			}
			// A weekday range may end at 7, which is Sunday again.
			limit := bounds.max
			if weekday {
				limit = 7
			}
			if !okLo || !okHi || !okStep || lo > hi || step < 1 || lo < bounds.min || hi > limit {
				return 0, false
			}
			for v := lo; v <= hi; v += step {
				bit := v
				if weekday && v == 7 {
					bit = 0
				}
				set |= 1 << bit
			}
			continue
		}
		if cronValuePattern.MatchString(part) {
			v, ok := cronNumber(part)
			if weekday && v == 7 {
				v = 0
			}
			if !ok || v < bounds.min || v > bounds.max {
				return 0, false
			}
			set |= 1 << v
			continue
		}
		return 0, false
	}
	return set, set != 0
}

// cronNumber reads a field number. Anything too large for an int is
// out of every field's range, so it reports false rather than wrapping.
func cronNumber(s string) (int, bool) {
	v, err := strconv.Atoi(s)
	return v, err == nil
}

func bitCount(set uint64) int {
	n := 0
	for ; set != 0; set &= set - 1 {
		n++
	}
	return n
}

func (s cronSchedule) has(set uint64, v int) bool { return set&(1<<v) != 0 }

// next returns the first matching minute strictly after `after`, in
// after's location (the CLI matches in local time). The walk is the
// CLI's: start at the next whole minute, then skip a whole month, day or
// hour whenever that field cannot match.
func (s cronSchedule) next(after time.Time) (time.Time, bool) {
	t := after
	t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute()+1, 0, 0, t.Location())
	for range cronSearchLimit {
		if !s.has(s.month, int(t.Month())) {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, t.Location())
			continue
		}
		if !s.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
			continue
		}
		if !s.has(s.hour, t.Hour()) {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, t.Location())
			continue
		}
		if !s.has(s.minute, t.Minute()) {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute()+1, 0, 0, t.Location())
			continue
		}
		return t, true
	}
	return time.Time{}, false
}

// dayMatches is the CLI's day rule: with both day fields restricted a
// day matching EITHER one counts; a full field defers to the other.
func (s cronSchedule) dayMatches(t time.Time) bool {
	dom := s.has(s.dayOfMonth, t.Day())
	dow := s.has(s.dayOfWeek, int(t.Weekday()))
	switch {
	case s.dayOfMonthFull && s.dayOfWeekFull:
		return true
	case s.dayOfMonthFull:
		return dow
	case s.dayOfWeekFull:
		return dom
	default:
		return dom || dow
	}
}

// cronRecurringCapLimit is the largest recurring fire delay the CLI's
// scheduler config accepts (`recurringCapMs` max, 2.1.284). The default
// is the same 30 minutes.
const cronRecurringCapLimit = 30 * time.Minute

// fireBy is the latest moment a job anchored at `anchor` (its creation,
// then each fire) can next fire. The CLI fires a recurring job at its
// next match i plus a per-job delay of frac(id) x recurringFrac x o
// capped at recurringCapMs, where o is the gap to the match after i, or
// for a `*/n` minute job up to cacheLeadMs EARLY. Its scheduler config
// can be replaced at runtime (`tengu_kairos_cron_config`) within a schema
// that caps recurringFrac at 1 and recurringCapMs at 30 minutes, so
// i + min(o, 30m) bounds every permitted config. A one-shot fires at its
// match or earlier, so the match bounds it.
func (s cronSchedule) fireBy(anchor time.Time, recurring bool) (time.Time, bool) {
	match, ok := s.next(anchor)
	if !ok {
		return time.Time{}, false
	}
	if !recurring {
		return match, true
	}
	following, ok := s.next(match)
	if !ok {
		return match, true
	}
	return match.Add(min(following.Sub(match), cronRecurringCapLimit)), true
}
