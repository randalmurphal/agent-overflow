package store

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func webHistory(issue string) MirrorHistory {
	two, five := 2.0, 5.0
	return MirrorHistory{IssueID: issue, CompleteFrom: 0, Changes: []MirrorChange{
		{Kind: MirrorChangeStatus, At: 2, AuthorID: "acc-user1", From: "1", To: "3"},
		{Kind: MirrorChangeSprint, At: 3, AuthorID: "acc-user1", FromSprints: []string{}, ToSprints: []string{"21", "20"}},
		{Kind: MirrorChangeEstimate, At: 4, BoardID: "10", FromNumber: nil, ToNumber: &two},
		{Kind: MirrorChangeEstimate, At: 5, BoardID: "10", FromNumber: &two, ToNumber: &five},
		{Kind: MirrorChangeAssignee, At: 6, To: "acc-user1"},
		{Kind: MirrorChangeStatus, At: 7, From: "3", To: "10001"},
	}}
}

// replaceHistory writes one history, as a batch of one.
func replaceHistory(s *Store, root int64, h MirrorHistory) (MirrorWrite, error) {
	results, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, []MirrorHistory{h}))
	if err != nil {
		return MirrorWrite{}, err
	}
	return MirrorWrite{Changed: results[0].Changed, Superseded: results[0].Superseded}, nil
}

func mustReplaceHistory(t *testing.T, s *Store, root int64, h MirrorHistory, want MirrorWrite) {
	t.Helper()
	if got, err := replaceHistory(s, root, h); err != nil || got != want {
		t.Fatalf("replace %s history (for %d): %+v, %v; want %+v", h.IssueID, h.For, got, err, want)
	}
}

