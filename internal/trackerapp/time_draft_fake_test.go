package trackerapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/store"
)

// The time draft against the fake Jira site and its Tempo account: an
// agent drafts the time of the connection's verified account, and only
// the person's approval (PostTimeEntries) sends it.

const (
	jiraWorklogPost  = "POST rest/api/3/issue/{issue}/worklog"
	tempoWorklogPost = "POST 4/worklogs"
)

// markGateStore runs gate, once, before the first MarkTimeEntriesSent
// after it is set.
type markGateStore struct {
	*worklogGateStore
	mu   sync.Mutex
	gate func()
}

func (g *markGateStore) MarkTimeEntriesSent(connection string, refs []store.TimeEntryRef, sentTo string, now, running int64) ([]store.TimeEntry, error) {
	g.mu.Lock()
	gate := g.gate
	g.gate = nil
	g.mu.Unlock()
	if gate != nil {
		gate()
	}
	return g.worklogGateStore.MarkTimeEntriesSent(connection, refs, sentTo, now, running)
}

// aheadOfTracker is how far this computer's clock is ahead of the
// tracker's in a draft scenario: within clockSlack, so every comparison
// of a worklog's update with an entry's time runs across the skew.
const aheadOfTracker = 30 * time.Second

// draftScenario is a time scenario whose service runs on the fake's
// clock, aheadOfTracker ahead, with its connection verified as user 1 and the events of its
// time entries kept. day is the range's middle day, which holds the
// fixture's worklog of WEB-1 in both time sources.
type draftScenario struct {
	*timeScenario
	marks  *markGateStore
	day    string
	label  string
	tempo  bool
	calls  int
	mu     sync.Mutex
	events []TimeEntriesEvent
}

func newDraftScenario(t *testing.T, source string) *draftScenario {
	t.Helper()
	s := newTimeScenario(t)
	// The fixture's worklogs were made before the draft, by more than
	// the clock slack.
	s.clock.advance(2 * clockSlack * time.Millisecond)
	s.svc.now = func() time.Time { return s.clock.Now().Add(aheadOfTracker) }
	s.svc.steadyFrom = millis(s.svc.now())
	marks := &markGateStore{worklogGateStore: s.gate}
	s.svc.store = marks
	s.verify(s.conn.ID)
	d := &draftScenario{timeScenario: s, marks: marks, day: s.days()[1], tempo: source == "tempo"}
	d.label = dayLabel(d.day)
	if d.tempo {
		s.addTempoOn(t)
	}
	s.svc.emitTimeEntries = func(e TimeEntriesEvent) {
		d.mu.Lock()
		d.events = append(d.events, e)
		d.mu.Unlock()
	}
	return d
}

func (d *draftScenario) takeEvents() []TimeEntriesEvent {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.events
	d.events = nil
	return out
}

var draftTickets = map[string]TimeOfferView{
	"WEB-1": {Issue: "10101", Key: "WEB-1", Title: "First story"},
	"WEB-2": {Issue: "10102", Key: "WEB-2", Title: "Second task"},
	"WEB-3": {Issue: "10103", Key: "WEB-3", Title: "Backlog task"},
}

// on is an entry of hours on key's ticket on the scenario's day.
func (d *draftScenario) on(key string, hours float64, reason string) TimeDraftEntry {
	t := draftTickets[key]
	return TimeDraftEntry{Day: d.day, Ticket: &t, Hours: hours, Reason: reason}
}

func (d *draftScenario) draft(t *testing.T, entries ...TimeDraftEntry) TimeDraftView {
	t.Helper()
	d.calls++
	return must[TimeDraftView](t)(d.svc.DraftTime(d.conn.ID, TimeDraftRequest{Thread: "thread-1", Call: fmt.Sprintf("call-%d", d.calls), Entries: entries}))
}

func entryRefs(views ...TimeEntryView) []TimeEntryRefView {
	out := make([]TimeEntryRefView, len(views))
	for i, v := range views {
		out[i] = TimeEntryRefView{ID: v.ID, Revision: v.Revision}
	}
	return out
}

// post approves refs and returns the answer with the worklog posts it made.
func (d *draftScenario) post(t *testing.T, alone bool, refs ...TimeEntryRefView) (PostTimeView, int) {
	t.Helper()
	var got PostTimeView
	reqs := d.requestsOf(t, func() {
		got = must[PostTimeView](t)(d.svc.PostTimeEntries(t.Context(), d.conn.ID, PostTimeRequest{Entries: refs, Alone: alone}))
	})
	return got, reqs[jiraWorklogPost] + reqs[tempoWorklogPost]
}

// posted fails unless the post answered sentence with posts worklog posts.
func (d *draftScenario) posted(t *testing.T, what string, got PostTimeView, posts int, sentence string, wantPosts int) {
	t.Helper()
	if got.Sentence != sentence || posts != wantPosts {
		t.Fatalf("%s: the post answered %q with %d worklog posts; want %q with %d", what, got.Sentence, posts, sentence, wantPosts)
	}
}

