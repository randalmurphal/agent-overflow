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
		state, revision, sent_ms, sent_to, worklog_id, posted_ms, created_ms, updated_ms) VALUES ('j1', 'acc-user1', ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, 1, 1)`
	good := [][]any{
		{"2026-11-23", "10001", "WEB-1", 3600, "Review", "[]", "agent", "t1", "drafted", 0, "", "", 0},
		{"2026-11-23", "", "", 1800, "Which ticket?", `[{"issue":"10002","key":"WEB-2","title":"Two"}]`, "agent", "t1", "drafted", 0, "", "", 0},
		{"2026-11-23", "10001", "WEB-1", 600, "Earlier", "[]", "person", "", "posted", 5, "j1", "300", 6},
	}
	for _, args := range good {
		if _, err := db.Exec(insert, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if _, err := db.Exec(insert, "2026-11-23", "10001", "WEB-1", 60, "Again", "[]", "person", "", "drafted", 0, "", "", 0); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("a second open entry for WEB-1 that day: %v, want a UNIQUE failure", err)
	}
	for _, bad := range []struct {
		name string
		args []any
	}{
		{"a day that is no date", []any{"Nov 23", "10003", "WEB-3", 60, "r", "[]", "agent", "t1", "drafted", 0, "", "", 0}},
		{"seconds not whole minutes", []any{"2026-11-23", "10003", "WEB-3", 61, "r", "[]", "agent", "t1", "drafted", 0, "", "", 0}},
		{"over a day", []any{"2026-11-23", "10003", "WEB-3", 86460, "r", "[]", "agent", "t1", "drafted", 0, "", "", 0}},
		{"no reason", []any{"2026-11-23", "10003", "WEB-3", 60, "", "[]", "agent", "t1", "drafted", 0, "", "", 0}},
		{"an agent entry with no thread", []any{"2026-11-23", "10003", "WEB-3", 60, "r", "[]", "agent", "", "drafted", 0, "", "", 0}},
		{"a question sent", []any{"2026-11-23", "", "", 60, "r", "[]", "agent", "t1", "sent", 5, "j1", "", 0}},
		{"a ticket with offers", []any{"2026-11-23", "10003", "WEB-3", 60, "r", `[{"issue":"1"}]`, "agent", "t1", "drafted", 0, "", "", 0}},
		{"sent with no time", []any{"2026-11-23", "10003", "WEB-3", 60, "r", "[]", "agent", "t1", "sent", 0, "j1", "", 0}},
		{"sent nowhere", []any{"2026-11-23", "10003", "WEB-3", 60, "r", "[]", "agent", "t1", "sent", 5, "", "", 0}},
		{"posted with no time", []any{"2026-11-23", "10003", "WEB-3", 60, "r", "[]", "agent", "t1", "posted", 5, "j1", "1", 0}},
		{"a drafted entry naming a worklog", []any{"2026-11-23", "10003", "WEB-3", 60, "r", "[]", "agent", "t1", "drafted", 0, "", "1", 0}},
		{"an unknown state", []any{"2026-11-23", "10003", "WEB-3", 60, "r", "[]", "agent", "t1", "approved", 0, "", "", 0}},
		{"offers not a list", []any{"2026-11-23", "", "", 60, "r", "{}", "agent", "t1", "drafted", 0, "", "", 0}},
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

// A draft replaces the agent's drafted entries of the days it names,
// whichever thread wrote them, keeps the person's and the sent ones, and
// skips a ticket that already has an open entry that day.
func TestReplaceAgentTimeDays(t *testing.T) {
	t.Parallel()
	s, conn := timeStore(t)
	first, err := s.ReplaceAgentTimeDays(conn, "acc-user1", "2026-11-26", []TimeEntry{
		agentEntry("2026-11-23", "10001", "WEB-1", 3600, "Review", "t1"),
		agentEntry("2026-11-23", "10002", "WEB-2", 1800, "Fix", "t1"),
		agentEntry("2026-11-24", "10001", "WEB-1", 1800, "More review", "t1"),
	}, 10)
	if err != nil || len(first.Made) != 3 || !first.Changed() {
		t.Fatalf("first draft %+v %v", first, err)
	}
	two := first.Made[1]
	reason := "Fixed the login"
	if _, ok, err := s.EditTimeEntry(conn, TimeEntryRef{two.ID, two.Revision}, TimeEntryChange{Reason: &reason}, 11); !ok || err != nil {
		t.Fatalf("the person's edit: %v %v", ok, err)
	}
	again, err := s.ReplaceAgentTimeDays(conn, "acc-user1", "2026-11-26", []TimeEntry{
		agentEntry("2026-11-23", "10003", "WEB-3", 900, "Standup", "t2"),
		agentEntry("2026-11-23", "10002", "WEB-2", 600, "Fix again", "t2"),
	}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Replaced) != 1 || again.Replaced[0].IssueKey != "WEB-1" || again.Replaced[0].ThreadID != "t1" {
		t.Errorf("replaced %+v, want t1's WEB-1 of the 23rd", again.Replaced)
	}
	if len(again.Made) != 1 || again.Made[0].IssueKey != "WEB-3" || len(again.Skipped) != 1 || again.Skipped[0].IssueKey != "WEB-2" {
		t.Errorf("made %+v skipped %+v, want WEB-3 made and WEB-2 skipped", again.Made, again.Skipped)
	}
	if len(again.Kept) != 1 || again.Kept[0].ID != two.ID || again.Kept[0].Source != TimeSourcePerson || again.Kept[0].Reason != reason {
		t.Errorf("kept %+v, want the person's WEB-2", again.Kept)
	}
	days, err := s.TimeEntriesOf(conn, "acc-user1", "2026-11-23", "2026-11-24")
	if err != nil || len(days) != 3 {
		t.Fatalf("entries %+v %v", days, err)
	}
	if days[2].Day != "2026-11-24" || days[2].IssueKey != "WEB-1" {
		t.Errorf("the 24th, not named, changed: %+v", days[2])
	}
}

// A posted or left-out entry is kept 62 days after its day; the draft
// write of its author deletes it after that.
func TestTimeEntryRetention(t *testing.T) {
	t.Parallel()
	s, conn := timeStore(t)
	old, err := s.ReplaceAgentTimeDays(conn, "acc-user1", "2026-09-01", []TimeEntry{
		agentEntry("2026-09-01", "", "", 600, "Which?", "t1"), agentEntry("2026-09-01", "10001", "WEB-1", 600, "Keep", "t1"),
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	q := old.Made[0]
	if _, ok, err := s.EditTimeEntry(conn, TimeEntryRef{q.ID, q.Revision}, TimeEntryChange{LeaveOut: true}, 2); !ok || err != nil {
		t.Fatalf("leave out: %v %v", ok, err)
	}
	if _, err := s.ReplaceAgentTimeDays(conn, "acc-user1", "2026-11-02", []TimeEntry{agentEntry("2026-11-02", "10002", "WEB-2", 600, "x", "t1")}, 3); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.TimeEntriesOf(conn, "acc-user1", "2026-09-01", "2026-09-01"); len(got) != 2 {
		t.Fatalf("61 days later %d entries, want both", len(got))
	}
	if _, err := s.ReplaceAgentTimeDays(conn, "acc-user1", "2026-11-03", []TimeEntry{agentEntry("2026-11-03", "10002", "WEB-2", 600, "x", "t1")}, 4); err != nil {
		t.Fatal(err)
	}
	got, _ := s.TimeEntriesOf(conn, "acc-user1", "2026-09-01", "2026-09-01")
	if len(got) != 1 || got[0].State != TimeEntryDrafted {
		t.Errorf("63 days later %+v, want the left-out question gone and the drafted entry kept", got)
	}
}

// The person's edit and remove are conditional on the revision read, and
// a mark is too: a second window's approval of an entry another marked
// marks nothing; settling needs the mark's time.
func TestTimeEntryTransitions(t *testing.T) {
	t.Parallel()
	s, conn := timeStore(t)
	w, err := s.ReplaceAgentTimeDays(conn, "acc-user1", "2026-11-26", []TimeEntry{
		agentEntry("2026-11-23", "10001", "WEB-1", 3600, "Review", "t1"),
		agentEntry("2026-11-23", "10002", "WEB-2", 1800, "Fix", "t1"),
		agentEntry("2026-11-23", "", "", 600, "Which?", "t1"),
	}, 10)
	if err != nil {
		t.Fatal(err)
	}
	one, two, q := w.Made[0], w.Made[1], w.Made[2]
	ref := func(e TimeEntry) TimeEntryRef { return TimeEntryRef{e.ID, e.Revision} }
	marked, err := s.MarkTimeEntriesSent(conn, []TimeEntryRef{ref(one), ref(two), ref(q)}, "tempo-1", 100, 50)
	if err != nil || len(marked) != 2 {
		t.Fatalf("marked %+v %v, want the two tickets and not the question", marked, err)
	}
	if again, err := s.MarkTimeEntriesSent(conn, []TimeEntryRef{ref(one)}, "tempo-1", 101, 50); err != nil || len(again) != 0 {
		t.Errorf("a second window's mark %+v %v, want nothing", again, err)
	}
	if again, err := s.MarkTimeEntriesSent(conn, []TimeEntryRef{ref(marked[0])}, "tempo-1", 101, 50); err != nil || len(again) != 0 {
		t.Errorf("a mark of an entry this run is sending %+v %v, want nothing", again, err)
	}
	if again, err := s.MarkTimeEntriesSent(conn, []TimeEntryRef{ref(marked[0])}, "tempo-1", 300, 200); err != nil || len(again) != 1 {
		t.Errorf("a mark of an entry an earlier run left sent %+v %v, want it marked", again, err)
	}
	if _, ok, _ := s.EditTimeEntry(conn, ref(one), TimeEntryChange{LeaveOut: true}, 102); ok {
		t.Error("an edit of an entry being sent was applied")
	}
	if removed, _ := s.RemoveTimeEntries(conn, []TimeEntryRef{ref(marked[1])}); len(removed) != 0 {
		t.Error("a remove of an entry being sent was applied")
	}
	settled, err := s.SettleTimeEntries(conn, 100, []TimeEntrySettle{
		{ID: one.ID, State: TimeEntryPosted, WorklogID: "300"}, {ID: two.ID, State: TimeEntryDrafted, Refusal: "Tempo: the period is approved"},
	}, 103)
	if err != nil || len(settled) != 1 || settled[0].ID != two.ID || settled[0].State != TimeEntryDrafted || settled[0].Refusal == "" {
		t.Fatalf("settled %+v %v, want only WEB-2, back to drafted with its refusal (WEB-1 was marked again at 300)", settled, err)
	}
	if settled, err := s.SettleTimeEntries(conn, 300, []TimeEntrySettle{{ID: one.ID, State: TimeEntryPosted, WorklogID: "300"}}, 301); err != nil ||
		len(settled) != 1 || settled[0].State != TimeEntryPosted || settled[0].WorklogID != "300" || settled[0].PostedMs != 301 {
		t.Fatalf("posted %+v %v", settled, err)
	}
	stale := TimeEntryRef{q.ID, q.Revision + 1}
	if _, ok, err := s.EditTimeEntry(conn, stale, TimeEntryChange{LeaveOut: true}, 104); ok || err != nil {
		t.Errorf("an edit at a revision never read: %v %v", ok, err)
	}
	answer := TimeOffer{Issue: "10001", Key: "WEB-1", Title: "One"}
	edited, ok, err := s.EditTimeEntry(conn, ref(q), TimeEntryChange{Issue: &answer}, 105)
	got := edited.Entry
	if !ok || err != nil || got.ID != q.ID || edited.Merged != nil || got.IssueKey != "WEB-1" || got.Source != TimeSourcePerson || len(got.Offers) != 0 {
		t.Fatalf("answering the question with WEB-1, posted already: %+v %v %v", edited, ok, err)
	}
	other := TimeOffer{Issue: "10002", Key: "WEB-2", Title: "Two"}
	if _, _, err := s.EditTimeEntry(conn, ref(got), TimeEntryChange{Issue: &other}, 106); !errors.Is(err, ErrTimeEntryOpen) {
		t.Errorf("moving it to WEB-2, drafted that day already: %v, want ErrTimeEntryOpen", err)
	}
	if removed, err := s.RemoveTimeEntries(conn, []TimeEntryRef{ref(got)}); err != nil || len(removed) != 1 {
		t.Errorf("removed %+v %v", removed, err)
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
	w, err := s.ReplaceAgentTimeDays(conn, "acc-user1", "2026-11-26", []TimeEntry{
		agentEntry("2026-11-23", "10001", "WEB-1", 3600, "Built the form.", "t1"),
		agentEntry("2026-11-23", "10002", "WEB-2", 1800, "Fix", "t1"),
		agentEntry("2026-11-23", "10003", "WEB-3", 600, "Triage", "t1"),
		agentEntry("2026-11-23", "", "", 1500, "25 minute call at 14:05", "t2"),
		agentEntry("2026-11-23", "", "", 600, "Standup", "t2"),
		agentEntry("2026-11-23", "", "", 82800, "Offsite", "t2"),
	}, 10)
	if err != nil {
		t.Fatal(err)
	}
	web1, web2, web3, call, standup, offsite := w.Made[0], w.Made[1], w.Made[2], w.Made[3], w.Made[4], w.Made[5]
	ref := func(e TimeEntry) TimeEntryRef { return TimeEntryRef{e.ID, e.Revision} }
	if _, err := s.MarkTimeEntriesSent(conn, []TimeEntryRef{ref(web2)}, "tempo-1", 20, 5); err != nil {
		t.Fatal(err)
	}
	ticket := func(e TimeEntry) *TimeOffer { return &TimeOffer{Issue: e.IssueID, Key: e.IssueKey, Title: "T"} }
	unchanged := func(what string, es ...TimeEntry) {
		t.Helper()
		for _, e := range es {
			now, ok, err := s.TimeEntry(conn, e.ID)
			if err != nil || !ok || now.Revision != e.Revision {
				t.Fatalf("%s changed entry %d: %+v %v %v", what, e.ID, now, ok, err)
			}
		}
	}

	edited, ok, err := s.EditTimeEntry(conn, ref(call), TimeEntryChange{Issue: ticket(web1)}, 30)
	m := edited.Entry
	if err != nil || !ok || m.ID != web1.ID || m.Seconds != 5100 || m.Reason != "Built the form, plus 25 minute call at 14:05" ||
		m.Source != TimeSourcePerson || m.Revision != web1.Revision+1 || m.ThreadID != "t1" || edited.Merged == nil || edited.Merged.ID != call.ID {
		t.Fatalf("answering the call with WEB-1: %+v %v %v", edited, ok, err)
	}
	if _, found, err := s.TimeEntry(conn, call.ID); found || err != nil {
		t.Fatalf("the merged question is still stored: %v %v", found, err)
	}
	if _, ok, err := s.EditTimeEntry(conn, ref(call), TimeEntryChange{Issue: ticket(web1)}, 31); ok || err != nil {
		t.Fatalf("answering the merged question again: %v %v", ok, err)
	}

	var open *TimeEntryOpenError
	if _, _, err := s.EditTimeEntry(conn, ref(standup), TimeEntryChange{Issue: ticket(web2)}, 32); !errors.As(err, &open) ||
		!errors.Is(err, ErrTimeEntryOpen) || open.Open.ID != web2.ID || open.Open.State != TimeEntrySent {
		t.Fatalf("answering with WEB-2, being sent: %v", err)
	}
	if _, _, err := s.EditTimeEntry(conn, ref(web3), TimeEntryChange{Issue: ticket(web1)}, 33); !errors.As(err, &open) || open.Open.ID != web1.ID {
		t.Fatalf("moving WEB-3's entry onto WEB-1's: %v", err)
	}
	if _, _, err := s.EditTimeEntry(conn, ref(offsite), TimeEntryChange{Issue: ticket(m)}, 34); !errors.Is(err, ErrTimeEntryMergeOver) {
		t.Fatalf("a merge past a day: %v", err)
	}
	long := strings.Repeat("x", MaxTimeEntryReason-len(", plus Standup")+1)
	if _, _, err := s.EditTimeEntry(conn, ref(m), TimeEntryChange{Reason: &long}, 35); err != nil {
		t.Fatal(err)
	}
	m, _, _ = s.TimeEntry(conn, m.ID)
	if _, _, err := s.EditTimeEntry(conn, ref(standup), TimeEntryChange{Issue: ticket(m)}, 36); !errors.Is(err, ErrTimeEntryMergeOver) {
		t.Fatalf("a merge past the longest reason: %v", err)
	}
	unchanged("the refused edits", m, standup, web3, offsite)
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
	w, err := s.ReplaceAgentTimeDays(conn, "acc-user1", "2026-11-26", []TimeEntry{agentEntry("2026-11-23", "10001", "WEB-1", 3600, "Review", "t1")}, 10)
	if err != nil {
		t.Fatal(err)
	}
	e := w.Made[0]
	if _, err := s.MarkTimeEntriesSent(conn, []TimeEntryRef{{e.ID, e.Revision}}, conn, 20, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettleTimeEntries(conn, 20, []TimeEntrySettle{{ID: e.ID, State: TimeEntryPosted, WorklogID: "300"}}, 21); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceAgentTimeDays(later.ID, "acc-user1", "2026-11-26", []TimeEntry{agentEntry("2026-11-23", "10001", "WEB-1", 600, "x", "t1")}, 30); err != nil {
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
	next, err := s.ReplaceAgentTimeDays(conn, "acc-user1", "2026-11-26", []TimeEntry{agentEntry("2026-11-24", "10001", "WEB-1", 600, "y", "t1")}, 40)
	if err != nil || next.Made[0].ID <= e.ID+1 {
		t.Errorf("an entry made after the restore has ID %v (%v); want past every ID given before it", next.Made, err)
	}
}