// A history stores whole and reads back as rows; the same history again
// changes nothing; a shorter one removes the changes it no longer has.
func TestReplaceMirrorHistoryReportsChanges(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 9, "a")), nil)

	h := webHistory("10001")
	mustReplaceHistory(t, s, root, h, MirrorWrite{Changed: true})
	mustReplaceHistory(t, s, root, h, MirrorWrite{})
	assertRows(t, s.db, `SELECT kind || ':' || seq || ':' || at || ':' || author_id || ':' || board_id || ':' || from_value || '>' || to_value
		|| ':' || coalesce(from_number, '-') || '>' || coalesce(to_number, '-')
		FROM tracker_cache_changes ORDER BY at`, strings.Join([]string{
		"1:0:2:acc-user1::1>3:->-",
		`2:0:3:acc-user1::[]>["20","21"]:->-`,
		"3:0:4::10:>:->2.0",
		"3:1:5::10:>:2.0>5.0",
		"4:0:6:::>acc-user1:->-",
		"1:1:7:::3>10001:->-",
	}, ","))
	assertRows(t, s.db, `SELECT history_from || ':' || history_for || ':' || history_answered_at || ':' || history_wait FROM tracker_cache_issues`, "0:9:1:0")

	for name, edit := range map[string]func(*MirrorHistory){
		"answered later": func(h *MirrorHistory) { h.AnsweredAt = 10 },
		"not final":      func(h *MirrorHistory) { h.Wait = 60_000 },
		"complete later": func(h *MirrorHistory) { h.CompleteFrom = 1 },
		"author":         func(h *MirrorHistory) { h.Changes[0].AuthorID = "" },
		"sprint set":     func(h *MirrorHistory) { h.Changes[1].ToSprints = []string{"21"} },
		"estimate value": func(h *MirrorHistory) { h.Changes[3].ToNumber = nil },
		"change dropped": func(h *MirrorHistory) { h.Changes = h.Changes[:5] },
		"kind emptied": func(h *MirrorHistory) {
			h.Changes = slices.DeleteFunc(h.Changes, func(c MirrorChange) bool { return c.Kind == MirrorChangeAssignee })
		},
		"change appended": func(h *MirrorHistory) {
			h.Changes = append(h.Changes, MirrorChange{Kind: MirrorChangeAssignee, At: 8, From: "acc-user1"})
		},
	} {
		s, _, root := newMirror(t)
		writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 9, "a")})
		mustReplaceHistory(t, s, root, webHistory("10001"), MirrorWrite{Changed: true})
		edited := webHistory("10001")
		edit(&edited)
		if got, err := replaceHistory(s, root, edited); err != nil || got != (MirrorWrite{Changed: true}) {
			t.Errorf("%s: %+v, %v", name, got, err)
		}
		if got, err := replaceHistory(s, root, edited); err != nil || got != (MirrorWrite{}) {
			t.Errorf("%s again: %+v, %v", name, got, err)
		}
		if n := countMirrorRows(t, s.db)["tracker_cache_changes"]; n != len(edited.Changes) {
			t.Errorf("%s: %d changes stored; want %d", name, n, len(edited.Changes))
		}
	}

	// The history of an issue the mirror does not keep stores nothing.
	mustReplaceHistory(t, s, root, webHistory("10009"), MirrorWrite{})
	for name, bad := range map[string]MirrorHistory{
		"no issue":      {For: 9},
		"no version":    {IssueID: "10001", For: -1},
		"version ahead": {IssueID: "10001", For: 10},
		"negative from": {IssueID: "10001", CompleteFrom: -1},
		"no answer":     {IssueID: "10001", AnsweredAt: -1},
		"negative wait": {IssueID: "10001", Wait: -1},
	} {
		if _, err := replaceHistory(s, root, bad); !errors.Is(err, ErrInvalidTrackerCache) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, edit := range map[string]func(*MirrorHistory){
		"status with sprints": func(h *MirrorHistory) { h.Changes[0].ToSprints = []string{"21"} },
		"status with number":  func(h *MirrorHistory) { h.Changes[0].ToNumber = h.Changes[2].ToNumber },
		"status with board":   func(h *MirrorHistory) { h.Changes[0].BoardID = "10" },
		"sprint with number":  func(h *MirrorHistory) { h.Changes[1].ToNumber = h.Changes[2].ToNumber },
		"sprint with IDs":     func(h *MirrorHistory) { h.Changes[1].To = "21" },
		"estimate with IDs":   func(h *MirrorHistory) { h.Changes[2].To = "3" },
		"estimate with set":   func(h *MirrorHistory) { h.Changes[2].ToSprints = []string{"21"} },
		"estimate no board":   func(h *MirrorHistory) { h.Changes[2].BoardID = "" },
		"assignee sprint set": func(h *MirrorHistory) { h.Changes[4].ToSprints = []string{"21"} },
		"unknown kind":        func(h *MirrorHistory) { h.Changes[0].Kind = "priority" },
		"no time":             func(h *MirrorHistory) { h.Changes[0].At = 0 },
	} {
		bad := webHistory("10001")
		edit(&bad)
		if _, err := replaceHistory(s, root, bad); !errors.Is(err, ErrInvalidTrackerCache) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A history listed before the stored one was written (its receipt is
// stale) is superseded and leaves it, however its content differs; one
// listed since replaces it.
func TestMirrorHistoryIsNeverReplacedByAnEarlierOne(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 9, "a")), nil)
	listed := receiptOf(t, s, root, "10001")
	mustReplaceHistory(t, s, root, webHistory("10001"), MirrorWrite{Changed: true})
	stored := countMirrorRows(t, s.db)["tracker_cache_changes"]

	early := listedAt(s, root, statusHistory("10001", 3), listed)
	if results, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), []MirrorHistory{early}); err != nil || !results[0].Superseded {
		t.Fatalf("a history listed before the stored one = %+v, %v; want superseded", results, err)
	}
	if n := countMirrorRows(t, s.db)["tracker_cache_changes"]; n != stored {
		t.Fatalf("an earlier history left %d changes; want the stored %d", n, stored)
	}
	assertRows(t, s.db, `SELECT history_receipt FROM tracker_cache_issues`, "1")
	mustReplaceHistory(t, s, root, statusHistory("10001", 3), MirrorWrite{Changed: true})
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_changes`, "3")
}

func mustStatusSince(t *testing.T, s *Store, root int64, issue string) int64 {
	t.Helper()
	cards := mustMirrorCards(t, s, root, issue)
	if len(cards) != 1 {
		t.Fatalf("issue %s has no card", issue)
	}
	return cards[0].StatusSince
}

// A stored history sets the time the card's status began when it shows
// it: the history was read for the card's update time and is final, and
// its last status change led to the card's status, or it changed no status
// and is complete from the issue's creation. Otherwise the time is zero,
// never guessed, and a card updated after the update its history was read
// for reads as unknown until a history read for it is stored.
func TestMirrorStatusSinceComesFromTheHistory(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	row := func(id, status string) MirrorRow {
		r := webRow(id, 9, "a"+id)
		r.Card.StatusID = status
		return r
	}
	mustWriteRowPage(t, s, root, webPage("10", row("10001", "10001"), row("10002", "10001"), row("10003", "3"), row("10004", "1"), row("10005", "1"),
		row("10006", "10001")), nil)
	assignee := []MirrorChange{{Kind: MirrorChangeAssignee, At: 6, To: "acc-user1"}}
	for _, tc := range []struct {
		name string
		h    MirrorHistory
		want int64
	}{
		{"last change leads to the status", webHistory("10001"), 7},
		{"card newer than the history", func() MirrorHistory { h := webHistory("10002"); h.For = 8; return h }(), 0},
		{"history not final", func() MirrorHistory { h := webHistory("10006"); h.Wait = 60_000; return h }(), 0},
		{"last change leads elsewhere", webHistory("10003"), 0},
		{"no change, complete from creation", MirrorHistory{IssueID: "10004", Changes: assignee}, 1},
		{"no change, complete from later", MirrorHistory{IssueID: "10005", CompleteFrom: 4, Changes: assignee}, 0},
	} {
		mustReplaceHistory(t, s, root, tc.h, MirrorWrite{Changed: true})
		if got := mustStatusSince(t, s, root, tc.h.IssueID); got != tc.want {
			t.Errorf("%s: status since %d; want %d", tc.name, got, tc.want)
		}
	}

	same := row("10001", "10001")
	same.Card.UpdatedAt = 10
	mustWriteRowPage(t, s, root, webPage("10", same), nil)
	if got := mustStatusSince(t, s, root, "10001"); got != 0 {
		t.Fatalf("after an update the history does not cover: %d; want unknown", got)
	}
	moved := row("10001", "3")
	moved.Card.UpdatedAt = 11
	mustWriteRowPage(t, s, root, webPage("10", moved), nil)
	if got := mustStatusSince(t, s, root, "10001"); got != 0 {
		t.Fatalf("after a status change: %d; want unknown", got)
	}
	h := webHistory("10001")
	h.Changes = append(h.Changes, MirrorChange{Kind: MirrorChangeStatus, At: 11, From: "10001", To: "3"})
	mustReplaceHistory(t, s, root, h, MirrorWrite{Changed: true})
	if got := mustStatusSince(t, s, root, "10001"); got != 11 {
		t.Fatalf("after the new history: %d; want 11", got)
	}

	// A history read for an earlier update than the card's does not cover
	// the card, even holding the change that made it: the time is unknown
	// until a history read for the card's update is stored.
	h.Changes = append(h.Changes, MirrorChange{Kind: MirrorChangeStatus, At: 15, From: "3", To: "1"})
	mustReplaceHistory(t, s, root, h, MirrorWrite{Changed: true})
	back := row("10001", "1")
	back.Card.UpdatedAt = 15
	mustWriteRowPage(t, s, root, webPage("10", back), nil)
	if got := mustStatusSince(t, s, root, "10001"); got != 0 {
		t.Fatalf("a card newer than the update its history was read for: %d; want unknown", got)
	}
	mustReplaceHistory(t, s, root, h, MirrorWrite{Changed: true})
	if got := mustStatusSince(t, s, root, "10001"); got != 15 {
		t.Fatalf("the history read for the card's update: %d; want 15", got)
	}
}

func mustDetailWrite(t *testing.T, what string, got MirrorDetailWrite, err error, want MirrorDetailWrite) {
	t.Helper()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %+v, %v; want %+v", what, got, err, want)
	}
}

// mustDetailStore stores one document of issue (StoreMirrorDetails) and
// requires its outcome and the details the write dropped.
func mustDetailStore(t *testing.T, s *Store, root int64, what, issue string, openedAt, readAt int64, want MirrorDetailWrite, dropped ...string) {
	t.Helper()
	got, err := s.StoreMirrorDetails(root, currentSweep(s, root), []MirrorDetailDoc{{IssueID: issue, Data: []byte(`{}`)}}, openedAt, readAt)
	if wantAll := (MirrorDetailWrites{Docs: []MirrorDetailWrite{want}, Dropped: dropped}); err != nil || !reflect.DeepEqual(got, wantAll) {
		t.Fatalf("%s = %+v, %v; want %+v", what, got, err, wantAll)
	}
}

// An opening only moves the open time forward, and the first is the
// detail's read watermark: an opening whose read began earlier stores no
// document. A document is stored with the time its read began, unless the
// stored one came from a read that began later, and the time it was opened
// stays. A detail with no document reads as absent, as does that of an
// issue the mirror stops keeping.
func TestMirrorDetailFollowsItsReadOrder(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a")})
	opened := func() string {
		var at string
		if err := s.db.QueryRow(`SELECT opened_at || ' ' || read_at FROM tracker_cache_issue_details`).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	got, err := s.StoreMirrorDetail(root, currentSweep(s, root), "10001", 100, 90, []byte(`{"v":0}`))
	mustDetailWrite(t, "first opening, its read earlier", got, err, MirrorDetailWrite{Stored: true, Superseded: true})
	if _, ok, err := s.GetMirrorDetail(root, "10001"); err != nil || ok {
		t.Fatalf("a detail with no document reads: %v, %v", ok, err)
	}
	got, err = s.StoreMirrorDetail(root, currentSweep(s, root), "10001", 50, 40, []byte(`{"v":0}`))
	mustDetailWrite(t, "earlier opening", got, err, MirrorDetailWrite{Stored: true, Superseded: true})
	if at := opened(); at != "100 100" {
		t.Fatalf("after an earlier opening: %s", at)
	}
	got, err = s.StoreMirrorDetail(root, currentSweep(s, root), "10001", 0, 180, []byte(`{"v":1}`))
	mustDetailWrite(t, "document", got, err, MirrorDetailWrite{Stored: true, Changed: true})
	got, err = s.StoreMirrorDetail(root, currentSweep(s, root), "10001", 0, 170, []byte(`{"v":0}`))
	mustDetailWrite(t, "earlier document", got, err, MirrorDetailWrite{Stored: true, Superseded: true})
	got, err = s.StoreMirrorDetail(root, currentSweep(s, root), "10001", 0, 170, []byte(`{"v":1}`))
	mustDetailWrite(t, "identical earlier document", got, err, MirrorDetailWrite{Stored: true})
	if d, ok, err := s.GetMirrorDetail(root, "10001"); err != nil || !ok || !reflect.DeepEqual(d, MirrorDetail{IssueID: "10001", OpenedAt: 100, ReadAt: 180,
		Data: []byte(`{"v":1}`), CardUpdatedAt: 5, row: d.row, sweep: currentSweep(s, root)}) || d.row <= 0 {
		t.Fatalf("detail = %+v, %v, %v", d, ok, err)
	}
	got, err = s.StoreMirrorDetail(root, currentSweep(s, root), "10001", 0, 190, []byte(`{"v":2}`))
	mustDetailWrite(t, "later document", got, err, MirrorDetailWrite{Stored: true, Changed: true})
	d, _, err := s.GetMirrorDetail(root, "10001")
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.ConfirmMirrorDetail(root, d, 200)
	mustDetailWrite(t, "later opening", got, err, MirrorDetailWrite{Stored: true})
	if at := opened(); at != "200 190" {
		t.Fatalf("after a later opening: %s", at)
	}

	got, err = s.StoreMirrorDetail(root, currentSweep(s, root), "10009", 300, 300, []byte(`{}`))
	mustDetailWrite(t, "opening of an unkept issue", got, err, MirrorDetailWrite{})
	got, err = s.StoreMirrorDetail(root, currentSweep(s, root), "10009", 0, 300, []byte(`{}`))
	mustDetailWrite(t, "document of an unkept issue", got, err, MirrorDetailWrite{})
	if _, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Missing: []string{"10001"}, ReadAt: 2})); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.GetMirrorDetail(root, "10001"); err != nil || ok {
		t.Fatalf("detail of an unkept issue reads: %v, %v", ok, err)
	}
	for name, err := range map[string]error{
		"no documents": func() error { _, err := s.StoreMirrorDetails(root, currentSweep(s, root), nil, 0, 1); return err }(),
		"an issue twice": func() error {
			_, err := s.StoreMirrorDetails(root, currentSweep(s, root), []MirrorDetailDoc{{IssueID: "10001"}, {IssueID: "10001"}}, 0, 1)
			return err
		}(),
		"document with no time": func() error {
			_, err := s.StoreMirrorDetail(root, currentSweep(s, root), "10001", 0, 0, nil)
			return err
		}(),
		"document with no issue": func() error { _, err := s.StoreMirrorDetail(root, currentSweep(s, root), "", 0, 1, nil); return err }(),
	} {
		if !errors.Is(err, ErrInvalidTrackerCache) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The root keeps the mirrorDetailCap most recently opened details; a new
// one that is itself the least recently opened is dropped at once and
// reported as not stored, and not as dropped: it was never held.
func TestMirrorDetailKeepsTheMostRecentlyOpened(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", mirrorRows(10000, mirrorDetailCap+2, "a"))
	for i := range mirrorDetailCap {
		got, err := s.StoreMirrorDetail(root, currentSweep(s, root), fmt.Sprint(10000+i), int64(100+i), int64(100+i), []byte(`{}`))
		mustDetailWrite(t, fmt.Sprint("opening ", i), got, err, MirrorDetailWrite{Stored: true, Changed: true})
	}
	oldest, _, err := s.GetMirrorDetail(root, "10000")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ConfirmMirrorDetail(root, oldest, 1000)
	mustDetailWrite(t, "reopening the oldest", got, err, MirrorDetailWrite{Stored: true})
	mustDetailStore(t, s, root, "one over the cap", "10200", 1001, 1001, MirrorDetailWrite{Stored: true, Changed: true}, "10001")
	mustDetailStore(t, s, root, "a new detail older than all", "10201", 50, 50, MirrorDetailWrite{})
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_details`, fmt.Sprint(mirrorDetailCap))
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_details WHERE issue_id IN ('10001', '10201')`, "0")
}

// A confirmation marks a detail opened, and changes no document, only
// while the mirror holds the document read: not once a later read
// replaced it, the cap dropped it, a sweep ran or its issue is no longer
// kept. Refused, it writes nothing, so it neither adds a detail with no
// document nor drops another.
func TestConfirmMirrorDetail(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", mirrorRows(10000, mirrorDetailCap+2, "a"))
	state := func(issue string) string {
		t.Helper()
		var at string
		if err := s.db.QueryRow(`SELECT opened_at || ' ' || read_at || ' ' || coalesce(data ->> 'v', '-') FROM tracker_cache_issue_details
			WHERE root_id = ? AND issue_id = ?`, root, issue).Scan(&at); err != nil {
			return err.Error()
		}
		return at
	}
	read := func(issue string) MirrorDetail {
		t.Helper()
		d, ok, err := s.GetMirrorDetail(root, issue)
		if err != nil || !ok {
			t.Fatalf("detail of %s: %v, %v", issue, ok, err)
		}
		return d
	}
	confirm := func(what string, d MirrorDetail, openedAt int64, want MirrorDetailWrite) {
		t.Helper()
		got, err := s.ConfirmMirrorDetail(root, d, openedAt)
		mustDetailWrite(t, what, got, err, want)
	}
	got, err := s.StoreMirrorDetail(root, currentSweep(s, root), "10000", 100, 100, []byte(`{"v":1}`))
	mustDetailWrite(t, "first read", got, err, MirrorDetailWrite{Stored: true, Changed: true})
	d := read("10000")
	if d.CardUpdatedAt != 5 {
		t.Fatalf("the card's update time read with the detail: %d", d.CardUpdatedAt)
	}
	confirm("an opening", d, 150, MirrorDetailWrite{Stored: true})
	confirm("an earlier opening", d, 120, MirrorDetailWrite{Stored: true})
	if at := state("10000"); at != "150 100 1" {
		t.Fatalf("after the openings: %s", at)
	}

	// A later read replaced the document: refused, nothing written.
	got, err = s.StoreMirrorDetail(root, currentSweep(s, root), "10000", 0, 200, []byte(`{"v":3}`))
	mustDetailWrite(t, "a later read", got, err, MirrorDetailWrite{Stored: true, Changed: true})
	confirm("a replaced document", d, 300, MirrorDetailWrite{})
	if at := state("10000"); at != "150 200 3" {
		t.Fatalf("after a refused confirmation: %s", at)
	}

	// The cap dropped the detail: refused, and no row comes back to drop
	// another.
	d = read("10000")
	for i := range mirrorDetailCap {
		if _, err := s.StoreMirrorDetail(root, currentSweep(s, root), fmt.Sprint(10001+i), int64(1000+i), int64(1000+i), []byte(`{"v":1}`)); err != nil {
			t.Fatal(err)
		}
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_details WHERE issue_id = '10000'`, "0")
	confirm("a dropped detail", d, 5000, MirrorDetailWrite{})
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_details WHERE issue_id = '10000'`, "0")
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_details`, fmt.Sprint(mirrorDetailCap))

	// An issue no longer kept: refused.
	got, err = s.StoreMirrorDetail(root, currentSweep(s, root), "10001", 6000, 6000, []byte(`{"v":4}`))
	mustDetailWrite(t, "a read of another issue", got, err, MirrorDetailWrite{Stored: true, Changed: true})
	d = read("10001")
	if _, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Missing: []string{"10001"}, ReadAt: 2})); err != nil {
		t.Fatal(err)
	}
	confirm("an unkept issue", d, 7000, MirrorDetailWrite{})
	if at := state("10001"); at != "6000 6000 4" {
		t.Fatalf("after an unkept issue's confirmation: %s", at)
	}

	for name, d := range map[string]MirrorDetail{"no detail read": {}, "a detail built by hand": {IssueID: "10002", ReadAt: 1}} {
		if _, err := s.ConfirmMirrorDetail(root, d, 1); !errors.Is(err, ErrInvalidTrackerCache) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.ConfirmMirrorDetail(root, d, 0); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Errorf("no opening time: %v", err)
	}
}

