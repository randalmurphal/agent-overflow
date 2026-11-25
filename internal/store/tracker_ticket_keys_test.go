package store

import (
	"encoding/json"
	"fmt"
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

	got, err := s.TicketsByKey([]string{"WEB-10001", "WEB-99", "APP-1", "UTF-8", "WEB-10001"}, nil)
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
	got, err = s.TicketsByKey([]string{"WEB-10002"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (KeyedTicket{Key: "WEB-10002", ConnectionID: c.ID}); got["WEB-10002"] != want {
		t.Fatalf("a missed issue = %+v, want %+v", got["WEB-10002"], want)
	}

	mustExec(t, s.db, `UPDATE tracker_connections SET enabled = 0 WHERE id = ?`, c.ID)
	if got, err := s.TicketsByKey([]string{"WEB-10001"}, nil); err != nil || len(got) != 0 {
		t.Fatalf("a connection turned off = %+v, %v, want nothing", got, err)
	}
	if got, err := s.TicketsByKey(nil, nil); err != nil || len(got) != 0 {
		t.Fatalf("no keys = %+v, %v", got, err)
	}
}

// keyedMirrorStore is a store whose Jira connection's mirror holds
// WEB-1 to WEB-n, and, with second, a Linear connection whose mirror holds
// OPS-1 alone.
func keyedMirrorStore(t *testing.T, n int, second bool) *Store {
	t.Helper()
	s, _, root := newMirror(t)
	mustExec(t, s.db, `WITH RECURSIVE seq(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM seq WHERE x < ?)
		INSERT INTO tracker_cache_issues(root_id, issue_id, issue_key, updated_at, read_at) SELECT ?, 'i' || x, 'WEB-' || x, 1, 1 FROM seq`, n, root)
	if second {
		linear := mustCreateTrackerConnection(t, s, NewTrackerConnection{Kind: TrackerKindLinear, Site: "workspace-a", CreatedAt: 12})
		other, _, err := s.GetOrCreateMirrorRoot(linear.ID)
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, s.db, `INSERT INTO tracker_cache_issues(root_id, issue_id, issue_key, updated_at, read_at) VALUES (?, 'o1', 'OPS-1', 1, 1)`, other)
	}
	return s
}

// The Pull requests page's match of 200 keys costs about the same when a
// second connection, holding none of their project, is added: which
// mirrors hold a project is read once per project, and each key is looked
// up only in those, each a seek.
func TestTicketsByKeyCostsTheSameWithASecondConnection(t *testing.T) {
	t.Parallel()
	keys := make([]string, 200)
	for i := range keys {
		keys[i] = fmt.Sprintf("WEB-%d", 100*i+1)
	}
	list, err := json.Marshal(keys)
	if err != nil {
		t.Fatal(err)
	}
	steps := func(second bool) int {
		s := keyedMirrorStore(t, 20_000, second)
		if got, err := s.TicketsByKey(keys, nil); err != nil || len(got) != len(keys) {
			t.Fatalf("tickets = %d, %v; want %d", len(got), err, len(keys))
		}
		return statementVMSteps(t, s, ticketsByKeySQL, string(list), "[]")
	}
	one, two := steps(false), steps(true)
	t.Logf("200 keys over 20,000 issues: %d VM steps with one connection, %d with a second", one, two)
	if two > one+one/2 {
		t.Errorf("a second connection takes the match from %d to %d VM steps", one, two)
	}
}

// Each probe is one seek of the key index in one root: a project's, by
// its key range, and an issue's, by its key; no read scans a mirror.
func TestTicketsByKeySeeksTheKeyIndex(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	plan := explainMirror(t, s, ticketsByKeySQL)
	for _, want := range []string{
		"SEARCH q EXISTS USING COVERING INDEX idx_tracker_cache_issues_key (root_id=? AND issue_key>? AND issue_key<?)",
		"SEARCH i USING INDEX idx_tracker_cache_issues_key (root_id=? AND issue_key=?)",
	} {
		if !strings.Contains(plan, want) {
			t.Fatalf("plan:\n%s\nwant %q", plan, want)
		}
	}
	if strings.Contains(plan, "SCAN i") || strings.Contains(plan, "SCAN q") {
		t.Fatalf("plan scans a mirror:\n%s", plan)
	}
}

// When two sites' mirrors both hold a key, the one the asker prefers (the
// connection the pull request's project lists boards of) answers; with no
// preference, the lowest connection ID.
func TestTicketsByKeyPrefersTheAskersConnection(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	linear := mustCreateTrackerConnection(t, s, NewTrackerConnection{Kind: TrackerKindLinear, Site: "workspace-a", CreatedAt: 12})
	other, _, err := s.GetOrCreateMirrorRoot(linear.ID)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s.db, `INSERT INTO tracker_cache_issues(root_id, issue_id, issue_key, updated_at, read_at) VALUES (?, 'j1', 'WEB-1', 1, 1), (?, 'l1', 'WEB-1', 1, 1)`, root, other)
	first := min(jira.ID, linear.ID)
	for _, c := range []struct {
		prefer []string
		want   string
	}{{nil, first}, {[]string{jira.ID}, jira.ID}, {[]string{linear.ID}, linear.ID}} {
		got, err := s.TicketsByKey([]string{"WEB-1"}, c.prefer)
		if err != nil || got["WEB-1"].ConnectionID != c.want {
			t.Errorf("preferring %v: %+v, %v; want connection %s", c.prefer, got, err, c.want)
		}
	}
}
