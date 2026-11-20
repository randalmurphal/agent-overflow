package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// Removing a connection detaches its mirror in the removal's own
// transaction and then drains it, never more than a batch per
// transaction; its Tempo connection goes with it.
func TestDeleteTrackerConnectionDrainsItsMirrorInBatches(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	tempo := mustCreateTrackerConnection(t, s, NewTrackerConnection{Kind: TrackerKindTempo, Site: "https://api.eu.tempo.io", ParentID: jira.ID, CreatedAt: 11})
	seedFullMirror(t, s, root)
	rows := make([]MirrorRow, 0, 2*clearMirrorChunk+10)
	for i := range 2*clearMirrorChunk + 10 {
		rows = append(rows, webRow(fmt.Sprintf("2%04d", i), 5, fmt.Sprintf("b%04d", i)))
	}
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: rows}, nil)

	before := mirrorRowTotal(countMirrorRows(t, s.db))
	last, pauses := before, 0
	deleted, err := s.deleteTrackerConnection(jira.ID, func() {
		pauses++
		now := mirrorRowTotal(countMirrorRows(t, s.db))
		if pauses == 1 {
			if now != before {
				t.Errorf("the removal's transaction deleted %d mirror rows; it only detaches the root", before-now)
			}
			if _, err := s.GetTrackerConnection(jira.ID); !errors.Is(err, sql.ErrNoRows) {
				t.Errorf("connection during the drain: %v", err)
			}
			if _, err := s.WriteMirrorRowPage(root, MirrorRowPage{BoardID: "10"}, nil); !errors.Is(err, ErrTrackerCacheGone) {
				t.Errorf("write during the drain: %v", err)
			}
		}
		if last-now > clearMirrorChunk {
			t.Errorf("one transaction deleted %d rows; the bound is %d", last-now, clearMirrorChunk)
		}
		last = now
	})
	if err != nil || !slices.Equal(deleted, []string{jira.ID, tempo.ID}) {
		t.Fatalf("delete = %v, %v; want %s and its Tempo connection %s", deleted, err, jira.ID, tempo.ID)
	}
	if pauses < 6 {
		t.Fatalf("%d pauses for %d rows", pauses, before)
	}
	if counts := countMirrorRows(t, s.db); mirrorRowTotal(counts) != 0 {
		t.Fatalf("after the removal: %v", counts)
	}
	if _, err := s.GetTrackerConnection(tempo.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Tempo connection after the removal: %v", err)
	}
}

// The exported removal drains too, and a connection with no mirror has
// nothing to drain.
func TestDeleteTrackerConnectionLeavesNoMirror(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	seedFullMirror(t, s, root)
	linear := mustCreateTrackerConnection(t, s, NewTrackerConnection{Kind: TrackerKindLinear, Site: "workspace-a", CreatedAt: 12})
	for _, c := range []TrackerConnection{jira, linear} {
		if deleted, err := s.DeleteTrackerConnection(c.ID); err != nil || !slices.Equal(deleted, []string{c.ID}) {
			t.Fatalf("delete %s = %v, %v", c.Kind, deleted, err)
		}
	}
	if counts := countMirrorRows(t, s.db); mirrorRowTotal(counts) != 0 {
		t.Fatalf("after the removals: %v", counts)
	}
}

// A drain that fails after the removal committed keeps the removal and
// leaves a detached root, which the next start's ResumeMirrorClears
// finishes.
func TestDeleteTrackerConnectionDrainFailureIsResumed(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	seedFullMirror(t, s, root)
	mustExec(t, s.db, `CREATE TRIGGER test_refuse_drain BEFORE DELETE ON tracker_cache_users BEGIN SELECT RAISE(ABORT, 'drain refused'); END`)
	deleted, err := s.DeleteTrackerConnection(jira.ID)
	if !errors.Is(err, ErrTrackerCacheDrain) || !slices.Equal(deleted, []string{jira.ID}) {
		t.Fatalf("delete with the drain refused = %v, %v; want the deleted ID and ErrTrackerCacheDrain", deleted, err)
	}
	if _, err := s.GetTrackerConnection(jira.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("connection after the failed drain: %v", err)
	}
	assertRows(t, s.db, `SELECT id || ':' || coalesce(connection_id, '-') FROM tracker_cache_roots`, root+":-")
	if _, err := s.SetMirrorSyncRead(root, "10", 1); !errors.Is(err, ErrTrackerCacheGone) {
		t.Fatalf("write to the detached root: %v", err)
	}
	mustExec(t, s.db, `DROP TRIGGER test_refuse_drain`)
	if resumed, err := s.ResumeMirrorClears(nil); err != nil || !slices.Equal(resumed, []string{root}) {
		t.Fatalf("resume = %v, %v", resumed, err)
	}
	if counts := countMirrorRows(t, s.db); mirrorRowTotal(counts) != 0 {
		t.Fatalf("after the resume: %v", counts)
	}
}

