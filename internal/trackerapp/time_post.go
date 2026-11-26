package trackerapp

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// PostTimeRequest is the person's approval of time entries, each as they
// read it, each named once. Alone is one entry logged by itself;
// otherwise the request approves a day or the whole draft, and an entry
// whose ticket and day got time logged since it was drafted is skipped
// (loggedSince).
type PostTimeRequest struct {
	Entries []TimeEntryRefView `json:"entries"`
	Alone   bool               `json:"alone,omitempty"`
}

// PostTimeView is what a post did: each entry named as it is now, how
// many it posted of those it could, and the sentence that says it. Stale
// is the read-back of the posted days that failed, as a write's
// (WriteView.Stale): those days do not show the posted time until their
// next read.
type PostTimeView struct {
	Entries  []TimeEntryView `json:"entries"`
	Posted   int             `json:"posted"`
	Of       int             `json:"of"`
	Sentence string          `json:"sentence"`
	Stale    []StaleView     `json:"stale,omitempty"`
}

// A post's notes, one per entry it did not post, in the sentence.
type postNote struct {
	e    store.TimeEntry
	why  string
	open bool // a question left open
}

// mayHaveBeenLogged is what the app says of an unknown entry, whose post
// may have happened: the app never sends it again, since no read it can
// make proves the first send did not land. Only the person settles it.
const mayHaveBeenLogged = "may already be logged, so it is not sent again; look at the day's logged time, and remove the entry if the time is there, or log the time by hand and then remove the entry"

// skippedSince is what a day's approval says of a drafted entry whose
// ticket got time logged that day since it was drafted.
const skippedSince = "was not posted: time was logged on it that day since it was drafted; log it alone to post it"

// loggedSince reports whether logs, the worklogs of entry e's day, hold
// one of its ticket created since e was drafted, less clockSlack. A
// worklog whose creation time the tracker did not give counts: the check
// errs toward a skip the person sees over time logged twice.
func loggedSince(e store.TimeEntry, logs []tracker.Worklog) bool {
	for _, w := range logs {
		if string(w.Issue) == e.IssueID && w.Day.String() == e.Day && (w.Created.IsZero() || w.Created.UnixMilli() >= e.CreatedMs-clockSlack) {
			return true
		}
	}
	return false
}

// clockSlack is how far the tracker's clock, which stamps a worklog's
// creation, may be behind this computer's, which stamps an entry, in Unix
// milliseconds. Tempo stamps whole seconds.
const clockSlack = 60_000

