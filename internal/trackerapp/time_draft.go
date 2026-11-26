package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// The time draft (migration v173) is the person's unposted entries on a
// Jira connection, by its verified account and day; each names the agent
// call that drafted it. An agent drafts through DraftTime and never
// posts: PostTimeEntries, the person's approval, is the one path that
// sends a draft's time. Every change to an entry is conditional on the
// revision the caller read, and each one that changes anything is told
// through Options.TimeEntriesChanged.

// ErrUnverified is a time draft on a connection with no verified account:
// whose time it is is not known.
var ErrUnverified = errors.New("trackerapp: the connection has no verified account")

// The bounds of a draft.
const (
	maxDraftReason = 1024
	maxDraftOffers = 5
	maxDayMinutes  = 24 * 60
)

// TimeEntriesEvent says that the time entries of a connection's author
// changed on days, drafted by threads' agents among others.
type TimeEntriesEvent struct {
	Connection string   `json:"connection"`
	Author     string   `json:"author"`
	Days       []string `json:"days"`
	Threads    []string `json:"threads,omitempty"`
}

// TimeOfferView is a ticket a question offers, or the ticket the person
// picks to answer it.
type TimeOfferView struct {
	Issue string `json:"issue"`
	Key   string `json:"key"`
	Title string `json:"title,omitempty"`
}

// TimeEntryView is one entry of the draft. By is agent or person, who
// last wrote it; State is drafted, sent, posted, unknown or left_out;
// Refusal is why its last post did not log it.
type TimeEntryView struct {
	ID         int64           `json:"id"`
	Revision   int64           `json:"revision"`
	Connection string          `json:"connection"`
	Day        string          `json:"day"`
	Issue      string          `json:"issue,omitempty"`
	Key        string          `json:"key,omitempty"`
	Title      string          `json:"title,omitempty"`
	Minutes    int64           `json:"minutes"`
	Reason     string          `json:"reason"`
	Offers     []TimeOfferView `json:"offers,omitempty"`
	By         string          `json:"by"`
	Thread     string          `json:"thread,omitempty"`
	Call       string          `json:"call,omitempty"`
	State      string          `json:"state"`
	Refusal    string          `json:"refusal,omitempty"`
	Worklog    string          `json:"worklog,omitempty"`
}

func timeEntryView(e store.TimeEntry) TimeEntryView {
	v := TimeEntryView{ID: e.ID, Revision: e.Revision, Connection: e.Connection, Day: e.Day, Issue: e.IssueID, Key: e.IssueKey,
		Title: e.IssueTitle, Minutes: e.Seconds / 60, Reason: e.Reason, By: e.Source, Thread: e.ThreadID, Call: e.CallID,
		State: e.State, Refusal: e.Refusal, Worklog: e.WorklogID}
	for _, o := range e.Offers {
		v.Offers = append(v.Offers, TimeOfferView{Issue: o.Issue, Key: o.Key, Title: o.Title})
	}
	return v
}

func timeEntryViews(es []store.TimeEntry) []TimeEntryView {
	out := make([]TimeEntryView, len(es))
	for i, e := range es {
		out[i] = timeEntryView(e)
	}
	return out
}

// TimeDraftEntry is one entry an agent drafts: a ticket (its issue ID,
// key and title), or none for a question whose Reason is what to ask
// the person and whose Offers are tickets to pick from.
type TimeDraftEntry struct {
	Day    string
	Ticket *TimeOfferView
	Hours  float64
	Reason string
	Offers []TimeOfferView
}

// TimeDraftRequest is one agent call's draft, Thread and Call naming it.
type TimeDraftRequest struct {
	Thread  string
	Call    string
	Entries []TimeDraftEntry
}

