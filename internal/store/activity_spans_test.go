package store

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// v174 adds activity_spans and thread_days: a span is at least 0 ms long
// and phone is 0 or 1; a thread day names a day and a thread, once.
func TestActivityMigration(t *testing.T) {
	db := migrateThrough(t, activityMigrationVersion)
	mustExec(t, db, `INSERT INTO activity_spans(thread_id, phone, start_ms, end_ms) VALUES ('', 0, 10, 10)`)
	mustExec(t, db, `INSERT INTO thread_days(day, thread_id) VALUES ('2026-11-24', 't1')`)
	for name, q := range map[string]string{
		"a span that ends first": `INSERT INTO activity_spans(thread_id, phone, start_ms, end_ms) VALUES ('', 0, 10, 9)`,
		"phone 2":                `INSERT INTO activity_spans(thread_id, phone, start_ms, end_ms) VALUES ('', 2, 10, 11)`,
		"no day":                 `INSERT INTO thread_days(day, thread_id) VALUES ('Tuesday', 't1')`,
		"no thread":              `INSERT INTO thread_days(day, thread_id) VALUES ('2026-11-24', '')`,
		"a thread day twice":     `INSERT INTO thread_days(day, thread_id) VALUES ('2026-11-24', 't1')`,
	} {
		if _, err := db.Exec(q); err == nil {
			t.Errorf("%s was stored", name)
		}
	}
}

