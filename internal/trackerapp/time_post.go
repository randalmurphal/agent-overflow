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
// read it, each named once. Alone is one entry logged by itself; otherwise
// the request approves a day or the whole draft, and an entry whose
// ticket and day got time logged since it was drafted is skipped
// (skipLoggedByHand).
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

// mayHaveBeenLogged is what a post says of an entry whose post may have
// happened (uncertain). Only the person settles one: they remove it, or
// log it alone, which sends it again.
const mayHaveBeenLogged = "may have been logged; check the day, then remove it or log it alone"

// loggedSince is what a day's approval says of an entry skipLoggedByHand
// skips. A worklog carries its last update, not its creation, so a
// worklog logged before the draft and changed after it skips the entry
// too.
const loggedSince = "was not posted: time on it that day was logged or changed since it was drafted; log it alone to post it"

// skipLoggedByHand (D4) skips, when a day or the whole draft is approved,
// an entry whose ticket and day have a worklog logged or changed since the
// entry was drafted (created_ms, which no edit or settle moves): posting
// both would log the time twice. Logging the entry alone posts it.
func skipLoggedByHand(e store.TimeEntry, logs []WorklogView) bool {
	for _, w := range logs {
		if w.Issue == e.IssueID && w.Day == e.Day && w.Updated >= e.CreatedMs-clockSlack {
			return true
		}
	}
	return false
}

// clockSlack is how far the tracker's clock, which stamps a worklog's
// update, may be behind this computer's, which stamps an entry, in Unix
// milliseconds. Tempo stamps whole seconds. The comparison errs toward
// finding the worklog: a skip the person can see, over time logged twice.
const clockSlack = 60_000

// uncertain reports whether entry e's post may have happened: its answer
// was lost (unknown), or a run before this one marked it sent and never
// stored the answer.
func (s *Service) uncertain(e store.TimeEntry) bool {
	return e.State == store.TimeEntryUnknown || (e.State == store.TimeEntrySent && e.SentMs < s.steadyFrom)
}