// PostTimeEntries is the one path that sends a draft's time: the
// person's approval of in.Entries on Jira connection id. It keeps these
// true:
//
//   - An entry is posted only when the time source answered applied, with
//     the worklog's ID.
//   - An entry whose post may have happened (unknown) is never sent
//     again: logging it alone or approving its day names it and sends
//     nothing, and the person removes it.
//   - A day's approval sends a drafted entry only when its day holds no
//     worklog of its ticket created since it was drafted; a drafted entry
//     logged alone is sent as the person chose. The check reads the days in
//     the send's own hold, on the provider and time source that then send
//     (sendTimeEntries).
//   - Nothing else sends: an entry is sent when it is drafted at the
//     revision the person read, for the connection's verified account,
//     marked sent in one statement after the connection is held and before
//     any request (Service.write).
//
// Questions stay open (D8). The answer names every entry as it is after
// the post and says in words what was not posted.
func (s *Service) PostTimeEntries(ctx context.Context, id string, in PostTimeRequest) (PostTimeView, error) {
	switch {
	case len(in.Entries) == 0 || len(in.Entries) > tracker.MaxWorklogInputs:
		return PostTimeView{}, invalidDraft("a post names 1 to %d entries", tracker.MaxWorklogInputs)
	case in.Alone && len(in.Entries) != 1:
		return PostTimeView{}, invalidDraft("logging alone names one entry")
	}
	ids := make([]int64, len(in.Entries))
	for i, ref := range in.Entries {
		if slices.Contains(ids[:i], ref.ID) {
			return PostTimeView{}, invalidDraft("entry %d is named twice", ref.ID)
		}
		ids[i] = ref.ID
	}
	if err := s.enterTime(); err != nil {
		return PostTimeView{}, err
	}
	_, author, err := s.timeAuthor(id)
	var read []store.TimeEntry
	if err == nil {
		if read, err = s.store.TimeEntriesByID(id, ids); err != nil {
			err = fmt.Errorf("trackerapp: %w", err)
		}
	}
	s.leave()
	if err != nil {
		return PostTimeView{}, err
	}
	byID := make(map[int64]store.TimeEntry, len(read))
	for _, e := range read {
		byID[e.ID] = e
	}
	var entries, send []store.TimeEntry
	var notes []postNote
	for _, ref := range in.Entries {
		e, ok := byID[ref.ID]
		if !ok {
			notes = append(notes, postNote{e: store.TimeEntry{ID: ref.ID}, why: "was removed since it was read"})
			continue
		}
		entries = append(entries, e)
		switch {
		case e.Revision != ref.Revision:
			notes = append(notes, postNote{e: e, why: movedOn(e)})
		case e.IssueID == "" && e.State == store.TimeEntryDrafted:
			notes = append(notes, postNote{e: e, open: true})
		case e.State != store.TimeEntryDrafted:
			notes = append(notes, postNote{e: e, why: movedOn(e)})
		case e.Author != author:
			notes = append(notes, postNote{e: e, why: "was drafted for another account and was not posted"})
		default:
			send = append(send, e)
		}
	}
	var applied map[int64]bool
	var unread []StaleView
	if len(send) > 0 {
		var settled []store.TimeEntry
		settled, applied, notes, unread, err = s.sendTimeEntries(ctx, id, author, send, !in.Alone, notes)
		// Entries a failed post settled changed all the same.
		s.timeEntriesChanged(id, author, settled)
		if err != nil {
			return PostTimeView{}, err
		}
	}
	if err := s.enter(); err != nil {
		return PostTimeView{}, err
	}
	now, err := s.store.TimeEntriesByID(id, ids)
	s.leave()
	if err != nil {
		return PostTimeView{}, fmt.Errorf("trackerapp: read the entries after the post: %w", err)
	}
	return postView(entries, now, applied, notes, unread), nil
}

// movedOn says why entry e, as read now, is not one the approval sends:
// posted, left out, possibly posted already, being posted, or changed.
func movedOn(e store.TimeEntry) string {
	switch e.State {
	case store.TimeEntryPosted:
		return "is logged already"
	case store.TimeEntryLeftOut:
		return "was left out"
	case store.TimeEntryUnknown:
		return mayHaveBeenLogged
	case store.TimeEntrySent:
		return "is being posted from another window"
	}
	return "changed since it was read; read the day again"
}

// checkedDays reads author's worklogs of the days of entries, at most
// worklogSpan days a read, through the run's provider, and stores them as
// a refresh does (run.worklogs).
func (r *run) checkedDays(author string, entries []store.TimeEntry) ([]tracker.Worklog, error) {
	var days []tracker.Date
	for _, e := range entries {
		d, err := tracker.ParseDate(e.Day)
		if err != nil {
			return nil, fmt.Errorf("trackerapp: entry %d: %w", e.ID, err)
		}
		days = append(days, d)
	}
	slices.SortFunc(days, tracker.Date.Compare)
	days = slices.Compact(days)
	var logs []tracker.Worklog
	for span := range daySpans(days) {
		got, err := r.worklogs(tracker.UserID(author), span[0], span[len(span)-1])
		if err != nil {
			return nil, err
		}
		logs = append(logs, got...)
	}
	return logs, nil
}

