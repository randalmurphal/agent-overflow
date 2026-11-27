package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
)

// v177 makes the Tempo triggers delete the worklog acts and receipts of
// the root whose time source changed, and only those: its comments and
// every act of another root stay.
func TestTheActsMigrationFollowsTheTimeSource(t *testing.T) {
	db := migrateThrough(t, activityMigrationVersion)
	m := migrationByVersion(t, trackerActsMigrationVersion)
	if err := applyMigration(db, m); err != nil {
		t.Fatalf("apply v%d: %v", m.Version, err)
	}
	mustExec(t, db, `INSERT INTO tracker_connections(id, kind, site, account, created_at) VALUES
		('j1', 'jira', 'https://example.atlassian.net', 'user1@example.com', 1),
		('j2', 'jira', 'https://example.atlassian.net', 'user2@example.com', 1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_roots(id, connection_id, created_at) VALUES (7, 'j1', 1), (8, 'j2', 1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_issues(root_id, issue_id, issue_key, updated_at) VALUES (7, '10001', 'WEB-1', 5), (8, '10001', 'WEB-1', 5)`)
	act := func() {
		t.Helper()
		mustExec(t, db, `INSERT OR REPLACE INTO tracker_cache_issue_acts(root_id, issue_id, source, act_id, person, at, seconds) VALUES
			(7, '10001', 1, 'c1', 'acc-user1', 2, 0), (7, '10001', 2, 'w1', 'acc-user1', 2, 60),
			(8, '10001', 1, 'c1', 'acc-user2', 2, 0), (8, '10001', 2, 'w1', 'acc-user2', 2, 60)`)
		mustExec(t, db, `INSERT OR REPLACE INTO tracker_cache_issue_act_reads(root_id, issue_id, source, read_for, read_at, read_wall) VALUES
			(7, '10001', 1, 5, 1, 1), (7, '10001', 2, 5, 1, 1), (8, '10001', 1, 5, 1, 1), (8, '10001', 2, 5, 1, 1)`)
	}
	rows := `SELECT (SELECT group_concat(root_id || ':' || source) FROM (SELECT root_id, source FROM tracker_cache_issue_acts ORDER BY root_id, source))
		|| '/' || (SELECT group_concat(root_id || ':' || source) FROM (SELECT root_id, source FROM tracker_cache_issue_act_reads ORDER BY root_id, source))`
	const kept, dropped = "7:1,7:2,8:1,8:2/7:1,7:2,8:1,8:2", "7:1,8:1,8:2/7:1,8:1,8:2"
	for _, step := range []struct{ name, sql, want string }{
		{"Tempo added off", `INSERT INTO tracker_connections(id, kind, site, parent_id, enabled, created_at) VALUES ('t1', 'tempo', 'https://api.tempo.io', 'j1', 0, 1)`, kept},
		{"Tempo turned on", `UPDATE tracker_connections SET enabled = 1 WHERE id = 't1'`, dropped},
		{"Tempo left on", `UPDATE tracker_connections SET enabled = 1 WHERE id = 't1'`, kept},
		{"Tempo turned off", `UPDATE tracker_connections SET enabled = 0 WHERE id = 't1'`, dropped},
		{"Tempo removed while off", `DELETE FROM tracker_connections WHERE id = 't1'`, kept},
		{"Tempo added", `INSERT INTO tracker_connections(id, kind, site, parent_id, created_at) VALUES ('t1', 'tempo', 'https://api.tempo.io', 'j1', 1)`, dropped},
		{"Tempo removed while on", `DELETE FROM tracker_connections WHERE id = 't1'`, dropped},
	} {
		act()
		mustExec(t, db, step.sql)
		assertRows(t, db, rows, step.want)
		if t.Failed() {
			t.Fatalf("after %s", step.name)
		}
	}
}

// v177 checks each source's acts on their own: a comment and a worklog
// name an account and a time, a comment spends no time, and a pull
// request approval (source 3) has no time and names its reviewer by
// account or by an unmatched name, one of the two; no other source is
// stored.
func TestTheActsMigrationChecksEachSourcesShape(t *testing.T) {
	db := migrateThrough(t, trackerActsMigrationVersion)
	mustExec(t, db, `INSERT INTO tracker_connections(id, kind, site, account, created_at) VALUES ('j1', 'jira', 'https://example.atlassian.net', 'user1@example.com', 1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_roots(id, connection_id, created_at) VALUES (7, 'j1', 1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_issues(root_id, issue_id, issue_key, updated_at) VALUES (7, '10001', 'WEB-1', 5)`)
	for i, c := range []struct {
		source            int
		person, unmatched string
		at, seconds       int64
		ok                bool
	}{
		{1, "acc-user1", "", 2, 0, true},
		{1, "", "", 2, 0, false},
		{1, "acc-user1", "User 1", 2, 0, false},
		{1, "acc-user1", "", 0, 0, false},
		{1, "acc-user1", "", 2, 60, false},
		{2, "acc-user1", "", 2, 60, true},
		{2, "acc-user1", "", 2, -1, false},
		{2, "", "", 2, 60, false},
		{2, "acc-user1", "", 0, 60, false},
		{3, "acc-user1", "", 0, 0, true},
		{3, "", "User 9", 0, 0, true},
		{3, "acc-user1", "User 1", 0, 0, false},
		{3, "", "", 0, 0, false},
		{3, "acc-user1", "", 2, 0, false},
		{3, "acc-user1", "", 0, 60, false},
		{4, "acc-user1", "", 2, 0, false},
	} {
		_, err := db.Exec(`INSERT INTO tracker_cache_issue_acts(root_id, issue_id, source, act_id, person, unmatched, at, seconds) VALUES (7, '10001', ?, ?, ?, ?, ?, ?)`,
			c.source, fmt.Sprint("a", i), c.person, c.unmatched, c.at, c.seconds)
		if (err == nil) != c.ok {
			t.Errorf("%+v: %v", c, err)
		}
	}
	// The worklog index holds a root's worklogs by their start, and no
	// other source's acts.
	assertRows(t, db, `SELECT group_concat(name, ',') FROM pragma_index_info('idx_tracker_cache_issue_acts_worklogs')`, "root_id,at")
	assertRows(t, db, `SELECT count(*) FROM tracker_cache_issue_acts INDEXED BY idx_tracker_cache_issue_acts_worklogs
		WHERE root_id = 7 AND source = 2 AND at >= 0`, "1")
}

