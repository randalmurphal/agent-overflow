package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/adf"
)

// PostTimeRequest is the person's approval of time entries, each as they
// read it. Alone is an entry logged by itself; otherwise the request
// approves a day or the whole draft, and an entry whose ticket and day
// got time logged by hand since it was drafted is skipped (skipLoggedByHand).
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

// skipLoggedByHand (D4) skips, when a day or the whole draft is approved,
// an entry whose ticket and day got time logged by hand since the entry
// was drafted: posting both would log the time twice. Logging the entry
// alone posts it.
func skipLoggedByHand(e store.TimeEntry, logs []WorklogView, claimed map[string]bool) bool {
	for _, w := range logs {
		if w.Issue == e.IssueID && w.Day == e.Day && w.Updated >= e.UpdatedMs-clockSlack && !claimed[w.ID] {
			return true
		}
	}
	return false
}

// clockSlack is how far the tracker's clock, which stamps a worklog's
// update, may be behind this computer's, which stamps an entry, in Unix
// milliseconds. Tempo stamps whole seconds. Each comparison of the two
// errs toward finding the worklog: a skip or a settle the person can see,
// over time logged twice.
const clockSlack = 60_000

// resendMatch is the worklog, among logs, that a sent or unknown entry
// made: its ticket, day and time, its description as the time source
// stores it (Jira's through the converter's round trip; Tempo's as
// written, unverified against Tempo), updated no earlier than a minute
// before it was sent, and named by no other entry. ok is false when the
// description cannot be compared, so the entry is not sent blind.
func resendMatch(e store.TimeEntry, logs []WorklogView, claimed map[string]bool, tempo bool) (id string, comparable bool) {
	want := e.Reason
	if !tempo {
		doc, _, err := adf.FromMarkdown(context.Background(), e.Reason, nil)
		if err != nil {
			return "", false
		}
		want = adf.ToMarkdown(doc)
	}
	for _, w := range logs {
		if w.Issue == e.IssueID && w.Day == e.Day && w.SpentSeconds == e.Seconds && w.Updated >= e.SentMs-clockSlack &&
			!claimed[w.ID] && strings.TrimSpace(w.Description) == strings.TrimSpace(want) {
			return w.ID, true
		}
	}
	return "", true
}

// timeSourceOf is where Jira connection row's time goes now: its Tempo
// connection's ID while that is on, the connection's own otherwise.
func (s *Service) timeSourceOf(row store.TrackerConnection) (string, error) {
	t, ok, err := s.tempoOf(row.ID)
	if err != nil {
		return "", err
	}
	if ok && t.Enabled {
		return t.ID, nil
	}
	return row.ID, nil
}

