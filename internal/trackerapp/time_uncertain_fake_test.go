package trackerapp

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/store"
)

// An entry whose post may have happened (unknown), as every path sees it:
// a run's read, remove, approval, log alone and settle, and the next run
// after the app stopped between the tracker's answer and the store's.

// sentAgain is what logging alone says of an unknown entry whose ticket
// got time logged that day since it was first sent.
const sentAgain = "was not sent again: time was logged on it that day since it was first sent; remove the entry if that worklog is it, or log the time by hand."

// restart makes the scenario's service a later run of the app over the
// same store: what the run before left is recovered before it reads or
// writes an entry.
func (d *draftScenario) restart() {
	d.clock.advance(time.Second)
	d.svc.earlier.Lock()
	d.svc.earlier.done = false
	d.svc.earlier.Unlock()
}

// crashAfterSend posts refs with the store's settle failing, as a run that
// stopped after the tracker's answer and before it stored it leaves the
// entries, and returns the worklog posts it made.
func (d *draftScenario) crashAfterSend(t *testing.T, refs ...TimeEntryRefView) int {
	t.Helper()
	d.marks.mu.Lock()
	d.marks.settleErr = errors.New("the app stopped")
	d.marks.mu.Unlock()
	reqs := d.requestsOf(t, func() {
		if _, err := d.svc.PostTimeEntries(t.Context(), d.conn.ID, PostTimeRequest{Entries: refs}); err == nil {
			t.Fatal("the post whose settle failed answered no error")
		}
	})
	return reqs[jiraWorklogPost] + reqs[tempoWorklogPost]
}

// An entry a run left sent, whose worklog landed, is unknown to the next
// run before it reads anything: its thread's entries show it unknown, a
// day's approval leaves it, logging it alone finds the worklog its first
// send made and sends nothing, and the person removes it at the revision
// read, all with no further worklog posted.
func TestAnEntryARunLeftSentIsUnknownToTheNext(t *testing.T) {
	t.Parallel()
	bothSources(t, func(t *testing.T, d *draftScenario) {
		v := d.draft(t, d.on("WEB-2", 1, "Reviewed the pull request"))
		if posts := d.crashAfterSend(t, entryRefs(v.Made...)...); posts != 1 {
			t.Fatalf("the post before the stop made %d worklog posts", posts)
		}
		d.restart()
		shown := must[[]TimeEntryView](t)(d.svc.ThreadTimeEntries("thread-1"))
		if len(shown) != 1 || shown[0].State != store.TimeEntryUnknown {
			t.Fatalf("the next run shows the thread's entries as %+v; want WEB-2 unknown", shown)
		}
		got, posts := d.post(t, false, entryRefs(shown...)...)
		d.posted(t, "the day approved on the next run", got, posts, "Posted 0 of 1 entries for "+d.label+". WEB-2, 1h, "+mayHave, 0)
		got, posts = d.post(t, true, entryRefs(shown...)...)
		d.posted(t, "WEB-2 logged alone on the next run", got, posts, "Posted 0 of 1 entries for "+d.label+". WEB-2, 1h, "+sentAgain, 0)
		if e := d.entry(t, shown[0].ID); e.State != store.TimeEntryUnknown {
			t.Fatalf("WEB-2 is %+v after logging it alone found its worklog; want it unknown", e)
		}
		gone := must[[]TimeEntryView](t)(d.svc.RemoveTimeEntries(d.conn.ID, entryRefs(got.Entries...)))
		if len(gone) != 1 || gone[0].ID != shown[0].ID {
			t.Fatalf("removing WEB-2 on the next run removed %+v", gone)
		}
		if logs := d.logsOn(t, "WEB-2"); len(logs) != 1 {
			t.Fatalf("WEB-2's worklogs are %+v; want the one the first send made", logs)
		}
	})
}

// Logging alone an entry whose answer was lost sends nothing when its day
// holds a worklog of its ticket created since it was first sent, the one
// that landed: the entry stays unknown and the sentence says what to do.
func TestLoggingAloneAfterALostAnswerSendsNothing(t *testing.T) {
	t.Parallel()
	bothSources(t, func(t *testing.T, d *draftScenario) {
		v := d.draft(t, d.on("WEB-2", 1, "Reviewed the pull request"))
		d.fault(t, trackerfake.Fault{Kind: trackerfake.FaultFailAfterApply, Times: 1})
		got, posts := d.post(t, false, entryRefs(v.Made...)...)
		d.posted(t, "the lost answer", got, posts, "Posted 0 of 1 entries for "+d.label+". WEB-2, 1h, "+mayHave, 1)
		got, posts = d.post(t, true, entryRefs(got.Entries...)...)
		d.posted(t, "WEB-2 logged alone after the lost answer", got, posts, "Posted 0 of 1 entries for "+d.label+". WEB-2, 1h, "+sentAgain, 0)
		if e := d.entry(t, v.Made[0].ID); e.State != store.TimeEntryUnknown {
			t.Fatalf("WEB-2 is %+v; want it unknown", e)
		}
		if logs := d.logsOn(t, "WEB-2"); len(logs) != 1 {
			t.Fatalf("WEB-2's worklogs are %+v; want only the one that landed", logs)
		}
	})
}

