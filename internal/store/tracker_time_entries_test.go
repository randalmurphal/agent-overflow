package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// v173 adds tracker_time_entries: its checks refuse a row no writer may
// leave, one ticket has at most one open entry a day, and a connection's
// delete takes its entries.
func TestTimeEntriesMigration(t *testing.T) {
	db := migrateThrough(t, timeEntriesMigrationVersion)
	if _, err := db.Exec(`INSERT INTO tracker_connections(id, kind, site, account, created_at) VALUES ('j1', 'jira', 'example.atlassian.net', 'user1@example.com', 1)`); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO tracker_time_entries(connection_id, author_id, day, issue_id, issue_key, seconds, reason, offers, source, thread_id,
		state, revision, sent_ms, worklog_id, posted_ms, created_ms, updated_ms) VALUES ('j1', 'acc-user1', ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, 1, 1)`
	good := [][]any{
		{"2026-11-23", "10001", "WEB-1", 3600, "Review", "[]", "agent", "t1", "drafted", 0, "", 0},
		{"2026-11-23", "", "", 1800, "Which ticket?", `[{"issue":"10002","key":"WEB-2","title":"Two"}]`, "agent", "t1", "drafted", 0, "", 0},
		{"2026-11-23", "10001", "WEB-1", 600, "Earlier", "[]", "person", "", "posted", 5, "300", 6},
	}
	for _, args := range good {
		if _, err := db.Exec(insert, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if _, err := db.Exec(insert, "2026-11-23", "10001", "WEB-1", 60, "Again", "[]", "person", "", "drafted", 0, "", 0); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("a second open entry for WEB-1 that day: %v, want a UNIQUE failure", err)
	}
	for _, bad := range []struct {
		name string
		args []any
	}{
		{"a day that is no date", []any{"Nov 23", "10003", "WEB-3", 60, "r", "[]", "agent", "t1", "drafted", 0, "", 0}},
		{"seconds not whole minutes", []any{"2026-11-23", "10003", "WEB-3", 61, "r", "[]", "agent", "t1", "drafted", 0, "", 0}},
		{"over a day", []any{"2026-11-23", "10003", "WEB-3", 86460, "r", "[]", "agent", "t1", "drafted", 0, "", 0}},
		{"no reason", []any{"2026-11-23", "10003", "WEB-3", 60, "", "[]", "agent", "t1", "drafted", 0, "", 0}},
		{"an agent entry with no thread", []any{"2026-11-23", "10003", "WEB-3", 60, "r", "[]", "agent", "", "drafted", 0, "", 0}},
		{"a question sent", []any{"2026-11-23", "", "", 60, "r", "[]", "agent", "t1", "sent", 5, "", 0}},
		{"a ticket with offers", []any{"2026-11-23", "10003", "WEB-3", 60, "r", `[{"issue":"1"}]`, "agent", "t1", "drafted", 0, "", 0}},
		{"sent with no time", []any{"2026-11-23", "10003", "WEB-3", 60, "r", "[]", "agent", "t1", "sent", 0, "", 0}},
		{"posted with no time", []any{"2026-11-23", "10003", "WEB-3", 60, "r", "[]", "agent", "t1", "posted", 5, "1", 0}},
		{"a drafted entry naming a worklog", []any{"2026-11-23", "10003", "WEB-3", 60, "r", "[]", "agent", "t1", "drafted", 0, "1", 0}},
		{"an unknown state", []any{"2026-11-23", "10003", "WEB-3", 60, "r", "[]", "agent", "t1", "approved", 0, "", 0}},
		{"offers not a list", []any{"2026-11-23", "", "", 60, "r", "{}", "agent", "t1", "drafted", 0, "", 0}},
	} {
		if _, err := db.Exec(insert, bad.args...); err == nil || !strings.Contains(err.Error(), "CHECK") {
			t.Errorf("%s: %v, want a CHECK failure", bad.name, err)
		}
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM tracker_connections WHERE id = 'j1'`); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := db.QueryRow(`SELECT count(*) FROM tracker_time_entries`).Scan(&left); err != nil || left != 0 {
		t.Errorf("%d entries left after their connection was deleted (%v)", left, err)
	}
}

