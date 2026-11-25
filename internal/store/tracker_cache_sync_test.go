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

// The last day a date holds is a range of one day: its days are counted,
// not compared as text, where 10000-01-01 sorts before 9999-12-31.
func TestMirrorWorklogsCoverTheLastDay(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceWorklogs(t, s, root, "9999-12-31", "9999-12-31", 10, nil, MirrorWorklogChanges{})
	mustReplaceWorklogs(t, s, root, "9999-12-01", "9999-12-31", 11, nil, MirrorWorklogChanges{})
	assertRows(t, s.db, `SELECT count(*) || ' ' || min(day) || ' ' || max(day) FROM tracker_cache_worklog_days`, "31 9999-12-01 9999-12-31")
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
// scope's state forward: a read or reconcile time never goes back, a read
// clears a failure recorded no later than it (a reconcile does not), a
// failure from before the scope's last read or outcome is superseded, and
// an attempt that changes nothing writes nothing.
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
		_, changed, err := s.RecordMirrorSyncAttempt(root, "10", currentAttempt(s, root, "10", step.attempt))
		mustSyncWrite(t, step.name, changed, err, step.want)
	}
	var structureAt int64
	if err := s.db.QueryRow(`SELECT structure_wall FROM tracker_cache_boards WHERE root_id = ? AND board_id = '10'`, root).Scan(&structureAt); err != nil || structureAt == 0 {
		t.Fatalf("structure read at %d: %v", structureAt, err)
	}
	want := MirrorSync{Lifetime: currentLifetime(s, root, "10"), Sweep: 1, StructureWall: structureAt, Cursor: []byte("c1"), ReadAt: 10, ReadWall: 10, ReconciledAt: 20, ReconciledWall: 20, Failure: MirrorSyncFailure{At: 30, Wall: 30, Kind: MirrorFailureRateLimited, Message: "The tracker asked to wait."}}
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
		_, changed, err := s.RecordMirrorSyncAttempt(root, "10", currentAttempt(s, root, "10", step.attempt))
		mustSyncWrite(t, step.name, changed, err, step.want)
	}
	assertRows(t, s.db, `SELECT read_at || ':' || reconciled_at || ':' || failure_kind || ':' || outcome_at FROM tracker_cache_sync WHERE board_id = '10'`, "60:55::60")
	next := webPage("10")
	next.ReadAt = mirrorReadAt(60)
	mustWriteRowPage(t, s, root, next, []byte("c2"))
	want = MirrorSync{Lifetime: want.Lifetime, Sweep: 1, StructureWall: structureAt, Cursor: []byte("c2"), ReadAt: 60, ReadWall: 60, ReconciledAt: 55, ReconciledWall: 55}
	if got, _, err := s.GetMirrorSync(root, "10", testNow); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("state = %+v, %v\nwant %+v", got, err, want)
	}

	// The linked issues are a scope of their own, with no board; a board
	// no project lists has none.
	_, changed, err := s.RecordMirrorSyncAttempt(root, "", currentAttempt(s, root, "", read(40)))
	mustSyncWrite(t, "linked scope read", changed, err, true)
	if got, ok, err := s.GetMirrorSync(root, "", testNow); err != nil || !ok || !reflect.DeepEqual(got, MirrorSync{Sweep: 1, ReadAt: 40, ReadWall: 40}) {
		t.Fatalf("linked scope = %+v, %v, %v", got, ok, err)
	}
	if _, _, err := s.RecordMirrorSyncAttempt(root, "13", currentAttempt(s, root, "13", read(40))); !errors.Is(err, ErrTrackerCacheUnlisted) {
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
		if _, _, err := s.RecordMirrorSyncAttempt(root, "10", currentAttempt(s, root, "10", attempt)); !errors.Is(err, ErrInvalidTrackerCache) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// One attempt records a tick whole, its successes and the failure that
// followed them, in one statement beside the listing check, which returns
// the outcome the row holds: at most one row written, and none when
// nothing would change, which reads the outcome in a third statement.
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
		{"read then a failure", MirrorSyncAttempt{ReadAt: mirrorReadAt(10), Failure: failure(11, MirrorFailureTransient)}, true, "10:0:11:transient:11"},
		{"the same tick again", MirrorSyncAttempt{ReadAt: mirrorReadAt(10), Failure: failure(11, MirrorFailureTransient)}, false, "10:0:11:transient:11"},
		{"read, reconcile, then another failure", MirrorSyncAttempt{ReadAt: mirrorReadAt(20), ReconciledAt: mirrorReadAt(21), Failure: failure(22, MirrorFailureTracker)},
			true, "20:21:22:tracker:22"},
		{"a read clears it", MirrorSyncAttempt{ReadAt: mirrorReadAt(30)}, true, "30:21:0::30"},
		{"an earlier tick", MirrorSyncAttempt{ReadAt: mirrorReadAt(25), Failure: failure(26, MirrorFailureTransient)}, false, "30:21:0::30"},
		{"a failure before the tick's read", MirrorSyncAttempt{ReadAt: mirrorReadAt(40), ReconciledAt: mirrorReadAt(35), Failure: failure(38, MirrorFailureSetup)},
			true, "40:35:0::30"},
		{"a failure alone", MirrorSyncAttempt{Failure: failure(45, MirrorFailureRateLimited)}, true, "40:35:45:rate_limited:45"},
		{"a reconcile leaves it", MirrorSyncAttempt{ReconciledAt: mirrorReadAt(50)}, true, "40:50:45:rate_limited:45"},
		{"a read at its time clears it", MirrorSyncAttempt{ReadAt: mirrorReadAt(45)}, true, "45:50:0::45"},
	} {
		attempt := currentAttempt(s, root, "10", step.attempt)
		recorder.reset()
		var (
			out     MirrorSyncOutcome
			changed bool
			err     error
		)
		statements := countMirrorStatements(t, func() { out, changed, err = s.RecordMirrorSyncAttempt(root, "10", attempt) })
		mustSyncWrite(t, step.name, changed, err, step.changed)
		writes, want := recorder.rowWrites(), map[string]int{}
		wantStatements := 3
		if step.changed {
			want["tracker_cache_sync"], wantStatements = 1, 2
		}
		if statements != wantStatements || !reflect.DeepEqual(writes, want) {
			t.Errorf("%s: %d statements, rows %v; want %d and %v", step.name, statements, writes, wantStatements, want)
		}
		assertRows(t, s.db, state, step.state)
		if got, _, err := s.GetMirrorSync(root, "10", testNow); err != nil || !reflect.DeepEqual(out, got.MirrorSyncOutcome) {
			t.Errorf("%s: returned %+v; the row holds %+v, %v", step.name, out, got.MirrorSyncOutcome, err)
		}
	}
}