// entry is entry id as stored now.
func (d *draftScenario) entry(t *testing.T, id int64) store.TimeEntry {
	t.Helper()
	e, ok, err := d.st.TimeEntry(d.conn.ID, id)
	if err != nil || !ok {
		t.Fatalf("entry %d: %v, %v", id, ok, err)
	}
	return e
}

// logsOn are the worklogs of key's issue on the day, read from the
// tracker now.
func (d *draftScenario) logsOn(t *testing.T, key string) []WorklogView {
	t.Helper()
	got := must[WorklogDays](t)(d.svc.RefreshWorklogs(t.Context(), d.conn.ID, "acc-user1", d.day, d.day))
	if got.Failure != nil {
		t.Fatalf("the refresh failed: %+v", got.Failure)
	}
	var out []WorklogView
	for _, w := range got.Worklogs {
		if w.Key == key {
			out = append(out, w)
		}
	}
	return out
}

func (d *draftScenario) fault(t *testing.T, f trackerfake.Fault) {
	t.Helper()
	f.Origin, f.Route = trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: "example.atlassian.net"}, jiraWorklogPost
	if d.tempo {
		f.Origin, f.Route = trackerfake.Origin{Tracker: trackerfake.TrackerTempo}, tempoWorklogPost
	}
	if err := d.e.AddFault(f); err != nil {
		t.Fatal(err)
	}
}

func bothSources(t *testing.T, f func(t *testing.T, d *draftScenario)) {
	for _, source := range []string{"jira", "tempo"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			f(t, newDraftScenario(t, source))
		})
	}
}

// Each draft no rule lets stand is refused with the sentence the agent
// reads, naming the entry by its place; so is a draft on a connection
// with no verified account, or whose key file holds a token other than
// the one verified.
func TestADraftIsRefusedByName(t *testing.T) {
	t.Parallel()
	d := newDraftScenario(t, "jira")
	day, tomorrow := d.day, d.svc.now().AddDate(0, 0, 1).Format(time.DateOnly)
	parsed, _ := time.Parse(time.DateOnly, day)
	monthBefore := parsed.AddDate(0, 0, -31).Format(time.DateOnly)
	question := TimeDraftEntry{Day: day, Hours: 1, Reason: "Which ticket was the standup?"}
	offers := question
	for range 6 {
		offers.Offers = append(offers.Offers, draftTickets["WEB-3"])
	}
	withOffer := d.on("WEB-1", 1, "Built the form")
	withOffer.Offers = []TimeOfferView{draftTickets["WEB-2"]}
	at := func(e TimeDraftEntry, f func(*TimeDraftEntry)) TimeDraftEntry { f(&e); return e }
	for _, tc := range []struct {
		name    string
		entries []TimeDraftEntry
		want    string
	}{
		{"no entries", nil, "time_draft takes 1 to 250 entries a call."},
		{"a bad day", []TimeDraftEntry{at(question, func(e *TimeDraftEntry) { e.Day = "2026-1-2" })}, "entries[0].day takes YYYY-MM-DD."},
		{"tomorrow", []TimeDraftEntry{at(question, func(e *TimeDraftEntry) { e.Day = tomorrow })}, "entries[0].day is in the future; draft only today or earlier."},
		{"no hours", []TimeDraftEntry{at(question, func(e *TimeDraftEntry) { e.Hours = 0 })}, "entries[0].hours must be more than 0."},
		{"over a day", []TimeDraftEntry{at(question, func(e *TimeDraftEntry) { e.Hours = 25 })}, "entries[0].hours is over 24."},
		{"a part of a minute", []TimeDraftEntry{question, at(question, func(e *TimeDraftEntry) { e.Hours = 0.3333 })}, "entries[1].hours is not a whole number of minutes (0.25 is 15 minutes)."},
		{"a question asking nothing", []TimeDraftEntry{at(question, func(e *TimeDraftEntry) { e.Reason = " " })}, "entries[0] has no key, so it is a question: reason is what to ask."},
		{"no description", []TimeDraftEntry{d.on("WEB-1", 1, "")}, "entries[0].reason is empty: it becomes the worklog's description."},
		{"a long reason", []TimeDraftEntry{d.on("WEB-1", 1, strings.Repeat("x", 1025))}, "entries[0].reason is over 1 KiB."},
		{"an offer with a key", []TimeDraftEntry{withOffer}, "entries[0].offer is for an entry with no key."},
		{"six offers", []TimeDraftEntry{offers}, "entries[0].offer holds at most 5 keys."},
		{"a ticket twice", []TimeDraftEntry{d.on("WEB-1", 1, "a"), d.on("WEB-1", 2, "b")}, "WEB-1 is drafted twice on " + day + "; give it once with the hours summed."},
		{"a day over 24h", []TimeDraftEntry{d.on("WEB-1", 13, "a"), d.on("WEB-2", 13, "b")}, day + " holds 26h; a day holds at most 24h."},
		{"over 31 days", []TimeDraftEntry{question, at(question, func(e *TimeDraftEntry) { e.Day = monthBefore })}, "time_draft covers at most 31 days a call; draft the rest in another call."},
	} {
		_, err := d.svc.DraftTime(d.conn.ID, TimeDraftRequest{Thread: "thread-1", Call: "call-1", Entries: tc.entries})
		if !errors.Is(err, ErrInvalidRequest) || !strings.HasSuffix(err.Error(), ": "+tc.want) {
			t.Errorf("%s: %v; want %q", tc.name, err, tc.want)
		}
	}
	if got, err := d.st.ThreadTimeEntries("thread-1"); err != nil || len(got) != 0 {
		t.Fatalf("the refused drafts stored %+v, %v", got, err)
	}
	if ev := d.takeEvents(); len(ev) != 0 {
		t.Fatalf("the refused drafts told %+v", ev)
	}

	unverified := newTimeScenario(t)
	if _, err := unverified.svc.DraftTime(unverified.conn.ID, TimeDraftRequest{Thread: "thread-1", Call: "call-1", Entries: []TimeDraftEntry{question}}); !errors.Is(err, ErrUnverified) ||
		err.Error() != "trackerapp: the connection has no verified account: "+unverified.conn.ID {
		t.Fatalf("a draft before the connection was verified: %v", err)
	}
	d.writeToken(d.conn, "fake-jira-token-2")
	if _, err := d.svc.DraftTime(d.conn.ID, TimeDraftRequest{Thread: "thread-1", Call: "call-1", Entries: []TimeDraftEntry{question}}); !errors.Is(err, ErrUnverified) {
		t.Fatalf("a draft after the key file's token changed: %v", err)
	}
}