func timeStore(t *testing.T) (*Store, string) {
	t.Helper()
	s := newTestStore(t)
	c, err := s.CreateTrackerConnection(NewTrackerConnection{Kind: TrackerKindJira, Site: "example.atlassian.net", Account: "user1@example.com", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	return s, c.ID
}

func agentEntry(day, issue, key string, seconds int64, reason, thread string) TimeEntry {
	return TimeEntry{Day: day, IssueID: issue, IssueKey: key, Seconds: seconds, Reason: reason, ThreadID: thread, CallID: "call-" + thread}
}

// A draft replaces its own thread's drafted entries of the days it names
// and nothing else: another thread's, the person's and the sent ones stay,
// and a ticket that already has an open entry that day is skipped.
func TestReplaceAgentTimeDays(t *testing.T) {
	t.Parallel()
	s, conn := timeStore(t)
	first, err := s.ReplaceAgentTimeDays(conn, "acc-user1", []TimeEntry{
		agentEntry("2026-11-23", "10001", "WEB-1", 3600, "Review", "t1"),
		agentEntry("2026-11-23", "10002", "WEB-2", 1800, "Fix", "t1"),
		agentEntry("2026-11-24", "10001", "WEB-1", 1800, "More review", "t1"),
	}, 10)
	if err != nil || len(first.Made) != 3 || !first.Changed() {
		t.Fatalf("first draft %+v %v", first, err)
	}
	one, two := first.Made[0], first.Made[1]
	reason := "Fixed the login"
	if _, ok, err := s.EditTimeEntry(conn, "acc-user1", TimeEntryRef{two.ID, two.Revision}, TimeEntryChange{Reason: &reason}, 11); !ok || err != nil {
		t.Fatalf("the person's edit: %v %v", ok, err)
	}
	other, err := s.ReplaceAgentTimeDays(conn, "acc-user1", []TimeEntry{
		agentEntry("2026-11-23", "10003", "WEB-3", 900, "Standup", "t2"),
		agentEntry("2026-11-23", "10002", "WEB-2", 600, "Fix again", "t2"),
	}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(other.Replaced) != 0 {
		t.Errorf("t2's draft replaced %+v, want nothing of t1's", other.Replaced)
	}
	if len(other.Made) != 1 || other.Made[0].IssueKey != "WEB-3" || len(other.Skipped) != 1 || other.Skipped[0].IssueKey != "WEB-2" {
		t.Errorf("made %+v skipped %+v, want WEB-3 made and WEB-2 skipped", other.Made, other.Skipped)
	}
	if len(other.Kept) != 2 || other.Kept[0].ID != one.ID || other.Kept[1].ID != two.ID || other.Kept[1].Reason != reason {
		t.Errorf("kept %+v, want t1's WEB-1 and the person's WEB-2", other.Kept)
	}
	again, err := s.ReplaceAgentTimeDays(conn, "acc-user1", []TimeEntry{agentEntry("2026-11-23", "10004", "WEB-4", 600, "Docs", "t1")}, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Replaced) != 1 || again.Replaced[0].ID != one.ID {
		t.Errorf("t1's second draft replaced %+v, want its own WEB-1 of the 23rd", again.Replaced)
	}
	days, err := s.TimeEntriesOf(conn, "acc-user1", "2026-11-23", "2026-11-24")
	if err != nil || len(days) != 4 {
		t.Fatalf("entries %+v %v", days, err)
	}
	if days[3].Day != "2026-11-24" || days[3].IssueKey != "WEB-1" {
		t.Errorf("the 24th, not named, changed: %+v", days[3])
	}
}

// A draft deletes nothing it did not replace: a posted entry and a
// left-out question of a day long past stay, whatever day the draft is
// for.
func TestADraftDeletesNoSettledEntry(t *testing.T) {
	t.Parallel()
	s, conn := timeStore(t)
	old, err := s.ReplaceAgentTimeDays(conn, "acc-user1", []TimeEntry{
		agentEntry("2026-09-01", "", "", 600, "Which?", "t1"), agentEntry("2026-09-01", "10001", "WEB-1", 600, "Keep", "t1"),
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	q, p := old.Made[0], old.Made[1]
	if _, ok, err := s.EditTimeEntry(conn, "acc-user1", TimeEntryRef{q.ID, q.Revision}, TimeEntryChange{LeaveOut: true}, 2); !ok || err != nil {
		t.Fatalf("leave out: %v %v", ok, err)
	}
	marked, err := s.MarkTimeEntriesSent(conn, []TimeEntryRef{{p.ID, p.Revision}}, 3)
	if err != nil || len(marked.Entries) != 1 {
		t.Fatalf("mark: %+v %v", marked, err)
	}
	if _, err := s.SettleTimeEntries(conn, []TimeEntrySettle{{ID: p.ID, Revision: marked.Entries[0].Revision, State: TimeEntryPosted, WorklogID: "300"}}, 3); err != nil {
		t.Fatal(err)
	}
	for _, day := range []string{"2027-03-01", "2026-09-01"} {
		if _, err := s.ReplaceAgentTimeDays(conn, "acc-user1", []TimeEntry{agentEntry(day, "10002", "WEB-2", 600, "x", "t1")}, 4); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.TimeEntriesOf(conn, "acc-user1", "2026-09-01", "2026-09-01")
	if len(got) != 3 || got[0].State != TimeEntryLeftOut || got[1].State != TimeEntryPosted || got[1].WorklogID != "300" {
		t.Errorf("after later drafts %+v, want the left-out question and the posted entry kept beside the new one", got)
	}
}

// A mark and a remove start from the entries named: their cost does not
// grow with the connection's other entries.
func TestMarkAndRemoveReadTheNamedEntriesOnly(t *testing.T) {
	t.Parallel()
	s, conn := timeStore(t)
	for name, q := range map[string]struct {
		sql  string
		args []any
	}{
		"mark":           {markTimeEntriesSentSQL, []any{conn, "[[1,1]]", 5}},
		"mark's unknown": {unknownAtRefSQL, []any{conn, "[[1,1]]"}},
		"remove":         {removeTimeEntriesSQL, []any{conn, "[[1,1]]", "acc-user1"}},
		"read":           {timeEntriesByIDSQL, []any{conn, "[1]"}},
	} {
		plan := queryPlanText(t, s.db, "EXPLAIN QUERY PLAN "+q.sql, q.args...)
		if !strings.Contains(plan, "tracker_time_entries USING INTEGER PRIMARY KEY") || strings.Contains(plan, "idx_tracker_time_entries_day") {
			t.Errorf("%s plans %s; want it driven by the named IDs", name, plan)
		}
	}
}

// The person's edit and remove are conditional on the revision read, and
// a mark is too: a second window's approval of an entry another marked
// marks nothing; a settle is of the revision the mark left. An entry being
// sent is not edited or removed.
func TestTimeEntryTransitions(t *testing.T) {
	t.Parallel()
	s, conn := timeStore(t)
	w, err := s.ReplaceAgentTimeDays(conn, "acc-user1", []TimeEntry{
		agentEntry("2026-11-23", "10001", "WEB-1", 3600, "Review", "t1"),
		agentEntry("2026-11-23", "10002", "WEB-2", 1800, "Fix", "t1"),
		agentEntry("2026-11-23", "", "", 600, "Which?", "t1"),
	}, 10)
	if err != nil {
		t.Fatal(err)
	}
	one, two, q := w.Made[0], w.Made[1], w.Made[2]
	ref := func(e TimeEntry) TimeEntryRef { return TimeEntryRef{e.ID, e.Revision} }
	marked, err := s.MarkTimeEntriesSent(conn, []TimeEntryRef{ref(one), ref(two), ref(q)}, 100)
	if err != nil || len(marked.Entries) != 2 || len(marked.Unknown) != 0 {
		t.Fatalf("marked %+v %v, want the two tickets, neither unknown, and not the question", marked, err)
	}
	if again, err := s.MarkTimeEntriesSent(conn, []TimeEntryRef{ref(one)}, 101); err != nil || len(again.Entries) != 0 {
		t.Errorf("a second window's mark %+v %v, want nothing", again, err)
	}
	if again, err := s.MarkTimeEntriesSent(conn, []TimeEntryRef{ref(marked.Entries[0])}, 101); err != nil || len(again.Entries) != 0 {
		t.Errorf("a mark of an entry this run is sending %+v %v, want nothing", again, err)
	}
	if _, ok, _ := s.EditTimeEntry(conn, "acc-user1", ref(one), TimeEntryChange{LeaveOut: true}, 102); ok {
		t.Error("an edit of an entry being sent was applied")
	}
	if removed, _ := s.RemoveTimeEntries(conn, "acc-user1", []TimeEntryRef{ref(marked.Entries[1])}); len(removed) != 0 {
		t.Error("a remove of an entry being sent was applied")
	}
	if settled, err := s.SettleTimeEntries(conn, []TimeEntrySettle{{ID: one.ID, Revision: one.Revision, State: TimeEntryPosted, WorklogID: "300"}}, 103); err != nil || len(settled) != 0 {
		t.Fatalf("a settle at the revision before the mark settled %+v %v", settled, err)
	}
	settled, err := s.SettleTimeEntries(conn, []TimeEntrySettle{
		{ID: one.ID, Revision: marked.Entries[0].Revision, State: TimeEntryUnknown},
		{ID: two.ID, Revision: marked.Entries[1].Revision, State: TimeEntryDrafted, Refusal: "Tempo: the period is approved"},
	}, 103)
	if err != nil || len(settled) != 2 || settled[0].State != TimeEntryUnknown || settled[0].SentMs != 100 ||
		settled[1].State != TimeEntryDrafted || settled[1].SentMs != 0 || settled[1].Refusal == "" {
		t.Fatalf("settled %+v %v, want WEB-1 unknown, sent at 100, and WEB-2 drafted with its refusal", settled, err)
	}

	// A retry of the unknown entry is marked as it was first sent, and
	// told unknown; one that did nothing keeps that.
	unknown := settled[0]
	retry, err := s.MarkTimeEntriesSent(conn, []TimeEntryRef{ref(unknown)}, 200)
	if err != nil || len(retry.Entries) != 1 || !retry.Unknown[one.ID] || retry.Entries[0].SentMs != 100 {
		t.Fatalf("the retry's mark %+v %v, want WEB-1 marked, unknown before, sent at 100", retry, err)
	}
	if again, err := s.SettleTimeEntries(conn, []TimeEntrySettle{{ID: one.ID, Revision: retry.Entries[0].Revision, State: TimeEntryUnknown, Refusal: "closed"}}, 201); err != nil ||
		len(again) != 1 || again[0].State != TimeEntryUnknown || again[0].SentMs != 100 {
		t.Fatalf("the refused retry settled %+v %v, want WEB-1 unknown, sent at 100", again, err)
	}
	unknown, _, _ = timeEntryOf(s, conn, one.ID)
	retry, err = s.MarkTimeEntriesSent(conn, []TimeEntryRef{ref(unknown)}, 300)
	if err != nil || len(retry.Entries) != 1 {
		t.Fatalf("the second retry's mark %+v %v", retry, err)
	}
	if posted, err := s.SettleTimeEntries(conn, []TimeEntrySettle{{ID: one.ID, Revision: retry.Entries[0].Revision, State: TimeEntryPosted, WorklogID: "300"}}, 301); err != nil ||
		len(posted) != 1 || posted[0].State != TimeEntryPosted || posted[0].WorklogID != "300" || posted[0].PostedMs != 301 {
		t.Fatalf("posted %+v %v", posted, err)
	}
	stale := TimeEntryRef{q.ID, q.Revision + 1}
	if _, ok, err := s.EditTimeEntry(conn, "acc-user1", stale, TimeEntryChange{LeaveOut: true}, 104); ok || err != nil {
		t.Errorf("an edit at a revision never read: %v %v", ok, err)
	}
	answer := TimeOffer{Issue: "10001", Key: "WEB-1", Title: "One"}
	edited, ok, err := s.EditTimeEntry(conn, "acc-user1", ref(q), TimeEntryChange{Issue: &answer}, 105)
	got := edited.Entry
	if !ok || err != nil || got.ID != q.ID || edited.Merged != nil || got.IssueKey != "WEB-1" || got.Source != TimeSourcePerson || len(got.Offers) != 0 {
		t.Fatalf("answering the question with WEB-1, posted already: %+v %v %v", edited, ok, err)
	}
	other := TimeOffer{Issue: "10002", Key: "WEB-2", Title: "Two"}
	if _, _, err := s.EditTimeEntry(conn, "acc-user1", ref(got), TimeEntryChange{Issue: &other}, 106); !errors.Is(err, ErrTimeEntryOpen) {
		t.Errorf("moving it to WEB-2, drafted that day already: %v, want ErrTimeEntryOpen", err)
	}
	if removed, err := s.RemoveTimeEntries(conn, "acc-user1", []TimeEntryRef{ref(got)}); err != nil || len(removed) != 1 {
		t.Errorf("removed %+v %v", removed, err)
	}
}

// The recovery of a run's sends makes each sent entry unknown, keeping
// the time it was sent, and moves its revision; it changes no other entry,
// and the person then removes it. Its plan reads the open-entry index,
// not the table's settled history.
func TestRecoverEarlierTimeSends(t *testing.T) {
	t.Parallel()
	s, conn := timeStore(t)
	w, err := s.ReplaceAgentTimeDays(conn, "acc-user1", []TimeEntry{
		agentEntry("2026-11-23", "10001", "WEB-1", 3600, "Review", "t1"),
		agentEntry("2026-11-23", "10002", "WEB-2", 1800, "Fix", "t1"),
	}, 10)
	if err != nil {
		t.Fatal(err)
	}
	one, two := w.Made[0], w.Made[1]
	marked, err := s.MarkTimeEntriesSent(conn, []TimeEntryRef{{one.ID, one.Revision}}, 20)
	if err != nil || len(marked.Entries) != 1 {
		t.Fatalf("mark: %+v %v", marked, err)
	}
	got, err := s.RecoverEarlierTimeSends(30)
	if err != nil || len(got) != 1 || got[0].ID != one.ID || got[0].State != TimeEntryUnknown || got[0].SentMs != 20 ||
		got[0].Revision != marked.Entries[0].Revision+1 || got[0].UpdatedMs != 30 {
		t.Fatalf("recovered %+v %v, want WEB-1 unknown, sent at 20, at the next revision", got, err)
	}
	if again, err := s.RecoverEarlierTimeSends(40); err != nil || len(again) != 0 {
		t.Fatalf("a second recovery %+v %v, want nothing", again, err)
	}
	if e, _, _ := timeEntryOf(s, conn, two.ID); e.Revision != two.Revision || e.State != TimeEntryDrafted {
		t.Fatalf("the recovery changed WEB-2: %+v", e)
	}
	if removed, err := s.RemoveTimeEntries(conn, "acc-user1", []TimeEntryRef{{got[0].ID, got[0].Revision}}); err != nil || len(removed) != 1 {
		t.Fatalf("removed %+v %v, want the recovered entry", removed, err)
	}
	if plan := queryPlanText(t, s.db, "EXPLAIN QUERY PLAN "+recoverTimeSendsSQL, 1); !strings.Contains(plan, "idx_tracker_time_entries_open") {
		t.Errorf("the recovery plans %s; want it to read the open-entry index", plan)
	}
}

// A day's entries not yet logged hold at most 24 hours: a draft or an
// edit that would pass it fails with the day and its total and writes
// nothing; posted and left-out entries do not count.
func TestADayHoldsAtMost24HoursOfOpenEntries(t *testing.T) {
	t.Parallel()
	s, conn := timeStore(t)
	w, err := s.ReplaceAgentTimeDays(conn, "acc-user1", []TimeEntry{agentEntry("2026-11-23", "10001", "WEB-1", 14*3600, "Review", "t1")}, 10)
	if err != nil {
		t.Fatal(err)
	}
	var over *TimeDayOverError
	_, err = s.ReplaceAgentTimeDays(conn, "acc-user1", []TimeEntry{agentEntry("2026-11-23", "10002", "WEB-2", 12*3600, "Fix", "t2")}, 20)
	if !errors.As(err, &over) || !errors.Is(err, ErrTimeDayOver) || over.Day != "2026-11-23" || over.Seconds != 26*3600 {
		t.Fatalf("t2's 12h beside t1's 14h: %v", err)
	}
	theirs, err := s.ReplaceAgentTimeDays(conn, "acc-user1", []TimeEntry{agentEntry("2026-11-23", "10002", "WEB-2", 8*3600, "Fix", "t2")}, 30)
	if err != nil {
		t.Fatal(err)
	}
	twelve := int64(12 * 3600)
	e := theirs.Made[0]
	if _, _, err := s.EditTimeEntry(conn, "acc-user1", TimeEntryRef{e.ID, e.Revision}, TimeEntryChange{Seconds: &twelve}, 40); !errors.As(err, &over) || over.Seconds != 26*3600 {
		t.Fatalf("an edit of WEB-2 to 12h: %v", err)
	}
	if got, _, _ := timeEntryOf(s, conn, e.ID); got.Seconds != 8*3600 || got.Revision != e.Revision {
		t.Fatalf("the refused edit changed WEB-2: %+v", got)
	}
	// A posted WEB-1 no longer counts.
	one := w.Made[0]
	m, err := s.MarkTimeEntriesSent(conn, []TimeEntryRef{{one.ID, one.Revision}}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettleTimeEntries(conn, []TimeEntrySettle{{ID: one.ID, Revision: m.Entries[0].Revision, State: TimeEntryPosted, WorklogID: "1"}}, 51); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.EditTimeEntry(conn, "acc-user1", TimeEntryRef{e.ID, e.Revision}, TimeEntryChange{Seconds: &twelve}, 60); !ok || err != nil {
		t.Fatalf("an edit of WEB-2 to 12h beside a posted WEB-1: %v %v", ok, err)
	}
}

// A question answered with a ticket that has a drafted entry that day
// adds its time and its reason to that entry, which becomes the person's,
// and is deleted, in one transaction. An entry of that ticket being sent
// or of unknown outcome refuses the answer, as a drafted one refuses a
// plain edit onto its ticket, and so does a merge past a day or past
// MaxTimeEntryReason; each refusal changes nothing.
func TestAnAnsweredQuestionMergesIntoItsTicketsEntry(t *testing.T) {
	t.Parallel()
	s, conn := timeStore(t)
	w, err := s.ReplaceAgentTimeDays(conn, "acc-user1", []TimeEntry{
		agentEntry("2026-11-23", "10001", "WEB-1", 3600, "Built the form.", "t1"),
		agentEntry("2026-11-23", "10002", "WEB-2", 1800, "Fix", "t1"),
		agentEntry("2026-11-23", "10003", "WEB-3", 600, "Triage", "t1"),
	}, 10)
	if err != nil {
		t.Fatal(err)
	}
	q, err := s.ReplaceAgentTimeDays(conn, "acc-user1", []TimeEntry{
		agentEntry("2026-11-23", "", "", 1500, "25 minute call at 14:05", "t2"),
		agentEntry("2026-11-23", "", "", 600, "Standup", "t2"),
	}, 10)
	if err != nil {
		t.Fatal(err)
	}
	web1, web2, web3, call, standup := w.Made[0], w.Made[1], w.Made[2], q.Made[0], q.Made[1]
	ref := func(e TimeEntry) TimeEntryRef { return TimeEntryRef{e.ID, e.Revision} }
	if _, err := s.MarkTimeEntriesSent(conn, []TimeEntryRef{ref(web2)}, 20); err != nil {
		t.Fatal(err)
	}
	ticket := func(e TimeEntry) *TimeOffer { return &TimeOffer{Issue: e.IssueID, Key: e.IssueKey, Title: "T"} }
	unchanged := func(what string, es ...TimeEntry) {
		t.Helper()
		for _, e := range es {
			now, ok, err := timeEntryOf(s, conn, e.ID)
			if err != nil || !ok || now.Revision != e.Revision {
				t.Fatalf("%s changed entry %d: %+v %v %v", what, e.ID, now, ok, err)
			}
		}
	}

	edited, ok, err := s.EditTimeEntry(conn, "acc-user1", ref(call), TimeEntryChange{Issue: ticket(web1)}, 30)
	m := edited.Entry
	if err != nil || !ok || m.ID != web1.ID || m.Seconds != 5100 || m.Reason != "Built the form, plus 25 minute call at 14:05" ||
		m.Source != TimeSourcePerson || m.Revision != web1.Revision+1 || m.ThreadID != "t1" || edited.Merged == nil || edited.Merged.ID != call.ID {
		t.Fatalf("answering the call with WEB-1: %+v %v %v", edited, ok, err)
	}
	if _, found, err := timeEntryOf(s, conn, call.ID); found || err != nil {
		t.Fatalf("the merged question is still stored: %v %v", found, err)
	}
	if _, ok, err := s.EditTimeEntry(conn, "acc-user1", ref(call), TimeEntryChange{Issue: ticket(web1)}, 31); ok || err != nil {
		t.Fatalf("answering the merged question again: %v %v", ok, err)
	}

	var open *TimeEntryOpenError
	if _, _, err := s.EditTimeEntry(conn, "acc-user1", ref(standup), TimeEntryChange{Issue: ticket(web2)}, 32); !errors.As(err, &open) ||
		!errors.Is(err, ErrTimeEntryOpen) || open.Open.ID != web2.ID || open.Open.State != TimeEntrySent {
		t.Fatalf("answering with WEB-2, being sent: %v", err)
	}
	if _, _, err := s.EditTimeEntry(conn, "acc-user1", ref(web3), TimeEntryChange{Issue: ticket(web1)}, 33); !errors.As(err, &open) || open.Open.ID != web1.ID {
		t.Fatalf("moving WEB-3's entry onto WEB-1's: %v", err)
	}
	day := int64(23 * 3600)
	var over *TimeDayOverError
	if _, _, err := s.EditTimeEntry(conn, "acc-user1", ref(standup), TimeEntryChange{Issue: ticket(m), Seconds: &day}, 34); !errors.As(err, &over) ||
		over.Seconds != 5100+1800+600+day {
		t.Fatalf("a merge past a day: %v", err)
	}
	long := strings.Repeat("x", MaxTimeEntryReason-len(", plus Standup")+1)
	if _, _, err := s.EditTimeEntry(conn, "acc-user1", ref(m), TimeEntryChange{Reason: &long}, 35); err != nil {
		t.Fatal(err)
	}
	m, _, _ = timeEntryOf(s, conn, m.ID)
	if _, _, err := s.EditTimeEntry(conn, "acc-user1", ref(standup), TimeEntryChange{Issue: ticket(m)}, 36); !errors.Is(err, ErrTimeEntryMergeOver) {
		t.Fatalf("a merge past the longest reason: %v", err)
	}
	unchanged("the refused edits", m, standup, web3)
}

// A restore keeps the time draft, so a posted entry is not offered again,
// and deletes the entries of a connection the snapshot lacks.
func TestRestoreKeepsTheTimeDraft(t *testing.T) {
	t.Parallel()
	s, conn := timeStore(t)
	path := filepath.Join(t.TempDir(), "snap.db")
	if err := s.SnapshotTo(path); err != nil {
		t.Fatal(err)
	}
	later, err := s.CreateTrackerConnection(NewTrackerConnection{Kind: TrackerKindJira, Site: "other.atlassian.net", Account: "user1@example.com", CreatedAt: 2})
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.ReplaceAgentTimeDays(conn, "acc-user1", []TimeEntry{agentEntry("2026-11-23", "10001", "WEB-1", 3600, "Review", "t1")}, 10)
	if err != nil {
		t.Fatal(err)
	}
	e := w.Made[0]
	m, err := s.MarkTimeEntriesSent(conn, []TimeEntryRef{{e.ID, e.Revision}}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettleTimeEntries(conn, []TimeEntrySettle{{ID: e.ID, Revision: m.Entries[0].Revision, State: TimeEntryPosted, WorklogID: "300"}}, 21); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceAgentTimeDays(later.ID, "acc-user1", []TimeEntry{agentEntry("2026-11-23", "10001", "WEB-1", 600, "x", "t1")}, 30); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreFrom(path); err != nil {
		t.Fatal(err)
	}
	kept, err := s.TimeEntriesOf(conn, "acc-user1", "2026-11-23", "2026-11-23")
	if err != nil || len(kept) != 1 || kept[0].State != TimeEntryPosted || kept[0].WorklogID != "300" {
		t.Errorf("after the restore %+v %v, want the posted entry as it was", kept, err)
	}
	if gone, _ := s.TimeEntriesOf(later.ID, "acc-user1", "2026-11-23", "2026-11-23"); len(gone) != 0 {
		t.Errorf("the entries of a connection the snapshot lacks stayed: %+v", gone)
	}
	next, err := s.ReplaceAgentTimeDays(conn, "acc-user1", []TimeEntry{agentEntry("2026-11-24", "10001", "WEB-1", 600, "y", "t1")}, 40)
	if err != nil || next.Made[0].ID <= e.ID+1 {
		t.Errorf("an entry made after the restore has ID %v (%v); want past every ID given before it", next.Made, err)
	}
}

// timeEntryOf is entry id on connection; false when there is none.
func timeEntryOf(s *Store, connection string, id int64) (TimeEntry, bool, error) {
	got, err := s.TimeEntriesByID(connection, []int64{id})
	if err != nil || len(got) == 0 {
		return TimeEntry{}, false, err
	}
	return got[0], true, nil
}