// reconcileFailed is an attempt whose reconcile failed at read at, the
// call returning at returned.
func reconcileFailed(at, returned int64, refused bool) MirrorSyncAttempt {
	return MirrorSyncAttempt{ReconcileFailure: MirrorReconcileAttempt{At: mirrorReadAt(at), Returned: returned,
		Kind: MirrorFailureTracker, Message: "Jira refused the query.", Refused: refused}}
}

// A board's reconcile failure has a slot of its own: only a reconcile that
// completed at or after it clears it, a later reconcile failure replaces
// it, and a read neither clears it nor is cleared by it. Its wall time is
// when its call returned, not when the read began.
func TestMirrorReconcileFailureHasItsOwnSlot(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	record := func(name string, a MirrorSyncAttempt, want bool) MirrorSyncOutcome {
		t.Helper()
		out, changed, err := s.RecordMirrorSyncAttempt(root, "10", currentAttempt(s, root, "10", a))
		mustSyncWrite(t, name, changed, err, want)
		return out
	}
	refusal := MirrorReconcileFailure{MirrorSyncFailure: MirrorSyncFailure{At: 10, Wall: 1_000, Kind: MirrorFailureTracker, Message: "Jira refused the query."}, Refused: true}
	if out := record("a refusal", reconcileFailed(10, 1_000, true), true); out.ReconcileFailure != refusal || out.Failure != (MirrorSyncFailure{}) {
		t.Fatalf("after a refusal: %+v", out)
	}
	if out := record("a read", MirrorSyncAttempt{ReadAt: mirrorReadAt(20)}, true); out.ReconcileFailure != refusal {
		t.Fatalf("a read cleared the refusal: %+v", out)
	}
	other := MirrorSyncFailure{At: 25, Wall: 25, Kind: MirrorFailureTransient}
	if out := record("a delta failure", MirrorSyncAttempt{Failure: MirrorAttemptFailure{At: mirrorReadAt(25), Kind: MirrorFailureTransient}}, true); out.ReconcileFailure != refusal || out.Failure != other {
		t.Fatalf("after a delta failure: %+v", out)
	}
	record("an earlier reconcile failure", reconcileFailed(8, 2_000, false), false)
	later := MirrorReconcileFailure{MirrorSyncFailure: MirrorSyncFailure{At: 26, Wall: 3_000, Kind: MirrorFailureTracker, Message: "Jira refused the query."}}
	if out := record("a later reconcile failure", reconcileFailed(26, 3_000, false), true); out.ReconcileFailure != later || out.Failure != other {
		t.Fatalf("after a later reconcile failure: %+v", out)
	}
	if out := record("a reconcile", MirrorSyncAttempt{ReconciledAt: mirrorReadAt(30)}, true); out.ReconcileFailure != (MirrorReconcileFailure{}) || out.Failure != other {
		t.Fatalf("after a reconcile: %+v; want the delta's failure alone", out)
	}
	record("a reconcile failure before the reconcile", reconcileFailed(28, 4_000, true), false)
	record("a reconcile failure at the reconcile", reconcileFailed(30, 4_000, true), false)

	// The linked scope has no reconcile, and a reconcile is either
	// completed or failed.
	for name, c := range map[string]struct {
		board   string
		attempt MirrorSyncAttempt
	}{
		"linked reconcile":         {"", MirrorSyncAttempt{ReconciledAt: mirrorReadAt(40)}},
		"linked reconcile failure": {"", reconcileFailed(40, 1, false)},
		"completed and failed":     {"10", MirrorSyncAttempt{ReconciledAt: mirrorReadAt(40), ReconcileFailure: reconcileFailed(41, 1, false).ReconcileFailure}},
		"never returned":           {"10", reconcileFailed(40, 0, false)},
		"unknown kind":             {"10", MirrorSyncAttempt{ReconcileFailure: MirrorReconcileAttempt{At: mirrorReadAt(40), Returned: 1, Kind: "flaky"}}},
		"refused with no time":     {"10", MirrorSyncAttempt{ReadAt: mirrorReadAt(40), ReconcileFailure: MirrorReconcileAttempt{Refused: true}}},
	} {
		if _, _, err := s.RecordMirrorSyncAttempt(root, c.board, currentAttempt(s, root, c.board, c.attempt)); !errors.Is(err, ErrInvalidTrackerCache) {
			t.Errorf("%s: %v; want ErrInvalidTrackerCache", name, err)
		}
	}
}

