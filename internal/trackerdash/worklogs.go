package trackerdash

import (
	"slices"
	"time"
)

// The viewer's logged time (the export's worklogs and worklog_days, the
// rules in store/tracker_cache_export_relations.go). The copy holds the
// worklogs of the days the mirror read and says which days those are;
// a day not read is not zero hours. What must hold:
//   - A metric that reads worklogs counts only where its days were read:
//     each of its periods, and its number over the range, with a working
//     day (Monday to Friday in the request's zone) not read is not known,
//     saying how many days were not read (Num.UR), and its widget lists
//     the range's days not read (WidgetValue.Unread) for the page to read
//     (frontend trackerDashboard). A day is in the period that holds its
//     dayStart, the instant day_start places a row of it at.
//   - Its rows are the viewer's: a row's person is the viewer unless the
//     metric names one, so Mine keeps them all, and a page narrowed to
//     another person says only the viewer's time is read rather than 0.
//   - A board with no time tracking (Linear), or a connection whose
//     account is not verified yet, says so in the widget's place.

// The sentences a metric that reads worklogs shows in its widget's place.
const (
	noTimeTracking   = "Linear has no time tracking, so this board has no logged time."
	viewerUnverified = "Your logged time needs this connection's account, which is not verified yet; check the connection in the Issues settings."
	onlyYourTime     = "Only your own logged time is read."
)

// readsWorklogs is whether p reads the viewer's logged time.
func (p prepared) readsWorklogs() bool { return slices.Contains(p.asks.tables, "worklogs") }

// worklogNote is why metric p cannot count on this page, "" when it can.
func (o *opened) worklogNote(p prepared) string {
	switch {
	case !p.readsWorklogs():
		return ""
	case !o.timeTracking:
		return noTimeTracking
	case o.ev.viewer == "":
		return viewerUnverified
	case o.n.person != nil && o.n.person.ID != o.ev.viewer:
		return onlyYourTime
	}
	return ""
}

// unread is the working days whose dayStart is after from and at or
// before to and now that the copy holds no read of, in order.
func (ev *evaluation) unread(from, to, now int64) []string {
	to = min(to, now)
	var out []string
	t := time.UnixMilli(from).In(ev.loc)
	for d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, ev.loc); ; d = d.AddDate(0, 0, 1) {
		at := d.UnixMilli() + 1
		if at > to {
			return out
		}
		day := d.Format(time.DateOnly)
		if at > from && d.Weekday() != time.Saturday && d.Weekday() != time.Sunday && !ev.readDays[day] {
			out = append(out, day)
		}
	}
}

// unreadOf is the days not read of periods ps, the ones still to come
// left out, in order.
func (ev *evaluation) unreadOf(ps []period, now int64) []string {
	var out []string
	for _, p := range ps {
		if !p.future {
			out = append(out, ev.unread(p.from, p.to, now)...)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// rangeUnread is the days not read that metric m's number over the range
// counts: its periods' (g), or for a metric that counts now every day the
// copy can hold.
func (ev *evaluation) rangeUnread(g *ranged, now int64) []string {
	if g.periods == nil {
		return ev.unread(ev.export.Earliest, now, now)
	}
	return ev.unreadOf(g.periods, now)
}

// markUnread makes n not known where days of it were not read.
func markUnread(n *Num, days int) {
	if n != nil && days > 0 {
		n.V, n.UR = nil, days
	}
}

// markWidget marks widget value out of metric m, its rows sorted into
// the range as g, where its days were not read, and lists the range's
// days not read.
func (ev *evaluation) markWidget(out *WidgetValue, w Widget, m Metric, g *ranged, sp span) {
	days := ev.rangeUnread(g, sp.now)
	out.Unread = days
	markUnread(out.Value, len(days))
	if out.Previous != nil && !out.Previous.Gap {
		markUnread(out.Previous, len(ev.unreadOf(sp.previous, sp.now)))
	}
	for i := range out.Trend {
		markUnread(&out.Trend[i], len(ev.unreadOf(g.periods[i:i+1], sp.now)))
	}
	for i := range out.Buckets {
		n := len(days)
		if !ranks(w, m) {
			n = len(ev.unreadOf(g.periods[i:i+1], sp.now))
		}
		b := &out.Buckets[i]
		markUnread(&b.Total, n)
		for j := range b.Cells {
			markUnread(&b.Cells[j], n)
		}
	}
}
