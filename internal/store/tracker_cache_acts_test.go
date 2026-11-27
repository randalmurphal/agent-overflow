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
		mustExec(t, db, `INSERT OR REPLACE INTO tracker_cache_issue_act_reads(root_id, issue_id, source, read_for, read_at) VALUES
			(7, '10001', 1, 5, 1), (7, '10001', 2, 5, 1), (8, '10001', 1, 5, 1), (8, '10001', 2, 5, 1)`)
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

const storedActsSQL = `SELECT issue_id || ':' || source || ':' || act_id || ':' || person || ':' || at || ':' || seconds
	FROM tracker_cache_issue_acts ORDER BY issue_id, source, act_id`

const storedActReadsSQL = `SELECT issue_id || ':' || source || ':' || read_for || ':' || read_at
	FROM tracker_cache_issue_act_reads ORDER BY issue_id, source`

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
	replace := func(at int64, in ...MirrorIssueActs) []MirrorActsResult {
		t.Helper()
		results, _, err := s.ReplaceMirrorActs(root, 0, mirrorReadOf(root, at), in, MirrorCatalog{})
		if err != nil {
			t.Fatal(err)
		}
		return results
	}
	worklogs := MirrorIssueActs{IssueID: "10001", Source: MirrorActWorklog, ReadFor: 5, Acts: []MirrorAct{{ID: "w1", Person: "acc-user2", At: 3, Seconds: 600}}}
	catalog := MirrorCatalog{Users: []MirrorUser{{ID: "acc-user2", Name: "User 2"}}}
	results, catalogWrite, err := s.ReplaceMirrorActs(root, 0, mirrorReadOf(root, 10), []MirrorIssueActs{commentsOf("10001", 5, 2, "c1", "c2"), worklogs,
		commentsOf("99999", 5, 2, "c9")}, catalog)
	if err != nil || !catalogWrite.Changed {
		t.Fatalf("first replace: %v, catalog %+v", err, catalogWrite)
	}
	if want := []MirrorActsResult{{"10001", 1, true, false}, {"10001", 2, true, false}, {"99999", 1, false, false}}; fmt.Sprint(results) != fmt.Sprint(want) {
		t.Fatalf("results = %+v; want %+v", results, want)
	}
	assertRows(t, s.db, storedActsSQL, "10001:1:c1:acc-user1:2:0,10001:1:c2:acc-user1:2:0,10001:2:w1:acc-user2:3:600")
	assertRows(t, s.db, storedActReadsSQL, "10001:1:5:10,10001:2:5:10")
	assertRows(t, s.db, `SELECT name FROM tracker_cache_users WHERE user_id = 'acc-user2'`, "User 2")

	changed := commentsOf("10001", 6, 2, "c2", "c3")
	changed.Acts[0].At = 4
	if got := replace(11, changed); !got[0].Stored {
		t.Fatalf("second replace = %+v", got)
	}
	assertRows(t, s.db, storedActsSQL, "10001:1:c2:acc-user1:4:0,10001:1:c3:acc-user1:2:0,10001:2:w1:acc-user2:3:600")
	assertRows(t, s.db, storedActReadsSQL, "10001:1:6:11,10001:2:5:10")

	for _, at := range []int64{11, 9} {
		if got := replace(at, commentsOf("10001", 7, 2)); got[0].Stored || !got[0].Superseded {
			t.Fatalf("a read begun at %d = %+v; want it superseded", at, got)
		}
	}
	assertRows(t, s.db, storedActReadsSQL, "10001:1:6:11,10001:2:5:10")
	if got := replace(12, commentsOf("10001", 7, 2)); !got[0].Stored {
		t.Fatalf("a read that found no comment = %+v", got)
	}
	assertRows(t, s.db, storedActsSQL, "10001:2:w1:acc-user2:3:600")
}