// TimeDraftView is what a draft did: the entries it made, the agent
// entries of its days it replaced, the entries of those days it kept,
// and the new entries it skipped because the ticket already had an
// entry not yet posted that day; and each of its days' time already
// logged, as this computer last read it.
type TimeDraftView struct {
	Author   string          `json:"author"`
	Made     []TimeEntryView `json:"made"`
	Replaced []TimeEntryView `json:"replaced,omitempty"`
	Kept     []TimeEntryView `json:"kept,omitempty"`
	Skipped  []TimeEntryView `json:"skipped,omitempty"`
	Logged   []LoggedDay     `json:"logged"`
}

// LoggedDay is a day's logged minutes as the mirror holds them; Unread
// is a day no read has replaced, whose minutes are not known.
type LoggedDay struct {
	Day     string `json:"day"`
	Minutes int64  `json:"minutes"`
	Unread  bool   `json:"unread,omitempty"`
}

// timeAuthor is Jira connection id's row and its verified account, whose
// time a draft is. The token in the key file must be the one verified:
// a post logs time as the token's owner, so another token would log one
// person's draft as someone else's time.
func (s *Service) timeAuthor(id string) (store.TrackerConnection, string, error) {
	row, err := s.get(id)
	if err != nil {
		return row, "", err
	}
	if err := usable(row); err != nil {
		return row, "", err
	}
	if !hasTimeTracking(row.Kind) {
		return row, "", fmt.Errorf("trackerapp: connection %s has no time tracking: %w", id, tracker.ErrUnsupported)
	}
	if _, found := s.readKeyFile(row); row.VerifiedAccountID == "" || !sameToken(row.VerifiedToken, found) {
		return row, "", fmt.Errorf("%w: %s", ErrUnverified, id)
	}
	return row, row.VerifiedAccountID, nil
}

// today is the day it is on this computer.
func (s *Service) today() string { return s.now().Format(time.DateOnly) }

// minutesOf is hours as whole minutes; false when it is not more than
// zero or not a whole number of minutes.
func minutesOf(hours float64) (int64, bool) {
	m := hours * 60
	r := math.Round(m)
	if math.IsNaN(m) || r <= 0 || math.Abs(m-r) > 1e-6 || r > maxDayMinutes {
		return 0, false
	}
	return int64(r), true
}

// hoursText is minutes as the refusals and sentences write it: 1h 30m.
func hoursText(minutes int64) string {
	h, m := minutes/60, minutes%60
	switch {
	case h == 0:
		return fmt.Sprintf("%dm", m)
	case m == 0:
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}

func invalidDraft(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidRequest, fmt.Sprintf(format, args...))
}

// CheckDraftSize refuses a draft of n entries that no call may make: the
// tools check it before they resolve the entries' keys.
func CheckDraftSize(n int) error {
	if n == 0 || n > tracker.MaxWorklogInputs {
		return invalidDraft("time_draft takes 1 to %d entries a call.", tracker.MaxWorklogInputs)
	}
	return nil
}

