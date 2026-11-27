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

// activityStore is a store with threads t1 and t2.
func activityStore(t *testing.T) *Store {
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
	return s
}

// A Jira connection is on while one is enabled: not with none, not with
// only a Linear connection, and not once the Jira one is turned off.
func TestJiraConnectionOnIsAnEnabledJiraConnection(t *testing.T) {
	t.Parallel()
	s := activityStore(t)
	on := func(want bool, when string) {
		t.Helper()
		if got, err := s.JiraConnectionOn(); err != nil || got != want {
			t.Fatalf("%s: a Jira connection on is %v, %v; want %v", when, got, err, want)
		}
	}
	on(false, "with no connection")
	if _, err := s.CreateTrackerConnection(NewTrackerConnection{Kind: TrackerKindLinear, Site: "linear.app", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	on(false, "with only a Linear connection")
	jira, err := s.CreateTrackerConnection(NewTrackerConnection{Kind: TrackerKindJira, Site: "example.atlassian.net", Account: "user1@example.com", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	on(true, "with a Jira connection")
	if _, _, err := s.SetTrackerConnectionEnabled(jira.ID, false); err != nil {
		t.Fatal(err)
	}
	on(false, "with the Jira connection off")
}

// A span is inserted while a Jira connection is on and its end moves on
// a later save; a read of a range answers the spans that reach it,
// through the start index bounded on both sides.
func TestActivitySpansAreSavedAndRead(t *testing.T) {
	t.Parallel()
	s := activityStore(t)
	const h = 3_600_000
	day := int64(1_795_000_000_000)
	ids, err := s.SaveActivity([]ActivitySpan{
		{ThreadID: "t1", StartMs: day, EndMs: day + h},
		{ThreadID: "t2", Phone: true, StartMs: day + 2*h, EndMs: day + 3*h},
		{StartMs: day - MaxActivitySpanMs - 1, EndMs: day - 1},
		{ThreadID: "t1", StartMs: day + 30*h, EndMs: day + 31*h},
	}, nil)
	if err != nil || len(ids) != 4 || slices.Contains(ids, 0) {
		t.Fatalf("saved %v, %v", ids, err)
	}
	if _, err := s.SaveActivity([]ActivitySpan{{ID: ids[0], ThreadID: "t1", StartMs: day, EndMs: day + 90*60_000}}, nil); err != nil {
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
	if _, err := s.SaveActivity([]ActivitySpan{{StartMs: day, EndMs: day + MaxActivitySpanMs + 1}}, nil); err == nil {
		t.Error("a span longer than a day was saved")
	}

}

// A saved day names its thread once: a second save of the thread that day
// writes no other row. The days read by day, through the key's day range.
func TestSavedDaysNameEachThreadOnce(t *testing.T) {
	t.Parallel()
	s := activityStore(t)
	for _, days := range [][]ThreadDay{
		{{Day: "2026-11-24", ThreadID: "t1"}},
		{{Day: "2026-11-24", ThreadID: "t1"}, {Day: "2026-11-25", ThreadID: "t2"}},
	} {
		if _, err := s.SaveActivity(nil, days); err != nil {
			t.Fatal(err)
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
	// A save that fails writes neither its days nor its spans.
	if _, err := s.SaveActivity([]ActivitySpan{{StartMs: 10, EndMs: 9}}, []ThreadDay{{Day: "2026-11-26", ThreadID: "t1"}}); err == nil {
		t.Fatal("a span that ends before it starts was saved")
	}
	if got, _ := s.ThreadDays("2026-11-26", "2026-11-26"); len(got) != 0 {
		t.Fatalf("a failed save named a day: %+v", got)
	}

}

// The prune deletes spans that start before its time and thread days
// before its day, and nothing else.
func TestThePruneDeletesOnlyOldActivity(t *testing.T) {
	t.Parallel()
	s := activityStore(t)
	day := int64(1_795_000_000_000)
	if _, err := s.SaveActivity([]ActivitySpan{{StartMs: day - 10, EndMs: day}, {StartMs: day, EndMs: day + 10}}, nil); err != nil {
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
	if _, err := st.SaveActivity([]ActivitySpan{{ThreadID: "t1", StartMs: day, EndMs: day + 60_000}}, []ThreadDay{{Day: "2026-11-24", ThreadID: "t1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RestoreFrom(snap); err != nil {
		t.Fatal(err)
	}
	if got, err := st.ActivitySpans(day, day+60_000); err != nil || len(got) != 1 {
		t.Fatalf("after the restore the spans are %+v, %v", got, err)
	}
	if got, err := st.ThreadDays("2026-11-24", "2026-11-24"); err != nil || len(got) != 1 {
		t.Fatalf("after the restore the thread days are %+v, %v", got, err)
	}
}

// ThreadProjects answers each thread's project once and leaves out a
// thread with no project or no row.
func TestThreadProjectsAreEachOnce(t *testing.T) {
	t.Parallel()
	s := activityStore(t)
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