// A later draft of a day replaces the agents' drafted entries of that day
// (D6) and keeps the person's; a ticket with an open entry that day is
// skipped. Each draft that changed anything is told once, with its days
// and threads; one that changed nothing is not told.
func TestADraftReplacesTheAgentsEntriesOfItsDays(t *testing.T) {
	t.Parallel()
	d := newDraftScenario(t, "jira")
	first := d.draft(t, d.on("WEB-1", 1, "Built the form"), TimeDraftEntry{Day: d.day, Hours: 0.5, Reason: "Which ticket was the standup?", Offers: []TimeOfferView{draftTickets["WEB-3"]}})
	if len(first.Made) != 2 || first.Author != "acc-user1" || first.Made[1].Offers[0].Key != "WEB-3" || first.Made[0].Minutes != 60 || first.Made[0].By != "agent" {
		t.Fatalf("the first draft answered %+v", first)
	}
	if ev := d.takeEvents(); len(ev) != 1 || !slices.Equal(ev[0].Days, []string{d.day}) || !slices.Equal(ev[0].Threads, []string{"thread-1"}) || ev[0].Author != "acc-user1" {
		t.Fatalf("the first draft told %+v", ev)
	}
	if !slices.Equal(first.Logged, []LoggedDay{{Day: d.day, Unread: true}}) {
		t.Fatalf("before any read the draft answered the logged time %+v; want the day unread", first.Logged)
	}
	d.logsOn(t, "WEB-1")
	second := d.draft(t, d.on("WEB-2", 0.5, "Reviewed the pull request"))
	if len(second.Made) != 1 || len(second.Replaced) != 2 || len(second.Kept) != 0 {
		t.Fatalf("the second draft answered %+v; want WEB-2 made, both of the first replaced", second)
	}
	if !slices.Equal(second.Logged, []LoggedDay{{Day: d.day, Minutes: 60}}) {
		t.Fatalf("after a read the draft answered the logged time %+v; want WEB-1's hour", second.Logged)
	}
	hours := 0.75
	edited := must[TimeEntryView](t)(d.svc.EditTimeEntry(d.conn.ID, entryRefs(second.Made[0])[0], TimeEntryEdit{Hours: &hours}))
	if edited.By != "person" || edited.Minutes != 45 {
		t.Fatalf("the person's edit answered %+v", edited)
	}
	if _, err := d.svc.EditTimeEntry(d.conn.ID, entryRefs(second.Made[0])[0], TimeEntryEdit{Hours: &hours}); !errors.Is(err, ErrTimeEntryStale) {
		t.Fatalf("an edit at the revision before: %v", err)
	}
	d.takeEvents()
	third := d.draft(t, d.on("WEB-2", 1, "Reviewed it again"), d.on("WEB-3", 0.25, "Triaged the backlog"))
	if len(third.Made) != 1 || third.Made[0].Key != "WEB-3" || len(third.Skipped) != 1 || third.Skipped[0].Key != "WEB-2" ||
		len(third.Kept) != 1 || third.Kept[0].ID != edited.ID {
		t.Fatalf("the third draft answered %+v; want WEB-3 made, WEB-2 skipped and the person's WEB-2 kept", third)
	}
	d.takeEvents()
	d.draft(t, d.on("WEB-2", 1, "Reviewed it again"))
	if ev := d.takeEvents(); len(ev) != 1 {
		t.Fatalf("a draft that replaced WEB-3 told %+v", ev)
	}
	if again := d.draft(t, d.on("WEB-2", 1, "Reviewed it again")); len(again.Made)+len(again.Replaced) != 0 {
		t.Fatalf("a draft of only the person's ticket answered %+v", again)
	}
	if ev := d.takeEvents(); len(ev) != 0 {
		t.Fatalf("a draft that changed nothing told %+v", ev)
	}
	got := must[[]TimeEntryView](t)(d.svc.ThreadTimeEntries("thread-1"))
	if len(got) != 1 || got[0].ID != edited.ID {
		t.Fatalf("the thread's entries are %+v; want the person's WEB-2", got)
	}
}