// After a dropped request, which made no worklog, logging the entry alone
// sends it. A retry the tracker refused did nothing: the entry stays
// unknown with its first send's time, so a redraft of its day keeps it,
// and logging it alone again posts it.
func TestARefusedRetryKeepsTheEntryUnknown(t *testing.T) {
	t.Parallel()
	bothSources(t, func(t *testing.T, d *draftScenario) {
		var got PostTimeView
		var posts int
		v := d.draft(t, d.on("WEB-1", 0.25, "Planned the sprint"))
		d.fault(t, trackerfake.Fault{Kind: trackerfake.FaultHold, Times: 1})
		done := make(chan PostTimeView, 1)
		go func() {
			got, err := d.svc.PostTimeEntries(t.Context(), d.conn.ID, PostTimeRequest{Entries: entryRefs(v.Made...)})
			if err != nil {
				t.Error(err)
			}
			done <- got
		}()
		route, origin := jiraWorklogPost, trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: "example.atlassian.net"}
		if d.tempo {
			route, origin = tempoWorklogPost, trackerfake.Origin{Tracker: trackerfake.TrackerTempo}
		}
		waitHeld(t, d.e, route)
		if err := d.e.Release(origin, route, true); err != nil {
			t.Fatal(err)
		}
		got = <-done
		if got.Entries[0].State != store.TimeEntryUnknown {
			t.Fatalf("the dropped post answered %+v; want it unknown", got)
		}
		dropped := d.entry(t, v.Made[0].ID)
		before := len(d.logsOn(t, "WEB-1"))

		// A retry the tracker refuses did nothing.
		body := `{"errorMessages":["Worklog is closed for this period."],"errors":{}}`
		if d.tempo {
			body = `{"errors":[{"message":"Worklog is closed for this period."}]}`
		}
		d.clock.advance(3 * clockSlack * time.Millisecond)
		d.fault(t, trackerfake.Fault{Kind: trackerfake.FaultRefuse, Times: 1, Status: 400, Body: []byte(body)})
		got, posts = d.post(t, true, entryRefs(got.Entries...)...)
		e := d.entry(t, dropped.ID)
		if posts != 1 || e.State != store.TimeEntryUnknown || e.SentMs != dropped.SentMs {
			t.Fatalf("WEB-1 is %+v after a refused retry (%q, %d posts); want it unknown, sent at %d", e, got.Sentence, posts, dropped.SentMs)
		}
		if w := d.draft(t, d.on("WEB-3", 0.5, "Triaged the backlog")); len(w.Replaced) != 0 {
			t.Fatalf("a redraft of the day replaced %+v", w.Replaced)
		}
		if e = d.entry(t, dropped.ID); e.State != store.TimeEntryUnknown {
			t.Fatalf("WEB-1 is %+v after a redraft of its day; want it unknown", e)
		}
		got, posts = d.post(t, true, TimeEntryRefView{ID: e.ID, Revision: e.Revision})
		d.posted(t, "WEB-1 logged alone after the refused retry", got, posts, "Posted 1 of 1 entries for "+d.label+".", 1)
		if logs := d.logsOn(t, "WEB-1"); len(logs) != before+1 || got.Entries[0].State != store.TimeEntryPosted {
			t.Fatalf("WEB-1's worklogs are %+v and the entry %+v; want one more, the entry's", logs, got.Entries[0])
		}
	})
}

// A worklog of the ticket logged before the entry was drafted and changed
// after it is no reason to skip the entry: the approval reads when the
// worklog was created, not when it last changed.
func TestApprovingADayPostsBesideAnOlderWorklogChangedSince(t *testing.T) {
	t.Parallel()
	bothSources(t, func(t *testing.T, d *draftScenario) {
		v := d.draft(t, d.on("WEB-1", 0.5, "Planned the sprint"))
		older := d.logsOn(t, "WEB-1")
		if len(older) != 1 {
			t.Fatalf("WEB-1's worklogs before the draft are %+v; want the fixture's", older)
		}
		d.clock.advance(3 * clockSlack * time.Millisecond)
		described := "Sprint planning, notes added"
		must[WriteView](t)(d.svc.EditWorklog(t.Context(), d.conn.ID, WorklogEditRequest{From: older[0], Description: &described}))
		got, posts := d.post(t, false, entryRefs(v.Made...)...)
		d.posted(t, "the day beside the changed worklog", got, posts, "Posted 1 of 1 entries for "+d.label+".", 1)
	})
}