// sendTimeEntries sends sends as one batch in one hold of the connection:
// when check, it reads their days and leaves out each whose day holds a
// worklog of its ticket made since it was drafted (loggedSince); it marks
// the rest sent, sends those it marked, and settles each with its outcome. It
// returns the entries it settled, which of them the time source applied,
// notes for those not posted, and the read-back that failed
// (WriteView.Stale). A day that could not be read sends nothing. A store
// write that fails after the send fails the post: its entries stay sent
// while this process runs, and the next is told they may have been
// logged (recoverEarlier).
func (s *Service) sendTimeEntries(ctx context.Context, id, author string, sends []store.TimeEntry, check bool, notes []postNote) ([]store.TimeEntry, map[int64]bool, []postNote, []StaleView, error) {
	applied := map[int64]bool{}
	var settled []store.TimeEntry
	var storeErr error
	notSent := func(err error) tracker.Result {
		out := tracker.Result{Parts: make([]tracker.Part, len(sends))}
		for i, e := range sends {
			out.Parts[i] = tracker.Part{Issue: tracker.IssueID(e.IssueID), Step: tracker.StepWorklog, Outcome: tracker.NotSent(err)}
		}
		return out
	}
	view, err := s.write(ctx, id, writeOp{unable: func(p tracker.Provider) error {
		if p.Time() == nil {
			return fmt.Errorf("trackerapp: connection %s has no time tracking: %w", id, tracker.ErrUnsupported)
		}
		return nil
	}, call: func(r *run) tracker.Result {
		var logs []tracker.Worklog
		if check {
			var err error
			if logs, err = r.checkedDays(author, sends); err != nil {
				why := s.staleView(r.row.Kind, nil, err).Failure.Message
				for _, e := range sends {
					notes = append(notes, postNote{e: e, why: "was not posted: the day's logged time could not be read to check it (" + strings.TrimSuffix(why, ".") + "); try again"})
				}
				return notSent(err)
			}
		}
		var entries []store.TimeEntry
		var refs []store.TimeEntryRef
		for _, e := range sends {
			if check && loggedSince(e, logs) {
				notes = append(notes, postNote{e: e, why: skippedSince})
				continue
			}
			entries = append(entries, e)
			refs = append(refs, store.TimeEntryRef{ID: e.ID, Revision: e.Revision})
		}
		res := tracker.Result{Parts: []tracker.Part{{Step: tracker.StepWorklog, Outcome: tracker.Applied(), Unchanged: true}}}
		if len(refs) == 0 {
			return res
		}
		marked, err := s.store.MarkTimeEntriesSent(id, refs, millis(s.now()))
		if err != nil {
			storeErr = fmt.Errorf("trackerapp: %w", err)
			return notSent(err)
		}
		byID := make(map[int64]store.TimeEntry, len(marked))
		for _, m := range marked {
			byID[m.ID] = m
		}
		var settles []store.TimeEntrySettle
		// An attempt that did nothing puts the entry back in the draft.
		nothing := func(m store.TimeEntry, refusal, why string) {
			settles = append(settles, store.TimeEntrySettle{ID: m.ID, Revision: m.Revision, State: store.TimeEntryDrafted, Refusal: refusal})
			notes = append(notes, postNote{e: m, why: why})
		}
		// The worklogs are sent in the order entries gives them.
		var sending []store.TimeEntry
		var inputs []tracker.WorklogInput
		var unmarked []int64
		for _, e := range entries {
			m, ok := byID[e.ID]
			if !ok {
				unmarked = append(unmarked, e.ID)
				continue
			}
			day, err := tracker.ParseDate(m.Day)
			if err != nil {
				nothing(m, err.Error(), "was not sent: "+err.Error())
				continue
			}
			sending = append(sending, m)
			inputs = append(inputs, tracker.WorklogInput{Issue: tracker.IssueID(m.IssueID), Day: day,
				Spent: time.Duration(m.Seconds) * time.Second, Description: m.Reason})
		}
		notes = append(notes, s.notMarked(id, entries, unmarked)...)
		if len(inputs) > 0 {
			res = r.p.Time().AddWorklogs(r.ctx, inputs)
		}
		for i, m := range sending {
			var p tracker.Part
			if len(res.Parts) == len(sending) {
				p = res.Parts[i]
			} else {
				p.Outcome = tracker.Unknown(fmt.Errorf("the tracker answered %d parts for %d worklogs", len(res.Parts), len(sending)))
			}
			o := p.Outcome
			switch o.State() {
			case tracker.StateApplied:
				applied[m.ID] = true
				settles = append(settles, store.TimeEntrySettle{ID: m.ID, Revision: m.Revision, State: store.TimeEntryPosted, WorklogID: string(p.Worklog)})
			case tracker.StateUnknown:
				settles = append(settles, store.TimeEntrySettle{ID: m.ID, Revision: m.Revision, State: store.TimeEntryUnknown})
				notes = append(notes, postNote{e: m, why: mayHaveBeenLogged})
			case tracker.StateRefused:
				refusal, _ := o.Refusal()
				nothing(m, refusal.Message, "was refused: "+refusal.Message)
			case tracker.StateNotSent:
				why := "it was not sent"
				if err := o.Err(); err != nil {
					why = err.Error()
				}
				nothing(m, why, "was not sent: "+why)
			}
		}
		if settled, err = s.store.SettleTimeEntries(id, settles, millis(s.now())); err != nil {
			storeErr = fmt.Errorf("trackerapp: settle the posted entries: %w", err)
		}
		return res
	}})
	if err == nil {
		err = storeErr
	}
	return settled, applied, notes, view.Stale, err
}