// Each way a thread's link goes (an unlink, the thread's delete, its
// retire to a holder, a move to another computer) leaves its issue's card
// until the sweep, which removes it; an issue a link still keeps, and one
// a board row keeps, stay.
func TestPruneUnkeptMirrorIssuesSweepsLostLinks(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	linkIssue(t, s, "unlinked", "10001")
	linkIssue(t, s, "deleted", "10002")
	seedLinearSource(t, s, "retired", 1)
	mustSetThreadTicket(t, s, ThreadTicket{ThreadID: "retired", Ticket: TicketRef{Kind: TrackerKindJira, Site: mirrorSite, IssueID: "10003"},
		IssueKey: "WEB-3", Origin: TicketOriginManual, LinkedAt: 10}, true)
	mustPointerFork(t, s, "retired", "retired-fork", ForkCut{})
	linkIssue(t, s, "moved", "10004")
	linkIssue(t, s, "kept", "10005")
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{webRow("10006", 5, "a")}}, nil)
	cards := []MirrorCard{webCard("10001", 5), webCard("10002", 5), webCard("10003", 5), webCard("10004", 5), webCard("10005", 5)}
	if changes, err := s.UpsertMirrorCards(root, cards, MirrorCatalog{}); err != nil || len(changes.Cards) != len(cards) {
		t.Fatalf("linked cards = %+v, %v", changes, err)
	}

	if _, err := s.ClearThreadTicket("unlinked"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteThread("deleted"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteThread("retired"); err != nil {
		t.Fatal(err)
	}
	assertRows(t, s.db, `SELECT mode FROM threads WHERE id = 'retired'`, "holder")
	seedTransfer(t, s, "moved", "outgoing", "move", "complete", 0)
	assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_issues ORDER BY 1`, "10001,10002,10003,10004,10005,10006")

	pruned, next, more, err := s.PruneUnkeptMirrorIssues(root, "")
	if err != nil || !slices.Equal(pruned, []string{"10001", "10002", "10003", "10004"}) || next != "10005" || more {
		t.Fatalf("sweep = %v, next %q, more %v, %v", pruned, next, more, err)
	}
	assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_issues ORDER BY 1`, "10005,10006")
	if pruned, next, more, err := s.PruneUnkeptMirrorIssues(root, ""); err != nil || pruned != nil || next != "10005" || more {
		t.Fatalf("second sweep = %v, next %q, more %v, %v", pruned, next, more, err)
	}
}

// A sweep reads a page at a time by ID, never an issue a board row keeps,
// and continues after the last issue it read: a kept issue ending a page
// is not read again. Its pages together remove every unkept issue.
func TestPruneUnkeptMirrorIssuesPages(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	// Every 50th issue has a board row: 2*page issues have none.
	n := 2*mirrorPruneSweepPage + 9
	mustExec(t, s.db, `WITH RECURSIVE i(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM i WHERE n < ?)
		INSERT INTO tracker_cache_issues(root_id, issue_id, issue_key, updated_at)
		SELECT ?, printf('1%04d', n), printf('WEB-%d', n), 5 FROM i`, n, root)
	var kept []string
	var rows []MirrorRow
	for i := 1; i <= n; i += 50 {
		kept = append(kept, fmt.Sprintf("1%04d", i))
		rows = append(rows, webRow(fmt.Sprintf("1%04d", i), 5, "a"))
	}
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: rows}, nil)
	boardless, err := queryIDs(s.db, `SELECT issue_id FROM tracker_cache_issues WHERE board_rows = 0 ORDER BY issue_id`)
	if err != nil || len(boardless) != 2*mirrorPruneSweepPage {
		t.Fatalf("%d issues with no board row, %v; want %d", len(boardless), err, 2*mirrorPruneSweepPage)
	}
	// The issue that ends the first page is linked, so it stays.
	linked := boardless[mirrorPruneSweepPage-1]
	linkIssue(t, s, "linked", linked)
	kept = append(kept, linked)
	slices.Sort(kept)

	var all []string
	after, calls := "", 0
	for {
		pruned, next, more, err := s.PruneUnkeptMirrorIssues(root, after)
		if err != nil {
			t.Fatal(err)
		}
		calls++
		all = append(all, pruned...)
		if calls == 1 && next != linked {
			t.Fatalf("first page ends at %s; want %s", next, linked)
		}
		if !more {
			break
		}
		after = next
	}
	if calls != 2 {
		t.Fatalf("%d calls; want 2 pages of %d", calls, mirrorPruneSweepPage)
	}
	if len(all) != len(boardless)-1 || !slices.IsSorted(all) {
		t.Fatalf("swept %d issues; want %d, in order", len(all), len(boardless)-1)
	}
	assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_issues ORDER BY 1`, strings.Join(kept, ","))
}