// Approving a day posts its entries in one batch through the time source,
// each settled with the worklog it made, and leaves its question open
// (D8). A worklog logged before the entry was drafted is not a reason to
// skip it. Approving the same entries again posts nothing.
func TestApprovingADayPostsItsEntries(t *testing.T) {
	t.Parallel()
	bothSources(t, func(t *testing.T, d *draftScenario) {
		v := d.draft(t, d.on("WEB-1", 1, "Built the form"), d.on("WEB-2", 0.5, "Reviewed the pull request"),
			TimeDraftEntry{Day: d.day, Hours: 0.5, Reason: "Which ticket was the standup?"})
		d.takeEvents()
		got, posts := d.post(t, false, entryRefs(v.Made...)...)
		d.posted(t, "the day", got, posts, "Posted 2 of 2 entries for "+d.label+". 1 question for "+d.label+" is still open.", 2)
		if got.Posted != 2 || got.Of != 2 || len(got.Entries) != 3 {
			t.Fatalf("the post answered %+v", got)
		}
		for i, key := range []string{"WEB-1", "WEB-2"} {
			e := got.Entries[i]
			logs := d.logsOn(t, key)
			if e.State != store.TimeEntryPosted || e.Worklog == "" || !slices.ContainsFunc(logs, func(w WorklogView) bool {
				return w.ID == e.Worklog && strings.TrimSpace(w.Description) == e.Reason && w.SpentSeconds == e.Minutes*60
			}) {
				t.Fatalf("%s is %+v; the day's worklogs of it are %+v", key, e, logs)
			}
		}
		if q := got.Entries[2]; q.State != store.TimeEntryDrafted {
			t.Fatalf("the question is %+v; want it open", q)
		}
		if ev := d.takeEvents(); len(ev) != 1 || !slices.Equal(ev[0].Days, []string{d.day}) {
			t.Fatalf("the post told %+v", ev)
		}
		again, posts := d.post(t, false, entryRefs(v.Made...)...)
		d.posted(t, "the same approval again", again, posts, "Posted 0 of 2 entries for "+d.label+". WEB-1, 1h, is logged already. WEB-2, 30m, is logged already. 1 question for "+d.label+" is still open.", 0)
		question, posts := d.post(t, false, entryRefs(got.Entries[2])...)
		d.posted(t, "the question alone", question, posts, "1 question for "+d.label+" is still open.", 0)
	})
}

// A post whose read-back failed says which days do not show the posted
// time yet and why: the entry left the draft and its worklog is not in
// the mirror until the next read.
func TestAPostSaysTheDaysItCouldNotReadBack(t *testing.T) {
	t.Parallel()
	bothSources(t, func(t *testing.T, d *draftScenario) {
		v := d.draft(t, d.on("WEB-2", 1, "Reviewed the pull request"))
		f := trackerfake.Fault{Kind: trackerfake.FaultRefuse, Status: 403, Times: 1,
			Origin: trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: "example.atlassian.net"}, Route: "POST rest/api/3/search/jql",
			Body: json.RawMessage(`{"errorMessages":["You do not have permission to read this."],"errors":{}}`)}
		if d.tempo {
			f.Origin, f.Route, f.Body = trackerfake.Origin{Tracker: trackerfake.TrackerTempo}, "GET 4/worklogs/user/{account}", json.RawMessage(`{"errors":[{"message":"No access."}]}`)
		}
		if err := d.e.AddFault(f); err != nil {
			t.Fatal(err)
		}
		got, posts := d.post(t, true, entryRefs(v.Made...)...)
		if posts != 1 || got.Posted != 1 || len(got.Stale) != 1 || got.Stale[0].Resources[0].Day != d.day {
			t.Fatalf("the post answered %+v with %d worklog posts", got, posts)
		}
		want := "Posted 1 of 1 entries for " + d.label + ". The posted time shows on " + d.label + " after the next read: " + got.Stale[0].Failure.Message + "."
		if got.Stale[0].Failure.Message == "" || got.Sentence != want {
			t.Fatalf("the post answered %q; want %q", got.Sentence, want)
		}
	})
}