// PostTimeEntries is the one path that sends a draft's time: the
// person's approval of in.Entries on Jira connection id. It keeps three
// things true. An entry is reported posted only when the time source
// answered applied for its worklog. An entry whose post may have happened
// (uncertain) is sent again only when the person logs it alone; a day's
// or the whole draft's approval names it and leaves it. Nothing else
// sends: an entry is sent when it is drafted at the revision the person
// read, for the connection's verified account, marked sent in one
// statement after the connection is held and before any request
// (Service.write). Questions stay open (D8). The answer names every entry
// as it is after the post and says in words what was not posted.
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
	if err := s.enter(); err != nil {
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
			notes = append(notes, postNote{e: e, why: s.movedOn(e)})
		case e.IssueID == "" && e.State == store.TimeEntryDrafted:
			notes = append(notes, postNote{e: e, open: true})
		case e.State != store.TimeEntryDrafted && !(in.Alone && s.uncertain(e)):
			notes = append(notes, postNote{e: e, why: s.movedOn(e)})
		case e.Author != author:
			notes = append(notes, postNote{e: e, why: "was drafted for another account and was not posted"})
		default:
			send = append(send, e)
		}
	}
	if !in.Alone && len(send) > 0 {
		logs, failure, err := s.readDays(ctx, id, author, send)
		if err != nil {
			return PostTimeView{}, err
		}
		var kept []store.TimeEntry
		for _, e := range send {
			switch {
			case failure != "":
				notes = append(notes, postNote{e: e, why: "was not posted: the day's logged time could not be read to check it (" + failure + "); try again"})
			case skipLoggedByHand(e, logs):
				notes = append(notes, postNote{e: e, why: loggedSince})
			default:
				kept = append(kept, e)
			}
		}
		send = kept
	}
	var applied map[int64]bool
	var unread []StaleView
	if len(send) > 0 {
		var settled []store.TimeEntry
		settled, applied, notes, unread, err = s.sendTimeEntries(ctx, id, send, notes)
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
// posted, left out, possibly posted already, being posted in this run, or
// changed.
func (s *Service) movedOn(e store.TimeEntry) string {
	switch {
	case e.State == store.TimeEntryPosted:
		return "is logged already"
	case e.State == store.TimeEntryLeftOut:
		return "was left out"
	case s.uncertain(e):
		return mayHaveBeenLogged
	case e.State == store.TimeEntrySent:
		return "is being posted from another window"
	}
	return "changed since it was read; read the day again"
}

// readDays refreshes author's worklogs of the days entries cover, at most
// worklogSpan days a read, and answers them; failure is why a read could
// not be made.
func (s *Service) readDays(ctx context.Context, id, author string, entries []store.TimeEntry) ([]WorklogView, string, error) {
	var days []string
	for _, e := range entries {
		if !slices.Contains(days, e.Day) {
			days = append(days, e.Day)
		}
	}
	slices.Sort(days)
	var logs []WorklogView
	for start := 0; start < len(days); {
		first, _ := tracker.ParseDate(days[start])
		end := start
		for end+1 < len(days) {
			d, _ := tracker.ParseDate(days[end+1])
			if daysBetween(first, d) >= worklogSpan {
				break
			}
			end++
		}
		got, err := s.RefreshWorklogs(ctx, id, author, days[start], days[end])
		if err != nil {
			return nil, "", err
		}
		if got.Failure != nil {
			return nil, got.Failure.Message, nil
		}
		if got.More {
			return nil, "the days hold more worklogs than one read answers", nil
		}
		logs = append(logs, got.Worklogs...)
		start = end + 1
	}
	return logs, "", nil
}

// sendTimeEntries marks entries sent once the connection is held, sends
// those it marked as one batch, settles each with its outcome, and returns
// the entries it settled, which of them the time source applied, notes for
// those not posted, and the read-back that failed (WriteView.Stale). A
// store write that fails after the send fails the post: its entries stay
// sent, and this run shows them as being posted until the app starts
// again, when they are uncertain.
func (s *Service) sendTimeEntries(ctx context.Context, id string, entries []store.TimeEntry, notes []postNote) ([]store.TimeEntry, map[int64]bool, []postNote, []StaleView, error) {
	refs := make([]store.TimeEntryRef, len(entries))
	issues := make([]tracker.IssueID, len(entries))
	for i, e := range entries {
		refs[i] = store.TimeEntryRef{ID: e.ID, Revision: e.Revision}
		issues[i] = tracker.IssueID(e.IssueID)
	}
	applied := map[int64]bool{}
	var settled []store.TimeEntry
	var storeErr error
	view, err := s.write(ctx, id, writeOp{unable: func(p tracker.Provider) error {
		if p.Time() == nil {
			return fmt.Errorf("trackerapp: connection %s has no time tracking: %w", id, tracker.ErrUnsupported)
		}
		return nil
	}, call: func(r *run) tracker.Result {
		at := millis(s.now())
		marked, err := s.store.MarkTimeEntriesSent(id, refs, at, s.steadyFrom)
		if err != nil {
			storeErr = fmt.Errorf("trackerapp: %w", err)
			out := tracker.Result{Parts: make([]tracker.Part, len(issues))}
			for i, is := range issues {
				out.Parts[i] = tracker.Part{Issue: is, Step: tracker.StepWorklog, Outcome: tracker.NotSent(err)}
			}
			return out
		}
		byID := make(map[int64]store.TimeEntry, len(marked))
		for _, m := range marked {
			byID[m.ID] = m
		}
		// The worklogs are sent in the order entries gives them.
		var settles []store.TimeEntrySettle
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
				settles = append(settles, store.TimeEntrySettle{ID: m.ID, State: store.TimeEntryDrafted, Refusal: err.Error()})
				continue
			}
			sending = append(sending, m)
			inputs = append(inputs, tracker.WorklogInput{Issue: tracker.IssueID(m.IssueID), Day: day,
				Spent: time.Duration(m.Seconds) * time.Second, Description: m.Reason})
		}
		notes = append(notes, s.notMarked(id, entries, unmarked)...)
		res := tracker.Result{Parts: []tracker.Part{{Step: tracker.StepWorklog, Outcome: tracker.Applied(), Unchanged: true}}}
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
				settles = append(settles, store.TimeEntrySettle{ID: m.ID, State: store.TimeEntryPosted, WorklogID: string(p.Worklog)})
			case tracker.StateUnknown:
				settles = append(settles, store.TimeEntrySettle{ID: m.ID, State: store.TimeEntryUnknown})
				notes = append(notes, postNote{e: m, why: mayHaveBeenLogged})
			case tracker.StateRefused:
				refusal, _ := o.Refusal()
				settles = append(settles, store.TimeEntrySettle{ID: m.ID, State: store.TimeEntryDrafted, Refusal: refusal.Message})
				notes = append(notes, postNote{e: m, why: "was refused: " + refusal.Message})
			case tracker.StateNotSent:
				why := "it was not sent"
				if err := o.Err(); err != nil {
					why = err.Error()
				}
				settles = append(settles, store.TimeEntrySettle{ID: m.ID, State: store.TimeEntryDrafted, Refusal: why})
				notes = append(notes, postNote{e: m, why: "was not sent: " + why})
			}
		}
		if settled, err = s.store.SettleTimeEntries(id, at, settles, millis(s.now())); err != nil {
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
			why = s.movedOn(now[i])
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