// commentsOf is issue's comments as one read found them: one act per ID,
// by User 1, at at.
func commentsOf(issue string, readFor, at int64, ids ...string) MirrorIssueActs {
	in := MirrorIssueActs{IssueID: issue, Source: MirrorActComment, ReadFor: readFor}
	for _, id := range ids {
		in.Acts = append(in.Acts, MirrorAct{ID: id, Person: "acc-user1", At: at})
	}
	return in
}

// worklogsOf is issue's worklogs from from as one read found them: one
// act per ID, by User 1, at at, a minute each.
func worklogsOf(issue string, readFor, from, at int64, ids ...string) MirrorIssueActs {
	in := MirrorIssueActs{IssueID: issue, Source: MirrorActWorklog, ReadFor: readFor, From: from}
	for _, id := range ids {
		in.Acts = append(in.Acts, MirrorAct{ID: id, Person: "acc-user1", At: at, Seconds: 60})
	}
	return in
}

const storedActsSQL = `SELECT issue_id || ':' || source || ':' || act_id || ':' || person || ':' || at || ':' || seconds
	FROM tracker_cache_issue_acts ORDER BY issue_id, source, act_id`

// storedActReadsSQL is each row of the reads: issue, source, read_for,
// read_at, and the coverage, wall and attempt when set.
const storedActReadsSQL = `SELECT issue_id || ':' || source || ':' || read_for || ':' || read_at
	    || iif(read_from > 0, ' from ' || read_from, '') || iif(read_wall <> read_at, ' wall ' || read_wall, '')
	    || iif(tried_at > 0, ' tried ' || tried_at || ' ' || tried_reason, '')
	FROM tracker_cache_issue_act_reads ORDER BY issue_id, source`

// replaceActs replaces in, and tried, as a read of root begun at at.
func replaceActs(t *testing.T, s *Store, root, at int64, tried []MirrorActsAttempt, in ...MirrorIssueActs) {
	t.Helper()
	if _, err := s.ReplaceMirrorActs(root, 0, mirrorReadOf(root, at), in, tried, MirrorCatalog{}); err != nil {
		t.Fatal(err)
	}
}

// A replace makes an issue's acts of a source what the read found, with
// its receipt: an act gone is removed, one changed is rewritten, the
// other source is left as it was. A read that began no later than the
// receipt's is superseded and changes nothing; an issue the mirror does
// not keep stores nothing; the authors go to the catalog.
func TestMirrorActsReplaceAnIssuesSource(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 5, "a", "21")), []byte("c1"))
	worklogs := MirrorIssueActs{IssueID: "10001", Source: MirrorActWorklog, ReadFor: 5, Acts: []MirrorAct{{ID: "w1", Person: "acc-user2", At: 3, Seconds: 600}}}
	catalog := MirrorCatalog{Users: []MirrorUser{{ID: "acc-user2", Name: "User 2"}}}
	catalogWrite, err := s.ReplaceMirrorActs(root, 0, mirrorReadOf(root, 10), []MirrorIssueActs{commentsOf("10001", 5, 2, "c1", "c2"), worklogs,
		commentsOf("99999", 5, 2, "c9")}, nil, catalog)
	if err != nil || !catalogWrite.Catalog.Changed {
		t.Fatalf("first replace: %v, catalog %+v", err, catalogWrite)
	}
	assertRows(t, s.db, storedActsSQL, "10001:1:c1:acc-user1:2:0,10001:1:c2:acc-user1:2:0,10001:2:w1:acc-user2:3:600")
	assertRows(t, s.db, storedActReadsSQL, "10001:1:5:10,10001:2:5:10")
	assertRows(t, s.db, `SELECT name FROM tracker_cache_users WHERE user_id = 'acc-user2'`, "User 2")

	changed := commentsOf("10001", 6, 2, "c2", "c3")
	changed.Acts[0].At = 4
	replaceActs(t, s, root, 11, nil, changed)
	assertRows(t, s.db, storedActsSQL, "10001:1:c2:acc-user1:4:0,10001:1:c3:acc-user1:2:0,10001:2:w1:acc-user2:3:600")
	assertRows(t, s.db, storedActReadsSQL, "10001:1:6:11,10001:2:5:10")

	for _, at := range []int64{11, 9} {
		replaceActs(t, s, root, at, nil, commentsOf("10001", 7, 2))
	}
	assertRows(t, s.db, storedActReadsSQL, "10001:1:6:11,10001:2:5:10")
	replaceActs(t, s, root, 12, nil, commentsOf("10001", 7, 2), worklogsOf("10001", 7, 400, 500, "w2"))
	assertRows(t, s.db, storedActsSQL, "10001:2:w2:acc-user1:500:60")
	assertRows(t, s.db, storedActReadsSQL, "10001:1:7:12,10001:2:7:12 from 400")
}

