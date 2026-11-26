package trackerapp

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// An entry whose post may have happened (unknown), as every path sees it:
// a run's read, remove, approval, log alone and settle, and the next run
// after the app stopped between the tracker's answer and the store's.

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
// run before it reads anything: its thread's entries show it unknown,
// neither a day's approval nor logging it alone sends it, and the person
// removes it at the revision read, all with no further worklog posted.
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
		d.posted(t, "WEB-2 logged alone on the next run", got, posts, "Posted 0 of 1 entries for "+d.label+". WEB-2, 1h, "+mayHave, 0)
		if e := d.entry(t, shown[0].ID); e.State != store.TimeEntryUnknown {
			t.Fatalf("WEB-2 is %+v after logging it alone; want it unknown", e)
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
// source: Tempo turned on from another window after the check, before the
// entry is marked and sent, is not where the entry goes; and a switch
// during the check's read sends nothing.
func TestADayIsCheckedAndPostedOnOneTimeSource(t *testing.T) {
	t.Parallel()
	d := newDraftScenario(t, "jira")
	v := d.draft(t, d.on("WEB-2", 1, "Reviewed the pull request"))
	tempo := d.addTempoOn(t)
	must[WriteView](t)(d.svc.AddWorklog(t.Context(), d.conn.ID, WorklogRequest{Issue: "10102", Day: d.day, SpentSeconds: 1800, Description: "Reviewed it by hand"}))
	must[Connection](t)(d.svc.SetEnabled(tempo.ID, false))
	switched := false
	d.marks.mu.Lock()
	d.marks.gate = func() {
		must[Connection](t)(d.svc.SetEnabled(tempo.ID, true))
		switched = true
	}
	d.marks.mu.Unlock()
	reqs := d.requestsOf(t, func() {
		_, err := d.svc.PostTimeEntries(t.Context(), d.conn.ID, PostTimeRequest{Entries: entryRefs(v.Made...)})
		t.Logf("the approval across the switch: %v", err)
	})
	if !switched || reqs[tempoWorklogPost] != 0 || reqs[jiraWorklogPost] != 1 {
		t.Fatalf("the approval across a switch after its check (switched %v) made %v; want one POST to Jira, which it checked, and none to Tempo, which holds time logged by hand on WEB-2", switched, reqs)
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

// Every read or change of entries that starts no run recovers an earlier
// run's sends first: called first on the next run, each sees the entry the
// run before left sent as unknown. q is a question of the same day.
func TestEveryEntryPathRecoversAnEarlierRunsSends(t *testing.T) {
	t.Parallel()
	for name, sees := range map[string]func(t *testing.T, d *draftScenario, left, q TimeEntryView) string{
		"the thread's entries": func(t *testing.T, d *draftScenario, _, _ TimeEntryView) string {
			return must[[]TimeEntryView](t)(d.svc.ThreadTimeEntries("thread-1"))[0].State
		},
		"the mirror read": func(t *testing.T, d *draftScenario, _, _ TimeEntryView) string {
			return must[WorklogDays](t)(d.svc.Worklogs(d.conn.ID, "acc-user1", d.day, d.day)).Drafted[0].State
		},
		"a draft of the day": func(t *testing.T, d *draftScenario, _, _ TimeEntryView) string {
			return d.draft(t, d.on("WEB-3", 0.5, "Triaged the backlog")).Kept[0].State
		},
		"a remove at the recovered revision": func(t *testing.T, d *draftScenario, left, _ TimeEntryView) string {
			if gone := must[[]TimeEntryView](t)(d.svc.RemoveTimeEntries(d.conn.ID, []TimeEntryRefView{{ID: left.ID, Revision: left.Revision + 1}})); len(gone) == 1 {
				return gone[0].State
			}
			return "kept"
		},
		"an answer onto its ticket": func(t *testing.T, d *draftScenario, _, q TimeEntryView) string {
			web2 := draftTickets["WEB-2"]
			_, err := d.svc.EditTimeEntry(d.conn.ID, entryRefs(q)[0], TimeEntryEdit{Ticket: &web2})
			if err != nil && strings.HasSuffix(err.Error(), mayHaveBeenLogged) {
				return store.TimeEntryUnknown
			}
			return fmt.Sprint(err)
		},
		"a day's approval": func(t *testing.T, d *draftScenario, left, _ TimeEntryView) string {
			got, _ := d.post(t, false, TimeEntryRefView{ID: left.ID, Revision: left.Revision + 1})
			if strings.HasSuffix(got.Sentence, mayHave) {
				return store.TimeEntryUnknown
			}
			return got.Sentence
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := newDraftScenario(t, "jira")
			v := d.draft(t, d.on("WEB-2", 1, "Reviewed the pull request"), TimeDraftEntry{Day: d.day, Hours: 0.5, Reason: "Standup"})
			d.crashAfterSend(t, entryRefs(v.Made[0])...)
			left := timeEntryViews([]store.TimeEntry{d.entry(t, v.Made[0].ID)})[0]
			d.restart()
			if got := sees(t, d, left, v.Made[1]); got != store.TimeEntryUnknown {
				t.Fatalf("%s, first on the next run, sees %s; want unknown", name, got)
			}
		})
	}
}

// A worklog whose creation time the tracker did not give counts as made
// since: the check errs toward a skip the person sees.
func TestAWorklogWithNoCreationTimeCountsAsLoggedSince(t *testing.T) {
	t.Parallel()
	day, err := tracker.ParseDate("2026-11-24")
	if err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC).UnixMilli()
	e := store.TimeEntry{IssueID: "10102", Day: "2026-11-24", CreatedMs: since}
	for _, tc := range []struct {
		name    string
		created time.Time
		want    bool
	}{
		{"no creation time", time.Time{}, true},
		{"created before, past the slack", time.UnixMilli(since - clockSlack - 1), false},
		{"created within the slack", time.UnixMilli(since - clockSlack), true},
	} {
		if got := loggedSince(e, []tracker.Worklog{{Issue: "10102", Day: day, Created: tc.created}}); got != tc.want {
			t.Errorf("%s: %v; want %v", tc.name, got, tc.want)
		}
	}
}

// An entry whose answer was lost is never sent again: no read here can
// prove its first send did not land. Neither logging it alone nor
// approving its day sends it, whether the day's read shows the worklog
// that send made or cannot: Jira's search not yet showing it, or the time
// source switched since. Each says what the person does instead.
func TestAnEntryThatMayBeLoggedIsNeverSentAgain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		source string
		edit   func(site *trackerfake.JiraSite, at time.Time)
		// after runs once the answer was lost.
		after func(t *testing.T, d *draftScenario)
	}{
		{"the worklog shown", "jira", nil, func(*testing.T, *draftScenario) {}},
		{"jira's search not showing the worklog", "jira", func(site *trackerfake.JiraSite, at time.Time) {
			for i := range site.Issues {
				if site.Issues[i].Key == "WEB-3" {
					site.Issues[i].Indexed = at.Add(24 * time.Hour)
				}
			}
		}, func(*testing.T, *draftScenario) {}},
		{"tempo turned off since", "tempo", nil, func(t *testing.T, d *draftScenario) {
			must[Connection](t)(d.svc.SetEnabled(d.tempoConn.ID, false))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newDraftScenarioWith(t, tc.source, tc.edit)
			v := d.draft(t, d.on("WEB-3", 1, "Triaged the backlog"))
			d.fault(t, trackerfake.Fault{Kind: trackerfake.FaultFailAfterApply, Times: 1})
			got, posts := d.post(t, false, entryRefs(v.Made...)...)
			d.posted(t, "the lost answer", got, posts, "Posted 0 of 1 entries for "+d.label+". WEB-3, 1h, "+mayHave, 1)
			tc.after(t, d)
			for _, alone := range []bool{true, false} {
				var reqs map[string]int
				reqs = d.requestsOf(t, func() {
					got = must[PostTimeView](t)(d.svc.PostTimeEntries(t.Context(), d.conn.ID, PostTimeRequest{Entries: entryRefs(got.Entries...), Alone: alone}))
				})
				if posts := reqs[jiraWorklogPost] + reqs[tempoWorklogPost]; posts != 0 || got.Sentence != "Posted 0 of 1 entries for "+d.label+". WEB-3, 1h, "+mayHave {
					t.Fatalf("approving it again (alone %v) answered %q with requests %v; want nothing sent", alone, got.Sentence, reqs)
				}
			}
			if e := d.entry(t, v.Made[0].ID); e.State != store.TimeEntryUnknown {
				t.Fatalf("WEB-3 is %+v; want it unknown", e)
			}
			if gone := must[[]TimeEntryView](t)(d.svc.RemoveTimeEntries(d.conn.ID, entryRefs(got.Entries...))); len(gone) != 1 {
				t.Fatalf("removing WEB-3 removed %+v", gone)
			}
		})
	}
}
