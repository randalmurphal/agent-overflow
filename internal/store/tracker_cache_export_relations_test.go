package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// dayMS is the instant day (YYYY-MM-DD) begins in UTC.
func dayMS(t *testing.T, day string) int64 {
	t.Helper()
	d, err := time.Parse(time.DateOnly, day)
	if err != nil {
		t.Fatal(err)
	}
	return d.UnixMilli()
}

// relationsFixture is the pace fixture's store with board 10's sprints
// starting on 1 November 2026, the connection's verified account "me", and
// these links and worklogs:
//
//	threads: A linked to 10001 with two turns, B archived on 10002, C (a
//	scratch, hidden) on 10003, D on 10004 (board 11's), E on 10001 of
//	another site, F being deleted on 10005
//	worklogs: me on 10001 on 31 October and 20 November, on 10004 on 20
//	November, on 10002 on 30 October; someone else on 10001 on 20 November
//	read days: me on 30 and 31 October, 19 and 20 November and in 2025;
//	someone else on 21 November
func relationsFixture(t *testing.T) (*Store, int64) {
	t.Helper()
	s, root := paceFixture(t)
	mustExec(t, s.db, `UPDATE tracker_connections SET verified_account_id = 'me' WHERE id = (SELECT connection_id FROM tracker_cache_roots WHERE id = ?)`, root)
	mustExec(t, s.db, `UPDATE tracker_cache_sprints SET start_at = ? WHERE root_id = ?`, dayMS(t, "2026-11-01"), root)
	for _, th := range []struct {
		id, mode       string
		archived, gone int
	}{{"A", "chat", 0, 0}, {"B", "chat", 1, 0}, {"C", "scratch", 0, 0}, {"D", "chat", 0, 0}, {"E", "chat", 0, 0}, {"F", "chat", 0, 1}} {
		mustExec(t, s.db, `INSERT INTO threads(id, title, provider, workspace_path, mode, archived, deleting, created_at, updated_at)
			VALUES (?, 'Thread ' || ?, 'claude', '/p', ?, ?, ?, 10, 10)`, th.id, th.id, th.mode, th.archived, th.gone)
	}
	mustExec(t, s.db, `INSERT INTO turns(turn_id, thread_id, turn_index, started_at) VALUES ('a0', 'A', 0, 100), ('a1', 'A', 1, 200)`)
	for _, l := range []struct{ thread, site, issue, origin string }{
		{"A", mirrorSite, "10001", "start_work"}, {"B", mirrorSite, "10002", "manual"}, {"C", mirrorSite, "10003", "ask"},
		{"D", mirrorSite, "10004", "manual"}, {"E", "https://other.atlassian.net", "10001", "manual"}, {"F", mirrorSite, "10005", "agent"},
	} {
		mustExec(t, s.db, `INSERT INTO thread_tickets(thread_id, kind, site, issue_id, issue_key, origin, linked_at) VALUES (?, 'jira', ?, ?, 'WEB-' || ?, ?, 50)`,
			l.thread, l.site, l.issue, l.issue, l.origin)
	}
	for _, w := range []struct{ id, issue, author, day string }{
		{"w1", "10001", "me", "2026-10-31"}, {"w2", "10001", "me", "2026-11-20"}, {"w3", "10004", "me", "2026-11-20"},
		{"w4", "10002", "me", "2026-10-30"}, {"w5", "10001", "someone", "2026-11-20"},
	} {
		mustExec(t, s.db, `INSERT INTO tracker_cache_worklogs(root_id, worklog_id, issue_id, author_id, day, spent_seconds) VALUES (?, ?, ?, ?, ?, 3600)`,
			root, w.id, w.issue, w.author, w.day)
	}
	for _, d := range []struct{ author, day string }{
		{"me", "2026-10-30"}, {"me", "2026-10-31"}, {"me", "2026-11-19"}, {"me", "2026-11-20"}, {"me", "2025-06-01"}, {"someone", "2026-11-21"},
	} {
		mustExec(t, s.db, `INSERT INTO tracker_cache_worklog_days(root_id, author_id, day, read_at) VALUES (?, ?, ?, 1)`, root, d.author, d.day)
	}
	return s, root
}

// The export holds the threads of this computer linked to the board's
// tickets, as the Ticket screen lists them (no hidden mode, none being
// deleted, none of another site or board), with each one's newest turn;
// and the verified account's logged time on the board's tickets and its
// read days, from a day before the earliest instant a count can ask.
func TestTheExportHoldsTheBoardsThreadsAndYourLoggedTime(t *testing.T) {
	t.Parallel()
	s, root := relationsFixture(t)
	conn := exportConn(t)
	window := dayMS(t, "2026-11-10")
	if _, ok, err := s.ExportMirrorBoard(context.Background(), conn, root, "10", MirrorRegroup{}, []string{"threads", "worklogs", "worklog_days"}, window, CardShapeForTest); err != nil || !ok {
		t.Fatalf("export: %v, %v", ok, err)
	}
	threads := exportRows(t, conn, `SELECT id || '|' || ticket || '|' || origin || '|' || linked_at || '|' || created_at || '|' || archived || '|' || coalesce(last_turn_at, '-')
		FROM threads ORDER BY id`)
	if want := []string{"A|10001|start_work|50|10|0|200", "B|10002|manual|50|10|1|-"}; !reflect.DeepEqual(threads, want) {
		t.Fatalf("threads = %v; want %v", threads, want)
	}
	worklogs := exportRows(t, conn, `SELECT id || '|' || ticket || '|' || day || '|' || seconds FROM worklogs ORDER BY id`)
	if want := []string{"w1|10001|2026-10-31|3600", "w2|10001|2026-11-20|3600"}; !reflect.DeepEqual(worklogs, want) {
		t.Fatalf("worklogs = %v; want %v", worklogs, want)
	}
	if got, want := exportRows(t, conn, `SELECT day FROM worklog_days ORDER BY day`), []string{"2026-10-31", "2026-11-19", "2026-11-20"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("read days = %v; want %v", got, want)
	}
}

// A connection with no verified account has no logged time in the export.
func TestTheExportHoldsNoLoggedTimeWithoutAnAccount(t *testing.T) {
	t.Parallel()
	s, root := relationsFixture(t)
	mustExec(t, s.db, `UPDATE tracker_connections SET verified_account_id = '' WHERE id = (SELECT connection_id FROM tracker_cache_roots WHERE id = ?)`, root)
	conn := exportConn(t)
	if _, ok, err := s.ExportMirrorBoard(context.Background(), conn, root, "10", MirrorRegroup{}, []string{"worklogs", "worklog_days"}, 0, CardShapeForTest); err != nil || !ok {
		t.Fatalf("export: %v, %v", ok, err)
	}
	if got := exportRows(t, conn, `SELECT (SELECT count(*) FROM worklogs) || '|' || (SELECT count(*) FROM worklog_days)`); got[0] != "0|0" {
		t.Fatalf("worklogs|days = %s; want none", got[0])
	}
}