// An entry the tracker refused goes back to the draft with the refusal,
// one whose answer was lost is unknown, and the others post; the sentence
// says each.
func TestAPartlyFailedPostSaysWhatWasNotPosted(t *testing.T) {
	t.Parallel()
	bothSources(t, func(t *testing.T, d *draftScenario) {
		v := d.draft(t, d.on("WEB-1", 1, "Built the form"), d.on("WEB-2", 0.5, "Reviewed the pull request"), d.on("WEB-3", 0.25, "Triaged the backlog"))
		body := json.RawMessage(`{"errorMessages":["Worklog is closed for this period."],"errors":{}}`)
		if d.tempo {
			body = json.RawMessage(`{"errors":[{"message":"Worklog is closed for this period."}]}`)
		}
		d.fault(t, trackerfake.Fault{Kind: trackerfake.FaultRefuse, Times: 1, Status: 400, Body: body})
		d.fault(t, trackerfake.Fault{Kind: trackerfake.FaultFailAfterApply, Times: 1})
		got, posts := d.post(t, false, entryRefs(v.Made...)...)
		d.posted(t, "the post", got, posts, "Posted 1 of 3 entries for "+d.label+". WEB-1, 1h, was refused: Worklog is closed for this period."+
			" WEB-2, 30m, may have been logged; posting it again checks the day first.", 3)
		if e := d.entry(t, v.Made[0].ID); e.Refusal != "Worklog is closed for this period." {
			t.Fatalf("WEB-1 is %+v; want the tracker's refusal kept on it", e)
		}
		states := []string{}
		for _, e := range got.Entries {
			states = append(states, e.State)
		}
		if !slices.Equal(states, []string{store.TimeEntryDrafted, store.TimeEntryUnknown, store.TimeEntryPosted}) {
			t.Fatalf("the entries are %v; want drafted, unknown, posted", states)
		}
	})
}

// An entry whose post may have happened is posted again only after a read
// of its day under the same time source finds no worklog it made: a lost
// answer whose worklog landed settles with that worklog and sends nothing;
// one that never landed is sent once.
func TestAnUnknownPostIsCheckedBeforeItIsSentAgain(t *testing.T) {
	t.Parallel()
	bothSources(t, func(t *testing.T, d *draftScenario) {
		v := d.draft(t, d.on("WEB-2", 1, "Reviewed the **pull request**"))
		d.fault(t, trackerfake.Fault{Kind: trackerfake.FaultFailAfterApply, Times: 1})
		got, posts := d.post(t, false, entryRefs(v.Made...)...)
		d.posted(t, "the lost answer", got, posts, "Posted 0 of 1 entries for "+d.label+". WEB-2, 1h, may have been logged; posting it again checks the day first.", 1)
		landed := d.logsOn(t, "WEB-2")
		if len(landed) != 1 {
			t.Fatalf("the lost answer's worklogs are %+v; want the one that landed", landed)
		}
		got, posts = d.post(t, false, entryRefs(got.Entries...)...)
		d.posted(t, "the landed post approved again", got, posts, "Posted 1 of 1 entries for "+d.label+".", 0)
		if got.Entries[0].Worklog != landed[0].ID {
			t.Fatalf("the entry settled as %+v; want worklog %s", got.Entries[0], landed[0].ID)
		}

		v = d.draft(t, d.on("WEB-3", 0.5, "Triaged the backlog"))
		d.fault(t, trackerfake.Fault{Kind: trackerfake.FaultHold, Times: 1})
		done := make(chan PostTimeView, 1)
		go func() {
			got, err := d.svc.PostTimeEntries(t.Context(), d.conn.ID, PostTimeRequest{Entries: entryRefs(v.Made...)})
			if err != nil {
				t.Error(err)
			}
			done <- got
		}()
		route := jiraWorklogPost
		if d.tempo {
			route = tempoWorklogPost
		}
		waitHeld(t, d.e, route)
		origin := trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: "example.atlassian.net"}
		if d.tempo {
			origin = trackerfake.Origin{Tracker: trackerfake.TrackerTempo}
		}
		if err := d.e.Release(origin, route, true); err != nil {
			t.Fatal(err)
		}
		got = <-done
		if got.Entries[0].State != store.TimeEntryUnknown || len(d.logsOn(t, "WEB-3")) != 0 {
			t.Fatalf("the dropped post answered %+v; want it unknown with nothing logged", got)
		}
		got, posts = d.post(t, false, entryRefs(got.Entries...)...)
		d.posted(t, "the dropped post approved again", got, posts, "Posted 1 of 1 entries for "+d.label+".", 1)
		if logs := d.logsOn(t, "WEB-3"); len(logs) != 1 || logs[0].ID != got.Entries[0].Worklog {
			t.Fatalf("WEB-3's worklogs are %+v; want the one entry %+v made", logs, got.Entries[0])
		}
	})
}

