package trackerdash

import (
	"slices"
	"time"
)

// The viewer's logged time (the export's worklogs and worklog_days, the
// rules in store/tracker_cache_export_relations.go). What must hold:
//   - A number counted from the viewer's logged time is shown only when
//     it is what the tracker holds for the days it counts. The copy holds
//     the days from its first (MirrorExport.LoggedFrom) to today in the
//     request's time zone (the export binds both, Fill's loggedThrough),
//     the window, and says which of them were read; a day not read is not
//     zero hours.
//   - One rule decides it, in one place (num), from what the reduction's
//     rows need (cover), which ranged hands out with every row it holds:
//     a state at an instant before the first day, or rows placed in a
//     period that begins before that day's instant, reach a day the copy
//     never holds and are not known for good (Num.Old); any other rows of
//     it are not known while a day of the window is not read (Num.UR, the
//     count), since a row can count in a period other than its day's (a
//     sprint's tickets, a state that holds from its day on); a period
//     still to come, or a range with no period, takes no row and is not
//     marked. A widget one of whose numbers needed the window lists its
//     days not read (WidgetValue.Unread); a list whose rows reach a day
//     the copy never holds says so (WidgetValue.Old).
//   - An evaluation in which a number needed the window tells the reads
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
func (ev *evaluation) windowUnread() []string {
	if !ev.windowRead {
		ev.windowDays, ev.windowRead = ev.unread(ev.loggedFrom-1, ev.now, ev.now), true
	}
	return ev.windowDays
}

// cover is what a reduction's rows need of the viewer's logged time:
// nothing (a metric that does not read it, a period still to come, a
// range with no period), every day of the window, or a day before it,
// which the copy never holds (ranged.inRange decides which).
type cover uint8

const (
	coverNone cover = iota
	coverWindow
	coverOld
)

// num is r over t's rows as a number, not known by what they need of the
// viewer's logged time: the one place a widget's, a drill's or a Try's
// number is made from a ranged's rows.
func (ev *evaluation) num(r Reducer, t taken) Num {
	if t.none {
		return Num{}
	}
	n := reduceRows(r, t.rows)
	if r == ReduceAverageOfPeriodSums && t.periods > 0 && n.V != nil {
		*n.V /= float64(t.periods)
	}
	switch unread, old := ev.need(t.cover); {
	case old:
		return Num{Old: true}
	case unread > 0:
		n.V, n.UR = nil, unread
	}
	return n
}

// need is what rows of cover c lack: the window's days not read, or a
// day before it. Rows that need the window count toward the reads
// (worklogNeed) and the widget's list of days (WidgetValue.Unread).
func (ev *evaluation) need(c cover) (unread int, old bool) {
	switch c {
	case coverOld:
		return 0, true
	case coverWindow:
		ev.logged++
		return len(ev.windowUnread()), false
	}
	return 0, false
}

// worklogNeed is what o's evaluation needs of the viewer's logged time,
// nil when no number of it needed the window.
func (o *opened) worklogNeed() *WorklogNeed {
	if o.ev.logged == 0 {
		return nil
	}
	return &WorklogNeed{Unread: o.ev.windowUnread(), Today: o.now.In(o.ev.loc).Format(time.DateOnly)}
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