// checkDraft refuses a draft no rule lets stand, naming the entry by its
// place in in.Entries, and returns its entries as stored.
func (s *Service) checkDraft(in TimeDraftRequest) ([]store.TimeEntry, error) {
	if in.Thread == "" || in.Call == "" {
		return nil, invalidDraft("a draft names its thread and call")
	}
	if err := CheckDraftSize(len(in.Entries)); err != nil {
		return nil, err
	}
	today := s.today()
	out := make([]store.TimeEntry, len(in.Entries))
	dayMinutes := map[string]int64{}
	tickets := map[[2]string]bool{}
	for i, e := range in.Entries {
		at := fmt.Sprintf("entries[%d]", i)
		if _, err := tracker.ParseDate(e.Day); err != nil {
			return nil, invalidDraft("%s.day takes YYYY-MM-DD.", at)
		}
		if e.Day > today {
			return nil, invalidDraft("%s.day is in the future; draft only today or earlier.", at)
		}
		switch {
		case e.Hours <= 0:
			return nil, invalidDraft("%s.hours must be more than 0.", at)
		case e.Hours > 24:
			return nil, invalidDraft("%s.hours is over 24.", at)
		}
		minutes, ok := minutesOf(e.Hours)
		if !ok {
			return nil, invalidDraft("%s.hours is not a whole number of minutes (0.25 is 15 minutes).", at)
		}
		switch {
		case strings.TrimSpace(e.Reason) == "" && e.Ticket == nil:
			return nil, invalidDraft("%s has no key, so it is a question: reason is what to ask.", at)
		case strings.TrimSpace(e.Reason) == "":
			return nil, invalidDraft("%s.reason is empty: it becomes the worklog's description.", at)
		case len(e.Reason) > maxDraftReason:
			return nil, invalidDraft("%s.reason is over 1 KiB.", at)
		case e.Ticket != nil && len(e.Offers) > 0:
			return nil, invalidDraft("%s.offer is for an entry with no key.", at)
		case len(e.Offers) > maxDraftOffers:
			return nil, invalidDraft("%s.offer holds at most %d keys.", at, maxDraftOffers)
		}
		entry := store.TimeEntry{Day: e.Day, Seconds: minutes * 60, Reason: e.Reason, ThreadID: in.Thread, CallID: in.Call}
		if t := e.Ticket; t != nil {
			if t.Issue == "" || t.Key == "" {
				return nil, invalidDraft("%s names no ticket", at)
			}
			if tickets[[2]string{e.Day, t.Issue}] {
				return nil, invalidDraft("%s is drafted twice on %s; give it once with the hours summed.", t.Key, e.Day)
			}
			tickets[[2]string{e.Day, t.Issue}] = true
			entry.IssueID, entry.IssueKey, entry.IssueTitle = t.Issue, t.Key, t.Title
		}
		for _, o := range e.Offers {
			entry.Offers = append(entry.Offers, store.TimeOffer{Issue: o.Issue, Key: o.Key, Title: o.Title})
		}
		dayMinutes[e.Day] += minutes
		out[i] = entry
	}
	days := make([]string, 0, len(dayMinutes))
	for d := range dayMinutes {
		days = append(days, d)
	}
	slices.Sort(days)
	for _, d := range days {
		if dayMinutes[d] > maxDayMinutes {
			return nil, invalidDraft("%s holds %s; a day holds at most 24h.", d, hoursText(dayMinutes[d]))
		}
	}
	first, _ := tracker.ParseDate(days[0])
	last, _ := tracker.ParseDate(days[len(days)-1])
	if daysBetween(first, last) >= worklogSpan {
		return nil, invalidDraft("time_draft covers at most %d days a call; draft the rest in another call.", worklogSpan)
	}
	return out, nil
}

// DraftTime replaces the agent-drafted entries of each day in's entries
// name, on Jira connection id, with them: the time of the connection's
// verified account. Entries the person made or changed stay, and so do
// those sent or posted; a new entry for a ticket that already has one
// not yet posted that day is skipped. Nothing is sent to the tracker.
func (s *Service) DraftTime(id string, in TimeDraftRequest) (TimeDraftView, error) {
	if err := s.enter(); err != nil {
		return TimeDraftView{}, err
	}
	defer s.leave()
	_, author, err := s.timeAuthor(id)
	if err != nil {
		return TimeDraftView{}, err
	}
	entries, err := s.checkDraft(in)
	if err != nil {
		return TimeDraftView{}, err
	}
	w, err := s.store.ReplaceAgentTimeDays(id, author, s.today(), entries, millis(s.now()))
	if err != nil {
		return TimeDraftView{}, fmt.Errorf("trackerapp: draft time on %s: %w", id, err)
	}
	s.timeEntriesChanged(id, author, slices.Concat(w.Made, w.Replaced))
	out := TimeDraftView{Author: author, Made: timeEntryViews(w.Made), Replaced: timeEntryViews(w.Replaced),
		Kept: timeEntryViews(w.Kept), Skipped: timeEntryViews(w.Skipped), Logged: []LoggedDay{}}
	var days []string
	for _, e := range entries {
		if !slices.Contains(days, e.Day) {
			days = append(days, e.Day)
		}
	}
	slices.Sort(days)
	// The draft is stored: a mirror that cannot be read (the connection
	// turned off since) answers its days unread, and is logged.
	logs, err := s.Worklogs(id, author, days[0], days[len(days)-1])
	if err != nil {
		s.logf("trackerapp: the logged time of a draft on %s: %v", id, err)
	}
	for _, d := range days {
		day := LoggedDay{Day: d, Unread: err != nil || slices.Contains(logs.Unread, d)}
		for _, w := range logs.Worklogs {
			if w.Day == d {
				day.Minutes += w.SpentSeconds / 60
			}
		}
		out.Logged = append(out.Logged, day)
	}
	return out, nil
}

