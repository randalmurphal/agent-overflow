package store

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func webWorklog(id, day string, spent int64) MirrorWorklog {
	return MirrorWorklog{ID: id, IssueID: "10001", AuthorID: "acc-user1", Day: day, SpentSeconds: spent, Description: "Review", UpdatedAt: 5}
}

func mustReplaceWorklogs(t *testing.T, s *Store, root int64, from, to string, readAt int64, logs []MirrorWorklog, want MirrorWorklogChanges) {
	t.Helper()
	got, err := s.ReplaceMirrorWorklogs(root, "acc-user1", from, to, mirrorReadAt(readAt), logs)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("replace %s to %s at %d = %+v, %v; want %+v", from, to, readAt, got, err, want)
	}
}

// A replace owns one author's days: it upserts what it lists, removes the
// rest of those days, and leaves other days and authors.
func TestReplaceMirrorWorklogs(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	week := []MirrorWorklog{webWorklog("w1", "2026-01-05", 3600), webWorklog("w2", "2026-01-06", 1800), webWorklog("w3", "2026-01-09", 600)}
	mustReplaceWorklogs(t, s, root, "2026-01-05", "2026-01-09", 10, week, MirrorWorklogChanges{Changed: []string{"w1", "w2", "w3"}})
	other := webWorklog("w9", "2026-01-06", 60)
	other.AuthorID, other.IssueID = "acc-user2", "10002"
	if _, err := s.ReplaceMirrorWorklogs(root, "acc-user2", "2026-01-06", "2026-01-06", mirrorReadAt(10), []MirrorWorklog{other}); err != nil {
		t.Fatal(err)
	}
	mustReplaceWorklogs(t, s, root, "2026-01-05", "2026-01-09", 10, week, MirrorWorklogChanges{})
	edited := webWorklog("w2", "2026-01-06", 2700)
	mustReplaceWorklogs(t, s, root, "2026-01-06", "2026-01-07", 11, []MirrorWorklog{edited}, MirrorWorklogChanges{Changed: []string{"w2"}})
	mustReplaceWorklogs(t, s, root, "2026-01-05", "2026-01-06", 12, nil, MirrorWorklogChanges{Removed: []string{"w1", "w2"}})
	logs, more, err := s.ListMirrorWorklogs(root, "acc-user1", "2026-01-01", "2026-01-31")
	if err != nil || more || !reflect.DeepEqual(logs, []MirrorWorklog{week[2]}) {
		t.Fatalf("author's month = %+v, %v, %v", logs, more, err)
	}
	logs, more, err = s.ListMirrorIssueWorklogs(root, "10002")
	if err != nil || more || !reflect.DeepEqual(logs, []MirrorWorklog{other}) {
		t.Fatalf("issue's worklogs = %+v, %v, %v", logs, more, err)
	}

	month := make([]MirrorWorklog, 0, clearMirrorChunk+1)
	for i := range clearMirrorChunk + 1 {
		month = append(month, webWorklog(fmt.Sprint("m", i), "2026-02-01", 60))
	}
	for name, call := range map[string]func() error{
		"bad day": func() error {
			_, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-01x", "2026-01-09", mirrorReadAt(1), nil)
			return err
		},
		"bad last day": func() error {
			_, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-9", mirrorReadAt(1), nil)
			return err
		},
		"days backwards": func() error {
			_, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-09", "2026-01-05", mirrorReadAt(1), nil)
			return err
		},
		"too many days": func() error {
			_, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-01", "2026-02-01", mirrorReadAt(1), nil)
			return err
		},
		"too many logs": func() error {
			_, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-02-01", "2026-02-01", mirrorReadAt(1), month)
			return err
		},
		"no author": func() error {
			_, err := s.ReplaceMirrorWorklogs(root, "", "2026-01-05", "2026-01-09", mirrorReadAt(1), nil)
			return err
		},
		"no read time": func() error {
			_, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-09", mirrorReadAt(0), nil)
			return err
		},
		"another author": func() error {
			_, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-09", mirrorReadAt(1), []MirrorWorklog{other})
			return err
		},
		"no issue":        func() error { w := week[0]; w.IssueID = ""; return replaceOne(s, root, w) },
		"no worklog ID":   func() error { w := week[0]; w.ID = ""; return replaceOne(s, root, w) },
		"negative update": func() error { w := week[0]; w.UpdatedAt = -1; return replaceOne(s, root, w) },
		"bad log day":     func() error { w := week[0]; w.Day = "2026-01-05T"; return replaceOne(s, root, w) },
		"no time spent":   func() error { return replaceOne(s, root, webWorklog("w4", "2026-01-05", 0)) },
		"outside the days": func() error {
			_, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-06", "2026-01-09", mirrorReadAt(1), week[:1])
			return err
		},
		"repeated": func() error {
			_, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-09", mirrorReadAt(1), []MirrorWorklog{week[0], week[0]})
			return err
		},
	} {
		if err := call(); !errors.Is(err, ErrInvalidTrackerCache) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func replaceOne(s *Store, root int64, w MirrorWorklog) error {
	_, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-09", mirrorReadAt(1), []MirrorWorklog{w})
	return err
}

// A day a read that began later changed is left as that read stored it
// and reported, its worklogs neither replaced nor removed nor moved; the
// other days of the range follow the replace. A later read is admitted
// for every day of its range, an identical one and a day with no worklogs
// included, so a read that began between the two is superseded there.
func TestMirrorWorklogDaysFollowTheirReadOrder(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	week := []MirrorWorklog{webWorklog("w1", "2026-01-05", 3600), webWorklog("w2", "2026-01-06", 1800), webWorklog("w3", "2026-01-09", 600)}
	mustReplaceWorklogs(t, s, root, "2026-01-05", "2026-01-09", 10, week, MirrorWorklogChanges{Changed: []string{"w1", "w2", "w3"}})
	edited := webWorklog("w2", "2026-01-06", 2700)
	mustReplaceWorklogs(t, s, root, "2026-01-06", "2026-01-06", 20, []MirrorWorklog{edited}, MirrorWorklogChanges{Changed: []string{"w2"}})

	early := []MirrorWorklog{webWorklog("w1", "2026-01-05", 60), webWorklog("w3", "2026-01-09", 60), webWorklog("w4", "2026-01-06", 60)}
	mustReplaceWorklogs(t, s, root, "2026-01-05", "2026-01-09", 15, early, MirrorWorklogChanges{
		Changed: []string{"w1", "w3"}, SupersededDays: []string{"2026-01-06"}})
	moved := webWorklog("w2", "2026-01-07", 2700)
	mustReplaceWorklogs(t, s, root, "2026-01-07", "2026-01-07", 15, []MirrorWorklog{moved}, MirrorWorklogChanges{})
	logs, _, err := s.ListMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-09")
	if want := []MirrorWorklog{early[0], edited, early[1]}; err != nil || !reflect.DeepEqual(logs, want) {
		t.Fatalf("worklogs = %+v, %v\nwant %+v", logs, err, want)
	}
	kept := []MirrorWorklog{early[0], edited, early[1]}
	mustReplaceWorklogs(t, s, root, "2026-01-05", "2026-01-09", 30, kept, MirrorWorklogChanges{})
	days := `SELECT day || '@' || read_at FROM tracker_cache_worklog_days ORDER BY day`
	assertRows(t, s.db, days, "2026-01-05@30,2026-01-06@30,2026-01-07@30,2026-01-08@30,2026-01-09@30")
	mustReplaceWorklogs(t, s, root, "2026-01-05", "2026-01-09", 25, nil, MirrorWorklogChanges{
		SupersededDays: []string{"2026-01-05", "2026-01-06", "2026-01-07", "2026-01-08", "2026-01-09"}})
	mustReplaceWorklogs(t, s, root, "2026-01-08", "2026-01-08", 25, []MirrorWorklog{webWorklog("w5", "2026-01-08", 60)},
		MirrorWorklogChanges{SupersededDays: []string{"2026-01-08"}})
	if logs, _, err := s.ListMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-09"); err != nil || !reflect.DeepEqual(logs, kept) {
		t.Fatalf("after the earlier reads: %+v, %v", logs, err)
	}
	assertRows(t, s.db, days, "2026-01-05@30,2026-01-06@30,2026-01-07@30,2026-01-08@30,2026-01-09@30")
}

// A range is replaced only while it holds at most clearMirrorChunk stored
// worklogs, so a replace removes at most a chunk; a read past its bound
// says so.
func TestMirrorWorklogsAreBounded(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	for d, n := range []int{400, 400, 201} {
		day := fmt.Sprintf("2026-01-%02d", 5+d)
		logs := make([]MirrorWorklog, 0, n)
		for i := range n {
			logs = append(logs, webWorklog(fmt.Sprintf("w%d-%04d", d, i), day, 60))
		}
		if _, err := s.ReplaceMirrorWorklogs(root, "acc-user1", day, day, mirrorReadAt(1), logs); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-06", mirrorReadAt(2), nil); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Fatalf("a range holding 800 worklogs: %v", err)
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_worklogs`, "1001")
	got, more, err := s.ListMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-07")
	if err != nil || !more || len(got) != mirrorWorklogLimit {
		t.Fatalf("read = %d, %v, %v", len(got), more, err)
	}
	got, more, err = s.ListMirrorIssueWorklogs(root, "10001")
	if err != nil || !more || len(got) != mirrorWorklogLimit {
		t.Fatalf("issue read = %d, %v, %v", len(got), more, err)
	}
}

func mustSyncWrite(t *testing.T, what string, changed bool, err error, want bool) {
	t.Helper()
	if err != nil || changed != want {
		t.Fatalf("%s: changed %v, %v; want %v", what, changed, err, want)
	}
}

// Each attempt is one write that leaves the cursor and only moves the
// scope's state forward: a read or reconcile time never goes back, a
// success clears a failure recorded no later than it, a failure from
// before the scope's last read, reconcile or outcome is superseded, and an
// attempt that changes nothing writes nothing.
func TestMirrorSyncStateMovesForward(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10"), []byte("c1"))
	read := func(at int64) MirrorSyncAttempt { return MirrorSyncAttempt{ReadAt: mirrorReadAt(at)} }
	reconcile := func(at int64) MirrorSyncAttempt { return MirrorSyncAttempt{ReconciledAt: mirrorReadAt(at)} }
	failed := func(at int64, kind, message string) MirrorSyncAttempt {
		return MirrorSyncAttempt{Failure: MirrorAttemptFailure{At: mirrorReadAt(at), Kind: kind, Message: message}}
	}
	limited := failed(30, MirrorFailureRateLimited, "The tracker asked to wait.")
	for _, step := range []struct {
		name    string
		attempt MirrorSyncAttempt
		want    bool
	}{
		{"read", read(10), true},
		{"read again", read(10), false},
		{"earlier read", read(5), false},
		{"reconcile", reconcile(20), true},
		{"reconcile again", reconcile(20), false},
		{"earlier reconcile", reconcile(15), false},
		{"failure", limited, true},
		{"failure again", limited, false},
		{"failure before the reconcile", failed(15, MirrorFailureTransient, ""), false},
	} {
		changed, err := s.RecordMirrorSyncAttempt(root, "10", currentAttempt(s, root, "10", step.attempt))
		mustSyncWrite(t, step.name, changed, err, step.want)
	}
	var structureAt int64
	if err := s.db.QueryRow(`SELECT structure_wall FROM tracker_cache_boards WHERE root_id = ? AND board_id = '10'`, root).Scan(&structureAt); err != nil || structureAt == 0 {
		t.Fatalf("structure read at %d: %v", structureAt, err)
	}
	want := MirrorSync{Lifetime: currentLifetime(s, root, "10"), Sweep: 1, StructureWall: structureAt, Cursor: []byte("c1"), ReadAt: 10, ReconciledAt: 20, ReconciledWall: 20, Failure: MirrorSyncFailure{At: 30, Kind: MirrorFailureRateLimited, Message: "The tracker asked to wait."}}
	if got, ok, err := s.GetMirrorSync(root, "10", testNow); err != nil || !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("state = %+v, %v, %v\nwant %+v", got, ok, err, want)
	}

	for _, step := range []struct {
		name    string
		attempt MirrorSyncAttempt
		want    bool
	}{
		{"read from before the failure", read(25), true},
		{"read after the failure", read(40), true},
		{"read with no failure", read(40), false},
		{"failure before the clear", failed(35, MirrorFailureTransient, ""), false},
		{"failure after", failed(50, MirrorFailureTracker, "The tracker failed."), true},
		{"reconcile after the failure", reconcile(55), true},
		{"later read", read(60), true},
		{"failure before the read", failed(58, MirrorFailureTransient, ""), false},
	} {
		changed, err := s.RecordMirrorSyncAttempt(root, "10", currentAttempt(s, root, "10", step.attempt))
		mustSyncWrite(t, step.name, changed, err, step.want)
	}
	assertRows(t, s.db, `SELECT read_at || ':' || reconciled_at || ':' || failure_kind || ':' || outcome_at FROM tracker_cache_sync WHERE board_id = '10'`, "60:55::55")
	next := webPage("10")
	next.ReadAt = mirrorReadAt(60)
	mustWriteRowPage(t, s, root, next, []byte("c2"))
	want = MirrorSync{Lifetime: want.Lifetime, Sweep: 1, StructureWall: structureAt, Cursor: []byte("c2"), ReadAt: 60, ReconciledAt: 55, ReconciledWall: 55}
	if got, _, err := s.GetMirrorSync(root, "10", testNow); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("state = %+v, %v\nwant %+v", got, err, want)
	}

	// The linked issues are a scope of their own, with no board; a board
	// no project lists has none.
	changed, err := s.RecordMirrorSyncAttempt(root, "", currentAttempt(s, root, "", read(40)))
	mustSyncWrite(t, "linked scope read", changed, err, true)
	if got, ok, err := s.GetMirrorSync(root, "", testNow); err != nil || !ok || !reflect.DeepEqual(got, MirrorSync{Sweep: 1, ReadAt: 40}) {
		t.Fatalf("linked scope = %+v, %v, %v", got, ok, err)
	}
	if _, err := s.RecordMirrorSyncAttempt(root, "13", currentAttempt(s, root, "13", read(40))); !errors.Is(err, ErrTrackerCacheUnlisted) {
		t.Fatalf("read of an unlisted board: %v", err)
	}
	for name, attempt := range map[string]MirrorSyncAttempt{
		"unknown kind":         failed(70, "flaky", ""),
		"failure no time":      failed(0, MirrorFailureSetup, ""),
		"message with no kind": failed(70, "", "x"),
		"no time":              read(0),
		"negative read":        {ReadAt: mirrorReadAt(-1), ReconciledAt: mirrorReadAt(70)},
		"negative reconcile":   {ReadAt: mirrorReadAt(70), ReconciledAt: mirrorReadAt(-1)},
		"negative failure":     {ReadAt: mirrorReadAt(70), Failure: MirrorAttemptFailure{At: mirrorReadAt(-1), Kind: MirrorFailureSetup}},
	} {
		if _, err := s.RecordMirrorSyncAttempt(root, "10", currentAttempt(s, root, "10", attempt)); !errors.Is(err, ErrInvalidTrackerCache) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// One attempt records a tick whole, its successes and the failure that
// followed them, in one statement beside the listing check: at most one
// row written, none when nothing would change.
func TestMirrorSyncAttemptRecordsATickInOneStatement(t *testing.T) {
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	recorder := recordMirrorWrites(t, s)
	state := `SELECT read_at || ':' || reconciled_at || ':' || failed_at || ':' || failure_kind || ':' || outcome_at
		FROM tracker_cache_sync WHERE board_id = '10'`
	failure := func(at int64, kind string) MirrorAttemptFailure {
		return MirrorAttemptFailure{At: mirrorReadAt(at), Kind: kind}
	}
	for _, step := range []struct {
		name    string
		attempt MirrorSyncAttempt
		changed bool
		state   string
	}{
		{"read then a failure", MirrorSyncAttempt{ReadAt: mirrorReadAt(10), Failure: failure(10, MirrorFailureTransient)}, true, "10:0:10:transient:10"},
		{"the same tick again", MirrorSyncAttempt{ReadAt: mirrorReadAt(10), Failure: failure(10, MirrorFailureTransient)}, false, "10:0:10:transient:10"},
		{"reconcile, read, then another failure", MirrorSyncAttempt{ReadAt: mirrorReadAt(20), ReconciledAt: mirrorReadAt(20), Failure: failure(21, MirrorFailureTracker)},
			true, "20:20:21:tracker:21"},
		{"a success clears it", MirrorSyncAttempt{ReadAt: mirrorReadAt(30)}, true, "30:20:0::30"},
		{"an earlier tick", MirrorSyncAttempt{ReadAt: mirrorReadAt(25), Failure: failure(25, MirrorFailureTransient)}, false, "30:20:0::30"},
		{"a failure before the tick's read", MirrorSyncAttempt{ReadAt: mirrorReadAt(40), ReconciledAt: mirrorReadAt(35), Failure: failure(38, MirrorFailureSetup)},
			true, "40:35:0::30"},
		{"a failure alone", MirrorSyncAttempt{Failure: failure(45, MirrorFailureRateLimited)}, true, "40:35:45:rate_limited:45"},
		{"a reconcile that clears it", MirrorSyncAttempt{ReconciledAt: mirrorReadAt(50)}, true, "40:50:0::50"},
	} {
		attempt := currentAttempt(s, root, "10", step.attempt)
		recorder.reset()
		var changed bool
		var err error
		statements := countMirrorStatements(t, func() { changed, err = s.RecordMirrorSyncAttempt(root, "10", attempt) })
		mustSyncWrite(t, step.name, changed, err, step.changed)
		writes, want := recorder.rowWrites(), map[string]int{}
		if step.changed {
			want["tracker_cache_sync"] = 1
		}
		if statements != 2 || !reflect.DeepEqual(writes, want) {
			t.Errorf("%s: %d statements, rows %v; want 2 and %v", step.name, statements, writes, want)
		}
		assertRows(t, s.db, state, step.state)
	}
}

// A tick's state read fails with ErrTrackerCacheGone for a root that is
// detached or was never made, so a caller that keeps the root's ID learns
// from that one read to look the root up again.
func TestGetMirrorSyncOfAGoneRoot(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	if _, ok, err := s.GetMirrorSync(root, "10", testNow); err != nil || ok {
		t.Fatalf("a live root never synced: %v, %v", ok, err)
	}
	if _, err := s.DetachMirror(jira.ID); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []int64{root, 9999} {
		for _, board := range []string{"10", ""} {
			if _, _, err := s.GetMirrorSync(gone, board, testNow); !errors.Is(err, ErrTrackerCacheGone) {
				t.Errorf("root %d board %q: %v; want ErrTrackerCacheGone", gone, board, err)
			}
		}
	}
}

// The sync state says whether an issue the root keeps is due a history
// read, in the same statement, for a board scope and the linked scope; an
// issue due that nothing keeps any more is not.
func TestMirrorSyncSaysWhetherHistoryIsDue(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	due := func(board string) bool {
		t.Helper()
		st, _, err := s.GetMirrorSync(root, board, testNow)
		if err != nil {
			t.Fatal(err)
		}
		return st.HistoryDue
	}
	if due("10") || due("") {
		t.Fatal("an empty mirror is due a history read")
	}
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a")})
	if !due("10") || !due("") {
		t.Fatal("an issue with no history is not due")
	}
	if _, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, []MirrorHistory{statusHistory("10001", 5)}), MirrorCatalog{}, mirrorReadAt(0)); err != nil {
		t.Fatal(err)
	}
	if due("10") {
		t.Fatal("an issue whose history is final through its update is due")
	}
	writeMirrorBoard(t, s, root, "11", []MirrorRow{webRow("10002", 5, "a")})
	if !due("10") {
		t.Fatal("an issue due on another board of the root is not due")
	}
	if _, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "11", Missing: []string{"10002"}, ReadAt: mirrorReadAt(9)})); err != nil {
		t.Fatal(err)
	}
	if due("10") {
		t.Fatal("an issue nothing keeps is due")
	}
}
