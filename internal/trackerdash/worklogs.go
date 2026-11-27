package trackerdash

import (
	"slices"
	"time"
)

// The viewer's logged time (the export's worklogs and worklog_days, the
// rules in store/tracker_cache_export_relations.go). The copy holds the
// worklogs of the days the mirror read from its first day
// (MirrorExport.LoggedFrom), and says which days those are; a day not
// read is not zero hours. What must hold:
//   - The days a metric's query can see are the days the copy checks: from
//     LoggedFrom up to today, the window. A number a reduction took rows
//     of logged time for (ranged.took) is known only once every day of the
//     window up to now is read: a row can count in a period other than its
//     day's (a sprint's tickets, a state that holds from its day on), so no
//     number is known while a day it could hold is not read. Each counts
//     the window's days not read, with those of its periods before
//     LoggedFrom, which the copy never holds (a sprint that began before
//     it), in Num.UR, and is not known while it counts any. The widget
//     lists the window's days not read (WidgetValue.Unread). A reduction
//     that took no rows (a metric counted by period over a range with no
//     period) marks and lists nothing. A day is in the period that holds
//     its placed instant, the one day_start places a row of it at. A
//     weekend's day counts like any other.
//   - An evaluation in which a widget counted logged time tells the reads
//     what it needs (Board.Worklogs) and answers their state
//     (Answer.Logged); the reads are the caller's, so the evaluation does
//     no I/O for them.
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

// WorklogNeed is what an evaluation whose widgets counted the viewer's
// logged time needs of it: the window's days not read, in order, and
// Today, the window's last day in the request's time zone (YYYY-MM-DD).
type WorklogNeed struct {
	Unread []string
	Today  string
}

// WorklogState is the state of the reads of the viewer's logged time, as
// Board.Worklogs answers it: whether they are reading, and why the last
// one failed, "" when none did.
type WorklogState struct {
	Reading bool   `json:"reading,omitempty"`
	Failure string `json:"failure,omitempty"`
}

// readsWorklogs is whether p reads the viewer's logged time.
func (p prepared) readsWorklogs() bool { return slices.Contains(p.asks.tables, "worklogs") }

// worklogNote is why metric p cannot count on this page, "" when it can.
func (o *opened) worklogNote(p prepared) string {
	switch {
	case !p.readsWorklogs():
		return ""
	case !o.timeTracking:
		return noTimeTracking
	case o.ev.export.Viewer == "":
		return viewerUnverified
	case o.n.person != nil && o.n.person.ID != o.ev.export.Viewer:
		return onlyYourTime
	}
	return ""
}

// unread is the days whose placed instant is after from and at or
// before to and now that the copy holds no read of, in order. The days
// are civil dates, counted apart from the instants a zone change moves.
func (ev *evaluation) unread(from, to, now int64) []string {
	to = min(to, now)
	var out []string
	y, m, d := time.UnixMilli(from).In(ev.loc).Date()
	for day := time.Date(y, m, d, 0, 0, 0, 0, time.UTC); ; day = day.AddDate(0, 0, 1) {
		at := placed(day, ev.loc)
		if at > to {
			return out
		}
		date := day.Format(time.DateOnly)
		if at > from && !ev.readDays[date] {
			out = append(out, date)
		}
	}
}

// windowUnread is the days of the window up to now not read, read once
// an evaluation.
func (ev *evaluation) windowUnread(now int64) []string {
	if !ev.windowRead {
		ev.windowDays, ev.windowRead = ev.unread(ev.loggedFrom-1, now, now), true
	}
	return ev.windowDays
}

// notRead is the days a number over periods ps needs read and are not:
// the window's, with those of ps before it.
func (ev *evaluation) notRead(ps []period, now int64) int {
	days := slices.Clone(ev.windowUnread(now))
	for _, p := range ps {
		days = append(days, ev.unread(p.from, min(p.to, ev.loggedFrom-1), now)...)
	}
	slices.Sort(days)
	return len(slices.Compact(days))
}

// markUnread makes n not known where days of it were not read.
func markUnread(n *Num, days int) {
	if n != nil && days > 0 {
		n.V, n.UR = nil, days
	}
}

// markWidget marks widget value out of metric m, its rows sorted into
// the range as g, not known while days it needs are not read, and lists
// the window's days not read, when its reduction took rows.
func (ev *evaluation) markWidget(out *WidgetValue, w Widget, m Metric, g *ranged, sp span) {
	if !g.took {
		return
	}
	ev.logged = true
	out.Unread = ev.windowUnread(sp.now)
	whole := ev.notRead(g.periods, sp.now)
	markUnread(out.Value, whole)
	if out.Previous != nil && !out.Previous.Gap {
		markUnread(out.Previous, ev.notRead(sp.previous, sp.now))
	}
	for i := range out.Trend {
		markUnread(&out.Trend[i], ev.notRead(g.periods[i:i+1], sp.now))
	}
	for i := range out.Buckets {
		n := whole
		if !ranks(w, m) {
			n = ev.notRead(g.periods[i:i+1], sp.now)
		}
		b := &out.Buckets[i]
		markUnread(&b.Total, n)
		for j := range b.Cells {
			markUnread(&b.Cells[j], n)
		}
	}
}

// worklogNeed is what o's evaluation needs of the viewer's logged time,
// nil when no widget of it counted it.
func (o *opened) worklogNeed() *WorklogNeed {
	if !o.ev.logged {
		return nil
	}
	now := o.now.UnixMilli()
	return &WorklogNeed{Unread: o.ev.windowUnread(now), Today: o.now.In(o.ev.loc).Format(time.DateOnly)}
}

// tellWorklogs tells b's reads need, and returns their state: nil for no
// need or a board with no reads.
func tellWorklogs(b Board, need *WorklogNeed) *WorklogState {
	if need == nil || b.Worklogs == nil {
		return nil
	}
	st := b.Worklogs(*need)
	return &st
}