// A success and a failure of its own slot at the same order time end in
// the same state whichever is recorded first: the success wins the tie,
// for the reconcile's slot and for the other one.
func TestMirrorSyncSuccessWinsATieInEitherOrder(t *testing.T) {
	t.Parallel()
	for name, pair := range map[string][2]MirrorSyncAttempt{
		"reconcile": {reconcileFailed(10, 1_000, true), {ReconciledAt: mirrorReadAt(10)}},
		"read":      {{Failure: MirrorAttemptFailure{At: mirrorReadAt(10), Kind: MirrorFailureTransient}}, {ReadAt: mirrorReadAt(10)}},
	} {
		var ends []MirrorSyncOutcome
		for _, order := range [][2]int{{0, 1}, {1, 0}} {
			s, _, root := newMirror(t)
			mustReplaceStructure(t, s, root, webStructure(), true)
			for _, i := range order {
				if _, _, err := s.RecordMirrorSyncAttempt(root, "10", currentAttempt(s, root, "10", pair[i])); err != nil {
					t.Fatal(err)
				}
			}
			got, _, err := s.GetMirrorSync(root, "10", testNow)
			if err != nil {
				t.Fatal(err)
			}
			ends = append(ends, got.MirrorSyncOutcome)
		}
		if ends[0] != ends[1] || ends[0].Failure != (MirrorSyncFailure{}) || ends[0].ReconcileFailure != (MirrorReconcileFailure{}) {
			t.Errorf("%s: failure then success %+v, success then failure %+v; want the same, with no failure", name, ends[0], ends[1])
		}
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
	if _, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, []MirrorHistory{statusHistory("10001", 5)}), MirrorCatalog{}, nextMirrorRead(s, root)); err != nil {
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