// PostTimeEntries is the one path that sends a draft's time: the
// person's approval of in.Entries on Jira connection id. An entry is
// posted when it is drafted, or sent or unknown and a read of its day
// under the same time source finds no worklog it made (resendMatch; a
// found one settles it as posted), at the revision the person read, for
// the connection's verified account. Each is marked sent in one
// transaction after the connection is held and before any request
// (Service.write), so an entry is never sent twice without that read.
// Questions stay open (D8). The answer names every entry and says in
// words what was not posted.
func (s *Service) PostTimeEntries(ctx context.Context, id string, in PostTimeRequest) (PostTimeView, error) {
	if len(in.Entries) == 0 || len(in.Entries) > tracker.MaxWorklogInputs {
		return PostTimeView{}, invalidDraft("a post names 1 to %d entries", tracker.MaxWorklogInputs)
	}
	if err := s.enter(); err != nil {
		return PostTimeView{}, err
	}
	row, author, err := s.timeAuthor(id)
	var sentTo string
	if err == nil {
		sentTo, err = s.timeSourceOf(row)
	}
	var entries []store.TimeEntry
	var notes []postNote
	for _, ref := range in.Entries {
		if err != nil {
			break
		}
		e, found, rerr := s.store.TimeEntry(id, ref.ID)
		switch {
		case rerr != nil:
			err = fmt.Errorf("trackerapp: %w", rerr)
		case !found:
			notes = append(notes, postNote{e: store.TimeEntry{ID: ref.ID}, why: "was removed since it was read"})
		default:
			entries = append(entries, e)
			if e.Revision != ref.Revision {
				notes = append(notes, postNote{e: e, why: s.movedOn(e)})
			}
		}
	}
	s.leave()
	if err != nil {
		return PostTimeView{}, err
	}
	noted := map[int64]bool{}
	for _, n := range notes {
		noted[n.e.ID] = true
	}
	var send, check []store.TimeEntry
	for _, e := range entries {
		switch {
		case noted[e.ID]:
		case e.IssueID == "" && e.State == store.TimeEntryDrafted:
			notes = append(notes, postNote{e: e, open: true})
		case e.State == store.TimeEntryPosted || e.State == store.TimeEntryLeftOut ||
			(e.State == store.TimeEntrySent && e.SentMs >= s.steadyFrom):
			notes = append(notes, postNote{e: e, why: s.movedOn(e)})
		case e.Author != author:
			notes = append(notes, postNote{e: e, why: "was drafted for another account and was not posted"})
		case e.State == store.TimeEntryDrafted:
			send = append(send, e)
		case e.SentTo != sentTo && in.Alone:
			send = append(send, e)
		case e.SentTo != sentTo:
			notes = append(notes, postNote{e: e, why: "may already be logged in the time source used before; check that day, then remove it or log it alone"})
		default:
			check = append(check, e)
		}
	}
	tempo := sentTo != id
	var found []store.TimeEntrySettle
	if len(check) > 0 || (!in.Alone && len(send) > 0) {
		logs, claimed, failure, err := s.readDays(ctx, id, author, slices.Concat(send, check))
		if err != nil {
			return PostTimeView{}, err
		}
		var kept []store.TimeEntry
		for _, e := range send {
			switch {
			case in.Alone || e.State != store.TimeEntryDrafted:
				kept = append(kept, e)
			case failure != "":
				notes = append(notes, postNote{e: e, why: "was not posted: the day's logged time could not be read to check it (" + failure + "); try again"})
			case skipLoggedByHand(e, logs, claimed):
				notes = append(notes, postNote{e: e, why: "was not posted: time was logged on it by hand that day since it was drafted; log it alone to post it"})
			default:
				kept = append(kept, e)
			}
		}
		send = kept
		for _, e := range check {
			if failure != "" {
				notes = append(notes, postNote{e: e, why: "may have been logged; the day could not be read to check it (" + failure + "); try again"})
				continue
			}
			made, comparable := resendMatch(e, logs, claimed, tempo)
			switch {
			case !comparable:
				notes = append(notes, postNote{e: e, why: "may have been logged and cannot be checked; check that day, then remove it or log it alone"})
			case made != "":
				claimed[made] = true
				found = append(found, store.TimeEntrySettle{ID: e.ID, State: store.TimeEntryPosted, WorklogID: made})
				send = append(send, e) // marked below, then settled as found
			default:
				send = append(send, e)
			}
		}
	}
	var res []store.TimeEntry
	var unread []StaleView
	if len(send) > 0 {
		res, notes, unread, err = s.sendTimeEntries(ctx, id, sentTo, send, found, notes)
		// Entries a failed post settled changed all the same.
		s.timeEntriesChanged(id, author, res)
		if err != nil {
			return PostTimeView{}, err
		}
	}
	return s.postView(entries, res, notes, unread), nil
}

// movedOn says why entry e, read again, is not the one the person
// approved: posted, left out, being posted in this run, or changed.
func (s *Service) movedOn(e store.TimeEntry) string {
	switch {
	case e.State == store.TimeEntryPosted:
		return "is logged already"
	case e.State == store.TimeEntryLeftOut:
		return "was left out"
	case e.State == store.TimeEntrySent && e.SentMs >= s.steadyFrom:
		return "is being posted from another window"
	}
	return "changed since it was read; read the day again"
}

// readDays refreshes author's worklogs of the days entries cover, at most
// worklogSpan days a read, and answers them with the worklogs entries of
// the connection name; failure is why a read could not be made.
func (s *Service) readDays(ctx context.Context, id, author string, entries []store.TimeEntry) ([]WorklogView, map[string]bool, string, error) {
	var days []string
	for _, e := range entries {
		if !slices.Contains(days, e.Day) {
			days = append(days, e.Day)
		}
	}
	slices.Sort(days)
	var logs []WorklogView
	claimed := map[string]bool{}
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
			return nil, nil, "", err
		}
		if got.Failure != nil {
			return nil, nil, got.Failure.Message, nil
		}
		if got.More {
			return nil, nil, "the days hold more worklogs than one read answers", nil
		}
		logs = append(logs, got.Worklogs...)
		for _, e := range got.Drafted {
			if e.Worklog != "" {
				claimed[e.Worklog] = true
			}
		}
		start = end + 1
	}
	return logs, claimed, "", nil
}

// errTimeSourceMoved is a post whose time source changed between its
// checks and its send.
var errTimeSourceMoved = errors.New("the connection's time source changed; approve again")

