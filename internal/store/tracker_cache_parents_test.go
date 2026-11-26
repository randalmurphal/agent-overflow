package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// mirrorParentNamesSQL lists the parents' names a store holds, each as
// issue:key:title:level@read.
const mirrorParentNamesSQL = `SELECT issue_id || ':' || issue_key || ':' || title || ':' || type_level || '@' || read_at
	FROM tracker_cache_parents ORDER BY root_id, issue_id`

// namedChildRow is a row of issue id, of update time updated, whose card's
// read names its parent with title.
func namedChildRow(id string, updated int64, parent, title string, level int) MirrorRow {
	r := webRow(id, updated, "a"+id)
	r.Card.ParentID, r.Card.ParentName = parent, &MirrorParentName{Key: "WEB-" + parent, Title: title, TypeLevel: level}
	return r
}

// writeParentPage writes rows to board 10 as a page read at readAt.
func writeParentPage(t *testing.T, s *Store, root, readAt int64, rows ...MirrorRow) MirrorChanges {
	t.Helper()
	page := webPage("10", rows...)
	page.ReadAt = mirrorReadAt(readAt)
	return mustWriteRowPage(t, s, root, page, nil)
}

// A parent's name is the newest read's: a child's read writes its parent's
// row whether or not the mirror keeps the parent, a later read naming the
// same writes nothing, a later rename replaces it, and an earlier read
// whose card is admitted for its later update leaves it. A card naming
// its parent by ID alone writes no row and leaves the one there. The
// parent's own card writes its row, in the transaction that stores the
// card, so the kept parent's record and its name agree. A card the write
// does not store (unkept, or older than the stored one) writes no name.
func TestMirrorParentNamesTakeTheNewestRead(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	step := func(name string, ch MirrorChanges, parents []string, names string) {
		t.Helper()
		if !reflect.DeepEqual(ch.Parents, parents) {
			t.Errorf("%s: changed parents %v; want %v", name, ch.Parents, parents)
		}
		assertRows(t, s.db, mirrorParentNamesSQL, names)
	}
	step("a child's read", writeParentPage(t, s, root, 2, namedChildRow("1", 5, "30", "Checkout", 1)),
		[]string{"30"}, "30:WEB-30:Checkout:1@2")
	step("a sibling read later naming the same", writeParentPage(t, s, root, 3, namedChildRow("2", 5, "30", "Checkout", 1)),
		nil, "30:WEB-30:Checkout:1@2")
	step("a rename read through a child", writeParentPage(t, s, root, 4, namedChildRow("2", 6, "30", "Payments", 1)),
		[]string{"30"}, "30:WEB-30:Payments:1@4")
	older := writeParentPage(t, s, root, 3, namedChildRow("1", 7, "30", "Checkout", 1))
	if !reflect.DeepEqual(older.Cards, []string{"1"}) {
		t.Fatalf("an earlier read of a later update stored cards %v; want its card", older.Cards)
	}
	step("an earlier read whose card is admitted", older, nil, "30:WEB-30:Payments:1@4")

	byID := webRow("3", 5, "a3")
	byID.Card.ParentID = "30"
	step("a card naming a named parent by ID alone", writeParentPage(t, s, root, 5, byID), nil, "30:WEB-30:Payments:1@4")
	newByID := webRow("4", 5, "a4")
	newByID.Card.ParentID = "40"
	step("a card naming a new parent by ID alone", writeParentPage(t, s, root, 6, newByID), nil, "30:WEB-30:Payments:1@4")
	if got, err := s.ListMirrorParents(root, []string{"40", "30"}); err != nil ||
		!reflect.DeepEqual(got, []MirrorParent{{IssueID: "30", MirrorParentName: MirrorParentName{Key: "WEB-30", Title: "Payments", TypeLevel: 1}}}) {
		t.Fatalf("the names of a parent no board lists and one never named = %+v, %v; want the first's", got, err)
	}

	epic := webRow("30", 5, "a30")
	epic.Card.Key, epic.Card.Title, epic.Card.TypeLevel = "WEB-30", "Billing", 1
	step("the parent's own card", writeParentPage(t, s, root, 7, epic), []string{"30"}, "30:WEB-30:Billing:1@7")
	assertRows(t, s.db, `SELECT i.title || '|' || n.title || '|' || i.issue_key || '|' || n.issue_key FROM tracker_cache_issues i
		JOIN tracker_cache_parents n ON n.root_id = i.root_id AND n.issue_id = i.issue_id`, "Billing|Billing|WEB-30|WEB-30")
	epic.Card.UpdatedAt, epic.Card.Title = 6, "Renamed"
	step("the parent's own card renamed", writeParentPage(t, s, root, 8, epic), []string{"30"}, "30:WEB-30:Renamed:1@8")
	assertRows(t, s.db, `SELECT i.title || '|' || n.title FROM tracker_cache_issues i
		JOIN tracker_cache_parents n ON n.root_id = i.root_id AND n.issue_id = i.issue_id`, "Renamed|Renamed")

	stale := namedChildRow("1", 6, "30", "Stale", 1)
	ch := writeParentPage(t, s, root, 9, stale)
	if !reflect.DeepEqual(ch.Superseded, []string{"1"}) {
		t.Fatalf("an older card = %+v; want it superseded", ch)
	}
	step("a superseded card", ch, nil, "30:WEB-30:Renamed:1@8")
	unkept := webCard("99", 5)
	unkept.ParentID, unkept.ParentName = "50", &MirrorParentName{Key: "WEB-50", Title: "Unkept's parent"}
	ch, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{unkept}, MirrorCatalog{}, mirrorReadAt(10))
	if err != nil || !reflect.DeepEqual(ch.Unkept, []string{"99"}) {
		t.Fatalf("an unkept card = %+v, %v; want it unkept", ch, err)
	}
	step("a card the mirror does not keep", ch, nil, "30:WEB-30:Renamed:1@8")

	nameless := webCard("98", 5)
	nameless.ParentName = &MirrorParentName{Key: "WEB-50"}
	if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{nameless}, MirrorCatalog{}, mirrorReadAt(11)); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Fatalf("a parent's name without the parent: %v; want it refused", err)
	}
}