// An entry marked sent by this run is being posted, and nothing sends it
// again; one marked by a run that ended before its answer was stored is
// checked on the next run's approval: settled with the worklog it made, or
// sent when it made none.
func TestAnEntrySentByAnEarlierRunIsChecked(t *testing.T) {
	t.Parallel()
	d := newDraftScenario(t, "jira")
	v := d.draft(t, d.on("WEB-2", 1, "Reviewed the pull request"), d.on("WEB-3", 0.5, "Triaged the backlog"))
	marked := must[[]store.TimeEntry](t)(d.st.MarkTimeEntriesSent(d.conn.ID, []store.TimeEntryRef{{ID: v.Made[0].ID, Revision: v.Made[0].Revision},
		{ID: v.Made[1].ID, Revision: v.Made[1].Revision}}, d.conn.ID, millis(d.svc.now()), d.svc.steadyFrom))
	refs := make([]TimeEntryRefView, len(marked))
	for i, e := range marked {
		refs[i] = TimeEntryRefView{ID: e.ID, Revision: e.Revision}
	}
	slices.SortFunc(refs, func(a, b TimeEntryRefView) int { return int(a.ID - b.ID) })
	got, posts := d.post(t, false, refs...)
	d.posted(t, "in the same run", got, posts, "Posted 0 of 2 entries for "+d.label+". WEB-2, 1h, is being posted from another window. WEB-3, 30m, is being posted from another window.", 0)
	// WEB-2's worklog landed before the run ended; WEB-3's did not.
	must[WriteView](t)(d.svc.AddWorklog(t.Context(), d.conn.ID, WorklogRequest{Issue: "10102", Day: d.day, SpentSeconds: 3600, Description: "Reviewed the pull request"}))
	d.clock.advance(time.Second)
	d.svc.steadyFrom = millis(d.svc.now())
	got, posts = d.post(t, false, refs...)
	d.posted(t, "on the next run", got, posts, "Posted 2 of 2 entries for "+d.label+".", 1)
	logs := d.logsOn(t, "WEB-2")
	if len(logs) != 1 || got.Entries[0].Worklog != logs[0].ID {
		t.Fatalf("WEB-2 settled as %+v; its worklogs are %+v", got.Entries[0], logs)
	}
}

// D4: approving a day skips an entry whose ticket and day got time logged
// by hand since it was drafted, and says so; logging the entry alone
// posts it.
func TestApprovingADaySkipsTimeLoggedByHand(t *testing.T) {
	t.Parallel()
	bothSources(t, func(t *testing.T, d *draftScenario) {
		v := d.draft(t, d.on("WEB-2", 1, "Reviewed the pull request"), d.on("WEB-3", 0.5, "Triaged the backlog"))
		// Logged by hand right after the draft, which the tracker's clock,
		// behind this computer's, stamps before the entry.
		must[WriteView](t)(d.svc.AddWorklog(t.Context(), d.conn.ID, WorklogRequest{Issue: "10102", Day: d.day, SpentSeconds: 900, Description: "Standup about it"}))
		got, posts := d.post(t, false, entryRefs(v.Made...)...)
		d.posted(t, "the day", got, posts, "Posted 1 of 2 entries for "+d.label+
			". WEB-2, 1h, was not posted: time was logged on it by hand that day since it was drafted; log it alone to post it.", 1)
		got, posts = d.post(t, true, entryRefs(got.Entries[0])...)
		d.posted(t, "WEB-2 alone", got, posts, "Posted 1 of 1 entries for "+d.label+".", 1)
	})
}