// activityStore is a store with threads t1 and t2, and a Jira connection
// on when jira.
func activityStore(t *testing.T, jira bool) *Store {
	t.Helper()
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	if _, err := s.CreateProject(Project{ID: "p1", Path: "/ws/p1", Name: "P", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"t1", "t2"} {
		if err := s.CreateThread(Thread{ID: id, ProjectID: "p1", Title: id, Provider: "claude", WorkspacePath: "/ws/p1", Mode: "chat",
			RuntimeMode: "approval-required", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if jira {
		if _, err := s.CreateTrackerConnection(NewTrackerConnection{Kind: TrackerKindJira, Site: "example.atlassian.net", Account: "user1@example.com", CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// A span is inserted while a Jira connection is on and its end moves on
// a later save; a read of a range answers the spans that reach it,
// through the start index bounded on both sides. With no Jira connection on, nothing is inserted.
func TestActivitySpansAreSavedAndRead(t *testing.T) {
	t.Parallel()
	s := activityStore(t, true)
	const h = 3_600_000
	day := int64(1_795_000_000_000)
	ids, err := s.SaveActivitySpans([]ActivitySpan{
		{ThreadID: "t1", StartMs: day, EndMs: day + h},
		{ThreadID: "t2", Phone: true, StartMs: day + 2*h, EndMs: day + 3*h},
		{StartMs: day - MaxActivitySpanMs - 1, EndMs: day - 1},
		{ThreadID: "t1", StartMs: day + 30*h, EndMs: day + 31*h},
	})
	if err != nil || len(ids) != 4 || slices.Contains(ids, 0) {
		t.Fatalf("saved %v, %v", ids, err)
	}
	if _, err := s.SaveActivitySpans([]ActivitySpan{{ID: ids[0], ThreadID: "t1", StartMs: day, EndMs: day + 90*60_000}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ActivitySpans(day, day+24*h)
	if err != nil {
		t.Fatal(err)
	}
	want := []ActivitySpan{
		{ID: ids[0], ThreadID: "t1", StartMs: day, EndMs: day + 90*60_000},
		{ID: ids[1], ThreadID: "t2", Phone: true, StartMs: day + 2*h, EndMs: day + 3*h},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("read %+v\nwant %+v", got, want)
	}
	if got, _ := s.ActivitySpans(day-h, day); len(got) != 1 || got[0].EndMs != day-1 {
		t.Fatalf("the hour before reads %+v; want the span that ends in it", got)
	}
	plan := queryPlanText(t, s.db, "EXPLAIN QUERY PLAN "+activitySpansSQL, day, day+24*h)
	if !strings.Contains(plan, "idx_activity_spans_start (start_ms>? AND start_ms<?)") {
		t.Errorf("the span read plans %s; want the start index bounded on both sides", plan)
	}
	if _, err := s.SaveActivitySpans([]ActivitySpan{{StartMs: day, EndMs: day + MaxActivitySpanMs + 1}}); err == nil {
		t.Error("a span longer than a day was saved")
	}

	off := activityStore(t, false)
	ids, err = off.SaveActivitySpans([]ActivitySpan{{ThreadID: "t1", StartMs: day, EndMs: day + h}})
	if err != nil || !slices.Equal(ids, []int64{0}) {
		t.Fatalf("with no Jira connection saved %v, %v; want nothing", ids, err)
	}
	if got, _ := off.ActivitySpans(day, day+h); len(got) != 0 {
		t.Fatalf("with no Jira connection read %+v", got)
	}
}

// A turn start names its thread on its local day, once, while a Jira
// connection is on; a second turn of the thread that day writes no other
// row, and with no Jira connection on no day is named. The days read by
// day.
func TestATurnStartNamesItsThreadsDay(t *testing.T) {
	t.Parallel()
	s := activityStore(t, true)
	at := time.Date(2026, 11, 24, 9, 0, 0, 0, time.Local).UnixMilli()
	for i, turn := range []Turn{
		{TurnID: "a", ThreadID: "t1", TurnIndex: 0, StartedAt: at},
		{TurnID: "b", ThreadID: "t1", TurnIndex: 1, StartedAt: at + 3_600_000},
		{TurnID: "c", ThreadID: "t2", TurnIndex: 0, StartedAt: at + 24*3_600_000},
	} {
		if err := s.InsertTurn(turn); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}
	got, err := s.ThreadDays("2026-11-24", "2026-11-25")
	if err != nil {
		t.Fatal(err)
	}
	want := []ThreadDay{{Day: "2026-11-24", ThreadID: "t1"}, {Day: "2026-11-25", ThreadID: "t2"}}
	if !slices.Equal(got, want) {
		t.Fatalf("thread days %+v\nwant %+v", got, want)
	}
	if plan := queryPlanText(t, s.db, "EXPLAIN QUERY PLAN "+threadDaysSQL, "2026-11-24", "2026-11-25"); !strings.Contains(plan, "SEARCH thread_days USING PRIMARY KEY (day>? AND day<?)") {
		t.Errorf("the thread day read plans %s; want the key's day range", plan)
	}
	// A turn whose insert fails names no day.
	if err := s.InsertTurn(Turn{TurnID: "a", ThreadID: "t2", TurnIndex: 5, StartedAt: at}); err == nil {
		t.Fatal("a turn ID twice was inserted")
	}
	if got, _ := s.ThreadDays("2026-11-24", "2026-11-24"); len(got) != 1 {
		t.Fatalf("a failed turn insert named a day: %+v", got)
	}

	off := activityStore(t, false)
	if err := off.InsertTurn(Turn{TurnID: "a", ThreadID: "t1", StartedAt: at}); err != nil {
		t.Fatal(err)
	}
	if got, _ := off.ThreadDays("2026-11-24", "2026-11-24"); len(got) != 0 {
		t.Fatalf("with no Jira connection the turn named %+v", got)
	}
}

// The prune deletes spans that start before its time and thread days
// before its day, and nothing else.
func TestThePruneDeletesOnlyOldActivity(t *testing.T) {
	t.Parallel()
	s := activityStore(t, true)
	day := int64(1_795_000_000_000)
	if _, err := s.SaveActivitySpans([]ActivitySpan{{StartMs: day - 10, EndMs: day}, {StartMs: day, EndMs: day + 10}}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s.db, `INSERT INTO thread_days(day, thread_id) VALUES ('2026-08-01', 't1'), ('2026-08-02', 't1')`)
	spans, days, err := s.PruneActivity(day, "2026-08-02")
	if err != nil || spans != 1 || days != 1 {
		t.Fatalf("pruned %d spans and %d days, %v; want 1 and 1", spans, days, err)
	}
	if got, _ := s.ActivitySpans(day-10, day+10); len(got) != 1 || got[0].StartMs != day {
		t.Fatalf("after the prune the spans are %+v", got)
	}
	if got, _ := s.ThreadDays("2026-01-01", "2026-12-31"); len(got) != 1 || got[0].Day != "2026-08-02" {
		t.Fatalf("after the prune the days are %+v", got)
	}
	var threads int
	if err := s.db.QueryRow(`SELECT count(*) FROM threads`).Scan(&threads); err != nil || threads != 2 {
		t.Fatalf("the prune left %d threads, %v", threads, err)
	}
}

// A restore keeps the activity this computer recorded since the
// snapshot: it is not in the snapshot to bring back.
func TestARestoreKeepsTheRecordedActivity(t *testing.T) {
	st := snapshotTestStore(t)
	seedSnapshotFixture(t, st, "t1", "before snapshot")
	if _, err := st.CreateTrackerConnection(NewTrackerConnection{Kind: TrackerKindJira, Site: "example.atlassian.net", Account: "user1@example.com", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := st.SnapshotTo(snap); err != nil {
		t.Fatal(err)
	}
	day := int64(1_795_000_000_000)
	if _, err := st.SaveActivitySpans([]ActivitySpan{{ThreadID: "t1", StartMs: day, EndMs: day + 60_000}}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertTurn(Turn{TurnID: "after", ThreadID: "t1", TurnIndex: 9, StartedAt: day}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RestoreFrom(snap); err != nil {
		t.Fatal(err)
	}
	if got, err := st.ActivitySpans(day, day+60_000); err != nil || len(got) != 1 {
		t.Fatalf("after the restore the spans are %+v, %v", got, err)
	}
	if got, err := st.ThreadDays(localDay(day), localDay(day)); err != nil || len(got) != 1 {
		t.Fatalf("after the restore the thread days are %+v, %v", got, err)
	}
}

// ThreadProjects answers each thread's project once and leaves out a
// thread with no project or no row.
func TestThreadProjectsAreEachOnce(t *testing.T) {
	t.Parallel()
	s := activityStore(t, false)
	now := time.Now().UnixMilli()
	if _, err := s.CreateProject(Project{ID: "p2", Path: "/ws/p2", Name: "Q", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for id, project := range map[string]string{"t3": "p2", "t4": ""} {
		if err := s.CreateThread(Thread{ID: id, ProjectID: project, Title: id, Provider: "claude", WorkspacePath: "/ws/p2", Mode: "chat",
			RuntimeMode: "approval-required", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ThreadProjects([]string{"t1", "t2", "t3", "t4", "gone"})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, p := range got {
		paths = append(paths, p.Path)
	}
	if !slices.Equal(paths, []string{"/ws/p1", "/ws/p2"}) {
		t.Fatalf("the projects are %v; want /ws/p1 and /ws/p2 once each", paths)
	}
}