// timeEntriesChanged tells the change of entries, on connection's
// author, by their days and threads.
func (s *Service) timeEntriesChanged(connection, author string, entries []store.TimeEntry) {
	if s.emitTimeEntries == nil || len(entries) == 0 {
		return
	}
	e := TimeEntriesEvent{Connection: connection, Author: author}
	for _, x := range entries {
		if !slices.Contains(e.Days, x.Day) {
			e.Days = append(e.Days, x.Day)
		}
		if x.ThreadID != "" && !slices.Contains(e.Threads, x.ThreadID) {
			e.Threads = append(e.Threads, x.ThreadID)
		}
	}
	slices.Sort(e.Days)
	slices.Sort(e.Threads)
	s.emitTimeEntries(e)
}

// ThreadTimeEntries are the entries thread's agent drafted that still
// exist, on every connection.
func (s *Service) ThreadTimeEntries(thread string) ([]TimeEntryView, error) {
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.leave()
	es, err := s.store.ThreadTimeEntries(thread)
	if err != nil {
		return nil, fmt.Errorf("trackerapp: %w", err)
	}
	return timeEntryViews(es), nil
}

// TimeEntryRefView names an entry as a screen read it.
type TimeEntryRefView struct {
	ID       int64 `json:"id"`
	Revision int64 `json:"revision"`
}

// TimeEntryEdit is the person's change of a drafted entry: the ticket,
// hours or reason set, or LeaveOut to close a question with nothing
// posted.
type TimeEntryEdit struct {
	Ticket   *TimeOfferView `json:"ticket,omitempty"`
	Hours    *float64       `json:"hours,omitempty"`
	Reason   *string        `json:"reason,omitempty"`
	LeaveOut bool           `json:"leave_out,omitempty"`
}

// ErrTimeEntryStale is an entry that changed, was posted or was removed
// since the caller read it.
var ErrTimeEntryStale = errors.New("trackerapp: the time entry changed since it was read")