// An entry whose post may have happened under the time source used before
// is not sent blind under the new one: approving its day says to check,
// and logging it alone posts it to the new source.
func TestAChangedTimeSourceIsNotResentBlind(t *testing.T) {
	t.Parallel()
	d := newDraftScenario(t, "jira")
	v := d.draft(t, d.on("WEB-2", 1, "Reviewed the pull request"))
	d.fault(t, trackerfake.Fault{Kind: trackerfake.FaultFailAfterApply, Times: 1})
	got, _ := d.post(t, false, entryRefs(v.Made...)...)
	if got.Entries[0].State != store.TimeEntryUnknown {
		t.Fatalf("the lost answer left %+v", got.Entries[0])
	}
	d.addTempoOn(t)
	got, posts := d.post(t, false, entryRefs(got.Entries...)...)
	d.posted(t, "with Tempo added", got, posts, "Posted 0 of 1 entries for "+d.label+
		". WEB-2, 1h, may already be logged in the time source used before; check that day, then remove it or log it alone.", 0)
	reqs := d.requestsOf(t, func() {
		got = must[PostTimeView](t)(d.svc.PostTimeEntries(t.Context(), d.conn.ID, PostTimeRequest{Entries: entryRefs(got.Entries...), Alone: true}))
	})
	if got.Sentence != "Posted 1 of 1 entries for "+d.label+"." || reqs[tempoWorklogPost] != 1 || reqs[jiraWorklogPost] != 0 {
		t.Fatalf("WEB-2 alone answered %q with requests %v; want one Tempo post", got.Sentence, reqs)
	}
}

// An entry drafted for the account the connection was verified as before
// is not posted as another account's time.
func TestAnEntryOfAnotherAccountIsNotPosted(t *testing.T) {
	t.Parallel()
	d := newDraftScenario(t, "jira")
	v := d.draft(t, d.on("WEB-2", 1, "Reviewed the pull request"))
	row := d.row(d.conn.ID)
	if _, _, err := d.st.RecordTrackerConnectionVerification(d.conn.ID, store.TrackerVerification{AccountID: "acc-user2", Name: "User 2",
		VerifiedAt: millis(d.clock.Now()), Token: row.VerifiedToken}); err != nil {
		t.Fatal(err)
	}
	got, posts := d.post(t, false, entryRefs(v.Made...)...)
	d.posted(t, "as user 2", got, posts, "Posted 0 of 1 entries for "+d.label+". WEB-2, 1h, was drafted for another account and was not posted.", 0)
}

// Two windows approving the same entry at its revision post it once: the
// mark decides, and the window that lost it says the entry is logged.
func TestTwoWindowsApprovingTheSameEntryPostItOnce(t *testing.T) {
	t.Parallel()
	d := newDraftScenario(t, "jira")
	v := d.draft(t, d.on("WEB-2", 1, "Reviewed the pull request"))
	arrived, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	let := func() { once.Do(func() { close(release) }) }
	t.Cleanup(let)
	d.marks.mu.Lock()
	d.marks.gate = func() {
		close(arrived)
		select {
		case <-release:
		case <-time.After(gateWait):
		}
	}
	d.marks.mu.Unlock()
	var first PostTimeView
	var second PostTimeView
	reqs := d.requestsOf(t, func() {
		done := make(chan PostTimeView, 1)
		go func() {
			got, err := d.svc.PostTimeEntries(t.Context(), d.conn.ID, PostTimeRequest{Entries: entryRefs(v.Made...)})
			if err != nil {
				t.Error(err)
			}
			done <- got
		}()
		select {
		case <-arrived:
		case <-time.After(gateWait):
			t.Fatal("the first window never reached its mark")
		}
		second = must[PostTimeView](t)(d.svc.PostTimeEntries(t.Context(), d.conn.ID, PostTimeRequest{Entries: entryRefs(v.Made...)}))
		let()
		first = <-done
	})
	if second.Sentence != "Posted 1 of 1 entries for "+d.label+"." || first.Sentence != "Posted 0 of 1 entries for "+d.label+". WEB-2, 1h, is logged already." || reqs[jiraWorklogPost] != 1 {
		t.Fatalf("the windows answered %q and %q with %d posts; want one post", second.Sentence, first.Sentence, reqs[jiraWorklogPost])
	}
}

// A post across its connection turned off and on logs the entry once:
// whatever the post answered, the next approval settles or sends it so
// that one worklog holds it.
func TestAPostAcrossTheConnectionTurnedOffLogsItOnce(t *testing.T) {
	t.Parallel()
	d := newDraftScenario(t, "jira")
	v := d.draft(t, d.on("WEB-2", 1, "Reviewed the pull request"))
	d.fault(t, trackerfake.Fault{Kind: trackerfake.FaultHold, Times: 1})
	type answer struct {
		v   PostTimeView
		err error
	}
	done := make(chan answer, 1)
	go func() {
		got, err := d.svc.PostTimeEntries(t.Context(), d.conn.ID, PostTimeRequest{Entries: entryRefs(v.Made...)})
		done <- answer{got, err}
	}()
	waitHeld(t, d.e, jiraWorklogPost)
	must[Connection](t)(d.svc.SetEnabled(d.conn.ID, false))
	if err := d.e.Release(trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: "example.atlassian.net"}, jiraWorklogPost, false); err != nil {
		t.Fatal(err)
	}
	a := <-done
	if a.err != nil && !errors.Is(a.err, ErrDisabled) {
		t.Fatalf("the post across the turn off failed with %v", a.err)
	}
	t.Logf("the post across the turn off answered %q, %v", a.v.Sentence, a.err)
	must[Connection](t)(d.svc.SetEnabled(d.conn.ID, true))
	e := d.entry(t, v.Made[0].ID)
	if e.State != store.TimeEntryPosted {
		got, _ := d.post(t, false, TimeEntryRefView{ID: e.ID, Revision: e.Revision})
		if got.Posted != 1 {
			t.Fatalf("the approval after the turn off answered %q", got.Sentence)
		}
		e = d.entry(t, e.ID)
	}
	if logs := d.logsOn(t, "WEB-2"); len(logs) != 1 || logs[0].ID != e.WorklogID {
		t.Fatalf("WEB-2's worklogs are %+v; want the one entry %+v made", logs, e)
	}
}