// A failed attempt is recorded beside the receipt the row holds, at the
// read's wall time and with its reason, superseded as a read is, and a
// read that stores clears it; an issue the mirror does not keep records
// none.
func TestMirrorActsRecordAFailedAttempt(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 5, "a", "21"), webRow("10002", 5, "b", "21")), []byte("c1"))
	replaceActs(t, s, root, 10, nil, commentsOf("10001", 5, 2, "c1"))
	replaceActs(t, s, root, 11, []MirrorActsAttempt{{IssueID: "10001", Source: MirrorActComment, Reason: "Jira is busy"},
		{IssueID: "10002", Source: MirrorActWorklog, Reason: "too large"}, {IssueID: "99999", Source: MirrorActComment, Reason: "gone"}})
	assertRows(t, s.db, storedActReadsSQL, "10001:1:5:10 tried 11 Jira is busy,10002:2:0:0 tried 11 too large")
	assertRows(t, s.db, storedActsSQL, "10001:1:c1:acc-user1:2:0")
	replaceActs(t, s, root, 9, []MirrorActsAttempt{{IssueID: "10001", Source: MirrorActComment, Reason: "older"}})
	assertRows(t, s.db, storedActReadsSQL, "10001:1:5:10 tried 11 Jira is busy,10002:2:0:0 tried 11 too large")
	replaceActs(t, s, root, 12, nil, commentsOf("10001", 6, 2), worklogsOf("10002", 5, 0, 3, "w1"))
	assertRows(t, s.db, storedActReadsSQL, "10001:1:6:12,10002:2:5:12")
}

// A replace refuses what it cannot store as given, before writing
// anything.
func TestMirrorActsRefuseAnInvalidReplace(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 5, "a", "21")), []byte("c1"))
	comment := commentsOf("10001", 5, 2, "c1")
	from := commentsOf("10001", 5, 2)
	from.From = 1
	attempt := MirrorActsAttempt{IssueID: "10001", Source: MirrorActComment, Reason: "busy"}
	for name, c := range map[string]struct {
		in    []MirrorIssueActs
		tried []MirrorActsAttempt
	}{
		"no issue":                  {in: []MirrorIssueActs{commentsOf("", 5, 2)}},
		"no source":                 {in: []MirrorIssueActs{{IssueID: "10001", ReadFor: 5}}},
		"no update time":            {in: []MirrorIssueActs{commentsOf("10001", 0, 2)}},
		"an issue twice":            {in: []MirrorIssueActs{comment, comment}},
		"read and attempted":        {in: []MirrorIssueActs{comment}, tried: []MirrorActsAttempt{attempt}},
		"an attempt with no reason": {tried: []MirrorActsAttempt{{IssueID: "10001", Source: MirrorActComment}}},
		"comments from a start":     {in: []MirrorIssueActs{from}},
		"an act twice":              {in: []MirrorIssueActs{commentsOf("10001", 5, 2, "c1", "c1")}},
		"an act with no time":       {in: []MirrorIssueActs{commentsOf("10001", 5, 0, "c1")}},
		"a comment's seconds":       {in: []MirrorIssueActs{{IssueID: "10001", Source: MirrorActComment, ReadFor: 5, Acts: []MirrorAct{{ID: "c1", Person: "acc-user1", At: 2, Seconds: 1}}}}},
		"no author":                 {in: []MirrorIssueActs{{IssueID: "10001", Source: MirrorActWorklog, ReadFor: 5, Acts: []MirrorAct{{ID: "w1", At: 2, Seconds: 1}}}}},
	} {
		if _, err := s.ReplaceMirrorActs(root, 0, mirrorReadOf(root, 10), c.in, c.tried, MirrorCatalog{}); !errors.Is(err, ErrInvalidTrackerCache) {
			t.Errorf("%s: %v; want it refused", name, err)
		}
	}
	if _, err := s.ReplaceMirrorActs(root, 0, MirrorRead{}, []MirrorIssueActs{comment}, nil, MirrorCatalog{}); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Errorf("no read: %v; want it refused", err)
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_acts`, "0")
}

// A call under an earlier time source generation stores its comments and
// none of its worklogs or worklog attempts, in every transaction, and
// returns ErrTrackerCacheSourceChanged; a call of comments alone names no
// generation.
func TestMirrorActsFollowTheirTimeSource(t *testing.T) {
	t.Parallel()
	s, c, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 5, "a", "21"), webRow("10002", 5, "b", "21")), []byte("c1"))
	mustCreateTrackerConnection(t, s, NewTrackerConnection{Kind: TrackerKindTempo, Site: "https://api.tempo.io", ParentID: c.ID, Label: "Tempo", CreatedAt: 11})
	many := commentsOf("10002", 5, 2)
	for i := range mirrorMaxBatchChanges {
		many.Acts = append(many.Acts, MirrorAct{ID: strconv.Itoa(i), Person: "acc-user1", At: 2})
	}
	_, err := s.ReplaceMirrorActs(root, 0, mirrorReadOf(root, 10), []MirrorIssueActs{commentsOf("10001", 5, 2, "c1"), worklogsOf("10001", 5, 0, 3, "w1"), many},
		[]MirrorActsAttempt{{IssueID: "10002", Source: MirrorActWorklog, Reason: "x"}}, MirrorCatalog{})
	if !errors.Is(err, ErrTrackerCacheSourceChanged) {
		t.Fatalf("a worklog read under generation 0: %v", err)
	}
	assertRows(t, s.db, `SELECT DISTINCT issue_id || ':' || source FROM tracker_cache_issue_acts ORDER BY 1`, "10001:1,10002:1")
	assertRows(t, s.db, storedActReadsSQL, "10001:1:5:10,10002:1:5:10")
	if _, err := s.ReplaceMirrorActs(root, 0, mirrorReadOf(root, 11), []MirrorIssueActs{commentsOf("10001", 6, 2, "c1")}, nil, MirrorCatalog{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceMirrorActs(root, 1, mirrorReadOf(root, 12), []MirrorIssueActs{worklogsOf("10001", 5, 0, 3, "w1")}, nil, MirrorCatalog{}); err != nil {
		t.Fatal(err)
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_act_reads WHERE source = 2`, "1")
}