// EditTimeEntry applies the person's edit to entry ref on connection id,
// which becomes the person's, so a later agent draft of its day keeps
// it. An entry that is not drafted at ref's revision is refused with
// ErrTimeEntryStale.
func (s *Service) EditTimeEntry(id string, ref TimeEntryRefView, in TimeEntryEdit) (TimeEntryView, error) {
	if err := s.enter(); err != nil {
		return TimeEntryView{}, err
	}
	defer s.leave()
	_, author, err := s.timeAuthor(id)
	if err != nil {
		return TimeEntryView{}, err
	}
	var ch store.TimeEntryChange
	switch {
	case in.LeaveOut && (in.Ticket != nil || in.Hours != nil || in.Reason != nil):
		return TimeEntryView{}, invalidDraft("leaving an entry out changes nothing else")
	case !in.LeaveOut && in.Ticket == nil && in.Hours == nil && in.Reason == nil:
		return TimeEntryView{}, invalidDraft("the edit changes nothing")
	}
	ch.LeaveOut = in.LeaveOut
	if t := in.Ticket; t != nil {
		if t.Issue == "" || t.Key == "" {
			return TimeEntryView{}, invalidDraft("the edit names no ticket")
		}
		ch.Issue = &store.TimeOffer{Issue: t.Issue, Key: t.Key, Title: t.Title}
	}
	if in.Hours != nil {
		minutes, ok := minutesOf(*in.Hours)
		if !ok {
			return TimeEntryView{}, invalidDraft("hours is more than 0, at most 24 and a whole number of minutes")
		}
		secs := minutes * 60
		ch.Seconds = &secs
	}
	if r := in.Reason; r != nil {
		if strings.TrimSpace(*r) == "" || len(*r) > maxDraftReason {
			return TimeEntryView{}, invalidDraft("the reason is 1 byte to 1 KiB")
		}
		ch.Reason = r
	}
	e, ok, err := s.store.EditTimeEntry(id, store.TimeEntryRef{ID: ref.ID, Revision: ref.Revision}, ch, millis(s.now()))
	switch {
	case errors.Is(err, store.ErrTimeEntryOpen):
		return TimeEntryView{}, invalidDraft("%s already has an entry not yet posted that day", in.Ticket.Key)
	case err != nil:
		return TimeEntryView{}, fmt.Errorf("trackerapp: %w", err)
	case !ok:
		return TimeEntryView{}, fmt.Errorf("%w: entry %d", ErrTimeEntryStale, ref.ID)
	}
	s.timeEntriesChanged(id, author, []store.TimeEntry{e})
	return timeEntryView(e), nil
}

// RemoveTimeEntries deletes the entries refs name on connection id that
// are drafted, unknown or left out at their revision, and returns those
// it deleted; an entry being sent or posted stays.
func (s *Service) RemoveTimeEntries(id string, refs []TimeEntryRefView) ([]TimeEntryView, error) {
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.leave()
	_, author, err := s.timeAuthor(id)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 || len(refs) > tracker.MaxWorklogInputs {
		return nil, invalidDraft("a remove names 1 to %d entries", tracker.MaxWorklogInputs)
	}
	gone, err := s.store.RemoveTimeEntries(id, storeRefs(refs))
	if err != nil {
		return nil, fmt.Errorf("trackerapp: %w", err)
	}
	s.timeEntriesChanged(id, author, gone)
	return timeEntryViews(gone), nil
}

func storeRefs(refs []TimeEntryRefView) []store.TimeEntryRef {
	out := make([]store.TimeEntryRef, len(refs))
	for i, r := range refs {
		out[i] = store.TimeEntryRef{ID: r.ID, Revision: r.Revision}
	}
	return out
}

// timeReadFresh is how recently a day's worklogs were read for ReadTime
// to answer them with no refresh: a second read in the same turn, or one
// moments after the Time screen's, costs no request.
const timeReadFresh = 60 * time.Second

// ReadTime is the time of Jira connection id's verified account from day
// from to day to, as Worklogs answers it, read from the tracker first
// unless every day was read within timeReadFresh by any path.
func (s *Service) ReadTime(ctx context.Context, id, from, to string) (WorklogDays, error) {
	if err := s.enter(); err != nil {
		return WorklogDays{}, err
	}
	_, author, err := s.timeAuthor(id)
	s.leave()
	if err != nil {
		return WorklogDays{}, err
	}
	days, err := s.Worklogs(id, author, from, to)
	if err != nil {
		return WorklogDays{}, err
	}
	fresh := len(days.Unread) == 0 && !days.More
	since := millis(s.now().Add(-timeReadFresh))
	for _, at := range days.readAt {
		if at < since {
			fresh = false
		}
	}
	if fresh {
		return days, nil
	}
	return s.RefreshWorklogs(ctx, id, author, from, to)
}