// A detail swept and stored again in its issue's next lifetime, in the
// same row with a read of the same time, is not the document read before
// the sweep: its confirmation is refused.
func TestConfirmMirrorDetailRefusesADocumentFromBeforeASweep(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	keep := func(thread string) {
		t.Helper()
		linkIssue(t, s, thread, "10001")
		if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{webCard("10001", 5)}, MirrorCatalog{}, 1); err != nil {
			t.Fatal(err)
		}
		if _, err := s.StoreMirrorDetail(root, currentSweep(s, root), "10001", 100, 100, []byte(`{"v":1}`)); err != nil {
			t.Fatal(err)
		}
	}
	keep("linked")
	before, ok, err := s.GetMirrorDetail(root, "10001")
	if err != nil || !ok {
		t.Fatalf("detail: %v, %v", ok, err)
	}
	if _, err := s.ClearThreadTicket("linked"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishMirrorCleanup(nil); err != nil {
		t.Fatal(err)
	}
	keep("relinked")
	after, ok, err := s.GetMirrorDetail(root, "10001")
	if err != nil || !ok || after.row != before.row || after.ReadAt != before.ReadAt || after.sweep == before.sweep {
		t.Fatalf("the detail stored again: row %d (was %d), read %d, sweep %d (was %d), %v, %v",
			after.row, before.row, after.ReadAt, after.sweep, before.sweep, ok, err)
	}
	got, err := s.ConfirmMirrorDetail(root, before, 200)
	mustDetailWrite(t, "a document from before the sweep", got, err, MirrorDetailWrite{})
	assertRows(t, s.db, `SELECT opened_at || ' ' || (data ->> 'v') FROM tracker_cache_issue_details WHERE issue_id = '10001'`, "100 1")
	got, err = s.ConfirmMirrorDetail(root, after, 200)
	mustDetailWrite(t, "the document after it", got, err, MirrorDetailWrite{Stored: true})
}