// notMarked notes each entry of entries that ids name, which the mark
// left out, by how it reads now.
func (s *Service) notMarked(id string, entries []store.TimeEntry, ids []int64) []postNote {
	if len(ids) == 0 {
		return nil
	}
	var notes []postNote
	now, err := s.store.TimeEntriesByID(id, ids)
	for _, e := range entries {
		if !slices.Contains(ids, e.ID) {
			continue
		}
		why := "was removed since it was read"
		if err != nil {
			why = "could not be read again: " + err.Error()
		} else if i := slices.IndexFunc(now, func(x store.TimeEntry) bool { return x.ID == e.ID }); i >= 0 {
			why = movedOn(now[i])
		}
		notes = append(notes, postNote{e: e, why: why})
	}
	return notes
}

// postView builds a post's answer: the entries named, as read before the
// post (read) for its count and days and as they are after it (now) for
// the entries shown, which of them the time source applied, the notes,
// and the read-back that failed. Only a worklog day the read-back missed
// is named as not showing the posted time.
func postView(read, now []store.TimeEntry, applied map[int64]bool, notes []postNote, stale []StaleView) PostTimeView {
	out := PostTimeView{Entries: []TimeEntryView{}, Stale: stale}
	for _, e := range read {
		if i := slices.IndexFunc(now, func(x store.TimeEntry) bool { return x.ID == e.ID }); i >= 0 {
			out.Entries = append(out.Entries, timeEntryView(now[i]))
		}
	}
	var days []string
	for _, e := range read {
		if e.IssueID == "" {
			continue
		}
		out.Of++
		if applied[e.ID] {
			out.Posted++
		}
		if !slices.Contains(days, e.Day) {
			days = append(days, e.Day)
		}
	}
	slices.Sort(days)
	var b strings.Builder
	if out.Of > 0 {
		fmt.Fprintf(&b, "Posted %d of %d entries for %s.", out.Posted, out.Of, DayRange(days))
	}
	questions := map[string]int{}
	for _, n := range notes {
		if n.open {
			questions[n.e.Day]++
			continue
		}
		fmt.Fprintf(&b, " %s %s.", entryName(n.e), strings.TrimSuffix(n.why, "."))
	}
	for _, d := range sortedKeys(questions) {
		if questions[d] == 1 {
			fmt.Fprintf(&b, " 1 question for %s is still open.", dayLabel(d))
		} else {
			fmt.Fprintf(&b, " %d questions for %s are still open.", questions[d], dayLabel(d))
		}
	}
	for _, st := range stale {
		var unread []string
		for _, r := range st.Resources {
			if r.Kind == resourceNames[tracker.ResourceWorklogs] && !slices.Contains(unread, r.Day) {
				unread = append(unread, r.Day)
			}
		}
		if len(unread) == 0 {
			continue
		}
		slices.Sort(unread)
		fmt.Fprintf(&b, " The posted time shows on %s after the next read: %s.", DayRange(unread), strings.TrimSuffix(st.Failure.Message, "."))
	}
	out.Sentence = strings.TrimPrefix(b.String(), " ")
	return out
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// entryName names an entry in a sentence: WEB-12, 1h 30m.
func entryName(e store.TimeEntry) string {
	if e.IssueKey == "" {
		return fmt.Sprintf("Entry %d", e.ID)
	}
	return fmt.Sprintf("%s, %s,", e.IssueKey, hoursText(e.Seconds/60))
}

// dayLabel is a day as a sentence writes it: Mon Nov 23.
func dayLabel(day string) string {
	t, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return day
	}
	return t.Format("Mon Jan 2")
}

// DayRange is days, sorted YYYY-MM-DD, as a sentence or a title writes
// them: "Tue Nov 24", or "Mon Nov 23 to Fri Nov 27".
func DayRange(days []string) string {
	switch len(days) {
	case 0:
		return "no day"
	case 1:
		return dayLabel(days[0])
	}
	return dayLabel(days[0]) + " to " + dayLabel(days[len(days)-1])
}