// A replace refuses what it cannot store as given, before writing
// anything.
func TestMirrorActsRefuseAnInvalidReplace(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 5, "a", "21")), []byte("c1"))
	comment := commentsOf("10001", 5, 2, "c1")
	for name, in := range map[string][]MirrorIssueActs{
		"no issue":            {commentsOf("", 5, 2)},
		"no source":           {{IssueID: "10001", ReadFor: 5}},
		"no update time":      {commentsOf("10001", 0, 2)},
		"an issue twice":      {comment, comment},
		"an act twice":        {commentsOf("10001", 5, 2, "c1", "c1")},
		"an act with no time": {commentsOf("10001", 5, 0, "c1")},
		"a comment's seconds": {{IssueID: "10001", Source: MirrorActComment, ReadFor: 5, Acts: []MirrorAct{{ID: "c1", Person: "acc-user1", At: 2, Seconds: 1}}}},
		"no author":           {{IssueID: "10001", Source: MirrorActWorklog, ReadFor: 5, Acts: []MirrorAct{{ID: "w1", At: 2, Seconds: 1}}}},
	} {
		if _, _, err := s.ReplaceMirrorActs(root, 0, mirrorReadOf(root, 10), in, MirrorCatalog{}); !errors.Is(err, ErrInvalidTrackerCache) {
			t.Errorf("%s: %v; want it refused", name, err)
		}
	}
	if _, _, err := s.ReplaceMirrorActs(root, 0, MirrorRead{}, []MirrorIssueActs{comment}, MirrorCatalog{}); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Errorf("no read: %v; want it refused", err)
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_acts`, "0")
}

// A worklog read under an earlier time source generation is refused in
// the write's transaction, comments of the same call included; a call of
// comments alone names no generation.
func TestMirrorActsFollowTheirTimeSource(t *testing.T) {
	t.Parallel()
	s, c, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 5, "a", "21")), []byte("c1"))
	mustCreateTrackerConnection(t, s, NewTrackerConnection{Kind: TrackerKindTempo, Site: "https://api.tempo.io", ParentID: c.ID, Label: "Tempo", CreatedAt: 11})
	worklogs := MirrorIssueActs{IssueID: "10001", Source: MirrorActWorklog, ReadFor: 5, Acts: []MirrorAct{{ID: "w1", Person: "acc-user1", At: 3, Seconds: 60}}}
	if _, _, err := s.ReplaceMirrorActs(root, 0, mirrorReadOf(root, 10), []MirrorIssueActs{commentsOf("10001", 5, 2, "c1"), worklogs}, MirrorCatalog{}); !errors.Is(err, ErrTrackerCacheSourceChanged) {
		t.Fatalf("a worklog read under generation 0: %v", err)
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_act_reads`, "0")
	if _, _, err := s.ReplaceMirrorActs(root, 0, mirrorReadOf(root, 10), []MirrorIssueActs{commentsOf("10001", 5, 2, "c1")}, MirrorCatalog{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReplaceMirrorActs(root, 1, mirrorReadOf(root, 11), []MirrorIssueActs{worklogs}, MirrorCatalog{}); err != nil {
		t.Fatal(err)
	}
	assertRows(t, s.db, storedActReadsSQL, "10001:1:5:10,10001:2:5:11")
}

// A call of more acts than one transaction writes is written in several,
// an issue's acts of a source never split, and returns every outcome.
func TestMirrorActsSplitAcrossTransactions(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 5, "a", "21"), webRow("10002", 5, "b", "21")), []byte("c1"))
	many := func(issue string) MirrorIssueActs {
		in := MirrorIssueActs{IssueID: issue, Source: MirrorActComment, ReadFor: 5}
		for i := range 1500 {
			in.Acts = append(in.Acts, MirrorAct{ID: strconv.Itoa(i), Person: "acc-user1", At: 2})
		}
		return in
	}
	before := s.CommitCount()
	results, _, err := s.ReplaceMirrorActs(root, 0, mirrorReadOf(root, 10), []MirrorIssueActs{many("10001"), many("10002")}, MirrorCatalog{})
	if err != nil || !results[0].Stored || !results[1].Stored {
		t.Fatalf("results %+v, %v", results, err)
	}
	if n := s.CommitCount() - before; n != 2 {
		t.Fatalf("%d transactions; want 2", n)
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_acts`, "3000")
}

// actsDue lists root's acts due on board 10 by its plain groups, from
// window start from, and returns the issues with support marked.
func actsDue(t *testing.T, s *Store, root int64, from int64) (string, bool) {
	t.Helper()
	due, more, err := s.ListMirrorActsDue(root, []MirrorActsBoard{{BoardID: "10"}, {BoardID: "10"}, {BoardID: "11"}}, from)
	if err != nil {
		t.Fatal(err)
	}
	out := ""
	for i, d := range due {
		if i > 0 {
			out += ","
		}
		out += d.IssueID
		if d.Support {
			out += "*"
		}
	}
	return out, more
}

// The acts due on a board: its active sprints' members done now, first,
// then its closed sprints' members done at the completion, newest
// completion first, whose sprint completed in the window, each once, until a read for their update
// stored a receipt of its comments and of its worklogs. A member carried on from a closed
// sprint whose history is unread is not done there yet, and a board the
// mirror does not show (11) lists nothing.
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
		if got, more := actsDue(t, s, root, from); got != want || more {
			t.Fatalf("due (from %d) = %q, more %v; want %q", from, got, more, want)
		}
	}
	check(0, "10001,10003,10006")
	check(501, "10001,10003")
	check(951, "10001")
	replace := func(at int64, in ...MirrorIssueActs) {
		t.Helper()
		if _, _, err := s.ReplaceMirrorActs(root, 0, mirrorReadOf(root, at), in, MirrorCatalog{}); err != nil {
			t.Fatal(err)
		}
	}
	replace(9, commentsOf("10006", 5, 2), MirrorIssueActs{IssueID: "10006", Source: MirrorActWorklog, ReadFor: 5})
	replace(10, commentsOf("10001", 5, 2), commentsOf("10003", 4, 2))
	check(0, "10001,10003")
	replace(11, commentsOf("10003", 5, 2), MirrorIssueActs{IssueID: "10001", Source: MirrorActWorklog, ReadFor: 6})
	check(0, "10003")
	replace(12, MirrorIssueActs{IssueID: "10003", Source: MirrorActWorklog, ReadFor: 5})
	check(0, "")
	mustWriteRowPage(t, s, root, webPage("10", done(webRow("10001", 7, "a", "21"))), []byte("c2"))
	check(0, "10001")
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
	due, more, err := s.ListMirrorActsDue(root, []MirrorActsBoard{{BoardID: "10"}}, 0)
	if err != nil || len(due) != mirrorActsDuePage || !more {
		t.Fatalf("%d due, more %v, %v; want a page and more", len(due), more, err)
	}
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
	if _, _, err := s.ReplaceMirrorActs(root, 0, mirrorReadOf(root, 10), []MirrorIssueActs{in, {IssueID: "10001", Source: MirrorActWorklog, ReadFor: 5}}, MirrorCatalog{}); err != nil {
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