// A call of more acts than one transaction writes is written in several,
// an issue's acts of a source never split, counting the acts it may
// remove as well as those it writes.
func TestMirrorActsSplitAcrossTransactions(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 5, "a", "21"), webRow("10002", 5, "b", "21")), []byte("c1"))
	many := func(issue string, n int) MirrorIssueActs {
		in := MirrorIssueActs{IssueID: issue, Source: MirrorActComment, ReadFor: 5}
		for i := range n {
			in.Acts = append(in.Acts, MirrorAct{ID: strconv.Itoa(i), Person: "acc-user1", At: 2})
		}
		return in
	}
	before := s.CommitCount()
	replaceActs(t, s, root, 10, nil, many("10001", 1500), many("10002", 1500))
	if n := s.CommitCount() - before; n != 2 {
		t.Fatalf("%d transactions; want 2", n)
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_acts`, "3000")
	// Emptying 10001 removes 1,500 acts: with 10002's 600 it is over one
	// transaction's bound.
	before = s.CommitCount()
	replaceActs(t, s, root, 11, nil, many("10001", 0), many("10002", 600))
	if n := s.CommitCount() - before; n != 2 {
		t.Fatalf("%d transactions for 1,500 removed and 600 written; want 2", n)
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_acts`, "600")
}

// The remove of the acts a read did not find costs steps in proportion
// to the acts: each stored act is one probe of the read's.
func TestMirrorActsReplaceGrowsLinearly(t *testing.T) {
	steps := map[int]int{}
	for _, n := range []int{1000, 2000, 4000} {
		s, _, root := newMirror(t)
		mustReplaceStructure(t, s, root, webStructure(), true)
		mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 5, "a", "21")), []byte("c1"))
		acts := func(from, count int) MirrorIssueActs {
			in := MirrorIssueActs{IssueID: "10001", Source: MirrorActComment, ReadFor: 5}
			for i := range count {
				in.Acts = append(in.Acts, MirrorAct{ID: strconv.Itoa(from + i), Person: "acc-user1", At: 2})
			}
			return in
		}
		replaceActs(t, s, root, 10, nil, acts(0, n))
		var err error
		_, steps[n] = countStatementRuns(t, mirrorActsRemoveSQL, func() {
			_, err = s.ReplaceMirrorActs(root, 0, mirrorReadOf(root, 11), []MirrorIssueActs{acts(n/2, n)}, nil, MirrorCatalog{})
		})
		if err != nil {
			t.Fatal(err)
		}
		assertRows(t, s.db, fmt.Sprintf(`SELECT count(*) FROM tracker_cache_issue_acts WHERE root_id = %d AND issue_id = '10001' AND source = 1`, root), strconv.Itoa(n))
	}
	t.Logf("remove steps by stored acts: %v", steps)
	if steps[4000] > 5*steps[1000] {
		t.Fatalf("the remove's steps grow faster than its acts: %v", steps)
	}
}