// A batch writes every admitted history in one transaction and reports
// each issue's outcome in input order, an issue the mirror holds but no
// longer keeps included; one whose removals would pass a
// chunk goes to a further transaction, so none deletes more than a chunk.
func TestReplaceMirrorHistoriesInBatches(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 9, "a"), webRow("10002", 9, "b"), webRow("10003", 9, "c"), webRow("10004", 9, "d")})
	if _, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Missing: []string{"10004"}, ReadAt: 2})); err != nil {
		t.Fatal(err)
	}
	mustReplaceHistory(t, s, root, webHistory("10001"), MirrorWrite{Changed: true})
	listed := receiptOf(t, s, root, "10003")
	mustReplaceHistory(t, s, root, webHistory("10003"), MirrorWrite{Changed: true})

	// 10003's history was listed before the one stored.
	batch := dueHistories(s, root, []MirrorHistory{webHistory("10009"), webHistory("10003"), webHistory("10002"), webHistory("10004"), webHistory("10001")})
	batch[1].Receipt = listed
	commits := s.CommitCount()
	results, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), batch)
	want := []MirrorHistoryResult{{IssueID: "10009", Unkept: true}, {IssueID: "10003", Superseded: true}, {IssueID: "10002", Changed: true},
		{IssueID: "10004", Unkept: true}, {IssueID: "10001"}}
	if err != nil || !reflect.DeepEqual(results, want) {
		t.Fatalf("batch = %+v, %v\nwant %+v", results, err, want)
	}
	if n := s.CommitCount() - commits; n != 1 {
		t.Fatalf("the batch committed %d transactions; want 1", n)
	}

	mustReplaceHistory(t, s, root, statusHistory("10001", clearMirrorChunk), MirrorWrite{Changed: true})
	mustReplaceHistory(t, s, root, statusHistory("10002", clearMirrorChunk), MirrorWrite{Changed: true})
	recorder := recordMirrorWrites(t, s)
	commits = s.CommitCount()
	results, err = s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, []MirrorHistory{{IssueID: "10001"}, {IssueID: "10002"}}))
	if err != nil || !reflect.DeepEqual(results, []MirrorHistoryResult{{IssueID: "10001", Changed: true}, {IssueID: "10002", Changed: true}}) {
		t.Fatalf("emptying batch = %+v, %v", results, err)
	}
	if most, total := recorder.mostDeletedInATransaction(); most > clearMirrorChunk || total != 2*clearMirrorChunk || s.CommitCount()-commits != 2 {
		t.Fatalf("deleted %d, at most %d in one of %d transactions", total, most, s.CommitCount()-commits)
	}

	stored := countMirrorRows(t, s.db)["tracker_cache_changes"]
	tooMany := make([]MirrorHistory, mirrorHistoryBatch+1)
	for i := range tooMany {
		tooMany[i] = MirrorHistory{IssueID: fmt.Sprint(20000 + i)}
	}
	for name, batch := range map[string][]MirrorHistory{
		"too many histories": tooMany,
		"issue twice":        {webHistory("10001"), webHistory("10001")},
		"one invalid":        {webHistory("10002"), {IssueID: "10001", For: -1}},
	} {
		if _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, batch)); !errors.Is(err, ErrInvalidTrackerCache) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := countMirrorRows(t, s.db)["tracker_cache_changes"]; n != stored {
		t.Fatalf("refused batches left %d changes; want %d", n, stored)
	}
}