// A day's approval checks the day's worklogs and posts on one time
// source: Tempo turned on from another window after the check is not
// where the entry goes, and a switch during the check's read sends
// nothing.
func TestADayIsCheckedAndPostedOnOneTimeSource(t *testing.T) {
	t.Parallel()
	d := newDraftScenario(t, "jira")
	v := d.draft(t, d.on("WEB-2", 1, "Reviewed the pull request"))
	tempo := d.addTempoOn(t)
	must[WriteView](t)(d.svc.AddWorklog(t.Context(), d.conn.ID, WorklogRequest{Issue: "10102", Day: d.day, SpentSeconds: 1800, Description: "Reviewed it by hand"}))
	must[Connection](t)(d.svc.SetEnabled(tempo.ID, false))
	d.marks.mu.Lock()
	d.marks.readGate = func() { must[Connection](t)(d.svc.SetEnabled(tempo.ID, true)) }
	d.marks.mu.Unlock()
	reqs := d.requestsOf(t, func() {
		must[PostTimeView](t)(d.svc.PostTimeEntries(t.Context(), d.conn.ID, PostTimeRequest{Entries: entryRefs(v.Made...)}))
	})
	d.marks.mu.Lock()
	d.marks.readGate = nil
	d.marks.mu.Unlock()
	if reqs[tempoWorklogPost] != 0 {
		t.Fatalf("the approval checked Jira and posted to Tempo, which holds time logged by hand on WEB-2: %v", reqs)
	}

	d = newDraftScenario(t, "jira")
	v = d.draft(t, d.on("WEB-2", 1, "Reviewed the pull request"))
	tempo = d.addTempoOn(t)
	must[Connection](t)(d.svc.SetEnabled(tempo.ID, false))
	d.gate.mu.Lock()
	d.gate.gate = func() { must[Connection](t)(d.svc.SetEnabled(tempo.ID, true)) }
	d.gate.mu.Unlock()
	got, posts := d.post(t, false, entryRefs(v.Made...)...)
	d.posted(t, "the day across a switch", got, posts, "Posted 0 of 1 entries for "+d.label+
		". WEB-2, 1h, was not posted: the day's logged time could not be read to check it ("+strings.TrimSuffix(timeSourceChanged, ".")+"); try again.", 0)
}

// A day holds at most 24 hours of entries not yet logged, whichever
// threads drafted them and whoever changed them: a draft or an edit past
// that is refused and changes nothing.
func TestADayHoldsAtMost24Hours(t *testing.T) {
	t.Parallel()
	d := newDraftScenario(t, "jira")
	web1, web2 := draftTickets["WEB-1"], draftTickets["WEB-2"]
	d.draft(t, d.on("WEB-1", 14, "Built the form"))
	over := d.day + " would hold 26h of entries not yet logged; a day holds at most 24h."
	_, err := d.svc.DraftTime(d.conn.ID, TimeDraftRequest{Thread: "thread-2", Call: "call-b",
		Entries: []TimeDraftEntry{{Day: d.day, Ticket: &web2, Hours: 12, Reason: "Reviewed"}}})
	if !errors.Is(err, ErrInvalidRequest) || !strings.HasSuffix(err.Error(), ": "+over) {
		t.Fatalf("thread-2's 12h beside thread-1's 14h: %v; want %q", err, over)
	}
	theirs := must[TimeDraftView](t)(d.svc.DraftTime(d.conn.ID, TimeDraftRequest{Thread: "thread-2", Call: "call-c",
		Entries: []TimeDraftEntry{{Day: d.day, Ticket: &web2, Hours: 8, Reason: "Reviewed"}}}))
	hours := 12.0
	if _, err := d.svc.EditTimeEntry(d.conn.ID, entryRefs(theirs.Made[0])[0], TimeEntryEdit{Hours: &hours}); !errors.Is(err, ErrInvalidRequest) ||
		!strings.HasSuffix(err.Error(), ": "+over) {
		t.Fatalf("an edit of WEB-2 to 12h beside WEB-1's 14h: %v; want %q", err, over)
	}
	all := must[[]TimeEntryView](t)(d.svc.ThreadTimeEntries("thread-2"))
	if len(all) != 1 || all[0].Minutes != 8*60 || all[0].Key != web2.Key {
		t.Fatalf("thread-2's entries are %+v; want WEB-2's 8h as drafted", all)
	}
	_ = web1
	if !slices.ContainsFunc(must[[]TimeEntryView](t)(d.svc.ThreadTimeEntries("thread-1")), func(e TimeEntryView) bool { return e.Minutes == 14*60 }) {
		t.Fatal("thread-1's 14h is gone")
	}
}