// actsDue lists root's acts due on boards 10 (twice) and 11 by their
// plain groups, from window start from at wall time now, and returns each
// issue with its sources due (c, w) and the start of its worklogs.
func actsDue(t *testing.T, s *Store, root, from, now int64, boards ...MirrorActsBoard) (string, bool) {
	t.Helper()
	if len(boards) == 0 {
		boards = []MirrorActsBoard{{BoardID: "10"}, {BoardID: "10"}, {BoardID: "11"}}
	}
	due, more, err := s.ListMirrorActsDue(root, boards, from, now)
	if err != nil {
		t.Fatal(err)
	}
	out := ""
	for i, d := range due {
		if i > 0 {
			out += ","
		}
		out += d.IssueID + ":"
		if d.Comments {
			out += "c"
		}
		if d.Worklogs {
			out += "w"
			if d.WorklogsFrom > 0 {
				out += "@" + strconv.FormatInt(d.WorklogsFrom, 10)
			}
		}
	}
	return out, more
}

// The acts due on a board: its active sprints' members done now, first,
// then its closed sprints' members done at the completion, newest
// completion first, whose sprint completed in the window, each once, each
// source until a read for its update stored its receipt. A member carried
// on from a closed sprint whose history is unread is not done there yet,
// and a board the mirror does not show (11) lists nothing.
func TestMirrorActsDueFollowTheSprintsAndTheReceipts(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	st := webStructure()
	st.Sprints = append(st.Sprints, MirrorSprint{ID: "19", OriginBoardID: "10", Name: "WEB Sprint 0", State: MirrorSprintClosed, StartAt: 10, EndAt: 500, CompletedAt: 500})
	mustReplaceStructure(t, s, root, st, true)
	done := func(r MirrorRow) MirrorRow { r.Card.StatusID = "10001"; return r }
	mustWriteRowPage(t, s, root, webPage("10",
		done(webRow("10001", 5, "a", "21")),       // active, done now
		webRow("10002", 5, "b", "21"),             // active, to do
		webRow("10003", 5, "c", "20"),             // closed, done at the completion by its history
		done(webRow("10004", 5, "d", "20", "22")), // closed and carried on, history unread
		done(webRow("10005", 5, "e", "20")),       // closed, not done at the completion by its history
		done(webRow("10006", 5, "f", "19")),       // closed earlier, done now and at the completion
	), []byte("c1"))
	mustReplaceHistory(t, s, root, MirrorHistory{IssueID: "10003", Changes: []MirrorChange{{Kind: MirrorChangeStatus, At: 900, From: "1", To: "10001"}}},
		MirrorWrite{Changed: true})
	mustReplaceHistory(t, s, root, MirrorHistory{IssueID: "10005", Changes: []MirrorChange{{Kind: MirrorChangeStatus, At: 960, From: "1", To: "10001"}}},
		MirrorWrite{Changed: true})
	check := func(from int64, want string) {
		t.Helper()
		if got, more := actsDue(t, s, root, from, 20); got != want || more {
			t.Fatalf("due (from %d) = %q, more %v; want %q", from, got, more, want)
		}
	}
	check(0, "10001:cw,10003:cw,10006:cw")
	check(501, "10001:cw,10003:cw")
	check(951, "10001:cw")
	replaceActs(t, s, root, 9, nil, commentsOf("10006", 5, 2), worklogsOf("10006", 5, 0, 2))
	replaceActs(t, s, root, 10, nil, commentsOf("10001", 5, 2), commentsOf("10003", 4, 2))
	check(0, "10001:w,10003:cw")
	replaceActs(t, s, root, 11, nil, commentsOf("10003", 5, 2), worklogsOf("10001", 6, 0, 2))
	check(0, "10003:w")
	replaceActs(t, s, root, 12, nil, worklogsOf("10003", 5, 0, 2))
	check(0, "")
	mustWriteRowPage(t, s, root, webPage("10", done(webRow("10001", 7, "a", "21"))), []byte("c2"))
	check(0, "10001:cw")
}

// On a board that plans no sprints, the tickets due are its rows done now
// whose card moved in the window, their comments and every worklog, each
// source until a read for its update stored its receipt; a row not done,
// one that did not move in the window, and a sprint board's done row in no
// sprint are not. Its worklogs are revalidated as a live sprint's are.
func TestMirrorActsDueOnABoardWithoutSprints(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	weeks := webStructure()
	weeks.Board.ID, weeks.Board.PlansSprints, weeks.Sprints = "11", false, nil
	mustReplaceStructure(t, s, root, weeks, true)
	done := func(r MirrorRow) MirrorRow { r.Card.StatusID = "10001"; return r }
	mustWriteRowPage(t, s, root, webPage("11",
		done(webRow("10001", 500, "a")), // done, moved in the window
		done(webRow("10002", 50, "b")),  // done, moved before it
		webRow("10003", 500, "c"),       // to do
	), nil)
	mustWriteRowPage(t, s, root, webPage("10", done(webRow("10004", 500, "d"))), nil)
	check := func(want string) {
		t.Helper()
		if got, more := actsDue(t, s, root, 100, 20); got != want || more {
			t.Fatalf("due = %q, more %v; want %q", got, more, want)
		}
	}
	check("10001:cw")
	replaceActs(t, s, root, 10, nil, commentsOf("10001", 500, 2))
	check("10001:w")
	replaceActs(t, s, root, 11, nil, worklogsOf("10001", 500, 0, 2))
	check("")
	// A week's reviews count through today, so its worklogs are read again
	// a day after their read, as a live sprint's are.
	if got, _ := actsDue(t, s, root, 100, 11+mirrorActsRevalidate.Milliseconds()+1); got != "10001:w" {
		t.Fatalf("due a day after the read = %q; want the worklogs again", got)
	}
}