// The cleanup deletes each parent's name no stored issue names, after the
// issues it sweeps: a name only a swept issue named, and the old parent
// of a child read under another, go; a name a stored issue names stays.
// A name with nothing else pending is cleanup due on its own. The delete
// runs a chunk a transaction, each after the cleanup's pause.
func TestMirrorCleanupDeletesTheNamesNoIssueNames(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	writeParentPage(t, s, root, 2, namedChildRow("1", 5, "30", "Checkout", 1), namedChildRow("2", 5, "40", "Search", 1),
		namedChildRow("3", 5, "40", "Search", 1))
	page := webPage("10")
	page.ReadAt, page.Missing = mirrorReadAt(3), []string{"1"}
	mustWriteRowPage(t, s, root, page, nil)
	writeParentPage(t, s, root, 4, namedChildRow("2", 6, "50", "Profile", 1))
	assertRows(t, s.db, mirrorParentNamesSQL, "30:WEB-30:Checkout:1@2,40:WEB-40:Search:1@2,50:WEB-50:Profile:1@4")
	if err := s.FinishMirrorCleanup(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_issues ORDER BY issue_id`, "2,3")
	assertRows(t, s.db, mirrorParentNamesSQL, "40:WEB-40:Search:1@2,50:WEB-50:Profile:1@4")

	orphans := 2*clearMirrorChunk + 1
	mustExec(t, s.db, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO tracker_cache_parents(root_id, issue_id, issue_key, title, type_level, read_at)
		SELECT ?, 'o' || i, 'WEB-o' || i, 'Orphan', 0, 1 FROM n`, orphans, root)
	var left []int
	pause := func() {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM tracker_cache_parents`).Scan(&n); err != nil {
			t.Error(err)
		}
		left = append(left, n)
	}
	if err := s.FinishMirrorCleanup(context.Background(), pause); err != nil {
		t.Fatal(err)
	}
	if want := []int{orphans + 2, clearMirrorChunk + 3, 3}; !reflect.DeepEqual(left, want) {
		t.Fatalf("names before each cleanup transaction = %v; want %v, a chunk deleted in each", left, want)
	}
	assertRows(t, s.db, mirrorParentNamesSQL, "40:WEB-40:Search:1@2,50:WEB-50:Profile:1@4")
}

// The statements that read a parent's name find it by key: the names read,
// the card write's probe of the parent's own row, and the export's
// tickets and epics fills.
func TestMirrorParentNamesAreReadByKey(t *testing.T) {
	t.Parallel()
	s, _, _ := newMirror(t)
	byKey := func(name, query string, wants ...string) {
		t.Helper()
		plan := explainMirror(t, s, query)
		for _, want := range wants {
			if !strings.Contains(plan, want) {
				t.Errorf("%s:\n%s\nwant %q", name, plan, want)
			}
		}
	}
	byKey("the names read", mirrorParentsSQL, "SEARCH n USING PRIMARY KEY (root_id=? AND issue_id=?)")
	byKey("the card write", mirrorLinkedCards.parents, "SEARCH n EXISTS USING PRIMARY KEY (root_id=? AND issue_id=?)")
	byKey("the name sweep's probe", mirrorParentsSweepSQL, "SEARCH c USING COVERING INDEX idx_tracker_cache_issues_parent (root_id=? AND parent_id=?)")

	conn := exportConn(t)
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, MirrorExportSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `ATTACH ? AS `+mirrorExportSchemaName, mirrorExportURI(s.path)); err != nil {
		t.Fatal(err)
	}
	wants := map[string][]string{
		"tickets": {"SEARCH n USING PRIMARY KEY (root_id=? AND issue_id=?)", "SEARCH gn USING PRIMARY KEY (root_id=? AND issue_id=?)"},
		"epics":   {"SEARCH e USING PRIMARY KEY (root_id=? AND issue_id=?)"},
	}
	for _, fill := range mirrorExportFills {
		if wants[fill.name] == nil {
			continue
		}
		rows, err := conn.QueryContext(ctx, `EXPLAIN QUERY PLAN `+fill.sql, 1, "10", "{}", 0, 1)
		if err != nil {
			t.Fatalf("plan %s: %v", fill.name, err)
		}
		var steps []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			steps = append(steps, detail)
		}
		if err := errorsOf(rows); err != nil {
			t.Fatal(err)
		}
		plan := strings.Join(steps, "\n")
		for _, want := range wants[fill.name] {
			if !strings.Contains(plan, want) {
				t.Errorf("the export's %s:\n%s\nwant %q", fill.name, plan, want)
			}
		}
		delete(wants, fill.name)
	}
	if len(wants) > 0 {
		t.Errorf("no export fill named %v", wants)
	}
}

// An export puts each ticket under its epic by the parents' names: a
// story under its epic whether or not a board lists the epic, a sub-task
// under its story's epic when its story is on a board, a sub-task whose
// story no board lists under no epic, and a story under a parent that is
// not an epic under none. The epics fill holds each epic once, named.
func TestMirrorExportNamesEpicsFromTheParents(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	story := namedChildRow("1", 5, "30", "Checkout", 1)
	story.Card.TypeLevel = 0
	subtask := namedChildRow("2", 5, "1", "Issue 1", 0)
	subtask.Card.TypeLevel = -1
	loose := namedChildRow("3", 5, "60", "Unlisted story", 0)
	loose.Card.TypeLevel = -1
	feature := namedChildRow("4", 5, "70", "A feature", 0)
	writeParentPage(t, s, root, 2, story, subtask, loose, feature, webRow("5", 5, "a5"))
	conn := exportConn(t)
	if _, ok, err := s.ExportMirrorBoard(context.Background(), conn, root, "10", MirrorRegroup{}, []string{"tickets", "epics"}); err != nil || !ok {
		t.Fatalf("export = %v, %v", ok, err)
	}
	if got, want := exportRows(t, conn, `SELECT id || '|' || coalesce(parent, '-') || '|' || coalesce(epic, '-') FROM tickets ORDER BY id`),
		[]string{"1|30|30", "2|1|30", "3|60|-", "4|70|-", "5|-|-"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tickets' parents and epics = %v; want %v", got, want)
	}
	if got, want := exportRows(t, conn, `SELECT id || '|' || key || '|' || title FROM epics ORDER BY id`), []string{"30|WEB-30|Checkout"}; !reflect.DeepEqual(got, want) {
		t.Errorf("epics = %v; want %v", got, want)
	}
}

// v172 makes tracker_cache_parents and names, from the stored cards, each
// parent the mirror keeps, as a view named it before: a parent on a shown
// board, and one a link keeps. A parent the mirror stores but does not
// keep, and one it does not hold, stay unnamed. Every board's cursor is
// forgotten, and nothing else of its sync state moves. The table refuses
// a row with no key or no read.
func TestTrackerParentsMigrationNamesTheKeptParents(t *testing.T) {
	t.Parallel()
	db := migrateThrough(t, trackerParentsMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects(id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	mustExec(t, db, `INSERT INTO tracker_connections(id, kind, site, account, created_at) VALUES ('c', 'jira', 'https://example.atlassian.net', 'user1@example.com', 1)`)
	mustExec(t, db, `INSERT INTO project_tracker_boards(project_id, connection_id, board_id, position) VALUES ('p', 'c', '10', 0)`)
	mustExec(t, db, `INSERT INTO tracker_cache_roots(id, connection_id, created_at) VALUES (1, 'c', 1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_boards(root_id, board_id, name, read_at) VALUES (1, '10', 'WEB board', 5), (1, '11', 'Unlisted', 5)`)
	mustExec(t, db, `INSERT INTO tracker_cache_sync(root_id, board_id, cursor, cursor_read_at, read_at, reconciled_at) VALUES
		(1, '10', x'01', 4, 5, 6), (1, '11', NULL, 0, 5, 0)`)
	issue := func(id, parent, title string, level int) {
		mustExec(t, db, `INSERT INTO tracker_cache_issues(root_id, issue_id, issue_key, type_level, parent_id, updated_at, read_at, title, data)
			VALUES (1, ?, 'WEB-' || ?, ?, ?, 1, 3, ?, x'')`, id, id, level, parent, title)
	}
	issue("30", "", "Checkout", 1)    // on board 10: kept
	issue("31", "", "Linked epic", 1) // kept by a link
	issue("32", "", "Unlisted", 1)    // on board 11, which no project lists: not kept
	issue("1", "30", "Story 1", 0)
	issue("2", "31", "Story 2", 0)
	issue("3", "32", "Story 3", 0)
	issue("4", "33", "Story 4", 0) // its parent is not stored
	mustExec(t, db, `INSERT INTO tracker_cache_board_issues(root_id, board_id, issue_id, order_key, read_at) VALUES
		(1, '10', '30', 'a', 3), (1, '10', '1', 'b', 3), (1, '10', '2', 'c', 3), (1, '10', '3', 'd', 3), (1, '10', '4', 'e', 3), (1, '11', '32', 'a', 3)`)
	mustExec(t, db, `INSERT INTO threads(id, project_id, provider, workspace_path, created_at, updated_at) VALUES ('t', 'p', 'claude', '/tmp/p', 1, 1)`)
	mustExec(t, db, `INSERT INTO thread_tickets(thread_id, kind, site, issue_id, issue_key, origin, linked_at)
		VALUES ('t', 'jira', 'https://example.atlassian.net', '31', 'WEB-31', 'manual', 1)`)

	migrateFrom(t, db, trackerParentsMigrationVersion-1)

	assertRows(t, db, mirrorParentNamesSQL, "30:WEB-30:Checkout:1@3,31:WEB-31:Linked epic:1@3")
	assertRows(t, db, `SELECT board_id || ':' || iif(cursor IS NULL, '-', hex(cursor)) || ':' || cursor_read_at || ':' || read_at || ':' || reconciled_at
		FROM tracker_cache_sync ORDER BY board_id`, "10:-:4:5:6,11:-:0:5:0")
	for _, bad := range []string{
		`INSERT INTO tracker_cache_parents(root_id, issue_id, issue_key, title, type_level, read_at) VALUES (1, '40', '', 't', 0, 1)`,
		`INSERT INTO tracker_cache_parents(root_id, issue_id, issue_key, title, type_level, read_at) VALUES (1, '40', 'WEB-40', 't', 0, 0)`,
		`INSERT INTO tracker_cache_parents(root_id, issue_id, issue_key, title, type_level, read_at) VALUES (1, '', 'WEB-40', 't', 0, 1)`,
	} {
		if _, err := db.Exec(bad); err == nil || !strings.Contains(err.Error(), "CHECK") {
			t.Errorf("%s: %v; want a CHECK failure", bad, err)
		}
	}
	mustExec(t, db, `DELETE FROM tracker_cache_roots`)
	assertRows(t, db, `SELECT count(*) FROM tracker_cache_parents`, "0")
}

// An upgraded database, before any board is read again, shows each parent
// as the release before showed it: a store written by the card writers,
// its parents' names taken away and v172 run on it again, names through
// the views' read (ListMirrorParents) exactly the parents the old read
// named (the kept parents' cards, ListMirrorCards), with their keys and
// titles.
func TestAnUpgradedMirrorNamesWhatItNamedBefore(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	epic := webRow("30", 5, "a30")
	epic.Card.Key, epic.Card.Title, epic.Card.TypeLevel = "WEB-30", "Checkout", 1
	byID := func(id, parent string) MirrorRow {
		r := webRow(id, 5, "a"+id)
		r.Card.ParentID = parent
		return r
	}
	writeParentPage(t, s, root, 2, epic, byID("1", "30"), byID("2", "31"), byID("3", "32"), byID("4", "33"))
	linked := webCard("31", 5)
	linked.Title = "Linked epic"
	linkIssue(t, s, "linking", "31")
	mustUpsertCards(t, s, root, mirrorReadAt(3), linked)
	// 32 was on a board and is no longer kept; its record awaits the sweep.
	writeParentPage(t, s, root, 3, func() MirrorRow { r := webRow("32", 5, "a32"); r.Card.Title = "Left"; return r }())
	page := webPage("10")
	page.ReadAt, page.Missing = mirrorReadAt(4), []string{"32"}
	mustWriteRowPage(t, s, root, page, nil)
	parents := []string{"30", "31", "32", "33"}
	before, err := s.ListMirrorCards(root, parents)
	if err != nil {
		t.Fatal(err)
	}
	var want []MirrorParent
	for _, c := range before {
		want = append(want, MirrorParent{IssueID: c.IssueID, MirrorParentName: MirrorParentName{Key: c.Key, Title: c.Title, TypeLevel: c.TypeLevel}})
	}
	if len(want) != 2 {
		t.Fatalf("the old read named %+v; want the board's epic and the linked one", want)
	}
	mustExec(t, s.db, `DELETE FROM tracker_cache_parents`)
	if _, err := s.db.Exec(trackerParentsV172SQL[strings.Index(trackerParentsV172SQL, "INSERT INTO"):]); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListMirrorParents(root, parents)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("after the upgrade the views name %+v, %v; the release before named %+v", got, err, want)
	}
}

// mustUpsertCards stores cards read with no board, failing the test on an
// error or a card it did not store.
func mustUpsertCards(t *testing.T, s *Store, root int64, read MirrorRead, cards ...MirrorCard) {
	t.Helper()
	ch, err := s.UpsertMirrorCards(root, currentSweep(s, root), cards, MirrorCatalog{}, read)
	if err != nil || len(ch.Unkept)+len(ch.Superseded) > 0 {
		t.Fatal(fmt.Errorf("upsert cards = %+v, %w", ch, err))
	}
}