// sendTimeEntries marks entries sent to sentTo once the connection is
// held, settles those found already made (found), sends the rest as one
// batch, settles each with its outcome, and returns the entries as
// stored, with notes for those not posted and the read-back that failed
// (WriteView.Stale). A store write that fails
// after the send fails the post: its entries stay sent, and this run
// shows them as being posted until the app starts again.
func (s *Service) sendTimeEntries(ctx context.Context, id, sentTo string, entries []store.TimeEntry, found []store.TimeEntrySettle, notes []postNote) ([]store.TimeEntry, []postNote, []StaleView, error) {
	refs := make([]store.TimeEntryRef, len(entries))
	issues := make([]tracker.IssueID, len(entries))
	for i, e := range entries {
		refs[i] = store.TimeEntryRef{ID: e.ID, Revision: e.Revision}
		issues[i] = tracker.IssueID(e.IssueID)
	}
	isFound := map[int64]store.TimeEntrySettle{}
	for _, f := range found {
		isFound[f.ID] = f
	}
	var stored []store.TimeEntry
	var storeErr error
	view, err := s.write(ctx, id, writeOp{unable: func(p tracker.Provider) error {
		if p.Time() == nil {
			return fmt.Errorf("trackerapp: connection %s has no time tracking: %w", id, tracker.ErrUnsupported)
		}
		return nil
	}, call: func(r *run) tracker.Result {
		notSent := func(err error) tracker.Result {
			out := tracker.Result{Parts: make([]tracker.Part, len(issues))}
			for i, is := range issues {
				out.Parts[i] = tracker.Part{Issue: is, Step: tracker.StepWorklog, Outcome: tracker.NotSent(err)}
			}
			return out
		}
		if now, err := s.timeSourceOf(r.row); err != nil || now != sentTo {
			if err == nil {
				err = errTimeSourceMoved
			}
			for _, e := range entries {
				notes = append(notes, postNote{e: e, why: "was not sent: " + err.Error()})
			}
			return notSent(err)
		}
		at := millis(s.now())
		marked, err := s.store.MarkTimeEntriesSent(id, refs, sentTo, at, s.steadyFrom)
		if err != nil {
			storeErr = fmt.Errorf("trackerapp: %w", err)
			return notSent(err)
		}
		byID := make(map[int64]store.TimeEntry, len(marked))
		for _, m := range marked {
			byID[m.ID] = m
		}
		// The worklogs are sent in the order entries gives them.
		var settles []store.TimeEntrySettle
		var sending []store.TimeEntry
		var inputs []tracker.WorklogInput
		for _, e := range entries {
			if m, ok := byID[e.ID]; ok {
				if f, ok := isFound[m.ID]; ok {
					settles = append(settles, f)
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
				continue
			}
			why := "was removed since it was read"
			now, ok, err := s.store.TimeEntry(id, e.ID)
			switch {
			case err != nil:
				why = "could not be read again: " + err.Error()
			case ok:
				why = s.movedOn(now)
			}
			notes = append(notes, postNote{e: e, why: why})
		}
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
				settles = append(settles, store.TimeEntrySettle{ID: m.ID, State: store.TimeEntryPosted, WorklogID: string(p.Worklog)})
			case tracker.StateUnknown:
				settles = append(settles, store.TimeEntrySettle{ID: m.ID, State: store.TimeEntryUnknown})
				notes = append(notes, postNote{e: m, why: "may have been logged; posting it again checks the day first"})
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
		if stored, err = s.store.SettleTimeEntries(id, at, settles, millis(s.now())); err != nil {
			storeErr = fmt.Errorf("trackerapp: settle the posted entries: %w", err)
		}
		return res
	}})
	if err == nil {
		err = storeErr
	}
	return stored, notes, view.Stale, err
}

// postView builds the answer from entries as read, the entries a send
// stored after, the notes, and the read-back that failed.
func (s *Service) postView(read, sent []store.TimeEntry, notes []postNote, stale []StaleView) PostTimeView {
	now := map[int64]store.TimeEntry{}
	for _, e := range read {
		now[e.ID] = e
	}
	for _, e := range sent {
		now[e.ID] = e
	}
	out := PostTimeView{Entries: []TimeEntryView{}, Stale: stale}
	var days []string
	var posted, attempted []store.TimeEntry
	for _, e := range read {
		cur := now[e.ID]
		out.Entries = append(out.Entries, timeEntryView(cur))
		if e.IssueID == "" {
			continue
		}
		if !slices.Contains(days, e.Day) {
			days = append(days, e.Day)
		}
		attempted = append(attempted, cur)
		if cur.State == store.TimeEntryPosted && slices.ContainsFunc(sent, func(x store.TimeEntry) bool { return x.ID == e.ID }) {
			posted = append(posted, cur)
		}
	}
	slices.Sort(days)
	out.Posted, out.Of = len(posted), len(attempted)
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
			if r.Day != "" && !slices.Contains(unread, r.Day) {
				unread = append(unread, r.Day)
			}
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