// A worklog receipt of a ticket the active-sprint arm lists is due again
// a day after its read, its comments not; a closed sprint's member is not
// read again until its card moves.
func TestMirrorActsDueRevalidatesLiveWorklogs(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	done := func(r MirrorRow) MirrorRow { r.Card.StatusID = "10001"; return r }
	mustWriteRowPage(t, s, root, webPage("10", done(webRow("10001", 5, "a", "21")), done(webRow("10003", 5, "c", "20"))), []byte("c1"))
	mustReplaceHistory(t, s, root, MirrorHistory{IssueID: "10003", Changes: []MirrorChange{{Kind: MirrorChangeStatus, At: 900, From: "1", To: "10001"}}},
		MirrorWrite{Changed: true})
	const read = 1_000
	replaceActs(t, s, root, read, nil, commentsOf("10001", 5, 2), worklogsOf("10001", 5, 0, 2), commentsOf("10003", 5, 2), worklogsOf("10003", 5, 0, 2))
	day := mirrorActsRevalidate.Milliseconds()
	for _, c := range []struct {
		now  int64
		want string
	}{{read + day, ""}, {read + day + 1, "10001:w"}} {
		if got, _ := actsDue(t, s, root, 0, c.now); got != c.want {
			t.Fatalf("due at %d = %q; want %q", c.now, got, c.want)
		}
	}
}

// A source whose read failed for its issue alone is not due for a day,
// whatever its card does, and is due again after; the other source keeps
// its own rule.
func TestMirrorActsDueRetryAFailedSourceAfterADay(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	done := func(r MirrorRow) MirrorRow { r.Card.StatusID = "10001"; return r }
	mustWriteRowPage(t, s, root, webPage("10", done(webRow("10001", 5, "a", "21"))), []byte("c1"))
	const tried = 1_000
	replaceActs(t, s, root, tried, []MirrorActsAttempt{{IssueID: "10001", Source: MirrorActComment, Reason: "Jira is busy"}}, worklogsOf("10001", 5, 0, 2))
	day := mirrorActsRetry.Milliseconds()
	if got, _ := actsDue(t, s, root, 0, tried+day); got != "" {
		t.Fatalf("due within a day of the failure = %q", got)
	}
	mustWriteRowPage(t, s, root, webPage("10", done(webRow("10001", 7, "a", "21"))), []byte("c2"))
	if got, _ := actsDue(t, s, root, 0, tried+day); got != "10001:w" {
		t.Fatalf("due after the card moved, within a day = %q; want the worklogs", got)
	}
	if got, _ := actsDue(t, s, root, 0, tried+day+1); got != "10001:cw" {
		t.Fatalf("due a day after the failure = %q", got)
	}
}

// A closed sprint's member whose source a read failed for alone, after
// a read that stored, is due again a day after the failure though its
// card never moves: credit does not count the source while the failure
// stands, so the due rule must not either.
func TestAFailedSourceOfAClosedSprintsMemberIsDueAgainAfterADay(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	done := func(r MirrorRow) MirrorRow { r.Card.StatusID = "10001"; return r }
	mustWriteRowPage(t, s, root, webPage("10", done(webRow("10003", 5, "c", "20"))), []byte("c1"))
	mustReplaceHistory(t, s, root, MirrorHistory{IssueID: "10003", Changes: []MirrorChange{{Kind: MirrorChangeStatus, At: 900, From: "1", To: "10001"}}},
		MirrorWrite{Changed: true})
	replaceActs(t, s, root, 1_000, nil, commentsOf("10003", 5, 2), worklogsOf("10003", 5, 0, 2))
	const tried = 2_000
	replaceActs(t, s, root, tried, []MirrorActsAttempt{{IssueID: "10003", Source: MirrorActWorklog, Reason: "Jira is busy"}})
	day := mirrorActsRetry.Milliseconds()
	for _, c := range []struct {
		now  int64
		want string
	}{{tried + day, ""}, {tried + day + 1, "10003:w"}, {tried + 2*day, "10003:w"}} {
		if got, _ := actsDue(t, s, root, 0, c.now); got != c.want {
			t.Errorf("due at %d = %q; want %q", c.now, got, c.want)
		}
	}
}

// supportBoard is board 10 with every row with no estimate a support
// ticket.
func supportBoard(t *testing.T) MirrorActsBoard {
	t.Helper()
	regroup, err := MirrorRegroup{}.WithSupport(MirrorSupportRule{Boards: []string{"10"}})
	if err != nil {
		t.Fatal(err)
	}
	return MirrorActsBoard{BoardID: "10", Regroup: regroup}
}

