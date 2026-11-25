package store

import (
	"maps"
	"strings"
	"testing"
)

// A key resolves to the issue the site's link reader holds, a key of a
// project it holds other issues of to its connection alone, and a key of
// no mirrored project to nothing; an issue a later read missed is not
// held, and a connection turned off reads nothing.
func TestTicketsByKeyReadsTheLinkReadersMirror(t *testing.T) {
	t.Parallel()
	s, c, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	card := titledCard("10001", 5, "Retry cursor")
	mustWriteRowPage(t, s, root, webPage("10", MirrorRow{Card: card, OrderKey: "a"}, webRow("10002", 5, "b")), nil)

	got, err := s.TicketsByKey([]string{"WEB-10001", "WEB-99", "APP-1", "UTF-8", "WEB-10001"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]KeyedTicket{
		"WEB-10001": {Key: "WEB-10001", ConnectionID: c.ID, IssueID: "10001", Title: "Retry cursor"},
		"WEB-99":    {Key: "WEB-99", ConnectionID: c.ID},
	}
	if !maps.Equal(got, want) {
		t.Fatalf("tickets = %+v, want %+v", got, want)
	}

	mustExec(t, s.db, `INSERT INTO tracker_cache_issue_missing(root_id, issue_id, read_at) VALUES (?, '10002', ?)`, root, int64(1)<<50)
	got, err = s.TicketsByKey([]string{"WEB-10002"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (KeyedTicket{Key: "WEB-10002", ConnectionID: c.ID}); got["WEB-10002"] != want {
		t.Fatalf("a missed issue = %+v, want %+v", got["WEB-10002"], want)
	}

	mustExec(t, s.db, `UPDATE tracker_connections SET enabled = 0 WHERE id = ?`, c.ID)
	if got, err := s.TicketsByKey([]string{"WEB-10001"}); err != nil || len(got) != 0 {
		t.Fatalf("a connection turned off = %+v, %v, want nothing", got, err)
	}
	if got, err := s.TicketsByKey(nil); err != nil || len(got) != 0 {
		t.Fatalf("no keys = %+v, %v", got, err)
	}
}

// Each key costs one seek of the key index per reader for its issue and
// one for its project, never a scan of a mirror.
func TestTicketsByKeySeeksTheKeyIndex(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	plan := explainMirror(t, s, ticketsByKeySQL)
	for _, want := range []string{
		"SEARCH i USING INDEX idx_tracker_cache_issues_key (root_id=? AND issue_key=?)",
		"SEARCH p USING COVERING INDEX idx_tracker_cache_issues_key (root_id=? AND issue_key>? AND issue_key<?)",
	} {
		if !strings.Contains(plan, want) {
			t.Fatalf("plan:\n%s\nwant %q", plan, want)
		}
	}
	if strings.Contains(plan, "SCAN i") || strings.Contains(plan, "SCAN p") {
		t.Fatalf("plan scans a mirror:\n%s", plan)
	}
}