// ReadTime reads the days from the tracker unless every one was read
// within timeReadFresh, and answers the drafted entries with them.
func TestReadTimeSkipsADayReadMomentsAgo(t *testing.T) {
	t.Parallel()
	d := newDraftScenario(t, "jira")
	d.draft(t, d.on("WEB-2", 1, "Reviewed the pull request"))
	read := func() (WorklogDays, int) {
		var got WorklogDays
		reqs := d.requestsOf(t, func() { got = must[WorklogDays](t)(d.svc.ReadTime(t.Context(), d.conn.ID, d.from, d.to)) })
		n := 0
		for _, c := range reqs {
			n += c
		}
		return got, n
	}
	got, n := read()
	if n == 0 || len(got.Worklogs) != 1 || len(got.Drafted) != 1 || got.Drafted[0].Key != "WEB-2" {
		t.Fatalf("the first read made %d requests and answered %+v", n, got)
	}
	d.clock.advance(timeReadFresh - time.Second)
	if _, n := read(); n != 0 {
		t.Fatalf("a read %s later made %d requests", timeReadFresh-time.Second, n)
	}
	d.clock.advance(2 * time.Second)
	if _, n := read(); n == 0 {
		t.Fatal("a read past timeReadFresh made no request")
	}
}

// A post whose connection is turned off after its entries were marked
// sent, before the worklogs were sent, sends nothing, fails as turned
// off, and puts the entries back in the draft, which it tells.
func TestAPostEndedBeforeItsSendPutsItsEntriesBack(t *testing.T) {
	t.Parallel()
	d := newDraftScenario(t, "jira")
	v := d.draft(t, d.on("WEB-2", 1, "Reviewed the pull request"))
	d.takeEvents()
	d.marks.mu.Lock()
	d.marks.gate = func() { must[Connection](t)(d.svc.SetEnabled(d.conn.ID, false)) }
	d.marks.mu.Unlock()
	var err error
	reqs := d.requestsOf(t, func() {
		_, err = d.svc.PostTimeEntries(t.Context(), d.conn.ID, PostTimeRequest{Entries: entryRefs(v.Made...)})
	})
	if !errors.Is(err, ErrDisabled) || reqs[jiraWorklogPost] != 0 {
		t.Fatalf("the post answered %v with requests %v; want it turned off with no worklog posted", err, reqs)
	}
	if e := d.entry(t, v.Made[0].ID); e.State != store.TimeEntryDrafted || e.SentMs != 0 {
		t.Fatalf("the entry is %+v; want it back in the draft", e)
	}
	if ev := d.takeEvents(); len(ev) != 1 || !slices.Equal(ev[0].Days, []string{d.day}) {
		t.Fatalf("the post told %+v", ev)
	}
}

// An entry the person edits while its approval runs, after the approval
// read it and before it was marked sent, is not posted as it was before
// the edit: the mark is at the revision the person approved.
func TestAnEntryEditedDuringItsApprovalIsNotPosted(t *testing.T) {
	t.Parallel()
	d := newDraftScenario(t, "jira")
	v := d.draft(t, d.on("WEB-2", 1, "Reviewed the pull request"))
	reason := "Reviewed and merged the pull request"
	d.marks.mu.Lock()
	d.marks.gate = func() {
		must[TimeEntryView](t)(d.svc.EditTimeEntry(d.conn.ID, entryRefs(v.Made...)[0], TimeEntryEdit{Reason: &reason}))
	}
	d.marks.mu.Unlock()
	got, posts := d.post(t, false, entryRefs(v.Made...)...)
	d.posted(t, "the approval across the edit", got, posts, "Posted 0 of 1 entries for "+d.label+". WEB-2, 1h, changed since it was read; read the day again.", 0)
	if e := d.entry(t, v.Made[0].ID); e.State != store.TimeEntryDrafted || e.Reason != reason {
		t.Fatalf("the entry is %+v; want the person's edit, drafted", e)
	}
}