// A support ticket is a row the rule names whose card moved since the
// window's start: its worklogs from that start are due, never its
// comments, a row whose card did not move is not listed, and a worklog
// receipt is revalidated a day after its read. A receipt that holds
// worklogs from earlier serves a later start, and a re-read keeps that
// earlier start; a support ticket a closed sprint in the window also
// lists is read whole with its comments.
func TestMirrorActsDueOfSupportTickets(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	done := func(r MirrorRow) MirrorRow { r.Card.StatusID = "10001"; return r }
	points := 3.0
	estimated := webRow("10004", 1200, "d")
	estimated.Estimate = &points
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 1200, "a"), webRow("10002", 300, "b"), done(webRow("10003", 1200, "c", "20")), estimated), []byte("c1"))
	mustReplaceHistory(t, s, root, MirrorHistory{IssueID: "10003", Changes: []MirrorChange{{Kind: MirrorChangeStatus, At: 900, From: "1", To: "10001"}}},
		MirrorWrite{Changed: true})
	board := supportBoard(t)
	check := func(from, now int64, want string) {
		t.Helper()
		if got, _ := actsDue(t, s, root, from, now, board); got != want {
			t.Fatalf("due (from %d at %d) = %q; want %q", from, now, got, want)
		}
	}
	// Sprint 20 completed at 950: a window from 960 leaves it out.
	check(960, 20, "10001:w@960,10003:w@960")
	replaceActs(t, s, root, 30, nil, worklogsOf("10001", 1200, 960, 1000, "w1"), worklogsOf("10003", 1200, 960, 1000))
	check(960, 30, "")
	check(1000, 30, "")
	day := mirrorActsRevalidate.Milliseconds()
	check(1000, 30+day+1, "10001:w@960,10003:w@960")
	check(900, 30, "10003:cw,10001:w@900")
	check(0, 30, "10003:cw,10002:w,10001:w")
}

// Two projects listing one board are each asked their need, and a ticket
// is listed once with the widest: by project A's rule (a label) a support
// ticket's worklogs from the window, by project B's (a type) another's;
// a ticket A counts as support and B as a member of a closed sprint in the
// window is read whole with its comments, at the sprint's place. A row
// neither rule names, or with an estimate, is not a support ticket.
func TestMirrorActsDueTakeTheWidestNeedAcrossProjects(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	labeled := func(r MirrorRow) MirrorRow { r.Card.Labels = []string{"support"}; return r }
	typed := func(r MirrorRow) MirrorRow { r.Card.TypeID = "10004"; return r }
	points := 3.0
	estimated := typed(webRow("10005", 1200, "e"))
	estimated.Estimate = &points
	closed := labeled(webRow("10003", 1200, "c", "20"))
	closed.Card.StatusID = "10001"
	mustWriteRowPage(t, s, root, webPage("10", labeled(webRow("10001", 1200, "a")), typed(webRow("10002", 1200, "b")), closed,
		webRow("10004", 1200, "d"), estimated), []byte("c1"))
	mustReplaceHistory(t, s, root, MirrorHistory{IssueID: "10003", Changes: []MirrorChange{{Kind: MirrorChangeStatus, At: 900, From: "1", To: "10001"}}},
		MirrorWrite{Changed: true})
	rule := func(rule MirrorSupportRule) MirrorActsBoard {
		regroup, err := MirrorRegroup{}.WithSupport(rule)
		if err != nil {
			t.Fatal(err)
		}
		return MirrorActsBoard{BoardID: "10", Regroup: regroup}
	}
	a, b := rule(MirrorSupportRule{Labels: []string{"support"}}), rule(MirrorSupportRule{Types: []string{"10004"}})
	for _, c := range []struct {
		from   int64
		boards []MirrorActsBoard
		want   string
	}{
		{900, []MirrorActsBoard{a}, "10003:cw,10001:w@900"},
		{900, []MirrorActsBoard{b}, "10003:cw,10002:w@900"},
		{900, []MirrorActsBoard{a, b}, "10003:cw,10001:w@900,10002:w@900"},
		{960, []MirrorActsBoard{a, b}, "10001:w@960,10003:w@960,10002:w@960"},
	} {
		if got, _ := actsDue(t, s, root, c.from, 20, c.boards...); got != c.want {
			t.Fatalf("due (from %d, %d projects) = %q; want %q", c.from, len(c.boards), got, c.want)
		}
	}
}

// A listing holds at most a page, and says when more are due.
func TestMirrorActsDueArePaged(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	var rows []MirrorRow
	for i := range mirrorActsDuePage + 1 {
		r := webRow(strconv.Itoa(20000+i), 5, fmt.Sprintf("%05d", i), "21")
		r.Card.StatusID = "10001"
		rows = append(rows, r)
	}
	mustWriteRowPage(t, s, root, webPage("10", rows...), []byte("c1"))
	due, more, err := s.ListMirrorActsDue(root, []MirrorActsBoard{{BoardID: "10"}}, 0, testNow)
	if err != nil || len(due) != mirrorActsDuePage || !more {
		t.Fatalf("%d due, more %v, %v; want a page and more", len(due), more, err)
	}
}

// A worklog write makes the worklog evidence of each issue whose worklog
// it adds, changes, moves or removes due again, superseding an evidence
// read that began before it; an unchanged worklog, and a receipt from a
// later read, are left as they were.
func TestAWorklogWriteMakesItsIssuesEvidenceDue(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	var rows []MirrorRow
	for i := range 6 {
		rows = append(rows, webRow(strconv.Itoa(10001+i), 5, string(rune('a'+i)), "21"))
	}
	mustWriteRowPage(t, s, root, webPage("10", rows...), []byte("c1"))
	logs := func(at int64, ws ...MirrorWorklog) {
		t.Helper()
		if _, err := s.ReplaceMirrorWorklogs(root, 0, "acc-user1", "2026-01-05", "2026-01-06", mirrorReadOf(root, at), ws); err != nil {
			t.Fatal(err)
		}
	}
	w := func(id, issue string, spent int64) MirrorWorklog {
		return MirrorWorklog{ID: id, IssueID: issue, AuthorID: "acc-user1", Day: "2026-01-05", SpentSeconds: spent}
	}
	logs(10, w("w1", "10001", 60), w("w2", "10002", 60), w("w3", "10003", 60), w("w4", "10004", 60))
	var reads []MirrorIssueActs
	for i := range 6 {
		reads = append(reads, worklogsOf(strconv.Itoa(10001+i), 5, 0, 2))
	}
	replaceActs(t, s, root, 20, nil, reads...)
	replaceActs(t, s, root, 40, nil, worklogsOf("10006", 5, 0, 2, "x"))
	// w1 unchanged, w2 changed, w3 moved to 10005, w4 removed, w5 added
	// on 10006, whose receipt is from a later read.
	logs(30, w("w1", "10001", 60), w("w2", "10002", 120), w("w3", "10005", 60), w("w5", "10006", 60))
	assertRows(t, s.db, `SELECT issue_id || ':' || read_for || ':' || read_at FROM tracker_cache_issue_act_reads ORDER BY issue_id`,
		"10001:5:20,10002:0:30,10003:0:30,10004:0:30,10005:0:30,10006:5:40")
	replaceActs(t, s, root, 25, nil, worklogsOf("10002", 5, 0, 2))
	assertRows(t, s.db, `SELECT read_for || ':' || read_at FROM tracker_cache_issue_act_reads WHERE issue_id = '10002'`, "0:30")
}

// The sweep removes an unkept issue's act receipts before its acts, in
// transactions of at most a chunk, so an issue kept again part way reads
// as not read, never as read in part.
func TestTheSweepRemovesActReceiptsBeforeActs(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	unkeptIssueOfSprints(t, s, root, 0)
	linkIssue(t, s, "acts", "10001")
	in := MirrorIssueActs{IssueID: "10001", Source: MirrorActComment, ReadFor: 5}
	for i := range 1200 {
		in.Acts = append(in.Acts, MirrorAct{ID: strconv.Itoa(i), Person: "acc-user1", At: 2})
	}
	if _, err := s.ReplaceMirrorActs(root, 0, mirrorReadOf(root, 10), []MirrorIssueActs{in, {IssueID: "10001", Source: MirrorActWorklog, ReadFor: 5}}, nil, MirrorCatalog{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClearThreadTicket("acts"); err != nil {
		t.Fatal(err)
	}
	counts := `SELECT (SELECT count(*) FROM tracker_cache_issue_act_reads) || ':' || (SELECT count(*) FROM tracker_cache_issue_acts)`
	recorder := recordMirrorWrites(t, s)
	pauses, err := sweepPauses(t, s, context.Background(), 10, map[int]func(){2: func() { assertRows(t, s.db, counts, "0:702") }})
	if err != nil {
		t.Fatal(err)
	}
	if pauses != 3 {
		t.Fatalf("%d sweep transactions; want 3", pauses)
	}
	if most, _ := recorder.mostDeletedInATransaction(); most > clearMirrorChunk {
		t.Fatalf("a sweep transaction deleted %d rows", most)
	}
	assertRows(t, s.db, counts+` || ':' || (SELECT count(*) FROM tracker_cache_issues)`, "0:0:0")
}

// A walk of the issues updated since an instant reads only those: of a
// mirror's 150 issues, all updated at 1000, a walk from later reads none,
// and one from 1000 reads them all.
func TestAnUpdateWalkStartsAtItsBound(t *testing.T) {
	s, root, _ := openSizedMirror(t, benchMirrorSize{boards: 1, issues: 100})
	walk := `SELECT count(*) FROM tracker_cache_issues i WHERE ` + mirrorUpdatedSinceSQL("i", ">=", intParam("?2"))
	for _, c := range []struct {
		from     int64
		want     int
		maxSteps int
	}{{5_000_000, 0, 30}, {1000, 150, 800}} {
		var n int
		if err := s.db.QueryRow(walk, root, c.from).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if steps := statementVMSteps(t, s, walk, root, c.from); n != c.want || steps > c.maxSteps {
			t.Errorf("from %d: %d issues in %d VM steps; want %d in at most %d", c.from, n, steps, c.want, c.maxSteps)
		}
	}
}
